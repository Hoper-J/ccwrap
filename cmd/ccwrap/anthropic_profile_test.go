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

// isolateLaunchEnv points HOME, Claude's config dir and the managed settings
// dir at temp dirs and blanks every other input to the launch posture and to
// Claude Code's profile selection. It returns HOME and Claude's config dir.
func isolateLaunchEnv(t *testing.T) (home, claudeDir string) {
	t.Helper()
	home, claudeDir = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{
		"XDG_CONFIG_HOME", "ANTHROPIC_CONFIG_DIR", "ANTHROPIC_PROFILE",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"CCWRAP_UPSTREAM", "CCWRAP_UPSTREAM_API_KEY", "CCWRAP_UPSTREAM_AUTH_TOKEN",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", t.TempDir())
	return home, claudeDir
}

// writeTestFile writes data to path, creating its directory.
func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// isolateAnthropicProfile isolates the launch env (isolateLaunchEnv) and puts
// an implicit Anthropic SDK profile (user_oauth, with credentials) whose
// base_url is baseURL in the new HOME.
func isolateAnthropicProfile(t *testing.T, baseURL string) {
	t.Helper()
	home, _ := isolateLaunchEnv(t)
	store := filepath.Join(home, ".config", "anthropic")
	writeTestFile(t, filepath.Join(store, "configs", "default.json"), `{"authentication":{"type":"user_oauth"},"base_url":"`+baseURL+`"}`)
	writeTestFile(t, filepath.Join(store, "credentials", "default.json"), `{"access_token":"tok"}`)
}

// Claude Code signs in with an implicit Anthropic profile on its own and then
// sends its requests to the profile's base_url, past ccwrap. The launch is
// refused wherever the child keeps its own credentials.
func TestComposeLaunchRejectsAnthropicProfileOffRoute(t *testing.T) {
	isolateAnthropicProfile(t, "https://gw.example.com")
	cwd := t.TempDir()
	_, _, _, err := composeLaunch(preflight.Options{ParentEnv: os.Environ(), WorkingDirectory: cwd})
	if err == nil || !strings.Contains(err.Error(), `Anthropic profile "default"`) {
		t.Fatalf("first-party passthrough: got %v, want the Anthropic profile rejection", err)
	}
	// ccwrap-owned auth on a third-party route puts the placeholder in the
	// child, and Claude Code then leaves profiles alone.
	_, pre, _, err := composeLaunch(preflight.Options{
		Upstream:         "https://gw.example/v1",
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

func TestDoctorReportsAnthropicProfile(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	isolateAnthropicProfile(t, "https://gw.example.com")
	checks := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))
	if c := checks["anthropic_profile"]; c.Status != "fail" || !strings.Contains(c.Summary, "https://gw.example.com") {
		t.Fatalf("anthropic_profile = %#v, want fail naming the base_url", c)
	}
	if c := checks["launch_contract"]; c.Status != "fail" || !strings.Contains(c.Detail, `Anthropic profile "default"`) {
		t.Fatalf("launch_contract = %#v, want the Anthropic profile rejection", c)
	}

	t.Setenv("CCWRAP_UPSTREAM", "https://gw.example/v1")
	t.Setenv("CCWRAP_UPSTREAM_AUTH_TOKEN", "tok")
	checks = doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))
	if c := checks["anthropic_profile"]; c.Status != "pass" || !strings.Contains(c.Summary, "ignores") {
		t.Fatalf("placeholder posture: anthropic_profile = %#v, want pass (ignored)", c)
	}
	if c := checks["launch_contract"]; c.Status != "pass" {
		t.Fatalf("placeholder posture: launch_contract = %#v, want pass", c)
	}
}
