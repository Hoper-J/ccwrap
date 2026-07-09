package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

func TestSessionProxyRoutesAndOverridesAuth(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var gotPath string
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("X-Api-Key")
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL + "/prefix",
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL + "/prefix",
		FailPolicy:        model.FailClosed,
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
	proxyURL := mustParse(t, "http://"+sess.ProxyListenAddr)
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?x=1", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer should-be-removed")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if gotPath != "/prefix/v1/messages?x=1" {
		t.Fatalf("unexpected upstream path: %s", gotPath)
	}
	if gotAuth != "sekret" {
		t.Fatalf("unexpected auth override: %q", gotAuth)
	}
	requests := waitForRequestRecord(t, client, sess.ID, "forwarded /v1/messages request",
		func(rec model.RequestRecord) bool {
			return rec.Method == http.MethodPost && strings.Contains(rec.Path, "/v1/messages")
		})
	if requests[len(requests)-1].ActualUpstreamHost != mustParse(t, upstream.URL).Hostname() {
		t.Fatalf("unexpected actual upstream host: %s", requests[len(requests)-1].ActualUpstreamHost)
	}
	if requests[len(requests)-1].Path != "/v1/messages?x=1" {
		t.Fatalf("unexpected logical path: %s", requests[len(requests)-1].Path)
	}
}

func waitForSupervisor(t *testing.T, client *control.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Status(context.Background()); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("session supervisor did not become ready")
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSessionProxyInfoEndpoints(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "info"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	base := "http://" + sess.ProxyListenAddr
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Pause stream") {
		t.Fatalf("expected live mode control; got: %s", string(body))
	}
	if !strings.Contains(string(body), "connecting") {
		t.Fatalf("expected live mode status UI; got: %s", string(body))
	}
	if !strings.Contains(string(body), "Activity") {
		t.Fatalf("expected activity panel; got: %s", string(body))
	}
	// The "Last activity" cell label lives in the hero sub-line as inline
	// lowercase text ("last activity"). The Time column in the activity
	// table remains unchanged.
	if !strings.Contains(string(body), "last activity") || !strings.Contains(string(body), ">Time<") {
		t.Fatalf("expected hero last-activity sub-line and time column; got: %s", string(body))
	}
	if strings.Contains(string(body), ">Scope<") {
		t.Fatalf("unexpected scope cell on single-session page; got: %s", string(body))
	}
	if strings.Contains(string(body), "session-table") || strings.Contains(string(body), ">Current route<") {
		t.Fatalf("unexpected legacy route table on session page; got: %s", string(body))
	}
	if strings.Contains(string(body), ">Proxy<") || strings.Contains(string(body), ">Fail<") {
		t.Fatalf("unexpected low-value summary item on session page; got: %s", string(body))
	}
	if strings.Contains(string(body), "Known limitations") || strings.Contains(string(body), "Child process attribution is best-effort") {
		t.Fatalf("unexpected static limitations copy on proxy page; got: %s", string(body))
	}
	if strings.Contains(string(body), "<h2>Session</h2>") {
		t.Fatalf("unexpected duplicate session section heading on proxy page; got: %s", string(body))
	}
	if strings.Contains(string(body), "Incremental refresh keeps recent history visible.") {
		t.Fatalf("unexpected explanatory refresh text; got: %s", string(body))
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Refresh")), "1") {
		t.Fatalf("unexpected refresh header: %q", resp.Header.Get("Refresh"))
	}
	if strings.Contains(string(body), "/sessions/") {
		t.Fatalf("unexpected legacy /sessions/ link on session page; got: %s", string(body))
	}
	if strings.Contains(string(body), "<th>Actions</th>") {
		t.Fatalf("unexpected Actions column on session page; got: %s", string(body))
	}
	if strings.Contains(string(body), "href=\"/session\"") {
		t.Fatalf("unexpected separate session link on single-page session UI; got: %s", string(body))
	}
	if !strings.Contains(string(body), "Configuration details") {
		t.Fatalf("expected configuration details affordance on canonical session page; got: %s", string(body))
	}

	sessionResp, err := http.Get(base + "/session")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionResp.Body.Close()
	if sessionResp.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected /session status: %d", sessionResp.StatusCode)
	}

	healthResp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer healthResp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(healthResp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["session"] == nil {
		t.Fatalf("expected session payload; got %#v", payload)
	}

	recentResp, err := http.Get(base + "/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer recentResp.Body.Close()
	var recent map[string]any
	if err := json.NewDecoder(recentResp.Body).Decode(&recent); err != nil {
		t.Fatal(err)
	}
	sessPayload, ok := recent["session"].(map[string]any)
	if !ok || sessPayload["session_url"] != "/" {
		t.Fatalf("expected recent payload session_url=/; got %#v", recent["session"])
	}
	requests, ok := recent["requests"].([]any)
	if !ok {
		t.Fatalf("expected recent requests to be an empty array, got %#v", recent["requests"])
	}
	if len(requests) != 0 {
		t.Fatalf("expected no recent requests, got %#v", requests)
	}
	errorsPayload, ok := recent["errors"].([]any)
	if !ok {
		t.Fatalf("expected recent errors to be an empty array, got %#v", recent["errors"])
	}
	if len(errorsPayload) != 0 {
		t.Fatalf("expected no recent errors, got %#v", errorsPayload)
	}
}

func TestSessionProxyEventsStream(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "events"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get("http://" + sess.ProxyListenAddr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gotCh := make(chan string, 1)
	go func() {
		defer close(gotCh)
		scanner := bufio.NewScanner(resp.Body)
		var body strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			body.WriteString(line)
			body.WriteByte('\n')
			if strings.Contains(body.String(), "event: trace") && strings.Contains(body.String(), "forwarded request observed") {
				gotCh <- body.String()
				return
			}
		}
		if err := scanner.Err(); err != nil {
			gotCh <- "scanner error: " + err.Error() + "\n" + body.String()
			return
		}
		gotCh <- body.String()
	}()

	time.Sleep(50 * time.Millisecond)
	srv.recordTrace(sess.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sess.ID,
		Category:  "route",
		Summary:   "forwarded request observed",
		Detail:    "test",
	})

	select {
	case got := <-gotCh:
		if !strings.Contains(got, "event: trace") || !strings.Contains(got, "forwarded request observed") {
			t.Fatalf("unexpected event stream body: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for trace event")
	}
}

func TestSessionProxyEventsStreamUsesProxyErrorEvent(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "proxy-error-events"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get("http://" + sess.ProxyListenAddr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gotCh := make(chan string, 1)
	go func() {
		defer close(gotCh)
		scanner := bufio.NewScanner(resp.Body)
		var body strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			body.WriteString(line)
			body.WriteByte('\n')
			if strings.Contains(body.String(), "proxy failed for test") {
				gotCh <- body.String()
				return
			}
		}
		gotCh <- body.String()
	}()

	time.Sleep(50 * time.Millisecond)
	srv.recordError(sess.ID, model.ErrorRecord{
		Timestamp:  time.Now(),
		SessionID:  sess.ID,
		Severity:   "error",
		ErrorClass: "upstream_unreachable",
		Summary:    "proxy failed for test",
	})

	select {
	case got := <-gotCh:
		if !strings.Contains(got, "event: proxy_error") {
			t.Fatalf("expected proxy_error event, got: %s", got)
		}
		if strings.Contains(got, "event: error") {
			t.Fatalf("business proxy errors should not use EventSource error event: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy_error event")
	}
}

func TestSessionProxyEventsStreamForwardProxyE2E(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "sse-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get("http://" + sess.ProxyListenAddr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gotCh := make(chan string, 1)
	go func() {
		defer close(gotCh)
		scanner := bufio.NewScanner(resp.Body)
		var body strings.Builder
		for scanner.Scan() {
			body.WriteString(scanner.Text())
			body.WriteByte('\n')
			if strings.Contains(body.String(), "event: trace") && strings.Contains(body.String(), "forward_proxy") {
				gotCh <- body.String()
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	proxyURL := mustParse(t, "http://"+sess.ProxyListenAddr)
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 3 * time.Second}
	probe, err := hc.Get(upstream.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Body.Close()

	select {
	case got := <-gotCh:
		if !strings.Contains(got, "event: trace") || !strings.Contains(got, "forward_proxy") {
			t.Fatalf("unexpected event stream body: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for forward_proxy trace event")
	}
}

func TestSessionProxyBlindTunnelPassThrough(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	tlsUpstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Through", "blind-tunnel")
		_, _ = io.WriteString(w, "ok")
	}))
	defer tlsUpstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "blind"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	proxyURL := mustParse(t, "http://"+sess.ProxyListenAddr)
	hc := tlsUpstream.Client()
	if tr, ok := hc.Transport.(*http.Transport); ok {
		clone := tr.Clone()
		clone.Proxy = http.ProxyURL(proxyURL)
		hc.Transport = clone
	} else {
		t.Fatalf("unexpected transport type %T", hc.Transport)
	}
	resp, err := hc.Get(tlsUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected blind tunnel status: %d", resp.StatusCode)
	}
	if string(body) != "ok" || resp.Header.Get("X-Through") != "blind-tunnel" {
		t.Fatalf("unexpected blind tunnel response: status=%d body=%q headers=%v", resp.StatusCode, string(body), resp.Header)
	}
	requests, err := client.Requests(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) == 0 {
		t.Fatal("expected blind tunnel CONNECT to be recorded")
	}
	last := requests[len(requests)-1]
	if last.Method != http.MethodConnect || last.StreamState != model.StreamStateUnknown || last.StatusCode != http.StatusOK {
		t.Fatalf("unexpected blind tunnel request record: %#v", last)
	}
}

func TestSessionProxyHTTPForwardProxyPassThrough(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("X-Through", "http-forward")
		_, _ = io.WriteString(w, "plain-ok")
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "plain-http"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	proxyURL := mustParse(t, "http://"+sess.ProxyListenAddr)
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := hc.Get(upstream.URL + "/plain/path?x=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected forward proxy status: %d", resp.StatusCode)
	}
	if string(body) != "plain-ok" || resp.Header.Get("X-Through") != "http-forward" {
		t.Fatalf("unexpected forward proxy response: status=%d body=%q headers=%v", resp.StatusCode, string(body), resp.Header)
	}
	if gotPath != "/plain/path?x=1" {
		t.Fatalf("unexpected upstream request path: %s", gotPath)
	}
	requests := waitForRequestRecord(t, client, sess.ID, "forward-proxy request",
		func(rec model.RequestRecord) bool { return rec.Method == http.MethodGet })
	last := requests[len(requests)-1]
	if last.Path != "/plain/path?<redacted>" {
		t.Fatalf("unexpected logical path (expected query redacted for non-Anthropic forward target): %#v", last)
	}
	if last.ActualUpstreamHost != mustParse(t, upstream.URL).Hostname() {
		t.Fatalf("unexpected actual upstream host: %#v", last)
	}
}

func TestSessionProxyCloseUnblocksHangingMITM(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	hangCh := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hangCh
	}))
	defer func() {
		close(hangCh)
		upstream.Close()
	}()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "close-mitm"})
	if err != nil {
		t.Fatal(err)
	}
	upstreamURL := mustParse(t, upstream.URL)
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: upstreamURL.Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
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
	proxyURL := mustParse(t, "http://"+sess.ProxyListenAddr)
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}, Timeout: 10 * time.Second}
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/hang", nil)
		resp, reqErr := hc.Do(req)
		if reqErr == nil {
			_ = resp.Body.Close()
		}
	}()

	waitForTrace(t, srv, sess.ID, "route", "forwarding request", 3*time.Second)

	state := srv.getSession(sess.ID)
	if state == nil || state.proxy == nil {
		t.Fatal("sessionProxy not found")
	}
	sp := state.proxy

	start := time.Now()
	if err := sp.Close(); err != nil {
		t.Fatalf("sessionProxy.Close: %v", err)
	}
	elapsed := time.Since(start)
	// A Close that waits on the hung request blocks until the test's own
	// teardown (hangCh never closes before then), so 3s still discriminates
	// sharply while giving a starved CI runner scheduling slack.
	if elapsed > 3*time.Second {
		t.Fatalf("Close took too long: %v (expected < 3s)", elapsed)
	}

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client request did not unblock after proxy close")
	}
}

func TestSessionProxyRegisterInnerServerAfterCloseRejects(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "close-register"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil || state.proxy == nil {
		t.Fatal("sessionProxy not found")
	}
	sp := state.proxy
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dummy := &http.Server{}
	if sp.registerInnerServer(dummy) {
		t.Fatal("registerInnerServer should return false after Close")
	}
}

func TestJoinTargetURLKeepsEncodedSegments(t *testing.T) {
	base := mustParse(t, "https://api.test/prefix")
	req := mustParse(t, "/v1/foo%2Fbar?q=1")
	got := joinTargetURL(base, req)
	if got.EscapedPath() != "/prefix/v1/foo%2Fbar" {
		t.Fatalf("escaped path mismatch: got %q want /prefix/v1/foo%%2Fbar", got.EscapedPath())
	}
	if got.Path != "/prefix/v1/foo/bar" {
		t.Fatalf("decoded path mismatch: got %q", got.Path)
	}
	if got.RawQuery != "q=1" {
		t.Fatalf("query mismatch: got %q", got.RawQuery)
	}
	if !strings.HasSuffix(got.String(), "/prefix/v1/foo%2Fbar?q=1") {
		t.Fatalf("serialized URL should keep single-level %%2F encoding, got %q", got.String())
	}
}

func TestJoinTargetURLPlainPathsUnchanged(t *testing.T) {
	base := mustParse(t, "https://api.test")
	req := mustParse(t, "/v1/messages")
	got := joinTargetURL(base, req)
	if got.Path != "/v1/messages" {
		t.Fatalf("unexpected Path: %q", got.Path)
	}
	if got.RawPath != "" {
		t.Fatalf("RawPath should be empty for plain paths (no special chars), got %q", got.RawPath)
	}
	if got.String() != "https://api.test/v1/messages" {
		t.Fatalf("unexpected serialized URL: %q", got.String())
	}
}

func TestPathForRecordAnthropicKeepsQuery(t *testing.T) {
	u := mustParse(t, "https://api.anthropic.com/v1/messages?stream=true")
	if got := pathForRecord(u, true); got != "/v1/messages?stream=true" {
		t.Fatalf("Anthropic path should keep query, got %q", got)
	}
	if got := pathForRecord(u, false); got != "/v1/messages?<redacted>" {
		t.Fatalf("non-Anthropic path should redact query, got %q", got)
	}
	plain := mustParse(t, "https://example.test/plain")
	if got := pathForRecord(plain, false); got != "/plain" {
		t.Fatalf("non-Anthropic path without query should be bare, got %q", got)
	}
}

