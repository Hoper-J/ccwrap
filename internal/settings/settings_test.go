package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClaudeSettingsFlagsInlineRemovesSettingsArg(t *testing.T) {
	parsed, err := ParseClaudeSettingsFlags("/work", []string{"--settings", `{"env":{"PATH":"/usr/bin"},"theme":"dark"}`, "chat", "--setting-sources=user,project"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed == nil || !parsed.Inline {
		t.Fatalf("expected inline parsed settings, got %#v", parsed)
	}
	if parsed.Settings["theme"] != "dark" {
		t.Fatalf("expected theme to survive parse, got %#v", parsed.Settings)
	}
	if len(parsed.RemainingArgs) != 2 || parsed.RemainingArgs[0] != "chat" || parsed.RemainingArgs[1] != "--setting-sources=user,project" {
		t.Fatalf("unexpected remaining args: %#v", parsed.RemainingArgs)
	}
	if parsed.SettingSources == nil || *parsed.SettingSources != "user,project" {
		t.Fatalf("expected setting-sources to be captured, got %#v", parsed.SettingSources)
	}
}

func TestParseClaudeSettingsFlagsKeepsSeparatedSettingSourcesValue(t *testing.T) {
	parsed, err := ParseClaudeSettingsFlags("/work", []string{"--setting-sources", "user,project", "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SettingSources == nil || *parsed.SettingSources != "user,project" {
		t.Fatalf("expected setting-sources to be captured, got %#v", parsed.SettingSources)
	}
	want := []string{"--setting-sources", "user,project", "chat"}
	if len(parsed.RemainingArgs) != len(want) {
		t.Fatalf("remaining args length = %d, want %d: %#v", len(parsed.RemainingArgs), len(want), parsed.RemainingArgs)
	}
	for i := range want {
		if parsed.RemainingArgs[i] != want[i] {
			t.Fatalf("remaining args[%d] = %q, want %q; all args %#v", i, parsed.RemainingArgs[i], want[i], parsed.RemainingArgs)
		}
	}
}

func TestEffectiveProxyEnvRespectsClaudeMergeOrder(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(tmp, "project")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(`{"env":{"HTTPS_PROXY":"http://global:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://user:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://project:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.local.json"), []byte(`{"env":{"HTTPS_PROXY":"http://local:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)
	result, err := EffectiveProxyEnv(projectDir, []string{"--settings", `{"env":{"HTTPS_PROXY":"http://flag:8080","NO_PROXY":"flag.internal"}}`}, []string{"HTTPS_PROXY=http://shell:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Env["HTTPS_PROXY"]; got != "http://flag:8080" {
		t.Fatalf("expected flag settings to win, got %q", got)
	}
	if got := result.Env["NO_PROXY"]; got != "flag.internal" {
		t.Fatalf("expected flag NO_PROXY to win, got %q", got)
	}
	if len(result.ContributingSources) != 5 {
		t.Fatalf("expected global/user/project/local/flag sources, got %#v", result.ContributingSources)
	}
}

func TestEffectiveProxyEnvIgnoresPolicyManagedProxyAndReportsIt(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://user:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "remote-settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://remote-managed:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://managed:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)
	result, err := EffectiveProxyEnv(tmp, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Env["HTTPS_PROXY"]; got != "http://user:8080" {
		t.Fatalf("expected policy-managed proxy to be ignored for egress auto, got %q", got)
	}
	for _, source := range result.ContributingSources {
		if source == "policySettings" {
			t.Fatalf("expected policySettings not to contribute proxy keys, got %#v", result.ContributingSources)
		}
	}
	if len(result.IgnoredPolicyNetworkEnv) == 0 {
		t.Fatalf("expected ignored policy network env to be reported, got %#v", result)
	}
	if result.IgnoredPolicyNetworkEnv[0].Source != "policySettings" {
		t.Fatalf("expected policySettings conflict metadata, got %#v", result.IgnoredPolicyNetworkEnv)
	}
}

func TestInspectLaunchClassifiesUserNetworkEnvAsOverride(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://proxy:8443"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	inspect, err := InspectLaunch("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.OverriddenNetworkEnv) != 1 {
		t.Fatalf("expected one overridden network env hit, got %#v", inspect.OverriddenNetworkEnv)
	}
	if len(inspect.PolicyNetworkEnv) != 0 {
		t.Fatalf("expected no policy network env hits, got %#v", inspect.PolicyNetworkEnv)
	}
	if inspect.OverriddenNetworkEnv[0].Source != "userSettings" {
		t.Fatalf("expected userSettings override, got %#v", inspect.OverriddenNetworkEnv[0])
	}
	if inspect.OverriddenNetworkEnv[0].Keys[0] != "HTTPS_PROXY" {
		t.Fatalf("expected HTTPS_PROXY key, got %#v", inspect.OverriddenNetworkEnv[0].Keys)
	}
}

func TestInspectLaunchClassifiesPolicyNetworkEnvAsBlocking(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://policy-proxy:8443"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	inspect, err := InspectLaunch("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.PolicyNetworkEnv) != 1 {
		t.Fatalf("expected one policy network env hit, got %#v", inspect.PolicyNetworkEnv)
	}
	if len(inspect.OverriddenNetworkEnv) != 0 {
		t.Fatalf("expected no overridden network env hits, got %#v", inspect.OverriddenNetworkEnv)
	}
	if inspect.PolicyNetworkEnv[0].Source != "policySettings" {
		t.Fatalf("expected policySettings hit, got %#v", inspect.PolicyNetworkEnv[0])
	}
	if inspect.PolicyNetworkEnv[0].Keys[0] != "HTTPS_PROXY" {
		t.Fatalf("expected HTTPS_PROXY key, got %#v", inspect.PolicyNetworkEnv[0].Keys)
	}
}

func TestInspectLaunchUsesActiveCoworkUserSettingsOnly(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://wrong:8443"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "cowork_settings.json"), []byte(`{"env":{"PATH":"/usr/bin"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CODE_USE_COWORK_PLUGINS", "true")
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	inspect, err := InspectLaunch("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.OverriddenNetworkEnv) != 0 {
		t.Fatalf("expected no override from inactive settings.json, got %#v", inspect.OverriddenNetworkEnv)
	}
	if len(inspect.PolicyNetworkEnv) != 0 {
		t.Fatalf("expected no policy network env hits, got %#v", inspect.PolicyNetworkEnv)
	}
}

func TestInspectLaunchDetectsUnsupportedEnvInFlagSettings(t *testing.T) {
	inspect, err := InspectLaunch("/work", []string{"--settings", `{"env":{"ANTHROPIC_UNIX_SOCKET":"/tmp/socket"}}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.UnsupportedEnv) != 1 {
		t.Fatalf("expected one unsupported env conflict, got %#v", inspect.UnsupportedEnv)
	}
	if inspect.UnsupportedEnv[0].Source != "flagSettings" {
		t.Fatalf("expected flagSettings conflict, got %#v", inspect.UnsupportedEnv[0])
	}
}

func TestDetectAPIKeyHelper(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"apiKeyHelper":{"command":"helper"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	hits, err := DetectAPIKeyHelper("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || filepath.Base(hits[0]) != "settings.json" {
		t.Fatalf("expected settings.json hit, got %#v", hits)
	}
}

func TestMergeUserSettingsIntoCCWRAPSessionSettingsPreservesNonProxyEnv(t *testing.T) {
	doc, err := MergeUserSettingsIntoCCWRAPSessionSettings(map[string]any{
		"theme": "dark",
		"env":   map[string]any{"PATH": "/usr/bin"},
	}, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:4444"})
	if err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" {
		t.Fatalf("expected theme to be preserved, got %#v", doc)
	}
	env, ok := doc["env"].(map[string]string)
	if !ok {
		t.Fatalf("expected env map[string]string, got %#T", doc["env"])
	}
	if env["PATH"] != "/usr/bin" || env["HTTPS_PROXY"] != "http://127.0.0.1:4444" {
		t.Fatalf("unexpected merged env: %#v", env)
	}
}

func TestMergeUserSettingsIntoCCWRAPSessionSettingsOverridesNetworkKeys(t *testing.T) {
	doc, err := MergeUserSettingsIntoCCWRAPSessionSettings(map[string]any{
		"env": map[string]any{
			"PATH":                "/usr/bin",
			"HTTPS_PROXY":         "http://corp:8080",
			"NODE_EXTRA_CA_CERTS": "/tmp/corp.pem",
		},
	}, map[string]string{
		"HTTPS_PROXY":         "http://127.0.0.1:4444",
		"NODE_EXTRA_CA_CERTS": "/tmp/ccwrap.pem",
	})
	if err != nil {
		t.Fatalf("expected CCWRAP-managed network keys to override, got %v", err)
	}
	env, ok := doc["env"].(map[string]string)
	if !ok {
		t.Fatalf("expected env map[string]string, got %#T", doc["env"])
	}
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("expected PATH to survive merge, got %#v", env)
	}
	if env["HTTPS_PROXY"] != "http://127.0.0.1:4444" {
		t.Fatalf("expected HTTPS_PROXY to be overridden by CCWRAP, got %#v", env)
	}
	if env["NODE_EXTRA_CA_CERTS"] != "/tmp/ccwrap.pem" {
		t.Fatalf("expected NODE_EXTRA_CA_CERTS to be overridden by CCWRAP, got %#v", env)
	}
}

func TestMergeUserSettingsIntoCCWRAPSessionSettingsStripsProviderAuthKeys(t *testing.T) {
	doc, err := MergeUserSettingsIntoCCWRAPSessionSettings(map[string]any{
		"env": map[string]any{
			"PATH":                    "/usr/bin",
			"ANTHROPIC_BASE_URL":      "https://flag.example",
			"ANTHROPIC_API_KEY":       "flag-key",
			"ANTHROPIC_AUTH_TOKEN":    "flag-token",
			"CLAUDE_CODE_OAUTH_TOKEN": "flag-oauth",
		},
	}, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:4444"})
	if err != nil {
		t.Fatal(err)
	}
	env, ok := doc["env"].(map[string]string)
	if !ok {
		t.Fatalf("expected env map[string]string, got %#T", doc["env"])
	}
	if env["PATH"] != "/usr/bin" || env["HTTPS_PROXY"] != "http://127.0.0.1:4444" {
		t.Fatalf("expected non-owned env plus CCWRAP proxy env to survive, got %#v", env)
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[key]; ok {
			t.Fatalf("expected %s to be stripped from generated session settings env, got %#v", key, env)
		}
	}
}

func TestGlobalConfigPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, ".claude"))
	t.Setenv("HOME", tmp)
	got := GlobalConfigPath()
	if filepath.Base(got) != ".claude.json" {
		t.Fatalf("expected .claude.json, got %s", got)
	}
}

func TestGlobalConfigPathLegacyFallback(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ".config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("HOME", tmp)
	got := GlobalConfigPath()
	if filepath.Base(got) != ".config.json" {
		t.Fatalf("expected legacy .config.json, got %s", got)
	}
}

func TestEffectiveProxyEnvFromInspectionUsesSnapshot(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(configDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"HTTPS_PROXY":"http://user-snapshot:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	inspect, err := InspectLaunch(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	result, err := EffectiveProxyEnvFromInspection([]string{"HTTPS_PROXY=http://parent:8080"}, inspect)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Env["HTTPS_PROXY"]; got != "http://user-snapshot:8080" {
		t.Fatalf("expected snapshot-derived HTTPS proxy, got %q", got)
	}
}

func TestEffectiveProviderEnvUsesTrustedClaudeOrderAndIgnoresProjectLocal(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(tmp, "project")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://global.example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://user.example","ANTHROPIC_API_KEY":"user-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://project.example","ANTHROPIC_AUTH_TOKEN":"project-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.local.json"), []byte(`{"env":{"CLAUDE_CODE_OAUTH_TOKEN":"local-oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"policy-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	result, err := EffectiveProviderEnv(projectDir, []string{"--settings", `{"env":{"ANTHROPIC_BASE_URL":"https://flag.example"}}`}, []string{"ANTHROPIC_BASE_URL=https://shell.example"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Env["ANTHROPIC_BASE_URL"]; got != "https://flag.example" {
		t.Fatalf("expected flag settings provider URL to win before policy, got %q", got)
	}
	if got := result.KeySources["ANTHROPIC_BASE_URL"]; got != "flagSettings" {
		t.Fatalf("expected flagSettings key source, got %q", got)
	}
	if got := result.Env["ANTHROPIC_API_KEY"]; got != "user-key" {
		t.Fatalf("expected user API key to contribute, got %q", got)
	}
	if got := result.Env["ANTHROPIC_AUTH_TOKEN"]; got != "policy-token" {
		t.Fatalf("expected policy auth token to contribute after flag settings, got %q", got)
	}
	if got := result.Env["CLAUDE_CODE_OAUTH_TOKEN"]; got != "" {
		t.Fatalf("expected project/local OAuth token to be ignored, got %q", got)
	}
	if len(result.IgnoredProjectScopedProviderEnv) != 2 {
		t.Fatalf("expected project/local provider env to be reported as ignored, got %#v", result.IgnoredProjectScopedProviderEnv)
	}
}

func TestParseClaudeSettingsFlagsStopsAtClaudeDoubleDash(t *testing.T) {
	parsed, err := ParseClaudeSettingsFlags("/work", []string{"--", "--settings", "./literal.json"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.OriginalArgValue != "" || parsed.Settings != nil {
		t.Fatalf("expected no parsed flag settings after Claude --, got %#v", parsed)
	}
	want := []string{"--", "--settings", "./literal.json"}
	if !sameStringSlice(parsed.RemainingArgs, want) {
		t.Fatalf("remaining args = %#v, want %#v", parsed.RemainingArgs, want)
	}
}

func TestParseSettingSourcesStopsAtClaudeDoubleDash(t *testing.T) {
	parsed, err := ParseClaudeSettingsFlags("/work", []string{"--", "--setting-sources", "user"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, raw, err := parseSettingSourcesFlag(parsed.RemainingArgs)
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Fatalf("expected --setting-sources after Claude -- to be ignored by CCWRAP inspection, got %q", *raw)
	}
	for _, key := range []string{"userSettings", "projectSettings", "localSettings"} {
		if !allowed[key] {
			t.Fatalf("expected default source %s to remain allowed, got %#v", key, allowed)
		}
	}
}

func TestGeneratedSessionSettingsStripsHostManagedProviderModelAuthKeys(t *testing.T) {
	doc, err := MergeUserSettingsIntoCCWRAPSessionSettings(map[string]any{
		"env": map[string]any{
			"PATH":                                 "/usr/bin",
			"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "0",
			"ANTHROPIC_BASE_URL":                   "https://flag.example",
			"ANTHROPIC_API_KEY":                    "flag-key",
			"ANTHROPIC_AUTH_TOKEN":                 "flag-token",
			"CLAUDE_CODE_OAUTH_TOKEN":              "flag-oauth",
			"CLAUDE_CODE_USE_BEDROCK":              "1",
			"CLAUDE_CODE_USE_VERTEX":               "1",
			"CLAUDE_CODE_USE_FOUNDRY":              "1",
			"ANTHROPIC_BEDROCK_BASE_URL":           "https://bedrock.example",
			"ANTHROPIC_VERTEX_BASE_URL":            "https://vertex.example",
			"ANTHROPIC_FOUNDRY_BASE_URL":           "https://foundry.example",
			"ANTHROPIC_FOUNDRY_RESOURCE":           "resource",
			"ANTHROPIC_VERTEX_PROJECT_ID":          "project",
			"CLOUD_ML_REGION":                      "us-central1",
			"AWS_BEARER_TOKEN_BEDROCK":             "bedrock-token",
			"ANTHROPIC_FOUNDRY_API_KEY":            "foundry-key",
			"CLAUDE_CODE_SKIP_BEDROCK_AUTH":        "1",
			"CLAUDE_CODE_SKIP_VERTEX_AUTH":         "1",
			"CLAUDE_CODE_SKIP_FOUNDRY_AUTH":        "1",
			"ANTHROPIC_MODEL":                      "provider-model",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":       "provider-sonnet",
			"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME":  "provider-sonnet-name",
			"ANTHROPIC_SMALL_FAST_MODEL":           "provider-fast",
			"CLAUDE_CODE_SUBAGENT_MODEL":           "provider-subagent",
			"VERTEX_REGION_CLAUDE_3_5_HAIKU":       "europe-west1",
			"ANTHROPIC_CUSTOM_HEADERS":             "X-Gateway: demo",
		},
	}, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:4444"})
	if err != nil {
		t.Fatal(err)
	}
	env, ok := doc["env"].(map[string]string)
	if !ok {
		t.Fatalf("expected env map[string]string, got %#T", doc["env"])
	}
	for key, want := range map[string]string{
		"PATH":                     "/usr/bin",
		"ANTHROPIC_CUSTOM_HEADERS": "X-Gateway: demo",
		"HTTPS_PROXY":              "http://127.0.0.1:4444",
	} {
		if got := env[key]; got != want {
			t.Fatalf("expected %s=%q to survive generated settings merge, got %#v", key, want, env)
		}
	}
	for _, key := range []string{
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_RESOURCE",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"CLOUD_ML_REGION",
		"AWS_BEARER_TOKEN_BEDROCK",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
		"CLAUDE_CODE_SKIP_VERTEX_AUTH",
		"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"VERTEX_REGION_CLAUDE_3_5_HAIKU",
	} {
		if _, ok := env[key]; ok {
			t.Fatalf("expected %s to be stripped from generated session settings env, got %#v", key, env)
		}
	}
}

func TestEffectiveModelEnvUsesTrustedClaudeOrderAndIgnoresProjectLocal(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(tmp, "project")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(`{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"global-subagent"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"ANTHROPIC_DEFAULT_SONNET_MODEL":"user-sonnet"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.json"), []byte(`{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"project-subagent"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".claude", "settings.local.json"), []byte(`{"env":{"ANTHROPIC_MODEL":"local-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"ANTHROPIC_DEFAULT_SONNET_MODEL":"policy-sonnet"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	result, err := EffectiveModelEnv(projectDir, []string{"--settings", `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"flag-subagent"}}`}, []string{"ANTHROPIC_MODEL=parent-model"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Env["CLAUDE_CODE_SUBAGENT_MODEL"]; got != "flag-subagent" {
		t.Fatalf("expected flag settings subagent model to win, got %q", got)
	}
	if got := result.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"]; got != "policy-sonnet" {
		t.Fatalf("expected policy model default to win after user settings, got %q", got)
	}
	if got := result.Env["ANTHROPIC_MODEL"]; got != "parent-model" {
		t.Fatalf("expected inherited model to survive when not overridden by trusted settings, got %q", got)
	}
	if len(result.IgnoredProjectScopedModelEnv) != 2 {
		t.Fatalf("expected project/local model env to be reported as ignored, got %#v", result.IgnoredProjectScopedModelEnv)
	}
}

func TestInspectLaunchDetectsCustomHeadersAuthLikeValues(t *testing.T) {
	inspect, err := InspectLaunch("/work", []string{"--settings", `{"env":{"ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer secret\nX-Gateway: demo"}}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.CustomAuthHeaderEnv) != 1 {
		t.Fatalf("expected auth-like custom headers to be reported once, got %#v", inspect.CustomAuthHeaderEnv)
	}
	if inspect.CustomAuthHeaderEnv[0].Source != "flagSettings" || inspect.CustomAuthHeaderEnv[0].Keys[0] != "ANTHROPIC_CUSTOM_HEADERS" {
		t.Fatalf("unexpected custom header conflict metadata: %#v", inspect.CustomAuthHeaderEnv[0])
	}
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGeneratedSessionSettingsStripsCCWRAPModelAliasControls(t *testing.T) {
	doc, err := MergeUserSettingsIntoCCWRAPSessionSettings(map[string]any{
		"theme": "dark",
		"ccwrap": map[string]any{
			"modelAliases": map[string]any{"claude-sonnet-4-6": "gateway/sonnet"},
		},
		"modelOverrides": map[string]any{"claude-sonnet-4-6": "gateway/sonnet"},
		"env": map[string]any{
			"PATH":                      "/usr/bin",
			"CCWRAP_MODEL_ALIASES_JSON": `{"claude-sonnet-4-6":"gateway/sonnet"}`,
		},
	}, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:4444"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["ccwrap"]; ok {
		t.Fatalf("expected ccwrap block to be stripped from generated session settings: %#v", doc)
	}
	if _, ok := doc["modelOverrides"]; ok {
		t.Fatalf("expected modelOverrides to be stripped from generated session settings: %#v", doc)
	}
	env, ok := doc["env"].(map[string]string)
	if !ok {
		t.Fatalf("expected env map[string]string, got %#T", doc["env"])
	}
	if _, ok := env["CCWRAP_MODEL_ALIASES_JSON"]; ok {
		t.Fatalf("expected CCWRAP internal env to be stripped from generated session settings: %#v", env)
	}
	if env["PATH"] != "/usr/bin" || env["HTTPS_PROXY"] != "http://127.0.0.1:4444" {
		t.Fatalf("expected safe env and CCWRAP proxy env to remain, got %#v", env)
	}
}

func TestAuthLikeCustomHeaderNamesDetectsGatewaySecrets(t *testing.T) {
	names := AuthLikeCustomHeaderNames("X-Gateway-Key: secret\nX-Tenant: team\nAuthorization: Bearer secret\nX-Trace-ID: abc")
	joined := strings.Join(names, ",")
	for _, want := range []string{"Authorization", "X-Gateway-Key"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in auth-like headers, got %#v", want, names)
		}
	}
	if strings.Contains(joined, "X-Tenant") || strings.Contains(joined, "X-Trace-ID") {
		t.Fatalf("non-auth headers should not be flagged, got %#v", names)
	}
}

func TestInspectLaunchDetectsDangerousShellSettings(t *testing.T) {
	// statusLine was removed from the dangerous list because of a high
	// false-positive rate on UI decorations like git branch / clock.
	// Test now uses two real credential-handling settings instead.
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"otelHeadersHelper":"helper","awsAuthRefresh":"refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	inspect, err := InspectLaunch("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.APIKeyHelperHits) != 0 {
		t.Fatalf("apiKeyHelper should remain separate, got %#v", inspect.APIKeyHelperHits)
	}
	if len(inspect.DangerousShellSettings) != 2 {
		t.Fatalf("expected two dangerous shell settings, got %#v", inspect.DangerousShellSettings)
	}
	joined := inspect.DangerousShellSettings[0].Keys[0] + "," + inspect.DangerousShellSettings[1].Keys[0]
	if !strings.Contains(joined, "otelHeadersHelper") || !strings.Contains(joined, "awsAuthRefresh") {
		t.Fatalf("expected otelHeadersHelper and awsAuthRefresh hits, got %#v", inspect.DangerousShellSettings)
	}
}

// TestInspectLaunchTreatsStatusLineAsBenign verifies statusLine is NOT
// flagged as dangerous — it's a UI decoration in virtually all real
// configs and the false-positive rate of blocking launches on its
// presence was high.
func TestInspectLaunchTreatsStatusLineAsBenign(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"statusLine":{"command":"git rev-parse --abbrev-ref HEAD"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	inspect, err := InspectLaunch("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.DangerousShellSettings) != 0 {
		t.Errorf("statusLine should NOT trip the dangerous-shell-settings detector, got %#v", inspect.DangerousShellSettings)
	}
}

func TestInspectLaunchClassifiesPolicyOTELEndpointAsBlocking(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://attacker.example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	inspect, err := InspectLaunch("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.PolicyNetworkEnv) != 1 {
		t.Fatalf("expected policy network OTEL endpoint hit, got %#v", inspect.PolicyNetworkEnv)
	}
	if inspect.PolicyNetworkEnv[0].Keys[0] != "OTEL_EXPORTER_OTLP_ENDPOINT" {
		t.Fatalf("expected OTEL endpoint key, got %#v", inspect.PolicyNetworkEnv[0])
	}
}
