package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

func TestResolveAuthConflict(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY":       "abc",
		"CLAUDE_CODE_OAUTH_TOKEN": "def",
	}
	_, _, _, err := ResolveAuth(env)
	if err == nil {
		t.Fatal("expected conflicting auth sources error")
	}
	if !strings.Contains(err.Error(), "conflicting auth sources") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildChildEnvScrubsSensitiveVars(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_BASE_URL=https://relay.example",
		"ANTHROPIC_API_KEY=sekret",
		"CLAUDE_CODE_USE_VERTEX=1",
		"VERTEX_REGION_CLAUDE_3_5_HAIKU=europe-west1",
		"HTTP_PROXY=http://corp-proxy:8080",
		"HTTPS_PROXY=http://corp-proxy:8443",
		"ALL_PROXY=socks5://corp-proxy:1080",
		"NO_PROXY=example.com",
		"SSL_CERT_FILE=/etc/custom.pem",
		"CLAUDE_CODE_SUBAGENT_MODEL=parent-subagent",
		"CCWRAP_MODEL_ALIASES_JSON={\"claude-sonnet-4-6\":\"gateway/sonnet\"}",
	}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", nil, ChildAuthBootstrap{}, ""), "\n")
	for _, forbidden := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "CLAUDE_CODE_USE_VERTEX=", "VERTEX_REGION_CLAUDE_3_5_HAIKU=", "HTTP_PROXY=http://corp-proxy:8080", "HTTPS_PROXY=http://corp-proxy:8443", "ALL_PROXY=", "SSL_CERT_FILE=/etc/custom.pem", "CCWRAP_MODEL_ALIASES_JSON="} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("child env should scrub %s; got:\n%s", forbidden, env)
		}
	}
	for _, required := range []string{
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
		"HTTPS_PROXY=http://127.0.0.1:4444",
		"http_proxy=http://127.0.0.1:4444",
		"NODE_EXTRA_CA_CERTS=/tmp/ca-cert.pem",
		"SSL_CERT_FILE=/tmp/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE=/tmp/ca-bundle.pem",
		"CURL_CA_BUNDLE=/tmp/ca-bundle.pem",
		"GIT_SSL_CAINFO=/tmp/ca-bundle.pem",
		"CLAUDE_CODE_SUBAGENT_MODEL=parent-subagent",
	} {
		if !strings.Contains(env, required) {
			t.Fatalf("child env missing %s; got:\n%s", required, env)
		}
	}
	if !strings.Contains(env, "NO_PROXY=127.0.0.1,::1,localhost") || strings.Contains(env, "example.com") {
		t.Fatalf("expected loopback-only NO_PROXY; got:\n%s", env)
	}
}

func TestBuildChildEnvAppliesTrustedModelEnvOverrides(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"CLAUDE_CODE_SUBAGENT_MODEL=parent-subagent",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=parent-sonnet",
	}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", map[string]string{
		"CLAUDE_CODE_SUBAGENT_MODEL":     "settings-subagent",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "settings-sonnet",
	}, ChildAuthBootstrap{}, ""), "\n")
	for _, required := range []string{
		"CLAUDE_CODE_SUBAGENT_MODEL=settings-subagent",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=settings-sonnet",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
	} {
		if !strings.Contains(env, required) {
			t.Fatalf("child env missing %s; got:\n%s", required, env)
		}
	}
	if strings.Contains(env, "parent-subagent") || strings.Contains(env, "parent-sonnet") {
		t.Fatalf("trusted settings model env should override parent model env; got:\n%s", env)
	}
}

func TestBuildChildEnvInjectsTimezone(t *testing.T) {
	// A non-empty tz is injected and overrides any inherited TZ.
	parent := []string{"PATH=/usr/bin", "TZ=Asia/Shanghai"}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", nil, ChildAuthBootstrap{}, "America/Los_Angeles"), "\n")
	if !strings.Contains(env, "TZ=America/Los_Angeles") {
		t.Fatalf("expected injected TZ=America/Los_Angeles; got:\n%s", env)
	}
	if strings.Contains(env, "TZ=Asia/Shanghai") {
		t.Fatalf("injected TZ should override the inherited one; got:\n%s", env)
	}
}

