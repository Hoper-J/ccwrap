package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// egress-test probes the egress the session's posture resolved to — not the
// launcher flag or the environment — and honors the posture's NO_PROXY.
func TestAPIv1EgressTestFollowsThePosture(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ip":"42.42.42.42"}`)
	}))
	defer target.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", target.URL)
	srv, _, sess, _, upstream := headerInspectorSessionWithSupervisor(t, "v1-egress.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)

	// A SOCKS5h exit that only the posture can name.
	state.active.Store(&posture{r: resolved{egress: model.EgressConfig{
		Mode: "explicit", HTTPSProxy: "socks5h://posture-proxy.test:1080", HTTPProxy: "socks5h://posture-proxy.test:1080",
		Source: "test_posture", Summary: "socks5h://posture-proxy.test:1080",
	}}})
	_, got := apiCall(t, srv.paths.SocketPath, "POST", "/api/v1/egress-test", "")
	if via, _ := got["egress_via"].(string); got["profile"] != "<active-session>" || !strings.Contains(via, "posture-proxy.test:1080") {
		t.Fatalf("the probe did not follow the posture: %v", got)
	}

	// The target's host is in the posture's NO_PROXY, so the probe goes
	// direct instead of through a proxy that refuses every connection.
	host, _, _ := strings.Cut(strings.TrimPrefix(target.URL, "http://"), ":")
	state.active.Store(&posture{r: resolved{egress: model.EgressConfig{
		Mode: "explicit", HTTPSProxy: "http://127.0.0.1:1", HTTPProxy: "http://127.0.0.1:1", NoProxy: host,
		Source: "test_posture", Summary: "http://127.0.0.1:1",
	}}})
	if _, got := apiCall(t, srv.paths.SocketPath, "POST", "/api/v1/egress-test", ""); got["status"] != "OK" {
		t.Fatalf("NO_PROXY not honored: %v", got)
	}
}

// capture flips the live posture's body and telemetry capture, each on its
// own, both ways.
func TestAPIv1CaptureTogglesTheLivePosture(t *testing.T) {
	srv, _, sess, _, upstream := headerInspectorSessionWithSupervisor(t, "v1-capture.sock")
	defer upstream.Close()
	state := srv.getSession(sess.ID)
	live := func() (bodies, telemetry bool) {
		l := state.active.Load().l
		return l.captureBodies, l.captureTelemetry
	}
	for _, step := range []struct {
		body              string
		bodies, telemetry bool
	}{
		{`{"bodies":true}`, true, false},
		{`{"telemetry":true}`, true, true},
		{`{"bodies":false}`, false, true},
		{`{"telemetry":false}`, false, false},
	} {
		if status, out := apiCall(t, srv.paths.SocketPath, "POST", "/api/v1/capture", step.body); status != 200 {
			t.Fatalf("%s: %d %v", step.body, status, out)
		}
		if b, tel := live(); b != step.bodies || tel != step.telemetry {
			t.Fatalf("after %s: bodies=%v telemetry=%v, want %v %v", step.body, b, tel, step.bodies, step.telemetry)
		}
	}
}

