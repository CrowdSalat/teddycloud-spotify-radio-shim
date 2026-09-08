package soloist

import (
	"context"
	"log/slog"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/process"
)

// PairOutcome is the result of a pairing attempt or the pairing loop.
type PairOutcome string

const (
	// PairDone means pairing succeeded: the pair process exited 0 and/or the
	// session appeared in the data dir.
	PairDone PairOutcome = "done"
	// PairRetry means the pair process failed and pairing should be retried
	// with exponential backoff.
	PairRetry PairOutcome = "retry"
	// PairExpired means the soloist binary is expired (exit code 10); pairing
	// must not be retried.
	PairExpired PairOutcome = "expired"
	// PairCanceled means the pairing context was cancelled.
	PairCanceled PairOutcome = "canceled"
)

// defaultPairPollInterval is how often the session is re-checked while the
// pair process runs.
const defaultPairPollInterval = 2 * time.Second

// Pairer owns the one-time Soloist pairing subprocess lifecycle.
type Pairer struct {
	// BinaryPath is the resolved soloist binary path.
	BinaryPath string
	// DeviceName is SOLOIST_DEVICE_NAME, advertised in the Spotify app.
	DeviceName string
	// APIKey is SOLOIST_API_KEY.
	APIKey string
	// DataDir is SOLOIST_DATA_DIR.
	DataDir string
	// CacheDir is SOLOIST_CACHE_DIR.
	CacheDir string
	// Manager spawns the pair process.
	Manager process.Manager
	// Checker reports whether a stored session exists.
	Checker *SessionChecker
	// BinaryManager removes the expired binary on exit code 10.
	BinaryManager *BinaryManager
	// PollInterval is the session re-check interval while pairing. Zero uses
	// the default.
	PollInterval time.Duration
}

// Pair spawns soloist in pair mode and blocks until the session is stored, the
// process exits, or ctx is cancelled. It is safe to call again on a retry.
func (p *Pairer) Pair(ctx context.Context) PairOutcome {
	checker := p.Checker
	if checker == nil {
		checker = &SessionChecker{DataDir: p.DataDir}
	}

	args := []string{
		"--device-name", p.DeviceName,
		"--api-key", p.APIKey,
		"--data-dir", p.DataDir,
		"--cache-dir", p.CacheDir,
		"--pair",
	}

	slog.Info("soloist pair: starting", "binary", p.BinaryPath, "device", p.DeviceName)

	proc, err := p.Manager.Start(ctx, p.BinaryPath, args...)
	if err != nil {
		slog.Error("soloist pair: start failed", "err", err)

		return PairRetry
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()

	interval := p.PollInterval
	if interval <= 0 {
		interval = defaultPairPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Warn("soloist pair: context canceled, terminating")
			_ = proc.Kill()

			return PairCanceled
		case waitErr := <-waitCh:
			switch code := process.ExitCode(waitErr); code {
			case 0:
				if checker.Check() == StateReady {
					slog.Info("soloist pair: exited 0, session stored")
				} else {
					slog.Warn("soloist pair: exited 0 without a stored session")
				}

				return PairDone
			case 10:
				slog.Error("soloist pair: binary expired", "exit_code", code, "err", waitErr)
				if p.BinaryManager != nil {
					p.BinaryManager.DeleteBinary()
				}

				return PairExpired
			default:
				slog.Error("soloist pair: failed", "exit_code", code, "err", waitErr)

				return PairRetry
			}
		case <-ticker.C:
			if checker.Check() == StateReady {
				slog.Info("soloist pair: session stored, terminating pair process")
				_ = proc.Kill()

				return PairDone
			}
		}
	}
}

// NextBackoff doubles current up to max. It never returns less than current or
// more than max.
func NextBackoff(current, max time.Duration) time.Duration {
	if current >= max {
		return max
	}

	next := current * 2
	if next <= 0 || next > max {
		return max
	}

	return next
}

// Drive repeats pair until it returns a non-retry outcome or ctx is cancelled,
// retrying failures with exponential backoff from initial up to max.
// Initial should be positive.
func Drive(ctx context.Context, pair func(context.Context) PairOutcome, initial, max time.Duration) PairOutcome {
	backoff := initial

	for {
		outcome := pair(ctx)
		if outcome != PairRetry {
			return outcome
		}

		slog.Warn("soloist pair: retrying", "backoff", backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()

			return PairCanceled
		case <-timer.C:
		}

		backoff = NextBackoff(backoff, max)
	}
}
