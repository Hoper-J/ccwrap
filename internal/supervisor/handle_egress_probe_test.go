package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// TestHandleEgressProbe_RejectsMissingToken — POST /profile/test-egress
// with no X-CCWRAP-Profile-Token header must return 403 before any side
// effect. Mirrors TestHandleProfileTest_RejectsMissingToken in
// handle_profile_probe_test.go. CSRF check runs first so the request
// consumes no resources.
func TestHandleEgressProbe_RejectsMissingToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-notok.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	body := strings.NewReader(`{"name":"x"}`)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestHandleEgressProbe_RejectsWrongToken — POST /profile/test-egress
// with a bogus X-CCWRAP-Profile-Token must return 403 before any side
// effect.
func TestHandleEgressProbe_RejectsWrongToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-wrongtok.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	body := strings.NewReader(`{"name":"x"}`)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", strings.Repeat("0", 32))
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for wrong token", resp.StatusCode)
	}
}

// TestHandleEgressProbe_RejectsNonPost — handler must reject GET/PUT/DELETE
// with 405 once dispatch is wired.
func TestHandleEgressProbe_RejectsNonPost(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-method-"+strings.ToLower(method)+".sock")
			defer upstream.Close()
			state := srv.getSession(sess.ID)
			url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
			req, _ := http.NewRequest(method, url, nil)
			req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
			resp, err := hc.Transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", method, resp.StatusCode)
			}
		})
	}
}

// TestHandleEgressProbe_RejectsBadBody — POST with a non-JSON body must
// return 400 from the handler after CSRF + method pass.
func TestHandleEgressProbe_RejectsBadBody(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-badbody.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHandleEgressProbe_RejectsEmptyName — POST with {}, {"name":""},
// or whitespace-only name must return 400 and the response body must
// mention "name" so the popover surfaces an actionable message.
func TestHandleEgressProbe_RejectsEmptyName(t *testing.T) {
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"   "}`} {
		t.Run(body, func(t *testing.T) {
			srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-emptyname-"+sanitizeSockSuffix(body)+".sock")
			defer upstream.Close()
			state := srv.getSession(sess.ID)
			url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
			req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
			resp, err := hc.Transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("body=%q → status %d, want 400", body, resp.StatusCode)
			}
			respBody, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(respBody), "name") {
				t.Errorf("body=%q → response should mention name, got %q", body, string(respBody))
			}
		})
	}
}

// sanitizeSockSuffix produces a deterministic socket-safe suffix from a
// test-input string so each subtest gets a unique Unix socket path.
func sanitizeSockSuffix(s string) string {
	r := strings.NewReplacer(`{`, "o", `}`, "c", `"`, "q", `:`, "k", `,`, "m", ` `, "s")
	return r.Replace(s)
}

