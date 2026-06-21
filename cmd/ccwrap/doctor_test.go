package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

func TestDoctorReportsOverriddenNetworkEnvWithoutWarn(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://corp:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	report := runDoctorJSON(t, paths, []string{"--json"})
	checks := doctorChecksByName(report)
	overridden := checks["settings_overridden_network_env"]
	if overridden.Status != "pass" {
		t.Fatalf("expected settings_overridden_network_env pass (CCWRAP override is intended behavior), got %#v", overridden)
	}
	if !strings.Contains(overridden.Detail, "HTTPS_PROXY") {
		t.Fatalf("expected overridden detail to list HTTPS_PROXY, got %#v", overridden)
	}
	if checks["settings_policy_network_env"].Status != "pass" {
		t.Fatalf("expected settings_policy_network_env pass, got %#v", checks["settings_policy_network_env"])
	}
	if checks["launch_contract"].Status != "pass" {
		t.Fatalf("expected launch_contract pass, got %#v", checks["launch_contract"])
	}
	if report.Overall != "ok" {
		t.Fatalf("expected overall ok (non-policy override is not a warn), got %#v", report)
	}
}

func TestDoctorReportsClaudeSettingsEgress(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://corp-proxy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	report := runDoctorJSON(t, paths, []string{"--json", "--verbose"})
	checks := doctorChecksByName(report)
	egressCheck, ok := checks["egress_proxy"]
	if !ok {
		t.Fatalf("expected egress_proxy check in report: %#v", report)
	}
	if egressCheck.Status != "pass" {
		t.Fatalf("expected egress_proxy pass, got %#v", egressCheck)
	}
	if egressCheck.Detail == "" || !containsAll(egressCheck.Detail, "corp-proxy:8080", "userSettings") {
		t.Fatalf("expected egress detail to mention settings-derived proxy and source, got %#v", egressCheck)
	}
}

func TestDoctorCAReportsStructuredBundleFields(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose"})
	checks := doctorChecksByName(report)
	caCheck, ok := checks["ca"]
	if !ok {
		t.Fatalf("expected ca check in report: %#v", report)
	}
	if caCheck.Status != "pass" {
		t.Fatalf("expected ca check pass, got %#v", caCheck)
	}
	if caCheck.Fields == nil {
		t.Fatalf("expected structured ca fields, got %#v", caCheck)
	}
	bundlePath, ok := caCheck.Fields["bundle_path"].(string)
	if !ok || bundlePath == "" {
		t.Fatalf("expected non-empty bundle_path, got %#v", caCheck.Fields)
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("expected bundle_path to exist: %v", err)
	}
	if mode, _ := caCheck.Fields["mode"].(string); mode == "" {
		t.Fatalf("expected mode in structured ca fields, got %#v", caCheck.Fields)
	}
	if _, ok := caCheck.Fields["system_roots"].(bool); !ok {
		t.Fatalf("expected bool system_roots in structured ca fields, got %#v", caCheck.Fields)
	}
	if _, ok := caCheck.Fields["ccwrap_root"].(bool); !ok {
		t.Fatalf("expected bool ccwrap_root in structured ca fields, got %#v", caCheck.Fields)
	}
}

