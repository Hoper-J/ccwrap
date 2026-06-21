package supervisor

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// streamingCaptureSession wires a session proxy to a CALLER-SUPPLIED upstream
// with request+response body capture ON, returning a proxy-routed HTTPS client.
// Unlike headerInspectorSession it lets the test own the upstream handler so it
// can stream and barrier the response. Default route class is third-party-hidden.
func streamingCaptureSession(t *testing.T, sockName string, upstream *httptest.Server) (*control.Client, *model.Session, *http.Client) {
	t.Helper()
	return streamingCaptureSessionClass(t, sockName, upstream, model.RouteClassThirdPartyHidden)
}

// streamingCaptureSessionClass is streamingCaptureSession with an explicit route
// class. First-party is required to exercise OAuth credential PATHS: under a
// third-party class, handleThirdPartySyntheticOrBlock synthesizes a 204 for
// non-gateway paths like /v1/oauth/token, so they never reach the upstream/tee.
func streamingCaptureSessionClass(t *testing.T, sockName string, upstream *httptest.Server, class model.RouteClass) (*control.Client, *model.Session, *http.Client) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, sockName)
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "respcap"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           class,
		RouteSource:          model.RouteSourceExplicit,
		AuthMode:             model.AuthModePassthrough,
		AuthSource:           model.AuthSourceNone,
		ExactUpstreamHost:    mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase:    upstream.URL,
		FailPolicy:           model.FailClosed,
		CaptureRequestBodies: true,
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
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	return client, sess, hc
}

// TestAnthropicMITMStreamsResponseBodyWithoutBufferingAndCaptures proves the
// response-body tap on the Anthropic MITM path does NOT buffer: the upstream
// streams "Hel", flushes, then BLOCKS; the client must receive "Hel" before the
// barrier is released (a ReadAll-style buffer would stall here and the read
// would time out). After the stream completes, the full response body "Hello"
// must be captured to ResponseBodyRef and spilled to disk.
func TestAnthropicMITMStreamsResponseBodyWithoutBufferingAndCaptures(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "Hel")
		fl.Flush()
		<-release // hold the stream open until the test confirms the client got "Hel"
		_, _ = io.WriteString(w, "lo")
		fl.Flush()
	}))
	defer upstream.Close()

	client, sess, hc := streamingCaptureSession(t, "respcap.sock", upstream)

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
	req.Header.Set("Content-Type", "application/json")

	var resp *http.Response
	firstCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := hc.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		resp = r
		b := make([]byte, 3)
		if _, err := io.ReadFull(r.Body, b); err != nil {
			errCh <- err
			return
		}
		firstCh <- b
	}()

	select {
	case b := <-firstCh:
		if string(b) != "Hel" {
			close(release)
			t.Fatalf("first streamed chunk = %q, want \"Hel\"", b)
		}
	case err := <-errCh:
		close(release)
		t.Fatalf("request/first-read failed: %v", err)
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("client did not receive the first streamed chunk before the upstream completed — response was buffered (streaming broken)")
	}
	close(release)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading rest of body: %v", err)
	}
	_ = resp.Body.Close()
	if got := "Hel" + string(rest); got != "Hello" {
		t.Fatalf("full client-visible body = %q, want %q", got, "Hello")
	}

	// The record + spill are produced after ServeHTTP returns; poll briefly.
	var rec *model.RequestRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests, err := client.Requests(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := range requests {
			if requests[i].Method == http.MethodPost && strings.Contains(requests[i].Path, "/v1/messages") {
				rec = &requests[i]
			}
		}
		if rec != nil && rec.ResponseBodyRef != nil {
			break
		}
		rec = nil
		time.Sleep(10 * time.Millisecond)
	}
	if rec == nil {
		t.Fatal("no /v1/messages record with a ResponseBodyRef appeared")
	}
	if rec.ResponseBodyRef.Size != int64(len("Hello")) {
		t.Fatalf("ResponseBodyRef.Size = %d, want %d", rec.ResponseBodyRef.Size, len("Hello"))
	}

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	respPath := filepath.Join(status.RuntimeDir, "bodies", rec.ResponseBodyRef.ID+".json")
	if got := pollBodyFile(t, respPath); string(got) != "Hello" {
		t.Fatalf("spilled response body = %q, want %q", got, "Hello")
	}
}

