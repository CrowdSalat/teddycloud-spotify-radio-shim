package librespot

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	// FIFOPath is the named pipe go-librespot writes PCM audio to.
	// The read end must be opened before Run() starts go-librespot.
	FIFOPath = "/tmp/spotify.fifo"

	librespotBin   = "go-librespot"
	librespotAPI   = "http://localhost:3678"
	readyTimeout   = 30 * time.Second
	readyPollDelay = 500 * time.Millisecond
	shutdownWait   = 5 * time.Second
	backoffInit    = 1 * time.Second
	backoffMax     = 30 * time.Second
)

// Process manages the go-librespot child process lifecycle.
type Process struct {
	configDir string
	logLevel  string
	logger    *slog.Logger
}

// NewProcess creates a Process. configDir is where go-librespot's config and
// session state live (typically a PVC mount). logLevel is passed to
// go-librespot's config.
func NewProcess(configDir, logLevel string, logger *slog.Logger) *Process {
	return &Process{
		configDir: configDir,
		logLevel:  logLevel,
		logger:    logger,
	}
}

// Prepare creates the FIFO and writes go-librespot's config.
// It must be called before Run(). After Prepare returns, open the FIFO
// read end (FIFOPath) in a goroutine before calling Run() — go-librespot
// opens the write end with O_NONBLOCK and errors if no reader exists.
func (s *Process) Prepare() error {
	if err := s.createFIFO(); err != nil {
		return fmt.Errorf("create fifo: %w", err)
	}
	return WriteConfig(s.configDir, FIFOPath, s.logLevel)
}

// Run manages go-librespot's lifecycle until ctx is cancelled.
// Call Prepare() and open the FIFO read end before calling Run().
func (s *Process) Run(ctx context.Context) error {
	backoff := backoffInit

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := s.runOnce(ctx)

		if ctx.Err() != nil {
			// Shutdown requested — don't restart.
			return ctx.Err()
		}

		s.logger.Warn("go-librespot exited, restarting",
			"error", err,
			"backoff", backoff.String(),
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, backoffMax)
	}
}

// runOnce starts go-librespot, waits for readiness, then blocks until it exits
// or ctx is cancelled.
func (s *Process) runOnce(ctx context.Context) error {
	cmd := exec.Command(librespotBin, "--config_dir", s.configDir)
	cmd.Stdout = &logWriter{logger: s.logger, level: slog.LevelInfo, prefix: "librespot"}
	cmd.Stderr = &logWriter{logger: s.logger, level: slog.LevelWarn, prefix: "librespot"}

	s.logger.Info("starting go-librespot", "config_dir", s.configDir)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Channel that closes when the process exits.
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()

	// Wait for API readiness or early exit.
	ready, err := s.waitForReady(ctx, exited)
	if err != nil {
		s.killProcess(cmd)
		return fmt.Errorf("readiness: %w", err)
	}
	if !ready {
		s.killProcess(cmd)
		return fmt.Errorf("process exited before becoming ready")
	}

	s.logger.Info("go-librespot is ready")

	// Block until exit or shutdown.
	select {
	case err := <-exited:
		return fmt.Errorf("process exited: %w", err)
	case <-ctx.Done():
		s.logger.Info("shutting down go-librespot")
		return s.shutdownProcess(cmd, exited)
	}
}

// waitForReady polls the go-librespot API until playback_ready is true,
// the context is cancelled, or the process exits.
func (s *Process) waitForReady(ctx context.Context, exited <-chan error) (bool, error) {
	deadline := time.After(readyTimeout)

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case err := <-exited:
			return false, fmt.Errorf("exited during startup: %w", err)
		case <-deadline:
			return false, fmt.Errorf("readiness timeout after %s", readyTimeout)
		case <-time.After(readyPollDelay):
			if s.checkReady() {
				return true, nil
			}
		}
	}
}

// checkReady verifies go-librespot's API server is accepting TCP connections.
// We use a raw TCP dial rather than an HTTP request because GET / blocks
// until authentication completes and never returns in unauthenticated state.
func (s *Process) checkReady() bool {
	conn, err := net.DialTimeout("tcp", "localhost:3678", 1*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// shutdownProcess sends SIGTERM and waits for graceful exit, then SIGKILL.
func (s *Process) shutdownProcess(cmd *exec.Cmd, exited <-chan error) error {
	if cmd.Process == nil {
		return nil
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)

	select {
	case err := <-exited:
		return err
	case <-time.After(shutdownWait):
		s.logger.Warn("go-librespot did not exit in time, killing")
		_ = cmd.Process.Kill()
		return <-exited
	}
}

// killProcess force-kills the process (used when startup fails).
func (s *Process) killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		// Drain the wait to avoid zombies.
		_ = cmd.Wait()
	}
}

// createFIFO creates the named pipe if it doesn't already exist.
func (s *Process) createFIFO() error {
	// Remove stale FIFO from a previous run.
	_ = os.Remove(FIFOPath)

	if err := syscall.Mkfifo(FIFOPath, 0660); err != nil {
		return fmt.Errorf("mkfifo %s: %w", FIFOPath, err)
	}

	s.logger.Info("created FIFO", "path", FIFOPath)
	return nil
}

// logWriter adapts go-librespot's stdout/stderr to slog.
type logWriter struct {
	logger *slog.Logger
	level  slog.Level
	prefix string
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	w.logger.Log(context.Background(), w.level, string(p), "source", w.prefix)
	return len(p), nil
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
