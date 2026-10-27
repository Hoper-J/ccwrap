package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
)

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
	if status != 200 || got["id"] != sess.ID || disp == nil || disp["route_value"] == "" || disp["models_value"] != "1 alias" {
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
	if _, err := io.ReadAll(io.LimitReader(resp.Body, 0)); err != nil {
		t.Fatal(err)
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
