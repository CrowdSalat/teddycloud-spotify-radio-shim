// Package server implements the shim HTTP server.
package server

import (
	"context"
	"log/slog"
	"net/http"
)

// Server is the shim HTTP server.
type Server struct {
	addr string
	mux  *http.ServeMux
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
