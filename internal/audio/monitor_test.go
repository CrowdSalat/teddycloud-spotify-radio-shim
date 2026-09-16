package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupEnvSetsPulseServer(t *testing.T) {
	dir := t.TempDir()
	p := &PulseAudio{HomeDir: filepath.Join(dir, "home"), XDGRuntimeDir: dir}

	if err := p.SetupEnv(); err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}

	want := "unix:" + filepath.Join(dir, "pulse", "native")
	if got := os.Getenv("PULSE_SERVER"); got != want {
		t.Errorf("PULSE_SERVER: got %q, want %q", got, want)
	}
}

func TestDaemonArgsPinSampleSpec(t *testing.T) {
	p := &PulseAudio{}

	for _, a := range p.daemonArgs() {
		if !strings.Contains(a, "module-null-sink") {
			continue
		}

		if !strings.Contains(a, fmt.Sprintf("rate=%d", SampleRate)) {
			t.Errorf("null-sink load argument missing rate=%d: %q", SampleRate, a)
		}

		if !strings.Contains(a, "channels=2") {
			t.Errorf("null-sink load argument missing channels=2: %q", a)
		}

		return
	}

	t.Error("daemonArgs: no module-null-sink load argument found")
}

func TestOpenMonitorStreamRejectsMissingSocket(t *testing.T) {
	server := "unix:" + filepath.Join(t.TempDir(), "pulse", "native")

	stream, err := OpenMonitorStream(context.Background(), server)
	if err == nil {
		_ = stream.Close()
		t.Fatal("OpenMonitorStream: expected connect error for missing socket, got nil")
	}
}

func TestOpenMonitorStreamHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := OpenMonitorStream(ctx, "unix:/does/not/exist"); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenMonitorStream: expected context.Canceled, got %v", err)
	}
}