// TestAnthropicMITMResponseCaptureTruncatesAtCapButStreamsFullBody proves the
// capture cap bounds only the SPILL, never the client stream: with the cap set
// below the response size, the client still receives the FULL "Hello" while the
// captured copy is truncated to the cap and flagged Truncated.
func TestAnthropicMITMResponseCaptureTruncatesAtCapButStreamsFullBody(t *testing.T) {
	orig := responseBodyCapBytes
	responseBodyCapBytes = 4
	defer func() { responseBodyCapBytes = orig }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "Hello")
	}))
	defer upstream.Close()

	client, sess, hc := streamingCaptureSession(t, "respcap-trunc.sock", upstream)

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	_ = resp.Body.Close()
	if string(body) != "Hello" {
		t.Fatalf("client must receive the FULL body despite the capture cap: got %q, want %q", body, "Hello")
	}

	var rec *model.RequestRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests, err := client.Requests(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := range requests {
			if requests[i].Method == http.MethodPost && strings.Contains(requests[i].Path, "/v1/messages") {
				rec = &requests[i]
			}
		}
		if rec != nil && rec.ResponseBodyRef != nil {
			break
		}
		rec = nil
		time.Sleep(10 * time.Millisecond)
	}
	if rec == nil {
		t.Fatal("no /v1/messages record with a ResponseBodyRef appeared")
	}
	if !rec.ResponseBodyRef.Truncated {
		t.Error("ResponseBodyRef.Truncated = false, want true (response exceeded the cap)")
	}
	if rec.ResponseBodyRef.Size != 4 {
		t.Errorf("ResponseBodyRef.Size = %d, want 4 (capped)", rec.ResponseBodyRef.Size)
	}

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	respPath := filepath.Join(status.RuntimeDir, "bodies", rec.ResponseBodyRef.ID+".json")
	if got := pollBodyFile(t, respPath); string(got) != "Hell" {
		t.Fatalf("spilled (capped) response body = %q, want %q", got, "Hell")
	}
}

// TestAnthropicMITMCaptureDecodesCompressedSpillAndPreservesUpstreamEncoding
// proves capture does NOT alter the upstream request (full HTTP fidelity): the
// upstream still sees Claude's real Accept-Encoding (gzip), and the gzipped
// response is DECODED ccwrap-side so the spill is readable JSON.
func TestAnthropicMITMCaptureDecodesCompressedSpillAndPreservesUpstreamEncoding(t *testing.T) {
	const payload = `{"msg":"Hello"}`
	var gotAcceptEnc string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEnc = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(payload))
			_ = gz.Close()
			return
		}
		_, _ = io.WriteString(w, payload)
	}))
	defer upstream.Close()

	client, sess, hc := streamingCaptureSession(t, "respcap-gzip.sock", upstream)

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip") // Claude Code asks for gzip
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Fidelity: ccwrap must NOT have rewritten Accept-Encoding to identity.
	if !strings.Contains(gotAcceptEnc, "gzip") {
		t.Fatalf("upstream Accept-Encoding = %q, want it to KEEP Claude's gzip (capture must not alter the upstream request)", gotAcceptEnc)
	}

	var rec *model.RequestRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests, err := client.Requests(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := range requests {
			if requests[i].Method == http.MethodPost && strings.Contains(requests[i].Path, "/v1/messages") {
				rec = &requests[i]
			}
		}
		if rec != nil && rec.ResponseBodyRef != nil {
			break
		}
		rec = nil
		time.Sleep(10 * time.Millisecond)
	}
	if rec == nil {
		t.Fatal("no /v1/messages record with a ResponseBodyRef appeared")
	}

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	respPath := filepath.Join(status.RuntimeDir, "bodies", rec.ResponseBodyRef.ID+".json")
	if got := pollBodyFile(t, respPath); string(got) != payload {
		t.Fatalf("spilled response body = %q, want the DECODED payload %q (capture must decode gzip ccwrap-side, not store wire bytes)", got, payload)
	}
}

// oauthRespRecord drives a POST to an OAuth credential PATH (/v1/oauth/token,
// which makes shouldRedactBody true) through the capture proxy to a stub that
// returns respJSON, and returns the captured ResponseBodyRef record + the raw
// body the CLIENT received. Shared by the OAuth-response redaction tests.
func oauthRespRecord(t *testing.T, sockName, respJSON string) (*control.Client, *model.Session, *model.RequestRecord, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respJSON)
	}))
	t.Cleanup(upstream.Close)

	// First-party route: under a third-party class the /v1/oauth/token path is
	// synthesized (204) and never forwarded/captured.
	client, sess, hc := streamingCaptureSessionClass(t, sockName, upstream, model.RouteClassFirstParty)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/oauth/token", strings.NewReader(`{"grant_type":"refresh_token"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("oauth request through proxy failed: %v", err)
	}
	clientBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var rec *model.RequestRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests, err := client.Requests(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := range requests {
			if requests[i].Method == http.MethodPost && strings.Contains(requests[i].Path, "/oauth/") {
				rec = &requests[i]
			}
		}
		if rec != nil && rec.ResponseBodyRef != nil {
			break
		}
		rec = nil
		time.Sleep(10 * time.Millisecond)
	}
	if rec == nil {
		t.Fatal("no /oauth/ record with a ResponseBodyRef appeared")
	}
	return client, sess, rec, string(clientBody)
}

