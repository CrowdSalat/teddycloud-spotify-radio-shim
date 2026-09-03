// Package server implements the shim HTTP server.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
)

// Server is the shim HTTP server.
type Server struct {
	addr string
	mux  *http.ServeMux

	mu              sync.RWMutex
	unhealthyReason string // non-empty → healthz returns 503
}

// New creates a new Server listening on addr.
func New(addr string) *Server {
	s := &Server{
		addr: addr,
		mux:  http.NewServeMux(),
	}
	s.mux.HandleFunc("/healthz", s.handleHealthz)

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
