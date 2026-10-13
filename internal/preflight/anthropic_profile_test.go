package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

// writeAnthropicProfile writes configs/<name>.json under dir and, when creds
// is non-empty, the default credentials/<name>.json next to it.
func writeAnthropicProfile(t *testing.T, dir, name, config, creds string) {
	t.Helper()
	for _, f := range []struct{ path, data string }{
		{filepath.Join(dir, "configs", name+".json"), config},
		{filepath.Join(dir, "credentials", name+".json"), creds},
	} {
		if f.data == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, []byte(f.data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	userOAuthGateway = `{"authentication":{"type":"user_oauth"},"base_url":" https://gw.example.com/ "}`
	someToken        = `{"access_token":"tok"}`
)

func TestActiveAnthropicProfileSelection(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".config", "anthropic")
	writeAnthropicProfile(t, store, "default", userOAuthGateway, someToken)
	writeAnthropicProfile(t, store, "work", `{"authentication":{"type":"oidc_federation","federation_rule_id":"r"},"base_url":"https://work.example.com"}`, "")
	writeAnthropicProfile(t, store, "nocreds", `{"authentication":{"type":"user_oauth"}}`, "")
	writeAnthropicProfile(t, store, "blankcreds", `{"authentication":{"type":"user_oauth"}}`, " \n")
	writeAnthropicProfile(t, store, "apikey", `{"authentication":{"type":"api_key"}}`, someToken)
	writeAnthropicProfile(t, store, "broken", `{"authentication":`, someToken)
	cwd := t.TempDir()
	writeAnthropicProfile(t, store, "custom", `{"authentication":{"type":"user_oauth","credentials_path":"tok.json"}}`, "")
	if err := os.WriteFile(filepath.Join(cwd, "tok.json"), []byte(someToken), 0o600); err != nil {
		t.Fatal(err)
	}
	xdg := t.TempDir()
	writeAnthropicProfile(t, filepath.Join(xdg, "anthropic"), "default", `{"authentication":{"type":"oidc_federation"},"base_url":"https://xdg.example.com"}`, "")
	cfgDir := t.TempDir()
	writeAnthropicProfile(t, cfgDir, "default", `{"authentication":{"type":"oidc_federation"},"base_url":"https://cfg.example.com"}`, "")

	cases := []struct {
		name       string
		env        map[string]string
		wantName   string // "" = no profile
		selectedBy string
		baseURL    string
	}{
		{"implicit default", map[string]string{"HOME": home}, "default", "default", "https://gw.example.com/"},
		{"explicit", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": " work "}, "work", "ANTHROPIC_PROFILE", "https://work.example.com"},
		{"explicit unusable does not fall back", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "nocreds"}, "", "", ""},
		{"explicit missing does not fall back", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "nope"}, "", "", ""},
		{"blank credentials", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "blankcreds"}, "", "", ""},
		{"other auth type", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "apikey"}, "", "", ""},
		{"malformed config", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "broken"}, "", "", ""},
		{"relative credentials_path", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "custom"}, "custom", "ANTHROPIC_PROFILE", ""},
		{"env federation outranks the implicit profile", map[string]string{"HOME": home, "ANTHROPIC_FEDERATION_RULE_ID": "r", "ANTHROPIC_ORGANIZATION_ID": "o"}, "", "", ""},
		{"explicit outranks env federation", map[string]string{"HOME": home, "ANTHROPIC_PROFILE": "work", "ANTHROPIC_FEDERATION_RULE_ID": "r", "ANTHROPIC_ORGANIZATION_ID": "o"}, "work", "ANTHROPIC_PROFILE", "https://work.example.com"},
		{"half federation env is ignored", map[string]string{"HOME": home, "ANTHROPIC_FEDERATION_RULE_ID": "r"}, "default", "default", "https://gw.example.com/"},
		{"XDG_CONFIG_HOME before HOME", map[string]string{"HOME": home, "XDG_CONFIG_HOME": xdg}, "default", "default", "https://xdg.example.com"},
		{"ANTHROPIC_CONFIG_DIR first", map[string]string{"HOME": home, "XDG_CONFIG_HOME": xdg, "ANTHROPIC_CONFIG_DIR": cfgDir}, "default", "default", "https://cfg.example.com"},
		{"no config dir", map[string]string{}, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ActiveAnthropicProfile(tc.env, cwd)
			if tc.wantName == "" {
				if p != nil {
					t.Fatalf("got profile %+v, want none", *p)
				}
				return
			}
			if p == nil {
				t.Fatalf("got no profile, want %q", tc.wantName)
			}
			if p.Name != tc.wantName || p.SelectedBy != tc.selectedBy || p.BaseURL != tc.baseURL {
				t.Fatalf("got %+v, want name=%q selectedBy=%q baseURL=%q", *p, tc.wantName, tc.selectedBy, tc.baseURL)
			}
		})
	}

	// active_config names the implicit profile.
	if err := os.WriteFile(filepath.Join(store, "active_config"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := ActiveAnthropicProfile(map[string]string{"HOME": home}, cwd); p == nil || p.Name != "work" || p.SelectedBy != "active_config" {
		t.Fatalf("active_config: got %+v, want work selected by active_config", p)
	}
}

func TestAnthropicProfileLeavesAnthropicAPI(t *testing.T) {
	cases := map[string]bool{
		"":                                  false,
		"https://api.anthropic.com":         false,
		"https://api.anthropic.com:443/":    false,
		"https://API.Anthropic.com./v1":     false,
		"http://api.anthropic.com":          false,
		"https://api-staging.anthropic.com": false,
		"https://gw.example.com":            true,
		"http://127.0.0.1:8080":             true,
		"https://console.anthropic.com":     true,
		"api.anthropic.com":                 true, // no scheme: no host
		"://bad":                            true,
	}
	for base, want := range cases {
		if got := (&AnthropicProfile{BaseURL: base}).LeavesAnthropicAPI(); got != want {
			t.Errorf("LeavesAnthropicAPI(%q) = %v, want %v", base, got, want)
		}
	}
	var none *AnthropicProfile
	if none.LeavesAnthropicAPI() {
		t.Error("a nil profile cannot leave the Anthropic API")
	}
}

// The launch check fires only where Claude Code keeps its own credentials;
// with ccwrap's placeholder in the child, Claude Code skips profiles.
func TestLaunchBypassAnthropicProfile(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".config", "anthropic")
	writeAnthropicProfile(t, store, "default", `{"authentication":{"type":"user_oauth"},"base_url":"https://user:secret@gw.example.com/path?key=k"}`, someToken)
	opts := Options{ParentEnv: []string{"HOME=" + home}, WorkingDirectory: t.TempDir()}

	firstParty := &Result{RouteClass: model.RouteClassFirstParty, AuthBootstrap: model.AuthBootstrapNotNeeded}
	err := LaunchBypass(firstParty, opts, nil)
	if err == nil {
		t.Fatal("first-party passthrough with a gateway profile must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{`"default"`, filepath.Join(store, "configs", "default.json"), "https://gw.example.com", "ANTHROPIC_PROFILE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	for _, leak := range []string{"secret", "key=k", "/path"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q leaks %q from base_url", msg, leak)
		}
	}
	if strings.Contains(msg, "ccwrap-owned auth") {
		t.Errorf("first-party error should not suggest route auth: %q", msg)
	}

	missingAuth := &Result{RouteClass: model.RouteClassThirdPartyHidden, AuthBootstrap: model.AuthBootstrapMissing}
	if err := LaunchBypass(missingAuth, opts, nil); err == nil || !strings.Contains(err.Error(), "ccwrap-owned auth") {
		t.Fatalf("third-party route without auth: got %v, want a rejection suggesting ccwrap-owned auth", err)
	}

	placeholder := &Result{RouteClass: model.RouteClassThirdPartyHidden, AuthBootstrap: model.AuthBootstrapPlaceholderActive}
	if err := LaunchBypass(placeholder, opts, nil); err != nil {
		t.Fatalf("placeholder posture: Claude Code skips profiles, got %v", err)
	}

	writeAnthropicProfile(t, store, "default", `{"authentication":{"type":"user_oauth"},"base_url":"https://api.anthropic.com"}`, "")
	if err := LaunchBypass(firstParty, opts, nil); err != nil {
		t.Fatalf("a profile on api.anthropic.com stays on ccwrap's route, got %v", err)
	}
}

