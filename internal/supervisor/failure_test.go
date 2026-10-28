package supervisor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

func TestErrorBodyMessage(t *testing.T) {
	cases := []struct {
		name, body, typ, msg string
	}{
		{"anthropic", `{"type":"error","error":{"type":"not_found_error","message":"model: claude-fable-5-1"}}`, "not_found_error", "model: claude-fable-5-1"},
		{"openai", `{"error":{"message":"The model does not exist","type":"invalid_request_error","code":"model_not_found"}}`, "invalid_request_error", "The model does not exist"},
		{"error string", `{"error":"upstream timeout"}`, "", "upstream timeout"},
		{"message only", `{"message":"Service temporarily unavailable"}`, "", "Service temporarily unavailable"},
		{"cloudflare problem json", `{"type":"https://developers.cloudflare.com/errors/502/","title":"Error 502: Bad gateway","status":502,"detail":"The origin web server returned an invalid or incomplete response to Cloudflare."}`, "", "Error 502: Bad gateway: The origin web server returned an invalid or incomplete response to Cloudflare."},
		{"html", "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>", "", "502 Bad Gateway"},
		{"plain text", "404 page not found\n", "", "404 page not found"},
		{"empty", "  ", "", ""},
	}
	for _, tc := range cases {
		typ, msg := errorBodyMessage([]byte(tc.body))
		if typ != tc.typ || msg != tc.msg {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, typ, msg, tc.typ, tc.msg)
		}
	}
}

func TestUpstreamFailureMasksEchoedKeys(t *testing.T) {
	body := []byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key: sk-ant-api03-abcdefghijklmnop"}}`)
	f := upstreamFailure(failureContext{status: 401, profile: "gw"}, body, false, false)
	if strings.Contains(f.Message, "abcdefghijklmnop") || strings.Contains(f.Body, "abcdefghijklmnop") {
		t.Fatalf("key leaked: message=%q body=%q", f.Message, f.Body)
	}
	if !strings.Contains(f.Hint, "profile gw") {
		t.Fatalf("hint = %q", f.Hint)
	}
	if f := upstreamFailure(failureContext{status: 401}, body, false, true); !strings.Contains(f.Body, "abcdefghijklmnop") {
		t.Fatal("unmask must keep the body verbatim")
	}
}

func TestFailureHint(t *testing.T) {
	unmapped := failureContext{status: 404, unmapped: true, model: "claude-fable-5-1", profile: "gw", aliasTarget: "gpt-6"}
	if got := failureHint(unmapped, &model.RequestFailure{}); !strings.Contains(got, "ccwrap profile edit gw --model-alias 'claude-fable-5-1=gpt-6'") || !strings.Contains(got, "'*=gpt-6'") {
		t.Errorf("unmapped hint = %q", got)
	}
	if got := failureHint(failureContext{status: 401, firstParty: true}, &model.RequestFailure{}); !strings.Contains(got, "/login") {
		t.Errorf("first-party 401 hint = %q", got)
	}
	if got := failureHint(failureContext{status: 529}, &model.RequestFailure{}); !strings.Contains(got, "temporary") {
		t.Errorf("5xx hint = %q", got)
	}
	if got := failureHint(failureContext{status: 400}, &model.RequestFailure{}); got != "" {
		t.Errorf("plain 400 hint = %q, want none", got)
	}
}