// apiCall sends one request to the session's v1 API over its control socket.
func apiCall(t *testing.T, socket, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, "http://unix"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: control.UnixTransport(socket), Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// A routed session answers meta, session (with display) and activity, and
// refuses bad input with a v1 error.
func TestAPIv1ReadsSession(t *testing.T) {
	up := diagUpstream(t, false)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"}))
	send := messagesClient(t, paths.CABundlePath, sess)
	send("claude-opus-5-5")
	waitForRequestRecord(t, control.NewClient(paths.SocketPath), sess.ID, "the request", func(model.RequestRecord) bool { return true })

	if status, meta := apiCall(t, paths.SocketPath, "GET", "/api/v1/meta", ""); status != 200 || meta["api"] != "v1" || meta["session_id"] != sess.ID {
		t.Fatalf("meta: %d %v", status, meta)
	}
	status, got := apiCall(t, paths.SocketPath, "GET", "/api/v1/session", "")
	disp, _ := got["display"].(map[string]any)
	cells, _ := disp["cells"].([]any)
	if status != 200 || got["id"] != sess.ID || len(cells) < 3 || cells[0].(map[string]any)["value"] == "" || cells[2].(map[string]any)["value"] != "1 alias" {
		t.Fatalf("session: %d %v", status, got)
	}
	status, act := apiCall(t, paths.SocketPath, "GET", "/api/v1/activity", "")
	reqs, _ := act["requests"].([]any)
	if status != 200 || len(reqs) != 1 || reqs[0].(map[string]any)["class"] != "forwarded-api" {
		t.Fatalf("activity: %d %v", status, act)
	}
	if _, ok := act["errors"]; !ok {
		t.Fatal("activity without kind carries errors too")
	}
	status, onlyTrace := apiCall(t, paths.SocketPath, "GET", "/api/v1/activity?kind=trace&limit=1", "")
	if tr, _ := onlyTrace["trace"].([]any); status != 200 || len(tr) != 1 || onlyTrace["requests"] != nil {
		t.Fatalf("activity kind=trace limit=1: %d %v", status, onlyTrace)
	}
	for _, q := range []string{"kind=nope", "limit=0", "limit=x"} {
		if status, out := apiCall(t, paths.SocketPath, "GET", "/api/v1/activity?"+q, ""); status != 400 || errCode(out) != "usage" {
			t.Errorf("activity?%s: %d %v", q, status, out)
		}
	}
	if status, out := apiCall(t, paths.SocketPath, "GET", "/api/v1/nope", ""); status != 404 || errCode(out) != "not_found" {
		t.Errorf("unknown endpoint: %d %v", status, out)
	}
	if status, out := apiCall(t, paths.SocketPath, "DELETE", "/api/v1/session", ""); status != http.StatusMethodNotAllowed || errCode(out) != "method_not_allowed" {
		t.Errorf("wrong method: %d %v", status, out)
	}
	if status, out := apiCall(t, paths.SocketPath, "GET", "/api/v1/bodies/0123456789abcdef", ""); status != 404 || out["error"] == nil {
		t.Errorf("missing body: %d %v", status, out)
	}
}

// capture toggles take effect and return the session; an empty request is
// refused, and model-default needs a profile.
func TestAPIv1Actions(t *testing.T) {
	up := diagUpstream(t, false)
	route := diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"})
	route.ActiveProfileName = ""
	paths, _ := routedSessionPaths(t, route)
	status, got := apiCall(t, paths.SocketPath, "POST", "/api/v1/capture", `{"bodies":true}`)
	if status != 200 || got["capture_bodies"] != true {
		t.Fatalf("capture on: %d %v", status, got)
	}
	if status, out := apiCall(t, paths.SocketPath, "POST", "/api/v1/capture", `{}`); status != 400 || errCode(out) != "usage" {
		t.Fatalf("empty capture: %d %v", status, out)
	}
	if status, out := apiCall(t, paths.SocketPath, "POST", "/api/v1/model-default", `{"target":"gpt-6"}`); status != 409 || errCode(out) != "no_profile" {
		t.Fatalf("model-default without profile: %d %v", status, out)
	}
	if status, out := apiCall(t, paths.SocketPath, "POST", "/api/v1/profile", `not json`); status != 400 || errCode(out) != "usage" {
		t.Fatalf("bad profile body: %d %v", status, out)
	}
}

// The event stream opens with hello naming the session, then carries the
// session's events under their v1 names.
func TestAPIv1Events(t *testing.T) {
	up := diagUpstream(t, false)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/api/v1/events", nil)
	resp, err := (&http.Client{Transport: control.UnixTransport(paths.SocketPath)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan [2]string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var typ string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{typ, strings.TrimPrefix(line, "data: ")}
			}
		}
		close(events)
	}()
	next := func() [2]string {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed")
			}
			return ev
		case <-ctx.Done():
			t.Fatal("timed out waiting for an event")
		}
		return [2]string{}
	}
	if ev := next(); ev[0] != "hello" || !strings.Contains(ev[1], sess.ID) {
		t.Fatalf("first event = %v", ev)
	}
	messagesClient(t, paths.CABundlePath, sess)("claude-opus-5-5")
	for {
		ev := next()
		if ev[0] == "request.added" {
			if !strings.Contains(ev[1], `"class":"forwarded-api"`) {
				t.Fatalf("request.added payload = %s", ev[1])
			}
			break
		}
	}
}