func TestDoctorFailsOnPolicyNetworkEnv(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "managed-settings.json"), []byte(`{"env":{"HTTPS_PROXY":"http://policy:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	report := runDoctorJSON(t, paths, []string{"--json", "--verbose"})
	checks := doctorChecksByName(report)
	if checks["settings_policy_network_env"].Status != "fail" {
		t.Fatalf("expected settings_policy_network_env fail, got %#v", checks["settings_policy_network_env"])
	}
	if !containsAll(checks["settings_policy_network_env"].Detail, "local/cache", "remote managed settings", "--egress-proxy") {
		t.Fatalf("expected actionable remediation in policy detail, got %#v", checks["settings_policy_network_env"])
	}
	if checks["launch_contract"].Status != "fail" {
		t.Fatalf("expected launch_contract fail, got %#v", checks["launch_contract"])
	}
	egressCheck := checks["egress_proxy"]
	if egressCheck.Status != "pass" {
		t.Fatalf("expected egress_proxy pass, got %#v", egressCheck)
	}
	if !strings.Contains(egressCheck.Detail, "Ignored detectable local/cache policy-managed network/trust env") {
		t.Fatalf("expected egress detail to mention ignored policy env, got %#v", egressCheck)
	}
	if strings.Contains(egressCheck.Detail, "Claude settings proxy sources: policySettings") {
		t.Fatalf("expected policySettings not to be reported as an active egress source, got %#v", egressCheck)
	}
	if report.Overall != "fail" {
		t.Fatalf("expected overall fail, got %#v", report)
	}
}

func runDoctorJSON(t *testing.T, paths app.Paths, args []string) model.DoctorReport {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	err = doctorCommand(paths, args)
	_ = w.Close()
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil {
		t.Fatalf("doctorCommand returned error: %v", err)
	}
	var report model.DoctorReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse doctor JSON: %v\noutput:\n%s", err, string(data))
	}
	return report
}

func doctorChecksByName(report model.DoctorReport) map[string]model.DoctorCheck {
	out := make(map[string]model.DoctorCheck, len(report.Checks))
	for _, check := range report.Checks {
		out[check.Name] = check
	}
	return out
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestDoctorWarnsOnAPIKeyHelperWithClaudeSideAuthDetail(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, ".claude")
	managedDir := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"apiKeyHelper":{"command":"helper"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", managedDir)

	report := runDoctorJSON(t, paths, []string{"--json", "--verbose"})
	check := doctorChecksByName(report)["api_key_helper"]
	if check.Status != "warn" {
		t.Fatalf("expected api_key_helper warn, got %#v", check)
	}
	if !containsAll(check.Detail, "Claude-side auth path", "blocked for third-party hidden mode", "settings.json") {
		t.Fatalf("expected precise apiKeyHelper detail, got %#v", check)
	}
}

func TestDoctorWarnsOnCustomHeadersAuthLikeValues(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose", "--", "--settings", `{"env":{"ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer secret\nX-Gateway: demo"}}`})
	check := doctorChecksByName(report)["custom_headers_auth"]
	if check.Status != "warn" {
		t.Fatalf("expected custom_headers_auth warn, got %#v", check)
	}
	if !containsAll(check.Detail, "values are not shown", "ANTHROPIC_CUSTOM_HEADERS") {
		t.Fatalf("expected custom header detail without values, got %#v", check)
	}
	if strings.Contains(check.Detail, "secret") {
		t.Fatalf("custom header detail leaked secret value: %#v", check)
	}
}

func TestDoctorFailsThirdPartyHiddenWithoutAuthOverride(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose", "--", "--settings", `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example/v1"}}`})
	check := doctorChecksByName(report)["hidden_auth_contract"]
	if check.Status != "fail" {
		t.Fatalf("expected hidden_auth_contract fail, got %#v", check)
	}
	if !containsAll(check.Detail, "Refusing to forward Claude-side authentication") {
		t.Fatalf("expected fail-closed detail, got %#v", check)
	}
}

func TestDoctorFailsAPIKeyHelperInThirdPartyHiddenMode(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose", "--", "--settings", `{"apiKeyHelper":{"command":"helper"},"env":{"ANTHROPIC_BASE_URL":"https://gateway.example/v1","ANTHROPIC_API_KEY":"gateway-key"}}`})
	check := doctorChecksByName(report)["api_key_helper"]
	if check.Status != "fail" {
		t.Fatalf("expected api_key_helper fail, got %#v", check)
	}
	if !containsAll(check.Detail, "blocked for third-party hidden mode") {
		t.Fatalf("expected hidden-mode apiKeyHelper detail, got %#v", check)
	}
}

