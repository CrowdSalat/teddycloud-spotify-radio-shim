package soloist

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCommandConnector_Play(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	upgrader := &websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}

	msgCh := make(chan []byte, 4)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msgCh <- msg
		}
	})}

	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	cc := &CommandConnector{}

	if err := cc.Play("spotify:album:X"); err == nil {
		t.Fatal("Play before attach should fail")
	}

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial("ws://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	cc.attach(conn)

	if err := cc.Play("spotify:album:X"); err != nil {
		t.Fatalf("Play: %v", err)
	}

	select {
	case msg := <-msgCh:
		want := `{"type":"command","command":"play","uri":"spotify:album:X"}`
		if string(msg) != want {
			t.Fatalf("received: got %q, want %q", msg, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for play message")
	}

	cc.detach()
	conn.Close()

	if err := cc.Play("spotify:album:Y"); err == nil {
		t.Fatal("Play after detach should fail")
	}

	conn2, _, err := dialer.Dial("ws://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cc.attach(conn2)
	defer conn2.Close()

	if err := cc.Play("spotify:album:Z"); err != nil {
		t.Fatalf("Play after re-attach: %v", err)
	}

	select {
	case msg := <-msgCh:
		want := `{"type":"command","command":"play","uri":"spotify:album:Z"}`
		if string(msg) != want {
			t.Fatalf("second play: got %q, want %q", msg, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second play message")
	}
}

// TestCommandConnector_TransportCommands checks that the non-URI commands are
// written as the exact Soloist frames and that attach/detach gates them.
func TestCommandConnector_TransportCommands(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	upgrader := &websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}

	msgCh := make(chan []byte, 8)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msgCh <- msg
		}
	})}

	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	cc := &CommandConnector{}

	// Before attach every command must fail.
	if err := cc.Pause(); err == nil {
		t.Fatal("Pause before attach should fail")
	}
	if err := cc.SkipNext(); err == nil {
		t.Fatal("SkipNext before attach should fail")
	}
	if err := cc.SkipPrev(); err == nil {
		t.Fatal("SkipPrev before attach should fail")
	}
	if err := cc.SetVolume(50); err == nil {
		t.Fatal("SetVolume before attach should fail")
	}

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial("ws://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cc.attach(conn)
	defer conn.Close()

	commands := []struct {
		name string
		want string
		send func() error
	}{
		{"play", `{"type":"command","command":"play","uri":"spotify:album:X"}`, func() error { return cc.Play("spotify:album:X") }},
		{"pause", `{"type":"command","command":"pause"}`, cc.Pause},
		{"skip_next", `{"type":"command","command":"skip_next"}`, cc.SkipNext},
		{"skip_prev", `{"type":"command","command":"skip_prev"}`, cc.SkipPrev},
		{"set_volume", `{"type":"command","command":"set_volume","volume":100}`, func() error { return cc.SetVolume(100) }},
	}

	for _, c := range commands {
		if err := c.send(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		select {
		case msg := <-msgCh:
			if string(msg) != c.want {
				t.Fatalf("%s: got %q, want %q", c.name, msg, c.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s message", c.name)
		}
	}

	cc.detach()
	conn.Close()

	for _, c := range commands {
		if err := c.send(); err == nil {
			t.Fatalf("%s after detach should fail", c.name)
		}
	}

	conn2, _, err := dialer.Dial("ws://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cc.attach(conn2)
	defer conn2.Close()

	if err := cc.SkipNext(); err != nil {
		t.Fatalf("SkipNext after re-attach: %v", err)
	}

	select {
	case msg := <-msgCh:
		if want := `{"type":"command","command":"skip_next"}`; string(msg) != want {
			t.Fatalf("skip_next after re-attach: got %q, want %q", msg, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for skip_next after re-attach")
	}
}