func TestBuildChildEnvEmptyTimezonePreservesInheritedTZ(t *testing.T) {
	// Empty tz must not add a TZ and must leave the inherited one untouched.
	parent := []string{"PATH=/usr/bin", "TZ=Asia/Shanghai"}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", nil, ChildAuthBootstrap{}, ""), "\n")
	if !strings.Contains(env, "TZ=Asia/Shanghai") {
		t.Fatalf("empty tz should preserve inherited TZ; got:\n%s", env)
	}
}

func TestBuildChildEnvEmptyTimezoneNoTZWhenAbsent(t *testing.T) {
	// Empty tz with no inherited TZ adds nothing.
	parent := []string{"PATH=/usr/bin"}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", nil, ChildAuthBootstrap{}, ""), "\n")
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, "TZ=") {
			t.Fatalf("empty tz with no inherited TZ should add no TZ; got line %q", line)
		}
	}
}

func TestEffectiveEgressEnvReadsClaudeSettingsProxy(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(tmp, "project")
	if err := os.MkdirAll(filepath.Join(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(`{"env":{"HTTPS_PROXY":"http://global-proxy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://user-proxy:8443"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://project-proxy:9000","NO_PROXY":"project.internal"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	merged, notes, usedSettings, err := EffectiveEgressEnv([]string{"HTTPS_PROXY=http://parent-proxy:8080", "NO_PROXY=parent.local"}, projectDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !usedSettings {
		t.Fatal("expected Claude settings to contribute to effective egress env")
	}
	if got := merged["HTTPS_PROXY"]; got != "http://project-proxy:9000" {
		t.Fatalf("expected Claude settings HTTPS_PROXY to win, got %q", got)
	}
	if got := merged["NO_PROXY"]; got != "project.internal" {
		t.Fatalf("expected Claude settings NO_PROXY to win, got %q", got)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "projectSettings") {
		t.Fatalf("expected notes to mention contributing Claude settings sources, got %#v", notes)
	}
}

func TestResolveEgressAutoMarksClaudeSettingsSource(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://settings-proxy:8443"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	cfg, notes, err := ResolveEgress("auto", []string{"PATH=/usr/bin"}, tmp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != "claude_settings" {
		t.Fatalf("expected claude_settings source, got %#v", cfg)
	}
	if cfg.HTTPSProxy != "http://settings-proxy:8443" {
		t.Fatalf("expected settings-derived HTTPS proxy, got %#v", cfg)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "userSettings") {
		t.Fatalf("expected notes to mention userSettings, got %#v", notes)
	}
}

func TestEffectiveEgressEnvIgnoresPolicyManagedProxyAndReportsIt(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://policy-proxy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)
	merged, notes, usedSettings, err := EffectiveEgressEnv([]string{"PATH=/usr/bin"}, tmp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if usedSettings {
		t.Fatal("expected policy-managed settings to be ignored for effective egress env")
	}
	if got := merged["HTTPS_PROXY"]; got != "" {
		t.Fatalf("expected policy-managed proxy to be ignored for egress auto, got %q", got)
	}
	joined := strings.Join(notes, " ; ")
	if !strings.Contains(joined, "Ignored detectable local/cache policy-managed network/trust env") {
		t.Fatalf("expected notes to explain ignored policy env, got %#v", notes)
	}
	if strings.Contains(joined, "Claude settings proxy sources: policySettings") {
		t.Fatalf("expected policySettings not to be reported as an active egress source, got %#v", notes)
	}
}

func TestRunAllowsUserSettingsNetworkEnv(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://corp-proxy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err != nil {
		t.Fatalf("user settings network env should not fail preflight: %v", err)
	}
	if len(res.OverriddenNetworkEnv) != 1 {
		t.Fatalf("expected one overridden network env hit, got %#v", res.OverriddenNetworkEnv)
	}
	if len(res.PolicyNetworkEnv) != 0 {
		t.Fatalf("expected no policy network env hits, got %#v", res.PolicyNetworkEnv)
	}
}

func TestRunRejectsPolicySettingsNetworkEnv(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://policy-proxy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)
	_, err := Run(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err == nil || !strings.Contains(err.Error(), "local/cache policy-managed network/trust env") {
		t.Fatalf("expected policy network/trust env error, got %v", err)
	}
	if !strings.Contains(err.Error(), "remote managed settings, MDM, and HKCU") {
		t.Fatalf("expected policy error to mention unsupported undetectable sources, got %v", err)
	}
}

func TestRunAllowsUserFlagSettingsNetworkEnv(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		ParentEnv:        []string{"PATH=/usr/bin"},
		WorkingDirectory: tmp,
		ChildArgs:        []string{"--settings", `{"env":{"HTTPS_PROXY":"http://corp:8080"},"theme":"dark"}`},
	})
	if err != nil {
		t.Fatalf("user-provided --settings network env should be overridden, not rejected: %v", err)
	}
	if len(res.OverriddenNetworkEnv) != 1 {
		t.Fatalf("expected one overridden network env hit, got %#v", res.OverriddenNetworkEnv)
	}
	if len(res.PolicyNetworkEnv) != 0 {
		t.Fatalf("expected no policy network env hits, got %#v", res.PolicyNetworkEnv)
	}
	if res.ParsedFlagSettings == nil || res.ParsedFlagSettings.Settings == nil {
		t.Fatalf("expected parsed flag settings to be retained, got %#v", res.ParsedFlagSettings)
	}
}

func TestRunAllowsAPIKeyHelper(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"apiKeyHelper":{"command":"helper"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err != nil {
		t.Fatalf("apiKeyHelper should not fail preflight: %v", err)
	}
	if len(res.APIKeyHelperHits) != 1 {
		t.Fatalf("expected apiKeyHelper hit to be recorded, got %#v", res.APIKeyHelperHits)
	}
}

func TestRunWithInspectionUsesSnapshot(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(configDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"HTTPS_PROXY":"http://snapshot-proxy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	inspect, err := settings.InspectLaunch(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	res, err := RunWithInspection(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp}, inspect)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Egress.HTTPSProxy; got != "http://snapshot-proxy:8080" {
		t.Fatalf("expected snapshot-derived egress proxy, got %#v", res.Egress)
	}
}

func TestRunCapturesClaudeSettingsProviderBaseURL(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://relay.example/base","ANTHROPIC_API_KEY":"settings-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.APIBaseURL.String(); got != "https://relay.example/base" {
		t.Fatalf("expected settings provider base URL to configure CCWRAP route, got %q", got)
	}
	if res.RouteSource != model.RouteSourceClaudeSettings || res.RouteConfigSource != "userSettings" {
		t.Fatalf("unexpected route source: %s/%s", res.RouteSource, res.RouteConfigSource)
	}
}

func TestRunCapturesFlagSettingsOAuthToken(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		ParentEnv:        []string{"PATH=/usr/bin"},
		WorkingDirectory: tmp,
		ChildArgs:        []string{"--settings", `{"env":{"CLAUDE_CODE_OAUTH_TOKEN":"oauth-token"}}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.AuthMode != model.AuthModeOverrideBearer || res.AuthSource != model.AuthSourceClaudeOAuthToken || res.AuthConfigSource != "flagSettings" {
		t.Fatalf("unexpected auth resolution: mode=%s source=%s config=%s", res.AuthMode, res.AuthSource, res.AuthConfigSource)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderName != "Authorization" || res.OverrideAuth.HeaderValue != "Bearer oauth-token" {
		t.Fatalf("unexpected OAuth override: %#v", res.OverrideAuth)
	}
	if res.OverrideAuth.AdditionalHeaders["anthropic-beta"] != "oauth-2025-04-20" {
		t.Fatalf("expected OAuth beta header, got %#v", res.OverrideAuth.AdditionalHeaders)
	}
}

// TestResolveProfileAuthIgnoresTrustedSettingsToken is the settings-file twin
// of the 4297f1d ambient-env regression: a profile that OWNS auth must remain
// authoritative even when ~/.claude/settings.json's env block carries a
// first-party ANTHROPIC_AUTH_TOKEN. Without the guard, the trusted-settings
// merge clobbers the profile-injected key and the real first-party token is
// shipped to the third-party gateway.
func TestResolveProfileAuthIgnoresTrustedSettingsToken(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
		[]byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"real-first-party-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	res, err := Run(Options{
		ParentEnv:        []string{"PATH=/usr/bin"},
		WorkingDirectory: tmp,
		Profile: &ProfileInput{
			BaseURL: "https://gateway.example/v1",
			Auth:    &AuthSpec{Mode: "ccwrap_bearer", Key: "gw-key"},
		},
	})
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.RouteClass != model.RouteClassThirdPartyHidden {
		t.Fatalf("RouteClass = %q, want third-party-hidden", res.RouteClass)
	}
	if res.OverrideAuth == nil {
		t.Fatal("expected the profile to own upstream auth, got no override")
	}
	if res.OverrideAuth.HeaderValue != "Bearer gw-key" {
		t.Fatalf("upstream auth = %q, want \"Bearer gw-key\" (profile must own auth)", res.OverrideAuth.HeaderValue)
	}
	if strings.Contains(res.OverrideAuth.HeaderValue, "real-first-party-token") {
		t.Fatalf("LEAK: first-party settings token shipped to third-party gateway: %q", res.OverrideAuth.HeaderValue)
	}
}

func TestRunCapturesCCWRAPModelAliasFromFlagSettings(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"},
		WorkingDirectory: tmp,
		ChildArgs:        []string{"--settings", `{"ccwrap":{"modelAliases":{"claude-sonnet-4-6":"gateway/sonnet"}}}`, "-p", "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RouteClass != model.RouteClassThirdPartyHidden {
		t.Fatalf("RouteClass = %q", res.RouteClass)
	}
	if !res.ModelAlias.Enabled() || res.ModelAlias.Forward["claude-sonnet-4-6"] != "gateway/sonnet" {
		t.Fatalf("expected model alias from flag settings, got %#v", res.ModelAlias)
	}
}

func TestRunImportsFlagSettingsModelOverridesAsCCWRAPAlias(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"},
		WorkingDirectory: tmp,
		ChildArgs:        []string{"--settings", `{"modelOverrides":{"claude-sonnet-4-6":"gateway/sonnet"}}`, "-p", "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.ModelAlias.Enabled() || res.ModelAlias.Forward["claude-sonnet-4-6"] != "gateway/sonnet" {
		t.Fatalf("expected modelOverrides import, got %#v", res.ModelAlias)
	}
}

func TestRunRejectsClaudeVisibleModelOverridesInThirdPartyHiddenMode(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"modelOverrides":{"claude-sonnet-4-6":"gateway/sonnet"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err == nil || !strings.Contains(err.Error(), "modelOverrides") {
		t.Fatalf("expected Claude-visible modelOverrides rejection, got %v", err)
	}
}

func TestRunRejectsProviderSpecificModelFlagInThirdPartyHiddenMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp, ChildArgs: []string{"--model", "gateway/sonnet", "-p", "hello"}})
	if err == nil || !strings.Contains(err.Error(), "provider-specific model") {
		t.Fatalf("expected provider-specific --model rejection, got %v", err)
	}
}

