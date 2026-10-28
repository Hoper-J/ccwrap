package supervisor

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// TestPostureEgressToSpec_Table — resolved session egress (model.EgressConfig)
// must convert correctly into the profiles.EgressSpec shape ProbeEgress
// consumes, including URL userinfo preservation so probe traffic can
// authenticate to an upstream proxy.
func TestPostureEgressToSpec_Table(t *testing.T) {
	cases := []struct {
		name     string
		in       model.EgressConfig
		wantMode string
		wantURL  string
	}{
		{"empty", model.EgressConfig{}, "", ""},
		{"direct lowercase", model.EgressConfig{Mode: "direct"}, "direct", ""},
		{"DIRECT uppercase", model.EgressConfig{Mode: "DIRECT"}, "direct", ""},
		{"none alias", model.EgressConfig{Mode: "none"}, "direct", ""},
		{"http from HTTPSProxy", model.EgressConfig{Mode: "http", HTTPSProxy: "http://127.0.0.1:7890"}, "http", "http://127.0.0.1:7890"},
		{"https url for http mode", model.EgressConfig{Mode: "http", HTTPSProxy: "https://proxy:8443"}, "http", "https://proxy:8443"},
		{"http preserves userinfo", model.EgressConfig{Mode: "http", HTTPSProxy: "http://user:pass@proxy:8080"}, "http", "http://user:pass@proxy:8080"},
		{"socks5 from HTTPSProxy", model.EgressConfig{Mode: "socks5", HTTPSProxy: "socks5://proxy:1080"}, "socks5", "socks5://proxy:1080"},
		{"socks5h from HTTPSProxy", model.EgressConfig{Mode: "socks5h", HTTPSProxy: "socks5h://proxy:1080"}, "socks5h", "socks5h://proxy:1080"},
		{"falls back to HTTPProxy when HTTPSProxy empty", model.EgressConfig{Mode: "http", HTTPProxy: "http://corp:8080"}, "http", "http://corp:8080"},
		{"HTTPSProxy wins over HTTPProxy", model.EgressConfig{Mode: "http", HTTPSProxy: "http://https-pref:1", HTTPProxy: "http://http-fallback:2"}, "http", "http://https-pref:1"},
		{"mode set but no URL → empty", model.EgressConfig{Mode: "http"}, "", ""},
		{"inherit without URL → empty", model.EgressConfig{Mode: "inherit"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := postureEgressToSpec(tc.in)
			if got.Mode != tc.wantMode || got.URL != tc.wantURL {
				t.Errorf("postureEgressToSpec(%+v) = {mode:%q url:%q}, want {mode:%q url:%q}",
					tc.in, got.Mode, got.URL, tc.wantMode, tc.wantURL)
			}
		})
	}
}

// TestSynthesizeActiveSession_FromResolvedPosture — when the session's
// active posture has a resolved egress (e.g., from Claude settings or
// inherited env), synthesizeActiveSessionProfile must reflect it. This
// is the regression guard for the bug where the function only read the
// launcher --egress-proxy flag and missed Claude-settings / env sources.
func TestSynthesizeActiveSession_FromResolvedPosture(t *testing.T) {
	srv, _, sess, _, upstream := headerInspectorSessionWithSupervisor(t, "synth-posture.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	if state.proxy == nil {
		t.Fatal("sessionProxy not initialized")
	}

	// Simulate a Claude-settings-derived egress on the resolved posture
	// (no profile, no --egress-proxy flag — the bug scenario).
	state.active.Store(&posture{
		r: resolved{
			egress: model.EgressConfig{
				Mode:       "http",
				HTTPSProxy: "http://user:pass@corp-proxy:8080",
				HTTPProxy:  "http://user:pass@corp-proxy:8080",
				Source:     "claude_settings",
				Summary:    "http://corp-proxy:8080",
			},
		},
	})

	profile := state.proxy.synthesizeActiveSessionProfile()
	if profile.Name != "<active-session>" {
		t.Errorf("name: got %q, want <active-session>", profile.Name)
	}
	if profile.Egress.Mode != "http" {
		t.Errorf("mode: got %q, want http", profile.Egress.Mode)
	}
	// Userinfo MUST be preserved — probe needs to authenticate to the proxy.
	if profile.Egress.URL != "http://user:pass@corp-proxy:8080" {
		t.Errorf("url: got %q, want http://user:pass@corp-proxy:8080 (userinfo must survive)", profile.Egress.URL)
	}
}

