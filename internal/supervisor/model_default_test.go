package supervisor

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// newSupervisedSession runs a supervisor with one session and returns the
// session, its state and the supervisor's StateDir (where profiles.json
// lives).
func newSupervisedSession(t *testing.T) (*model.Session, *sessionState, string) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, "pmut.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "pmut"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatal("getSession returned nil after CreateSession")
	}
	return sess, state, paths.StateDir
}

// sessionOnGatewayProfile is a live session switched onto profile "gw",
// whose only alias is claude-opus-4-8 → gpt-6.
func sessionOnGatewayProfile(t *testing.T) (*model.Session, *sessionState, string) {
	t.Helper()
	sess, state, dir := newSupervisedSession(t)
	sv := state.proxy.supervisor
	installLaunchCtxForSwitch(t, sv)
	if err := sv.setRoute(sess.ID, model.SessionRouteRequest{
		APIBaseURL: "https://api.anthropic.com", RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceFallback,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone, AuthPolicy: model.AuthPolicyFirstPartyPassthrough,
		AuthBootstrap: model.AuthBootstrapNotNeeded, AuthBootstrapKind: model.AuthBootstrapKindNone,
		ExactUpstreamHost: "api.anthropic.com", ExactUpstreamBase: "https://api.anthropic.com",
		FailPolicy: model.FailClosed, Egress: model.EgressConfig{Mode: "direct", Source: "fallback"},
	}); err != nil {
		t.Fatal(err)
	}
	writeProfilesJSON(t, sv, `{"profiles":{"gw":{"provider":"Gateway","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"tok"},"egress":{"mode":"inherit"},"model_aliases":{"claude-opus-4-8":"gpt-6"}}}}`)
	if out := sv.SwitchProfile(sess.ID, "gw"); out.Result != SwitchResultSwitched {
		t.Fatalf("switch to gw: %+v", out)
	}
	return sess, state, dir
}

// The one-click fix adds only "*" to the active profile, keeps its other
// aliases, and applies it to the live session, which clears the warning.
func TestModelDefaultSavesAndApplies(t *testing.T) {
	sess, state, dir := sessionOnGatewayProfile(t)
	socket := state.proxy.supervisor.paths.SocketPath
	state.proxy.supervisor.recordRequest(sess.ID, model.RequestRecord{Method: "POST", Path: "/v1/messages", StatusCode: 200, Model: "claude-opus-5-5", ModelUnmapped: true, ActiveProfileName: "gw"})
	if len(state.snapshot().UnmappedModels) != 1 {
		t.Fatal("setup: no unmapped model")
	}

	for _, target := range []string{"gpt-6", "*"} {
		status, out := apiCall(t, socket, "POST", "/api/v1/model-default", `{"target":"`+target+`"}`)
		if status != 200 || out["result"] != string(SwitchResultSwitched) {
			t.Fatalf("target %s: %d %v", target, status, out)
		}
		f, err := profiles.Load(profiles.DefaultPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Profiles["gw"].ModelAliases; !reflect.DeepEqual(got, map[string]string{"claude-opus-4-8": "gpt-6", "*": target}) {
			t.Fatalf("target %s: saved aliases %v", target, got)
		}
		snap := state.snapshot()
		if snap.ModelAliasForward["*"] != target || len(snap.UnmappedModels) != 0 {
			t.Fatalf("target %s: live forward %v, unmapped %v", target, snap.ModelAliasForward, snap.UnmappedModels)
		}
	}
}

func TestModelDefaultRefusals(t *testing.T) {
	_, state, _ := sessionOnGatewayProfile(t)
	if status, out := apiCall(t, state.proxy.supervisor.paths.SocketPath, "POST", "/api/v1/model-default", `{"target":"  "}`); status != 400 || errCode(out) != "usage" {
		t.Fatalf("empty target: %d %v", status, out)
	}
	// A session without a profile has nowhere to save the choice.
	_, bare, _ := newSupervisedSession(t)
	status, out := apiCall(t, bare.proxy.supervisor.paths.SocketPath, "POST", "/api/v1/model-default", `{"target":"gpt-6"}`)
	msg, _ := out["error"].(map[string]any)["message"].(string)
	if status != 409 || errCode(out) != "no_profile" || !strings.Contains(msg, "--model-alias '*=gpt-6'") {
		t.Fatalf("no profile: %d %v", status, out)
	}
}
