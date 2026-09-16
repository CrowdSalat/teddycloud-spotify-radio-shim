package telemetry

import (
	"math"
	"testing"
	"time"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestSampleFirstCallBaselinesOnly(t *testing.T) {
	s := &Sampler{}

	cur := Snapshot{Chunks: 43, Dropped: 0, Delivered: 176400, Streams: 1}
	sum := s.Sample(time.Unix(0, 0), cur)

	if sum.ChunksPerSec != 0 || sum.DroppedPerSec != 0 || sum.DeliveredKiBps != 0 {
		t.Fatalf("first Sample must only baseline, got %+v", sum)
	}
	if sum.Streams != 1 {
		t.Errorf("Streams = %d, want 1", sum.Streams)
	}
}

func TestSampleComputesRatesAndDropRatio(t *testing.T) {
	s := &Sampler{}

	s.Sample(time.Unix(0, 0), Snapshot{Chunks: 0, Dropped: 0, Delivered: 0, Streams: 1})

	cur := Snapshot{Chunks: 100, Dropped: 40, Delivered: 60 * 4096, Streams: 1}
	sum := s.Sample(time.Unix(20, 0), cur)

	if !approx(sum.ChunksPerSec, 5) {
		t.Errorf("ChunksPerSec = %v, want 5", sum.ChunksPerSec)
	}
	if !approx(sum.DroppedPerSec, 2) {
		t.Errorf("DroppedPerSec = %v, want 2", sum.DroppedPerSec)
	}
	if !approx(sum.DropRatio, 40.0/140.0) {
		t.Errorf("DropRatio = %v, want %v", sum.DropRatio, 40.0/140.0)
	}
	if !approx(sum.DeliveredKiBps, (60*4096.0)/(1024.0*20.0)) {
		t.Errorf("DeliveredKiBps = %v, want %v", sum.DeliveredKiBps, (60*4096.0)/(1024.0*20.0))
	}
	if sum.Streams != 1 {
		t.Errorf("Streams = %d, want 1", sum.Streams)
	}
}

func TestSampleZeroElapsedDoesNotDivideByZero(t *testing.T) {
	s := &Sampler{}

	s.Sample(time.Unix(0, 0), Snapshot{Chunks: 0, Dropped: 0})
	cur := Snapshot{Chunks: 100, Dropped: 50}
	sum := s.Sample(time.Unix(0, 0), cur)

	if !approx(sum.ChunksPerSec, 100) {
		t.Errorf("ChunksPerSec = %v, want 100", sum.ChunksPerSec)
	}
	if !approx(sum.DropRatio, 50.0/150.0) {
		t.Errorf("DropRatio = %v, want %v", sum.DropRatio, 50.0/150.0)
	}
}

func TestSampleNoDropsHasZeroRatio(t *testing.T) {
	s := &Sampler{}

	s.Sample(time.Unix(0, 0), Snapshot{Chunks: 0, Dropped: 0})
	sum := s.Sample(time.Unix(10, 0), Snapshot{Chunks: 50, Dropped: 0})

	if sum.DroppedPerSec != 0 {
		t.Errorf("DroppedPerSec = %v, want 0", sum.DroppedPerSec)
	}
	if sum.DropRatio != 0 {
		t.Errorf("DropRatio = %v, want 0", sum.DropRatio)
	}
}

func TestSampleRebuildsBaselineAfterRecorderReconnect(t *testing.T) {
	s := &Sampler{}

	// First epoch: recorder produced a lot, small amount dropped.
	s.Sample(time.Unix(1000, 0), Snapshot{Chunks: 900, Dropped: 100, Delivered: 2000, Streams: 1})
	// Reconnect: fresh recorder starts counters at zero, delivery counters
	// stay monotonic across the whole process.
	sum := s.Sample(time.Unix(1005, 0), Snapshot{Chunks: 0, Dropped: 0, Delivered: 3500, Streams: 1})

	if sum.ChunksPerSec != 0 || sum.DroppedPerSec != 0 || sum.DropRatio != 0 || sum.DeliveredKiBps != 0 {
		t.Fatalf("reconnect sample must baseline again, got %+v", sum)
	}

	// New epoch continues normally from the rebuilt baseline.
	sum = s.Sample(time.Unix(1015, 0), Snapshot{Chunks: 300, Dropped: 150, Delivered: 3500 + 80*4096, Streams: 1})

	if !approx(sum.ChunksPerSec, 30) {
		t.Errorf("ChunksPerSec = %v, want 30", sum.ChunksPerSec)
	}
	if !approx(sum.DroppedPerSec, 15) {
		t.Errorf("DroppedPerSec = %v, want 15", sum.DroppedPerSec)
	}
	if !approx(sum.DropRatio, 150.0/450.0) {
		t.Errorf("DropRatio = %v, want %v", sum.DropRatio, 150.0/450.0)
	}
	if !approx(sum.DeliveredKiBps, (80*4096.0)/(1024.0*10.0)) {
		t.Errorf("DeliveredKiBps = %v, want %v", sum.DeliveredKiBps, (80*4096.0)/(1024.0*10.0))
	}
}

func TestSampleCapsNegativeElapsedAtOneSecond(t *testing.T) {
	s := &Sampler{}

	s.Sample(time.Unix(100, 0), Snapshot{Chunks: 0, Dropped: 0})
	sum := s.Sample(time.Unix(50, 0), Snapshot{Chunks: 10, Dropped: 0})

	if !approx(sum.ChunksPerSec, 10) {
		t.Errorf("ChunksPerSec = %v, want 10", sum.ChunksPerSec)
	}
}