func TestIsAnthropicHostMatchesAnyAnthropicSuffix(t *testing.T) {
	cases := map[string]bool{
		// *.anthropic.com suffix branch (historical).
		"api.anthropic.com":               true,
		"API.Anthropic.com":               true,
		"api.anthropic.com.":              true,
		"api-staging.anthropic.com":       true,
		"mcp-proxy.anthropic.com":         true,
		"mcp-proxy-staging.anthropic.com": true,
		"telemetry.anthropic.com":         true,
		"fancy-new-service.anthropic.com": true,
		"anthropic.com":                   false,
		"not-anthropic.com":               false,
		"evil.com":                        false,
		// OAuth exact-host branch. Three hosts MUST MITM — these carry
		// the OAuth refresh + authorize + CIMD client-metadata flows
		// (claude-code/src/constants/oauth.ts).
		"platform.claude.com": true,
		"claude.com":          true,
		"claude.ai":           true,
		// Exact match — NOT suffix. code.claude.com / docs.claude.com (docs
		// hosts) must NOT MITM (no auth value, widens scope unnecessarily).
		"code.claude.com": false,
		"docs.claude.com": false,
		// Also case-insensitive on the exact-host branch.
		"Platform.Claude.com": true,
		"CLAUDE.AI":           true,
	}
	for host, want := range cases {
		if got := isAnthropicHost(host); got != want {
			t.Fatalf("isAnthropicHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestResolveUpstreamNormalizesAnthropicHost(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "case-host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://relay.example.test/prefix",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "relay.example.test",
		ExactUpstreamBase: "https://relay.example.test/prefix",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	state := srv.getSession(sess.ID)
	if state == nil || state.proxy == nil {
		t.Fatal("sessionProxy not found")
	}
	target, upstreamHost, errClass, err := state.proxy.resolveUpstream("API.Anthropic.com.", state.active.Load())
	if err != nil {
		t.Fatalf("resolveUpstream returned err=%v class=%s", err, errClass)
	}
	if target == nil || target.String() != "https://relay.example.test/prefix" {
		t.Fatalf("expected API host to route to configured upstream, got %#v", target)
	}
	if upstreamHost != "relay.example.test" {
		t.Fatalf("upstreamHost = %q, want relay.example.test", upstreamHost)
	}
}

func TestSessionProxyDefaultsMCPUpstreamToPublicAnthropic(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "mcp-default"})
	if err != nil {
		t.Fatal(err)
	}
	// Route is set WITHOUT an mcp proxy upstream.
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}

	state := srv.getSession(sess.ID)
	if state == nil || state.proxy == nil {
		t.Fatal("sessionProxy not found")
	}
	target, upstreamHost, errClass, err := state.proxy.resolveUpstream("mcp-proxy.anthropic.com", state.active.Load())
	if err != nil {
		t.Fatalf("resolveUpstream should fall back to public mcp-proxy, got err: %v (class=%s)", err, errClass)
	}
	if target == nil || target.Host != "mcp-proxy.anthropic.com" || target.Scheme != "https" {
		t.Fatalf("expected fallback to https://mcp-proxy.anthropic.com, got %#v", target)
	}
	if upstreamHost != "mcp-proxy.anthropic.com" {
		t.Fatalf("upstreamHost = %q, want mcp-proxy.anthropic.com", upstreamHost)
	}

	// Arbitrary new anthropic host also resolves to its public form.
	target2, _, _, err := state.proxy.resolveUpstream("telemetry.anthropic.com", state.active.Load())
	if err != nil {
		t.Fatalf("resolveUpstream for arbitrary anthropic host returned error: %v", err)
	}
	if target2 == nil || target2.Host != "telemetry.anthropic.com" {
		t.Fatalf("expected telemetry.anthropic.com passthrough, got %#v", target2)
	}
}

func TestBroadcastEvictsSlowSubscriber(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	ch, unsubscribe := srv.subscribe()
	defer unsubscribe()

	// Fill the subscriber's buffer without draining so the next broadcast
	// discovers the slow consumer and evicts it.
	for i := 0; i < 130; i++ {
		srv.broadcast("trace", "test", model.TraceRecord{Category: "fill", Summary: "backlog"})
	}

	deadline := time.Now().Add(2 * time.Second)
	drained := 0
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-ch:
			if !ok {
				if drained == 0 {
					t.Fatal("subscriber channel closed before any event drained")
				}
				return
			}
			drained++
		case <-time.After(100 * time.Millisecond):
			// no more events right now, force another broadcast to trip eviction
			srv.broadcast("trace", "test", model.TraceRecord{Category: "fill", Summary: "kick"})
		}
	}
	t.Fatalf("subscriber channel was never closed after %d drained events; eviction did not kick in", drained)
}

func waitForTrace(t *testing.T, srv *Supervisor, sessionID, category, summarySubstr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, rec := range srv.listTrace(sessionID) {
			if rec.Category == category && strings.Contains(rec.Summary, summarySubstr) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("trace category=%q summary~%q not observed within %v", category, summarySubstr, timeout)
}

// waitForForwardedMessage polls the control Requests endpoint until a forwarded
// POST /v1/messages record appears, returning it. The proxy records forwarded
// requests asynchronously, so reading once immediately after the HTTP round trip
// races — the record can lag the response, especially on slower CI runners.
func waitForForwardedMessage(t *testing.T, client *control.Client, sessionID string) *model.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last []model.RequestRecord
	for {
		requests, err := client.Requests(context.Background(), sessionID)
		if err == nil {
			last = requests
			for i := len(requests) - 1; i >= 0; i-- {
				if requests[i].Method == http.MethodPost && strings.Contains(requests[i].Path, "/v1/messages") {
					rec := requests[i]
					return &rec
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("forwarded /v1/messages request not recorded within 3s: %#v", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForRequestRecord generalizes waitForForwardedMessage to any record
// shape: it polls the control Requests endpoint until pred matches a record,
// returning the ring snapshot that contained the match (so invariant sweeps
// can run over the same data). Forwarded records land only after the
// ReverseProxy handler returns and synthetic records only after the response
// is written, so a single read immediately after the client-observed round
// trip races — the record can lag the response, especially on slower CI
// runners. Only blind-tunnel CONNECT (recorded before the relay starts) and
// rewrite-rejected 502s (recorded before http.Error) are exempt.
func waitForRequestRecord(t *testing.T, client *control.Client, sessionID, what string, pred func(model.RequestRecord) bool) []model.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last []model.RequestRecord
	for {
		requests, err := client.Requests(context.Background(), sessionID)
		if err == nil {
			last = requests
			for _, rec := range requests {
				if pred(rec) {
					return requests
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not recorded within 3s: %#v", what, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisorRejectsSecondSession(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	if _, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "first"}); err != nil {
		t.Fatalf("first CreateSession() error = %v", err)
	}
	if _, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "second"}); err == nil {
		t.Fatal("second CreateSession() unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "single-session") {
		t.Fatalf("second CreateSession() error = %v, want single-session conflict", err)
	}
}

func TestApplyAuthOverrideAddsOAuthBetaAndMergesExistingBeta(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer old")
	headers.Set("anthropic-beta", "existing-beta")
	applyAuthOverride(headers, &model.AuthOverride{
		Mode:        model.AuthModeOverrideBearer,
		Source:      model.AuthSourceClaudeOAuthToken,
		HeaderName:  "Authorization",
		HeaderValue: "Bearer new",
	})
	if got := headers.Get("Authorization"); got != "Bearer new" {
		t.Fatalf("expected Authorization override, got %q", got)
	}
	if got := headers.Get("anthropic-beta"); !strings.Contains(got, "existing-beta") || !strings.Contains(got, "oauth-2025-04-20") {
		t.Fatalf("expected OAuth beta to merge with existing beta, got %q", got)
	}
}

func TestApplyAuthOverrideDropsClaudeSideGatewayAuthHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer placeholder")
	headers.Set("X-API-Key", "placeholder")
	headers.Set("X-Gateway-Key", "gateway-placeholder")
	headers.Set("X-LitellM-Key", "litellm-placeholder")
	headers.Set("X-Tenant", "team-a")
	applyAuthOverride(headers, &model.AuthOverride{
		Mode:        model.AuthModeOverrideXAPIKey,
		Source:      model.AuthSourceAnthropicAPIKey,
		HeaderName:  "X-API-Key",
		HeaderValue: "real-upstream-key",
	})
	if got := headers.Get("X-API-Key"); got != "real-upstream-key" {
		t.Fatalf("expected real upstream key, got %q", got)
	}
	for _, name := range []string{"Authorization", "X-Gateway-Key", "X-LitellM-Key"} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("%s should be removed before upstream, got %q", name, got)
		}
	}
	if got := headers.Get("X-Tenant"); got != "team-a" {
		t.Fatalf("non-auth custom header should remain, got %q", got)
	}
}

func TestIsAnthropicAPIHostRestrictsAuthOverrideTargets(t *testing.T) {
	if !isAnthropicAPIHost("api.anthropic.com") || !isAnthropicAPIHost("api-staging.anthropic.com") {
		t.Fatal("expected API hosts to be auth-override eligible")
	}
	for _, host := range []string{"telemetry.anthropic.com", "mcp-proxy.anthropic.com", "anthropic.com", "example.com"} {
		if isAnthropicAPIHost(host) {
			t.Fatalf("did not expect %s to be auth-override eligible", host)
		}
	}
}

func TestSessionProxyCachesUpstreamTransportByEgressConfig(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "transport-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
		Egress:            model.EgressConfig{Mode: "direct", Source: "none"},
	}); err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil || state.proxy == nil {
		t.Fatal("sessionProxy not found")
	}
	first := state.proxy.upstreamTransport(state.active.Load().r.egress)
	second := state.proxy.upstreamTransport(state.active.Load().r.egress)
	if first == nil || first != second {
		t.Fatalf("expected same cached transport for unchanged egress config, got %p and %p", first, second)
	}
}

func TestSessionProxyModelAliasRewritesRequestAndResponse(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		upstreamModel, _ = payload["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "model": upstreamModel, "content": []any{}})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "alias"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		ModelAlias: model.ModelAliasConfig{
			Mode:   model.ModelAliasRewrite,
			Strict: true,
			Source: "test",
			Forward: map[string]string{
				"claude-sonnet-4-6": "gateway/sonnet-4.6-prod",
			},
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
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if upstreamModel != "gateway/sonnet-4.6-prod" {
		t.Fatalf("upstream model = %q", upstreamModel)
	}
	if !strings.Contains(string(body), `"model":"claude-sonnet-4-6"`) {
		t.Fatalf("expected client-visible logical model, got %s", body)
	}
	snap, err := client.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ModelAliasMode != model.ModelAliasRewrite || snap.ModelAliasCount != 1 || snap.ModelAliasFingerprint == "" {
		t.Fatalf("expected redacted model alias summary, got %#v", snap)
	}
}

// TestSessionProxyHTTPForwardPathAnchorsBodyOnRewriteError — when
// modelalias.RewriteRequest rejects a captured-body request (e.g.,
// Content-Type: text/plain on /v1/messages while a rewrite alias is
// configured), the handler returns 502 with an ErrorRecord, but the
// inbound capture tap had already spilled the body to disk. Without
// the fix below, no RequestRecord referenced that body — the spill
// became an orphan file (bounded by bodystore LRU but invisible
// through /recent).
//
// This test asserts that the early-return path emits a partial
// RequestRecord with status=502 + the spilled bodyRef so the inspect
// drawer can surface what was attempted.
func TestSessionProxyHTTPForwardPathAnchorsBodyOnRewriteError(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should NEVER be reached when rewrite rejects")
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "alias-anchor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
		RouteSource:          model.RouteSourceExplicit,
		AuthMode:             model.AuthModePassthrough,
		AuthSource:           model.AuthSourceNone,
		ExactUpstreamHost:    mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase:    upstream.URL,
		FailPolicy:           model.FailClosed,
		CaptureRequestBodies: true,
		ModelAlias: model.ModelAliasConfig{
			Mode:   model.ModelAliasRewrite,
			Strict: true,
			Source: "test",
			Forward: map[string]string{
				"claude-sonnet-4-6": "gateway/sonnet-4.6-prod",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)),
	}}
	// Content-Type: text/plain on /v1/messages — RewriteRequest will
	// reject ("requires JSON request body"). The body still spills via
	// the inbound capture tap before the rejection.
	req, _ := http.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages", strings.NewReader(`some-body-that-was-spilled`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	requests, err := client.Requests(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) == 0 {
		t.Fatal("expected partial RequestRecord on rewrite-rejected path; got none (orphan body bug not fixed)")
	}
	last := requests[len(requests)-1]
	if last.StatusCode != http.StatusBadGateway {
		t.Errorf("RequestRecord status = %d, want 502", last.StatusCode)
	}
	if last.BodyRef == nil {
		t.Errorf("RequestRecord.BodyRef = nil; spilled body is orphan")
	}
	// The rewrite-error record must carry a defined StreamState like
	// every other record, not the empty zero value.
	if last.StreamState == "" {
		t.Errorf("RequestRecord.StreamState is empty; want a defined state (e.g. unknown) for consistency with all other records")
	}
}

// TestSessionProxyHTTPForwardPathRewritesModelAlias — the HTTP forward
// path must apply modelalias.RewriteRequest identically to the CONNECT/
// MITM path. Pre-fix, only mitmHandler called RewriteRequest, so a
// third-party gateway profile whose client used plain-HTTP proxy
// semantics (POST http://api.anthropic.com/...) leaked the logical
// model name unchanged to the upstream — the hidden-mode "Claude only
// sees logical names" invariant was broken on this code path.
//
// Mirrors TestSessionProxyModelAliasRewritesRequestAndResponse with
// the one difference that the request targets http://api.anthropic.com
// (proxy forwards via handleForwardProxyRequest), not https://.
//
// Authoritative on intent: the body the upstream stub observes must
// carry the provider model ID (rewrite happened), and the
// client-visible response body must carry the logical model ID
// (RewriteResponse normalized on the way back).
func TestSessionProxyHTTPForwardPathRewritesModelAlias(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		upstreamModel, _ = payload["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "model": upstreamModel, "content": []any{}})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "alias-http"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		ModelAlias: model.ModelAliasConfig{
			Mode:   model.ModelAliasRewrite,
			Strict: true,
			Source: "test",
			Forward: map[string]string{
				"claude-sonnet-4-6": "gateway/sonnet-4.6-prod",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Plain HTTP proxy — no TLS, no CONNECT. Forces the request through
	// handleForwardProxyRequest, the path that pre-fix skipped modelalias.
	hc := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)),
	}}
	req, _ := http.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if upstreamModel != "gateway/sonnet-4.6-prod" {
		t.Fatalf("upstream model = %q (HTTP forward path did not rewrite)", upstreamModel)
	}
	if !strings.Contains(string(body), `"model":"claude-sonnet-4-6"`) {
		t.Fatalf("expected client-visible logical model in response (RewriteResponse missed); got %s", body)
	}
}