// diagUpstream is a plain-HTTP gateway that serves gpt-6 and rejects every
// other model name, optionally gzip-compressing its error bodies.
func diagUpstream(t *testing.T, gzipErrors bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "gpt-6" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"type":"message","role":"assistant","model":"gpt-6","content":[]}`)
			return
		}
		body := []byte(fmt.Sprintf(`{"type":"error","error":{"type":"not_found_error","message":"model not found: %s"}}`, req.Model))
		w.Header().Set("Content-Type", "application/json")
		if gzipErrors {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(body)
			_ = zw.Close()
			body = buf.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func diagRoute(base string, aliases map[string]string) model.SessionRouteRequest {
	route := xAPIKeyRoute(base, model.EgressConfig{Mode: "direct"})
	route.ActiveProfileName = "gw"
	route.ModelAlias = model.ModelAliasConfig{Mode: model.ModelAliasRewrite, Strict: true, Forward: aliases}
	return route
}

// messagesClient sends /v1/messages requests the way Claude Code does
// (CONNECT + TLS to the MITM) and returns the status and decoded body.
func messagesClient(t *testing.T, caBundle string, sess *model.Session) func(modelID string) (int, string) {
	t.Helper()
	pool := x509.NewCertPool()
	caPEM, err := os.ReadFile(caBundle)
	if err != nil || !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("load ccwrap CA: %v", err)
	}
	return func(modelID string) (int, string) {
		t.Helper()
		raw, err := net.DialTimeout("tcp", sess.ProxyListenAddr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		fmt.Fprintf(raw, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
		br := bufio.NewReader(raw)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT: %v %v", resp, err)
		}
		conn := tls.Client(raw, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool, NextProtos: []string{"http/1.1"}})
		body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, modelID)
		fmt.Fprintf(conn, "POST /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Type: application/json\r\nAuthorization: Bearer placeholder\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r io.Reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			if r, err = gzip.NewReader(resp.Body); err != nil {
				t.Fatal(err)
			}
		}
		out, _ := io.ReadAll(r)
		return resp.StatusCode, string(out)
	}
}

func lastRequest(t *testing.T, client *control.Client, sessionID string) model.RequestRecord {
	t.Helper()
	var recs []model.RequestRecord
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recs, _ = client.Requests(context.Background(), sessionID)
		if len(recs) > 0 {
			return recs[len(recs)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no request recorded")
	return model.RequestRecord{}
}

// Through the real supervisor: every request records its model view; an
// unaliased model's upstream error is kept (decoded, with a hint) without
// body capture, folds into one warn-level error entry, counts as failed, and
// a later success heals the session.
func TestSessionProxyRecordsModelsAndUpstreamFailures(t *testing.T) {
	for _, gz := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%v", gz), func(t *testing.T) {
			up := diagUpstream(t, gz)
			paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"}))
			client := control.NewClient(paths.SocketPath)
			send := messagesClient(t, paths.CABundlePath, sess)

			if status, body := send("claude-opus-5-5"); status != 200 || !strings.Contains(body, `"claude-opus-5-5"`) {
				t.Fatalf("aliased request: %d %s", status, body)
			}
			ok := lastRequest(t, client, sess.ID)
			if ok.Model != "claude-opus-5-5" || ok.UpstreamModel != "gpt-6" || ok.ModelUnmapped || ok.Failure != nil {
				t.Fatalf("aliased record: %+v", ok)
			}

			for i := 0; i < 2; i++ {
				if status, _ := send("claude-fable-5-1"); status != 404 {
					t.Fatalf("unaliased request status = %d", status)
				}
			}
			rec := lastRequest(t, client, sess.ID)
			if rec.Model != "claude-fable-5-1" || rec.UpstreamModel != "claude-fable-5-1" || !rec.ModelUnmapped {
				t.Fatalf("unaliased record model view: %+v", rec)
			}
			f := rec.Failure
			if f == nil || f.Source != "upstream" || f.Type != "not_found_error" || f.Message != "model not found: claude-fable-5-1" {
				t.Fatalf("failure = %+v", f)
			}
			if !strings.Contains(f.Hint, "ccwrap profile edit gw --model-alias 'claude-fable-5-1=gpt-6'") {
				t.Fatalf("hint = %q", f.Hint)
			}
			if rec.BodyRef != nil || rec.ResponseBodyRef != nil {
				t.Fatal("the failure must not depend on body capture")
			}

			errs, err := client.Errors(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(errs) != 1 || errs[0].ErrorClass != "upstream_http_404" || errs[0].Severity != "warn" || errs[0].Count != 2 {
				t.Fatalf("errors = %+v", errs)
			}
			got, err := client.GetSession(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.RecentFailedCount != 2 || got.Health != model.HealthWarn {
				t.Fatalf("after failures: failed=%d health=%s", got.RecentFailedCount, got.Health)
			}

			// A later success does not alias the model: its issue stays open,
			// and so does the warning.
			send("claude-opus-5-5")
			got, _ = client.GetSession(context.Background(), sess.ID)
			if got.Health != model.HealthWarn || got.RecentFailedCount != 2 {
				t.Fatalf("after success: failed=%d health=%s", got.RecentFailedCount, got.Health)
			}
			if len(got.Issues) != 1 || got.Issues[0].ID != model.IssueUnaliasedModel || got.Issues[0].State != model.IssueOpen ||
				got.Issues[0].Evidence != "404 model not found: claude-fable-5-1 · 2 requests" {
				t.Fatalf("issues = %+v", got.Issues)
			}
		})
	}
}

// The default alias maps every other Claude model, and each response comes
// back under the model its own request named.
func TestSessionProxyDefaultAliasMapsOtherModels(t *testing.T) {
	up := diagUpstream(t, false)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6", modelalias.DefaultKey: "gpt-6"}))
	client := control.NewClient(paths.SocketPath)
	send := messagesClient(t, paths.CABundlePath, sess)
	status, body := send("claude-fable-5-1")
	if status != 200 || !strings.Contains(body, `"claude-fable-5-1"`) {
		t.Fatalf("default-mapped request: %d %s", status, body)
	}
	rec := lastRequest(t, client, sess.ID)
	if rec.Model != "claude-fable-5-1" || rec.UpstreamModel != "gpt-6" || rec.ModelUnmapped || rec.Failure != nil {
		t.Fatalf("record: %+v", rec)
	}
}

// With no upstream response, the failure is ccwrap's own and the error list
// keeps only ccwrap's entry.
func TestSessionProxyRecordsUnreachableUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()
	paths, sess := routedSessionPaths(t, diagRoute(dead, map[string]string{"claude-opus-5-5": "gpt-6"}))
	client := control.NewClient(paths.SocketPath)
	if status, _ := messagesClient(t, paths.CABundlePath, sess)("claude-opus-5-5"); status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	rec := lastRequest(t, client, sess.ID)
	if rec.Failure == nil || rec.Failure.Source != "ccwrap" || rec.Failure.Message == "" {
		t.Fatalf("failure = %+v", rec.Failure)
	}
	errs, _ := client.Errors(context.Background(), sess.ID)
	for _, e := range errs {
		if strings.HasPrefix(e.ErrorClass, upstreamHTTPErrorClassPrefix) {
			t.Fatalf("unexpected upstream entry: %+v", e)
		}
	}
	got, _ := client.GetSession(context.Background(), sess.ID)
	if got.RecentFailedCount != 1 || got.Health != model.HealthError {
		t.Fatalf("failed=%d health=%s", got.RecentFailedCount, got.Health)
	}
}

// routedSessionPaths is routedSession that also returns the paths, for tests
// that read records back over the control socket.
func routedSessionPaths(t *testing.T, route model.SessionRouteRequest) (app.Paths, *model.Session) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, "f.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "diag"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, route); err != nil {
		t.Fatal(err)
	}
	return paths, sess
}

// A base URL ending in /v1 doubles the version segment; the 404 that follows
// gets a hint with the corrected base, and its plain-text body is shown.
func TestSessionProxyHintsDoubledV1Base(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","model":"gpt-6","content":[]}`)
	}))
	t.Cleanup(up.Close)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL+"/v1", map[string]string{"claude-opus-5-5": "gpt-6"}))
	client := control.NewClient(paths.SocketPath)
	if status, _ := messagesClient(t, paths.CABundlePath, sess)("claude-opus-5-5"); status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	f := lastRequest(t, client, sess.ID).Failure
	if f == nil || f.Message != "404 page not found" || !strings.Contains(f.Body, "404 page not found") {
		t.Fatalf("failure = %+v", f)
	}
	if want := "ccwrap profile edit gw --base-url " + up.URL; !strings.Contains(f.Hint, want) || !strings.Contains(f.Hint, "/v1/v1/messages") {
		t.Fatalf("hint = %q, want it to contain %q", f.Hint, want)
	}
}