// writeEgressProbeProfilesJSON places a profiles.json under the
// supervisor's StateDir so probe lookup can find it.
func writeEgressProbeProfilesJSON(t *testing.T, srv *Supervisor, body string) {
	t.Helper()
	path := filepath.Join(srv.paths.StateDir, "profiles.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
}

// TestHandleEgressProbe_UnknownProfile — POST with a profile name that
// doesn't exist in profiles.json must return 404.
func TestHandleEgressProbe_UnknownProfile(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-unknownprofile.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{"profiles": {}}`)

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"missing"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleEgressProbe_NoProfilesFile — POST when profiles.json is
// absent must return 404 (no such profile available).
func TestHandleEgressProbe_NoProfilesFile(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-nofile.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	// Do NOT write profiles.json.

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"any"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleEgressProbe_OK_WithStubbedTarget — happy path with the
// probe target stubbed via CCWRAP_EGRESS_TEST_URL. Asserts 200 +
// JSON body with status=OK and the stub's public_ip.
func TestHandleEgressProbe_OK_WithStubbedTarget(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond) // ensure LatencyMs > 0
		_, _ = w.Write([]byte(`{"ip":"7.7.7.7","country":"US","city":"Seattle","region":"WA","org":"AS1 Acme"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-ok.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{
		"profiles": {
			"gw": {"egress": {"mode": "direct"}}
		}
	}`)

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"gw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 200", resp.StatusCode, string(body))
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "OK" {
		t.Errorf("status: got %v, want OK", result["status"])
	}
	if result["public_ip"] != "7.7.7.7" {
		t.Errorf("public_ip: got %v, want 7.7.7.7", result["public_ip"])
	}
	if result["egress_via"] != "direct" {
		t.Errorf("egress_via: got %v, want direct", result["egress_via"])
	}
}

// TestHandleEgressProbe_ProbeFailureIs200 — probe network failure must
// still surface as HTTP 200 with status=NET_FAIL (or TIMEOUT) in the
// body. Only pre-probe failures (CSRF, lookup) return 4xx.
func TestHandleEgressProbe_ProbeFailureIs200(t *testing.T) {
	t.Setenv("CCWRAP_EGRESS_TEST_URL", "https://nonexistent.invalid./json")

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-probefail.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{"profiles":{"gw":{"egress":{"mode":"direct"}}}}`)

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"gw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe failure should still return 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if s, _ := result["status"].(string); s != "NET_FAIL" && s != "TIMEOUT" {
		t.Errorf("status: want NET_FAIL or TIMEOUT, got %v", result["status"])
	}
}

// TestHandleEgressProbe_ActiveSession — synthetic name "<active-session>"
// resolves through sp.session.active.Load() (the resolved posture),
// NOT state.public.ActiveProfileName. An earlier version of this test set
// the wrong field and asserted only the sentinel echo — a regression that
// dropped the entire postureEgressToSpec read path would still have
// passed. This test:
//
//  1. Populates the posture's resolved egress with a distinctive
//     SOCKS5 spec.
//  2. Asserts the wire response's egress_via field reflects that
//     posture (not "inherit", not "direct", and not anything derived
//     from a stale launcher flag).
//
// Together these prove the posture-reading code path is
// exercised on every CI run.
func TestHandleEgressProbe_ActiveSession(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ip":"3.3.3.3"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-active.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	// Pretend the session is exiting through a SOCKS5h proxy — distinct
	// enough that no fallback path (launcher flag, env) could produce
	// the same egress_via summary.
	state.active.Store(&posture{
		r: resolved{
			egress: model.EgressConfig{
				Mode:       "explicit",
				HTTPSProxy: "socks5h://posture-proxy.test:1080",
				HTTPProxy:  "socks5h://posture-proxy.test:1080",
				Source:     "test_posture",
				Summary:    "socks5h://posture-proxy.test:1080",
			},
		},
	})

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"<active-session>"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body=%s), want 200", resp.StatusCode, string(body))
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["profile"] != "<active-session>" {
		t.Errorf("profile: got %v, want <active-session>", result["profile"])
	}
	// The core assertion: egress_via reflects the posture we stored.
	// "socks5h://posture-proxy.test:1080" is unique to this test setup —
	// it cannot be derived from profiles.json (which is empty here), the
	// launcher --egress-proxy flag (also empty), or env (no proxy set).
	// Only the posture path could produce it.
	via, _ := result["egress_via"].(string)
	if !strings.Contains(via, "posture-proxy.test:1080") {
		t.Errorf("egress_via must reflect the stored posture; got %q (posture path likely not exercised)", via)
	}
}

