package process

import (
	"bytes"
	"context"
	"io"
)

// Fake is a test Manager that returns a pre-configured FakeProcess.
type Fake struct {
	// ExitErr is the error returned by Wait. Use ExitError to set a specific exit code.
	ExitErr error
	// StdoutData is the content returned by Stdout.
	StdoutData []byte
}

// Start implements Manager. It ignores the command and returns a FakeProcess.
func (f *Fake) Start(_ context.Context, _ string, _ ...string) (Process, error) {
	return &FakeProcess{
		exitErr: f.ExitErr,
		stdout:  bytes.NewReader(f.StdoutData),
	}, nil
}

// FakeProcess is a Process whose behaviour is controlled by the test.
type FakeProcess struct {
	exitErr error
	stdout  io.Reader
}

// Wait implements Process.
func (p *FakeProcess) Wait() error {
	return p.exitErr
}

// Kill implements Process.
func (p *FakeProcess) Kill() error {
	return nil
}

// Stdout implements Process.
func (p *FakeProcess) Stdout() io.Reader {
	return p.stdout
}