// First-party (canonical Anthropic) now permits a Claude→Claude remap — e.g.
// claude-opus-4-8 → claude-fable-5 — because the rewritten target is a model
// api.anthropic.com can actually serve and the response normalizer restores
// the logical id. The gate only fails closed on targets the official endpoint
// can never resolve (see TestRunRejectsProviderSpecificAliasTargetOnFirstParty).
func TestRunAllowsClaudeToClaudeAliasOnFirstParty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp, ModelAliasPairs: []string{"claude-opus-4-8=claude-fable-5"}})
	if err != nil {
		t.Fatalf("first-party Claude→Claude alias should be allowed, got %v", err)
	}
	if got := res.ModelAlias.Forward["claude-opus-4-8"]; got != "claude-fable-5" {
		t.Fatalf("alias not resolved on first-party route: Forward=%v", res.ModelAlias.Forward)
	}
	if !res.ModelAlias.Enabled() || res.ModelAlias.Count() != 1 {
		t.Fatalf("expected one enabled alias, got enabled=%v count=%d", res.ModelAlias.Enabled(), res.ModelAlias.Count())
	}
}

// A provider-routed alias target (Bedrock/Vertex/ARN/path/tagged id) can never
// resolve at api.anthropic.com, so first-party must still reject it fail-closed
// rather than silently misroute to a 404 at the upstream.
func TestRunRejectsProviderSpecificAliasTargetOnFirstParty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp, ModelAliasPairs: []string{"claude-sonnet-4-6=gateway/sonnet"}})
	if err == nil || !strings.Contains(err.Error(), "provider-specific") {
		t.Fatalf("expected provider-specific alias-target rejection on first-party, got %v", err)
	}
}

