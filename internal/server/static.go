package server

import (
	"math"
	"sync"
)

// staticSource is a ChunkSource that never runs dry: it synthesizes a
// low-amplitude PCM tone at full speed so a /stream consumer is always served
// (unlike the recorder, which produces at real time). It exists to isolate
// whether teddycloud's slow drain is caused by the shim's live-stream pacing
// or by its own HTTP ingest path (Phase 12 static-file test).
type staticSource struct {
	ch       chan []byte
	stop     chan struct{}
	stopOnce sync.Once
	rate     uint32
	chunk    []byte
}

// NewStaticSource returns a ChunkSource yielding fixed-size 4096-byte s16le
// stereo PCM at sampleRate, producing as fast as the consumer drains it.
// Close stops the generator.
func NewStaticSource(sampleRate uint32) *staticSource {
	s := &staticSource{
		ch:    make(chan []byte, 16),
		stop:  make(chan struct{}),
		rate:  sampleRate,
		chunk: sineChunk(440, sampleRate, 1024), // 440 Hz at ~1/16 full scale
	}

	// Pre-fill the buffer once so the first consumer read never blocks on the
	// generator goroutine.
	for range 4 {
		s.ch <- append([]byte(nil), s.chunk...)
	}

	go func() {
		for {
			select {
			case <-s.stop:
				return
			case s.ch <- append([]byte(nil), s.chunk...):
			}
		}
	}()

	return s
}

// sineChunk builds one 4096-byte s16le stereo chunk of a soft sine tone. The
// tone makes a playback path audibly confirmable without risking the speaker.
func sineChunk(hz int, rate, amplitude uint32) []byte {
	buf := make([]byte, 4096)
	for i := 0; i < len(buf)/2; i++ {
		v := int16(math.Sin(2*math.Pi*float64(hz)*float64(i)/float64(rate)) * float64(amplitude/2))
		buf[i*2+0] = byte(v)
		buf[i*2+1] = byte(v >> 8)
	}

	return buf
}

// Chunks returns the synthesized chunk channel.
func (s *staticSource) Chunks() <-chan []byte {
	return s.ch
}

// SampleRate reports the synthesized sample rate.
func (s *staticSource) SampleRate() uint32 {
	return s.rate
}

// Close stops the generator goroutine.
func (s *staticSource) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}
