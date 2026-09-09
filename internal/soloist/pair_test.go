package soloist_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/process"
	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/soloist"
)

// stubProcess is a test Process whose exit behaviour is fully controlled by
// the test. A non-nil wait channel makes Wait block until the channel closes.
type stubProcess struct {
	err    error
	wait   chan struct{}
	killed bool
}

func (p *stubProcess) Wait() error {
	if p.wait != nil {
		<-p.wait
	}

	return p.err
}

func (p *stubProcess) Kill() error {
	p.killed = true

	return nil
}

func (p *stubProcess) Stdout() io.Reader {
	return bytes.NewReader(nil)
}

// stubManager returns a scripted sequence of stubProcesses and records the
// spawn calls.
type stubManager struct {
	mu       sync.Mutex
	procs    []*stubProcess
	starts   int
	lastName string
	lastArgs []string
}

func (m *stubManager) Start(_ context.Context, name string, args ...string) (process.Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.starts++
	m.lastName = name
	m.lastArgs = append([]string{}, args...)

	if len(m.procs) == 0 {
		return &stubProcess{}, nil
	}

	i := m.starts - 1
	if i >= len(m.procs) {
		i = len(m.procs) - 1
	}

	return m.procs[i], nil
}

func (m *stubManager) started() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.starts
}

// exitError runs /bin/sh to produce a genuine *exec.ExitError with the given
// exit code.
func exitError(t *testing.T, code int) error {
	t.Helper()

	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("expected /bin/sh -c exit %d to fail", code)
	}

	return err
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatal("condition not met before timeout")
}

func newPairer(binary, dir string, m process.Manager) *soloist.Pairer {
	return &soloist.Pairer{
		BinaryPath: binary,
		DeviceName: "shim-test",
		APIKey:     "test-key",
		DataDir:    dir,
		CacheDir:   filepath.Join(dir, "cache"),
		Manager:    m,
		Checker:    &soloist.SessionChecker{DataDir: dir},
	}
}

func TestPairer_ExitZero_SessionPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "settings", "Users"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	m := &stubManager{procs: []*stubProcess{{}}}
	p := newPairer("/opt/soloist", dir, m)

	if got := p.Pair(context.Background()); got != soloist.PairDone {
		t.Fatalf("Pair(): got %q, want %q", got, soloist.PairDone)
	}

	if m.started() != 1 {
		t.Errorf("started: got %d, want 1 (no respawn)", m.started())
	}

	if m.lastName != "/opt/soloist" {
		t.Errorf("name: got %q, want %q", m.lastName, "/opt/soloist")
	}

	wantArgs := []string{
		"--device-name", "shim-test",
		"--api-key", "test-key",
		"--data-dir", dir,
		"--cache-dir", filepath.Join(dir, "cache"),
		"--pair",
	}
	if len(m.lastArgs) != len(wantArgs) {
		t.Fatalf("args: got %v, want %v", m.lastArgs, wantArgs)
	}
	for i := range wantArgs {
		if m.lastArgs[i] != wantArgs[i] {
			t.Errorf("args[%d]: got %q, want %q", i, m.lastArgs[i], wantArgs[i])
		}
	}
}

func TestPairer_ExitZero_SessionAbsent(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{{}}}
	p := newPairer("/opt/soloist", dir, m)

	if got := p.Pair(context.Background()); got != soloist.PairDone {
		t.Fatalf("Pair(): got %q, want %q", got, soloist.PairDone)
	}
}

