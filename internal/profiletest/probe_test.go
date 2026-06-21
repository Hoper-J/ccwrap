package profiletest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestProbe_SOCKS5_Honored — regression guard for the silent-drop bug
// where profileEgressToFlag mapped mode=socks5 to "" (env fallback)
// instead of using the profile's URL. After the fix, Probe must dial
// through the SOCKS5 proxy.
//
// Mode is "socks5h" + a non-loopback hostname because the NO_PROXY
// floor (egress.go defaultBypassFloor) unconditionally bypasses
// loopback targets even when an explicit proxy is configured — same
// behavior as curl, Go net/http, and Python requests. socks5h sends
// the FQDN to the proxy without local DNS resolution, and the stub
// blindly tunnels every CONNECT to the real loopback backend.
// Together this exercises the SOCKS5 dial path while still asserting
// against an in-process backend.
func TestProbe_SOCKS5_Honored(t *testing.T) {
	// Backend httptest server pretends to be Anthropic.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer backend.Close()

	// SOCKS5 stub that records the requested target and tunnels to backend.
	stub, requested := newSocks5Stub(t, backend.Listener.Addr().String())
	defer stub.Close()

	profile := profiles.Profile{
		Name:    "socks5-profile",
		BaseURL: "http://example.invalid", // non-loopback so NO_PROXY floor doesn't bypass
		Auth:    &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "test"},
		Egress: profiles.EgressSpec{
			Mode: "socks5h", // proxy-side DNS — no local lookup of example.invalid
			URL:  "socks5h://" + stub.Addr().String(),
		},
	}

	res := Probe(profile, ProbeOptions{Timeout: 5 * time.Second})
	if res.Status != StatusOK {
		t.Fatalf("status: want OK, got %s (err=%q)", res.Status, res.Err)
	}
	if atomic.LoadInt32(requested) == 0 {
		t.Fatalf("SOCKS5 stub recorded zero connections — Probe bypassed the proxy")
	}
}

