// Package server provides the shim's HTTP handlers.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/audio"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/librespot"
)

const willPlayTimeout = 10 * time.Second

// StreamHandler serves GET /stream?spotify_uri=<URI>.
//
// At most one stream is active at a time. A new request cancels the
// previous one before starting. After calling play, the handler waits
// for a will_play event from go-librespot before forwarding any audio
// bytes, ensuring stale PCM from the previous track is flushed.
type StreamHandler struct {
	client *librespot.Client
	events *librespot.Events
	audio  <-chan []byte
	logger *slog.Logger

	// mu protects cancelActive.
	mu           sync.Mutex
	cancelActive context.CancelFunc
}

// NewStreamHandler creates a StreamHandler.
func NewStreamHandler(
	client *librespot.Client,
	events *librespot.Events,
	audioCh <-chan []byte,
	logger *slog.Logger,
) *StreamHandler {
	return &StreamHandler{
		client: client,
		events: events,
		audio:  audioCh,
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

	// Cancel any previously active stream, then register this one.
	ctx := h.activate(r.Context())
	defer h.deactivate()

	// Subscribe to events before calling play so we don't miss will_play.
	eventCh, unsubscribe := h.events.Subscribe()
	defer unsubscribe()

	if err := h.client.Play(ctx, uri); err != nil {
		h.logger.Error("play failed", "uri", uri, "error", err)
		http.Error(w, "failed to start playback: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Wait for will_play for our URI before forwarding audio bytes.
	// This ensures stale PCM from the previous track is not sent.
	if !h.waitWillPlay(ctx, eventCh, uri) {
		h.logger.Info("timed out waiting for will_play, flushing and continuing",
			"uri", uri)
		// Flush whatever is in the channel before proceeding.
	}
	h.flushChannel()

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
		case <-ctx.Done():
			h.logger.Info("stream ended",
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

// activate cancels the previous active stream and registers this request's
// context. Returns a new context that is cancelled when the stream should end
// (either by a new request or by the client disconnecting).
func (h *StreamHandler) activate(parent context.Context) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cancelActive != nil {
		h.cancelActive()
	}

	ctx, cancel := context.WithCancel(parent)
	h.cancelActive = cancel
	return ctx
}

// deactivate clears the active cancel func if it is still this stream's.
func (h *StreamHandler) deactivate() {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Don't nil cancelActive — a newer stream may have already replaced it.
}

// waitWillPlay blocks until a will_play event for uri arrives or the context
// or timeout expires. Returns true if will_play was received.
func (h *StreamHandler) waitWillPlay(ctx context.Context, ch <-chan librespot.Event, uri string) bool {
	deadline := time.NewTimer(willPlayTimeout)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case ev, ok := <-ch:
			if !ok {
				return false
			}
			if ev.Type == librespot.EventWillPlay && ev.URI == uri {
				return true
			}
		}
	}
}

// flushChannel drains all currently buffered chunks from the audio channel
// non-blocking. Called after a track switch to discard stale PCM.
func (h *StreamHandler) flushChannel() {
	flushed := 0
	for {
		select {
		case _, ok := <-h.audio:
			if !ok {
				return // channel closed
			}
			flushed++
		default:
			if flushed > 0 {
				h.logger.Debug("flushed stale audio chunks", "count", flushed)
			}
			return
		}
	}
}
