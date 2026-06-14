package radio

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/librespot"
)

// PlaybackCoordinator serves GET /stream?spotify_uri=<URI>.
//
// Play and pause are driven by backpressure: Teddycloud opens the HTTP
// connection to play and closes it to pause. The FIFO fills naturally when
// nobody reads, throttling go-librespot without any API call.
//
// For hot-swap (different URI while something is playing), the coordinator
// calls pauseWithDrain to safely pause go-librespot — avoiding the pipe
// driver deadlock — before starting the new track.
//
// At most one stream is active at a time. A new request cancels the previous.
type PlaybackCoordinator struct {
	client *librespot.Client
	chunks chan []byte
	logger *slog.Logger

	mu           sync.Mutex
	lastURI      string
	cancelActive context.CancelFunc
}

// NewPlaybackCoordinator creates a PlaybackCoordinator.
// The caller must start a Reader goroutine that sends to Chunks().
func NewPlaybackCoordinator(client *librespot.Client, logger *slog.Logger) *PlaybackCoordinator {
	return &PlaybackCoordinator{
		client: client,
		chunks: make(chan []byte, ChunkBuf),
		logger: logger,
	}
}

// Chunks returns the write end of the audio channel. The FIFO reader
// goroutine sends decoded PCM chunks here; the HTTP handler reads them.
func (c *PlaybackCoordinator) Chunks() chan<- []byte {
	return c.chunks
}

func (c *PlaybackCoordinator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("spotify_uri")
	if uri == "" {
		http.Error(w, "missing spotify_uri query parameter", http.StatusBadRequest)
		return
	}

	c.logger.Info("stream requested", "uri", uri, "remote", r.RemoteAddr)

	ctx := c.activate(r.Context())
	defer c.deactivate()

	c.mu.Lock()
	sameURI := c.lastURI == uri
	wasPlaying := c.lastURI != ""
	if !sameURI {
		c.lastURI = uri
	}
	c.mu.Unlock()

	if !sameURI {
		if wasPlaying {
			// Hot-swap: safely pause go-librespot (draining chunks concurrently
			// to prevent the pipe driver deadlock), then start the new track.
			if err := c.pauseWithDrain(ctx); err != nil && ctx.Err() == nil {
				c.logger.Warn("pause failed during hot-swap", "error", err)
			}
		}
		if err := c.client.Play(ctx, uri); err != nil {
			c.logger.Error("play failed", "uri", uri, "error", err)
			http.Error(w, "play failed: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Same URI: backpressure releases the moment we start reading — go-librespot
	// resumes from exactly where it was blocked. No API call needed.

	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(http.StatusOK)

	if err := WriteStreamingWAVHeader(w); err != nil {
		c.logger.Error("failed to write WAV header", "error", err)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	bytesSent := 0

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("stream ended",
				"uri", uri,
				"bytes_sent", bytesSent,
				"remote", r.RemoteAddr,
			)
			return
		case chunk, ok := <-c.chunks:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				c.logger.Info("client disconnected",
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

// activate cancels the previous active stream and registers this one.
func (c *PlaybackCoordinator) activate(parent context.Context) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelActive != nil {
		c.cancelActive()
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancelActive = cancel
	return ctx
}

func (c *PlaybackCoordinator) deactivate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Intentionally empty: cancelActive stays set for the next activate call.
}

// pauseWithDrain sends a pause command to go-librespot while concurrently
// draining the chunk channel. This is required because go-librespot's pipe
// driver holds out.lock across file.Write() calls. If the FIFO is full,
// Write() blocks and all API calls (including pause) deadlock.
//
// Draining the channel unblocks the reader goroutine, which reads from the
// FIFO, which lets go-librespot's Write() complete, which releases out.lock,
// which allows the pause command to proceed.
func (c *PlaybackCoordinator) pauseWithDrain(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- c.client.Pause(ctx)
	}()

	for {
		select {
		case err := <-done:
			return err
		case <-c.chunks:
			// Drain one chunk: unblocks the reader goroutine, which reads
			// from the FIFO, freeing space for go-librespot to write and
			// release out.lock — allowing the pause call to proceed.
		}
	}
}