func TestProbeStatusString(t *testing.T) {
	cases := []struct {
		s    ProbeStatus
		want string
	}{
		{StatusOK, "OK"},
		{StatusSkipped, "SKIPPED"},
		{StatusAuthFail, "AUTH_FAIL"},
		{StatusModel404, "MODEL_404"},
		{StatusHTTP4xx, "HTTP_4XX"},
		{StatusHTTP5xx, "HTTP_5XX"},
		{StatusTimeout, "TIMEOUT"},
		{StatusNetFail, "NET_FAIL"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("ProbeStatus(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestProbeStatusIsFailure(t *testing.T) {
	if StatusOK.IsFailure() || StatusSkipped.IsFailure() {
		t.Error("OK and SKIPPED must not count as failure")
	}
	for _, s := range []ProbeStatus{StatusAuthFail, StatusModel404, StatusHTTP4xx, StatusHTTP5xx, StatusTimeout, StatusNetFail} {
		if !s.IsFailure() {
			t.Errorf("%s must count as failure", s)
		}
	}
}

func TestClassifyProbeResult(t *testing.T) {
	type want struct {
		status     ProbeStatus
		errKeyword string // substring expected in the err summary, or "" if don't check
	}
	cases := []struct {
		name     string
		resp     *http.Response
		respBody string
		err      error
		want     want
	}{
		{"deadline", nil, "", context.DeadlineExceeded, want{StatusTimeout, "timeout"}},
		{"deadline-wrapped", nil, "", &net.OpError{Op: "dial", Err: context.DeadlineExceeded}, want{StatusTimeout, "timeout"}},
		{"dns", nil, "", &net.DNSError{Name: "nosuch.invalid", Err: "no such host"}, want{StatusNetFail, "dns"}},
		{"tcp-refused", nil, "", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, want{StatusNetFail, "connection refused"}},
		{"http-200-with-model", makeResp(200), `{"model":"x"}`, nil, want{StatusOK, ""}},
		{"http-200-empty-body", makeResp(200), `{}`, nil, want{StatusOK, ""}},
		{"http-401", makeResp(401), "", nil, want{StatusAuthFail, "401"}},
		{"http-403", makeResp(403), "", nil, want{StatusAuthFail, "403"}},
		{"http-404-model-keyword", makeResp(404), `{"error":"model not found"}`, nil, want{StatusModel404, "404"}},
		{"http-404-no-model-keyword", makeResp(404), `{"error":"route missing"}`, nil, want{StatusHTTP4xx, "404"}},
		{"http-400", makeResp(400), "", nil, want{StatusHTTP4xx, "400"}},
		{"http-422", makeResp(422), "", nil, want{StatusHTTP4xx, "422"}},
		{"http-500", makeResp(500), "", nil, want{StatusHTTP5xx, "500"}},
		{"http-502", makeResp(502), "", nil, want{StatusHTTP5xx, "502"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStatus, gotErr := classifyProbeResult(c.resp, []byte(c.respBody), c.err)
			if gotStatus != c.want.status {
				t.Errorf("status: got %s, want %s", gotStatus, c.want.status)
			}
			if c.want.errKeyword != "" && !strings.Contains(strings.ToLower(gotErr), strings.ToLower(c.want.errKeyword)) {
				t.Errorf("err summary %q missing keyword %q", gotErr, c.want.errKeyword)
			}
		})
	}
}

func makeResp(code int) *http.Response {
	return &http.Response{StatusCode: code, Status: http.StatusText(code)}
}

func TestProbe_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: got %q, want /v1/messages", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.MaxTokens != 1 {
			t.Errorf("max_tokens: got %d, want 1", body.MaxTokens)
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"claude-haiku-4-5-20251001","id":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{
		Name:    "anthropic",
		BaseURL: srv.URL,
		Auth:    &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "sk-test"},
	}
	got := Probe(p, ProbeOptions{Timeout: 5 * time.Second})

	if got.Status != StatusOK {
		t.Errorf("status: got %s, want OK; err=%q", got.Status, got.Err)
	}
	if got.HTTPStatus != 200 {
		t.Errorf("http_status: got %d, want 200", got.HTTPStatus)
	}
	if got.ModelEchoed != "claude-haiku-4-5-20251001" {
		t.Errorf("model_echoed: got %q", got.ModelEchoed)
	}
	if got.Latency <= 0 {
		t.Errorf("latency must be positive, got %v", got.Latency)
	}
}

func TestProbe_StampsXApiKey(t *testing.T) {
	var gotXApi, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXApi = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "sk-fake"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotXApi != "sk-fake" {
		t.Errorf("X-Api-Key: got %q, want sk-fake", gotXApi)
	}
	if gotAuth != "" {
		t.Errorf("Authorization must NOT be set when Mode=ccwrap_x_api_key; got %q", gotAuth)
	}
}

func TestProbe_StampsBearer(t *testing.T) {
	var gotXApi, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXApi = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "tok-fake"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotAuth != "Bearer tok-fake" {
		t.Errorf("Authorization: got %q, want Bearer tok-fake", gotAuth)
	}
	if gotXApi != "" {
		t.Errorf("X-Api-Key must NOT be set when Mode=ccwrap_bearer; got %q", gotXApi)
	}
}

func TestProbe_ResolvesKeyEnv(t *testing.T) {
	t.Setenv("CCWRAP_TEST_KEY_VAR", "from-env")
	var gotXApi string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXApi = r.Header.Get("X-Api-Key")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", KeyEnv: "CCWRAP_TEST_KEY_VAR"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotXApi != "from-env" {
		t.Errorf("X-Api-Key: got %q, want from-env", gotXApi)
	}
}

func TestProbe_InlineKeyBeatsKeyEnv(t *testing.T) {
	t.Setenv("CCWRAP_TEST_KEY_VAR", "from-env")
	var gotXApi string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXApi = r.Header.Get("X-Api-Key")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "inline-wins", KeyEnv: "CCWRAP_TEST_KEY_VAR"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotXApi != "inline-wins" {
		t.Errorf("X-Api-Key: got %q, want inline-wins (inline must beat env)", gotXApi)
	}
}

func TestProbe_AnthropicVersionDefault(t *testing.T) {
	var gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("Anthropic-Version")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotVer != "2023-06-01" {
		t.Errorf("Anthropic-Version: got %q, want 2023-06-01", gotVer)
	}
}

func TestProbe_UpstreamHeadersAppliedAndOverride(t *testing.T) {
	var gotVer, gotUA, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("Anthropic-Version")
		gotUA = r.Header.Get("User-Agent")
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{
		Name: "p", BaseURL: srv.URL,
		Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"},
		UpstreamHeaders: map[string]string{
			"User-Agent":        "claude-cli/1.0",
			"Anthropic-Version": "2024-10-22",
			"Anthropic-Beta":    "prompt-caching-2024-07-31",
		},
	}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotVer != "2024-10-22" {
		t.Errorf("Anthropic-Version (overridden): got %q, want 2024-10-22", gotVer)
	}
	if gotUA != "claude-cli/1.0" {
		t.Errorf("User-Agent: got %q, want claude-cli/1.0", gotUA)
	}
	if gotBeta != "prompt-caching-2024-07-31" {
		t.Errorf("Anthropic-Beta: got %q", gotBeta)
	}
}