func TestSessionProxyThirdPartyPathAwareSyntheticAndBlockedPaths(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var upstreamRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "path-aware"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		OverrideAuth:      &model.AuthOverride{Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey, HeaderName: "X-API-Key", HeaderValue: "real"},
		ModelAlias: model.ModelAliasConfig{Mode: model.ModelAliasRewrite, Strict: true, Source: "test", Forward: map[string]string{
			"claude-sonnet-4-6": "gateway/sonnet-4.6-prod",
		}},
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
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)), TLSClientConfig: &tls.Config{RootCAs: pool}}}

	modelsResp, err := hc.Get("https://api.anthropic.com/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	modelsBody, _ := io.ReadAll(modelsResp.Body)
	modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d body=%s", modelsResp.StatusCode, modelsBody)
	}
	if !strings.Contains(string(modelsBody), "claude-sonnet-4-6") || strings.Contains(string(modelsBody), "gateway/sonnet") {
		t.Fatalf("synthetic models should expose only logical IDs, got %s", modelsBody)
	}
	if upstreamRequests != 0 {
		t.Fatalf("/v1/models should not reach third-party upstream in hidden mode, got %d upstream requests", upstreamRequests)
	}

	bootstrapResp, err := hc.Get("https://api.anthropic.com/api/claude_cli/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	bootstrapBody, _ := io.ReadAll(bootstrapResp.Body)
	bootstrapResp.Body.Close()
	if bootstrapResp.StatusCode != http.StatusOK || !strings.Contains(string(bootstrapBody), "additional_model_options") {
		t.Fatalf("bootstrap noop response unexpected: status=%d body=%s", bootstrapResp.StatusCode, bootstrapBody)
	}
	if upstreamRequests != 0 {
		t.Fatalf("bootstrap should not reach third-party upstream, got %d upstream requests", upstreamRequests)
	}

	mcpResp, err := hc.Get("https://api.anthropic.com/v1/mcp_servers")
	if err != nil {
		t.Fatal(err)
	}
	mcpBody, _ := io.ReadAll(mcpResp.Body)
	mcpResp.Body.Close()
	if mcpResp.StatusCode != http.StatusOK || !strings.Contains(string(mcpBody), "has_more") || !strings.Contains(string(mcpBody), "data") {
		t.Fatalf("mcp servers synthetic response unexpected: status=%d body=%s", mcpResp.StatusCode, mcpBody)
	}
	if upstreamRequests != 0 {
		t.Fatalf("mcp servers should not reach third-party upstream, got %d upstream requests", upstreamRequests)
	}

	registryResp, err := hc.Get("https://api.anthropic.com/mcp-registry/v0/servers")
	if err != nil {
		t.Fatal(err)
	}
	registryBody, _ := io.ReadAll(registryResp.Body)
	registryResp.Body.Close()
	if registryResp.StatusCode != http.StatusOK || !strings.Contains(string(registryBody), "servers") {
		t.Fatalf("mcp registry synthetic response unexpected: status=%d body=%s", registryResp.StatusCode, registryBody)
	}
	if upstreamRequests != 0 {
		t.Fatalf("mcp registry should not reach third-party upstream, got %d upstream requests", upstreamRequests)
	}

	policyResp, err := hc.Get("https://api.anthropic.com/api/claude_code/policy_limits")
	if err != nil {
		t.Fatal(err)
	}
	policyResp.Body.Close()
	if policyResp.StatusCode != http.StatusNoContent {
		t.Fatalf("policy noop status = %d", policyResp.StatusCode)
	}
	if upstreamRequests != 0 {
		t.Fatalf("policy limits should not reach third-party upstream, got %d upstream requests", upstreamRequests)
	}

	for _, url := range []string{
		"https://api.anthropic.com/v1/files",
		"https://api.anthropic.com/api/event_logging/v2/batch",
		"https://api.anthropic.com/api/claude_code_penguin_mode",
		"https://api.anthropic.com/api/anything/new/from/claude",
	} {
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || len(bytes.TrimSpace(body)) != 0 {
			t.Fatalf("silent synthetic path %s response unexpected: status=%d body=%s", url, resp.StatusCode, body)
		}
		if upstreamRequests != 0 {
			t.Fatalf("silent synthetic path %s should not reach third-party upstream, got %d upstream requests", url, upstreamRequests)
		}
	}
}

func TestForwardProxyThirdPartyUnknownPathSynthetic204(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var upstreamRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "forward-synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		OverrideAuth:      &model.AuthOverride{Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey, HeaderName: "X-API-Key", HeaderValue: "real"},
	}); err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr))}}
	req, err := http.NewRequest(http.MethodPost, "http://api.anthropic.com/api/event_logging/v2/batch", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("forward proxy synthetic response unexpected: status=%d body=%s", resp.StatusCode, body)
	}
	if upstreamRequests != 0 {
		t.Fatalf("forward proxy synthetic path should not reach upstream, got %d", upstreamRequests)
	}
}

func TestPathAwareHelpersClassifyImplementedGatewayPaths(t *testing.T) {
	if !isImplementedThirdPartyGatewayPath(http.MethodPost, "/v1/messages", false) {
		t.Fatal("/v1/messages should be an implemented model gateway path")
	}
	if !isImplementedThirdPartyGatewayPath(http.MethodPost, "/v1/messages/count_tokens", false) {
		t.Fatal("/v1/messages/count_tokens should be an implemented model gateway path")
	}
	if isImplementedThirdPartyGatewayPath(http.MethodGet, "/v1/models", false) {
		t.Fatal("/v1/models should be synthetic in hidden mode unless provider model passthrough is enabled")
	}
	if !isImplementedThirdPartyGatewayPath(http.MethodGet, "/v1/models", true) {
		t.Fatal("/v1/models should be pass-through in provider model passthrough mode")
	}
	if spec, ok := firstPartySyntheticSpec(http.MethodGet, "/api/claude_cli/bootstrap"); !ok || spec.Status != http.StatusOK || spec.Body == nil {
		t.Fatalf("bootstrap should have a shaped synthetic spec, got spec=%#v ok=%v", spec, ok)
	}
	if spec, ok := firstPartySyntheticSpec(http.MethodGet, "/v1/mcp_servers"); !ok || spec.Status != http.StatusOK || spec.Body == nil {
		t.Fatalf("mcp servers should have a shaped synthetic spec, got spec=%#v ok=%v", spec, ok)
	}
	if spec, ok := firstPartySyntheticSpec(http.MethodGet, "/mcp-registry/v0/servers"); !ok || spec.Status != http.StatusOK || spec.Body == nil {
		t.Fatalf("mcp registry should have a shaped synthetic spec, got spec=%#v ok=%v", spec, ok)
	}
	if _, ok := firstPartySyntheticSpec(http.MethodGet, "/api/claude_code_penguin_mode"); ok {
		t.Fatal("unknown first-party service paths should use silent 204 default, not a shaped spec")
	}
	// Cloud-sync GETs (settingsSync, teamMemorySync) accept 200/404 but NOT 204
	// (claude-code validateStatus: status===200||304||404; 404 = "no data exists
	// yet"). The default synthetic 204 makes the client's axios call throw; these
	// two paths must synthesize 404 (no body) instead so the client cleanly reads
	// "no remote data".
	for _, p := range []string{"/api/claude_code/user_settings", "/api/claude_code/team_memory"} {
		spec, ok := firstPartySyntheticSpec(http.MethodGet, p)
		if !ok || spec.Status != http.StatusNotFound || spec.Body != nil {
			t.Fatalf("%s should synthesize a bodyless 404, got spec=%#v ok=%v", p, spec, ok)
		}
	}
	if !isImplementedThirdPartyGatewayPath(http.MethodPost, "/v1/messages/batches", false) {
		t.Fatal("/v1/messages/batches create should be an implemented model gateway path")
	}
	if !isImplementedThirdPartyGatewayPath(http.MethodGet, "/v1/messages/batches/msgbatch_123/results", false) {
		t.Fatal("/v1/messages/batches results should be an implemented model gateway path")
	}
	if isImplementedThirdPartyGatewayPath(http.MethodGet, "/v1/files", false) {
		t.Fatal("/v1/files must not be routed to the third-party model gateway by default")
	}
}

func TestSessionProxyMessageBatchesRewriteAndUpstreamHeaders(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var batchBody string
	var resultsRequested bool
	var gotGatewayTenant string
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGatewayTenant = r.Header.Get("X-Gateway-Tenant")
		gotAuth = r.Header.Get("X-API-Key")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/batches":
			body, _ := io.ReadAll(r.Body)
			batchBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msgbatch_123", "type": "message_batch", "model": "gateway/sonnet"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches/msgbatch_123/results":
			resultsRequested = true
			w.Header().Set("Content-Type", "application/x-jsonl")
			_, _ = w.Write([]byte(`{"custom_id":"a","result":{"type":"succeeded","message":{"model":"gateway/sonnet","content":[]}}}` + "\n"))
		default:
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "batches"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		OverrideAuth:      &model.AuthOverride{Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey, HeaderName: "X-API-Key", HeaderValue: "real-key"},
		UpstreamHeaders:   map[string]string{"X-Gateway-Tenant": "team-a"},
		ModelAlias: model.ModelAliasConfig{Mode: model.ModelAliasRewrite, Strict: true, Source: "test", Forward: map[string]string{
			"claude-sonnet-4-6": "gateway/sonnet",
		}},
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
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)), TLSClientConfig: &tls.Config{RootCAs: pool}}}

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/batches", strings.NewReader(`{"requests":[{"custom_id":"a","params":{"model":"claude-sonnet-4-6","messages":[]}}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch create status=%d body=%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(batchBody, `"model":"gateway/sonnet"`) || strings.Contains(batchBody, "claude-sonnet-4-6") {
		t.Fatalf("batch create body not rewritten correctly: %s", batchBody)
	}
	if !strings.Contains(string(respBody), `"model":"claude-sonnet-4-6"`) || strings.Contains(string(respBody), "gateway/sonnet") {
		t.Fatalf("batch create response not normalized: %s", respBody)
	}
	if gotGatewayTenant != "team-a" || gotAuth != "real-key" {
		t.Fatalf("unexpected upstream headers: tenant=%q auth=%q", gotGatewayTenant, gotAuth)
	}

	resultsResp, err := hc.Get("https://api.anthropic.com/v1/messages/batches/msgbatch_123/results")
	if err != nil {
		t.Fatal(err)
	}
	resultsBody, _ := io.ReadAll(resultsResp.Body)
	resultsResp.Body.Close()
	if !resultsRequested || resultsResp.StatusCode != http.StatusOK {
		t.Fatalf("results request failed: requested=%v status=%d body=%s", resultsRequested, resultsResp.StatusCode, resultsBody)
	}
	if !strings.Contains(string(resultsBody), `"model":"claude-sonnet-4-6"`) || strings.Contains(string(resultsBody), "gateway/sonnet") {
		t.Fatalf("batch results response not normalized: %s", resultsBody)
	}
}

// headerInspectorSession builds a third-party-hidden session whose
// upstream is a local httptest server, returning an http.Client that
// proxies through the session (composite-CA trusted) so requests hit
// the MITM ReverseProxy capture path. Mirrors the harness of
// TestSessionProxyModelAliasRewritesRequestAndResponse — no new
// helpers invented.
func headerInspectorSession(t *testing.T, sockName string) (*control.Client, *model.Session, *http.Client, *httptest.Server) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, sockName)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "content": []any{}})
	}))
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "hdr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
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
	return client, sess, hc, upstream
}

func TestRequestHeadersCapturedWithCredentialsMasked(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer sk-CAPTURESECRET")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForForwardedMessage(t, client, sess.ID)
	if rec.RequestHeaders == nil {
		t.Fatalf("forwarded request must capture inbound headers, got nil")
	}
	// Non-credential headers are captured full-fidelity.
	if got := rec.RequestHeaders.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("captured Anthropic-Version = %q, want 2023-06-01", got)
	}
	// Credential headers are masked AT THE STORE (recordRequest), so the raw
	// secret never reaches the activity ring, /recent, the SSE stream, or a
	// HAR export. The scheme + short prefix survive for recognizability; the
	// secret body does not. Render-time redaction is now defense in depth on
	// top of this. (Raw storage is opt-in via CCWRAP_UNMASK_CREDENTIALS=1.)
	auth := rec.RequestHeaders.Get("Authorization")
	if strings.Contains(auth, "CAPTURESECRET") {
		t.Fatalf("stored Authorization must NOT carry the raw secret; got %q", auth)
	}
	if !strings.HasPrefix(auth, "Bearer ") || !strings.Contains(auth, "redacted") {
		t.Fatalf("stored Authorization must be structure-preservingly masked; got %q", auth)
	}
}

