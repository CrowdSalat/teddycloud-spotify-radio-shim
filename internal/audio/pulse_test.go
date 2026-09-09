package audio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/process"
)

// stubProcess is a test Process whose exit behaviour is fully controlled by
// the test. Wait blocks until the gate is closed (by Kill or release).
type stubProcess struct {
	mu   sync.Mutex
	gate chan struct{}
	err  error
	once sync.Once
}

func newStubProcess() *stubProcess {
	return &stubProcess{gate: make(chan struct{})}
}

// newFinishedProcess returns a process that has already exited with err.
func newFinishedProcess(err error) *stubProcess {
	p := newStubProcess()
	p.release(err)

	return p
}

func (p *stubProcess) Wait() error {
	<-p.gate

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.err
}

func (p *stubProcess) release(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	p.once.Do(func() { close(p.gate) })
}

func (p *stubProcess) Kill() error {
	p.once.Do(func() { close(p.gate) })

	return nil
}

func (p *stubProcess) Stdout() io.Reader {
	return bytes.NewReader(nil)
}

// stubManager returns scripted stubProcesses for pulseaudio and finished
// processes for pactl, recording every spawn.
type stubManager struct {
	mu          sync.Mutex
	pulseProcs  []process.Process
	pulseSpawns int
	setup       func()
	onPactlExit func()
	pactlArgv   [][]string
}

func (m *stubManager) Start(_ context.Context, name string, args ...string) (process.Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if name != "pulseaudio" {
		m.pactlArgv = append(m.pactlArgv, append([]string{}, args...))

		if len(args) == 1 && args[0] == "exit" && m.onPactlExit != nil {
			m.onPactlExit()
		}

		if len(args) >= 2 && args[0] == "list" {
			return newFinishedProcess(nil).withStdout("0\tvirtual_out\tmodule-null-sink.c\ts16le 2ch 44100Hz\tIDLE\n"), nil
		}

		return newFinishedProcess(nil), nil
	}

	m.pulseSpawns++

	if m.setup != nil {
		m.setup()
	}

	i := m.pulseSpawns - 1
	if len(m.pulseProcs) == 0 {
		return newFinishedProcess(nil), nil
	}
	if i >= len(m.pulseProcs) {
		i = len(m.pulseProcs) - 1
	}

	return m.pulseProcs[i], nil
}

func (m *stubManager) pulseSpawnCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.pulseSpawns
}

func (m *stubManager) ranSetDefaultSink() bool {
	return m.ranPactl("set-default-sink")
}

func (m *stubManager) ranPactlExit() bool {
	return m.ranPactl("exit")
}

func (m *stubManager) ranPactl(firstArg string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, args := range m.pactlArgv {
		if len(args) > 0 && args[0] == firstArg {
			return true
		}
	}

	return false
}

// withStdout wraps an exiting stubProcess with stdout content.
func (p *stubProcess) withStdout(s string) process.Process {
	return &stdoutProcess{stubProcess: p, out: s}
}

type stdoutProcess struct {
	*stubProcess
	out string
}

func (p *stdoutProcess) Stdout() io.Reader {
	return bytes.NewReader([]byte(p.out))
}

// healthRecorder collects Health callback reasons.
type healthRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *healthRecorder) record(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seen = append(r.seen, reason)
}

func (r *healthRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string{}, r.seen...)
}

func newPulseAudio(m *stubManager, dir string, h *healthRecorder) *PulseAudio {
	rec := func(string) {}
	if h != nil {
		rec = h.record
	}

	return &PulseAudio{
		Manager:       m,
		HomeDir:       filepath.Join(dir, "home"),
		XDGRuntimeDir: dir,
		ReadyTimeout:  2 * time.Second,
		StartBackoff:  10 * time.Millisecond,
		MaxBackoff:    10 * time.Millisecond,
		ProbeInterval: 10 * time.Millisecond,
		StopGrace:     200 * time.Millisecond,
		Health:        rec,
	}
}

