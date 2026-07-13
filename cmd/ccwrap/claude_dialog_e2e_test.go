package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Opt-in acceptance check for the bearer-placeholder contract: an x-api-key
// provider profile must launch interactive Claude Code WITHOUT the
// "Detected a custom API key" approval dialog (which, combined with
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST, strands a declining user at
// "Not logged in" with no /login recovery).
//
// It drives the REAL installed `claude` binary in a tmux pane, so it is
// environment-dependent by design and skipped unless explicitly requested:
//
//	CCWRAP_E2E_CLAUDE_DIALOG=1 go test ./cmd/ccwrap -run TestClaudeDialogE2E -v -timeout 10m
//
// Re-run after Claude Code upgrades: it pins an upstream behavior (the env
// ANTHROPIC_API_KEY approval gate vs. the ungated ANTHROPIC_AUTH_TOKEN)
// that ccwrap cannot control.
//
// The control case runs FIRST and asserts the dialog DOES appear for an env
// ANTHROPIC_API_KEY placeholder. That proves the harness can detect the
// dialog at all — without it, "no dialog seen" in the subject case would be
// indistinguishable from a broken marker (e.g. Claude Code rewording the
// prompt).
const dialogMarker = "Detected a custom API key"

func TestClaudeDialogE2E_BearerPlaceholderSkipsApprovalDialog(t *testing.T) {
	if os.Getenv("CCWRAP_E2E_CLAUDE_DIALOG") == "" {
		t.Skip("opt-in e2e: set CCWRAP_E2E_CLAUDE_DIALOG=1 (drives the real claude binary in tmux)")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH (needed to build ccwrap)")
	}

	root := t.TempDir()
	ccwrapBin := filepath.Join(root, "ccwrap-e2e")
	if out, err := exec.Command(goBin, "build", "-o", ccwrapBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build ccwrap: %v\n%s", err, out)
	}

	tm := &tmuxDriver{t: t, bin: tmuxBin, socket: filepath.Join(root, "tmux.sock")}
	defer tm.killServer()

	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Control: claude directly, env ANTHROPIC_API_KEY placeholder → the
	// approval dialog MUST appear (validates the marker + harness).
	t.Run("control_api_key_placeholder_shows_dialog", func(t *testing.T) {
		home := seedClaudeHome(t, filepath.Join(root, "home-control"), workDir)
		script := writeLaunchScript(t, filepath.Join(root, "run-control.sh"), workDir, []string{
			"HOME=" + home,
			"ANTHROPIC_API_KEY=ccwrap-placeholder-e2e-control-0000",
			"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
			// Dead-end proxy: nothing leaves the machine; the dialog is
			// rendered before any request is needed.
			"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9",
			"TZ=America/Los_Angeles",
			"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		}, claudeBin)
		tm.newSession("control", script)
		defer tm.killSession("control")
		screen := tm.waitFor("control", regexp.MustCompile(regexp.QuoteMeta(dialogMarker)), 90*time.Second)
		if screen == "" {
			t.Fatalf("approval dialog never appeared for env ANTHROPIC_API_KEY; harness cannot detect the dialog (marker %q stale?)", dialogMarker)
		}
	})

	// Subject: ccwrap with an x-api-key profile → no dialog, and the child
	// authenticates via the bearer placeholder (positive assertion through
	// /status, not just absence of the marker).
	t.Run("ccwrap_x_api_key_profile_no_dialog", func(t *testing.T) {
		home := seedClaudeHome(t, filepath.Join(root, "home-subject"), workDir)
		stateDir := filepath.Join(root, "state")
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		profiles := `{
  "default": "gwtest",
  "profiles": {
    "gwtest": {
      "provider": "e2e-gateway",
      "base_url": "https://gateway.example/v1",
      "auth": {"mode": "ccwrap_x_api_key", "key": "sk-e2e-not-a-real-key"},
      "egress": {"mode": "inherit"}
    }
  }
}`
		if err := os.WriteFile(filepath.Join(stateDir, "profiles.json"), []byte(profiles), 0o600); err != nil {
			t.Fatal(err)
		}
		script := writeLaunchScript(t, filepath.Join(root, "run-subject.sh"), workDir, []string{
			"HOME=" + home,
			"CCWRAP_STATE_DIR=" + stateDir,
			// A non-China TZ skips ccwrap's interactive timezone prompt.
			"TZ=America/Los_Angeles",
			"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		}, ccwrapBin)
		tm.newSession("subject", script)
		defer tm.killSession("subject")

		ready := tm.waitFor("subject", regexp.MustCompile(`\? for shortcuts`), 90*time.Second)
		if ready == "" {
			t.Fatalf("claude (under ccwrap) never reached the ready prompt; last screen:\n%s", tm.capture("subject"))
		}
		if strings.Contains(ready, dialogMarker) {
			t.Fatalf("approval dialog appeared for the bearer placeholder:\n%s", ready)
		}

		tm.sendText("subject", "/status")
		time.Sleep(1 * time.Second) // let the slash-command palette register before Enter
		tm.sendEnter("subject")
		status := tm.waitFor("subject", regexp.MustCompile(`Auth token:\s+ANTHROPIC_AUTH_TOKEN`), 30*time.Second)
		if status == "" {
			t.Fatalf("/status never showed Auth token: ANTHROPIC_AUTH_TOKEN; last screen:\n%s", tm.capture("subject"))
		}
		if strings.Contains(status, dialogMarker) {
			t.Fatalf("approval dialog appeared late:\n%s", status)
		}
	})
}

// seedClaudeHome creates an isolated HOME whose .claude.json skips
// onboarding and pre-trusts workDir, so the startup dialog queue (where the
// approval dialog lives) is reached deterministically.
func seedClaudeHome(t *testing.T, home, workDir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"hasCompletedOnboarding": true, "theme": "dark", "projects": {%q: {"hasTrustDialogAccepted": true}}}`, workDir)
	for _, p := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", ".claude.json")} {
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// writeLaunchScript wraps the target binary so tmux runs it with exactly the
// given env pairs layered over the test process env (PATH etc. inherited).
func writeLaunchScript(t *testing.T, path, workDir string, env []string, bin string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "cd %q || exit 1\n", workDir)
	for _, kv := range env {
		fmt.Fprintf(&b, "export %s\n", kv)
	}
	fmt.Fprintf(&b, "exec %q\n", bin)
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// tmuxDriver runs panes on a private tmux socket so the test never touches
// the user's tmux server.
type tmuxDriver struct {
	t      *testing.T
	bin    string
	socket string
}

func (m *tmuxDriver) run(args ...string) (string, error) {
	out, err := exec.Command(m.bin, append([]string{"-S", m.socket}, args...)...).CombinedOutput()
	return string(out), err
}

func (m *tmuxDriver) newSession(name, script string) {
	m.t.Helper()
	if out, err := m.run("new-session", "-d", "-s", name, "-x", "130", "-y", "40", script); err != nil {
		m.t.Fatalf("tmux new-session %s: %v\n%s", name, err, out)
	}
}

func (m *tmuxDriver) killSession(name string) { _, _ = m.run("kill-session", "-t", name) }
func (m *tmuxDriver) killServer()             { _, _ = m.run("kill-server") }

func (m *tmuxDriver) capture(name string) string {
	out, err := m.run("capture-pane", "-t", name, "-p")
	if err != nil {
		return ""
	}
	return out
}

// waitFor polls the pane until re matches the captured screen, returning the
// matching screen, or "" on timeout (the pane may also have exited).
func (m *tmuxDriver) waitFor(name string, re *regexp.Regexp, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if screen := m.capture(name); re.MatchString(screen) {
			return screen
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

func (m *tmuxDriver) sendText(name, text string) {
	m.t.Helper()
	if out, err := m.run("send-keys", "-t", name, text); err != nil {
		m.t.Fatalf("tmux send-keys: %v\n%s", err, out)
	}
}

func (m *tmuxDriver) sendEnter(name string) {
	m.t.Helper()
	if out, err := m.run("send-keys", "-t", name, "Enter"); err != nil {
		m.t.Fatalf("tmux send-keys Enter: %v\n%s", err, out)
	}
}
