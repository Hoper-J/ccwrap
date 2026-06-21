package profiletest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

func TestSanitizedEgressDescriptor_Table(t *testing.T) {
	cases := []struct {
		name string
		spec profiles.EgressSpec
		want string
	}{
		{"empty → inherit", profiles.EgressSpec{}, "inherit"},
		{"inherit explicit", profiles.EgressSpec{Mode: "inherit"}, "inherit"},
		{"direct", profiles.EgressSpec{Mode: "direct"}, "direct"},
		{"http no creds", profiles.EgressSpec{Mode: "http", URL: "http://proxy:8080"}, "http://proxy:8080"},
		{"http with creds → stripped", profiles.EgressSpec{Mode: "http", URL: "http://u:p@proxy:8080"}, "http://proxy:8080"},
		{"socks5h with creds → stripped", profiles.EgressSpec{Mode: "socks5h", URL: "socks5h://u:p@proxy:1080"}, "socks5h://proxy:1080"},
		{"malformed url → mode only", profiles.EgressSpec{Mode: "socks5", URL: "::nope"}, "socks5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizedEgressDescriptor(tc.spec)
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestParseIPInfoJSON_HappyPath(t *testing.T) {
	body := []byte(`{
		"ip": "1.2.3.4",
		"city": "Seattle",
		"region": "Washington",
		"country": "US",
		"org": "AS16509 Amazon.com, Inc.",
		"timezone": "America/Los_Angeles"
	}`)
	res := EgressProbeResult{}
	ok := parseIPInfoJSON(body, &res)
	if !ok {
		t.Fatal("want true (ip populated)")
	}
	if res.PublicIP != "1.2.3.4" {
		t.Errorf("ip: want 1.2.3.4, got %q", res.PublicIP)
	}
	if res.City != "Seattle" {
		t.Errorf("city: want Seattle, got %q", res.City)
	}
	if res.Country != "US" {
		t.Errorf("country: want US, got %q", res.Country)
	}
	if res.Org != "AS16509 Amazon.com, Inc." {
		t.Errorf("org mismatch: %q", res.Org)
	}
}

func TestParseIPInfoJSON_MalformedReturnsFalse(t *testing.T) {
	res := EgressProbeResult{}
	if parseIPInfoJSON([]byte(`not json`), &res) {
		t.Fatal("want false for non-JSON body")
	}
	if res.PublicIP != "" {
		t.Errorf("res should be untouched, got PublicIP=%q", res.PublicIP)
	}
}

func TestParseIPInfoJSON_EmptyIPReturnsFalse(t *testing.T) {
	res := EgressProbeResult{}
	if parseIPInfoJSON([]byte(`{"country":"US"}`), &res) {
		t.Fatal("want false when ip field missing")
	}
}

func TestEgressProbeUserAgent_HasNoVersionSuffix(t *testing.T) {
	if egressProbeUserAgent != "ccwrap-egress-probe" {
		t.Fatalf("UA mismatch: %q (should be exactly \"ccwrap-egress-probe\" — no version)", egressProbeUserAgent)
	}
}

// fakeIPInfo serves a minimal ipinfo-shape body. Used by all happy-path
// probe tests so no live network is touched. A 2ms sleep guarantees
// LatencyMs > 0 on fast hardware where loopback would otherwise round
// to 0 via time.Duration.Milliseconds() truncation.
func fakeIPInfo(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestProbeEgress_HappyPath(t *testing.T) {
	srv := fakeIPInfo(t, `{"ip":"1.2.3.4","city":"X","region":"Y","country":"Z","org":"AS1 Acme"}`, 200)
	defer srv.Close()

	res := ProbeEgress(profiles.Profile{Name: "p", Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusOK {
		t.Fatalf("status: want OK, got %s (err=%q)", res.Status, res.Err)
	}
	if res.PublicIP != "1.2.3.4" {
		t.Errorf("ip mismatch: %q", res.PublicIP)
	}
	if res.City != "X" || res.Country != "Z" || res.Org != "AS1 Acme" {
		t.Errorf("geo fields incomplete: %+v", res)
	}
	if res.LatencyMs <= 0 {
		t.Errorf("latency_ms: want > 0, got %d", res.LatencyMs)
	}
	if res.Target != srv.URL {
		t.Errorf("target: want %q, got %q", srv.URL, res.Target)
	}
	if res.EgressVia != "direct" {
		t.Errorf("egress_via: want direct, got %q", res.EgressVia)
	}
}

func TestProbeEgress_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 50 * time.Millisecond,
		Target:  srv.URL,
	})
	if res.Status != StatusTimeout {
		t.Fatalf("status: want TIMEOUT, got %s (err=%q)", res.Status, res.Err)
	}
	if res.LatencyMs <= 0 {
		t.Errorf("latency_ms still populated on timeout, got %d", res.LatencyMs)
	}
}

func TestProbeEgress_NetFail_BadHost(t *testing.T) {
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 1 * time.Second,
		Target:  "https://nonexistent.invalid.",
	})
	if res.Status != StatusNetFail {
		t.Fatalf("status: want NET_FAIL, got %s", res.Status)
	}
	if res.Err == "" {
		t.Error("err: want non-empty")
	}
}

func TestProbeEgress_HTTP5xx(t *testing.T) {
	srv := fakeIPInfo(t, `boom`, 500)
	defer srv.Close()
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusHTTP5xx {
		t.Fatalf("status: want HTTP_5XX, got %s", res.Status)
	}
}

func TestProbeEgress_HTTP4xx_NoParse(t *testing.T) {
	srv := fakeIPInfo(t, `{"ip":"1.2.3.4"}`, 404)
	defer srv.Close()
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusHTTP4xx {
		t.Fatalf("status: want HTTP_4XX, got %s", res.Status)
	}
	if res.PublicIP != "" {
		t.Errorf("ip should not be parsed on 4xx, got %q", res.PublicIP)
	}
}

func TestProbeEgress_MalformedJSON_StillOK(t *testing.T) {
	srv := fakeIPInfo(t, `not json`, 200)
	defer srv.Close()
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusOK {
		t.Fatalf("status: want OK (egress was reachable), got %s", res.Status)
	}
	if res.PublicIP != "" {
		t.Errorf("ip should be empty on shape mismatch, got %q", res.PublicIP)
	}
	if res.Err == "" {
		t.Error("err: want shape-mismatch note, got empty")
	}
}

// TestProbeEgress_BodyReadError_DemotesToNetFail — server sends valid
// headers (HTTP 200 + Content-Length) then closes the conn before the
// promised body arrives. io.ReadAll sees io.ErrUnexpectedEOF. The probe
// must surface this as NET_FAIL rather than reporting StatusOK + the
// 200 status code (which would falsely suggest a successful probe).
func TestProbeEgress_BodyReadError_DemotesToNetFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter not Hijacker")
		}
		// Promise 100 bytes, send headers only, slam the conn.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusNetFail {
		t.Fatalf("status: want NET_FAIL (body read failed after headers), got %s err=%q", res.Status, res.Err)
	}
	if res.Err == "" {
		t.Error("err: want diagnostic of body-read failure, got empty")
	}
}

