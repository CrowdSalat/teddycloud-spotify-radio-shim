package main

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

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/toniebox"
)

// observedEvent is one SSE event as teddyCloud published it, with the moment the
// mock saw it. The timestamp is what makes the timing assertions possible: the
// shim's own logs are the other half of the picture, and the two have to be
// correlated by hand.
type observedEvent struct {
	At   time.Time `json:"at"`
	Name string    `json:"event"`
	Data string    `json:"data"`
}

// eventHandler receives every observed event in arrival order.
type eventHandler func(observedEvent)

// sseParser turns teddyCloud's wire format into callbacks.
//
// The format is `event:<name>\ndata: {"type":"<name>","data":"<value>"}\n\n`
// (docs/research/teddycloud-sse-events.md). The parser tolerates an optional
// space after the colon and a bare `data:` line, because the point of the
// observer is to notice what teddyCloud actually sent rather than to enforce what
// it is supposed to send.
type sseParser struct {
	name string
	data strings.Builder
}

// feed consumes one line, returning a completed event when the blank separator
// arrives.
func (p *sseParser) feed(line string) (observedEvent, bool) {
	switch {
	case line == "":
		if p.name == "" {
			return observedEvent{}, false
		}
		evt := observedEvent{Name: p.name, Data: strings.TrimSpace(p.data.String())}
		p.name = ""
		p.data.Reset()
		return evt, true
	case strings.HasPrefix(line, ":"):
		return observedEvent{}, false
	case strings.HasPrefix(line, "event:"):
		p.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		p.data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return observedEvent{}, false
}

// payload returns the inner data string of an event whose data field is a JSON
// object of the shape teddyCloud uses. Raw-log frames carry a nested object
// rather than a string (313 of 741 events in the live capture), so the inner
// value is returned as raw JSON when it is not a plain string.
func (e observedEvent) payload() string {
	var wrapper struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(e.Data), &wrapper); err != nil {
		return e.Data
	}
	if wrapper.Type == "" {
		return e.Data
	}

	var s string
	if err := json.Unmarshal(wrapper.Data, &s); err == nil {
		return s
	}
	return string(wrapper.Data)
}

// observer consumes teddyCloud's SSE feed for the lifetime of ctx, reconnecting
// with backoff.
type observer struct {
	url    string
	client *http.Client
	handle eventHandler
}

func newObserver(baseURL string, client *http.Client, handle eventHandler) *observer {
	return &observer{
		url:    strings.TrimSuffix(baseURL, "/") + "/api/sse",
		client: client,
		handle: handle,
	}
}

const (
	observerMinBackoff = 1 * time.Second
	observerMaxBackoff = 30 * time.Second
)

func (o *observer) run(ctx context.Context) {
	backoff := observerMinBackoff

	for ctx.Err() == nil {
		if err := o.consume(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("mock-toniebox: SSE feed dropped", "err", err, "retry_in", backoff)
		}

		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, observerMaxBackoff)
	}
}

func (o *observer) consume(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url, nil)
	if err != nil {
		return fmt.Errorf("building SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", toniebox.UserAgent)

	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", o.url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("mock-toniebox: closing SSE response", "err", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE feed returned %d", resp.StatusCode)
	}
	slog.Info("mock-toniebox: observing SSE feed", "url", o.url)

	var parser sseParser
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if evt, ok := parser.feed(scanner.Text()); ok {
			evt.At = time.Now()
			o.handle(evt)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading SSE feed: %w", err)
	}
	return io.EOF
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
