// Package audio defines the interfaces for the shim's audio stack and the
// PulseAudio daemon orchestration that satisfies AudioDaemon.
package audio

// SampleRate is the recording rate shared by the null sink, the monitor
// stream and the WAV header. It was halved from 44100 Hz (Phase 12): the raw
// s16le stereo byte rate drops from 176400 B/s to 88200 B/s, comfortably
// under the teddycloud consumer's measured ~136 kB/s HTTP read ceiling, so
// the recorder's drop-on-full valve stays silent while playing. Speech quality
// (Die drei ??? etc.) is unaffected in the audible range.
const SampleRate = 22050

// ByteRate returns the s16le stereo byte rate for SampleRate, as written into
// the WAV header.
func ByteRate() uint32 {
	return SampleRate * 2 * 2
}

// AudioDaemon is the contract for the shim's audio server process. The
// PulseAudio type implements it; a future PipeWire implementation would
// satisfy the same interface without touching the rest of the shim.
type AudioDaemon interface {
	// Start brings the daemon up and returns an error if it cannot become
	// ready.
	Start() error

	// Ready reports whether the daemon is currently running and usable.
	Ready() bool

	// Stop shuts the daemon down cleanly. It is safe to call when the daemon
	// was never started.
	Stop()
}
