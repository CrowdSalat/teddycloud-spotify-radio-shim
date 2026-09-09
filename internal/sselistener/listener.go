// Package sselistener consumes Teddycloud SSE events and translates them into
// Soloist WebSocket commands.
//
// Real Teddycloud event mapping (source: docs/research/teddycloud-sse-events.md):
//
//   - TagValid carries the tonie NFC UID hex (e.g. "E00403500EEA4BF2") and
//     signals a figurine was placed. The Spotify URI arrives separately via the
//     /stream?spotify_uri= request, never through SSE → Play("").
//   - playback "starting"/"started" is debug-ignored; "stopped" signals the
//     figurine was lifted → Pause().
//   - pressed "ear-big" → SkipNext(); "ear-small" → SkipPrev();
//     "ear-small-double" and unknown values are debug-ignored.
//   - There is no TagInvalid event on the real server.
//   - VolumeLevel, VolumedB, ContentAudioId, ContentTitle, knock, keep-alive
//     and all other events are irrelevant to the control path and debug-ignored.
//
// The wire format is: event:<name>\ndata: {"type":"<name>","data":"<value>"}\n\n
package sselistener

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/audio"
)

const (
	defaultBackoff     = 1 * time.Second
	defaultMaxBackoff  = 30 * time.Second
	defaultIDLETimeout = 2 * time.Minute
)

// CommandSender is the injectable command sink for translated events.
type CommandSender interface {
	Play(uri string) error
	Pause() error
	SkipNext() error
	SkipPrev() error
}

// Listener connects to a Teddycloud SSE stream and forwards events to the
// command sink.
type Listener struct {
	// URL is the base Teddycloud URL; "/api/sse" is appended.
	URL string
	// Commands receives translated soloist commands.
	Commands CommandSender

	// Backoff is the initial reconnect backoff. Zero uses the default.
	Backoff time.Duration
	// MaxBackoff caps the reconnect backoff. Zero uses the default.
	MaxBackoff time.Duration
	// Client performs the SSE GET. Nil uses http.DefaultClient.
	Client *http.Client
	// IDLETimeout closes a silent connection so a stalled stream is
	// detected and reconnected. Zero uses the default.
	IDLETimeout time.Duration
}

func (l *Listener) backoff() time.Duration {
	if l.Backoff > 0 {
		return l.Backoff
	}
	return defaultBackoff
}

func (l *Listener) maxBackoff() time.Duration {
	if l.MaxBackoff > 0 {
		return l.MaxBackoff
	}
	return defaultMaxBackoff
}

func (l *Listener) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return http.DefaultClient
}

func (l *Listener) idleTimeout() time.Duration {
	if l.IDLETimeout > 0 {
		return l.IDLETimeout
	}
	return defaultIDLETimeout
}

// sseURL returns the SSE endpoint for the configured base URL.
func (l *Listener) sseURL() string {
	return strings.TrimRight(l.URL, "/") + "/api/sse"
}

// Run subscribes to the SSE stream until ctx is cancelled, reconnecting with
// exponential backoff on every drop. It never returns an error; the loop only
// exits on cancellation.
func (l *Listener) Run(ctx context.Context) {
	backoff := l.backoff()
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		err := l.subscribeOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		attempt++
		slog.Warn("sselistener: connection lost",
			"err", err, "attempt", attempt, "backoff", backoff)

		if !sleep(ctx, backoff) {
			return
		}

		backoff = audio.NextBackoff(backoff, l.maxBackoff())
	}
}

// subscribeOnce opens one SSE subscription and processes events until the
// connection breaks or ctx is cancelled.
func (l *Listener) subscribeOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.sseURL(), nil)
	if err != nil {
		return err
	}

	resp, err := l.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sselistener: unexpected status %s", resp.Status)
	}

	return l.consume(ctx, resp.Body)
}

// consume reads the stream until EOF/error or ctx cancel.
func (l *Listener) consume(ctx context.Context, body io.Reader) error {
	done := make(chan error, 1)
	go func() {
		done <- l.readLoop(ctx, body)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// readLoop parses SSE frames (event:/data: lines, blank line terminates).
// Keep-alives, comments (": "), unknown events and malformed lines are skipped.
func (l *Listener) readLoop(ctx context.Context, body io.Reader) error {
	s := bufio.NewScanner(body)
	s.Buffer(nil, 64*1024) // tolerate generous data payloads

	var eventName string
	var data strings.Builder

	// A silent stream means a dead Teddycloud. Abort to force a reconnect.
	idle := time.NewTimer(l.idleTimeout())
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle.C:
			return errIdle
		default:
		}

		if !s.Scan() {
			// A clean EOF is still a drop from the subscribe loop's view.
			if err := s.Err(); err != nil {
				return err
			}
			return io.EOF
		}

		idle.Reset(l.idleTimeout())

		line := s.Text()
		switch {
		case line == "":
			if eventName != "" {
				l.dispatch(eventName, data.String())
			}
			eventName = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		default:
			// Comments (": ..."), "id:", "retry:" are transport noise.
			slog.Debug("sselistener: skipping SSE line", "line", line)
		}
	}
}

var errIdle = &textError{"sse stream idle"}

type textError struct{ msg string }

func (e *textError) Error() string { return e.msg }

// dispatch routes a complete SSE event to the command sink.
func (l *Listener) dispatch(eventName, data string) {
	switch eventName {
	case "TagValid":
		// Real Teddycloud: TagValid carries the tonie NFC UID hex, not a
		// Spotify URI. The URI arrives via /stream?spotify_uri=.
		l.send("play", "", func() error { return l.Commands.Play("") })
	case "playback":
		switch wrappedValue(data) {
		case "stopped":
			l.send("pause", "", l.Commands.Pause)
		case "starting", "started":
			slog.Debug("sselistener: ignoring playback state", "value", wrappedValue(data))
		default:
			slog.Debug("sselistener: ignoring playback event", "data", data)
		}
	case "pressed":
		// Real Teddycloud labels ear presses by physical placement:
		// "ear-big" is the volume-up (forward/right) ear,
		// "ear-small" the volume-down (left) ear.
		switch wrappedValue(data) {
		case "ear-big":
			l.send("skip_next", "", l.Commands.SkipNext)
		case "ear-small":
			l.send("skip_prev", "", l.Commands.SkipPrev)
		default:
			slog.Debug("sselistener: ignoring pressed event", "data", data)
		}
	default:
		// keep-alive, VolumeLevel, VolumedB, ContentTitle, ContentAudioId,
		// knock, and all other events are irrelevant to the control path.
		slog.Debug("sselistener: ignoring event", "event", eventName)
	}
}

// send logs the successful send and forwards the command. Command errors (e.g.
// a missing Soloist session) are logged and swallowed so the listener never
// stops resubscribing.
func (l *Listener) send(command, uri string, fn func() error) {
	if uri != "" {
		slog.Info("sselistener: " + command + " " + uri)
	} else {
		slog.Info("sselistener: " + command)
	}

	if err := fn(); err != nil {
		slog.Warn("sselistener: soloist command failed", "command", command, "err", err)
	}
}

// wrappedValue unwraps the JSON object {"type":...,"data":<value>} real
// Teddycloud emits, returning the inner string value. Non-object data passes
// through unchanged.
func wrappedValue(data string) string {
	s := strings.TrimSpace(data)
	if s == "" || !strings.HasPrefix(s, "{") {
		return s
	}

	var obj struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return s
	}

	raw := string(obj.Data)
	var str string
	if json.Unmarshal([]byte(raw), &str) == nil {
		return str
	}

	return raw
}

// sleep waits d or returns early when ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
