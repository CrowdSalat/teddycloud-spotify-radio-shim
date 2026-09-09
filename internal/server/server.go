// Package server implements the shim HTTP server.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
)

// ChunkSource yields PCM chunks for /stream consumers.
type ChunkSource interface {
	Chunks() <-chan []byte
}

// Server is the shim HTTP server.
type Server struct {
	addr string
	mux  *http.ServeMux

	src  func() ChunkSource
	play func(uri string) error

	mu              sync.RWMutex
	unhealthyReason string // non-empty → healthz returns 503
}

// New creates a new Server listening on addr.
func New(addr string, src func() ChunkSource, play func(uri string) error) *Server {
	s := &Server{
		addr: addr,
		mux:  http.NewServeMux(),
		src:  src,
		play: play,
	}
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/stream", s.handleStream)

	return s
}

// SetUnhealthy marks the server as unhealthy with a machine-readable reason.
// Reason is surfaced as {"error":"<reason>"} with HTTP 503.
// Pass an empty string to clear the unhealthy state.
func (s *Server) SetUnhealthy(reason string) {
	s.mu.Lock()
	s.unhealthyReason = reason
	s.mu.Unlock()
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.addr,
		Handler: s.mux,
	}

	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
	}()

	slog.Info("server listening", "addr", s.addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}
