package soloist_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/soloist"
)

func TestSessionChecker_MissingSession(t *testing.T) {
	dir := t.TempDir()

	checker := &soloist.SessionChecker{DataDir: dir}

	if got := checker.Check(); got != soloist.StateUnpaired {
		t.Errorf("Check(): got %q, want %q", got, soloist.StateUnpaired)
	}
}

func TestSessionChecker_PresentSession(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "settings", "Users"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	checker := &soloist.SessionChecker{DataDir: dir}

	if got := checker.Check(); got != soloist.StateReady {
		t.Errorf("Check(): got %q, want %q", got, soloist.StateReady)
	}
}
