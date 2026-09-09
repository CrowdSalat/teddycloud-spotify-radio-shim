package audio

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// monitorSampleRate is the record format shared with PulseRecorder: s16le at
// 44100 Hz, stereo (4 bytes/frame).
const monitorSampleRate = 44100

// monitorStream turns a live PulseAudio monitor recording of SinkName into an
// io.Reader. PCM is delivered already shaped as s16le, 44100 Hz, stereo, so
// PulseRecorder can consume it without client-side resampling.
type monitorStream struct {
	client *pulse.Client
	rec    *pulse.RecordStream
	reader *io.PipeReader
	writer *io.PipeWriter

	closeOnce sync.Once
}

// OpenMonitorStream connects to the PulseAudio server at server (a native
// protocol server string) and opens a monitor recording from the SinkName
// sink. The returned ReadCloser yields s16le, 44100 Hz, stereo PCM; Close stops
// the recording and releases the connection. Errors distinguish connect,
// sink-lookup, and record-setup failures for caller reconnection logic.
func OpenMonitorStream(ctx context.Context, server string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	client, err := pulse.NewClient(pulse.ClientServerString(server))
	if err != nil {
		return nil, fmt.Errorf("connect to pulse server %q: %w", server, err)
	}

	sinks, err := client.ListSinks()
	if err != nil {
		client.Close()

		return nil, fmt.Errorf("list pulse sinks: %w", err)
	}

	sink := findSinkByName(sinks)
	if sink == nil {
		client.Close()

		return nil, fmt.Errorf("pulse sink %q not found", SinkName)
	}

	reader, writer := io.Pipe()
	rec, err := client.NewRecord(
		pulse.NewWriter(writer, proto.FormatInt16LE),
		pulse.RecordMonitor(sink),
		pulse.RecordStereo,
		pulse.RecordSampleRate(monitorSampleRate),
	)
	if err != nil {
		_ = writer.Close()
		client.Close()

		return nil, fmt.Errorf("create pulse record stream: %w", err)
	}

	rec.Start()

	return &monitorStream{client: client, rec: rec, reader: reader, writer: writer}, nil
}

// Read implements io.Reader.
func (m *monitorStream) Read(p []byte) (int, error) {
	return m.reader.Read(p)
}

// Close releases the record stream and the pipe so a blocked Read returns
// io.EOF. It is safe to call multiple times.
func (m *monitorStream) Close() error {
	var err error

	m.closeOnce.Do(func() {
		m.rec.Close()
		err = m.writer.Close()
		m.client.Close()
	})

	return err
}

// findSinkByName returns the sink whose unique name matches want, or nil.
func findSinkByName(sinks []*pulse.Sink) *pulse.Sink {
	for _, s := range sinks {
		if s.ID() == SinkName {
			return s
		}
	}

	return nil
}
