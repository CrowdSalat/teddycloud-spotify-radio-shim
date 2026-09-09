package soloist_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/soloist"
	"github.com/gorilla/websocket"
)

func newSupervisor(dir string, m *stubManager, health chan string) *soloist.Supervisor {
	s := &soloist.Supervisor{
		BinaryPath:    "/opt/soloist",
		DeviceName:    "shim-test",
		APIKey:        "test-key",
		DataDir:       dir,
		CacheDir:      filepath.Join(dir, "cache"),
		Manager:       m,
		StartBackoff:  time.Millisecond,
		MaxBackoff:    time.Millisecond,
		WSPortTimeout: 2 * time.Second,
	}
	if health != nil {
		s.Health = func(reason string) {
			select {
			case health <- reason:
			default:
			}
		}
	}

	return s
}

// startTestWSServer starts a minimal Soloist-like WebSocket server. Each
// accepted connection reads one activate command into activateCh, sends the
// handshake event, then relays any client messages into serverMsgCh.
func startTestWSServer(t *testing.T) (port int, activateCh chan []byte, msgCh chan []byte) {
	t.Helper()

	activateCh = make(chan []byte, 1)
	msgCh = make(chan []byte, 8)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	upgrader := &websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, activate, err := conn.ReadMessage()
		if err == nil {
			activateCh <- activate
		}

		if err := conn.WriteMessage(websocket.TextMessage,
			[]byte(`{"type":"auth_state","status":"logged-in"}`)); err != nil {
			return
		}

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}

			select {
			case msgCh <- msg:
			default:
			}
		}
	})}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = srv.Close()
	})

	return ln.Addr().(*net.TCPAddr).Port, activateCh, msgCh
}

func TestSupervisor_ActivatesOnConnect(t *testing.T) {
	dir := t.TempDir()
	port, activateCh, _ := startTestWSServer(t)

	if err := os.WriteFile(filepath.Join(dir, "ws.port"), []byte(strconv.Itoa(port)), 0o644); err != nil {
		t.Fatalf("write ws.port: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ws.addr"), []byte("127.0.0.1"), 0o644); err != nil {
		t.Fatalf("write ws.addr: %v", err)
	}

	m := &stubManager{procs: []*stubProcess{{wait: make(chan struct{})}}}
	health := make(chan string, 2)
	s := newSupervisor(dir, m, health)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	select {
	case activate := <-activateCh:
		if got, want := string(activate), `{"type":"command","command":"activate"}`; got != want {
			t.Fatalf("activate: got %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for activate command")
	}

	waitFor(t, 2*time.Second, func() bool {
		select {
		case reason := <-health:
			return reason == ""
		default:
			return false
		}
	})

	cancel()
	waitFor(t, 2*time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}

func TestSupervisor_ExpiredNoRestart(t *testing.T) {
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin", "soloist")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := &stubManager{procs: []*stubProcess{{err: exitError(t, 10)}}}
	health := make(chan string, 2)
	s := newSupervisor(dir, m, health)
	s.BinaryManager = &soloist.BinaryManager{DataDir: dir}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.Run(ctx)

	if m.started() != 1 {
		t.Errorf("started: got %d, want 1 (no restart on expiry)", m.started())
	}

	select {
	case reason := <-health:
		if reason != "soloist_expired" {
			t.Errorf("health: got %q, want %q", reason, "soloist_expired")
		}
	default:
		t.Error("health: expected soloist_expired to be reported")
	}

	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("expected expired binary at %s to be deleted", bin)
	}
}

func TestSupervisor_RestartOnErrorExit(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{
		{err: exitError(t, 1)},
		{wait: make(chan struct{})},
	}}
	s := newSupervisor(dir, m, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitFor(t, 3*time.Second, func() bool { return m.started() >= 2 })

	cancel()
	waitFor(t, 3*time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})

	if m.started() != 2 {
		t.Errorf("started: got %d, want 2 (restart after non-zero exit)", m.started())
	}
}

func TestSupervisor_WSPortTimeoutRestarts(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{
		{wait: make(chan struct{})},
		{wait: make(chan struct{})},
	}}
	s := newSupervisor(dir, m, nil)
	s.WSPortTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitFor(t, 3*time.Second, func() bool { return m.started() >= 2 })

	cancel()
	waitFor(t, 3*time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}
