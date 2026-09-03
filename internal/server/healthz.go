package server

import (
	"encoding/json"
	"net/http"
)

// healthError is the JSON body for unhealthy responses.
type healthError struct {
	Error string `json:"error"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	reason := s.unhealthyReason
	s.mu.RUnlock()

	if reason != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(healthError{Error: reason})

		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