func TestPairer_SessionAppearsWhileRunning(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{{wait: make(chan struct{})}}}
	p := newPairer("/opt/soloist", dir, m)
	p.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan soloist.PairOutcome, 1)
	go func() { done <- p.Pair(ctx) }()

	waitFor(t, time.Second, func() bool { return m.started() == 1 })

	if err := os.MkdirAll(filepath.Join(dir, "settings", "Users"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	select {
	case out := <-done:
		if out != soloist.PairDone {
			t.Fatalf("Pair(): got %q, want %q", out, soloist.PairDone)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pair(): timed out waiting for session detection")
	}

	if !m.procs[0].killed {
		t.Error("Pair(): expected the still-running pair process to be killed")
	}
}

func TestPairer_ExitOne_Retry(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{{err: exitError(t, 1)}}}
	p := newPairer("/opt/soloist", dir, m)

	if got := p.Pair(context.Background()); got != soloist.PairRetry {
		t.Fatalf("Pair(): got %q, want %q", got, soloist.PairRetry)
	}
}

func TestPairer_ExitTen_ExpiredAndBinaryDeleted(t *testing.T) {
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin", "soloist")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := &stubManager{procs: []*stubProcess{{err: exitError(t, 10)}}}
	p := newPairer(bin, dir, m)
	p.BinaryManager = &soloist.BinaryManager{DataDir: dir}

	if got := p.Pair(context.Background()); got != soloist.PairExpired {
		t.Fatalf("Pair(): got %q, want %q", got, soloist.PairExpired)
	}

	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("expected expired binary at %s to be deleted", bin)
	}
}

func TestPairer_Cancel(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{{wait: make(chan struct{})}}}
	p := newPairer("/opt/soloist", dir, m)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan soloist.PairOutcome, 1)
	go func() { done <- p.Pair(ctx) }()

	waitFor(t, time.Second, func() bool { return m.started() == 1 })

	cancel()

	select {
	case out := <-done:
		if out != soloist.PairCanceled {
			t.Fatalf("Pair(): got %q, want %q", out, soloist.PairCanceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pair(): timed out waiting for context cancellation")
	}

	if !m.procs[0].killed {
		t.Error("Pair(): expected the pair process to be killed on cancel")
	}
}

func TestDrive_NoRespawnOnSuccess(t *testing.T) {
	calls := 0
	pair := func(_ context.Context) soloist.PairOutcome {
		calls++

		return soloist.PairDone
	}

	out := soloist.Drive(context.Background(), pair, time.Second, time.Minute)
	if out != soloist.PairDone {
		t.Fatalf("Drive(): got %q, want %q", out, soloist.PairDone)
	}

	if calls != 1 {
		t.Errorf("Drive(): pair called %d times, want 1 (no respawn)", calls)
	}
}

func TestDrive_RetriesThenSucceeds(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{
		{err: exitError(t, 1)},
		{},
	}}
	p := newPairer("/opt/soloist", dir, m)

	out := soloist.Drive(context.Background(), p.Pair, time.Millisecond, time.Millisecond)
	if out != soloist.PairDone {
		t.Fatalf("Drive(): got %q, want %q", out, soloist.PairDone)
	}

	if m.started() != 2 {
		t.Errorf("Drive(): started %d processes, want 2", m.started())
	}
}

func TestDrive_CanceledDuringRetry(t *testing.T) {
	dir := t.TempDir()

	m := &stubManager{procs: []*stubProcess{{err: exitError(t, 1)}}}
	p := newPairer("/opt/soloist", dir, m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if out := soloist.Drive(ctx, p.Pair, time.Minute, time.Minute); out != soloist.PairCanceled {
		t.Fatalf("Drive(): got %q, want %q", out, soloist.PairCanceled)
	}
}

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		current time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{5 * time.Second, 60 * time.Second, 10 * time.Second},
		{30 * time.Second, 60 * time.Second, 60 * time.Second},
		{60 * time.Second, 60 * time.Second, 60 * time.Second},
		{2 * time.Minute, 60 * time.Second, 60 * time.Second},
	}

	for _, tc := range cases {
		if got := soloist.NextBackoff(tc.current, tc.max); got != tc.want {
			t.Errorf("NextBackoff(%v, %v): got %v, want %v", tc.current, tc.max, got, tc.want)
		}
	}
}