// TestHandleEgressProbe_EgressOverride_DraftPath — when the request
// includes egress_override, the handler validates via
// profiles.ValidateEgressSpec and (on success) overlays it onto the
// resolved profile's Egress before probing. The stored profile's egress
// is "direct" but the override forces "direct" too — the test verifies
// validation passes (200 + OK) for a well-formed override.
func TestHandleEgressProbe_EgressOverride_DraftPath(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ip":"4.4.4.4"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-override.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{
		"profiles": {"gw":{"egress":{"mode":"direct"}}}
	}`)

	body := `{"name":"gw","egress_override":{"mode":"direct","url":""}}`
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("override should pass validation; got %d body=%s", resp.StatusCode, string(raw))
	}
}

// TestHandleEgressProbe_NameLessDraft — when name is empty but
// egress_override is supplied, the handler skips profile lookup and
// probes the override standalone. Result.Profile carries the
// "<draft>" sentinel. This is the popover add-mode flow: validate
// proxy edits before [save] is clicked, while the name input is
// still empty / in flight.
func TestHandleEgressProbe_NameLessDraft(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ip":"7.7.7.7"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-namelessdraft.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	// No profiles.json on disk — the name-less path must NOT touch it.

	body := `{"egress_override":{"mode":"direct","url":""}}`
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 200", resp.StatusCode, string(raw))
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["profile"] != "<draft>" {
		t.Errorf("profile: got %v, want <draft>", result["profile"])
	}
}

// TestHandleEgressProbe_NamedInheritReadsPosture — a saved profile with
// egress.mode="inherit" must probe through the session's resolved
// posture, not the supervisor's process env. Pre-fix the probe fell
// through to egress.Resolve("", os.Environ()) — and since Claude
// settings inject HTTPS_PROXY into Claude's child env (not the
// supervisor's parent env), a session showing "Egress: <proxy> · claude
// settings" on the ribbon would silently probe DIRECT for any mode=
// inherit profile. Same-session, two-⚡-buttons, two-different-exits.
//
// This test stores a distinctive socks5h posture and asserts that:
//   - probing the named inherit profile yields egress_via reflecting
//     the posture (not "inherit", not "direct"),
//   - and the supervisor's own os.Environ() is NOT consulted.
func TestHandleEgressProbe_NamedInheritReadsPosture(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-inheritposture.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	// Seed a saved profile with mode=inherit.
	writeEgressProbeProfilesJSON(t, srv, `{
		"profiles": {"p-inherit":{"egress":{"mode":"inherit"}}}
	}`)

	// Posture has a SOCKS5h URL the env couldn't possibly produce —
	// proves the substitution actually walked the posture path.
	state.active.Store(&posture{
		r: resolved{
			egress: model.EgressConfig{
				Mode:       "explicit",
				HTTPSProxy: "socks5h://posture-only.test:1080",
				HTTPProxy:  "socks5h://posture-only.test:1080",
				Source:     "claude_settings",
				Summary:    "socks5h://posture-only.test:1080",
			},
		},
	})

	// Stub probe target — doesn't matter if dial fails, we only care
	// about which proxy the probe ATTEMPTED to go through (read via
	// res.EgressVia / err shape).
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"unused"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"p-inherit"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", resp.StatusCode, string(body))
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["profile"] != "p-inherit" {
		t.Errorf("profile: got %v, want p-inherit", result["profile"])
	}
	via, _ := result["egress_via"].(string)
	// The key assertion: the posture URL must show through. "inherit"
	// or "direct" would mean we missed the substitution and read env.
	if !strings.Contains(via, "posture-only.test:1080") {
		t.Errorf("egress_via must reflect the stored posture, not the env-resolved fallback; got %q (probe ignored mode=inherit posture-substitution)", via)
	}
	if via == "inherit" || via == "direct" {
		t.Errorf("egress_via=%q means the probe fell through to env, not posture (egress posture regression)", via)
	}
}

// TestHandleEgressProbe_PropagatesPostureNoProxy — when the substitution
// path replaces profile.Egress with posture-derived spec, the probe must
// ALSO honor posture.egress.NoProxy. EgressSpec once carried only
// Mode + URL, so NoProxy was silently dropped. The probe would route
// through the proxy for hosts the forward path bypasses, defeating
// the invariant that the probe reflects what the session actually does.
//
// Test mechanism: posture has HTTPSProxy=unreachable-proxy + a NoProxy
// list matching the probe target's host. If NoProxy propagates, the
// probe dials direct → reaches the stub (OK). If NoProxy is dropped,
// the probe routes through the unreachable proxy → TIMEOUT or NET_FAIL.
// The OK-vs-failure shape distinguishes the two behaviors unambiguously.
func TestHandleEgressProbe_PropagatesPostureNoProxy(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-noproxy.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{
		"profiles": {"p-inherit":{"egress":{"mode":"inherit"}}}
	}`)

	// Stub on loopback — the probe target.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"42.42.42.42"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	// Posture: a deliberately unreachable proxy URL. If NoProxy is
	// honored, the probe doesn't try to use it for the stub's host.
	stubHost, _, _ := strings.Cut(strings.TrimPrefix(stub.URL, "http://"), ":")
	state.active.Store(&posture{
		r: resolved{
			egress: model.EgressConfig{
				Mode:       "explicit",
				HTTPSProxy: "http://127.0.0.1:1", // closed port — fails fast if used
				HTTPProxy:  "http://127.0.0.1:1",
				NoProxy:    stubHost, // bypass for stub host
				Source:     "test_posture",
				Summary:    "http://127.0.0.1:1",
			},
		},
	})

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"p-inherit"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	// If NoProxy propagated, probe bypassed the bogus proxy → reached stub → OK.
	// If NoProxy was dropped, probe went through 127.0.0.1:1 (closed) → NET_FAIL/TIMEOUT.
	status, _ := result["status"].(string)
	if status != "OK" {
		t.Errorf("probe status = %q (want OK); NoProxy likely not propagated — got result %+v", status, result)
	}
}

