package soloist

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/process"
	"github.com/gorilla/websocket"
)

const (
	defaultStartBackoff   = 5 * time.Second
	defaultMaxBackoff     = 60 * time.Second
	defaultDialRetry      = 1 * time.Second
	defaultDialMaxRetry   = 15 * time.Second
	defaultWSPortTimeout  = 15 * time.Second
	defaultWSPollInterval = 250 * time.Millisecond
)

// activateCommand marks the Soloist process as the active Spotify Connect
// device once the WebSocket is open.
const activateCommand = `{"type":"command","command":"activate"}`

var errWSPortTimeout = errors.New("soloist did not publish ws.port in time")

// Supervisor owns the Soloist Connect-mode subprocess lifecycle and its
// local WebSocket session. It spawns Soloist, waits for the ws.port file,
// dials and activates, then keeps the connection healthy by consuming events.
// On exit code 10 it deletes the expired binary and reports expired.
type Supervisor struct {
	// BinaryPath is the resolved soloist binary path.
	BinaryPath string
	// DeviceName is the Spotify Connect device name.
	DeviceName string
	// APIKey is the Soloist API key.
	APIKey string
	// DataDir is the Soloist data directory.
	DataDir string
	// CacheDir is the Soloist cache directory.
	CacheDir string
	// Manager spawns the soloist process.
	Manager process.Manager
	// BinaryManager removes the expired binary on exit code 10.
	BinaryManager *BinaryManager
	// Health reports soloist health state changes. An empty reason means
	// healthy; a non-empty reason becomes the /healthz error id.
	Health func(reason string)
	// StartBackoff is the initial process-restart backoff. Zero uses the default.
	StartBackoff time.Duration
	// MaxBackoff caps the process-restart backoff. Zero uses the default.
	MaxBackoff time.Duration
	// WSPortTimeout bounds how long the supervisor waits for ws.port after
	// spawning soloist. Zero uses the default.
	WSPortTimeout time.Duration
	// Dialer dials the Soloist WebSocket. Nil uses websocket.DefaultDialer.
	Dialer *websocket.Dialer
	// Commands serialises WebSocket command writes for concurrent callers.
	// Nil keeps today's inline-activate behaviour unchanged.
	Commands *CommandConnector
}

// Run supervises Soloist until ctx is cancelled or the binary is expired.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.startBackoff()
	dialBackoff := defaultDialRetry

	slog.Info("soloist: supervisor starting",
		"binary", s.BinaryPath, "device", s.DeviceName)

	for {
		if ctx.Err() != nil {
			return
		}

		reason, restart := s.runOnce(ctx, dialBackoff)
		if reason != "" {
			s.setHealth(reason)
		}

		if !restart || ctx.Err() != nil {
			return
		}

		slog.Warn("soloist: restarting", "backoff", backoff)
		if !sleep(ctx, backoff) {
			return
		}

		backoff = NextBackoff(backoff, s.maxBackoff())
	}
}