// TestSynthesizeActiveSession_DirectPosture — when the posture resolved
// to "direct" (session is exiting without a proxy), the synthesized
// spec must be Mode=direct so ProbeEgress dials directly.
func TestSynthesizeActiveSession_DirectPosture(t *testing.T) {
	srv, _, sess, _, upstream := headerInspectorSessionWithSupervisor(t, "synth-direct.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	state.active.Store(&posture{
		r: resolved{
			egress: model.EgressConfig{Mode: "direct"},
		},
	})

	profile := state.proxy.synthesizeActiveSessionProfile()
	if profile.Egress.Mode != "direct" {
		t.Errorf("mode: got %q, want direct", profile.Egress.Mode)
	}
}

// TestParseLauncherEgressFlag_Table — pure function: flag-string →
// EgressSpec inverse of preflight/profile.go's spec→flag conversion.
func TestParseLauncherEgressFlag_Table(t *testing.T) {
	cases := []struct {
		in       string
		wantMode string
		wantURL  string
	}{
		{"", "inherit", ""},
		{"direct", "direct", ""},
		{"http://proxy:8080", "http", "http://proxy:8080"},
		{"https://proxy:8443", "http", "https://proxy:8443"},
		{"socks5://proxy:1080", "socks5", "socks5://proxy:1080"},
		{"socks5h://proxy:1080", "socks5h", "socks5h://proxy:1080"},
		{"weird-but-non-empty", "inherit", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseLauncherEgressFlag(tc.in)
			if got.Mode != tc.wantMode || got.URL != tc.wantURL {
				t.Errorf("in=%q → got {%s, %s}, want {%s, %s}",
					tc.in, got.Mode, got.URL, tc.wantMode, tc.wantURL)
			}
		})
	}
}

// TestEgressProbeNoProxyOverlay — the session posture's
// NoProxy bypass list must be overlaid onto an egress probe ONLY when the
// probe's egress spec actually came FROM the posture: the <active-session>
// synthetic, or a named/draft profile whose mode=inherit/empty was
// substituted from the posture. An EXPLICIT-proxy profile (mode=http/socks5
// with a URL) must be probed THROUGH its own URL — overlaying the posture
// NoProxy can match the probe target and silently dial DIRECT, reporting OK
// for a proxy that was never exercised (a dead proxy reads as working). The
// helper takes ONE posture snapshot from the caller so the Egress
// substitution and the NoProxy read can never tear across a mid-probe
// SwitchProfile.
func TestEgressProbeNoProxyOverlay(t *testing.T) {
	ap := &posture{r: resolved{egress: model.EgressConfig{NoProxy: "egress-probe.example.com"}}}
	cases := []struct {
		desc        string
		profileName string
		substituted bool
		posture     *posture
		want        string
	}{
		{"explicit-proxy profile is NOT overlaid (the bug)", "myproxy", false, ap, ""},
		{"inherit/empty substituted IS overlaid", "p-inherit", true, ap, "egress-probe.example.com"},
		{"<active-session> IS overlaid", "<active-session>", false, ap, "egress-probe.example.com"},
		{"inherit-env never overlaid", "inherit-env", false, ap, ""},
		{"explicit even with substituted=false stays empty on nil posture", "myproxy", false, nil, ""},
		{"substituted but nil posture → empty", "p-inherit", true, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := egressProbeNoProxyOverlay(tc.profileName, tc.substituted, tc.posture)
			if got != tc.want {
				t.Errorf("egressProbeNoProxyOverlay(%q, substituted=%v) = %q, want %q",
					tc.profileName, tc.substituted, got, tc.want)
			}
		})
	}
}
