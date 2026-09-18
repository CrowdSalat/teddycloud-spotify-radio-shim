package server

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeChunkSource struct {
	ch   chan []byte
	rate uint32
}

func (f *fakeChunkSource) Chunks() <-chan []byte {
	return f.ch
}

func (f *fakeChunkSource) SampleRate() uint32 {
	if f.rate > 0 {
		return f.rate
	}

	return 22050
}

func newFakeChunkSource(chunks ...[]byte) *fakeChunkSource {
	ch := make(chan []byte, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)

	return &fakeChunkSource{ch: ch}
}

// flushableSource is a ChunkSource that records flushStalePreFill invocations
// so tests can prove the stale pre-fill is discarded at connect.
type flushableSource struct {
	fakeChunkSource
	flushed int
}

func (f *flushableSource) Flush() {
	for {
		select {
		case <-f.ch:
			f.flushed++
		default:
			return
		}
	}
}

func TestStream_FlushesStalePreFill(t *testing.T) {
	ch := make(chan []byte, 8)
	ch <- []byte("stale1")
	ch <- []byte("stale2")

	src := &flushableSource{fakeChunkSource: fakeChunkSource{ch: ch}}
	s := New("localhost:0", func() ChunkSource { return src }, nil)

	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/stream?spotify_uri=spotify:album:FLS")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Feed the real audio only after the handler had a chance to flush, then
	// close so the handler terminates and the body is fully readable.
	time.Sleep(50 * time.Millisecond)
	ch <- []byte("fresh1")
	close(ch)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if src.flushed != 2 {
		t.Errorf("flushed %d stale chunks, want 2", src.flushed)
	}

	if got := string(body[44:]); got != "fresh1" {
		t.Errorf("body after header: got %q, want %q (stale pre-fill must be dropped)", got, "fresh1")
	}
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
	if got := binary.LittleEndian.Uint32(body[24:28]); got != 22050 {
		t.Errorf("sample rate: got %d, want 22050", got)
	}
	if got := binary.LittleEndian.Uint32(body[28:32]); got != 88200 {
		t.Errorf("byte rate: got %d, want 88200", got)
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

func TestStreamCounters(t *testing.T) {
	fake := newFakeChunkSource([]byte("chunk1"), []byte("chunk2"))
	s := New("localhost:0", func() ChunkSource { return fake }, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:album:CNT", nil)
	rec := httptest.NewRecorder()
	s.handleStream(rec, req)

	if s.ActiveStreams() != 0 {
		t.Errorf("ActiveStreams() = %d after handler returned, want 0", s.ActiveStreams())
	}
	wantDelivered := uint64(44 + len("chunk1") + len("chunk2"))
	if got := s.DeliveredBytes(); got != wantDelivered {
		t.Errorf("DeliveredBytes() = %d, want %d (WAV header + payload)", got, wantDelivered)
	}
}

func TestStream_HotSwap(t *testing.T) {
	ch := make(chan []byte, 1024)
	var plays []string
	var playMu sync.Mutex

	s := New("localhost:0", func() ChunkSource {
		return &fakeChunkSource{ch: ch}
	}, func(uri string) error {
		playMu.Lock()
		plays = append(plays, uri)
		playMu.Unlock()
		return nil
	})

	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case ch <- make([]byte, 4096):
			case <-stop:
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	respA, err := http.Get(srv.URL + "/stream?spotify_uri=spotify:album:AAA")
	if err != nil {
		t.Fatal(err)
	}
	defer respA.Body.Close()

	buf := make([]byte, 256)
	if _, err := io.ReadFull(respA.Body, buf[:44]); err != nil {
		t.Fatalf("client A header read: %v", err)
	}
	if _, err := respA.Body.Read(buf); err != nil {
		t.Fatalf("client A first chunk read: %v", err)
	}

	respB, err := http.Get(srv.URL + "/stream?spotify_uri=spotify:album:BBB")
	if err != nil {
		t.Fatal(err)
	}
	defer respB.Body.Close()

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, respA.Body)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client A did not terminate after hot-swap")
	}

	playMu.Lock()
	defer playMu.Unlock()

	if len(plays) != 2 {
		t.Fatalf("play calls: got %d, want 2", len(plays))
	}
	if plays[0] != "spotify:album:AAA" {
		t.Errorf("play[0]: got %q, want %q", plays[0], "spotify:album:AAA")
	}
	if plays[1] != "spotify:album:BBB" {
		t.Errorf("play[1]: got %q, want %q", plays[1], "spotify:album:BBB")
	}
}

// writeRecorder captures the size of every Write call so tests can
// observe HTTP write segmentation.
type writeRecorder struct {
	writes []int
}

func (wr *writeRecorder) Write(p []byte) (int, error) {
	wr.writes = append(wr.writes, len(p))
	return len(p), nil
}

func TestStreamBatching(t *testing.T) {
	const chunkSize = 4096
	const numChunks = 40

	ch := make(chan []byte, numChunks)
	for i := 0; i < numChunks; i++ {
		ch <- make([]byte, chunkSize)
	}
	close(ch)

	rec := &writeRecorder{}
	var delivered atomic.Uint64
	streamChunks(context.Background(), rec, &delivered, ch, streamFlushThreshold)

	want := uint64(numChunks * chunkSize)
	if got := delivered.Load(); got != want {
		t.Errorf("delivered: got %d, want %d", got, want)
	}

	if len(rec.writes) == 0 {
		t.Fatal("no writes observed")
	}
	// Every segment except the trailing partial flush must meet the
	// threshold; the consumer must never see a drip of partial chunks.
	for i, n := range rec.writes[:len(rec.writes)-1] {
		if n < streamFlushThreshold {
			t.Errorf("write[%d]: %d bytes < %d threshold", i, n, streamFlushThreshold)
		}
	}
}

func TestStreamBatchedBody(t *testing.T) {
	const chunkSize = 4096
	const numChunks = 8

	chunks := make([][]byte, numChunks)
	for i := range chunks {
		chunks[i] = make([]byte, chunkSize)
	}
	fake := newFakeChunkSource(chunks...)
	s := New("localhost:0", func() ChunkSource { return fake }, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:album:BAT", nil)
	rec := httptest.NewRecorder()
	s.handleStream(rec, req)

	want := uint64(44 + numChunks*chunkSize)
	if got := uint64(rec.Body.Len()); got != want {
		t.Errorf("body length: got %d, want %d (WAV header + payload)", got, want)
	}
	if got := s.DeliveredBytes(); got != want {
		t.Errorf("DeliveredBytes(): got %d, want %d (WAV header + payload)", got, want)
	}
}
