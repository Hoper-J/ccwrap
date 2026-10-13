package preflight

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

func TestMovesClaudeOffAnthropic(t *testing.T) {
	cases := []struct {
		key, value string
		want       bool
	}{
		{"ANTHROPIC_BASE_URL", "https://gw.example.com", true},
		{"ANTHROPIC_BASE_URL", "http://127.0.0.1:8080", true},
		{"ANTHROPIC_BASE_URL", "https://api.anthropic.com", false},
		{"ANTHROPIC_BASE_URL", " ", false},
		{"CLAUDE_CODE_USE_BEDROCK", "1", true},
		{"CLAUDE_CODE_USE_VERTEX", "true", true},
		{"CLAUDE_CODE_USE_FOUNDRY", "enabled", true},
		{"CLAUDE_CODE_USE_BEDROCK", "0", false},
		{"CLAUDE_CODE_USE_BEDROCK", "OFF", false},
		{"CLAUDE_CODE_USE_BEDROCK", "", false},
	}
	for _, tc := range cases {
		if got := movesClaudeOffAnthropic(tc.key, tc.value); got != tc.want {
			t.Errorf("movesClaudeOffAnthropic(%q, %q) = %v, want %v", tc.key, tc.value, got, tc.want)
		}
	}
}

// writeClaudeSettings writes a settings file with the given env block.
func writeClaudeSettings(t *testing.T, path, env string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":`+env+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// claudeSettingsFixture isolates Claude's settings and returns its config dir,
// a project dir and an inspector for launches there.
func claudeSettingsFixture(t *testing.T) (configDir, cwd string, inspect func(args ...string) *settings.InspectionResult) {
	t.Helper()
	configDir, cwd = t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", t.TempDir())
	return configDir, cwd, func(args ...string) *settings.InspectionResult {
		t.Helper()
		in, err := settings.InspectLaunch(cwd, args)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
}

// Claude Code takes routing env from its settings files merged by precedence,
// so only the value it ends up with counts.
func TestClaudeSettingsBypasses(t *testing.T) {
	configDir, cwd, inspect := claudeSettingsFixture(t)
	userFile := filepath.Join(configDir, "settings.json")
	projectFile := filepath.Join(cwd, ".claude", "settings.json")
	localFile := filepath.Join(cwd, ".claude", "settings.local.json")

	if got := ClaudeSettingsBypasses(inspect()); len(got) != 0 {
		t.Fatalf("no settings: %+v", got)
	}
	// --settings is replaced by ccwrap's generated settings, which strip these.
	if got := ClaudeSettingsBypasses(inspect("--settings", `{"env":{"ANTHROPIC_BASE_URL":"https://gw.example.com"}}`)); len(got) != 0 {
		t.Fatalf("--settings only: %+v", got)
	}

	writeClaudeSettings(t, projectFile, `{"ANTHROPIC_BASE_URL":"https://user:secret@gw.example.com/v1?key=k"}`)
	got := ClaudeSettingsBypasses(inspect())
	if len(got) != 1 || got[0].Source != "project settings" || !strings.Contains(got[0].Where, projectFile) || got[0].What != "ANTHROPIC_BASE_URL (https://gw.example.com)" || !got[0].Endpoint {
		t.Fatalf("project gateway: %+v", got)
	}

	// The project's own local file overrides it back to the Anthropic API.
	writeClaudeSettings(t, localFile, `{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}`)
	if got := ClaudeSettingsBypasses(inspect()); len(got) != 0 {
		t.Fatalf("overridden in settings.local.json: %+v", got)
	}

	// A provider switch in user settings, turned off by a higher file.
	writeClaudeSettings(t, userFile, `{"CLAUDE_CODE_USE_BEDROCK":"1"}`)
	got = ClaudeSettingsBypasses(inspect())
	if len(got) != 1 || got[0].Source != "user settings" || got[0].What != "CLAUDE_CODE_USE_BEDROCK" || got[0].Endpoint {
		t.Fatalf("user switch: %+v", got)
	}
	writeClaudeSettings(t, localFile, `{"ANTHROPIC_BASE_URL":"https://api.anthropic.com","CLAUDE_CODE_USE_BEDROCK":"0"}`)
	if got := ClaudeSettingsBypasses(inspect()); len(got) != 0 {
		t.Fatalf("switch turned off in settings.local.json: %+v", got)
	}
}

// LaunchBypass reports every bypass at once, and only where Claude Code keeps
// its own credentials.
func TestLaunchBypass(t *testing.T) {
	_, cwd, inspect := claudeSettingsFixture(t)
	home := t.TempDir()
	opts := Options{ParentEnv: []string{"HOME=" + home}, WorkingDirectory: cwd}
	firstParty := &Result{RouteClass: model.RouteClassFirstParty, AuthBootstrap: model.AuthBootstrapNotNeeded}
	missingAuth := &Result{RouteClass: model.RouteClassThirdPartyHidden, AuthBootstrap: model.AuthBootstrapMissing}
	placeholder := &Result{RouteClass: model.RouteClassThirdPartyHidden, AuthBootstrap: model.AuthBootstrapPlaceholderActive}

	if err := LaunchBypass(firstParty, opts, inspect()); err != nil {
		t.Fatalf("clean launch: %v", err)
	}
	writeAnthropicProfile(t, filepath.Join(home, ".config", "anthropic"), "default", userOAuthGateway, someToken)
	writeClaudeSettings(t, filepath.Join(cwd, ".claude", "settings.json"), `{"ANTHROPIC_BASE_URL":"https://gw.project.example","CLAUDE_CODE_USE_VERTEX":"1"}`)

	err := LaunchBypass(firstParty, opts, inspect())
	var rb *RouteBypassError
	if !errors.As(err, &rb) || len(rb.Bypasses) != 3 {
		t.Fatalf("want all three bypasses in one error, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"ANTHROPIC_BASE_URL (https://gw.project.example)", "CLAUDE_CODE_USE_VERTEX",
		`Anthropic profile "default"`, "base_url https://gw.example.com",
		"settings.local.json", "ANTHROPIC_PROFILE", "set it up as a ccwrap profile",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "ccwrap-owned auth") {
		t.Errorf("first-party error should not suggest route auth:\n%s", msg)
	}

	err = LaunchBypass(missingAuth, opts, inspect())
	if err == nil || !strings.Contains(err.Error(), "ccwrap-owned auth") || strings.Contains(err.Error(), "set it up as a ccwrap profile") {
		t.Fatalf("third-party route without auth: got %v", err)
	}
	if err := LaunchBypass(placeholder, opts, inspect()); err != nil {
		t.Fatalf("placeholder posture: Claude Code ignores all of these, got %v", err)
	}
}