func TestDoctorFailsAuthLikeCustomHeadersInThirdPartyHiddenMode(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose", "--", "--settings", `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example/v1","ANTHROPIC_API_KEY":"gateway-key","ANTHROPIC_CUSTOM_HEADERS":"X-Gateway-Key: secret"}}`})
	check := doctorChecksByName(report)["custom_headers_auth"]
	if check.Status != "fail" {
		t.Fatalf("expected custom_headers_auth fail, got %#v", check)
	}
	if !containsAll(check.Detail, "ANTHROPIC_CUSTOM_HEADERS", "values are not shown") {
		t.Fatalf("expected custom header detail without values, got %#v", check)
	}
	if strings.Contains(check.Detail, "secret") {
		t.Fatalf("custom header detail leaked secret value: %#v", check)
	}
}

func TestDoctorFailsDangerousShellSettingsInThirdPartyHiddenMode(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose", "--", "--settings", `{"otelHeadersHelper":"helper","env":{"ANTHROPIC_BASE_URL":"https://gateway.example/v1","ANTHROPIC_API_KEY":"gateway-key"}}`})
	check := doctorChecksByName(report)["dangerous_shell_settings"]
	if check.Status != "fail" {
		t.Fatalf("expected dangerous_shell_settings fail, got %#v", check)
	}
	if !containsAll(check.Detail, "shell-exec", "third-party hidden mode") {
		t.Fatalf("expected dangerous shell detail, got %#v", check)
	}
}

func TestDoctorGroupForCheck(t *testing.T) {
	cases := map[string]string{
		"paths":                "Runtime",
		"ca":                   "Runtime",
		"session_listener":     "Runtime",
		"effective_upstream":   "Provider + Auth",
		"hidden_auth_contract": "Provider + Auth",
		"settings_inspection":  "Settings inspection",
		"api_key_helper":       "Settings inspection",
		"custom_headers_auth":  "Settings inspection",
		"discovery":            "Discovery + launch",
		"launch_contract":      "Discovery + launch",
		"session":              "Session",
		"some_future_unknown":  "Runtime", // default fallback
	}
	for in, want := range cases {
		if got := doctorGroupForCheck(in); got != want {
			t.Fatalf("doctorGroupForCheck(%q) = %q, want %q", in, got, want)
		}
	}
	all := []string{
		"paths", "ca", "session_listener", "inherited_upstream", "provider_selection", "unsupported_env",
		"parent_env_auth_sources", "egress_proxy", "active_setting_sources",
		"effective_upstream", "auth_sources", "upstream_inputs", "hidden_auth_contract",
		"settings_unsupported_env", "settings_malformed_env", "settings_overridden_network_env",
		"settings_policy_network_env", "api_key_helper", "dangerous_shell_settings",
		"ccwrap_internal_keys_in_settings", "custom_headers_auth", "flag_settings",
		"settings_inspection", "launch_contract", "discovery", "session",
	}
	for _, n := range all {
		if _, ok := doctorGroupExplicit[n]; !ok {
			t.Fatalf("check %q has no explicit group mapping", n)
		}
	}
}

func TestDoctorTextRenderGrouped(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "doc.sock")
	out := captureStdout(t, func() { _ = doctorCommand(paths, nil) })
	mustContainAll(t, out,
		"ccwrap doctor", "passed", "fail", // counts line
		"Runtime", "Provider + Auth", "Discovery + launch",
		"✓", // at least one pass glyph
		"Overall:",
	)
	mustNotContain(t, out, "[PASS]") // old format gone

	// --verbose surfaces each check's Detail (the doctor "suggested
	// action" vehicle). Verbose output is strictly longer.
	vout := captureStdout(t, func() { _ = doctorCommand(paths, []string{"--verbose"}) })
	if len(vout) <= len(out) {
		t.Fatalf("--verbose output should be longer (Detail lines), got %d vs %d", len(vout), len(out))
	}
}

// TestDoctorAppliesDefaultProfile pins that doctor resolves the profiles.json
// default profile (as an actual launch would), so its route/auth diagnosis
// matches the launch instead of the env-only path. With a default gateway
// profile that owns auth, the route is third-party-hidden and the hidden-auth
// contract is satisfied by the profile — not a false fail-closed.
func TestDoctorAppliesDefaultProfile(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profJSON := `{"default":"gw","profiles":{"gw":{"provider":"openrouter","base_url":"https://gateway.example/v1","auth":{"mode":"ccwrap_bearer","key":"gw-key"}}}}`
	if err := os.WriteFile(filepath.Join(paths.StateDir, "profiles.json"), []byte(profJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	report := runDoctorJSON(t, paths, []string{"--json", "--verbose"})
	checks := doctorChecksByName(report)

	if checks["profile"].Status != "pass" || !strings.Contains(checks["profile"].Summary, "gw") {
		t.Fatalf("expected profile check to report the gw overlay, got %#v", checks["profile"])
	}
	if !strings.Contains(checks["effective_upstream"].Detail, "gateway.example") {
		t.Fatalf("effective_upstream should reflect the profile's gateway base_url, got %#v", checks["effective_upstream"])
	}
	// "CCWRAP-owned" summary only appears on the third-party-hidden + owned-auth
	// branch — i.e. route IS third-party AND auth IS owned by the profile. The
	// old env-only doctor would have read first-party (or passthrough → fail).
	if !strings.Contains(checks["hidden_auth_contract"].Summary, "CCWRAP-owned") {
		t.Fatalf("expected third-party-hidden + profile-owned-auth contract, got %#v", checks["hidden_auth_contract"])
	}
	if checks["hidden_auth_contract"].Status != "pass" {
		t.Fatalf("hidden_auth_contract should pass when the profile owns auth, got %#v", checks["hidden_auth_contract"])
	}
}