// Claude settings env can select the profile too: trusted sources count,
// project settings (applied only after workspace trust) do not.
func TestLaunchAnthropicProfileReadsTrustedSettingsEnv(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".config", "anthropic")
	writeAnthropicProfile(t, store, "default", `{"authentication":{"type":"user_oauth"}}`, someToken)
	writeAnthropicProfile(t, store, "gw", userOAuthGateway, someToken)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_PROFILE":"gw"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{ParentEnv: []string{"HOME=" + home}, WorkingDirectory: cwd}
	pick := func(args ...string) string {
		t.Helper()
		inspect, err := settings.InspectLaunch(cwd, args)
		if err != nil {
			t.Fatal(err)
		}
		p := LaunchAnthropicProfile(opts, inspect)
		if p == nil {
			t.Fatal("no profile selected")
		}
		return p.Name
	}
	if got := pick(); got != "default" {
		t.Fatalf("project settings env must not select the profile: got %q", got)
	}
	if got := pick("--settings", `{"env":{"ANTHROPIC_PROFILE":"gw"}}`); got != "gw" {
		t.Fatalf("--settings env: got %q, want gw", got)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"ANTHROPIC_PROFILE":"gw"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pick(); got != "gw" {
		t.Fatalf("user settings env: got %q, want gw", got)
	}
}