// TestSanitizeProbeTarget — userinfo (user:pass@) must be stripped
// before the URL lands in EgressProbeResult.Target, which is wire-
// visible (JSON, CLI table, popover). EgressVia already does this
// for the proxy URL — Target needs the same protection for the
// probe target URL (env-supplied or --target flag).
func TestSanitizeProbeTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"no userinfo plain http", "http://example.com/json", "http://example.com/json"},
		{"no userinfo plain https", "https://example.com/json", "https://example.com/json"},
		{"with userinfo", "https://user:secret@private.example/json", "https://private.example/json"},
		{"user only no password", "https://user@private.example/path", "https://private.example/path"},
		{"empty user", "https://:@private.example/path", "https://private.example/path"},
		{"unparseable fallback strips creds", "://user:secret@badhost", "://badhost"},
		{"plain non-url passes through", "not a url", "not a url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeProbeTarget(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeProbeTarget(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "secret") {
				t.Errorf("output leaked credential: %q", got)
			}
		})
	}
}

// TestProbeEgress_TargetSanitizedInResult — when CCWRAP_EGRESS_TEST_URL
// carries userinfo, the actual request still goes through (auth works)
// but res.Target lands sanitized.
func TestProbeEgress_TargetSanitizedInResult(t *testing.T) {
	srv := fakeIPInfo(t, `{"ip":"1.2.3.4"}`, 200)
	defer srv.Close()
	// Inject userinfo into the env URL — the server doesn't verify
	// Basic-Auth (httptest server accepts anything), so the request
	// succeeds and we can assert the result is scrubbed.
	withCreds := strings.Replace(srv.URL, "http://", "http://probe:secret@", 1)
	t.Setenv("CCWRAP_EGRESS_TEST_URL", withCreds)
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
	})
	if res.Status != StatusOK {
		t.Fatalf("status: %s err=%q", res.Status, res.Err)
	}
	if strings.Contains(res.Target, "secret") || strings.Contains(res.Target, "probe:") {
		t.Errorf("res.Target leaked credentials: %q", res.Target)
	}
	// Sanity: the host should still be present so the user knows what was probed.
	if !strings.Contains(res.Target, "127.0.0.1") && !strings.Contains(res.Target, "localhost") {
		t.Errorf("res.Target lost the host entirely: %q", res.Target)
	}
}

