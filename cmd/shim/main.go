// Command shim is the teddycloud-spotify-radio-shim orchestrator.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/config"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/server"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/soloist"
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

	if _, err := bm.Resolve(); err != nil {
		slog.Error("soloist binary unavailable", "err", err)
		srv.SetUnhealthy("soloist_missing")
	}

	if err := srv.Run(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
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
