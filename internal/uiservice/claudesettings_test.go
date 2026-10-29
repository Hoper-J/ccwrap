package uiservice

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// The Profiles page's tests resolve an inherit egress the way a session
// does, with the proxy from the user's Claude settings; malformed settings
// are reported instead of being skipped.
func TestProfileTestsUseClaudeSettingsProxy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	settingsFile := filepath.Join(cfg, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"env":{"HTTPS_PROXY":"http://127.0.0.1:10800"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var probeOpts profiletest.ProbeOptions
	var egressOpts profiletest.EgressProbeOptions
	probeProfile = func(p profiles.Profile, o profiletest.ProbeOptions) profiletest.ProbeResult {
		probeOpts = o
		return profiletest.ProbeResult{Profile: p.Name, Status: profiletest.StatusOK}
	}
	probeEgress = func(p profiles.Profile, o profiletest.EgressProbeOptions) profiletest.EgressProbeResult {
		egressOpts = o
		return profiletest.EgressProbeResult{}
	}
	t.Cleanup(func() { probeProfile, probeEgress = profiletest.Probe, profiletest.ProbeEgress })
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	if status, _ := r.write(t, "POST", "/api/v1/profiles", `{"name":"gw","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"k"}}`); status != http.StatusCreated {
		t.Fatalf("add: %d", status)
	}

	if status, _ := r.write(t, "POST", "/api/v1/profiles/gw/test", ""); status != 200 || probeOpts.Env["HTTPS_PROXY"] != "http://127.0.0.1:10800" {
		t.Fatalf("test: %d, env %v", status, probeOpts.Env)
	}
	if status, _ := r.write(t, "POST", "/api/v1/profiles/gw/test-egress", ""); status != 200 || egressOpts.Env["HTTPS_PROXY"] != "http://127.0.0.1:10800" || !egressOpts.EnvFromSettings {
		t.Fatalf("test-egress: %d, env %v from settings %v", status, egressOpts.Env, egressOpts.EnvFromSettings)
	}

	if err := os.WriteFile(settingsFile, []byte(`{"env":"not an object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/profiles/gw/test", "/api/v1/profiles/gw/test-egress"} {
		if status, out := r.write(t, "POST", path, ""); status != http.StatusUnprocessableEntity || code(out) != "claude_settings" {
			t.Fatalf("%s with malformed settings: %d %v", path, status, out)
		}
	}
	if status, out := r.write(t, "POST", "/api/v1/egress-test", `{"egress":{"mode":"inherit"}}`); status != http.StatusUnprocessableEntity || code(out) != "claude_settings" {
		t.Fatalf("draft egress with malformed settings: %d %v", status, out)
	}
	// Settings only matter to an inherit egress.
	if status, out := r.write(t, "POST", "/api/v1/egress-test", `{"egress":{"mode":"direct"}}`); status != 200 || egressOpts.Env != nil {
		t.Fatalf("direct egress with malformed settings: %d %v, env %v", status, out, egressOpts.Env)
	}
}