func createSocket(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, "pulse"), 0o700); err != nil {
		t.Fatalf("mkdir pulse: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "pulse", "native"), nil, 0o600); err != nil {
		t.Fatalf("write native socket: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("condition not met within", timeout)
}

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		current, max, want time.Duration
	}{
		{10 * time.Millisecond, time.Minute, 20 * time.Millisecond},
		{time.Minute, time.Minute, time.Minute},
		{2 * time.Minute, time.Minute, time.Minute},
	}

	for _, tc := range cases {
		if got := NextBackoff(tc.current, tc.max); got != tc.want {
			t.Errorf("NextBackoff(%v, %v): got %v, want %v", tc.current, tc.max, got, tc.want)
		}
	}
}

func TestStartFailsWhenDaemonizerExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	m := &stubManager{
		pulseProcs: []process.Process{newFinishedProcess(errors.New("boom"))},
	}
	p := newPulseAudio(m, dir, nil)

	if err := p.Start(); err == nil {
		t.Fatal("Start: expected error, got nil")
	}

	if p.Ready() {
		t.Error("Start: Ready() must be false after failure")
	}
}

func TestStartVerifiesAndSetsDefaultSink(t *testing.T) {
	dir := t.TempDir()
	proc := newFinishedProcess(nil)
	m := &stubManager{pulseProcs: []process.Process{proc}}
	m.setup = func() { createSocket(t, dir) }

	p := newPulseAudio(m, dir, nil)

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !p.Ready() {
		t.Error("Start: Ready() must be true after success")
	}

	if !m.ranSetDefaultSink() {
		t.Error("Start: pactl set-default-sink virtual_out was not invoked")
	}
}

func TestRunRestartsAfterDaemonDeath(t *testing.T) {
	dir := t.TempDir()

	var alive atomic.Bool
	alive.Store(true)

	m := &stubManager{pulseProcs: []process.Process{newFinishedProcess(nil)}}
	m.setup = func() { createSocket(t, dir) }

	h := &healthRecorder{}
	p := newPulseAudio(m, dir, h)
	p.alive = alive.Load

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { return m.pulseSpawnCount() == 1 && p.Ready() })

	alive.Store(false)

	waitFor(t, 3*time.Second, func() bool { return m.pulseSpawnCount() >= 2 })

	alive.Store(true)

	waitFor(t, 3*time.Second, func() bool { return p.Ready() })

	seen := h.list()
	for _, reason := range seen {
		if reason == PulseNotReady {
			return
		}
	}

	t.Errorf("Health: expected pulseaudio_not_ready during recovery, got %v", seen)
}

func TestStopTerminatesGracefully(t *testing.T) {
	dir := t.TempDir()

	var alive atomic.Bool
	alive.Store(true)

	m := &stubManager{pulseProcs: []process.Process{newFinishedProcess(nil)}}
	m.setup = func() { createSocket(t, dir) }

	p := newPulseAudio(m, dir, nil)
	p.alive = alive.Load

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// pactl exit "turns the daemon off": flip liveness when the fake pactl
	// exit command is spawned.
	m.onPactlExit = func() { alive.Store(false) }

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop: did not return within timeout")
	}

	if !m.ranPactlExit() {
		t.Error("Stop: pactl exit was not issued")
	}

	if p.Ready() {
		t.Error("Stop: Ready() must be false after stop")
	}
}

func TestStopReturnsWhenDaemonUnresponsive(t *testing.T) {
	dir := t.TempDir()

	var alive atomic.Bool
	alive.Store(true)

	m := &stubManager{pulseProcs: []process.Process{newFinishedProcess(nil)}}
	m.setup = func() { createSocket(t, dir) }

	p := newPulseAudio(m, dir, nil)
	p.alive = alive.Load

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop: must not hang when the daemon ignores pactl exit")
	}
}
