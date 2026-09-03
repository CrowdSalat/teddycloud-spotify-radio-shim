// Package soloist manages the Soloist subprocess lifecycle.
package soloist

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	cdnBase    = "https://soloist-builds.spotifycdn.com"
	expiryDays = 90
	warnDays   = 80
	binaryName = "soloist"
)

// cdnURL returns the Spotify CDN download URL for the current architecture.
func cdnURL() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return cdnBase + "/soloist_release_x86_64.tar.gz", nil
	case "arm64":
		return cdnBase + "/soloist_release_arm64.tar.gz", nil
	case "arm":
		return cdnBase + "/soloist_release_arm32.tar.gz", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}

// BinaryManager finds or downloads the Soloist binary.
type BinaryManager struct {
	// ExplicitPath is the path from SOLOIST_BIN env var. Skips discovery if set.
	ExplicitPath string
	// DataDir is SOLOIST_DATA_DIR. Downloaded binary is placed here.
	DataDir string
}

// Resolve returns the path to a usable Soloist binary.
// Resolution order: ExplicitPath → PATH → DataDir/bin/soloist → download.
func (b *BinaryManager) Resolve() (string, error) {
	if b.ExplicitPath != "" {
		slog.Info("soloist binary: using explicit path", "path", b.ExplicitPath)
		return b.ExplicitPath, b.smokeTest(b.ExplicitPath)
	}

	// Check PATH.
	if path, err := exec.LookPath(binaryName); err == nil {
		slog.Info("soloist binary: found on PATH", "path", path)
		b.checkAge(path)

		return path, b.smokeTest(path)
	}

	// Check data dir.
	dataPath := filepath.Join(b.DataDir, "bin", binaryName)
	if _, err := os.Stat(dataPath); err == nil {
		slog.Info("soloist binary: found in data dir", "path", dataPath)
		b.checkAge(dataPath)

		return dataPath, b.smokeTest(dataPath)
	}

	// Download.
	slog.Info("soloist binary: not found, downloading from Spotify CDN")

	path, err := b.download()
	if err != nil {
		return "", fmt.Errorf("download soloist: %w", err)
	}

	return path, b.smokeTest(path)
}

// download fetches the tarball from the Spotify CDN and extracts the binary.
func (b *BinaryManager) download() (string, error) {
	url, err := cdnURL()
	if err != nil {
		return "", err
	}

	slog.Info("soloist binary: downloading", "url", url)

	resp, err := http.Get(url) //nolint:noctx // startup download, no request context needed
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}

	dest := filepath.Join(b.DataDir, "bin", binaryName)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create bin dir: %w", err)
	}

	if err := extractBinary(resp.Body, dest); err != nil {
		return "", fmt.Errorf("extract binary: %w", err)
	}

	slog.Info("soloist binary: downloaded", "path", dest)

	return dest, nil
}

// extractBinary reads a .tar.gz stream and writes the "soloist" entry to dest.
func extractBinary(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		if hdr.Name != binaryName {
			continue
		}

		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("create %s: %w", dest, err)
		}

		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return fmt.Errorf("write %s: %w", dest, err)
		}

		return f.Close()
	}

	return fmt.Errorf("soloist binary not found in tarball")
}

// smokeTest runs soloist --version to verify the binary executes.
func (b *BinaryManager) smokeTest(path string) error {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("soloist smoke test failed (%s): %w", string(out), err)
	}

	slog.Info("soloist binary: smoke test passed", "version", string(out))

	return nil
}

// checkAge logs a warning if the binary is older than warnDays.
// On expiry (>= expiryDays) it logs an error but does not fail — the running
// process will exit with code 10, which the supervisor handles.
func (b *BinaryManager) checkAge(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	age := time.Since(info.ModTime())
	days := int(age.Hours() / 24)

	switch {
	case days >= expiryDays:
		slog.Error("soloist binary: expired", "age_days", days, "path", path)
	case days >= warnDays:
		slog.Warn("soloist binary: expiry approaching", "age_days", days,
			"days_remaining", expiryDays-days, "path", path)
	default:
		slog.Debug("soloist binary: age ok", "age_days", days,
			"days_remaining", expiryDays-days)
	}
}

// DeleteBinary removes the binary from the data dir so it is re-downloaded
// on next startup. Called by the supervisor on exit code 10.
func (b *BinaryManager) DeleteBinary() {
	path := filepath.Join(b.DataDir, "bin", binaryName)

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("soloist binary: failed to delete expired binary", "err", err, "path", path)

		return
	}

	slog.Info("soloist binary: deleted expired binary, will re-download on next start", "path", path)
}