// TestRequestBodyCapturedToFileWhenEnabled is the capture contract:
// with capture ON, a POST through the MITM ReverseProxy site
// must tee r.Body BEFORE modelalias.RewriteRequest consumes it, spill
// it to <sessionRuntimeDir>/bodies/<id>.json via the async writer, and
// record a BodyRef on the RequestRecord. Reuses headerInspectorSession
// exactly as TestRequestHeadersCapturedWithCredentialsMasked does, only flipping
// the route's CaptureRequestBodies on (same control SetRoute path).
func TestRequestBodyCapturedToFileWhenEnabled(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()

	// Flip capture ON via the same control SetRoute path the harness
	// used (third-party-hidden route to the httptest upstream), now
	// with CaptureRequestBodies:true (the only delta vs the header test).
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
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

	sentBody := `{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"hello"}],"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(sentBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForForwardedMessage(t, client, sess.ID)
	if rec.BodyRef == nil {
		t.Fatalf("capture ON: forwarded request must record a BodyRef, got nil (%#v)", rec)
	}
	if rec.BodyRef.Size != int64(len(sentBody)) {
		t.Fatalf("BodyRef.Size = %d, want %d", rec.BodyRef.Size, len(sentBody))
	}

	// <sessionRuntimeDir> is the supervisor's per-session RuntimeDir
	// (single-session supervisor); derive it via the control Status
	// endpoint — do not hardcode. bodyStore writes to
	// <RuntimeDir>/bodies/<id>.json.
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bodyPath := filepath.Join(status.RuntimeDir, "bodies", rec.BodyRef.ID+".json")

	// The writer is async; poll like bodystore_test.go.
	var fileBytes []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, rerr := os.ReadFile(bodyPath); rerr == nil {
			fileBytes = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fileBytes == nil {
		t.Fatalf("spilled body file never appeared at %s", bodyPath)
	}
	if string(fileBytes) != sentBody {
		t.Fatalf("spilled body mismatch:\n got %q\nwant %q", fileBytes, sentBody)
	}
	sum := sha256.Sum256(fileBytes)
	if got := hex.EncodeToString(sum[:]); got != rec.BodyRef.SHA256 {
		t.Fatalf("BodyRef.SHA256 = %q, want %q (sha of spilled file)", rec.BodyRef.SHA256, got)
	}
}

// TestRequestBodyCapturedTwiceWithModelAliasRewrite asserts that when both
// capture is ON and modelalias rewrite ACTUALLY mutates the body, the
// RequestRecord carries BOTH BodyRef (client view, pre-rewrite) and
// UpstreamBodyRef (post-rewrite, what hit the wire). The two refs must
// have distinct IDs and distinct SHA256s — confirming we spilled both
// versions, not one ref shared. Setup mirrors TestRequestBodyCapturedToFileWhenEnabled
// but adds a non-claude-* alias target so RewriteRequest changes the body.
func TestRequestBodyCapturedTwiceWithModelAliasRewrite(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()

	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
		RouteSource:          model.RouteSourceExplicit,
		AuthMode:             model.AuthModePassthrough,
		AuthSource:           model.AuthSourceNone,
		ExactUpstreamHost:    mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase:    upstream.URL,
		FailPolicy:           model.FailClosed,
		CaptureRequestBodies: true,
		// Non-claude-* target triggers system-block stripping on top of
		// the model field rewrite, so client and upstream bodies differ.
		ModelAlias: model.ModelAliasConfig{
			Mode:    model.ModelAliasMode("rewrite"),
			Source:  "test",
			Strict:  true,
			Forward: map[string]string{"claude-opus-4-7": "gpt-5.5"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	sentBody := `{"model":"claude-opus-4-7","system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1; cc_entrypoint=cli;"},` +
		`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},` +
		`{"type":"text","text":"keep me"}],"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(sentBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForForwardedMessage(t, client, sess.ID)
	if rec.BodyRef == nil {
		t.Fatalf("BodyRef must be set (capture ON)")
	}
	if rec.UpstreamBodyRef == nil {
		t.Fatalf("UpstreamBodyRef must be set (capture ON + alias rewrite hit). BodyRef=%+v", rec.BodyRef)
	}
	if rec.BodyRef.ID == rec.UpstreamBodyRef.ID {
		t.Fatalf("BodyRef.ID == UpstreamBodyRef.ID — must be distinct spilled files")
	}
	if rec.BodyRef.SHA256 == rec.UpstreamBodyRef.SHA256 {
		t.Fatalf("BodyRef.SHA256 == UpstreamBodyRef.SHA256 — rewrite did not change the body")
	}
	if rec.BodyRef.Size != int64(len(sentBody)) {
		t.Fatalf("BodyRef.Size = %d, want %d (pre-rewrite must equal the sent bytes)", rec.BodyRef.Size, len(sentBody))
	}
	// Upstream body must be readable + actually differ in content.
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clientBytes, err := os.ReadFile(filepath.Join(status.RuntimeDir, "bodies", rec.BodyRef.ID+".json"))
	deadline := time.Now().Add(2 * time.Second)
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		clientBytes, err = os.ReadFile(filepath.Join(status.RuntimeDir, "bodies", rec.BodyRef.ID+".json"))
	}
	if err != nil {
		t.Fatalf("client spill never appeared: %v", err)
	}
	upstreamBytes, err := os.ReadFile(filepath.Join(status.RuntimeDir, "bodies", rec.UpstreamBodyRef.ID+".json"))
	deadline = time.Now().Add(2 * time.Second)
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		upstreamBytes, err = os.ReadFile(filepath.Join(status.RuntimeDir, "bodies", rec.UpstreamBodyRef.ID+".json"))
	}
	if err != nil {
		t.Fatalf("upstream spill never appeared: %v", err)
	}
	// Client view = original bytes.
	if string(clientBytes) != sentBody {
		t.Fatalf("client body must equal sent bytes:\n got %s\nwant %s", clientBytes, sentBody)
	}
	// Upstream view: model rewritten + Claude Code blocks stripped, 'keep me' retained.
	upstreamStr := string(upstreamBytes)
	if !strings.Contains(upstreamStr, `"gpt-5.5"`) {
		t.Fatalf("upstream body must contain rewritten model gpt-5.5: %s", upstreamStr)
	}
	if strings.Contains(upstreamStr, "x-anthropic-billing-header") {
		t.Fatalf("upstream body must NOT contain billing-header (default-strip on non-claude-*): %s", upstreamStr)
	}
	if strings.Contains(upstreamStr, "You are Claude Code, Anthropic") {
		t.Fatalf("upstream body must NOT contain identity prefix: %s", upstreamStr)
	}
	if !strings.Contains(upstreamStr, "keep me") {
		t.Fatalf("upstream body must retain regular system block 'keep me': %s", upstreamStr)
	}
}

// TestRequestBodyNoUpstreamRefWhenAliasMisses asserts that when capture is
// ON but the request's model is not in the alias map (RewriteRequest
// returns Rewritten=false), only BodyRef is set — UpstreamBodyRef stays
// nil because the body is forwarded byte-identically and a duplicate
// spill would waste disk.
func TestRequestBodyNoUpstreamRefWhenAliasMisses(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()

	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
		RouteSource:          model.RouteSourceExplicit,
		AuthMode:             model.AuthModePassthrough,
		AuthSource:           model.AuthSourceNone,
		ExactUpstreamHost:    mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase:    upstream.URL,
		FailPolicy:           model.FailClosed,
		CaptureRequestBodies: true,
		// Alias map exists but the request below uses a model NOT in it.
		ModelAlias: model.ModelAliasConfig{
			Mode:    model.ModelAliasMode("rewrite"),
			Source:  "test",
			Forward: map[string]string{"claude-opus-4-7": "gpt-5.5"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	sentBody := `{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"hello"}],"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(sentBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForForwardedMessage(t, client, sess.ID)
	if rec.BodyRef == nil {
		t.Fatalf("BodyRef must be set (capture ON)")
	}
	if rec.UpstreamBodyRef != nil {
		t.Fatalf("UpstreamBodyRef must be nil when alias did not match — got %+v", rec.UpstreamBodyRef)
	}
}

// TestRecentBodyEndpointStreamsAndMisses is the delivery contract:
// the session-proxy info endpoint GET /recent/body streams a
// spilled body file as application/json when present, and 404s with a
// JSON reason on a miss. Reuses headerInspectorSession + the capture-ON
// SetRoute / POST-through-MITM / async-poll plumbing exactly as
// TestRequestBodyCapturedToFileWhenEnabled does.
func TestRecentBodyEndpointStreamsAndMisses(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()

	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
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

	sentBody := `{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"hello"}],"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(sentBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForForwardedMessage(t, client, sess.ID)
	if rec.BodyRef == nil {
		t.Fatalf("capture ON: forwarded request must record a BodyRef, got nil (%#v)", rec)
	}

	// The writer is async; poll like bodystore_test.go before hitting
	// the lazy endpoint.
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bodyPath := filepath.Join(status.RuntimeDir, "bodies", rec.BodyRef.ID+".json")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, rerr := os.ReadFile(bodyPath); rerr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Info endpoints are reached via a plain GET to the proxy address
	// (not a forward-proxy/CONNECT request); use a direct client.
	base := "http://" + sess.ProxyListenAddr + "/recent/body"

	// Present ⇒ stream the spilled file as application/json (+nosniff),
	// bytes == sent body, sha == BodyRef.SHA256.
	hit, err := http.Get(base + "?session=" + sess.ID + "&id=" + rec.BodyRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer hit.Body.Close()
	if hit.StatusCode != http.StatusOK {
		t.Fatalf("present body GET status = %d, want 200", hit.StatusCode)
	}
	if ct := hit.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("present body Content-Type = %q, want application/json", ct)
	}
	if no := hit.Header.Get("X-Content-Type-Options"); no != "nosniff" {
		t.Fatalf("present body X-Content-Type-Options = %q, want nosniff", no)
	}
	gotBytes, err := io.ReadAll(hit.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBytes) != sentBody {
		t.Fatalf("streamed body mismatch:\n got %q\nwant %q", gotBytes, sentBody)
	}
	sum := sha256.Sum256(gotBytes)
	if got := hex.EncodeToString(sum[:]); got != rec.BodyRef.SHA256 {
		t.Fatalf("sha256(streamed) = %q, want BodyRef.SHA256 %q", got, rec.BodyRef.SHA256)
	}

	// Missing id ⇒ 404 with a JSON reason (not text/plain).
	miss, err := http.Get(base + "?session=" + sess.ID + "&id=does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("missing body GET status = %d, want 404", miss.StatusCode)
	}
	if ct := miss.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("miss Content-Type = %q, want application/json (not text/plain)", ct)
	}
	var reason map[string]any
	if err := json.NewDecoder(miss.Body).Decode(&reason); err != nil {
		t.Fatalf("miss body must be JSON, decode failed: %v", err)
	}
	if _, ok := reason["error"]; !ok {
		t.Fatalf("miss JSON must carry an \"error\" reason, got %#v", reason)
	}

	// --- CRITICAL: path-traversal / arbitrary-*.json read ---
	//
	// bodiesDir() == filepath.Join(status.RuntimeDir, "bodies"), and
	// load() does os.ReadFile(filepath.Join(bodiesDir(), id+".json")).
	// filepath.Join lexically cleans ".." WITHOUT containment, so a
	// crafted ?id= can escape the bodies/ dir and read any *.json on
	// disk (including other sessions' cleartext bodies).
	//
	// Deterministic proof: drop a sentinel ONE level above bodies/ at
	// <RuntimeDir>/sentinel.json. Pre-fix, id="../sentinel" resolves to
	//   filepath.Join(<RuntimeDir>/bodies, "../sentinel.json")
	//   == <RuntimeDir>/sentinel.json
	// so the endpoint streams the marker (test FAILS). Post-fix the
	// strict id check + load() containment make it a generic 404 and the
	// marker never appears (test PASSES).
	const sentinelMarker = "SENTINEL-LEAK-MARKER-7f3a9c"
	sentinelPath := filepath.Join(status.RuntimeDir, "sentinel.json")
	if err := os.WriteFile(sentinelPath, []byte(`{"secret":"`+sentinelMarker+`"}`), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	traversalPayloads := []string{
		"../sentinel",                       // deterministic: pre-fix reads the sentinel marker
		"../../../../../../../../etc/hosts", // classic deep escape
		"../" + rec.BodyRef.ID,              // sibling-escape form
		"%2e%2e%2f%2e%2e%2fetc%2fhosts",     // percent-encoded ../../ (decoded by r.URL.Query().Get)
		"foo/../../bar",                     // mid-path escape
		"/etc/hosts",                        // absolute path
	}
	for _, payload := range traversalPayloads {
		q := url.Values{}
		q.Set("session", sess.ID)
		q.Set("id", payload)
		tr, err := http.Get(base + "?" + q.Encode())
		if err != nil {
			t.Fatalf("traversal GET %q: %v", payload, err)
		}
		trBytes, _ := io.ReadAll(tr.Body)
		tr.Body.Close()

		if tr.StatusCode != http.StatusNotFound {
			t.Fatalf("traversal id=%q: status = %d, want 404 (path-traversal must not stream a file)", payload, tr.StatusCode)
		}
		if ct := tr.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("traversal id=%q: Content-Type = %q, want application/json", payload, ct)
		}
		var trReason map[string]any
		if err := json.Unmarshal(trBytes, &trReason); err != nil {
			t.Fatalf("traversal id=%q: body must be JSON, decode failed: %v (body=%q)", payload, err, trBytes)
		}
		if _, ok := trReason["error"]; !ok {
			t.Fatalf("traversal id=%q: JSON must carry an \"error\" reason, got %#v", payload, trReason)
		}
		// Belt-and-suspenders: no foreign file content may leak. The
		// sentinel marker and the legit captured body are the two
		// concrete things a traversal here could exfiltrate.
		if strings.Contains(string(trBytes), sentinelMarker) {
			t.Fatalf("traversal id=%q LEAKED the sentinel marker — path-traversal NOT closed:\n%s", payload, trBytes)
		}
		if strings.Contains(string(trBytes), sentBody) {
			t.Fatalf("traversal id=%q LEAKED the captured request body — path-traversal NOT closed:\n%s", payload, trBytes)
		}
	}

	// Happy path must still work after the guards: the legit hex id
	// streams 200 + exact bytes.
	again, err := http.Get(base + "?session=" + sess.ID + "&id=" + rec.BodyRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("post-guard legit body GET status = %d, want 200", again.StatusCode)
	}
	againBytes, err := io.ReadAll(again.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(againBytes) != sentBody {
		t.Fatalf("post-guard streamed body mismatch:\n got %q\nwant %q", againBytes, sentBody)
	}
}

// assertHeaderlessInvariant is the contract: NO recorded request that
// is synthetic or a blind-tunnel CONNECT may carry
// captured headers — capture exists only at the two decrypted
// ReverseProxy sites.
func assertHeaderlessInvariant(t *testing.T, requests []model.RequestRecord) {
	t.Helper()
	for _, rec := range requests {
		if rec.Synthetic || rec.Method == http.MethodConnect {
			if rec.RequestHeaders != nil {
				t.Fatalf("synthetic/CONNECT record must have nil RequestHeaders: %#v", rec)
			}
		}
	}
}

func TestBlindTunnelAndSyntheticHaveNoHeaders(t *testing.T) {
	// Scenario 1 — synthetic: in third-party-hidden mode, GET
	// /v1/models is answered synthetically (upstream not reached).
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()
	resp, err := hc.Get("https://api.anthropic.com/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	requests := waitForRequestRecord(t, client, sess.ID, "synthetic /v1/models record",
		func(rec model.RequestRecord) bool { return rec.Synthetic })
	assertHeaderlessInvariant(t, requests)

	// Scenario 2 — blind tunnel: no RouteClass ⇒ CCWRAP does not MITM;
	// the CONNECT is recorded with Method=CONNECT and no headers.
	paths := testutil.ShortAppPaths(t, "b.sock")
	tlsUpstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer tlsUpstream.Close()
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	bclient := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, bclient)
	bsess, err := bclient.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "blind"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bclient.SetRoute(context.Background(), bsess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}
	bhc := tlsUpstream.Client()
	tr, ok := bhc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", bhc.Transport)
	}
	clone := tr.Clone()
	clone.Proxy = http.ProxyURL(mustParse(t, "http://"+bsess.ProxyListenAddr))
	bhc.Transport = clone
	bresp, err := bhc.Get(tlsUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	bresp.Body.Close()
	brequests, err := bclient.Requests(context.Background(), bsess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawConnect := false
	for _, rec := range brequests {
		if rec.Method == http.MethodConnect {
			sawConnect = true
		}
	}
	if !sawConnect {
		t.Fatalf("expected a CONNECT (blind tunnel) record, got %#v", brequests)
	}
	assertHeaderlessInvariant(t, brequests)
}

// assertBodylessInvariant is the contract: NO recorded request that is
// synthetic or a blind-tunnel CONNECT may carry a
// captured BodyRef — capture exists ONLY at the two decrypted
// ReverseProxy sites (MITM pre-rewrite + forward), never for the
// blind-tunnel blind spot and never for synthetic/path-aware blocked
// responses. This is the body-side parallel of assertHeaderlessInvariant.
func assertBodylessInvariant(t *testing.T, requests []model.RequestRecord) {
	t.Helper()
	for _, rec := range requests {
		if rec.Synthetic || rec.Method == http.MethodConnect {
			if rec.BodyRef != nil {
				t.Fatalf("synthetic/CONNECT record must have nil BodyRef: %#v", rec)
			}
		}
	}
}

// TestBlindTunnelAndSyntheticHaveNoBodyRef is the exact body-side
// parallel of TestBlindTunnelAndSyntheticHaveNoHeaders, pinning the
// invariant against regression: even with the route's
// CaptureRequestBodies flipped ON, neither a synthetic/path-aware
// blocked response nor a blind-tunnel CONNECT may record a BodyRef, and
// NO body file may be spilled under <sessionRuntimeDir>/bodies/. The
// tap exists only at the two decrypted ReverseProxy sites, so this test
// is a guard (it must PASS) — a failure means capture leaked into the
// blind spot. Reuses the same real harness:
// headerInspectorSession for the synthetic scenario, a fresh New()
// supervisor with no RouteClass for the blind tunnel, the same control
// SetRoute path with CaptureRequestBodies:true (the only delta vs the
// header test), and client.Status().RuntimeDir to derive the bodies dir
// (never hardcoded).
func TestBlindTunnelAndSyntheticHaveNoBodyRef(t *testing.T) {
	// Scenario 1 — synthetic with capture ON: in third-party-hidden
	// mode, GET /v1/models is answered synthetically (upstream not
	// reached). Flip CaptureRequestBodies:true via the same control
	// SetRoute path TestRequestBodyCapturedToFileWhenEnabled uses, so
	// the no-capture invariant is exercised against an enabled route.
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
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
	resp, err := hc.Get("https://api.anthropic.com/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	requests := waitForRequestRecord(t, client, sess.ID, "synthetic /v1/models record",
		func(rec model.RequestRecord) bool { return rec.Synthetic })
	assertBodylessInvariant(t, requests)

	// No spilled body file may exist for the synthetic session.
	// <sessionRuntimeDir> is the supervisor's per-session RuntimeDir
	// (single-session supervisor); derive it via the control Status
	// endpoint — do not hardcode. The dir may be created lazily by an
	// unrelated path, so assert NO *.json entries rather than absence.
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertNoSpilledBodies(t, status.RuntimeDir)

	// Scenario 2 — blind tunnel with capture ON: no RouteClass ⇒ CCWRAP
	// does not MITM; the CONNECT is recorded with Method=CONNECT and no
	// body. CaptureRequestBodies:true is set so the invariant is pinned
	// against an enabled route (the blind-tunnel blind spot).
	paths := testutil.ShortAppPaths(t, "b.sock")
	tlsUpstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer tlsUpstream.Close()
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	bclient := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, bclient)
	bsess, err := bclient.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "blind"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bclient.SetRoute(context.Background(), bsess.ID, model.SessionRouteRequest{
		APIBaseURL:           "https://api.example.test",
		RouteSource:          model.RouteSourceExplicit,
		AuthMode:             model.AuthModePassthrough,
		AuthSource:           model.AuthSourceNone,
		ExactUpstreamHost:    "api.example.test",
		ExactUpstreamBase:    "https://api.example.test",
		FailPolicy:           model.FailClosed,
		CaptureRequestBodies: true,
	}); err != nil {
		t.Fatal(err)
	}
	bhc := tlsUpstream.Client()
	tr, ok := bhc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", bhc.Transport)
	}
	clone := tr.Clone()
	clone.Proxy = http.ProxyURL(mustParse(t, "http://"+bsess.ProxyListenAddr))
	bhc.Transport = clone
	bresp, err := bhc.Get(tlsUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	bresp.Body.Close()
	brequests, err := bclient.Requests(context.Background(), bsess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawConnect := false
	for _, rec := range brequests {
		if rec.Method == http.MethodConnect {
			sawConnect = true
		}
	}
	if !sawConnect {
		t.Fatalf("expected a CONNECT (blind tunnel) record, got %#v", brequests)
	}
	assertBodylessInvariant(t, brequests)

	bstatus, err := bclient.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertNoSpilledBodies(t, bstatus.RuntimeDir)
}

// assertNoSpilledBodies fails if any *.json body file exists under
// <runtimeDir>/bodies. The dir may be created lazily by an unrelated
// code path, so absence of the dir is fine; what must hold is that no
// body was ever spilled (bodyStore.put never called for
// blind-tunnel/synthetic).
func assertNoSpilledBodies(t *testing.T, runtimeDir string) {
	t.Helper()
	bodiesDir := filepath.Join(runtimeDir, "bodies")
	entries, err := os.ReadDir(bodiesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return // dir absent ⇒ nothing spilled, invariant holds
		}
		t.Fatalf("reading %s: %v", bodiesDir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("blind-tunnel/synthetic must not spill a body file, found %q in %s", e.Name(), bodiesDir)
		}
	}
}

