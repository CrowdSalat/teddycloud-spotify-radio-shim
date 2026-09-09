package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/process"
)

// Health reason reported while PulseAudio is down or restarting.
const PulseNotReady = "pulseaudio_not_ready"

const (
	SinkName          = "virtual_out"
	sinkDeviceDesc    = "Shim_Sink"
	defaultHomeDir    = "/tmp/home"
	defaultRuntimeDir = "/tmp/runtime"
)

const (
	defaultReadyTimeout  = 10 * time.Second
	defaultStartBackoff  = 5 * time.Second
	defaultMaxBackoff    = 60 * time.Second
	defaultStopGrace     = 3 * time.Second
	defaultPollInterval  = 250 * time.Millisecond
	defaultProbeInterval = 2 * time.Second
	defaultPactlRetries  = 5
	pactlRetryInterval   = 500 * time.Millisecond
	pidPollInterval      = 100 * time.Millisecond
)

var errSocketTimeout = errors.New("pulseaudio native socket did not appear in time")

// PulseAudio owns the PulseAudio daemon subprocess, the virtual null sink, and
// its crash-recovery loop.
//
// PulseAudio runs with --daemonize=yes: the spawned process is a launcher that
// forks the real daemon and exits 0 once it is up. A foreground daemon stalls
// for ~10 s on unreachable D-Bus lookups in the container base image (Debian
// trixie), so the launcher is not monitored for crashes; instead the daemon's
// liveness is probed via its PID file.
type PulseAudio struct {
	// Manager spawns the pulseaudio and pactl subprocesses.
	Manager process.Manager
	// HomeDir overrides HOME for the daemon and pactl clients. Empty uses
	// /tmp/home.
	HomeDir string
	// XDGRuntimeDir overrides XDG_RUNTIME_DIR; the native socket is created at
	// <XDGRuntimeDir>/pulse/native. Empty uses /tmp/runtime.
	XDGRuntimeDir string
	// ReadyTimeout bounds how long a start waits for the native socket. Zero
	// uses the default.
	ReadyTimeout time.Duration
	// StartBackoff is the initial crash-restart backoff. Zero uses the default.
	StartBackoff time.Duration
	// MaxBackoff caps the crash-restart backoff. Zero uses the default.
	MaxBackoff time.Duration
	// ProbeInterval is how often daemon liveness is checked. Zero uses the
	// default.
	ProbeInterval time.Duration
	// StopGrace is how long Stop waits for a clean daemon shutdown before
	// killing it. Zero uses the default.
	StopGrace time.Duration
	// Health reports audio health state changes. An empty reason means healthy;
	// a non-empty reason becomes the /healthz error id.
	Health func(reason string)

	// alive reports whether the daemon process is running. It defaults to
	// checking the daemon PID file; tests override it.
	alive func() bool

	mu       sync.Mutex
	ready    bool
	stopping bool
	attempt  int
}

// SetupEnv prepares HOME and XDG_RUNTIME_DIR as writable scratch dirs so
// PulseAudio and its clients find the native socket. Call it before starting
// any other subprocess that talks to PulseAudio.
func (p *PulseAudio) SetupEnv() error {
	home := p.homeDir()
	runtime := p.runtimeDir()

	for _, d := range []string{home, runtime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	if err := os.Setenv("HOME", home); err != nil {
		return err
	}

	if err := os.Setenv("XDG_RUNTIME_DIR", runtime); err != nil {
		return err
	}

	return os.Setenv("PULSE_SERVER", "unix:"+filepath.Join(runtime, "pulse", "native"))
}

// Start brings the daemon up and verifies the sink. It returns an error when
// the daemon fails to start or the sink is missing.
func (p *PulseAudio) Start() error {
	if err := p.startDaemon(); err != nil {
		p.setReady(false)
		p.setHealth(PulseNotReady)

		return err
	}

	p.setReady(true)
	p.setHealth("")

	return nil
}

// Ready reports whether the daemon is currently up and usable.
func (p *PulseAudio) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.ready
}

