// Package telemetry turns cumulative pipeline counters into per-interval
// rate diagnostics. It has no dependencies on the audio, server, or logging
// layers: it is pure math over point-in-time snapshots, so operators can
// classify playback-quality problems (producer dropping, consumer behind,
// stalled stream) from a single log line.
package telemetry

import "time"

// Snapshot is a point-in-time reading of the audio pipeline counters. Fields
// are cumulative since the start of the process or the current recorder epoch.
type Snapshot struct {
	// Chunks is the number of PCM chunks captured from the monitor.
	Chunks uint64
	// Dropped is the number of chunks discarded because the consumer was behind.
	Dropped uint64
	// Delivered is the number of payload bytes written to /stream consumers.
	Delivered uint64
	// Streams is the number of /stream connections currently open.
	Streams int64
}

// Summary is a rate report derived from two consecutive snapshots.
type Summary struct {
	// ChunksPerSec is the capture rate of the producer.
	ChunksPerSec float64
	// DroppedPerSec is the discard rate of the consumer-side buffer.
	DroppedPerSec float64
	// DropRatio is the fraction of produced audio discarded, in 0..1.
	// ~0.5 with a sustained stream means the consumer cannot keep up.
	DropRatio float64
	// DeliveredKiBps is the delivery rate to /stream consumers.
	DeliveredKiBps float64
	// Streams is the number of /stream connections currently open.
	Streams int64
}

// Sampler diffs consecutive snapshots into per-interval rates. The first
// Sample establishes a baseline. A counter moving backwards (e.g. a recorder
// reconnect restarts the chunk stream at zero) silently rebuilds the baseline,
// so a single Sampler can span recorder restarts.
type Sampler struct {
	prev  Snapshot
	prevT time.Time
	set   bool
}

// Sample returns the rates between the previous call and now. The first call
// only records a baseline and returns zero rates.
func (s *Sampler) Sample(now time.Time, cur Snapshot) Summary {
	if !s.set || cur.Chunks < s.prev.Chunks || cur.Dropped < s.prev.Dropped {
		s.prev, s.prevT, s.set = cur, now, true
		return Summary{Streams: cur.Streams}
	}

	dt := now.Sub(s.prevT)
	if dt <= 0 {
		dt = time.Second
	}
	sec := dt.Seconds()

	sum := Summary{
		ChunksPerSec:   float64(cur.Chunks-s.prev.Chunks) / sec,
		DroppedPerSec:  float64(cur.Dropped-s.prev.Dropped) / sec,
		DeliveredKiBps: float64(cur.Delivered-s.prev.Delivered) / 1024 / sec,
		Streams:        cur.Streams,
	}

	dropped := float64(cur.Dropped - s.prev.Dropped)
	produced := dropped + float64(cur.Chunks-s.prev.Chunks)
	if produced > 0 {
		sum.DropRatio = dropped / produced
	}

	s.prev, s.prevT = cur, now
	return sum
}
