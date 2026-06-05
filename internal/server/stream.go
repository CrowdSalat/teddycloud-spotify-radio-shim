// Package server provides the shim's HTTP handlers.
package server

import (
	"log/slog"
	"net/http"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/audio"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/librespot"
)

// StreamHandler serves GET /stream?spotify_uri=<URI>.
// It tells go-librespot to play the URI, writes a WAV header, then
// streams raw PCM chunks from the audio channel to the HTTP response.
type StreamHandler struct {
	client *librespot.Client
	audio  <-chan []byte
	logger *slog.Logger
}

// NewStreamHandler creates a StreamHandler.
func NewStreamHandler(client *librespot.Client, audio <-chan []byte, logger *slog.Logger) *StreamHandler {
	return &StreamHandler{
		client: client,
		audio:  audio,
		logger: logger,
	}
}

func (h *StreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("spotify_uri")
	if uri == "" {
		http.Error(w, "missing spotify_uri query parameter", http.StatusBadRequest)
		return
	}

	h.logger.Info("stream requested", "uri", uri, "remote", r.RemoteAddr)

	if err := h.client.Play(r.Context(), uri); err != nil {
		h.logger.Error("play failed", "uri", uri, "error", err)
		http.Error(w, "failed to start playback: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", audio.ContentType)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	if err := audio.WriteStreamingWAVHeader(w); err != nil {
		h.logger.Error("failed to write WAV header", "error", err)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	bytesSent := 0

	for {
		select {
		case <-r.Context().Done():
			h.logger.Info("stream disconnected",
				"uri", uri,
				"bytes_sent", bytesSent,
				"remote", r.RemoteAddr,
			)
			return
		case chunk, ok := <-h.audio:
			if !ok {
				h.logger.Info("audio channel closed", "uri", uri)
				return
			}
			if _, err := w.Write(chunk); err != nil {
				h.logger.Info("write error, client disconnected",
					"uri", uri,
					"error", err,
				)
				return
			}
			bytesSent += len(chunk)
			if canFlush {
				flusher.Flush()
			}
		}
	}
}
