package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// TestHandleProfileTest_RejectsMissingToken — POST /profile/test with
// no X-CCWRAP-Profile-Token header must return 403 before any side effect.
// Mirrors TestHandleProfileSwitch_RejectsMissingToken (proxy_test.go:3241).
//
// NOTE: This test is expected to FAIL with status 405 until Task A7 wires
// dispatch (case "/profile/test") into the proxy router. The test is
// written here at A2 because it fits the spec compliance check; the
// pass-state (403) is delivered by A7. The handler method itself
// compiles and returns 403 when invoked directly — that's A2's
// contribution; the integration path comes online at A7.
func TestHandleProfileTest_RejectsMissingToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-notok.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	body := strings.NewReader(`{"name":"any"}`)
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

// TestHandleProfileTest_RejectsWrongToken — POST /profile/test with a
// bogus X-CCWRAP-Profile-Token must return 403 before any side effect.
// Mirrors TestHandleProfileSwitch_RejectsWrongToken (proxy_test.go:3259).
//
// Same A7-deferred FAIL note as RejectsMissingToken above: until
// dispatch is wired, this test returns 405 instead of 403.
func TestHandleProfileTest_RejectsWrongToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-wrongtok.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	body := strings.NewReader(`{"name":"any"}`)
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

// TestHandleProfileTest_RejectsNonPost — handler must reject GET/PUT/DELETE
// with 405 once dispatch is wired. Pre-A7 these still surface 405 via the
// route fallthrough rather than the handler itself, so the expected
// post-A7 contract (handler-level 405) ships green from day one.
func TestHandleProfileTest_RejectsNonPost(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-method-"+strings.ToLower(method)+".sock")
			defer upstream.Close()
			_ = srv
			state := srv.getSession(sess.ID)
			url := "http://" + sess.ProxyListenAddr + "/profile/test"
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

// TestHandleProfileTest_RejectsBadBody — POST with a non-JSON body must
// return 400 from the handler after CSRF + method pass. Pre-A7 the route
// fallthrough yields 405 instead; the test flips green at A7.
func TestHandleProfileTest_RejectsBadBody(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-badbody.sock")
	defer upstream.Close()
	_ = srv
	state := srv.getSession(sess.ID)
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
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

// TestHandleProfileTest_RejectsEmptyName — POST with {"name":""} must
// return 400 (name required) from the handler. Pre-A7 yields 405 via
// route fallthrough. Flips green at A7.
func TestHandleProfileTest_RejectsEmptyName(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-emptyname.sock")
	defer upstream.Close()
	_ = srv
	state := srv.getSession(sess.ID)
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":""}`))
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

// TestHandleProfileTest_RejectsMissingNameField — POST with {} (no name)
// must return 400 (name required) from the handler. Pre-A7 yields 405 via
// route fallthrough. Flips green at A7.
func TestHandleProfileTest_RejectsMissingNameField(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-missingname.sock")
	defer upstream.Close()
	_ = srv
	state := srv.getSession(sess.ID)
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
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

// TestHandleProfileTest_UnknownProfile — POST with a name that does not
// exist in profiles.json must surface 404 via writeProfileTestError once
// dispatch is wired (Task A7). Pre-A7 the route fallthrough yields 405,
// which is the same A2/A3 deferred pass-state.
func TestHandleProfileTest_UnknownProfile(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-unknown.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeProfilesJSON(t, srv, `{"profiles":{"glm":{"provider":"GLM","base_url":"http://nowhere","egress":{"mode":"inherit"}}}}`)
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleProfileTest_NoProfilesFile — when profiles.json is absent on
// disk, a named-profile probe request must surface 404 (the file Load
// path returns (nil, nil) for missing files, which we treat as
// "profile not found"). Pre-A7 yields 405 via route fallthrough; flips
// to 404 after A7 wires dispatch.
func TestHandleProfileTest_NoProfilesFile(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-noprofiles.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	// Intentionally do NOT write a profiles.json.
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"glm"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleProfileTest_InheritEnvMissing(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-inherit-missing.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"inherit-env"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no env creds)", resp.StatusCode)
	}
}

func TestResolveInheritEnv_APIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-fake")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	p, err := resolveInheritEnv()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Name != "inherit-env" {
		t.Errorf("name: %q", p.Name)
	}
	if p.Auth.Mode != "ccwrap_x_api_key" || p.Auth.Key != "sk-fake" {
		t.Errorf("auth: %+v", p.Auth)
	}
	if p.BaseURL != "https://api.anthropic.com" {
		t.Errorf("base_url default: %q", p.BaseURL)
	}
}

func TestResolveInheritEnv_Bearer(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-fake")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom.example")
	p, err := resolveInheritEnv()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "tok-fake" {
		t.Errorf("auth: %+v", p.Auth)
	}
	if p.BaseURL != "https://custom.example" {
		t.Errorf("base_url: %q", p.BaseURL)
	}
}

func TestResolveInheritEnv_APIKeyBeatsAuthToken(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-wins")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-loses")
	p, err := resolveInheritEnv()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Auth.Mode != "ccwrap_x_api_key" {
		t.Errorf("API_KEY must beat AUTH_TOKEN; got %s", p.Auth.Mode)
	}
}