// runOnce spawns one Soloist Connect-mode process and maintains its WebSocket
// session until the process exits, ctx is cancelled, or the binary expires.
func (s *Supervisor) runOnce(ctx context.Context, dialBackoff time.Duration) (string, bool) {
	proc, err := s.Manager.Start(ctx, s.BinaryPath, s.args()...)
	if err != nil {
		slog.Error("soloist: start failed", "err", err)

		return "", true
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()

	wsAddr, err := s.waitWSAddr(ctx, waitCh)
	if err != nil {
		_ = proc.Kill()

		switch {
		case errors.Is(err, context.Canceled):
			return "", false
		case errors.Is(err, errWSPortTimeout):
			slog.Warn("soloist ws: no ws.port before timeout, restarting")

			return "", true
		default:
			return s.handleExit(err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			_ = proc.Kill()

			return "", false
		case exitErr := <-waitCh:
			_ = proc.Kill()

			return s.handleExit(exitErr)
		default:
		}

		conn, dialErr := s.dial(ctx, wsAddr)
		if dialErr != nil {
			slog.Warn("soloist ws: dial failed", "err", dialErr, "backoff", dialBackoff)

			select {
			case exitErr := <-waitCh:
				return s.handleExit(exitErr)
			default:
			}

			if !sleep(ctx, dialBackoff) {
				_ = proc.Kill()

				return "", false
			}

			dialBackoff = NextBackoff(dialBackoff, defaultDialMaxRetry)

			continue
		}
		dialBackoff = defaultDialRetry

		if s.Commands != nil {
			s.Commands.attach(conn)
		}

		if err := s.writeActivate(conn); err != nil {
			slog.Warn("soloist ws: activate send failed", "err", err)
			if s.Commands != nil {
				s.Commands.detach()
			}
			_ = conn.Close()

			continue
		}

		slog.Info("soloist ws: connected, activated")
		s.setHealth("")

		consumeErr := s.wsConsume(ctx, conn)
		if s.Commands != nil {
			s.Commands.detach()
		}
		_ = conn.Close()

		if ctx.Err() != nil {
			_ = proc.Kill()

			return "", false
		}

		slog.Warn("soloist ws: connection lost", "err", consumeErr)

		select {
		case exitErr := <-waitCh:
			return s.handleExit(exitErr)
		default:
		}
	}
}

// handleExit maps a Soloist exit error to a health reason and restart decision.
func (s *Supervisor) handleExit(exitErr error) (string, bool) {
	code := process.ExitCode(exitErr)

	switch code {
	case 10:
		slog.Error("soloist: binary expired, not restarting", "exit_code", code)
		if s.BinaryManager != nil {
			s.BinaryManager.DeleteBinary()
		}

		return "soloist_expired", false
	case 0:
		slog.Warn("soloist: exited unexpectedly", "exit_code", code)

		return "", true
	default:
		slog.Error("soloist: process exited", "exit_code", code, "err", exitErr)

		return "", true
	}
}

// waitWSAddr polls the data directory for soloist's ws.port file and returns
// the WebSocket URL. It aborts early if the process exits or ctx is cancelled.
func (s *Supervisor) waitWSAddr(ctx context.Context, waitCh <-chan error) (string, error) {
	timeout := time.NewTimer(s.wsPortTimeout())
	defer timeout.Stop()

	ticker := time.NewTicker(defaultWSPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case exitErr := <-waitCh:
			return "", exitErr
		case <-timeout.C:
			return "", errWSPortTimeout
		case <-ticker.C:
			port, err := readWSPort(s.DataDir)
			if err != nil {
				continue
			}

			addr := "127.0.0.1"
			if raw, aerr := os.ReadFile(filepath.Join(s.DataDir, "ws.addr")); aerr == nil {
				if v := strings.TrimSpace(string(raw)); v != "" {
					addr = v
				}
			}

			return "ws://" + net.JoinHostPort(addr, port), nil
		}
	}
}

// readWSPort returns the port soloist published in its data directory.
func readWSPort(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "ws.port"))
	if err != nil {
		return "", err
	}

	p := strings.TrimSpace(string(raw))
	if p == "" {
		return "", errors.New("empty ws.port")
	}

	return p, nil
}

// wsConsume drains WebSocket events until the connection breaks or ctx is
// cancelled. Soloist requires an active reader to serve pings and heartbeat.
func (s *Supervisor) wsConsume(ctx context.Context, conn *websocket.Conn) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		slog.Debug("soloist ws event", "msg", string(msg))
	}
}

func (s *Supervisor) writeActivate(conn *websocket.Conn) error {
	if s.Commands != nil {
		return s.Commands.write([]byte(activateCommand))
	}

	return conn.WriteMessage(websocket.TextMessage, []byte(activateCommand))
}

// args returns the Soloist Connect-mode command line.
func (s *Supervisor) args() []string {
	return []string{
		"--device-name", s.DeviceName,
		"--api-key", s.APIKey,
		"--data-dir", s.DataDir,
		"--cache-dir", s.CacheDir,
		"--ws", "127.0.0.1:0",
	}
}

func (s *Supervisor) dial(ctx context.Context, wsURL string) (*websocket.Conn, error) {
	d := s.Dialer
	if d == nil {
		d = websocket.DefaultDialer
	}

	conn, _, err := d.DialContext(ctx, wsURL, nil)

	return conn, err
}

func (s *Supervisor) setHealth(reason string) {
	if s.Health != nil {
		s.Health(reason)
	}
}

func (s *Supervisor) startBackoff() time.Duration {
	if s.StartBackoff > 0 {
		return s.StartBackoff
	}

	return defaultStartBackoff
}

func (s *Supervisor) maxBackoff() time.Duration {
	if s.MaxBackoff > 0 {
		return s.MaxBackoff
	}

	return defaultMaxBackoff
}

func (s *Supervisor) wsPortTimeout() time.Duration {
	if s.WSPortTimeout > 0 {
		return s.WSPortTimeout
	}

	return defaultWSPortTimeout
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
