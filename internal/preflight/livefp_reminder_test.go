//go:build livefp

package preflight

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestLiveClaudeReminderSwitches checks that the installed (in CI: the latest)
// Claude Code still reads the env switches ClaudeReminderOffEnv sets. They are
// internal codenames: if a release renames or drops one, ccwrap's fix for
// non-Claude aliases silently stops applying, so a missing name is logged as
// CLAUDE_REMINDER_SWITCH_MISSING (the weekly workflow turns it into a warning)
// for a maintainer to check what the new release reads instead.
//
// claude comes from CCWRAP_LIVEFP_CLAUDE_BIN or PATH; without it the test skips,
// unless CCWRAP_LIVEFP_REQUIRE_CLAUDE=1.
//
//	go test ./internal/preflight/ -tags livefp -run TestLiveClaudeReminderSwitches -v
func TestLiveClaudeReminderSwitches(t *testing.T) {
	bin := os.Getenv("CCWRAP_LIVEFP_CLAUDE_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("claude")
	}
	if bin == "" {
		if os.Getenv("CCWRAP_LIVEFP_REQUIRE_CLAUDE") != "" {
			t.Fatal("claude not found and CCWRAP_LIVEFP_REQUIRE_CLAUDE is set")
		}
		t.Skip("claude not found; put it on PATH or set CCWRAP_LIVEFP_CLAUDE_BIN")
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	found, err := fileContainsAll(real, claudeReminderEnvKeys)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range claudeReminderEnvKeys {
		if found[key] {
			t.Logf("%s present in %s", key, real)
			continue
		}
		t.Logf("CLAUDE_REMINDER_SWITCH_MISSING %s is not in %s: ClaudeReminderOffEnv may no longer switch that reminder off", key, real)
	}
}

// fileContainsAll streams path once and reports which needles occur in it.
func fileContainsAll(path string, needles []string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	overlap := 0
	for _, n := range needles {
		overlap = max(overlap, len(n)-1)
	}
	found := map[string]bool{}
	buf := make([]byte, 1<<20)
	var tail []byte
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := append(tail, buf[:n]...)
			for _, needle := range needles {
				if !found[needle] && bytes.Contains(chunk, []byte(needle)) {
					found[needle] = true
				}
			}
			tail = append([]byte(nil), chunk[len(chunk)-min(len(chunk), overlap):]...)
		}
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
