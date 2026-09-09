// Command mock-teddycloud is a minimal Teddycloud SSE mock for local testing.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	keepAliveInterval = 15 * time.Second
	eventPath         = "/api/event"
	ssePath           = "/api/sse"
)

var (
	addrFlag = flag.String("addr", ":9090", "HTTP listen address")
	uriFlag  = flag.String("uri", "", "Spotify URI for figurine-placed events")
)

// event represents an SSE event dispatched to subscribers.
type event struct {
	Name string
	Data string
}

// hub manages SSE subscribers and broadcasts.
type hub struct {
	mu      sync.Mutex
	clients map[chan event]struct{}
	uri     string
}

func newHub(uri string) *hub {
	return &hub{
		clients: make(map[chan event]struct{}),
		uri:     uri,
	}
}

func (h *hub) subscribe(ch chan event) {
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *hub) broadcast(evt event) {
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- evt:
		default:
		}
	}
	h.mu.Unlock()
}

// trigger fires an event, correlating button actions to SSE event names.
// The figurine-placed event includes the configured URI.
func (h *hub) trigger(action string) {
	slog.Info("mock-teddycloud: trigger", "action", action)

	switch action {
	case "figurine-placed":
		h.broadcast(event{Name: "figurine-placed", Data: jsonStr(h.uri)})
	case "figurine-lifted":
		h.broadcast(event{Name: "figurine-lifted", Data: jsonStr("")})
	case "right-ear-slap":
		h.broadcast(event{Name: "right-ear-slap", Data: jsonStr("")})
	case "left-ear-slap":
		h.broadcast(event{Name: "left-ear-slap", Data: jsonStr("")})
	default:
		slog.Warn("mock-teddycloud: unknown action", "action", action)
	}
}

// jsonStr wraps s in a JSON string (quoted, escaped).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// handleIndex serves the control page with four buttons.
func handleIndex(h *hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, indexHTML)
	}
}

// handleEvent triggers an SSE event from POST/?action= query param.
func handleEvent(h *hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		if action == "" {
			http.Error(w, "missing ?action=", http.StatusBadRequest)
			return
		}
		h.trigger(action)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSSE streams events to connected clients.
func handleSSE(h *hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := make(chan event, 16)
		h.subscribe(ch)
		defer h.unsubscribe(ch)

		// Flush headers before entering the loop.
		flusher.Flush()

		keepAlive := time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepAlive.C:
				_, _ = fmt.Fprintf(w, "event: keep-alive\ndata: {\"type\":\"keep-alive\",\"data\":\"\"}\n\n")
				flusher.Flush()
			case evt := <-ch:
				_, _ = fmt.Fprintf(w, "event: %s\ndata: {\"type\":\"%s\",\"data\":%s}\n\n",
					evt.Name, evt.Name, evt.Data)
				flusher.Flush()
			}
		}
	}
}

func main() {
	flag.Parse()

	hub := newHub(*uriFlag)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex(hub))
	mux.HandleFunc(eventPath, handleEvent(hub))
	mux.HandleFunc(ssePath, handleSSE(hub))

	slog.Info("mock-teddycloud: listening", "addr", *addrFlag, "uri", *uriFlag)

	if err := http.ListenAndServe(*addrFlag, mux); err != nil {
		slog.Error("mock-teddycloud: server error", "err", err)
	}
}

const indexHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Mock Teddycloud</title>
<style>
body { font-family: sans-serif; margin: 2rem; }
button { display: block; margin: 0.5rem 0; font-size: 1rem; padding: 0.6rem 1rem; }
</style>
</head>
<body>
<h1>Mock Teddycloud Control</h1>
<p>Click a button to emit the corresponding SSE event.</p>
<form method="post" action="/api/event?action=figurine-placed"><button type="submit">Place figurine</button></form>
<form method="post" action="/api/event?action=figurine-lifted"><button type="submit">Lift figurine</button></form>
<form method="post" action="/api/event?action=right-ear-slap"><button type="submit">Right ear</button></form>
<form method="post" action="/api/event?action=left-ear-slap"><button type="submit">Left ear</button></form>
</body>
</html>
`