func TestRecentJSONMasksCredentialsByDefault(t *testing.T) {
	// CONTRACT PROTECTION: /recent (like the page bootstrap and the SSE stream)
	// carries credential header values MASKED. The raw secret is removed
	// store-side in recordRequest before any record reaches the wire, so
	// curl /recent, a browser extension reading the bootstrap, or a shared HAR
	// can never lift a live bearer. Only the CCWRAP_UNMASK_CREDENTIALS=1 launch
	// opt-in serves raw. If this test fails because the raw secret reappeared
	// on /recent, that REOPENS the credential-on-the-wire hole — understand the
	// store-side masking (recordRequest + ui.MaskCredentialHeaders) before
	// "fixing" the test.
	client, sess, hc, upstream := headerInspectorSession(t, "r.sock")
	defer upstream.Close()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-RECENTSECRET")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The record lands only after the proxy handler returns, so /recent
	// polled immediately can win the race and show an empty ring. Wait via
	// the control surface first — same ring, same lock, so control-visible
	// implies /recent-visible.
	waitForRequestRecord(t, client, sess.ID, "forwarded /v1/messages request",
		func(rec model.RequestRecord) bool {
			return rec.Method == http.MethodPost && strings.Contains(rec.Path, "/v1/messages")
		})

	rresp, err := http.Get("http://" + sess.ProxyListenAddr + "/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer rresp.Body.Close()
	body, _ := io.ReadAll(rresp.Body)
	if strings.Contains(string(body), "sk-RECENTSECRET") {
		t.Fatalf("/recent leaked the raw credential (store-side masking regressed):\n%s", body)
	}
	// Masked, not dropped: the scheme + redaction marker survive.
	if !strings.Contains(string(body), "redacted") {
		t.Fatalf("/recent should carry the masked Authorization marker; got:\n%s", body)
	}
}

// TestRecentJSONCarriesBodyRefNeverInlineBody is a CONTRACT PROTECTION
// guard: the bounded /recent JSON carries body_ref METADATA
// (id/sha256/size) so a client can fetch the body out-of-band
// via /recent/body, but it must NEVER inline the captured body bytes
// into /recent itself. This passes today because model.RequestRecord
// holds only a *RequestBodyRef (reference: id/size/sha256/captured_at/
// truncated), no body-bytes field — /recent serializes records, so the
// sent body cannot appear. It is a regression guard (a sibling to
// TestRecentJSONMasksCredentialsByDefault): if it ever fails because
// the body got inlined into /recent, that BREAKS the bounded-/recent
// contract — understand the body_ref design before "fixing" the test.
//
// Harness mirrors TestRecentJSONMasksCredentialsByDefault exactly
// (headerInspectorSession + GET http://<ProxyAddr>/recent), only
// flipping the route's CaptureRequestBodies on via the same control
// SetRoute path TestRequestBodyCapturedToFileWhenEnabled uses, and
// reusing its client.Requests poll-for-BodyRef idiom. No new helpers.
func TestRecentJSONCarriesBodyRefNeverInlineBody(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "r.sock")
	defer upstream.Close()

	// Flip capture ON via the same control SetRoute path
	// TestRequestBodyCapturedToFileWhenEnabled uses (third-party-hidden
	// route to the httptest upstream), now with CaptureRequestBodies:true.
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           upstream.URL,
		RouteClass:           model.RouteClassThirdPartyHidden,
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

	// A unique, improbable marker embedded as a string value inside a
	// valid /v1/messages-ish JSON body. The distinctive prefix makes an
	// accidental substring match in /recent implausible.
	marker := fmt.Sprintf("CCWRAPBODYMARKER_neverinline_%d", time.Now().UnixNano())
	sentBody := fmt.Sprintf(`{"model":"claude-sonnet-4-6","system":[{"type":"text","text":%q}],"messages":[{"role":"user","content":%q}]}`, marker, marker)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(sentBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Poll client.Requests for the recorded /v1/messages request to have
	// a non-nil BodyRef (the spill writer is async), bounded ~2s, same
	// idiom as TestRequestBodyCapturedToFileWhenEnabled.
	var rec *model.RequestRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests, rerr := client.Requests(context.Background(), sess.ID)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for i := range requests {
			if requests[i].Method == http.MethodPost && strings.Contains(requests[i].Path, "/v1/messages") && requests[i].BodyRef != nil {
				rec = &requests[i]
				break
			}
		}
		if rec != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec == nil || rec.BodyRef == nil {
		t.Fatalf("capture ON: forwarded /v1/messages request must record a BodyRef within deadline, got nil")
	}

	rresp, err := http.Get("http://" + sess.ProxyListenAddr + "/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer rresp.Body.Close()
	body, _ := io.ReadAll(rresp.Body)
	recentStr := string(body)

	// (a) /recent MUST carry the body_ref metadata: the field name plus
	// the recorded ref's sha256 and size (as it appears in JSON).
	if !strings.Contains(recentStr, `"body_ref"`) {
		t.Fatalf("/recent must carry body_ref metadata, field absent.\n%s", recentStr)
	}
	if !strings.Contains(recentStr, rec.BodyRef.SHA256) {
		t.Fatalf("/recent must carry BodyRef.SHA256 %q.\n%s", rec.BodyRef.SHA256, recentStr)
	}
	if !strings.Contains(recentStr, fmt.Sprint(rec.BodyRef.Size)) {
		t.Fatalf("/recent must carry BodyRef.Size %d.\n%s", rec.BodyRef.Size, recentStr)
	}

	// (b) /recent MUST NOT inline the captured body bytes: the unique
	// marker (and a long substring of the sent body) must be absent.
	// Bodies are referenced via BodyRef and fetched out-of-band through
	// /recent/body — never inlined into the bounded /recent.
	if strings.Contains(recentStr, marker) {
		t.Fatalf("REGRESSION: /recent inlined the captured body — unique marker %q present. /recent must reference bodies via body_ref, never inline bytes. Do not weaken this guard.\n%s", marker, recentStr)
	}
	if strings.Contains(recentStr, `{"type":"text","text":"CCWRAPBODYMARKER_neverinline_`) {
		t.Fatalf("REGRESSION: /recent inlined a substring of the captured body. /recent must reference bodies via body_ref, never inline bytes.\n%s", recentStr)
	}
}

// TestSetRouteCarriesCaptureBodies locks the plumbing hop:
// SessionRouteRequest.CaptureRequestBodies must land on the session's
// routeConfig.captureBodies and be readable via the sessionProxy
// accessor, mirroring the existing Upstream/modelAlias route plumbing.
// Harness mirrors TestResolveUpstreamNormalizesAnthropicHost — no new
// helpers invented.
func TestSetRouteCarriesCaptureBodies(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "capbodies"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:           "https://api.anthropic.com",
		RouteSource:          model.RouteSourceExplicit,
		AuthMode:             model.AuthModePassthrough,
		AuthSource:           model.AuthSourceNone,
		FailPolicy:           model.FailClosed,
		CaptureRequestBodies: true,
	}); err != nil {
		t.Fatal(err)
	}

	state := srv.getSession(sess.ID)
	if state == nil || state.proxy == nil {
		t.Fatal("sessionProxy not found")
	}
	// The legacy state.l.captureBodies + captureBodiesEnabled()
	// reads observed live sess.route through the torn-read-prone path
	// the hot code no longer uses. The published immutable posture is
	// the source of truth for the proxy's routing reads; we assert via
	// the same atomic.Pointer the handlers read.
	ap := state.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after setRoute")
	}
	if !ap.l.captureBodies {
		t.Fatalf("posture.l.captureBodies = false, want true")
	}
}