func TestResolveInheritEnv_NoEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	_, err := resolveInheritEnv()
	if !errors.Is(err, errInheritEnvMissing) {
		t.Errorf("err: %v (want errInheritEnvMissing)", err)
	}
}

func TestHandleProfileTest_OK(t *testing.T) {
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer upstream2.Close()
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-ok.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeProfilesJSON(t, srv, fmt.Sprintf(`{"profiles":{"glm":{"base_url":%q,"auth":{"mode":"ccwrap_x_api_key","key":"sk-fake"}}}}`, upstream2.URL))
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"glm"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["profile"] != "glm" {
		t.Errorf("profile: %v", got["profile"])
	}
	if got["status"] != "OK" {
		t.Errorf("status: %v", got["status"])
	}
	if got["http_status"].(float64) != 200 {
		t.Errorf("http_status: %v", got["http_status"])
	}
}

func TestHandleProfileTest_ProbeFailureIs200(t *testing.T) {
	// Probe internal failure (AUTH_FAIL) should still be HTTP 200 + JSON.
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer upstream2.Close()
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-pfis200.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeProfilesJSON(t, srv, fmt.Sprintf(`{"profiles":{"kimi":{"base_url":%q,"auth":{"mode":"ccwrap_x_api_key","key":"sk-bad"}}}}`, upstream2.URL))
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"kimi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (probe failures stay 200); body=%s", resp.StatusCode, string(body))
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["status"] != "AUTH_FAIL" {
		t.Errorf("status: got %v, want AUTH_FAIL", got["status"])
	}
}

func TestHandleProfileTest_Passthrough(t *testing.T) {
	var upstreamHits int32
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.WriteHeader(200)
	}))
	defer upstream2.Close()
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-passthrough.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeProfilesJSON(t, srv, fmt.Sprintf(`{"profiles":{"oauth":{"base_url":%q}}}`, upstream2.URL))
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"oauth"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["status"] != "SKIPPED" {
		t.Errorf("status: got %v", got["status"])
	}
	if hits := atomic.LoadInt32(&upstreamHits); hits != 0 {
		t.Errorf("passthrough must not hit upstream; hits=%d", hits)
	}
}

// Silence "imported but not used" until profiletest types are referenced
// directly (e.g. via direct unit tests of probe-handler internals).
var _ = profiletest.StatusOK

func TestHandleProfileTest_NoSecretLeak(t *testing.T) {
	// Probe a profile whose definition includes userinfo URL, inline
	// secret, key_env name, and a sensitive upstream header. None of
	// these substrings may appear in the response JSON.
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer upstream2.Close()
	srv, _, sess, _, upstream := headerInspectorSessionWithSupervisor(t, "pte-nosecret.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	// Inject userinfo into the upstream URL.
	userinfoURL := strings.Replace(upstream2.URL, "://", "://u:supersecret@", 1)

	writeProfilesJSON(t, srv, fmt.Sprintf(
		`{"profiles":{"glm":{"base_url":%q,"auth":{"mode":"ccwrap_x_api_key","key":"sk-fake-MUST-NOT-LEAK","key_env":"MY_SECRET_VAR_NAME"},"upstream_headers":{"X-Custom-Secret":"header-leak-MUST-NOT-APPEAR"}}}}`,
		userinfoURL,
	))
	// Browser-like direct fetch — NO http.ProxyURL wrapper. Production
	// fetch('/profile/test') sends a relative URI, hitting handleInfoRequest
	// directly (not handleForwardProxyRequest). Using hc (with ProxyURL)
	// would cause a self-loopback that records the outer call to /recent,
	// which is a test-fixture artifact, not a real /profile/test code path.
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"glm"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	directClient := &http.Client{}
	resp, err := directClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	for _, secret := range []string{
		"supersecret",
		"sk-fake-MUST-NOT-LEAK",
		"MY_SECRET_VAR_NAME",
		"header-leak-MUST-NOT-APPEAR",
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("response JSON leaked %q\nfull body:\n%s", secret, string(body))
		}
	}
}

func TestHandleProfileTest_DoesNotPolluteRecent(t *testing.T) {
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer upstream2.Close()
	srv, _, sess, _, upstream := headerInspectorSessionWithSupervisor(t, "pte-norecent.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeProfilesJSON(t, srv, fmt.Sprintf(`{"profiles":{"glm":{"base_url":%q,"auth":{"mode":"ccwrap_x_api_key","key":"sk"}}}}`, upstream2.URL))

	// Snapshot recent-request count BEFORE the probe.
	before := len(srv.listRequests(sess.ID))

	// Browser-like direct fetch — NO http.ProxyURL wrapper. Production
	// fetch('/profile/test') sends a relative URI, hitting handleInfoRequest
	// directly. Using hc (with ProxyURL) would route via
	// handleForwardProxyRequest which records the outer self-loopback to
	// /recent — a test-fixture artifact, not real /profile/test behavior.
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"glm"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	directClient := &http.Client{}
	resp, err := directClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("probe failed: %d %s", resp.StatusCode, body)
	}

	// Snapshot AFTER. Must be identical — probe does not flow through
	// the proxy port and the handler does not call recordRequest.
	after := len(srv.listRequests(sess.ID))
	if after != before {
		t.Errorf("recent-request count changed: before=%d, after=%d (probe pollution)", before, after)
	}
}