// TestEgressProbeResult_LatencyMsAlwaysSerialized — the wire JSON must
// always carry latency_ms (no omitempty). A zero value distinguishes
// "sub-ms successful probe" from "field missing", and renderers must
// be able to print "0ms" rather than fall back to "—" which users
// read as broken.
func TestEgressProbeResult_LatencyMsAlwaysSerialized(t *testing.T) {
	r := EgressProbeResult{Profile: "p", Status: StatusOK, LatencyMs: 0}
	data, err := r.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"latency_ms":0`) {
		t.Fatalf("wire JSON must contain `\"latency_ms\":0`; got %s", string(data))
	}
}

func TestProbeEgress_EnvOverride(t *testing.T) {
	srv := fakeIPInfo(t, `{"ip":"9.9.9.9"}`, 200)
	defer srv.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", srv.URL)
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		// Target intentionally empty — env should win
	})
	if res.Status != StatusOK {
		t.Fatalf("status: %s err=%q", res.Status, res.Err)
	}
	if res.Target != srv.URL {
		t.Errorf("target: want env value %q, got %q", srv.URL, res.Target)
	}
}

func TestProbeEgress_OptionsTargetWinsOverEnv(t *testing.T) {
	srvA := fakeIPInfo(t, `{"ip":"1.1.1.1"}`, 200)
	defer srvA.Close()
	srvB := fakeIPInfo(t, `{"ip":"2.2.2.2"}`, 200)
	defer srvB.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", srvA.URL)
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srvB.URL,
	})
	if res.PublicIP != "2.2.2.2" {
		t.Fatalf("opts.Target should win — got ip %q", res.PublicIP)
	}
}

func TestProbeEgress_DefaultTargetConstant(t *testing.T) {
	// Expected value is assembled from fragments so this test file does
	// not contain the literal live-URL substring that
	// TestProbeEgress_NoLiveHostsInTests guards against.
	want := "https://" + "ipinfo" + ".io" + "/json"
	if defaultEgressTestTarget != want {
		t.Fatalf("default target constant drifted: %q (want %q)", defaultEgressTestTarget, want)
	}
	_ = os.Unsetenv("CCWRAP_EGRESS_TEST_URL")
}

