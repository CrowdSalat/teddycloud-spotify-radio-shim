// Package process provides an interface for managing subprocesses.
package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
)

// Manager starts and controls a subprocess.
type Manager interface {
	// Start starts the subprocess. Returns an error if it fails to start.
	Start(ctx context.Context, name string, args ...string) (Process, error)
}

// Process represents a running subprocess.
type Process interface {
	// Wait blocks until the process exits and returns its exit error.
	Wait() error
	// Kill terminates the process immediately.
	Kill() error
	// Stdout returns a reader for the process standard output.
	Stdout() io.Reader
}

// ExecManager is the production Manager implementation using os/exec.
type ExecManager struct{}

// Start implements Manager.
func (ExecManager) Start(ctx context.Context, name string, args ...string) (Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &execProcess{cmd: cmd, stdout: stdout}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Kill() error {
	return p.cmd.Process.Kill()
}

func (p *execProcess) Stdout() io.Reader {
	return p.stdout
}

// ExitCode extracts the exit code from an error returned by Process.Wait.
// A nil error means exit code 0. If the error is not an *exec.ExitError or
// does not carry a valid exit code, ExitCode returns -1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return -1
}