func TestProbe_UserAgentFallback(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotUA != "github.com/Hoper-J/ccwrap/profile-test" {
		t.Errorf("User-Agent fallback: got %q, want ccwrap/profile-test", gotUA)
	}
}

func TestProbe_AliasRewritesBody(t *testing.T) {
	var bodyModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodyModel = b.Model
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"glm-4-flash"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{
		Name: "glm", BaseURL: srv.URL,
		Auth:         &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"},
		ModelAliases: map[string]string{"haiku": "glm-4-flash"},
	}
	res := Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if bodyModel != "glm-4-flash" {
		t.Errorf("body.model: got %q, want glm-4-flash (alias rewrite)", bodyModel)
	}
	if res.ModelSent != "glm-4-flash" {
		t.Errorf("ModelSent: got %q, want glm-4-flash (post-rewrite)", res.ModelSent)
	}
	if res.ModelSentRewroteFrom != "haiku" {
		t.Errorf("ModelSentRewroteFrom: got %q, want haiku", res.ModelSentRewroteFrom)
	}
}

func TestProbe_ModelOverrideBypassesAlias(t *testing.T) {
	var bodyModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodyModel = b.Model
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{
		Name: "glm", BaseURL: srv.URL,
		Auth:         &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"},
		ModelAliases: map[string]string{"haiku": "glm-4-flash"},
	}
	res := Probe(p, ProbeOptions{Timeout: 3 * time.Second, Model: "claude-sonnet-4-5-20251001"})

	if bodyModel != "claude-sonnet-4-5-20251001" {
		t.Errorf("body.model (--model override): got %q, want claude-sonnet-4-5-20251001", bodyModel)
	}
	if res.ModelSentRewroteFrom != "" {
		t.Errorf("ModelSentRewroteFrom must be empty when --model is set; got %q", res.ModelSentRewroteFrom)
	}
}

func TestProbe_AppliesEgressProxy(t *testing.T) {
	var proxyHits int
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer proxySrv.Close()

	p := profiles.Profile{
		Name:    "p",
		BaseURL: "http://example.invalid",
		Auth:    &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"},
		Egress:  profiles.EgressSpec{Mode: "http", URL: proxySrv.URL},
	}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if proxyHits == 0 {
		t.Errorf("egress proxy was not used; proxyHits=%d", proxyHits)
	}
}

func TestProbe_NoProxyFloorHonored(t *testing.T) {
	// Loopback target — must bypass proxy by NO_PROXY floor.
	var proxyHits int
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		w.WriteHeader(200)
	}))
	defer proxySrv.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer upstream.Close()

	p := profiles.Profile{
		Name:    "p",
		BaseURL: upstream.URL, // 127.0.0.1
		Auth:    &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"},
		Egress:  profiles.EgressSpec{Mode: "http", URL: proxySrv.URL},
	}
	res := Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if res.Status != StatusOK {
		t.Errorf("expected OK to loopback upstream; got %s err=%q", res.Status, res.Err)
	}
	if proxyHits != 0 {
		t.Errorf("proxy MUST be bypassed for loopback target (NO_PROXY floor); proxyHits=%d", proxyHits)
	}
}

func TestProbe_ErrMessageHidesUserinfo(t *testing.T) {
	// Loopback unbound port: connection refused. URL has userinfo.
	p := profiles.Profile{
		Name:    "p",
		BaseURL: "http://user:supersecret@127.0.0.1:1", // port 1 is reserved, refused
		Auth:    &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"},
	}
	res := Probe(p, ProbeOptions{Timeout: 2 * time.Second})

	if res.Status == StatusOK {
		t.Fatalf("expected failure to refused port; got OK")
	}
	if strings.Contains(res.Err, "supersecret") {
		t.Errorf("err MUST NOT contain userinfo; got %q", res.Err)
	}
	if strings.Contains(res.Err, "user:") {
		t.Errorf("err MUST NOT contain userinfo; got %q", res.Err)
	}
}

