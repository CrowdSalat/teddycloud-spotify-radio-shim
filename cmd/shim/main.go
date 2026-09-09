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
	"syscall"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/audio"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/config"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/process"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/server"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/soloist"
)

const (
	pairingBackoffInitial = 5 * time.Second
	pairingBackoffMax     = 60 * time.Second

	// monitorPollInterval is how often the recorder waits for the PulseAudio
	// daemon to become ready and retries a failed monitor connect.
	monitorPollInterval = 250 * time.Millisecond
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}

	setupLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := server.New(cfg.ListenAddr)

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
	go runRecorder(ctx, pa)

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
			go runSupervisor(ctx, srv, cfg, binPath, bm)
		} else {
			srv.SetUnhealthy("soloist_unpaired")
			slog.Warn(fmt.Sprintf(
				"No Soloist session found. Pairing now — open the Spotify app and select the device %q from the device picker.",
				cfg.SoloistDeviceName,
			))

			go pairThenSupervisor(ctx, srv, cfg, binPath, bm)
		}
	}

	if err := srv.Run(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// pairThenSupervisor drives the one-time pairing, then hands over to the
// Connect-mode supervisor once a session is stored.
func pairThenSupervisor(ctx context.Context, srv *server.Server, cfg *config.Config, binPath string, bm *soloist.BinaryManager) {
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
		runSupervisor(ctx, srv, cfg, binPath, bm)
	case soloist.PairExpired:
		srv.SetUnhealthy("soloist_expired")
		slog.Error("soloist binary expired, pairing halted")
	}
}

// runSupervisor runs the Soloist Connect-mode subprocess and WebSocket
// supervisor in the current goroutine until ctx is cancelled or soloist exits
// with a non-retryable code.
func runSupervisor(ctx context.Context, srv *server.Server, cfg *config.Config, binPath string, bm *soloist.BinaryManager) {
	supervisor := &soloist.Supervisor{
		BinaryPath:    binPath,
		DeviceName:    cfg.SoloistDeviceName,
		APIKey:        cfg.SoloistAPIKey,
		DataDir:       cfg.SoloistDataDir,
		CacheDir:      cfg.SoloistCacheDir,
		Manager:       process.ExecManager{},
		BinaryManager: bm,
		Health:        func(reason string) { srv.SetUnhealthy(reason) },
	}

	supervisor.Run(ctx)
}

// runRecorder waits for the PulseAudio daemon to become ready, then keeps the
// live virtual_out.monitor stream recording through PulseRecorder until ctx is
// cancelled. It never blocks the Soloist pairing or supervisor branches.
func runRecorder(ctx context.Context, pa *audio.PulseAudio) {
	for !pa.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(monitorPollInterval):
		}
	}

	stream, err := openMonitorStream(ctx)
	if err != nil {
		return
	}
	defer stream.Close()

	rec := &audio.PulseRecorder{Source: stream}
	defer rec.Stop()
	rec.Start(ctx)

	slog.Info("recorder started, reading from virtual_out.monitor")

	<-ctx.Done()
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
