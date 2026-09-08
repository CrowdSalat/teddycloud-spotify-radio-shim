// Package audio defines the interfaces for the shim's audio stack and the
// PulseAudio daemon orchestration that satisfies AudioDaemon.
package audio

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