// TestRingEvictionDeletesBodyFileAndCloseRemovesDir is the
// storage-lifecycle contract:
//
//  1. when a RequestRecord is evicted from the maxSessionRequests-deep
//     request ring, its spilled body file is deleted (if it had a
//     BodyRef);
//     1b. when an evicted record has *no* BodyRef (synthetic / blind-tunnel
//     / capture-off — the mixed-ring reality), the eviction hook's
//     `evicted.BodyRef != nil` guard skips the delete: no panic, no
//     spurious file op, residents untouched;
//  2. when the session is closed, the whole per-session bodies/ dir is
//     removed.
//
// Reuses the real New()+client.CreateSession harness and srv.getSession
// to reach the session's *bodyStore exactly as the sibling proxy tests
// do; derives <RuntimeDir>/bodies via the same control Status() path the
// body-capture test uses. Drives recordRequest/closeSession directly
// (unit level — the eviction/close hooks are independent of the proxy
// data path).
func TestRingEvictionDeletesBodyFileAndCloseRemovesDir(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil || state.bodies == nil {
		t.Fatal("sessionState/bodyStore not found")
	}

	// <RuntimeDir>/bodies/<id>.json — derive via control Status (single
	// -session supervisor; do not hardcode), same as the body-capture test.
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bodiesDir := filepath.Join(status.RuntimeDir, "bodies")
	bodyPath := func(id string) string { return filepath.Join(bodiesDir, id+".json") }

	// Push maxSessionRequests+2 records, each with its own spilled body.
	// Indices 0 and 1 are the two oldest — they get evicted by appendCapped
	// (over == 2). Spill via the session's own bodyStore (sess.bodies.put)
	// then record with the resulting BodyRef, exactly as the proxy path
	// does. Poll each async write before moving on (bodystore_test.go
	// idiom: 2s deadline + 10ms sleep).
	total := maxSessionRequests + 2
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("life-%04d", i)
		ids[i] = id
		payload := []byte(fmt.Sprintf(`{"model":"x","seq":%d}`, i))
		ref := state.bodies.put(id, payload)
		if ref == nil {
			t.Fatalf("bodyStore.put returned nil ref for %s", id)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, rerr := os.Stat(bodyPath(id)); rerr == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, rerr := os.Stat(bodyPath(id)); rerr != nil {
			t.Fatalf("spilled body file for %s never appeared: %v", id, rerr)
		}
		srv.recordRequest(sess.ID, model.RequestRecord{
			Timestamp:         time.Now(),
			SessionID:         sess.ID,
			Method:            http.MethodPost,
			LogicalTargetHost: "api.anthropic.com",
			Path:              "/v1/messages",
			BodyRef:           ref,
		})
	}

	// The ring is capped: RecentRequestCount must equal the cap, not the
	// number pushed (existing recordRequest behavior must be intact).
	st := srv.getSession(sess.ID)
	st.mu.Lock()
	gotCount := st.public.RecentRequestCount
	gotLen := len(st.requests)
	st.mu.Unlock()
	if gotCount != maxSessionRequests || gotLen != maxSessionRequests {
		t.Fatalf("ring not capped: RecentRequestCount=%d len(requests)=%d, want %d", gotCount, gotLen, maxSessionRequests)
	}

	// The two oldest (ids[0], ids[1]) were evicted: their body files must
	// be deleted by the ring-eviction hook (async — poll up to ~2s).
	for _, evictedID := range []string{ids[0], ids[1]} {
		deadline := time.Now().Add(2 * time.Second)
		gone := false
		for time.Now().Before(deadline) {
			if _, rerr := os.Stat(bodyPath(evictedID)); os.IsNotExist(rerr) {
				gone = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !gone {
			t.Fatalf("evicted record %s: body file %s still present (ring-eviction hook did not delete it)", evictedID, bodyPath(evictedID))
		}
	}

	// A still-resident record (the last one pushed) keeps its file.
	residentID := ids[total-1]
	if _, rerr := os.Stat(bodyPath(residentID)); rerr != nil {
		t.Fatalf("resident record %s: body file unexpectedly gone: %v", residentID, rerr)
	}

	// Mixed-ring reality (1b): not every record has a BodyRef — synthetic
	// responses, blind-tunneled hosts and capture-off requests are recorded
	// with no spilled file. The ring-eviction hook's `evicted.BodyRef !=
	// nil` guard (server.go) must skip the body-file delete for those.
	// Above, every evicted record had a BodyRef so the guard's nil branch
	// was never taken. Push maxSessionRequests+2 *no-body* records: every
	// at-cap push evicts sess.requests[0], and after the first
	// maxSessionRequests no-body pushes the ring is all no-body records, so
	// the subsequent evictions are no-body records — the guard's nil branch
	// is exercised on those calls. It must not panic, must not error, and
	// must not touch the filesystem (no body file is ever created for a
	// no-body record, and the still-resident no-body tail keeps none).
	noBodyTotal := maxSessionRequests + 2
	for i := 0; i < noBodyTotal; i++ {
		// No BodyRef, no spilled file — exactly how recordRequest is
		// called for synthetic/blind-tunnel/capture-off traffic.
		srv.recordRequest(sess.ID, model.RequestRecord{
			Timestamp: time.Now(),
			SessionID: sess.ID,
			Method:    http.MethodGet,
			Path:      "/healthz",
		})
	}
	// Ring still capped — recordRequest behavior is intact through the
	// nil-BodyRef eviction path (no panic / no error: a panic in the
	// eviction loop would have crashed the test goroutine here).
	st = srv.getSession(sess.ID)
	st.mu.Lock()
	gotCount = st.public.RecentRequestCount
	gotLen = len(st.requests)
	allNoBody := true
	for _, r := range st.requests {
		if r.BodyRef != nil {
			allNoBody = false
			break
		}
	}
	st.mu.Unlock()
	if gotCount != maxSessionRequests || gotLen != maxSessionRequests {
		t.Fatalf("ring not capped after no-body interleave: RecentRequestCount=%d len(requests)=%d, want %d", gotCount, gotLen, maxSessionRequests)
	}
	if !allNoBody {
		t.Fatalf("after %d no-body pushes the ring should hold only no-body records", noBodyTotal)
	}
	// End state: every phase-1 body-bearing record has been evicted (its
	// file deleted by the BodyRef!=nil branch) and no-body records never
	// create a file, so no *.json must remain. The two final evictions
	// were *no-body* records — the guard's nil branch skipped a delete
	// with no spurious file op. Poll: the last body-file deletes are async.
	deadline := time.Now().Add(2 * time.Second)
	noJSON := false
	for time.Now().Before(deadline) {
		entries, rerr := os.ReadDir(bodiesDir)
		if os.IsNotExist(rerr) {
			noJSON = true
			break
		}
		if rerr == nil {
			anyJSON := false
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					anyJSON = true
					break
				}
			}
			if !anyJSON {
				noJSON = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !noJSON {
		t.Fatalf("no-body eviction interleave left orphan *.json in %s (guard or delete path wrong)", bodiesDir)
	}

	// Closing the session removes the whole bodies/ dir (removeAll waits
	// for in-flight writers then RemoveAll — poll for it).
	if err := srv.closeSession(sess.ID, "test cleanup"); err != nil {
		t.Fatalf("closeSession: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	wiped := false
	for time.Now().Before(deadline) {
		if entries, rerr := os.ReadDir(bodiesDir); os.IsNotExist(rerr) {
			wiped = true
			break
		} else if rerr == nil {
			anyJSON := false
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					anyJSON = true
					break
				}
			}
			if !anyJSON {
				wiped = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !wiped {
		t.Fatalf("closeSession did not remove %s (or it still holds *.json)", bodiesDir)
	}
}

// TestRequestSSEEventCarriesClass is the contract: the request SSE
// payload must carry the Go-derived class top-level so the live JS
// never re-derives it (no Go↔JS drift with first paint). Reuses
// headerInspectorSession exactly as TestRequestHeadersCapturedWithCredentialsMasked
// does (third-party-hidden route to the httptest upstream): a POST to
// /v1/messages is forwarded (class "forwarded-api"); a POST to
// /api/event_logging/v2/batch is answered synthetically (class
// "synthetic"). The /events read loop mirrors TestSessionProxyEventsStream.
func TestRequestSSEEventCarriesClass(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "s.sock")
	defer upstream.Close()
	_ = client

	resp, err := http.Get("http://" + sess.ProxyListenAddr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// classByPath collects, per request event, the top-level "class" plus
	// proof the embedded model.RequestRecord fields are still flattened
	// (method/path/synthetic). Keyed by recorded path so we can assert the
	// forwarded vs synthetic record independently of arrival order.
	type seenEvent struct {
		class     string
		method    string
		path      string
		synthetic bool
	}
	gotCh := make(chan []seenEvent, 1)
	go func() {
		defer close(gotCh)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var seen []seenEvent
		curType := ""
		haveForwarded, haveSynthetic := false, false
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				curType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && curType == "request":
				// The SSE data: line is a model.Event envelope
				// ({id,type,time,session_id,data}); the request
				// payload (classifiedRecord) is ev.Data. The payload
				// flattens model.RequestRecord and adds "class" at its
				// top level — assert there.
				var env struct {
					Data struct {
						Class     string `json:"class"`
						Method    string `json:"method"`
						Path      string `json:"path"`
						Synthetic bool   `json:"synthetic"`
					} `json:"data"`
				}
				raw := strings.TrimPrefix(line, "data: ")
				if err := json.Unmarshal([]byte(raw), &env); err != nil {
					gotCh <- []seenEvent{{class: "UNMARSHAL-ERR: " + err.Error() + " :: " + raw}}
					return
				}
				payload := env.Data
				seen = append(seen, seenEvent{payload.Class, payload.Method, payload.Path, payload.Synthetic})
				if payload.Class == "forwarded-api" {
					haveForwarded = true
				}
				if payload.Class == "synthetic" {
					haveSynthetic = true
				}
				if haveForwarded && haveSynthetic {
					gotCh <- seen
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			gotCh <- []seenEvent{{class: "SCANNER-ERR: " + err.Error()}}
			return
		}
		gotCh <- seen
	}()

	// Give the SSE subscription time to attach before producing traffic
	// (same 50ms idiom as TestSessionProxyEventsStream).
	time.Sleep(50 * time.Millisecond)

	fwdReq, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	fwdReq.Header.Set("Content-Type", "application/json")
	fwdResp, err := hc.Do(fwdReq)
	if err != nil {
		t.Fatal(err)
	}
	fwdResp.Body.Close()

	synReq, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/api/event_logging/v2/batch", strings.NewReader(`{}`))
	synReq.Header.Set("Content-Type", "application/json")
	synResp, err := hc.Do(synReq)
	if err != nil {
		t.Fatal(err)
	}
	synResp.Body.Close()

	var seen []seenEvent
	select {
	case seen = <-gotCh:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for forwarded + synthetic request SSE events")
	}

	var fwd, syn *seenEvent
	for i := range seen {
		if strings.Contains(seen[i].path, "/v1/messages") {
			fwd = &seen[i]
		}
		if strings.Contains(seen[i].path, "/api/event_logging") {
			syn = &seen[i]
		}
	}
	if fwd == nil {
		t.Fatalf("no request SSE event for forwarded /v1/messages; saw %+v", seen)
	}
	if syn == nil {
		t.Fatalf("no request SSE event for synthetic /api/event_logging; saw %+v", seen)
	}

	// (a) top-level class is the Go-derived recordClass for each record.
	if fwd.class != "forwarded-api" {
		t.Fatalf("forwarded /v1/messages SSE class = %q, want %q (recordClass) — %+v", fwd.class, "forwarded-api", *fwd)
	}
	if syn.class != "synthetic" {
		t.Fatalf("synthetic /api/event_logging SSE class = %q, want %q (recordClass) — %+v", syn.class, "synthetic", *syn)
	}
	// Cross-check against recordClass itself so the assertion tracks the
	// single-source rule, not a hardcoded string.
	if want := recordClass(model.RequestRecord{Method: http.MethodPost, Path: fwd.path}); fwd.class != want {
		t.Fatalf("forwarded class %q != recordClass %q", fwd.class, want)
	}
	if want := recordClass(model.RequestRecord{Method: http.MethodPost, Path: syn.path, Synthetic: true}); syn.class != want {
		t.Fatalf("synthetic class %q != recordClass %q", syn.class, want)
	}
	// (b) the embedded model.RequestRecord is still flattened: method,
	// path, and the synthetic flag survive the classifiedRecord rewrap.
	if fwd.method != http.MethodPost || !strings.Contains(fwd.path, "/v1/messages") || fwd.synthetic {
		t.Fatalf("forwarded event lost embedded record fields: %+v", *fwd)
	}
	if syn.method != http.MethodPost || !strings.Contains(syn.path, "/api/event_logging") || !syn.synthetic {
		t.Fatalf("synthetic event lost embedded record fields (synthetic flag must remain true): %+v", *syn)
	}
}

// TestPerRequestApCaptureSurvivesMidHandlerStore is the deterministic
// torn-read guard. It pins the invariant that every routing read on the
// proxy hot path closes over the per-request captured
// *activePosture — a mid-handler sess.active.Store(B) (driven by the
// testHookAfterApCapture seam) MUST have zero effect on the in-flight
// request.
//
// The test exercises the MITM handler (the densest routing-read surface —
// third-party blocker, resolveUpstream, modelalias.RewriteRequest, transport
// selection, header + auth rewrite, capture-bodies gate, record stamping).
// The forward-proxy and blind-tunnel handlers place the same hook at the
// same structural position (immediately after ap := sess.active.Load(),
// BEFORE any routing branch), so the invariant proven here generalizes to
// those handlers by construction; the spec calls out the MITM-path test as
// sufficient ("the test only needs to exercise one to prove the invariant").
//
// Mechanism:
//  1. SetRoute under posture A: RouteClass=third_party_hidden +
//     OverrideAuth set + APIBaseURL pointing at a test upstream. Under A,
//     handleThirdPartySyntheticOrBlock synthesizes 204 for unknown paths
//     (e.g. /api/event_logging/v2/batch) — the request never reaches the
//     upstream.
//  2. Build posture B in test code: RouteClass=first_party + ZERO
//     OverrideAuth + a different egress mode. Under B, the same path would
//     be FORWARDED upstream (FirstParty → blocker returns false → reverse
//     proxy runs).
//  3. Set testHookAfterApCapture to sess.active.Store(B) — fires exactly
//     once per ServeHTTP entry, AFTER the handler captured A into a local,
//     and BEFORE any routing branch.
//  4. Issue an HTTPS request to api.anthropic.com (MITM intercepts).
//  5. Assertions:
//     - The response is 204 (synthetic decision held under A's RouteClass).
//     - The test upstream received ZERO requests (the captured-ap discipline
//     prevented B's first-party path from forwarding).
//     - sess.active.Load() equals B after the request (the hook fired —
//     the test would be vacuously passing if the hook hadn't run).
//
// If any routing read regressed to a lazy sess.route or sess.active.Load,
// the third-party blocker would observe B's first_party class, return false,
// and forward upstream → status 200, upstream count 1, test fails loudly.
func TestPerRequestApCaptureSurvivesMidHandlerStore(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	var upstreamRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "t4"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		OverrideAuth:      &model.AuthOverride{Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey, HeaderName: "X-API-Key", HeaderValue: "real"},
	}); err != nil {
		t.Fatal(err)
	}

	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatal("sessionState not found")
	}
	apA := state.active.Load()
	if apA == nil {
		t.Fatal("active.Load() == nil after SetRoute")
	}
	if apA.r.routeClass != model.RouteClassThirdPartyHidden {
		t.Fatalf("posture A routeClass = %q, want third_party_hidden", apA.r.routeClass)
	}

	// Posture B: distinct routeClass + zero overrideAuth + a tagged egress
	// mode so an accidental lazy-read on ANY of {routeClass, overrideAuth,
	// egress} immediately diverges from A's behavior. Reuses A's apiBaseURL so a
	// forwarded request under B would reach our test upstream (and increment the
	// counter we assert remains 0).
	apB := &posture{
		r: resolved{
			routeClass:      model.RouteClassFirstParty,
			authBootstrap:   apA.r.authBootstrap,
			apiBaseURL:      apA.r.apiBaseURL,
			overrideAuth:    nil, // contrasts with A's X-API-Key override
			egress:          model.EgressConfig{Mode: "direct-tagged-B", Source: "t4-test"},
			modelAlias:      apA.r.modelAlias,
			upstreamHeaders: apA.r.upstreamHeaders,
		},
		l: live{captureBodies: apA.l.captureBodies},
	}

	// Hook fires exactly once per ServeHTTP entry across all 3 handlers; here
	// the MITM handler will trigger it. Restore to nil at end so subsequent
	// tests aren't affected (parallel tests are safe — each runs its own
	// supervisor + the hook is conditioned on a routing decision that only
	// affects the request issued below).
	var hookFired int32 // 0/1, observed only via apA.Load() comparison below
	testHookAfterApCapture = func() {
		state.active.Store(apB)
		hookFired = 1
	}
	t.Cleanup(func() { testHookAfterApCapture = nil })

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
	// /api/event_logging/v2/batch is NOT a first-party synthetic shaped path
	// and NOT a third-party implemented gateway path, so:
	//   - under A (third_party_hidden): handleThirdPartySyntheticOrBlock
	//     returns true via writeSyntheticDefault204 → 204, no upstream hit.
	//   - under B (first_party): the blocker returns false → forwarded to
	//     upstream → 200 with the test server's body, upstream count == 1.
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/api/event_logging/v2/batch", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// (1) Response status proves the path-aware blocker decision used
	// posture A's routeClass (third_party_hidden ⇒ synthesize 204), not
	// posture B's (first_party ⇒ forward).
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("response status = %d body=%q, want 204 (proves A's routeClass held); a 200 here means routing read regressed to a lazy live load", resp.StatusCode, body)
	}
	if len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("response body = %q, want empty (synthetic 204)", body)
	}
	// (2) Zero upstream hits — the request never reached the test server.
	// Under B's posture this same path would have forwarded; the counter
	// would be 1.
	if upstreamRequests != 0 {
		t.Fatalf("upstream received %d requests; want 0 (proves the captured-ap discipline held — a forwarded request means a routing read observed B)", upstreamRequests)
	}
	// (3) The hook actually fired (otherwise (1) + (2) would be vacuously
	// satisfied because no Store(B) ever happened). After ServeHTTP returns,
	// sess.active.Load() MUST equal apB, not apA.
	if hookFired != 1 {
		t.Fatal("testHookAfterApCapture never fired — the seam isn't placed in the MITM handler entry")
	}
	if got := state.active.Load(); got != apB {
		t.Fatalf("after request, active.Load() = %p, want apB %p (hook should have Store(apB)d; if active still points at apA the hook is broken — and the (1)+(2) assertions would be vacuously satisfied)", got, apB)
	}
	// (4) Defense in depth: the recorded request also carries the
	// captured-A truth (synthetic flag + 204), not the live-B one.
	requests := waitForRequestRecord(t, client, sess.ID, "synthetic response record",
		func(rec model.RequestRecord) bool { return rec.Synthetic })
	last := requests[len(requests)-1]
	if !last.Synthetic {
		t.Fatalf("recorded request.Synthetic = false, want true (synthetic path under A); last = %+v", last)
	}
	if last.StatusCode != http.StatusNoContent {
		t.Fatalf("recorded request.StatusCode = %d, want 204; last = %+v", last.StatusCode, last)
	}
}

// TestDrainSupersededTransports exercises drainSupersededTransports(newKey):
// every cached transport whose egressTransportKey != newKey must be removed
// from sp.transports under sp.mu, and CloseIdleConnections() must run after
// the lock is released. The active-key transport is preserved by pointer
// equality; a subsequent upstreamTransport(oldCfg) call lazy-creates a FRESH
// *http.Transport (not the drained one).
func TestDrainSupersededTransports(t *testing.T) {
	_, sess := newTestSessionForCreate(t)
	sp := sess.proxy
	if sp == nil {
		t.Fatal("sessionState.proxy is nil")
	}

	// Two distinct EgressConfig values — egressTransportKey concatenates
	// Mode\x00HTTPProxy\x00HTTPSProxy\x00NoProxy\x00Source so any field
	// difference produces a distinct key.
	cfgA := model.EgressConfig{Mode: "direct", Source: "test-A"}
	cfgB := model.EgressConfig{Mode: "explicit", HTTPProxy: "http://proxy.example.com:3128", HTTPSProxy: "http://proxy.example.com:3128", Source: "test-B"}
	keyA := egressTransportKey(cfgA)
	keyB := egressTransportKey(cfgB)
	if keyA == keyB {
		t.Fatalf("setup: expected distinct keys for distinct configs, got %q == %q", keyA, keyB)
	}

	trA := sp.upstreamTransport(cfgA)
	trB := sp.upstreamTransport(cfgB)
	if trA == nil || trB == nil {
		t.Fatalf("setup: upstreamTransport returned nil (trA=%p, trB=%p)", trA, trB)
	}
	if trA == trB {
		t.Fatal("setup: expected distinct transports for distinct egress configs")
	}

	sp.mu.Lock()
	preCount := len(sp.transports)
	_, hasA := sp.transports[keyA]
	_, hasB := sp.transports[keyB]
	sp.mu.Unlock()
	if preCount < 2 || !hasA || !hasB {
		t.Fatalf("setup: expected both keyA and keyB cached (preCount=%d, hasA=%v, hasB=%v)", preCount, hasA, hasB)
	}

	// Drain everything that is not keyB. cfgA's transport must be evicted
	// AND have CloseIdleConnections() called on it; cfgB's transport must
	// survive at the same pointer.
	sp.drainSupersededTransports(keyB)

	sp.mu.Lock()
	postCount := len(sp.transports)
	got, present := sp.transports[keyB]
	_, stillHasA := sp.transports[keyA]
	sp.mu.Unlock()

	if postCount != 1 {
		t.Errorf("post-drain transports count = %d, want 1", postCount)
	}
	if stillHasA {
		t.Error("post-drain: keyA entry still present — drain skipped a superseded key")
	}
	if !present {
		t.Error("post-drain: keyB entry missing — drain removed the active transport")
	}
	if got != trB {
		t.Errorf("post-drain transports[keyB] = %p, want %p (active transport must be preserved)", got, trB)
	}

	// Cache-hit on the surviving key: same pointer.
	trB2 := sp.upstreamTransport(cfgB)
	if trB2 != trB {
		t.Errorf("post-drain upstreamTransport(cfgB) = %p, want %p (cache hit)", trB2, trB)
	}

	// Lazy-create on the drained key: a FRESH transport, not trA. Pointer
	// inequality is the indirect evidence that drain removed trA from the
	// cache (so the second call could not return the drained instance);
	// CloseIdleConnections() on trA was the side-effect that bounds the old
	// pool — by definition idempotent and safe-to-call on a now-orphan
	// transport.
	trA2 := sp.upstreamTransport(cfgA)
	if trA2 == nil {
		t.Fatal("post-drain upstreamTransport(cfgA) returned nil")
	}
	if trA2 == trA {
		t.Errorf("post-drain upstreamTransport(cfgA) = %p, want a fresh transport (drained %p reappeared)", trA2, trA)
	}
}

// The MITM "forwarding request" trace in proxy.go MUST strip userinfo
// from base.String() before writing it into the trace ring.
// The ap.r.apiBaseURL preserves whatever the launcher gave it (operator-
// set CCWRAP_API_BASE_URL → setRoute → url.Parse keeps u.User), and
// resolveUpstream clones that URL verbatim. Without the strip, an operator
// who put credentials in the base URL would see them in the public trace.
//
// Drives a real MITM round-trip: launches the supervisor, sets a route whose
// apiBaseURL points at the test httptest server but with embedded "u:secret@"
// userinfo, then sends a request through the proxy to api.anthropic.com (the
// MITM gate is isAnthropicAPIHost(logicalHost), so the logical host must be
// an Anthropic suffix). The expected trace.Detail is the userinfo-stripped
// form; the test fails loud if any leak token appears.
func TestMITMForwardingTraceStripsURLUserinfo(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "mitm-trace-strip"})
	if err != nil {
		t.Fatal(err)
	}
	upURL := mustParse(t, upstream.URL)
	// Embed userinfo on the apiBaseURL — preflight does NOT strip userinfo
	// on the route value (only the public projection is stripped);
	// resolveUpstream clones the URL verbatim, so base.String() at the
	// trace site carries the credential unless the userinfo strip is applied.
	apiBaseURLWithUserinfo := fmt.Sprintf("%s://op:secret@%s/v1", upURL.Scheme, upURL.Host)
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        apiBaseURLWithUserinfo,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: upURL.Hostname(),
		ExactUpstreamBase: apiBaseURLWithUserinfo,
		FailPolicy:        model.FailClosed,
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
	proxyURL := mustParse(t, "http://"+sess.ProxyListenAddr)
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}, Timeout: 5 * time.Second}
	// Hit any path on api.anthropic.com — the MITM gate is logical-host based
	// (isAnthropicAPIHost), and we only need the trace to fire.
	req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/ping", nil)
	resp, err := hc.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	// Don't gate on response — the trace is recorded before the upstream
	// reply (proxy.go fires immediately after resolveUpstream).

	waitForTrace(t, srv, sess.ID, "route", "forwarding request", 3*time.Second)

	traces := srv.listTrace(sess.ID)
	var found bool
	for _, te := range traces {
		if te.Category != "route" || !strings.Contains(te.Summary, "forwarding request") {
			continue
		}
		found = true
		// Leak tokens — any of these inside Detail means the userinfo strip regressed.
		if strings.Contains(te.Detail, "op:secret@") || strings.Contains(te.Detail, "secret") || strings.Contains(te.Detail, "op@") {
			t.Errorf("trace.Detail leaks userinfo: %q", te.Detail)
		}
		// Host must survive the strip (only the userinfo is removed).
		if !strings.Contains(te.Detail, upURL.Host) {
			t.Errorf("trace.Detail lost host info after strip: %q", te.Detail)
		}
		// Format-string preamble survives.
		if !strings.Contains(te.Detail, "api.anthropic.com -> ") {
			t.Errorf("trace.Detail lost logicalHost preamble: %q", te.Detail)
		}
		break
	}
	if !found {
		t.Fatalf("no MITM 'forwarding request' trace found; got %d traces", len(traces))
	}
}