// After a request a session.updated follows with the new counts and state.
// When the session closes and the supervisor stops right after, as the
// launcher does, the stream still carries session.ended, then ends.
func TestAPIv1EventsFollowRequestsAndEnd(t *testing.T) {
	up := diagUpstream(t, false)
	paths := testutil.ShortAppPaths(t, "e.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = srv.Run(runCtx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "diag"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/api/v1/events", nil)
	resp, err := (&http.Client{Transport: control.UnixTransport(paths.SocketPath)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var typ string
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event: ") {
				typ = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				events <- [2]string{typ, strings.TrimPrefix(line, "data: ")}
			}
		}
		close(events)
	}()
	waitFor := func(typ, contains string) {
		t.Helper()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("stream closed before %s", typ)
				}
				if ev[0] == typ && strings.Contains(ev[1], contains) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s with %s", typ, contains)
			}
		}
	}
	waitFor("hello", sess.ID)
	messagesClient(t, paths.CABundlePath, sess)("claude-opus-5-5")
	waitFor("session.updated", `"recent_request_count":1`)
	if err := client.CloseSession(ctx, sess.ID, model.SessionCloseRequest{Reason: "claude process exited"}); err != nil {
		t.Fatal(err)
	}
	_ = srv.Shutdown(ctx)
	waitFor("session.ended", `"state":"ended"`)
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("the stream goes on after session.ended")
		}
	case <-ctx.Done():
		t.Fatal("the stream did not end")
	}
}

// On a session with a profile, the API's model-default and profile switch
// apply live, without a token.
func TestAPIv1ProfileActions(t *testing.T) {
	_, state, _ := sessionOnGatewayProfile(t)
	socket := state.proxy.supervisor.paths.SocketPath
	if status, out := apiCall(t, socket, "POST", "/api/v1/model-default", `{"target":"gpt-6"}`); status != 200 || out["result"] != string(SwitchResultSwitched) {
		t.Fatalf("model-default: %d %v", status, out)
	}
	if state.snapshot().ModelAliasForward["*"] != "gpt-6" {
		t.Fatalf("forward = %v", state.snapshot().ModelAliasForward)
	}
	if status, out := apiCall(t, socket, "POST", "/api/v1/profile", `{"name":"gw"}`); status != 200 || out["result"] != string(SwitchResultSwitched) {
		t.Fatalf("switch: %d %v", status, out)
	}
	if status, out := apiCall(t, socket, "POST", "/api/v1/profile", `{"name":"nope"}`); status != 200 || out["result"] == string(SwitchResultSwitched) {
		t.Fatalf("switch to unknown profile: %d %v", status, out)
	}
}

// The v1 session words its ribbon, configuration and posture for clients:
// display.cells, display.config, display.posture (+ posture_note).
func TestAPIv1SessionDisplayForClients(t *testing.T) {
	up := diagUpstream(t, false)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"}))
	status, got := apiCall(t, paths.SocketPath, "GET", "/api/v1/session", "")
	disp, _ := got["display"].(map[string]any)
	if status != 200 || disp == nil {
		t.Fatalf("session: %d %v", status, got)
	}
	cells, _ := disp["cells"].([]any)
	var keys []string
	for _, c := range cells {
		keys = append(keys, c.(map[string]any)["key"].(string))
	}
	if strings.Join(keys, ",") != "route,auth,models,traffic,profile,bodies,egress,native_tls" {
		t.Fatalf("cells = %v", keys)
	}
	models := cells[2].(map[string]any)
	if models["label"] != "Models" || models["value"] != "1 alias" || models["state"] != "aliases-active" {
		t.Fatalf("models cell = %v", models)
	}
	if profile := cells[4].(map[string]any); profile["value"] != "gw" || profile["state"] != "active" {
		t.Fatalf("profile cell = %v", profile)
	}
	config, _ := disp["config"].([]any)
	if len(config) == 0 || config[0].(map[string]any)["label"] != "Session ID" || config[0].(map[string]any)["value"] != sess.ID {
		t.Fatalf("config = %v", config)
	}
	if !strings.HasPrefix(disp["posture"].(string), "Routing Claude Code through ") || disp["posture_note"] != nil {
		t.Fatalf("posture = %v / %v", disp["posture"], disp["posture_note"])
	}
}

// egress-test probes where the session's traffic exits now.
func TestAPIv1EgressTest(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ip":"203.0.113.9","country":"JP"}`)
	}))
	defer target.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", target.URL)
	paths, _ := routedSessionPaths(t, diagRoute("http://127.0.0.1:9", nil))
	status, got := apiCall(t, paths.SocketPath, "POST", "/api/v1/egress-test", "")
	if status != 200 || got["status"] != "OK" || got["public_ip"] != "203.0.113.9" {
		t.Fatalf("egress-test: %d %v", status, got)
	}
	if status, out := apiCall(t, paths.SocketPath, "GET", "/api/v1/egress-test", ""); status != http.StatusMethodNotAllowed || errCode(out) != "method_not_allowed" {
		t.Fatalf("GET egress-test: %d %v", status, out)
	}
}
