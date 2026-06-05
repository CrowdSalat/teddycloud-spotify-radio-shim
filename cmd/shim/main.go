package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/supervisor"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(Version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(getEnv("LOG_LEVEL", "info")),
	}))

	logger.Info("starting teddycloud-spotify-radio-shim", "version", Version)

	configDir := getEnv("LIBRESPOT_CONFIG_DIR", "/config")
	logLevel := getEnv("LOG_LEVEL", "info")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	sv := supervisor.New(configDir, logLevel, logger)

	if err := sv.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("supervisor failed", "error", err)
		os.Exit(1)
	}

	logger.Info("shutdown complete")
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