func TestProbeEgress_NoAuthHeaderSent(t *testing.T) {
	gotAuth := ""
	gotXAPI := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPI = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ip":"0.0.0.0"}`))
	}))
	defer srv.Close()
	profile := profiles.Profile{
		Egress: profiles.EgressSpec{Mode: "direct"},
		Auth:   &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "SHOULD-NOT-LEAK"},
	}
	_ = ProbeEgress(profile, EgressProbeOptions{Timeout: 2 * time.Second, Target: srv.URL})
	if gotAuth != "" {
		t.Errorf("Authorization header leaked: %q", gotAuth)
	}
	if gotXAPI != "" {
		t.Errorf("X-Api-Key header leaked: %q", gotXAPI)
	}
}

func TestProbeEgress_UserAgentSet(t *testing.T) {
	gotUA := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"ip":"0.0.0.0"}`))
	}))
	defer srv.Close()
	_ = ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{Timeout: 2 * time.Second, Target: srv.URL})
	if gotUA != "ccwrap-egress-probe" {
		t.Errorf("UA: want ccwrap-egress-probe, got %q", gotUA)
	}
}

func TestProbeEgress_NeverPanics(t *testing.T) {
	cases := []profiles.Profile{
		{},
		{Egress: profiles.EgressSpec{Mode: "weird"}},
		{Egress: profiles.EgressSpec{Mode: "http", URL: "::malformed"}},
		{Egress: profiles.EgressSpec{Mode: "socks5", URL: ""}},
	}
	for i, p := range cases {
		t.Run("", func(t *testing.T) {
			res := ProbeEgress(p, EgressProbeOptions{Timeout: 100 * time.Millisecond, Target: "https://nonexistent.invalid."})
			_ = res
			_ = i
		})
	}
}

// TestProbeEgress_SOCKS5_Dialed — confirms ProbeEgress actually routes
// through a SOCKS5 proxy when profile.Egress.Mode = "socks5h".
//
// Target uses the non-loopback hostname "example.invalid" + socks5h so
// the NO_PROXY floor (egress.go::defaultBypassFloor) does NOT bypass
// the proxy — the floor would otherwise bypass any 127.0.0.1 target
// even when an explicit proxy is configured (same behavior as curl,
// Go net/http, Python requests). socks5h sends the FQDN to the proxy
// without local DNS resolution, and the stub blindly tunnels every
// CONNECT to the real loopback backend regardless of the requested
// host. Together this exercises the SOCKS5 dial path while still
// asserting against an in-process backend (no live network).
func TestProbeEgress_SOCKS5_Dialed(t *testing.T) {
	backend := fakeIPInfo(t, `{"ip":"5.5.5.5"}`, 200)
	defer backend.Close()
	stub, requested := newSocks5Stub(t, backend.Listener.Addr().String())
	defer stub.Close()

	res := ProbeEgress(profiles.Profile{
		Name: "via-socks",
		Egress: profiles.EgressSpec{
			Mode: "socks5h",
			URL:  "socks5h://" + stub.Addr().String(),
		},
	}, EgressProbeOptions{
		Timeout: 3 * time.Second,
		Target:  "http://example.invalid/json", // non-loopback so NO_PROXY floor doesn't bypass
	})
	if res.Status != StatusOK {
		t.Fatalf("status: want OK, got %s (err=%q)", res.Status, res.Err)
	}
	if atomic.LoadInt32(requested) == 0 {
		t.Fatal("SOCKS5 stub never dialed — ProbeEgress bypassed the proxy")
	}
}

// TestProbeEgress_HTTPSCertVerified — TLS server with self-signed cert
// (not in trust store) must fail with NET_FAIL. Guards against future
// InsecureSkipVerify regressions.
func TestProbeEgress_HTTPSCertVerified(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"never reached"}`))
	}))
	defer srv.Close()
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusNetFail {
		t.Fatalf("status: want NET_FAIL on untrusted cert, got %s (err=%q)", res.Status, res.Err)
	}
	if !containsCertError(res.Err) {
		t.Errorf("err should mention cert verification, got %q", res.Err)
	}
}

func containsCertError(s string) bool {
	// Go's stdlib emits one of these substrings depending on platform.
	for _, marker := range []string{"x509", "certificate", "unknown authority", "not trusted"} {
		if strings.Contains(strings.ToLower(s), marker) {
			return true
		}
	}
	return false
}