func TestHandleInfoRecent_IncludesActiveProfileFields(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "recap.sock")
	defer upstream.Close()
	_ = client
	url := "http://" + sess.ProxyListenAddr + "/recent"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var payload struct {
		Session map[string]json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode /recent: %v\n  body: %s", err, body)
	}
	if _, ok := payload.Session["active_profile_name"]; !ok {
		t.Fatalf("/recent.session missing active_profile_name key (body: %s)", body)
	}
	if _, ok := payload.Session["active_profile_provider"]; !ok {
		t.Fatalf("/recent.session missing active_profile_provider key (body: %s)", body)
	}
	// capture_bodies key MUST be present on /recent (not just bootstrap/SSE)
	// — JS re-bootstrap on SSE-disconnect-reconnect calls /recent and reads
	// session.capture_bodies to re-render the Bodies cell. The field-by-field
	// map in handleInfoRecent doesn't auto-serialize new model.Session fields,
	// so this guards against a future field-on-model-but-missing-on-/recent drift.
	if _, ok := payload.Session["capture_bodies"]; !ok {
		t.Fatalf("/recent.session missing capture_bodies key (body: %s)", body)
	}
	if _, ok := payload.Session["capture_telemetry"]; !ok {
		t.Fatalf("/recent.session missing capture_telemetry key (body: %s)", body)
	}
	if _, ok := payload.Session["native_tls"]; !ok {
		t.Fatalf("/recent.session missing native_tls key (body: %s)", body)
	}
	// Same guard for the CCWRAP_UNMASK_CREDENTIALS marker — without this field
	// the inspect ribbon would re-render to bodies-on after a reconnect,
	// hiding the danger-color UNMASKED state even though the supervisor is
	// still unmask-on. The reconnect path MUST surface the launch-fixed flag.
	if _, ok := payload.Session["capture_bodies_unmasked"]; !ok {
		t.Fatalf("/recent.session missing capture_bodies_unmasked key (body: %s)", body)
	}
}

