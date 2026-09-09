// Command shim is the teddycloud-spotify-radio-shim orchestrator.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/audio"
	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/config"
	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/process"
	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/server"
	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/soloist"
	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/sselistener"
)

const (
	pairingBackoffInitial = 5 * time.Second
	pairingBackoffMax     = 60 * time.Second

	// recorderBackoffInitial/Max govern how long runRecorder waits between
	// reconnect attempts after the monitor stream dies.
	recorderBackoffInitial = 2 * time.Second
	recorderBackoffMax     = 30 * time.Second

	// monitorPollInterval is how often the recorder waits for the PulseAudio
	// daemon to become ready and retries a failed monitor connect.
	monitorPollInterval = 250 * time.Millisecond

	// recorderSummaryInterval is how often a live recording logs its
	// chunk/drop counters (debug level).
	recorderSummaryInterval = 10 * time.Second
)

// recorderSlot holds the current live recorder for /stream consumers. The
// recorder is recreated per reconnect, so the source changes over time.
type recorderSlot struct {
	mu  sync.Mutex
	src audio.ChunkSource
}

func (r *recorderSlot) get() audio.ChunkSource {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.src
}

func (r *recorderSlot) set(src audio.ChunkSource) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.src = src
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}

	setupLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slot := &recorderSlot{}
	cc := &soloist.CommandConnector{}
	srv := server.New(cfg.ListenAddr, func() server.ChunkSource { return slot.get() }, cc.Play)

	// Phase 3a: PulseAudio daemon + virtual sink. SetupEnv runs synchronously so
	// every later subprocess (Soloist, pactl) inherits HOME and XDG_RUNTIME_DIR.
	pa := &audio.PulseAudio{
		Manager: process.ExecManager{},
		Health:  srv.SetUnhealthy,
	}

	if err := pa.SetupEnv(); err != nil {
		slog.Error("pulseaudio: env setup failed", "err", err)
	}

	defer pa.Stop()
	go pa.Run(ctx)

	// Phase 3b.2: live monitor recorder. It runs independently of the Soloist
	// branch below: audio flows even while the Soloist session is missing or
	// unpaired.
	go runRecorder(ctx, pa, slot)

	// Phase 5: Teddycloud SSE listener. Independent of pairing: events are
	// translated into WebSocket commands which fail harmlessly ("not
	// connected") until the Soloist session is attached.
	go runSseListener(ctx, cfg, cc)

	// Phase 2a: resolve Soloist binary before starting anything else.
	bm := &soloist.BinaryManager{
		ExplicitPath: cfg.SoloistBin,
		DataDir:      cfg.SoloistDataDir,
	}

	binPath, binErr := bm.Resolve()

	// Phase 2b/2c: session check, then pairing gate or the Connect-mode
	// supervisor. A missing binary keeps soloist_missing and must not be
	// overwritten by the pairing gate.
	if binErr != nil {
		slog.Error("soloist binary unavailable", "err", binErr)
		srv.SetUnhealthy("soloist_missing")
	} else {
		checker := &soloist.SessionChecker{DataDir: cfg.SoloistDataDir}
		if checker.Check() == soloist.StateReady {
			slog.Info("soloist session found, ready")
			go runSupervisor(ctx, srv, cfg, binPath, bm, cc)
		} else {
			srv.SetUnhealthy("soloist_unpaired")
			slog.Warn(fmt.Sprintf(
				"No Soloist session found. Pairing now — open the Spotify app and select the device %q from the device picker.",
				cfg.SoloistDeviceName,
			))

			go pairThenSupervisor(ctx, srv, cfg, binPath, bm, cc)
		}
	}

	if err := srv.Run(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// runSseListener subscribes to the Teddycloud SSE stream and translates events
// into Soloist WebSocket commands. It runs as an independent goroutine and
// never gates on Soloist pairing.
func runSseListener(ctx context.Context, cfg *config.Config, cc *soloist.CommandConnector) {
	l := &sselistener.Listener{
		URL:      cfg.TeddycloudURL,
		Commands: cc,
	}
	l.Run(ctx)
}

// pairThenSupervisor drives the one-time pairing, then hands over to the
// Connect-mode supervisor once a session is stored.
func pairThenSupervisor(ctx context.Context, srv *server.Server, cfg *config.Config, binPath string, bm *soloist.BinaryManager, cc *soloist.CommandConnector) {
	pairer := &soloist.Pairer{
		BinaryPath:    binPath,
		DeviceName:    cfg.SoloistDeviceName,
		APIKey:        cfg.SoloistAPIKey,
		DataDir:       cfg.SoloistDataDir,
		CacheDir:      cfg.SoloistCacheDir,
		Manager:       process.ExecManager{},
		Checker:       &soloist.SessionChecker{DataDir: cfg.SoloistDataDir},
		BinaryManager: bm,
	}

	switch soloist.Drive(ctx, pairer.Pair, pairingBackoffInitial, pairingBackoffMax) {
	case soloist.PairDone:
		slog.Info("soloist paired, session stored")
		runSupervisor(ctx, srv, cfg, binPath, bm, cc)
	case soloist.PairExpired:
		srv.SetUnhealthy("soloist_expired")
		slog.Error("soloist binary expired, pairing halted")
	}
}

// runSupervisor runs the Soloist Connect-mode subprocess and WebSocket
// supervisor in the current goroutine until ctx is cancelled or soloist exits
// with a non-retryable code.
func runSupervisor(ctx context.Context, srv *server.Server, cfg *config.Config, binPath string, bm *soloist.BinaryManager, cc *soloist.CommandConnector) {
	supervisor := &soloist.Supervisor{
		BinaryPath:    binPath,
		DeviceName:    cfg.SoloistDeviceName,
		APIKey:        cfg.SoloistAPIKey,
		DataDir:       cfg.SoloistDataDir,
		CacheDir:      cfg.SoloistCacheDir,
		Manager:       process.ExecManager{},
		BinaryManager: bm,
		Commands:      cc,
		Health:        func(reason string) { srv.SetUnhealthy(reason) },
	}

	supervisor.Run(ctx)
}

// runRecorder waits for the PulseAudio daemon to become ready, then keeps the
// live virtual_out.monitor stream recording through PulseRecorder, reconnecting
// with exponential backoff when the stream dies (e.g. after a daemon crash). It
// never blocks the Soloist pairing or supervisor branches.
func runRecorder(ctx context.Context, pa *audio.PulseAudio, slot *recorderSlot) {
	for !pa.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(monitorPollInterval):
		}
	}

	backoff := recorderBackoffInitial
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		// Create a FRESH recorder per attempt: PulseRecorder.Start is one-shot
		// and must only ever spawn a single pump per instance.
		stream, err := openMonitorStream(ctx)
		if err != nil {
			return
		}

		rec := &audio.PulseRecorder{Source: stream}
		rec.Start(ctx)
		slot.set(rec)

		attempt++
		if attempt == 1 {
			slog.Info("recorder started, reading from virtual_out.monitor")
		} else {
			slog.Info("recorder: reconnected to virtual_out.monitor", "attempt", attempt)
		}

		// Periodic debug summary distinguishes a live but silent recording
		// from a dead pump.
		summaryDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(recorderSummaryInterval)
			defer ticker.Stop()

			for {
				select {
				case <-summaryDone:
					return
				case <-ticker.C:
					slog.Debug("recorder: live",
						"chunks", rec.ChunksSent(), "dropped", rec.Dropped())
				}
			}
		}()

		// The pump can block in Read while the daemon is down: the monitor
		// stream marks the connection serverLost but never reports it to the
		// pipe, so rec.Done() alone won't fire. Watch daemon liveness and force
		// the stream closed to unblock the pump.
		lostDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(monitorPollInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-lostDone:
					return
				case <-ticker.C:
					if !pa.Ready() {
						rec.Stop()
						stream.Close()
						return
					}
				}
			}
		}()

		select {
		case <-ctx.Done():
			close(summaryDone)
			close(lostDone)
			rec.Stop()
			stream.Close()
			slot.set(nil)

			return
		case <-rec.Done():
			// Pump ended: the connection died (possibly forced by the watchdog
			// above) or the source failed. Stop and close before reconnecting.
			close(summaryDone)
			close(lostDone)
			rec.Stop()
			stream.Close()
			slot.set(nil)

			slog.Warn("recorder: stream lost, reconnecting",
				"attempt", attempt, "backoff", backoff)

			if !recorderSleep(ctx, backoff) {
				return
			}

			backoff = audio.NextBackoff(backoff, recorderBackoffMax)
		}
	}
}

// openMonitorStream opens the live monitor stream, retrying until one is
// established or ctx is cancelled.
func openMonitorStream(ctx context.Context) (io.ReadCloser, error) {
	server := pulseServerSocket()

	for {
		stream, err := audio.OpenMonitorStream(ctx, server)
		if err == nil {
			return stream, nil
		}

		slog.Warn("recorder: monitor open failed", "err", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(monitorPollInterval):
		}
	}
}

// pulseServerSocket is the native socket this shim's PulseAudio daemon listens
// on, as a server string the pulse client library accepts.
func pulseServerSocket() string {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = "/tmp/runtime"
	}

	return "unix:" + filepath.Join(runtime, "pulse", "native")
}

// recorderSleep sleeps for d, returning early when ctx is cancelled.
func recorderSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func setupLogger(level string) {
	var l slog.Level

	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
