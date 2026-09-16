package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
)

var validURI = regexp.MustCompile(`^spotify:(track|album|playlist|episode):[A-Za-z0-9]+$`)

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("spotify_uri")

	if !validURI.MatchString(uri) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(healthError{Error: "invalid spotify_uri"})

		return
	}

	src := s.src()
	if src == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(healthError{Error: "pulseaudio_not_ready"})

		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.streamMu.Lock()
	if s.streamCancel != nil {
		s.streamCancel()
	}
	s.streamID++
	myID := s.streamID
	s.streamCancel = cancel
	s.streamMu.Unlock()

	defer func() {
		s.streamMu.Lock()
		if s.streamID == myID {
			s.streamCancel = nil
		}
		s.streamMu.Unlock()
	}()

	if s.play != nil {
		if err := s.play(uri); err != nil {
			slog.Warn("soloist play failed", "uri", uri, "err", err)
		}
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.WriteHeader(http.StatusOK)
	header := writeWAVHeader()
	_, _ = w.Write(header)
	s.delivered.Add(uint64(len(header)))
	s.active.Add(1)
	defer s.active.Add(-1)
	flush(w)

	ch := src.Chunks()
	s.delivered.Add(streamChunks(ctx, w, ch, streamFlushThreshold))
}

// streamFlushThreshold is the minimum batch size for HTTP writes.
// 16 KiB (4–8 chunks) avoids delayed-ACK coalescing that starves
// ffmpeg at small segment sizes.
const streamFlushThreshold = 16384

// streamChunks writes audio chunks to w using batched writes of at
// least flushThreshold bytes. It returns the total chunk bytes written.
func streamChunks(ctx context.Context, w io.Writer, ch <-chan []byte, flushThreshold int) uint64 {
	bw := bufio.NewWriterSize(w, flushThreshold)
	var delivered uint64

	for {
		select {
		case <-ctx.Done():
			_ = bw.Flush()
			return delivered
		case chunk, ok := <-ch:
			if !ok {
				_ = bw.Flush()
				return delivered
			}
			n, _ := bw.Write(chunk)
			delivered += uint64(n)
		}
	}
}

func writeWAVHeader() []byte {
	h := make([]byte, 44)

	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], 0xFFFFFFFF)
	copy(h[8:12], "WAVE")

	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1)
	binary.LittleEndian.PutUint16(h[22:24], 2)
	binary.LittleEndian.PutUint32(h[24:28], 44100)
	binary.LittleEndian.PutUint32(h[28:32], 176400)
	binary.LittleEndian.PutUint16(h[32:34], 4)
	binary.LittleEndian.PutUint16(h[34:36], 16)

	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], 0xFFFFFFFF)

	return h
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
