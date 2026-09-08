package soloist

import (
	"os"
	"path/filepath"
)

// sessionUsersRel is the data-dir-relative path to authenticated per-user
// session storage. Soloist writes it only after a Spotify app completes
// pairing; an unpaired data dir contains no settings/ tree.
const sessionUsersRel = "settings/Users"

// SessionState is the result of a session check.
type SessionState string

const (
	// StateReady means a stored Soloist session was found.
	StateReady SessionState = "ready"
	// StateUnpaired means no stored Soloist session was found.
	StateUnpaired SessionState = "unpaired"
)

// SessionChecker inspects the Soloist data directory for a stored session.
type SessionChecker struct {
	// DataDir is SOLOIST_DATA_DIR.
	DataDir string
}

// Check returns ready if the data directory holds a stored session, unpaired otherwise.
func (c *SessionChecker) Check() SessionState {
	info, err := os.Stat(filepath.Join(c.DataDir, sessionUsersRel))
	if err == nil && info.IsDir() {
		return StateReady
	}

	return StateUnpaired
}