// Stop shuts the daemon down cleanly. It prefers pactl exit and falls back to
// signalling the daemon PID.
func (p *PulseAudio) Stop() {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()

		return
	}
	p.stopping = true
	p.mu.Unlock()

	p.setReady(false)

	if _, err := p.pactlRun("exit"); err == nil {
		if p.waitDaemonGone(p.stopGrace()) {
			return
		}
	} else {
		slog.Warn("pulseaudio: pactl exit failed", "err", err)
	}

	if pid, err := p.daemonPID(); err == nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	p.waitDaemonGone(p.stopGrace())
}

// Run supervises the daemon until ctx is cancelled: it starts PulseAudio,
// probes liveness, restarts on death with exponential backoff, and reports
// health changes via Health.
func (p *PulseAudio) Run(ctx context.Context) {
	backoff := p.startBackoff()

	for {
		if ctx.Err() != nil || p.isStopping() {
			return
		}

		p.attempt++

		if err := p.startDaemon(); err != nil {
			slog.Error("pulseaudio: start failed",
				"err", err, "attempt", p.attempt, "backoff", backoff)
			p.setReady(false)
			p.setHealth(PulseNotReady)
		} else {
			slog.Info("pulseaudio: ready")
			p.setReady(true)
			p.setHealth("")
		}

		if !p.waitDaemonDown(ctx) {
			return
		}

		if p.isStopping() {
			return
		}

		slog.Warn("pulseaudio: daemon not reachable, restarting",
			"attempt", p.attempt, "backoff", backoff)
		p.setReady(false)
		p.setHealth(PulseNotReady)

		if !sleep(ctx, backoff) {
			return
		}

		backoff = NextBackoff(backoff, p.maxBackoff())
	}
}

// startDaemon performs one bring-up: spawn the daemonizing launcher, wait for
// the native socket, verify the virtual_out sink, and make it the default.
func (p *PulseAudio) startDaemon() error {
	if err := p.SetupEnv(); err != nil {
		return fmt.Errorf("prepare env: %w", err)
	}

	p.removeStaleRuntime()

	proc, err := p.Manager.Start(context.Background(), "pulseaudio", p.daemonArgs()...)
	if err != nil {
		return fmt.Errorf("start pulseaudio: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()

	if err := p.waitForSocket(waitCh); err != nil {
		return err
	}

	if err := p.waitSinkReady(); err != nil {
		p.shutdownBestEffort()

		return err
	}

	if _, err := p.pactlRun("set-default-sink", SinkName); err != nil {
		p.shutdownBestEffort()

		return fmt.Errorf("set default sink: %w", err)
	}

	return nil
}

// waitForSocket polls for the native Unix socket, aborting early if the
// launcher reports a failed daemon startup.
func (p *PulseAudio) waitForSocket(waitCh <-chan error) error {
	socket := filepath.Join(p.runtimeDir(), "pulse", "native")
	timeout := time.NewTimer(p.readyTimeout())
	defer timeout.Stop()

	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	for {
		select {
		case exitErr := <-waitCh:
			if process.ExitCode(exitErr) != 0 {
				return fmt.Errorf("pulseaudio failed to daemonize: %w", exitErr)
			}
		case <-timeout.C:
			return errSocketTimeout
		case <-ticker.C:
			if _, err := os.Stat(socket); err == nil {
				return nil
			}
		}
	}
}

// waitSinkReady retries pactl list sinks until virtual_out is listed.
func (p *PulseAudio) waitSinkReady() error {
	var lastErr error

	for i := 0; i < defaultPactlRetries; i++ {
		out, err := p.pactlRun("list", "sinks", "short")
		if err != nil {
			lastErr = err
		} else if sinkListed(out) {
			return nil
		} else {
			lastErr = errors.New("virtual_out sink not found")
		}

		time.Sleep(pactlRetryInterval)
	}

	return lastErr
}

// sinkListed reports whether a pactl sinks short listing contains a sink
// named SinkName. Lines look like "<index>\t<name>\t<module>\t...".
func sinkListed(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == SinkName {
			return true
		}
	}

	return false
}

// pactlRun runs pactl and returns its standard output.
func (p *PulseAudio) pactlRun(args ...string) (string, error) {
	proc, err := p.Manager.Start(context.Background(), "pactl", args...)
	if err != nil {
		return "", err
	}

	out, _ := io.ReadAll(proc.Stdout())

	if err := proc.Wait(); err != nil {
		return string(out), err
	}

	return string(out), nil
}

// shutdownBestEffort asks the daemon to exit without surfacing an error. It is
// used to clean up a partially verified startup.
func (p *PulseAudio) shutdownBestEffort() {
	_, _ = p.pactlRun("exit")
}

// waitDaemonDown polls daemon liveness until it dies or ctx is cancelled or a
// stop was requested.
func (p *PulseAudio) waitDaemonDown(ctx context.Context) bool {
	ticker := time.NewTicker(p.probeInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if p.isStopping() {
				return false
			}

			if !p.daemonAlive() {
				return true
			}
		}
	}
}

