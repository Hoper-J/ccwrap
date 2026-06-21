package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
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
	// Generous deadline; a synthetic match must return immediately, not wait it out.
	res, synthetic, err := runCaptureLoop(ts.URL, opts, time.Now().Add(5*time.Second), nil)
	if err != nil {
		t.Fatalf("runCaptureLoop: %v", err)
	}
	if !synthetic {
		t.Fatalf("synthetic record must set the synthetic flag")
	}
	if time.Since(start) > 2*time.Second {
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
	if time.Since(start) > 2*time.Second {
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
	if time.Since(start) > 2*time.Second {
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
	// Should give up quickly after the child exits, well before the deadline.
	if time.Since(start) > 3*time.Second {
		t.Fatalf("child-exit abort took too long: %v", time.Since(start))
	}
}

// ensure url import is used even if a future edit drops the only reference.
var _ = url.QueryEscape

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
// The deterministic acceptance gate for the orchestration is
// TestRunCaptureLoop_* above, which exercises the exact polling/assembly logic
// against an httptest stand-in for the session proxy without needing real
// TLS/CA/launch. Landing the full launcher E2E would require exporting a
// test-only upstream-remap + egress-trust seam from internal/supervisor.
func TestCaptureCommand_FullLauncherE2E(t *testing.T) {
	t.Skip("full fake-claude launcher E2E deferred: needs an exported upstream-remap + egress-trust test seam from internal/supervisor (see doc comment); runCaptureLoop httptest gate is the primary acceptance test")
}