// TestHandleEgressProbe_OverrideInheritReadsPosture — the popover
// edit panel constructs egress_override from the form's current values.
// For a profile saved with mode=inherit, the form's mode select still
// reads "inherit", so the override carries {mode:"inherit", url:""}.
// Pre-fix this override overwrote the posture-substituted Egress with
// {mode:"inherit"}, putting the probe right back on the env-fallback
// path — same Singapore-vs-Cox split the dashboard ⚡ avoided.
//
// The substitution runs AFTER override apply for exactly this reason.
func TestHandleEgressProbe_OverrideInheritReadsPosture(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-overrideinherit.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{
		"profiles": {"p-inherit":{"egress":{"mode":"inherit"}}}
	}`)
	state.active.Store(&posture{
		r: resolved{
			egress: model.EgressConfig{
				Mode:       "explicit",
				HTTPSProxy: "socks5h://posture-only.test:1080",
				HTTPProxy:  "socks5h://posture-only.test:1080",
				Source:     "claude_settings",
				Summary:    "socks5h://posture-only.test:1080",
			},
		},
	})

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"unused"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	body := `{"name":"p-inherit","egress_override":{"mode":"inherit","url":""}}`
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", resp.StatusCode, string(raw))
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	via, _ := result["egress_via"].(string)
	if !strings.Contains(via, "posture-only.test:1080") {
		t.Errorf("egress_via must reflect posture (override-inherit substitution), got %q", via)
	}
	if via == "inherit" || via == "direct" {
		t.Errorf("egress_via=%q means override re-introduced inherit and substitution missed it", via)
	}
}

// TestHandleEgressProbe_OverrideOnSynthetic_422 — synthetic names
// (<active-session>, inherit-env) probe specific live state; an
// egress_override against them defeats the contract and would land
// in a result mislabeled with the synthetic profile name. Reject
// with 422 and point at the name-less draft alternative.
func TestHandleEgressProbe_OverrideOnSynthetic_422(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-overridesynthetic.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	for _, syntheticName := range []string{"<active-session>", "inherit-env"} {
		t.Run(syntheticName, func(t *testing.T) {
			body := fmt.Sprintf(`{"name":%q,"egress_override":{"mode":"direct"}}`, syntheticName)
			url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
			req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
			resp, err := hc.Transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("name=%q → status %d (body=%s), want 422", syntheticName, resp.StatusCode, string(raw))
			}
			raw, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(raw), "name-less") {
				t.Errorf("response should mention name-less alternative, got %q", string(raw))
			}
		})
	}
}

// TestHandleEgressProbe_EmptyOverride_422 — egress_override with both
// mode and URL empty (whitespace-only counts) is rejected with 422 so
// the caller surfaces a clear contract violation rather than silently
// swapping the resolved profile's egress for an empty spec.
func TestHandleEgressProbe_EmptyOverride_422(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-emptyoverride.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{"profiles":{"gw":{"egress":{"mode":"direct"}}}}`)

	for _, body := range []string{
		`{"name":"gw","egress_override":{}}`,
		`{"name":"gw","egress_override":{"mode":"","url":""}}`,
		`{"name":"gw","egress_override":{"mode":"  ","url":"\t"}}`,
		`{"egress_override":{}}`, // name-less + empty override
	} {
		t.Run(body, func(t *testing.T) {
			url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
			req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
			resp, err := hc.Transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("body=%q → status %d (resp=%s), want 422", body, resp.StatusCode, string(raw))
			}
		})
	}
}