// TestSanitizeNetErr exercises the userinfo-stripping helper directly,
// independent of which Go runtime path produces an error string. This
// is the unit-level red→green guard for the sanitizer; the probe-level
// test above is a smoke check that still passes on runtimes where Go's
// stdlib already redacts via *url.Error.Redacted().
func TestSanitizeNetErr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"url-error-with-user-pass",
			`Post "http://user:supersecret@127.0.0.1:1/v1/messages": dial tcp 127.0.0.1:1: connect: connection refused`,
			`Post "http://127.0.0.1:1/v1/messages": dial tcp 127.0.0.1:1: connect: connection refused`,
		},
		{
			"url-error-with-user-only",
			`Get "http://justuser@example.com/path": some error`,
			`Get "http://example.com/path": some error`,
		},
		{
			"https-scheme",
			`Post "https://u:p@api.example.com/v1": tls handshake failure`,
			`Post "https://api.example.com/v1": tls handshake failure`,
		},
		{
			"no-userinfo-untouched",
			`Post "http://127.0.0.1:1/v1/messages": dial tcp 127.0.0.1:1: connect: connection refused`,
			`Post "http://127.0.0.1:1/v1/messages": dial tcp 127.0.0.1:1: connect: connection refused`,
		},
		{
			"empty",
			"",
			"",
		},
		{
			"plain-message-no-url",
			"dial: connect: connection refused",
			"dial: connect: connection refused",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeNetErr(c.in); got != c.want {
				t.Errorf("sanitizeNetErr(%q):\n  got  %q\n  want %q", c.in, got, c.want)
			}
		})
	}
}

func TestProbe_PassthroughZeroHits(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := profiles.Profile{
		Name:    "oauth-only",
		BaseURL: srv.URL,
		Auth:    &profiles.AuthSpec{Mode: "passthrough"},
	}
	res := Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if res.Status != StatusSkipped {
		t.Errorf("status: got %s, want SKIPPED", res.Status)
	}
	if res.SkippedReason == "" {
		t.Errorf("SkippedReason must be non-empty")
	}
	if !strings.Contains(strings.ToLower(res.SkippedReason), "passthrough") {
		t.Errorf("SkippedReason should mention passthrough; got %q", res.SkippedReason)
	}
	if hits := atomic.LoadInt32(&hits); hits != 0 {
		t.Errorf("passthrough MUST NOT issue HTTP request; mock hits=%d", hits)
	}
	if res.Latency != 0 {
		t.Errorf("Latency for SKIPPED should be 0; got %v", res.Latency)
	}
}

func TestProbe_StampsXApiKey_UppercaseMode(t *testing.T) {
	// D10 stamp fix: validation accepts uppercase auth.mode (matches
	// runtime applyProfileOverlay), so stampProfileAuth MUST also
	// normalize — otherwise a profile with mode="CCWRAP_X_API_KEY" would
	// pass validation but silently fail to stamp the header here,
	// surfacing as a confusing AUTH_FAIL chip.
	var gotXApi, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXApi = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "CCWRAP_X_API_KEY", Key: "sk-fake"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotXApi != "sk-fake" {
		t.Errorf("X-Api-Key: got %q, want sk-fake (D10 normalize)", gotXApi)
	}
	if gotAuth != "" {
		t.Errorf("Authorization MUST NOT be set; got %q", gotAuth)
	}
}

func TestProbe_StampsBearer_UppercaseMode(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "CCWRAP_BEARER", Key: "tok"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization: got %q, want Bearer tok (D10 normalize)", gotAuth)
	}
}

func TestProbe_StampsBearer_WhitespaceMode(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	p := profiles.Profile{Name: "p", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: " ccwrap_bearer ", Key: "tok"}}
	_ = Probe(p, ProbeOptions{Timeout: 3 * time.Second})

	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization: got %q (D10 TrimSpace + ToLower)", gotAuth)
	}
}

func TestProbeModelFromAliases(t *testing.T) {
	if k, v := probeModelFromAliases(map[string]string{"haiku": "glm-4-flash"}); k != "haiku" || v != "glm-4-flash" {
		t.Errorf("bare haiku: got %q/%q", k, v)
	}
	// The full Claude id the proxy rewrites — must be honored, not ignored.
	if k, v := probeModelFromAliases(map[string]string{"claude-haiku-4-5-20251001": "glm-4-flash"}); k != "claude-haiku-4-5-20251001" || v != "glm-4-flash" {
		t.Errorf("full-id haiku: got %q/%q", k, v)
	}
	// Bare "haiku" preferred over a full-id key.
	if k, _ := probeModelFromAliases(map[string]string{"haiku": "a", "claude-haiku-x": "b"}); k != "haiku" {
		t.Errorf("bare must win: got %q", k)
	}
	// Deterministic across multiple full-id haiku keys (sorted).
	if k, _ := probeModelFromAliases(map[string]string{"claude-haiku-z": "z", "claude-haiku-a": "a"}); k != "claude-haiku-a" {
		t.Errorf("sorted determinism: got %q", k)
	}
	// No haiku alias → empty, caller falls back to defaultProbeModel.
	if _, v := probeModelFromAliases(map[string]string{"opus": "x"}); v != "" {
		t.Errorf("no haiku: want empty, got %q", v)
	}
	if _, v := probeModelFromAliases(nil); v != "" {
		t.Errorf("nil map: want empty, got %q", v)
	}
}
