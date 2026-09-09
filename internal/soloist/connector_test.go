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