func TestHandleProfileCatalog_NoFile(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "pcat-nofile-h.sock")
	defer upstream.Close()
	_ = client
	url := "http://" + sess.ProxyListenAddr + "/profile/catalog"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"has_profiles_file":false`) {
		t.Fatalf("no-file response shape: %s", body)
	}
	if !strings.Contains(string(body), `"items":[]`) {
		t.Fatalf("items must be empty array not null: %s", body)
	}
}

func TestHandleProfileCatalog_RejectsWrongMethod(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "pcat-method.sock")
	defer upstream.Close()
	_ = client
	url := "http://" + sess.ProxyListenAddr + "/profile/catalog"
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// headerInspectorSessionWithSupervisor mirrors headerInspectorSession but
// returns the *Supervisor instance too, so tests can reach into private
// state (sessionState.profileToken). Test-only.
func headerInspectorSessionWithSupervisor(t *testing.T, sockName string) (*Supervisor, *control.Client, *model.Session, *http.Client, *httptest.Server) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, sockName)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "content": []any{}})
	}))
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "hdr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
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
	return srv, client, sess, hc, upstream
}

func TestHandleProfileSwitch_RejectsMissingToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "psw-notok.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/switch"
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

func TestHandleProfileSwitch_RejectsWrongToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "psw-wrongtok.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/switch"
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

func TestHandleProfileSwitch_AcceptsRightToken_ReturnsSwitchOutcome(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "psw-rt.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatalf("session not found")
	}
	url := "http://" + sess.ProxyListenAddr + "/profile/switch"
	body := strings.NewReader(`{"name":""}`)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, respBody)
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode outcome: %v body=%s", err, respBody)
	}
	if out.Result == "" {
		t.Fatalf("outcome.result empty: %s", respBody)
	}
}

// TestHandleProfileSwitch_BodyNeverDecodedOnCSRFMiss asserts the
// defense-in-depth contract: on missing/wrong CSRF token, the handler
// returns 403 BEFORE consuming or parsing the body. We assert this by
// sending a malformed JSON body alongside a missing token — if the
// handler decoded first, we'd see 400 "invalid request body"; if it
// checks CSRF first, we see 403 regardless of body shape.
func TestHandleProfileSwitch_BodyNeverDecodedOnCSRFMiss(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "psw-csrf-body.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/switch"
	body := strings.NewReader(`{"name": NOT VALID JSON`)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF must check before body decode)", resp.StatusCode)
	}
}

// TestHandleCaptureBodies_RejectsWrongMethod confirms GET/HEAD/PUT/PATCH/DELETE
// to /capture/bodies return 405 (the method-aware dispatch matches the
// path before /404 fall-through, mirroring /profile/switch's contract).
func TestHandleCaptureBodies_RejectsWrongMethod(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "cb-405.sock")
	defer upstream.Close()
	_ = srv
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, _ := http.NewRequest(m, "http://"+sess.ProxyListenAddr+"/capture/bodies", nil)
		resp, err := hc.Transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /capture/bodies status = %d, want 405", m, resp.StatusCode)
		}
	}
}

// TestHandleCaptureBodies_RejectsMissingToken locks the CSRF guard: a POST
// without X-CCWRAP-Profile-Token must be refused with 403 before body decode.
func TestHandleCaptureBodies_RejectsMissingToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "cb-notok.sock")
	defer upstream.Close()
	_ = srv
	body := strings.NewReader(`{"enable":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/bodies", body)
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

// TestHandleCaptureBodies_RejectsWrongToken — defense-in-depth for the
// constant-time compare path: any non-matching token must 403.
func TestHandleCaptureBodies_RejectsWrongToken(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "cb-wrongtok.sock")
	defer upstream.Close()
	_ = srv
	body := strings.NewReader(`{"enable":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/bodies", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", strings.Repeat("0", 32))
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestHandleCaptureBodies_MissingEnable returns 400 when the required field
// is absent. This guards against an accidental empty body silently disabling
// capture (the *bool sentinel encodes "absent" vs "explicit false").
func TestHandleCaptureBodies_MissingEnable(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "cb-noenable.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatalf("session not found")
	}
	// Empty JSON object — valid JSON, but no `enable` field.
	body := strings.NewReader(`{}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/bodies", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing enable", resp.StatusCode)
	}
}

// TestHandleCaptureBodies_BodyNeverDecodedOnCSRFMiss — defense-in-depth: when
// CSRF fails, the handler must not parse the JSON body (mirrors the
// /profile/switch contract). We send malformed JSON without a token — if the
// handler decoded first, we'd see 400; if CSRF runs first, we see 403.
func TestHandleCaptureBodies_BodyNeverDecodedOnCSRFMiss(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "cb-csrf-body.sock")
	defer upstream.Close()
	_ = srv
	body := strings.NewReader(`{"enable":  NOT JSON`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/bodies", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF must check before body decode)", resp.StatusCode)
	}
}

// TestHandleCaptureBodies_RoundTrip — happy path: POST {enable:true} returns
// 200 {enabled:true}, AND the session state actually flipped (sess.public
// .CaptureBodies and ap.l.captureBodies both updated). Then POST
// {enable:false} flips back.
func TestHandleCaptureBodies_RoundTrip(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "cb-rt.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatalf("session not found")
	}
	post := func(enable bool) (int, bool) {
		body := strings.NewReader(fmt.Sprintf(`{"enable":%t}`, enable))
		req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/bodies", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
		resp, err := hc.Transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf, _ := io.ReadAll(resp.Body)
		var out struct {
			Enabled bool `json:"enabled"`
		}
		if jerr := json.Unmarshal(buf, &out); jerr != nil {
			t.Fatalf("decode capture response: %v; body=%s", jerr, buf)
		}
		return resp.StatusCode, out.Enabled
	}
	// 1. Enable
	if status, enabled := post(true); status != 200 || !enabled {
		t.Fatalf("POST enable=true: status=%d enabled=%v", status, enabled)
	}
	if !state.active.Load().l.captureBodies {
		t.Errorf("after enable: ap.l.captureBodies = false, want true")
	}
	// 2. Disable
	if status, enabled := post(false); status != 200 || enabled {
		t.Fatalf("POST enable=false: status=%d enabled=%v", status, enabled)
	}
	if state.active.Load().l.captureBodies {
		t.Errorf("after disable: ap.l.captureBodies = true, want false")
	}
}

// TestHandleCaptureTelemetryEndpoint mirrors the /capture/bodies endpoint
// contract for the telemetry toggle: (1) a valid POST {enable:true} with the
// correct CSRF token returns 200 {enabled:true} and actually flips the live
// session state; (2) a missing/invalid token returns 403; (3) a non-POST
// method returns 405; (4) a POST with no enable field returns 400.
func TestHandleCaptureTelemetryEndpoint(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "ct-ep.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatalf("session not found")
	}

	// (1) Valid POST {enable:true} with the correct token -> 200 {enabled:true}.
	body := strings.NewReader(`{"enable":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/telemetry", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	buf, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out struct {
		Enabled bool `json:"enabled"`
	}
	if jerr := json.Unmarshal(buf, &out); jerr != nil {
		t.Fatalf("decode capture response: %v; body=%s", jerr, buf)
	}
	if resp.StatusCode != http.StatusOK || !out.Enabled {
		t.Fatalf("POST enable=true: status=%d enabled=%v", resp.StatusCode, out.Enabled)
	}
	if !state.active.Load().l.captureTelemetry {
		t.Errorf("after enable: ap.l.captureTelemetry = false, want true")
	}

	// (2) Missing token -> 403.
	body = strings.NewReader(`{"enable":true}`)
	req, _ = http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/telemetry", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err = hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing token: status = %d, want 403", resp.StatusCode)
	}

	// (3) Non-POST (GET) -> 405.
	req, _ = http.NewRequest(http.MethodGet, "http://"+sess.ProxyListenAddr+"/capture/telemetry", nil)
	resp, err = hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /capture/telemetry: status = %d, want 405", resp.StatusCode)
	}

	// (4) POST with no enable field -> 400.
	body = strings.NewReader(`{}`)
	req, _ = http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/capture/telemetry", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err = hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing enable: status = %d, want 400", resp.StatusCode)
	}
}

// TestHandleAnthropicMITM_RefusesWhenAuthMissing — request-time
// fail-closed. SetRoute publishes a posture with AuthBootstrap=Missing
// + OverrideAuth=nil + MissingAuthEnv="ACME_TOKEN" (Case A); a CONNECT to
// api.anthropic.com + GET /v1/messages must return 502 with the
// ccwrap_auth_missing JSON body and the upstream must NOT be called.
//
// Replaces the preflight-launch-time refusal — the invariant
// (no un-authed forward) is preserved at the layer where it actually
// applies (the forward boundary).
func TestHandleAnthropicMITM_RefusesWhenAuthMissing(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "amm.sock")
	var upstreamRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "amm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		AuthPolicy:        model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap:     model.AuthBootstrapMissing, // ← the gate trigger
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		OverrideAuth:      nil, // intentional — Missing implies nothing to inject
		ActiveProfileName: "local",
		MissingAuthEnv:    "ACME_TOKEN", // Case A
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
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := hc.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-3-5-haiku","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body=%s", resp.StatusCode, body)
	}
	if upstreamRequests != 0 {
		t.Errorf("upstream got %d requests; want 0 (gate must not forward)", upstreamRequests)
	}
	var errResp authMissingErrorBody
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, body)
	}
	if errResp.Type != "error" {
		t.Errorf("error body type = %q, want 'error'", errResp.Type)
	}
	if errResp.Error.Type != "ccwrap_auth_missing" {
		t.Errorf("error.type = %q, want 'ccwrap_auth_missing'", errResp.Error.Type)
	}
	if errResp.Error.Profile != "local" {
		t.Errorf("error.profile = %q, want 'local'", errResp.Error.Profile)
	}
	if errResp.Error.EnvVar != "ACME_TOKEN" {
		t.Errorf("error.env_var = %q, want 'ACME_TOKEN'", errResp.Error.EnvVar)
	}
	if !strings.Contains(errResp.Error.Message, "ACME_TOKEN") {
		t.Errorf("error.message should name the env; got %q", errResp.Error.Message)
	}
}

// TestSessionProxy_ProfileTest_HappyPath exercises POST /profile/test
// end-to-end through the proxy dispatch (proxy.go case "/profile/test").
// Complements handle_profile_probe_test.go's handler-level tests by
// living next to the other proxy integration tests (e.g.
// TestHandleProfileSwitch_AcceptsRightToken_ReturnsSwitchOutcome).
func TestSessionProxy_ProfileTest_HappyPath(t *testing.T) {
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer upstream2.Close()
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-integ-ok.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	writeProfilesJSON(t, srv, fmt.Sprintf(`{"profiles":{"glm":{"base_url":%q,"auth":{"mode":"ccwrap_x_api_key","key":"sk-fake"}}}}`, upstream2.URL))

	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	body := strings.NewReader(`{"name":"glm"}`)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, respBody)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "OK" {
		t.Errorf("status field: got %v, want OK", got["status"])
	}
	if got["profile"] != "glm" {
		t.Errorf("profile field: got %v, want glm", got["profile"])
	}
}

// TestSessionProxy_ProfileTest_CSRFRejection exercises the CSRF guard
// through the proxy dispatch. Confirms the dispatch arm doesn't bypass
// the handler's CSRF check.
func TestSessionProxy_ProfileTest_CSRFRejection(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "pte-integ-csrf.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/test"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"glm"}`))
	req.Header.Set("Content-Type", "application/json")
	// NO X-CCWRAP-Profile-Token header.
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestSessionProxy_EgressTest_HappyPath — exercises POST /profile/test-egress
// through the proxy mux end-to-end. Companion of
// TestSessionProxy_ProfileTest_HappyPath; covers the route dispatch
// in proxy.go::case "/profile/test-egress".
func TestSessionProxy_EgressTest_HappyPath(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ip":"1.2.3.4","country":"US"}`))
	}))
	defer stub.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", stub.URL)

	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "psp-egress-ok.sock")
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
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s, want 200", resp.StatusCode, string(body))
	}
}

// TestSessionProxy_EgressTest_CSRFRejection — POST /profile/test-egress
// without the X-CCWRAP-Profile-Token header must return 403 at the
// handler level (defense-in-depth, same as /profile/test).
func TestSessionProxy_EgressTest_CSRFRejection(t *testing.T) {
	srv, _, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "psp-egress-csrf.sock")
	defer upstream.Close()
	_ = srv
	url := "http://" + sess.ProxyListenAddr + "/profile/test-egress"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"any"}`))
	req.Header.Set("Content-Type", "application/json")
	// NO X-CCWRAP-Profile-Token header.
	resp, err := hc.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestNativeTLSInfoEndpoint(t *testing.T) {
	sp := &sessionProxy{session: &sessionState{}}

	// nil (pre-capture) -> captured:false, no panic.
	rec := httptest.NewRecorder()
	sp.handleNativeTLSInfo(rec, httptest.NewRequest("GET", "/native-tls", nil))
	if !strings.Contains(rec.Body.String(), `"captured":false`) {
		t.Errorf("nil state must report captured:false, got %s", rec.Body.String())
	}

	// captured -> fingerprints + hex; no secret leak.
	raw, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatal(err)
	}
	cp := append([]byte(nil), raw...)
	sp.session.mirroredHelloRaw.Store(&cp)
	rec = httptest.NewRecorder()
	sp.handleNativeTLSInfo(rec, httptest.NewRequest("GET", "/native-tls", nil))
	body := rec.Body.String()
	for _, want := range []string{`"ja3":"983846581fdb62fafdb21d2282592c57"`, `"ja4":"t13d5212h1`, `"peetprint":"20e60f2e`, `"captured":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	for _, secret := range []string{"sk-ant", "Bearer"} {
		if strings.Contains(body, secret) {
			t.Errorf("endpoint leaked %q", secret)
		}
	}
}

func TestNativeTLSInfoLoadedSource(t *testing.T) {
	sp := &sessionProxy{session: &sessionState{}}
	sp.session.nativeTLSHelloLoaded = true
	raw, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatal(err)
	}
	cp := append([]byte(nil), raw...)
	sp.session.mirroredHelloRaw.Store(&cp)
	rec := httptest.NewRecorder()
	sp.handleNativeTLSInfo(rec, httptest.NewRequest("GET", "/native-tls", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"source":"loaded"`) {
		t.Errorf("want source:loaded, got %s", body)
	}
	if !strings.Contains(body, "pinned") {
		t.Errorf("loaded note must say pinned, got %s", body)
	}
}

func TestNativeTLSClientHelloDownload(t *testing.T) {
	sp := &sessionProxy{session: &sessionState{}}
	// nil -> 404
	rec := httptest.NewRecorder()
	sp.handleNativeTLSClientHello(rec, httptest.NewRequest("GET", "/native-tls/clienthello.bin", nil))
	if rec.Code != 404 {
		t.Fatalf("nil download code=%d want 404", rec.Code)
	}
	// captured -> byte-equal, octet-stream
	raw, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatal(err)
	}
	cp := append([]byte(nil), raw...)
	sp.session.mirroredHelloRaw.Store(&cp)
	rec = httptest.NewRecorder()
	sp.handleNativeTLSClientHello(rec, httptest.NewRequest("GET", "/native-tls/clienthello.bin", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type=%q want application/octet-stream", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Error("served bytes != stored bytes")
	}
}