func oauthSpillPath(t *testing.T, client *control.Client, rec *model.RequestRecord) string {
	t.Helper()
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(status.RuntimeDir, "bodies", rec.ResponseBodyRef.ID+".json")
}

// A complete OAuth token RESPONSE carrying access_token/refresh_token must be
// forwarded RAW to the client but MASKED in the on-disk spill.
func TestAnthropicMITMOAuthResponseRedactsCredentialFieldsInSpill(t *testing.T) {
	const accessTok = "sk-ant-oat01-ACCESSSECRET"
	const refreshTok = "sk-ant-ort01-REFRESHSECRET"
	respJSON := `{"access_token":"` + accessTok + `","refresh_token":"` + refreshTok + `","token_type":"Bearer","expires_in":3600}`

	client, _, rec, clientBody := oauthRespRecord(t, "respcap-oauth-mask.sock", respJSON)

	// Forward unchanged: the real client gets the RAW tokens.
	if !strings.Contains(clientBody, accessTok) || !strings.Contains(clientBody, refreshTok) {
		t.Fatalf("client must receive the raw upstream body unchanged; got %q", clientBody)
	}
	spill := string(pollBodyFile(t, oauthSpillPath(t, client, rec)))
	if strings.Contains(spill, accessTok) || strings.Contains(spill, refreshTok) {
		t.Fatalf("SPILL leaked a raw credential: %s", spill)
	}
	if !strings.Contains(spill, "‹redacted by ccwrap; sha256:") {
		t.Fatalf("spill must carry the redaction sentinel; got %s", spill)
	}
	if !strings.Contains(spill, `"expires_in":3600`) {
		t.Errorf("non-sensitive fields should be preserved; got %s", spill)
	}
}

// A truncated OAuth response (cap cuts mid-token) is INVALID JSON on a
// credential host → redaction fails CLOSED: the spill is the withheld sentinel,
// never a raw token prefix, while the client still gets the FULL body.
func TestAnthropicMITMOAuthResponseTruncatedFailsClosed(t *testing.T) {
	orig := responseBodyCapBytes
	responseBodyCapBytes = 25 // `{"access_token":"SUPERSEC` — includes a secret prefix
	defer func() { responseBodyCapBytes = orig }()

	const accessTok = "SUPERSECRETtokenvalue123"
	respJSON := `{"access_token":"` + accessTok + `"}`

	client, _, rec, clientBody := oauthRespRecord(t, "respcap-oauth-trunc.sock", respJSON)

	if clientBody != respJSON {
		t.Fatalf("client must receive the FULL untruncated body; got %q", clientBody)
	}
	spill := string(pollBodyFile(t, oauthSpillPath(t, client, rec)))
	if strings.Contains(spill, "SUPERSEC") {
		t.Fatalf("fail-closed must withhold the truncated token prefix; spill leaked it: %s", spill)
	}
	if !strings.Contains(spill, "withheld from capture (fail-closed redaction)") {
		t.Fatalf("spill must be the fail-closed sentinel; got %s", spill)
	}
}

// CCWRAP_UNMASK_CREDENTIALS=1 deliberately writes the OAuth response token in
// cleartext — this locks the escape hatch the server.go warning is about.
func TestAnthropicMITMOAuthResponseUnmaskedWritesCleartext(t *testing.T) {
	t.Setenv("CCWRAP_UNMASK_CREDENTIALS", "1")
	const accessTok = "sk-ant-oat01-UNMASKEDSECRET"
	respJSON := `{"access_token":"` + accessTok + `"}`

	client, _, rec, _ := oauthRespRecord(t, "respcap-oauth-unmask.sock", respJSON)

	spill := string(pollBodyFile(t, oauthSpillPath(t, client, rec)))
	if !strings.Contains(spill, accessTok) {
		t.Fatalf("with unmask on, the spill must contain the cleartext token; got %s", spill)
	}
}