func TestRunRejectsClaudeVisibleCCWRAPModelAliasesInThirdPartyHiddenMode(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"ccwrap":{"modelAliases":{"claude-sonnet-4-6":"gateway/sonnet"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err == nil || !strings.Contains(err.Error(), "model alias") {
		t.Fatalf("expected Claude-visible ccwrap.modelAliases rejection, got %v", err)
	}
}

func TestRunNormalizesProviderSpecificModelFlagThroughReverseAlias(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"},
		WorkingDirectory: tmp,
		ModelAliasPairs:  []string{"claude-sonnet-4-6=gateway/sonnet"},
		ChildArgs:        []string{"--model", "gateway/sonnet", "-p", "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "claude-sonnet-4-6", "-p", "hello"}
	if strings.Join(res.RewrittenChildArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("RewrittenChildArgs = %#v, want %#v", res.RewrittenChildArgs, want)
	}
	if !res.ModelAlias.Strict || res.ModelAlias.ProviderModelPassthrough {
		t.Fatalf("expected strict hidden alias, got %#v", res.ModelAlias)
	}
}

func TestRunAllowsProviderSpecificModelFlagWithExplicitPassthrough(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		Upstream:                      "https://gateway.example/v1",
		ParentEnv:                     []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"},
		WorkingDirectory:              tmp,
		ChildArgs:                     []string{"--model=gateway/sonnet", "-p", "hello"},
		AllowProviderModelPassthrough: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.RewrittenChildArgs, "\x00") != strings.Join([]string{"--model=gateway/sonnet", "-p", "hello"}, "\x00") {
		t.Fatalf("provider model should pass through unchanged, got %#v", res.RewrittenChildArgs)
	}
	if !res.ModelAlias.ProviderModelPassthrough || res.ModelAlias.Strict {
		t.Fatalf("expected provider passthrough with non-strict alias config, got %#v", res.ModelAlias)
	}
	if res.RouteClass != model.RouteClassThirdPartyCompatible {
		t.Fatalf("RouteClass = %q, want %q", res.RouteClass, model.RouteClassThirdPartyCompatible)
	}
}

func TestRunNormalizesProviderSpecificModelEnvThroughReverseAlias(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key", "CLAUDE_CODE_SUBAGENT_MODEL=gateway/haiku"},
		WorkingDirectory: tmp,
		ModelAliasPairs:  []string{"claude-haiku-4-5=gateway/haiku"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelEnv["CLAUDE_CODE_SUBAGENT_MODEL"] != "claude-haiku-4-5" {
		t.Fatalf("expected logical subagent model in child env, got %#v", res.ModelEnv)
	}
}

// TestRunThirdPartyHiddenReturnsMissingResult locks the contract that a
// third-party-hidden route with no auth source no longer refuses launch.
// Launch now succeeds so the inspect tools stay reachable; the Result carries
// AuthBootstrap=Missing + MissingAuthEnv="" (Case B — no profile and no
// env source named). The supervisor fail-closes at request time.
func TestRunThirdPartyHiddenReturnsMissingResult(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp})
	if err != nil {
		t.Fatalf("launch must succeed under C1 (request-time fail-closed); err=%v", err)
	}
	if res == nil {
		t.Fatal("Result must be non-nil")
	}
	if res.AuthBootstrap != model.AuthBootstrapMissing {
		t.Errorf("AuthBootstrap = %q, want %q", res.AuthBootstrap, model.AuthBootstrapMissing)
	}
	if res.MissingAuthEnv != "" {
		t.Errorf("MissingAuthEnv = %q, want empty (Case B — no profile + no env source)", res.MissingAuthEnv)
	}
}

func TestRunThirdPartyHiddenAuthOverrideEnablesPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"}, WorkingDirectory: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if res.AuthPolicy != model.AuthPolicyCCWRAPOverrideFailClosed {
		t.Fatalf("AuthPolicy = %q", res.AuthPolicy)
	}
	if res.AuthBootstrap != model.AuthBootstrapPlaceholderActive || res.AuthBootstrapKind != model.AuthBootstrapKindXAPIKey || res.AuthBootstrapEnvKey != "ANTHROPIC_API_KEY" {
		t.Fatalf("unexpected bootstrap: %s/%s/%s", res.AuthBootstrap, res.AuthBootstrapKind, res.AuthBootstrapEnvKey)
	}
}