// TestHandleEgressProbe_NameLessDraft_InvalidOverride_422 — name-less
// path still validates the override; an unknown mode must surface 422
// without touching the filesystem.
func TestHandleEgressProbe_NameLessDraft_InvalidOverride_422(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-namelessbadmode.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	body := `{"egress_override":{"mode":"ssh","url":""}}`
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 422", resp.StatusCode, string(raw))
	}
}

// TestHandleEgressProbe_EgressOverride_InvalidMode_422 — override with
// an unknown egress mode must return 422 via ValidateEgressSpec.
func TestHandleEgressProbe_EgressOverride_InvalidMode_422(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-override-badmode.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{"profiles":{"gw":{"egress":{"mode":"direct"}}}}`)

	body := `{"name":"gw","egress_override":{"mode":"ssh","url":""}}`
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: want 422, got %d body=%s", resp.StatusCode, string(raw))
	}
}

// TestHandleEgressProbe_EgressOverride_SchemeMismatch_422 — override
// with mode=socks5 but a non-socks5:// URL must return 422.
func TestHandleEgressProbe_EgressOverride_SchemeMismatch_422(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-override-schememismatch.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{"profiles":{"gw":{"egress":{"mode":"direct"}}}}`)

	body := `{"name":"gw","egress_override":{"mode":"socks5","url":"http://proxy:1080"}}`
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: want 422 on scheme mismatch, got %d body=%s", resp.StatusCode, string(raw))
	}
}

// TestHandleEgressProbe_NoSecretLeak — when a profile has an inline
// Auth.Key, the probe must NOT send that key to the probe target AND
// the response body must NOT contain it.
func TestHandleEgressProbe_NoSecretLeak(t *testing.T) {
	gotAuth := ""
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			gotAuth = "Authorization:" + h
		}
		if h := r.Header.Get("X-Api-Key"); h != "" {
			gotAuth = "X-Api-Key:" + h
		}
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ip":"0.0.0.0"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pteg-noleak.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeEgressProbeProfilesJSON(t, srv, `{
		"profiles": {
			"gw": {
				"egress": {"mode": "direct"},
				"auth": {"mode": "ccwrap_x_api_key", "key": "SECRET-DO-NOT-LEAK"}
			}
		}
	}`)

	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"gw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if gotAuth != "" {
		t.Errorf("auth header leaked to probe target: %q", gotAuth)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(bodyBytes), "SECRET-DO-NOT-LEAK") {
		t.Errorf("secret leaked in response body: %s", string(bodyBytes))
	}
}

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
