package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
)

// A header only upstream was injected, one only from Claude Code stripped,
// one with other values rewritten; the rest went as sent and are not listed.
func TestUpstreamHeaderChanges(t *testing.T) {
	in := http.Header{
		"Authorization":     {"Bearer placeholder"},
		"Anthropic-Version": {"2023-06-01"},
		"Anthropic-Beta":    {"a,b"},
		"Accept-Encoding":   {"gzip, br"},
	}
	out := http.Header{
		"X-Api-Key":         {"real-key"},
		"Anthropic-Version": {"2023-06-01"},
		"Anthropic-Beta":    {"a,b,oauth"},
		"Accept-Encoding":   {"identity"},
		"X-Team":            {"team"},
	}
	got := upstreamHeaderChanges(in, out).Changes
	want := []model.HeaderChange{
		{Name: "Accept-Encoding", Action: model.HeaderRewritten},
		{Name: "Anthropic-Beta", Action: model.HeaderRewritten},
		{Name: "Authorization", Action: model.HeaderStripped},
		{Name: "X-Api-Key", Action: model.HeaderInjected},
		{Name: "X-Team", Action: model.HeaderInjected},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v\nwant      %+v", got, want)
	}
	if d := upstreamHeaderChanges(in, in.Clone()); d.Changes == nil || len(d.Changes) != 0 {
		t.Fatalf("unchanged headers = %+v, want an empty list", d.Changes)
	}
}

// A forwarded request records what ccwrap did to its headers: the
// placeholder credential stripped, the gateway key and configured upstream
// header injected. The record never carries either value.
func TestForwardedRequestRecordsHeaderChanges(t *testing.T) {
	client, sess, hc, upstream := headerInspectorSession(t, "hd.sock")
	defer upstream.Close()
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        upstream.URL,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParse(t, upstream.URL).Hostname(),
		ExactUpstreamBase: upstream.URL,
		FailPolicy:        model.FailClosed,
		OverrideAuth: &model.AuthOverride{
			Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey,
			HeaderName: "X-API-Key", HeaderValue: "gateway-KEY-do-not-show",
		},
		UpstreamHeaders: map[string]string{"X-Team": "team-SECRET-do-not-show"},
	}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer placeholder")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	isMessages := func(rec model.RequestRecord) bool {
		return rec.Method == http.MethodPost && strings.Contains(rec.Path, "/v1/messages")
	}
	var rec model.RequestRecord
	for _, r := range waitForRequestRecord(t, client, sess.ID, "forwarded /v1/messages request", isMessages) {
		if isMessages(r) {
			rec = r
		}
	}
	if rec.UpstreamHeaders == nil {
		t.Fatal("no header changes recorded")
	}
	actions := map[string]model.HeaderAction{}
	for _, c := range rec.UpstreamHeaders.Changes {
		actions[c.Name] = c.Action
	}
	if actions["Authorization"] != model.HeaderStripped || actions["X-Api-Key"] != model.HeaderInjected || actions["X-Team"] != model.HeaderInjected || actions["Content-Type"] != "" {
		t.Fatalf("changes = %+v", rec.UpstreamHeaders.Changes)
	}
	act := apiGetRaw(t, client.SocketPath(), "/api/v1/activity")
	defer act.Body.Close()
	body, _ := io.ReadAll(act.Body)
	for _, secret := range []string{"gateway-KEY-do-not-show", "team-SECRET-do-not-show"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("the activity carries %q:\n%s", secret, body)
		}
	}
	var wire struct {
		Requests []struct {
			Path            string `json:"path"`
			UpstreamHeaders *struct {
				Changes []model.HeaderChange `json:"changes"`
			} `json:"upstream_headers"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range wire.Requests {
		found = found || strings.Contains(r.Path, "/v1/messages") && r.UpstreamHeaders != nil && len(r.UpstreamHeaders.Changes) == len(rec.UpstreamHeaders.Changes)
	}
	if !found {
		t.Fatalf("the activity carries no header changes:\n%s", body)
	}
}

// apiGetRaw sends GET path to the session's v1 API over its control socket.
func apiGetRaw(t *testing.T, socket, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: control.UnixTransport(socket), Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