// TestProbeEgress_NoLiveHostsInTests scans test files to ensure no
// test accidentally hits the live ipinfo endpoint. The
// defaultEgressTestTarget constant in the implementation file is the
// only legitimate place that string belongs; a literal live URL
// pasted into a test file is the smell this guard catches.
//
// The disallowed needles are assembled at runtime from split fragments
// so this guard test does not match itself; the same trick is used in
// TestProbeEgress_DefaultTargetConstant's expected value. Future tests
// in this package that paste a live URL will still be caught because
// they won't split the literal.
func TestProbeEgress_NoLiveHostsInTests(t *testing.T) {
	files := []string{
		"egress_probe_test.go",
		"../../cmd/ccwrap/profile_test_egress_cmd_test.go",
	}
	host := "ipinfo" + ".io" // split prevents self-match
	disallowed := []string{
		"http://" + host,
		"https://" + host,
	}
	for _, fn := range files {
		data, err := os.ReadFile(fn)
		if err != nil {
			t.Skipf("could not read %s: %v", fn, err)
			continue
		}
		for _, needle := range disallowed {
			if strings.Contains(string(data), needle) {
				t.Errorf("file %s contains live host %q — tests must use httptest stubs", fn, needle)
			}
		}
	}
}

// TestProbeEgress_403_IsHTTP4xx_NotAuthFail — the egress probe sends NO
// credentials, so a 401/403 from the probe target must surface as HTTP_4XX,
// never AUTH_FAIL. Reusing the upstream-auth classifier here would mislabel
// 401/403 as AUTH_FAIL — telling the user their credentials failed when none
// were ever sent.
func TestProbeEgress_403_IsHTTP4xx_NotAuthFail(t *testing.T) {
	srv := fakeIPInfo(t, `forbidden`, 403)
	defer srv.Close()
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusHTTP4xx {
		t.Fatalf("403 egress response: want HTTP_4XX (no creds sent), got %s", res.Status)
	}
}

// TestProbeEgress_404ModelBody_IsHTTP4xx_NotModel404 — a 404 whose body
// happens to contain the word "model" must NOT be classified MODEL_404 by the
// egress probe; that arm is upstream-auth-probe semantics.
func TestProbeEgress_404ModelBody_IsHTTP4xx_NotModel404(t *testing.T) {
	srv := fakeIPInfo(t, `{"error":"model not recognized"}`, 404)
	defer srv.Close()
	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  srv.URL,
	})
	if res.Status != StatusHTTP4xx {
		t.Fatalf("404+model-body egress response: want HTTP_4XX, got %s", res.Status)
	}
}

// TestProbeEgress_DoesNotFollowRedirect — the egress probe must measure the
// CONFIGURED target's exit, not a redirected hop. With the
// default redirect policy a 302 from the probe target is followed, and the
// per-hop proxy decision's bypass floor dials a redirected internal address
// DIRECTLY — letting a malicious/compromised target steer the probe to touch
// arbitrary internal URLs (SSRF surface) and report the redirect's IP. The
// probe must NOT follow redirects.
func TestProbeEgress_DoesNotFollowRedirect(t *testing.T) {
	var redirectTargetHits int
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHits++
		_, _ = w.Write([]byte(`{"ip":"10.0.0.1","org":"internal"}`))
	}))
	defer secret.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secret.URL+"/json", http.StatusFound) // 302
	}))
	defer redirector.Close()

	res := ProbeEgress(profiles.Profile{Egress: profiles.EgressSpec{Mode: "direct"}}, EgressProbeOptions{
		Timeout: 2 * time.Second,
		Target:  redirector.URL + "/json",
	})
	if redirectTargetHits != 0 {
		t.Errorf("probe followed the redirect and dialed the redirect target %d time(s) — SSRF surface", redirectTargetHits)
	}
	if res.Status == StatusOK {
		t.Errorf("probe must not report OK from a redirected hop; got status=%s ip=%q", res.Status, res.PublicIP)
	}
}
