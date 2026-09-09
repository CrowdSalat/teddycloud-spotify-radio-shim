package server

import (
	"encoding/binary"
	"encoding/json"
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

	if s.play != nil {
		if err := s.play(uri); err != nil {
			slog.Warn("soloist play failed", "uri", uri, "err", err)
		}
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.WriteHeader(http.StatusOK)
	writeWAVHeader(w)
	flush(w)

	ch := src.Chunks()
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write(chunk)
			flush(w)
		}
	}
}

func writeWAVHeader(w http.ResponseWriter) {
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

	_, _ = w.Write(h)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
