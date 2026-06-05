package librespot

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// EventType is a go-librespot WebSocket event name.
type EventType string

const (
	EventWillPlay EventType = "will_play"
	EventPlaying  EventType = "playing"
	EventPaused   EventType = "paused"
	EventStopped  EventType = "stopped"
	EventMetadata EventType = "metadata"
)

// Event is a parsed go-librespot WebSocket event.
type Event struct {
	Type EventType
	URI  string // set for will_play, playing, paused
}

// MarshalWire encodes an Event into the JSON wire format go-librespot emits.
// Used in tests to inject synthetic events into the WebSocket.
func (e Event) MarshalWire() ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": string(e.Type),
		"data": map[string]string{"uri": e.URI},
	})
}

// rawEvent is the wire format from go-librespot's /events WebSocket.
type rawEvent struct {
	Type string `json:"type"`
	Data struct {
		URI        string `json:"uri"`
		ContextURI string `json:"context_uri"`
	} `json:"data"`
}

const (
	eventsReconnectDelay = 2 * time.Second
)

// Events is a long-lived WebSocket client for go-librespot's /events endpoint.
// It reconnects automatically on drops and fans events out to subscribers.
type Events struct {
	url    string
	logger *slog.Logger

	mu   sync.Mutex
	subs []chan Event
}

// NewEvents creates an Events client. baseURL is the go-librespot HTTP base
// URL (e.g. "http://localhost:3678"); it is converted to a WebSocket URL.
func NewEvents(baseURL string, logger *slog.Logger) *Events {
	wsURL := "ws" + baseURL[len("http"):] + "/events"
	return &Events{
		url:    wsURL,
		logger: logger,
	}
}

// Run connects to the WebSocket, reads events, and fans them out to
// subscribers until ctx is cancelled. Reconnects automatically on drops.
func (e *Events) Run(ctx context.Context) {
	for {
		if err := e.connect(ctx); err != nil {
			e.logger.Warn("events WebSocket disconnected", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(eventsReconnectDelay):
		}
	}
}

// Subscribe returns a channel that receives events. The channel is buffered
// with capacity 8. Unsubscribe by calling the returned cancel func.
func (e *Events) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 8)

	e.mu.Lock()
	e.subs = append(e.subs, ch)
	e.mu.Unlock()

	cancel := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for i, s := range e.subs {
			if s == ch {
				e.subs = append(e.subs[:i], e.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}

	return ch, cancel
}

// WaitFor blocks until an event matching the predicate arrives or ctx is
// cancelled. It creates and removes a subscription internally.
func (e *Events) WaitFor(ctx context.Context, match func(Event) bool) (Event, bool) {
	ch, cancel := e.Subscribe()
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return Event{}, false
		case ev, ok := <-ch:
			if !ok {
				return Event{}, false
			}
			if match(ev) {
				return ev, true
			}
		}
	}
}

func (e *Events) connect(ctx context.Context) error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, e.url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	e.logger.Info("connected to go-librespot events WebSocket")

	done := make(chan error, 1)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			e.dispatch(msg)
		}
	}()

	select {
	case <-ctx.Done():
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (e *Events) dispatch(msg []byte) {
	var raw rawEvent
	if err := json.Unmarshal(msg, &raw); err != nil {
		e.logger.Debug("failed to parse event", "error", err, "msg", string(msg))
		return
	}

	ev := Event{
		Type: EventType(raw.Type),
		URI:  raw.Data.URI,
	}

	e.mu.Lock()
	subs := make([]chan Event, len(e.subs))
	copy(subs, e.subs)
	e.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Subscriber not keeping up — drop the event rather than block.
		}
	}
}
