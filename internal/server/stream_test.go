package server

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeChunkSource struct {
	ch chan []byte
}

func (f *fakeChunkSource) Chunks() <-chan []byte {
	return f.ch
}

func newFakeChunkSource(chunks ...[]byte) *fakeChunkSource {
	ch := make(chan []byte, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)

	return &fakeChunkSource{ch: ch}
}

func TestStream_ValidURI(t *testing.T) {
	fake := newFakeChunkSource([]byte("chunk1"), []byte("chunk2"), []byte("chunk3"))
	s := New("localhost:0", func() ChunkSource { return fake }, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:album:ABC123", nil)
	rec := httptest.NewRecorder()
	s.handleStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("Content-Type: got %q, want %q", ct, "audio/wav")
	}

	body := rec.Body.Bytes()
	if len(body) < 44 {
		t.Fatalf("body too short: %d bytes", len(body))
	}

	if string(body[0:4]) != "RIFF" {
		t.Errorf("RIFF magic: got %q", body[0:4])
	}
	if got := binary.LittleEndian.Uint32(body[4:8]); got != 0xFFFFFFFF {
		t.Errorf("RIFF size: got 0x%08X, want 0xFFFFFFFF", got)
	}
	if string(body[8:12]) != "WAVE" {
		t.Errorf("WAVE magic: got %q", body[8:12])
	}
	if string(body[12:16]) != "fmt " {
		t.Errorf("fmt magic: got %q", body[12:16])
	}
	if got := binary.LittleEndian.Uint32(body[16:20]); got != 16 {
		t.Errorf("fmt length: got %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint16(body[20:22]); got != 1 {
		t.Errorf("audio format: got %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(body[22:24]); got != 2 {
		t.Errorf("channels: got %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(body[24:28]); got != 44100 {
		t.Errorf("sample rate: got %d, want 44100", got)
	}
	if got := binary.LittleEndian.Uint32(body[28:32]); got != 176400 {
		t.Errorf("byte rate: got %d, want 176400", got)
	}
	if got := binary.LittleEndian.Uint16(body[32:34]); got != 4 {
		t.Errorf("block align: got %d, want 4", got)
	}
	if got := binary.LittleEndian.Uint16(body[34:36]); got != 16 {
		t.Errorf("bits per sample: got %d, want 16", got)
	}
	if string(body[36:40]) != "data" {
		t.Errorf("data magic: got %q", body[36:40])
	}
	if got := binary.LittleEndian.Uint32(body[40:44]); got != 0xFFFFFFFF {
		t.Errorf("data size: got 0x%08X, want 0xFFFFFFFF", got)
	}

	expected := []byte("chunk1chunk2chunk3")
	got := body[44:]
	if string(got) != string(expected) {
		t.Errorf("body: got %d bytes after header, want %d", len(got), len(expected))
	}
}

func TestStream_MissingURI(t *testing.T) {
	s := New("localhost:0", func() ChunkSource { return nil }, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()
	s.handleStream(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestStream_InvalidURI(t *testing.T) {
	cases := []string{
		"http://evil.com",
		"spotify:track:",
		"",
		"spotify:foo:bar",
	}
	s := New("localhost:0", func() ChunkSource { return nil }, nil)

	for _, uri := range cases {
		req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)
		rec := httptest.NewRecorder()
		s.handleStream(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("uri %q: status got %d, want %d", uri, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestStream_NilSource(t *testing.T) {
	s := New("localhost:0", func() ChunkSource { return nil }, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:album:ABC", nil)
	rec := httptest.NewRecorder()
	s.handleStream(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestStream_PlayCalled(t *testing.T) {
	fake := newFakeChunkSource([]byte("audio"))
	var playedURI string

	s := New("localhost:0", func() ChunkSource { return fake }, func(uri string) error {
		playedURI = uri
		return nil
	})

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:album:XYZ789", nil)
	rec := httptest.NewRecorder()
	s.handleStream(rec, req)

	if playedURI != "spotify:album:XYZ789" {
		t.Errorf("play URI: got %q, want %q", playedURI, "spotify:album:XYZ789")
	}
}
