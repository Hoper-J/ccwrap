package supervisor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestTelemetryPinnedSet(t *testing.T) {
	sp := &sessionProxy{}
	if sp.telemetryPinned("http-intake.logs.us5.datadoghq.com") {
		t.Fatal("fresh sessionProxy should have no pinned telemetry hosts")
	}
	sp.markTelemetryPinned("http-intake.logs.us5.datadoghq.com")
	if !sp.telemetryPinned("http-intake.logs.us5.datadoghq.com") {
		t.Error("host should be pinned after markTelemetryPinned")
	}
	if sp.telemetryPinned("anthropic.sentry.io") {
		t.Error("unrelated host must not be pinned")
	}
}

// TestTelemetryMITMCapturesRequestAndResponseBodies drives a CONNECT to an
// allowlisted telemetry host through the session proxy with capture enabled.
// The transparent MITM must terminate TLS with ccwrap's cert, forward the
// request AND response UNCHANGED to the (test-redirected) upstream, and spill
// BOTH the request body (BodyRef) and the response body (ResponseBodyRef).
func TestTelemetryMITMCapturesRequestAndResponseBodies(t *testing.T) {
	var gotReqBody []byte
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"received"}`))
	}))
	defer stub.Close()

	// Redirect the transparent telemetry forward to the local stub (prod ->
	// https://<host>).
	testHookTelemetryUpstream = func(string) string { return stub.URL }
	defer func() { testHookTelemetryUpstream = nil }()

	srv, client, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "tele-capture.sock")
	defer upstream.Close()

	// Enable telemetry capture on the live posture (control-plane wiring lands
	// in a later task) by cloning the immutable posture and re-publishing.
	state := srv.getSession(sess.ID)
	ap := state.active.Load()
	apCopy := *ap
	apCopy.l.captureTelemetry = true
	state.active.Store(&apCopy)

	reqBody := []byte(`{"event":"hello-telemetry","n":1}`)
	req, _ := http.NewRequest(http.MethodPost, "https://http-intake.logs.us5.datadoghq.com/api/v2/logs", bytes.NewReader(reqBody))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("telemetry request through proxy failed: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Forwarded UNCHANGED both ways.
	if string(respBody) != `{"status":"received"}` {
		t.Errorf("client response = %q, want the stub's body unchanged", respBody)
	}
	if string(gotReqBody) != string(reqBody) {
		t.Errorf("stub received request body %q, want %q (unchanged)", gotReqBody, reqBody)
	}

	recs, err := client.Requests(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rec *model.RequestRecord
	for i := range recs {
		if recs[i].LogicalTargetHost == "http-intake.logs.us5.datadoghq.com" && recs[i].Method == http.MethodPost {
			rec = &recs[i]
		}
	}
	if rec == nil {
		t.Fatalf("no telemetry POST record found: %#v", recs)
	}
	if rec.BodyRef == nil {
		t.Error("request body was not captured (BodyRef nil)")
	}
	if rec.ResponseBodyRef == nil {
		t.Error("response body was not captured (ResponseBodyRef nil)")
	}
	if t.Failed() {
		return
	}

	// Derive the spill path from the supervisor's per-session RuntimeDir, then
	// poll for the async writer (mirrors TestRequestBodyCapturedToFileWhenEnabled).
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reqPath := filepath.Join(status.RuntimeDir, "bodies", rec.BodyRef.ID+".json")
	respPath := filepath.Join(status.RuntimeDir, "bodies", rec.ResponseBodyRef.ID+".json")

	if got := pollBodyFile(t, reqPath); string(got) != string(reqBody) {
		t.Errorf("spilled request body = %q, want %q", got, reqBody)
	}
	if got := pollBodyFile(t, respPath); string(got) != `{"status":"received"}` {
		t.Errorf("spilled response body = %q, want %q", got, `{"status":"received"}`)
	}
}

// TestTelemetryMITMRedactsCredentialFieldsInSpill drives a telemetry request
// whose body carries a credential-named field (access_token) end-to-end. The
// upstream stub MUST receive the RAW token (forward unchanged), but the SPILLED
// request-body file MUST mask it with the SHA sentinel -- redaction affects
// only ccwrap's on-disk observability surface.
func TestTelemetryMITMRedactsCredentialFieldsInSpill(t *testing.T) {
	const secret = "sk-tele-SUPERSECRET"
	var gotReqBody []byte
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"received"}`))
	}))
	defer stub.Close()

	testHookTelemetryUpstream = func(string) string { return stub.URL }
	defer func() { testHookTelemetryUpstream = nil }()

	srv, client, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "tele-redact.sock")
	defer upstream.Close()

	state := srv.getSession(sess.ID)
	ap := state.active.Load()
	apCopy := *ap
	apCopy.l.captureTelemetry = true
	state.active.Store(&apCopy)

	reqBody := []byte(`{"access_token":"` + secret + `","event":"hello"}`)
	req, _ := http.NewRequest(http.MethodPost, "https://http-intake.logs.us5.datadoghq.com/api/v2/logs", bytes.NewReader(reqBody))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("telemetry request through proxy failed: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Upstream stub received the RAW body unchanged.
	if string(gotReqBody) != string(reqBody) {
		t.Errorf("stub received %q, want raw unchanged body %q", gotReqBody, reqBody)
	}

	recs, err := client.Requests(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rec *model.RequestRecord
	for i := range recs {
		if recs[i].LogicalTargetHost == "http-intake.logs.us5.datadoghq.com" && recs[i].Method == http.MethodPost {
			rec = &recs[i]
		}
	}
	if rec == nil || rec.BodyRef == nil {
		t.Fatalf("no telemetry POST record with BodyRef: %#v", recs)
	}

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reqPath := filepath.Join(status.RuntimeDir, "bodies", rec.BodyRef.ID+".json")
	got := pollBodyFile(t, reqPath)

	// Spilled body masks the credential.
	if bytes.Contains(got, []byte(secret)) {
		t.Errorf("spilled request body leaked the credential: %s", got)
	}
	if !bytes.Contains(got, []byte("‹redacted by ccwrap; sha256:")) {
		t.Errorf("spilled request body missing redaction sentinel: %s", got)
	}
	// Non-sensitive field preserved.
	if !bytes.Contains(got, []byte(`"event":"hello"`)) {
		t.Errorf("non-sensitive event field lost in spill: %s", got)
	}
}

func pollBodyFile(t *testing.T, path string) []byte {
	t.Helper()
	// Skip empty reads, not just missing files. bodyStore.put now publishes
	// atomically (temp+rename), so a reader should never observe an empty file;
	// this stays as defensive belt-and-suspenders, since no spill is ever
	// legitimately zero-length (put is only called for non-empty bodies). Treat
	// empty as "not written yet" and keep polling.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) > 0 {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("spilled body file never appeared (or stayed empty) at %s", path)
	return nil
}
