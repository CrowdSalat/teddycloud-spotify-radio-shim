package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/librespot"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/radio"
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
	listenAddr := getEnv("LISTEN_ADDR", ":8080")
	logLevel := getEnv("LOG_LEVEL", "info")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	librespotProcess := librespot.NewProcess(configDir, logLevel, logger)

	// Prepare creates the FIFO and writes config. Must happen before opening
	// the FIFO read end and before Run() starts go-librespot.
	if err := librespotProcess.Prepare(); err != nil {
		logger.Error("librespot process prepare failed", "error", err)
		os.Exit(1)
	}

	// The FIFO channel carries audio chunks from go-librespot to the HTTP
	// handler. The channel is created here so the HTTP server can start
	// before the FIFO is open.
	audioCh := make(chan []byte, radio.ChannelCapacity)

	// Open the FIFO read end in a goroutine — os.Open blocks until
	// go-librespot opens the write end inside librespotProcess.Run() below.
	// Running concurrently avoids a deadlock.
	go func() {
		fifo, err := os.Open(librespot.FIFOPath)
		if err != nil {
			logger.Error("failed to open FIFO", "path", librespot.FIFOPath, "error", err)
			return
		}
		defer fifo.Close()
		reader := radio.NewReader(fifo, logger)
		if err := reader.RunInto(ctx, audioCh); err != nil && ctx.Err() == nil {
			logger.Error("audio reader stopped", "error", err)
		}
	}()

	librespotClient := librespot.NewClient("http://localhost:3678")

	eventStream := librespot.NewEventStream("http://localhost:3678", logger)
	go eventStream.Run(ctx)

	streamHandler := radio.NewPlaybackCoordinator(librespotClient, eventStream, audioCh, logger)

	mux := http.NewServeMux()
	mux.Handle("/stream", streamHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	go func() {
		logger.Info("HTTP server listening", "addr", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		if err := httpServer.Shutdown(context.Background()); err != nil {
			logger.Error("HTTP server shutdown error", "error", err)
		}
	}()

	if err := librespotProcess.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("librespot process failed", "error", err)
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
