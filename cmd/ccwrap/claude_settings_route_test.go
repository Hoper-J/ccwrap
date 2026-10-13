package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// A project's Claude settings can point Claude Code elsewhere on their own; on
// the official route (Claude Code's own credentials) the launch is refused.
func TestComposeLaunchRejectsClaudeSettingsOffRoute(t *testing.T) {
	isolateLaunchEnv(t)
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://gw.example.com"}}`)
	_, _, _, err := composeLaunch(preflight.Options{ParentEnv: os.Environ(), WorkingDirectory: cwd})
	if err == nil || !strings.Contains(err.Error(), "project settings") || !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
		t.Fatalf("official route: got %v, want the Claude settings rejection", err)
	}
	// Overriding it in the project's own local file, the way Claude Code
	// merges settings, clears it.
	writeTestFile(t, filepath.Join(cwd, ".claude", "settings.local.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}}`)
	if _, _, _, err := composeLaunch(preflight.Options{ParentEnv: os.Environ(), WorkingDirectory: cwd}); err != nil {
		t.Fatalf("overridden in settings.local.json: %v", err)
	}
	if err := os.Remove(filepath.Join(cwd, ".claude", "settings.local.json")); err != nil {
		t.Fatal(err)
	}
	// With ccwrap-owned auth the child carries the placeholder, and Claude Code
	// drops provider keys from its settings.
	_, pre, _, err := composeLaunch(preflight.Options{
		Upstream:         "https://gw2.example/v1",
		ParentEnv:        append(os.Environ(), "CCWRAP_UPSTREAM_AUTH_TOKEN=tok"),
		WorkingDirectory: cwd,
	})
	if err != nil {
		t.Fatalf("placeholder posture: %v", err)
	}
	if pre.AuthBootstrap != model.AuthBootstrapPlaceholderActive {
		t.Fatalf("AuthBootstrap = %v, want the placeholder", pre.AuthBootstrap)
	}
}

func TestDoctorReportsClaudeSettingsRoute(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	_, claudeDir := isolateLaunchEnv(t)
	writeTestFile(t, filepath.Join(claudeDir, "settings.json"), `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)
	checks := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))
	if c := checks["claude_settings_route"]; c.Status != "fail" || !strings.Contains(c.Detail, "CLAUDE_CODE_USE_BEDROCK") {
		t.Fatalf("claude_settings_route = %#v, want fail naming the switch", c)
	}
	if c := checks["launch_contract"]; c.Status != "fail" || !strings.Contains(c.Detail, "CLAUDE_CODE_USE_BEDROCK") {
		t.Fatalf("launch_contract = %#v, want the Claude settings rejection", c)
	}

	t.Setenv("CCWRAP_UPSTREAM", "https://gw.example/v1")
	t.Setenv("CCWRAP_UPSTREAM_AUTH_TOKEN", "tok")
	checks = doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))
	if c := checks["claude_settings_route"]; c.Status != "pass" || !strings.Contains(c.Summary, "ignores") {
		t.Fatalf("placeholder posture: claude_settings_route = %#v, want pass (ignored)", c)
	}
	if c := checks["launch_contract"]; c.Status != "pass" {
		t.Fatalf("placeholder posture: launch_contract = %#v, want pass", c)
	}
}

// Every bypass is reported in one go, so fixing one does not just reveal the
// next.
func TestComposeLaunchReportsEveryBypass(t *testing.T) {
	isolateAnthropicProfile(t, "https://gw.corp.example")
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)
	_, _, _, err := composeLaunch(preflight.Options{ParentEnv: os.Environ(), WorkingDirectory: cwd})
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_USE_BEDROCK") || !strings.Contains(err.Error(), `Anthropic profile "default"`) {
		t.Fatalf("want both the settings switch and the profile in one error, got %v", err)
	}
}
