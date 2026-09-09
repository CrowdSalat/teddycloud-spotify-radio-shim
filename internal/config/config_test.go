package config_test

import (
	"testing"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/config"
)

func TestLoad_MissingRequired(t *testing.T) {
	t.Setenv("SOLOIST_API_KEY", "")
	t.Setenv("TEDDYCLOUD_URL", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for missing required vars, got nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("SOLOIST_API_KEY", "test-key")
	t.Setenv("TEDDYCLOUD_URL", "http://teddycloud")

	c, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr: got %q, want %q", c.ListenAddr, ":8080")
	}

	if c.SoloistDataDir != "/data" {
		t.Errorf("SoloistDataDir: got %q, want %q", c.SoloistDataDir, "/data")
	}

	if c.SoloistDeviceName != "teddycloud-spotify-shim" {
		t.Errorf("SoloistDeviceName: got %q, want %q", c.SoloistDeviceName, "teddycloud-spotify-shim")
	}

	if c.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want %q", c.LogLevel, "info")
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("SOLOIST_API_KEY", "my-key")
	t.Setenv("TEDDYCLOUD_URL", "http://mycloud")
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("LOG_LEVEL", "debug")

	c, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c.SoloistAPIKey != "my-key" {
		t.Errorf("SoloistAPIKey: got %q, want %q", c.SoloistAPIKey, "my-key")
	}

	if c.ListenAddr != ":9090" {
		t.Errorf("ListenAddr: got %q, want %q", c.ListenAddr, ":9090")
	}

	if c.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want %q", c.LogLevel, "debug")
	}
}