func TestRunAllowsExplicitUnsafeAuthPassthrough(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp, AllowAuthPassthroughToThirdParty: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.RouteClass != model.RouteClassThirdPartyCompatible || res.AuthPolicy != model.AuthPolicyUnsafePassthrough {
		t.Fatalf("expected compatible unsafe passthrough, got route=%s auth_policy=%s", res.RouteClass, res.AuthPolicy)
	}
}

func TestRunRejectsAPIKeyHelperInThirdPartyHiddenMode(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"apiKeyHelper":{"command":"helper"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"}, WorkingDirectory: tmp})
	if err == nil || !strings.Contains(err.Error(), "apiKeyHelper") {
		t.Fatalf("expected apiKeyHelper hidden-mode rejection, got %v", err)
	}
}

func TestRunRejectsAuthLikeCustomHeadersInThirdPartyHiddenMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "config"))
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key", "ANTHROPIC_CUSTOM_HEADERS=X-Gateway-Key: sentinel"},
		WorkingDirectory: tmp,
	})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_CUSTOM_HEADERS") {
		t.Fatalf("expected custom header rejection, got %v", err)
	}
}

func TestBuildChildEnvInjectsPlaceholderAuth(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=real-secret"}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", nil, ChildAuthBootstrap{EnvKey: "ANTHROPIC_API_KEY", Value: "ccwrap-placeholder"}, ""), "\n")
	if strings.Contains(env, "real-secret") {
		t.Fatalf("real upstream secret leaked into child env:\n%s", env)
	}
	if !strings.Contains(env, "ANTHROPIC_API_KEY=ccwrap-placeholder") {
		t.Fatalf("placeholder auth missing from child env:\n%s", env)
	}
}

