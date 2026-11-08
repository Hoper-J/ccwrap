package update

import (
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/statefile"
)

// ClaudeFile records the Claude Code version the latest session ran, which
// ccwrap doctor compares with the latest release. Claude Code does not
// update itself in ccwrap sessions, so ccwrap reports newer versions.
const ClaudeFile = "claude-version.json"

// ClaudeSeen is the Claude Code version a session ran and when it ended.
type ClaudeSeen struct {
	Version string    `json:"version"`
	SeenAt  time.Time `json:"seen_at"`
}

// SaveClaudeSeen records version as the one the latest session ran.
func SaveClaudeSeen(stateDir, version string, now time.Time) error {
	return statefile.Write(stateDir, ClaudeFile, ClaudeSeen{Version: version, SeenAt: now.UTC()})
}

// LoadClaudeSeen returns the recorded version, if there is a usable one.
func LoadClaudeSeen(stateDir string) (ClaudeSeen, bool) {
	var c ClaudeSeen
	statefile.Read(stateDir, ClaudeFile, &c)
	return c, strings.TrimSpace(c.Version) != ""
}
