package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// recentServer is a stand-in for the per-session supervisor's HTTP surface
// that capture polls: /recent (the request ring), /recent/body?id= (spilled
// bodies), and /native-tls/clienthello.bin (the captured ClientHello).
type recentServer struct {
	mu sync.Mutex

	records  []model.RequestRecord
	bodies   map[string][]byte // id -> body bytes
	hello    []byte            // raw ClientHello bytes (may be nil)
	bodyMiss map[string]int    // id -> remaining 404s before serving (spill retry)
}

func (s *recentServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/recent", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		recs := s.records
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": recs})
	})
	mux.HandleFunc("/recent/body", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		s.mu.Lock()
		if s.bodyMiss[id] > 0 {
			s.bodyMiss[id]--
			s.mu.Unlock()
			http.Error(w, "not available yet", http.StatusNotFound)
			return
		}
		b, ok := s.bodies[id]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "no such body", http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/native-tls/clienthello.bin", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		hello := s.hello
		s.mu.Unlock()
		if len(hello) == 0 {
			http.Error(w, "no hello", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(hello)
	})
	return mux
}

// undiciHello reads the real captured undici ClientHello fixture so
// tlsfp.Compute yields the known baseline JA3.
func undiciHello(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../internal/supervisor/testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatalf("read undici fixture: %v", err)
	}
	return b
}

const undiciBaselineJA3 = "983846581fdb62fafdb21d2282592c57"