func TestRunCapturesCCWRAPNativeUpstreamAuthAndHeaders(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := Run(Options{
		ParentEnv:           []string{"PATH=/usr/bin", "CCWRAP_UPSTREAM=https://gateway.example/v1", "CCWRAP_UPSTREAM_API_KEY=real-key", `CCWRAP_UPSTREAM_HEADERS_JSON={"X-Gateway-Tenant":"team-a"}`},
		WorkingDirectory:    tmp,
		UpstreamHeaderPairs: []string{"X-Trace-Source=ccwrap"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.APIBaseURL.String() != "https://gateway.example/v1" || res.RouteConfigSource != "CCWRAP_UPSTREAM" {
		t.Fatalf("unexpected route: url=%s source=%s", res.APIBaseURL, res.RouteConfigSource)
	}
	if res.AuthSource != model.AuthSourceCCWRAPUpstreamAPIKey || res.AuthConfigSource != "CCWRAP_UPSTREAM_API_KEY" {
		t.Fatalf("unexpected auth source: %s %s", res.AuthSource, res.AuthConfigSource)
	}
	if res.UpstreamHeaders.Headers["X-Gateway-Tenant"] != "team-a" || res.UpstreamHeaders.Headers["X-Trace-Source"] != "ccwrap" {
		t.Fatalf("unexpected upstream headers: %#v", res.UpstreamHeaders.Headers)
	}
}

func TestBuildChildEnvScrubsCCWRAPNativeGatewayConfig(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"CCWRAP_UPSTREAM=https://gateway.example/v1",
		"CCWRAP_UPSTREAM_API_KEY=real-key",
		`CCWRAP_UPSTREAM_HEADERS_JSON={"X-Gateway-Key":"secret"}`,
	}
	env := strings.Join(BuildChildEnv(parent, "http://127.0.0.1:4444", "/tmp/ca-cert.pem", "/tmp/ca-bundle.pem", nil, ChildAuthBootstrap{EnvKey: "ANTHROPIC_API_KEY", Value: "placeholder"}, ""), "\n")
	for _, forbidden := range []string{"CCWRAP_UPSTREAM=", "CCWRAP_UPSTREAM_API_KEY=", "CCWRAP_UPSTREAM_HEADERS_JSON=", "real-key", "X-Gateway-Key"} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("child env should scrub %s; got:\n%s", forbidden, env)
		}
	}
	if !strings.Contains(env, "ANTHROPIC_API_KEY=placeholder") {
		t.Fatalf("expected placeholder auth in child env, got:\n%s", env)
	}
}

func TestRunRejectsDangerousShellSettingsInThirdPartyHiddenMode(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"otelHeadersHelper":"helper"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_, err := Run(Options{Upstream: "https://gateway.example/v1", ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"}, WorkingDirectory: tmp})
	if err == nil || !strings.Contains(err.Error(), "shell-exec settings") {
		t.Fatalf("expected dangerous shell hidden-mode rejection, got %v", err)
	}
}
