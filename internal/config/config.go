// Package config loads shim configuration from environment variables.
package config

import (
	"fmt"
	"os"
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
	// LogLevel controls log verbosity: debug, info, warn, error.
	LogLevel string
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
