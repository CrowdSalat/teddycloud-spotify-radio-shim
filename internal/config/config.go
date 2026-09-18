// Package config loads shim configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the shim.
type Config struct {
	// SoloistAPIKey is the Spotify Soloist API key. Required.
	SoloistAPIKey string
	// TeddycloudURL is the base URL of the Teddycloud instance. Required.
	TeddycloudURL string
	// ListenAddr is the HTTP listen address for the shim.
	ListenAddr string
	// SoloistDataDir is the directory for Soloist session and runtime files.
	SoloistDataDir string
	// SoloistCacheDir is the directory for Soloist playback cache.
	SoloistCacheDir string
	// SoloistDeviceName is the Spotify Connect device name.
	SoloistDeviceName string
	// SoloistBin is an explicit path to the soloist binary. If empty, auto-discovery is used.
	SoloistBin string
	// SoloistVolume is the Soloist playback volume (0–100). Set via SOLOIST_VOLUME.
	SoloistVolume int
	// LogLevel controls log verbosity: debug, info, warn, error.
	LogLevel string
	// StaticStream forces /stream to serve a synthesized WAV instead of the
	// live recorder, for the Phase 12 static-file ingest test.
	StaticStream bool
	// StaticSampleRate is the sample rate of the synthesized static stream.
	StaticSampleRate uint32
	// RecorderBuffer is the buffered channel capacity in chunks for the live
	// PulseRecorder. More slack absorbs transient consumer stalls instead of
	// dropping chunks. Set via RECORDER_BUFFER.
	RecorderBuffer int
}

// Load reads configuration from environment variables.
// Returns an error if any required variable is missing.
func Load() (*Config, error) {
	c := &Config{
		ListenAddr:        getenv("LISTEN_ADDR", ":8080"),
		SoloistDataDir:    getenv("SOLOIST_DATA_DIR", "/data"),
		SoloistCacheDir:   getenv("SOLOIST_CACHE_DIR", "/cache"),
		SoloistDeviceName: getenv("SOLOIST_DEVICE_NAME", "teddycloud-spotify-shim"),
		SoloistBin:        getenv("SOLOIST_BIN", ""),
		LogLevel:          getenv("LOG_LEVEL", "info"),
	}

	var missing []string

	if v := os.Getenv("SOLOIST_API_KEY"); v != "" {
		c.SoloistAPIKey = v
	} else {
		missing = append(missing, "SOLOIST_API_KEY")
	}

	if v := os.Getenv("TEDDYCLOUD_URL"); v != "" {
		c.TeddycloudURL = v
	} else {
		missing = append(missing, "TEDDYCLOUD_URL")
	}

	vol, err := getenvInt("SOLOIST_VOLUME", 100)
	if err != nil || vol < 0 || vol > 100 {
		return nil, fmt.Errorf("SOLOIST_VOLUME must be 0–100, got %q", os.Getenv("SOLOIST_VOLUME"))
	}
	c.SoloistVolume = vol

	c.StaticStream = getenvBool("STATIC_STREAM")

	sr, err := getenvInt("STATIC_SAMPLE_RATE", 22050)
	if err != nil || sr < 8000 || sr > 96000 {
		return nil, fmt.Errorf("STATIC_SAMPLE_RATE must be 8000–96000, got %q", os.Getenv("STATIC_SAMPLE_RATE"))
	}
	c.StaticSampleRate = uint32(sr)

	buf, err := getenvInt("RECORDER_BUFFER", 256)
	if err != nil || buf < 1 || buf > 65536 {
		return nil, fmt.Errorf("RECORDER_BUFFER must be 1–65536 chunks, got %q", os.Getenv("RECORDER_BUFFER"))
	}
	c.RecorderBuffer = buf

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %v", missing)
	}

	return c, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func getenvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}

	return strconv.Atoi(v)
}

func getenvBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
