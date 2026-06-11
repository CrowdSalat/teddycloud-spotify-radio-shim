package radio

import (
	"encoding/binary"
	"io"
)

// WAV format constants matching go-librespot's fixed output format.
const (
	SampleRate  = 44100
	Channels    = 2
	BitDepth    = 16
	ContentType = "audio/wav"
)

// WriteStreamingWAVHeader writes a WAV header suitable for a stream of
// unknown length. Size fields are set to 0xFFFFFFFF per the streaming
// WAV convention — decoders treat this as "read until EOF."
//
// NOTE: The Toniebox natively plays OGG/Vorbis. Whether it accepts WAV
// via Teddycloud proxy needs validation in integration testing (see
// ARCHITECTURE.md open questions). This is the simplest first attempt.
func WriteStreamingWAVHeader(w io.Writer) error {
	const unknown = uint32(0xFFFFFFFF)

	byteRate := uint32(SampleRate * Channels * BitDepth / 8)
	blockAlign := uint16(Channels * BitDepth / 8)

	// RIFF chunk.
	if err := writeBytes(w, []byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, unknown); err != nil {
		return err
	}
	if err := writeBytes(w, []byte("WAVE")); err != nil {
		return err
	}

	// fmt sub-chunk.
	if err := writeBytes(w, []byte("fmt ")); err != nil {
		return err
	}
	for _, v := range []any{
		uint32(16), // sub-chunk size
		uint16(1),  // PCM format
		uint16(Channels),
		uint32(SampleRate),
		byteRate,
		blockAlign,
		uint16(BitDepth),
	} {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}

	// data sub-chunk header.
	if err := writeBytes(w, []byte("data")); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, unknown)
}

func writeBytes(w io.Writer, b []byte) error {
	_, err := w.Write(b)
	return err
}