// waitDaemonGone waits up to d for the daemon to stop responding.
func (p *PulseAudio) waitDaemonGone(d time.Duration) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()

	ticker := time.NewTicker(pidPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.C:
			return !p.daemonAlive()
		case <-ticker.C:
			if !p.daemonAlive() {
				return true
			}
		}
	}
}

// daemonAlive reports whether the PulseAudio daemon process is running.
func (p *PulseAudio) daemonAlive() bool {
	if p.alive != nil {
		return p.alive()
	}

	pid, err := p.daemonPID()
	if err != nil {
		return false
	}

	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}

	return strings.HasPrefix(strings.TrimSpace(string(comm)), "pulseaudio")
}

// daemonPID reads the daemon PID published by PulseAudio.
func (p *PulseAudio) daemonPID() (int, error) {
	raw, err := os.ReadFile(filepath.Join(p.runtimeDir(), "pulse", "pid"))
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, err
	}

	return pid, nil
}

// removeStaleRuntime clears the native socket and PID file from an earlier
// daemon so a restart binds cleanly.
func (p *PulseAudio) removeStaleRuntime() {
	pulseDir := filepath.Join(p.runtimeDir(), "pulse")
	_ = os.Remove(filepath.Join(pulseDir, "native"))
	_ = os.Remove(filepath.Join(pulseDir, "pid"))
}

// daemonArgs builds the PulseAudio command line. The null sink and the native
// protocol module are loaded in the same invocation; -n skips the system
// config which could otherwise override these modules.
func (p *PulseAudio) daemonArgs() []string {
	return []string{
		"--exit-idle-time=-1",
		"-n",
		"--load=module-native-protocol-unix",
		fmt.Sprintf("--load=module-null-sink sink_name=%s rate=44100 channels=2 sink_properties=device.description=%s", SinkName, sinkDeviceDesc),
		"--daemonize=yes",
		"--log-target=stderr",
	}
}

func (p *PulseAudio) homeDir() string {
	if p.HomeDir != "" {
		return p.HomeDir
	}

	return defaultHomeDir
}

func (p *PulseAudio) runtimeDir() string {
	if p.XDGRuntimeDir != "" {
		return p.XDGRuntimeDir
	}

	return defaultRuntimeDir
}

func (p *PulseAudio) readyTimeout() time.Duration {
	if p.ReadyTimeout > 0 {
		return p.ReadyTimeout
	}

	return defaultReadyTimeout
}

func (p *PulseAudio) startBackoff() time.Duration {
	if p.StartBackoff > 0 {
		return p.StartBackoff
	}

	return defaultStartBackoff
}

func (p *PulseAudio) maxBackoff() time.Duration {
	if p.MaxBackoff > 0 {
		return p.MaxBackoff
	}

	return defaultMaxBackoff
}

func (p *PulseAudio) probeInterval() time.Duration {
	if p.ProbeInterval > 0 {
		return p.ProbeInterval
	}

	return defaultProbeInterval
}

func (p *PulseAudio) stopGrace() time.Duration {
	if p.StopGrace > 0 {
		return p.StopGrace
	}

	return defaultStopGrace
}

func (p *PulseAudio) setReady(v bool) {
	p.mu.Lock()
	p.ready = v
	p.mu.Unlock()
}

func (p *PulseAudio) isStopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.stopping
}

func (p *PulseAudio) setHealth(reason string) {
	if p.Health != nil {
		p.Health(reason)
	}
}

// NextBackoff doubles current up to max. It never returns less than current or
// more than max.
func NextBackoff(current, max time.Duration) time.Duration {
	if current >= max {
		return max
	}

	next := current * 2
	if next <= 0 || next > max {
		return max
	}

	return next
}

// sleep returns true after d elapses, or false when ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
