package supervisor

import (
	"bytes"
	"context"
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

// droppingUpstream answers 200 and closes the connection halfway through
// the body, so the proxy's copy of the response fails mid-stream.
func droppingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, bw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"partial\":")
		_ = bw.Flush()
		_ = c.Close()
	}))
	t.Cleanup(up.Close)
	return up
}

// orphanBodies lists the captured-body files no request record points to.
func orphanBodies(t *testing.T, runtimeDir string, recs []model.RequestRecord) []string {
	t.Helper()
	refs := map[string]bool{}
	for _, r := range recs {
		for _, ref := range []*model.RequestBodyRef{r.BodyRef, r.UpstreamBodyRef, r.ResponseBodyRef} {
			if ref != nil {
				refs[ref.ID+".json"] = true
			}
		}
	}
	time.Sleep(200 * time.Millisecond) // body files are written asynchronously
	entries, _ := os.ReadDir(filepath.Join(runtimeDir, "bodies"))
	var orphans []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !refs[e.Name()] {
			orphans = append(orphans, e.Name())
		}
	}
	return orphans
}

// When a response breaks off mid-stream (the upstream dropped it, or Claude
// Code was interrupted), ReverseProxy aborts the handler. The request is
// still recorded, so it shows in Activity and its captured body stays
// referenced, evicted with it, instead of lying orphaned until the session
// ends.
func TestAbortedRequestIsRecordedWithItsCapturedBody(t *testing.T) {
	up := droppingUpstream(t)
	paths := testutil.ShortAppPaths(t, "ab.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "abort"})
	if err != nil {
		t.Fatal(err)
	}
	route := xAPIKeyRoute(up.URL, model.EgressConfig{Mode: "direct"})
	route.CaptureRequestBodies = true
	if err := client.SetRoute(context.Background(), sess.ID, route); err != nil {
		t.Fatal(err)
	}
	claudeSideClient(t, paths.CABundlePath, sess)(claudeHead("/v1/messages", "2"), `{"model":"claude-sonnet-5-5"}`)

	recs := waitForRequestRecord(t, client, sess.ID, "aborted /v1/messages", func(r model.RequestRecord) bool {
		return strings.HasPrefix(r.Path, "/v1/messages")
	})
	for _, r := range recs {
		if strings.HasPrefix(r.Path, "/v1/messages") && r.BodyRef == nil {
			t.Fatal("the aborted request was recorded without its captured body")
		}
	}
	if o := orphanBodies(t, paths.RuntimeDir, srv.listRequests(sess.ID)); len(o) > 0 {
		t.Fatalf("captured body files with no request record: %v", o)
	}
}

// The same holds for telemetry captured through the MITM.
func TestAbortedTelemetryRequestIsRecordedWithItsCapturedBody(t *testing.T) {
	up := droppingUpstream(t)
	testHookTelemetryUpstream = func(string) string { return up.URL }
	defer func() { testHookTelemetryUpstream = nil }()
	srv, client, sess, hc, upstream := headerInspectorSessionWithSupervisor(t, "ab-tele.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	apCopy := *state.active.Load()
	apCopy.l.captureTelemetry = true
	state.active.Store(&apCopy)

	req, _ := http.NewRequest(http.MethodPost, "https://http-intake.logs.us5.datadoghq.com/api/v2/logs", bytes.NewReader([]byte(`{"event":"x"}`)))
	if resp, err := hc.Do(req); err == nil {
		_ = resp.Body.Close()
	}

	recs := waitForRequestRecord(t, client, sess.ID, "aborted telemetry POST", func(r model.RequestRecord) bool {
		return r.LogicalTargetHost == "http-intake.logs.us5.datadoghq.com"
	})
	_ = recs
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if o := orphanBodies(t, status.RuntimeDir, srv.listRequests(sess.ID)); len(o) > 0 {
		t.Fatalf("captured body files with no request record: %v", o)
	}
}

func TestServeAbortable(t *testing.T) {
	serve := func(h http.HandlerFunc) bool {
		return serveAbortable(h, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if serve(func(http.ResponseWriter, *http.Request) {}) {
		t.Fatal("a handler that returned was reported as aborted")
	}
	if !serve(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }) {
		t.Fatal("an ErrAbortHandler panic was not reported as aborted")
	}
	defer func() {
		if p := recover(); p != "boom" {
			t.Fatalf("another panic must go on as it was, got %v", p)
		}
	}()
	serve(func(http.ResponseWriter, *http.Request) { panic("boom") })
	t.Fatal("another panic was swallowed")
}