func TestRunCaptureLoop_HappyPath(t *testing.T) {
	srv := &recentServer{
		records: []model.RequestRecord{{
			Method: "POST", LogicalTargetHost: "api.anthropic.com",
			Path: "/v1/messages", StatusCode: 200,
			BodyRef:         &model.RequestBodyRef{ID: "req1"},
			ResponseBodyRef: &model.RequestBodyRef{ID: "resp1"},
		}},
		bodies: map[string][]byte{
			"req1":  []byte(`{"model":"claude-x","max_tokens":7}`),
			"resp1": []byte("event: message_start\ndata: {}\n\n"),
		},
		hello: undiciHello(t),
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{Response: true, TLS: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	res, synthetic, err := runCaptureLoop(ts.URL, opts, time.Now().Add(2*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop: %v", err)
	}
	if synthetic {
		t.Fatalf("non-synthetic record must not set synthetic flag")
	}
	if res.Request == nil {
		t.Fatalf("request block missing: %+v", res)
	}
	gotBody, _ := json.Marshal(res.Request.Body)
	if string(gotBody) != `{"max_tokens":7,"model":"claude-x"}` {
		t.Fatalf("request body mismatch: %s", gotBody)
	}
	if res.Response == nil || res.Response.BodyEncoding != "sse" {
		t.Fatalf("response should be sse: %+v", res.Response)
	}
	if res.TLS == nil || res.TLS.JA3 != undiciBaselineJA3 {
		t.Fatalf("tls JA3 mismatch: %+v", res.TLS)
	}
}

func TestRunCaptureLoop_ExactPathBeatsCountTokens(t *testing.T) {
	srv := &recentServer{
		// count_tokens recorded BEFORE the real /v1/messages — a prefix
		// matcher would wrongly pick this one.
		records: []model.RequestRecord{
			{Path: "/v1/messages/count_tokens", LogicalTargetHost: "api.anthropic.com", StatusCode: 200,
				BodyRef: &model.RequestBodyRef{ID: "ct"}, ResponseBodyRef: &model.RequestBodyRef{ID: "ctr"}},
			{Path: "/v1/messages", LogicalTargetHost: "api.anthropic.com", StatusCode: 200,
				BodyRef: &model.RequestBodyRef{ID: "req1"}, ResponseBodyRef: &model.RequestBodyRef{ID: "resp1"}},
		},
		bodies: map[string][]byte{
			"ct":    []byte(`{"input_tokens":3}`),
			"ctr":   []byte(`{"input_tokens":3}`),
			"req1":  []byte(`{"model":"claude-x"}`),
			"resp1": []byte("event: message_start\ndata: {}\n\n"),
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{Response: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	res, _, err := runCaptureLoop(ts.URL, opts, time.Now().Add(2*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop: %v", err)
	}
	if res.Request == nil || res.Request.Path != "/v1/messages" {
		t.Fatalf("expected /v1/messages, got %+v", res.Request)
	}
	b, _ := json.Marshal(res.Request.Body)
	if !strings.Contains(string(b), `"model":"claude-x"`) {
		t.Fatalf("matched the wrong (count_tokens) record: %s", b)
	}
}

func TestRunCaptureLoop_SyntheticReturnsPromptlyWithFlag(t *testing.T) {
	srv := &recentServer{
		records: []model.RequestRecord{
			{Path: "/v1/messages", LogicalTargetHost: "api.anthropic.com", StatusCode: 502, Synthetic: true},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{Response: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	start := time.Now()
	// Generous deadline; a synthetic match must return immediately, not wait
	// it out. The 4s promptness bound stays well under the 10s deadline (the
	// discriminator: a broken strict matcher only returns via giveUp AT the
	// deadline) while giving a starved CI runner scheduling slack.
	res, synthetic, err := runCaptureLoop(ts.URL, opts, time.Now().Add(10*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop: %v", err)
	}
	if !synthetic {
		t.Fatalf("synthetic record must set the synthetic flag")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("synthetic match should return promptly, took %v", time.Since(start))
	}
	joined := strings.Join(res.Meta.Notes, " ")
	if !strings.Contains(joined, "502") {
		t.Fatalf("expected a 502 note for the synthetic record: %v", res.Meta.Notes)
	}
}

func TestRunCaptureLoop_SpillRetry(t *testing.T) {
	srv := &recentServer{
		records: []model.RequestRecord{{
			Method: "POST", LogicalTargetHost: "api.anthropic.com",
			Path: "/v1/messages", StatusCode: 200,
			BodyRef: &model.RequestBodyRef{ID: "req1"},
		}},
		bodies:   map[string][]byte{"req1": []byte(`{"model":"claude-x"}`)},
		bodyMiss: map[string]int{"req1": 1}, // 404 once, then serve
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{Response: false, Host: "api.anthropic.com", Path: "/v1/messages"}
	res, _, err := runCaptureLoop(ts.URL, opts, time.Now().Add(2*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop: %v", err)
	}
	if res.Request == nil {
		t.Fatalf("expected a request block")
	}
	b, _ := json.Marshal(res.Request.Body)
	if !strings.Contains(string(b), `"model":"claude-x"`) {
		t.Fatalf("spill retry failed; body=%s", b)
	}
}

// TestRunCaptureLoop_BodyLessResponseDoesNotHang guards Fix 3: a successful
// request whose response body was empty / never spilled (BodyRef set,
// ResponseBodyRef nil) under --response must NOT hang to the deadline and must
// NOT report "no request reached the API". The strict matcher never fires (no
// response body), so the loop falls through to the relaxed give-up poll, which
// reports the request with an absent response block + note.
func TestRunCaptureLoop_BodyLessResponseDoesNotHang(t *testing.T) {
	srv := &recentServer{
		records: []model.RequestRecord{{
			Method: "POST", LogicalTargetHost: "api.anthropic.com",
			Path: "/v1/messages", StatusCode: 200,
			BodyRef: &model.RequestBodyRef{ID: "req1"}, // response ref intentionally nil
		}},
		bodies: map[string][]byte{"req1": []byte(`{"model":"claude-x","max_tokens":7}`)},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{Response: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	start := time.Now()
	// Short deadline: with the bug this would spin to the deadline then error.
	res, synthetic, err := runCaptureLoop(ts.URL, opts, time.Now().Add(400*time.Millisecond), nil)
	if err != nil {
		t.Fatalf("body-less response must not error: %v", err)
	}
	if synthetic {
		t.Fatalf("non-synthetic record must not set the synthetic flag")
	}
	// Anti-runaway only (the bug this guards — spin to the deadline then
	// error — is caught by the err check above); generous for slow runners.
	if time.Since(start) > 5*time.Second {
		t.Fatalf("body-less response capture should not hang, took %v", time.Since(start))
	}

	// Request present and decoded.
	if res.Request == nil {
		t.Fatalf("request block missing: %+v", res)
	}
	b, _ := json.Marshal(res.Request.Body)
	if !strings.Contains(string(b), `"model":"claude-x"`) {
		t.Fatalf("request body not captured: %s", b)
	}

	// Response present but explicitly absent.
	if res.Response == nil {
		t.Fatalf("response block should be present (absent), got nil")
	}
	if res.Response.BodyEncoding != "absent" {
		t.Fatalf("response body_encoding should be \"absent\", got %q", res.Response.BodyEncoding)
	}
	if res.Response.Body != nil {
		t.Fatalf("absent response must carry a nil body, got %#v", res.Response.Body)
	}
	if res.Response.Status != 200 {
		t.Fatalf("absent response should still carry the status, got %d", res.Response.Status)
	}
	joined := strings.Join(res.Meta.Notes, " ")
	if !strings.Contains(joined, "response body not captured") {
		t.Fatalf("expected an absent-response note, got %v", res.Meta.Notes)
	}
}

func TestRunCaptureLoop_TimeoutMentionsPath(t *testing.T) {
	srv := &recentServer{
		// No matching record ever appears.
		records: []model.RequestRecord{
			{Path: "/v1/other", LogicalTargetHost: "api.anthropic.com", StatusCode: 200,
				BodyRef: &model.RequestBodyRef{ID: "x"}},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{Response: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	start := time.Now()
	_, _, err := runCaptureLoop(ts.URL, opts, time.Now().Add(300*time.Millisecond), nil)
	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	// Anti-runaway only (the contract under test is the error + its
	// message); generous for slow runners.
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
	if !strings.Contains(err.Error(), "/v1/messages") {
		t.Fatalf("timeout error should mention the path: %v", err)
	}
}

func TestRunCaptureLoop_TLSOnly(t *testing.T) {
	srv := &recentServer{hello: undiciHello(t)}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	opts := captureOpts{TLSOnly: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	res, synthetic, err := runCaptureLoop(ts.URL, opts, time.Now().Add(2*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop --tls-only: %v", err)
	}
	if synthetic {
		t.Fatalf("tls-only must not report synthetic")
	}
	if res.TLS == nil || res.TLS.JA3 != undiciBaselineJA3 {
		t.Fatalf("tls-only JA3 mismatch: %+v", res.TLS)
	}
	if res.Request != nil || res.Response != nil {
		t.Fatalf("tls-only must emit only the TLS block: %+v", res)
	}
}

func TestRunCaptureLoop_ChildExitAborts(t *testing.T) {
	srv := &recentServer{
		// Never a match: the loop only stops because the child exited.
		records: []model.RequestRecord{},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	exited := make(chan struct{})
	close(exited) // child already gone

	opts := captureOpts{Response: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	start := time.Now()
	_, _, err := runCaptureLoop(ts.URL, opts, time.Now().Add(10*time.Second), exited)
	if err == nil {
		t.Fatalf("expected an error after child exit with no match")
	}
	// Should give up quickly after the child exits, well before the 10s
	// deadline — the bound IS the discriminator here (a loop that ignores
	// childExited only errors via the deadline), so it must stay clearly
	// under 10s; 5s leaves slack for a starved runner.
	if time.Since(start) > 5*time.Second {
		t.Fatalf("child-exit abort took too long: %v", time.Since(start))
	}
}

// TestRunCaptureLoop_MatesWithRealSupervisor is the mating proof between the
// capture poller and the real per-session supervisor surface. The
// TestRunCaptureLoop_* gates above pin the poller against an httptest
// stand-in, which shares wire TYPES with the supervisor (internal/model) but
// not SEMANTICS — if the recording behavior behind /recent drifts (when
// BodyRef is set, how spilled bodies are served, what logical host/path a
// record carries), the stand-in keeps passing while real `ccwrap capture`
// breaks. This test closes that gap without the launcher scaffolding the
// full E2E below needs: it boots a real supervisor in-process, drives a real
// MITM round-trip through the session proxy (the same fixture
// internal/supervisor's proxy_test locks), then points runCaptureLoop at the
// session's REAL listener — the exact base URL captureCommand derives.
func TestRunCaptureLoop_MatesWithRealSupervisor(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "m.sock")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
	}))
	defer upstream.Close()

	srv, err := supervisor.New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	// Deterministic teardown (LIFO: before cancel): the supervisor stops
	// writing before ShortAppPaths' cleanup removes its directories.
	defer func() { _ = srv.Shutdown(context.Background()) }()
	client := control.NewClient(paths.SocketPath)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := waitForControl(waitCtx, client); err != nil {
		t.Fatal(err)
	}

	sess, err := client.CreateSession(context.Background(),
		model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "capture-mating"})
	if err != nil {
		t.Fatal(err)
	}
	up, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: up.Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		// The initial /route publish is authoritative for the launch-scoped
		// toggles (setRoute takes the live half straight from the request), so
		// body capture is enabled HERE — mirroring how captureCommand's
		// CaptureBodies launch flag reaches the session. A pre-route
		// SetCaptureBodies call would be clobbered by this publish, by design.
		CaptureRequestBodies: true,
		OverrideAuth: &model.AuthOverride{
			Mode:        model.AuthModeOverrideXAPIKey,
			Source:      model.AuthSourceAnthropicAPIKey,
			HeaderName:  "X-API-Key",
			HeaderValue: "sekret",
		},
	}); err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pem, err := os.ReadFile(paths.CABundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("append CA bundle")
	}
	proxyURL, err := url.Parse("http://" + sess.ProxyListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-x","max_tokens":7}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected proxy round-trip status: %d", resp.StatusCode)
	}

	base := "http://" + sess.ProxyListenAddr
	opts := captureOpts{Response: true, Host: "api.anthropic.com", Path: "/v1/messages"}
	res, synthetic, err := runCaptureLoop(base, opts, time.Now().Add(10*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop against the real supervisor: %v", err)
	}
	if synthetic {
		t.Fatal("a real MITM round-trip must not be reported synthetic")
	}
	if res.Request == nil || res.Request.Host != "api.anthropic.com" || res.Request.Path != "/v1/messages" {
		t.Fatalf("request block mismatch: %+v", res.Request)
	}
	// Content-exact equality on the request body: capture stores the decoded
	// value, so this proves the value round-trips losslessly through the real
	// tee/bodystore/spill/serve path (re-marshal sorts keys, so the literal is
	// key-sorted — this is value equality, not original-byte preservation). A
	// Contains check would let truncation or a masking mutation slide.
	b, _ := json.Marshal(res.Request.Body)
	if string(b) != `{"max_tokens":7,"model":"claude-x"}` {
		t.Fatalf("request body did not survive the real record/spill/poll path: %s", b)
	}
	if res.Response == nil || res.Response.BodyEncoding != "sse" || res.Response.Status != 200 {
		t.Fatalf("response block mismatch: %+v", res.Response)
	}
	// Byte-exact on the response: SSE is carried as an opaque string, so this
	// is true byte-for-byte preservation through the real tee/spill/poll path.
	if got, ok := res.Response.Body.(string); !ok || got != "event: message_start\ndata: {}\n\n" {
		t.Fatalf("response body did not survive the real tee/spill/poll path: %#v", res.Response.Body)
	}
}

// TestCaptureCommand_FullLauncherE2E is the deferred end-to-end gate that would
// run captureCommand with a real fake-claude --claude-bin that POSTs
// /v1/messages through the injected HTTPS_PROXY (trusting NODE_EXTRA_CA_CERTS),
// with ccwrap's upstream egress pointed at a local SSE origin.
//
// It is skipped because the required TLS/CA scaffolding is not reachable from
// package main:
//
//   - api.anthropic.com (the default ExactUpstreamHost) must be redirected to a
//     local SSE origin. The launcher has no hook to remap the exact upstream
//     host to a loopback origin at capture time.
//   - The egress transport must trust the local origin's throwaway CA. The
//     trust-injection hooks used by the supervisor's own E2E
//     (nativeRootsForTest, forceNativeTLSFail in internal/supervisor) are
//     package-private and not exported across the supervisor boundary.
//   - The route must resolve to a non-synthetic auth posture so the proxy
//     performs a real MITM round-trip rather than emitting the auth-missing
//     502 short-circuit.
//
// The deterministic acceptance gates for the orchestration are
// TestRunCaptureLoop_* above (polling/assembly against an httptest stand-in)
// plus TestRunCaptureLoop_MatesWithRealSupervisor (the same poller against a
// REAL in-process supervisor and a real MITM round-trip, so /recent recording
// semantics cannot drift past the stand-in unnoticed). The only ground left
// to this E2E is the launcher handoff itself — fake-claude spawn plus the
// HTTPS_PROXY / NODE_EXTRA_CA_CERTS env injection. Landing it would require
// exporting a test-only upstream-remap + egress-trust seam from
// internal/supervisor.
func TestCaptureCommand_FullLauncherE2E(t *testing.T) {
	t.Skip("full fake-claude launcher E2E deferred: needs an exported upstream-remap + egress-trust test seam from internal/supervisor (see doc comment); runCaptureLoop httptest gate is the primary acceptance test")
}
