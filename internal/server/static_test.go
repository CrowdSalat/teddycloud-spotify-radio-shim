package server

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStaticSource_ProducesChunks(t *testing.T) {
	const rate = 44100
	s := NewStaticSource(rate)
	defer s.Close()

	if got := s.SampleRate(); got != rate {
		t.Fatalf("SampleRate: got %d, want %d", got, rate)
	}

	for i := 0; i < 16; i++ {
		select {
		case chunk := <-s.Chunks():
			if len(chunk) != 4096 {
				t.Fatalf("chunk %d: got %d bytes, want 4096", i, len(chunk))
			}
		case <-time.After(time.Second):
			t.Fatalf("chunk %d: generator stalled", i)
		}
	}
}

func TestStaticSource_ToneNonzero(t *testing.T) {
	s := NewStaticSource(44100)
	defer s.Close()

	chunk := <-s.Chunks()
	var nonzero int
	for _, b := range chunk {
		if b != 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		t.Fatal("chunk is all zeros, expected a tone")
	}
}

func TestStream_StaticSource(t *testing.T) {
	const rate = 22050
	src := NewStaticSource(rate)
	defer src.Close()

	s := New("localhost:0", func() ChunkSource { return src }, nil)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream?spotify_uri=spotify:album:STT", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content-type: got %q, want %q", ct, "audio/wav")
	}

	// The stream never ends and the static source produces at consumer pace
	// (not real time), so every read must be bounded; an unbounded read here
	// would buffer the whole stream until OOM.
	hdr := make([]byte, 44)
	if _, err := io.ReadFull(resp.Body, hdr); err != nil {
		t.Fatalf("read WAV header: %v", err)
	}
	if got := binary.LittleEndian.Uint32(hdr[24:28]); got != rate {
		t.Errorf("header sample rate: got %d, want %d", got, rate)
	}
	if got := binary.LittleEndian.Uint32(hdr[28:32]); got != rate*4 {
		t.Errorf("header byte rate: got %d, want %d", got, rate*4)
	}

	if s.ActiveStreams() != 1 {
		t.Fatalf("ActiveStreams() = %d, want 1 while streaming", s.ActiveStreams())
	}

	payload := make([]byte, 4096)
	if _, err := io.ReadFull(resp.Body, payload); err != nil {
		t.Fatalf("read payload chunk: %v", err)
	}
	var nonzero int
	for _, b := range payload {
		if b != 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		t.Fatal("no static payload served")
	}

	// Cancel the request so the handler terminates instead of running forever.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for s.ActiveStreams() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("stream handler did not terminate after context cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
