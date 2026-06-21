package main

import (
	"strings"
	"testing"
)

// TestResolveClaudeBinMissingIsActionable pins that a missing Claude Code
// binary fails fast (it is the first launch step) with a message that tells the
// user how to fix it — not the old cryptic "find claude executable: ..." that
// was only reached after CA/proxy/session bring-up.
func TestResolveClaudeBinMissingIsActionable(t *testing.T) {
	l := &sessionLauncher{launch: launchArgs{ClaudeBin: "ccwrap-nonexistent-claude-xyzzy"}}
	err := l.ResolveClaudeBin()
	if err == nil {
		t.Fatal("expected an error for a missing claude binary")
	}
	msg := err.Error()
	for _, want := range []string{"ccwrap-nonexistent-claude-xyzzy", "PATH", "--claude-bin", "CLAUDE_BIN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should mention %q to be actionable; got:\n%s", want, msg)
		}
	}
}

// TestResolveClaudeBinIdempotent pins that a pre-resolved binary short-circuits
// (SpawnChild relies on this so the early step + its own call don't double-look).
func TestResolveClaudeBinIdempotent(t *testing.T) {
	l := &sessionLauncher{bin: "/already/resolved/claude", launch: launchArgs{ClaudeBin: "ccwrap-nonexistent-claude-xyzzy"}}
	if err := l.ResolveClaudeBin(); err != nil {
		t.Fatalf("pre-resolved bin should short-circuit, got: %v", err)
	}
	if l.bin != "/already/resolved/claude" {
		t.Fatalf("bin mutated to %q", l.bin)
	}
}

// TestAuthPassthroughWarning pins that the riskiest auth opt-out is surfaced
// loudly (and only when enabled), parallel to the native-TLS kill-switch warning.
func TestAuthPassthroughWarning(t *testing.T) {
	if authPassthroughWarning(false) != "" {
		t.Error("no warning expected when passthrough is off")
	}
	w := authPassthroughWarning(true)
	for _, want := range []string{"--allow-auth-passthrough-to-third-party", "leak", "fail closed"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning should mention %q; got: %s", want, w)
		}
	}
}
