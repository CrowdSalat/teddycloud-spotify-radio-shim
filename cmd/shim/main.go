// Command shim is the teddycloud-spotify-radio-shim orchestrator.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/config"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/process"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/server"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/soloist"
)

const (
	pairingBackoffInitial = 5 * time.Second
	pairingBackoffMax     = 60 * time.Second
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

	// Phase 2a: resolve Soloist binary before starting anything else.
	bm := &soloist.BinaryManager{
		ExplicitPath: cfg.SoloistBin,
		DataDir:      cfg.SoloistDataDir,
	}

	binPath, binErr := bm.Resolve()

	// Phase 2b: session check + pairing gate. Only applies when the binary is
	// present; a missing binary keeps soloist_missing and must not be
	// overwritten by the pairing gate.
	if binErr != nil {
		slog.Error("soloist binary unavailable", "err", binErr)
		srv.SetUnhealthy("soloist_missing")
	} else {
		drivePairing(ctx, srv, &soloist.Pairer{
			BinaryPath:    binPath,
			DeviceName:    cfg.SoloistDeviceName,
			APIKey:        cfg.SoloistAPIKey,
			DataDir:       cfg.SoloistDataDir,
			CacheDir:      cfg.SoloistCacheDir,
			Manager:       process.ExecManager{},
			Checker:       &soloist.SessionChecker{DataDir: cfg.SoloistDataDir},
			BinaryManager: bm,
		})
	}

	if err := srv.Run(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// drivePairing checks for a stored Soloist session and, when absent, spawns
// the pair process itself. Pairing runs in a goroutine so /healthz serves
// immediately; soloist_unpaired clears once the session is stored.
func drivePairing(ctx context.Context, srv *server.Server, pairer *soloist.Pairer) {
	if pairer.Checker.Check() == soloist.StateReady {
		slog.Info("soloist session found, ready")

		return
	}

	srv.SetUnhealthy("soloist_unpaired")
	slog.Warn(fmt.Sprintf(
		"No Soloist session found. Pairing now — open the Spotify app and select the device %q from the device picker.",
		pairer.DeviceName,
	))

	go func() {
		switch soloist.Drive(ctx, pairer.Pair, pairingBackoffInitial, pairingBackoffMax) {
		case soloist.PairDone:
			srv.SetUnhealthy("")
			slog.Info("soloist paired, session stored")
		case soloist.PairExpired:
			srv.SetUnhealthy("soloist_expired")
			slog.Error("soloist binary expired, pairing halted")
		}
	}()
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
