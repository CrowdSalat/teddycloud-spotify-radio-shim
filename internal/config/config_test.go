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

	if c.SoloistVolume != 100 {
		t.Errorf("SoloistVolume: got %d, want %d", c.SoloistVolume, 100)
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
	t.Setenv("SOLOIST_VOLUME", "50")

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

	if c.SoloistVolume != 50 {
		t.Errorf("SoloistVolume: got %d, want %d", c.SoloistVolume, 50)
	}

	if c.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want %q", c.LogLevel, "debug")
	}
}

func TestLoad_VolumeValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valid zero", "0", false},
		{"valid hundred", "100", false},
		{"below range", "-1", true},
		{"above range", "101", true},
		{"non-numeric", "abc", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SOLOIST_API_KEY", "test-key")
			t.Setenv("TEDDYCLOUD_URL", "http://teddycloud")
			t.Setenv("SOLOIST_VOLUME", tt.value)

			_, err := config.Load()
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
