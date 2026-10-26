package supervisor

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/unmapped"
)

// A Claude model sent with no alias shows on the session once and is
// remembered for the profile, so the next launch can warn; aliased models
// are neither.
func TestSessionRemembersUnmappedModels(t *testing.T) {
	up := diagUpstream(t, false)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"}))
	client := control.NewClient(paths.SocketPath)
	send := messagesClient(t, paths.CABundlePath, sess)
	send("claude-opus-5-5")
	send("claude-fable-5-1")
	send("claude-fable-5-1")

	want := []string{"claude-fable-5-1"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := client.GetSession(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(got.UnmappedModels, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UnmappedModels = %v, want %v", got.UnmappedModels, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := unmapped.Models(paths.StateDir, "gw"); !reflect.DeepEqual(got, want) {
		t.Fatalf("remembered for gw: %v", got)
	}
}

// Models unmapped on one route say nothing about the next: a profile switch
// clears the session's list.
func TestSwitchClearsUnmappedModels(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	if err := sv.setRoute(sess.public.ID, model.SessionRouteRequest{
		APIBaseURL: "https://api.anthropic.com", RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceFallback,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone, AuthPolicy: model.AuthPolicyFirstPartyPassthrough,
		AuthBootstrap: model.AuthBootstrapNotNeeded, AuthBootstrapKind: model.AuthBootstrapKindNone,
		ExactUpstreamHost: "api.anthropic.com", ExactUpstreamBase: "https://api.anthropic.com",
		FailPolicy: model.FailClosed, Egress: model.EgressConfig{Mode: "direct", Source: "fallback"},
	}); err != nil {
		t.Fatal(err)
	}
	sv.recordRequest(sess.public.ID, model.RequestRecord{Method: "POST", Path: "/v1/messages", StatusCode: 200, Model: "claude-fable-5-1", ModelUnmapped: true, ActiveProfileName: "gw"})
	if got := sess.snapshot().UnmappedModels; !reflect.DeepEqual(got, []string{"claude-fable-5-1"}) {
		t.Fatalf("before switch: %v", got)
	}
	writeProfilesJSON(t, sv, `{"profiles":{"alpha":{"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}}}}`)
	if out := sv.SwitchProfile(sess.public.ID, "alpha"); out.Result != SwitchResultSwitched {
		t.Fatalf("switch: %+v", out)
	}
	if got := sess.snapshot().UnmappedModels; len(got) != 0 {
		t.Fatalf("after switch: %v", got)
	}
}
