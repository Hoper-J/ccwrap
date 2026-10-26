package supervisor

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// sessionOnGatewayProfile is a live session switched onto profile "gw",
// whose only alias is claude-opus-4-8 → gpt-6.
func sessionOnGatewayProfile(t *testing.T) (*model.Session, *sessionState, string) {
	t.Helper()
	sess, state, dir := newSessionForProfileMutation(t)
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

func postModelDefault(t *testing.T, sess *model.Session, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+sess.ProxyListenAddr+"/profile/model-default", strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-CCWRAP-Profile-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The one-click fix adds only "*" to the active profile, keeps its other
// aliases, and applies it to the live session, which clears the warning.
func TestProfileModelDefaultSavesAndApplies(t *testing.T) {
	sess, state, dir := sessionOnGatewayProfile(t)
	state.proxy.supervisor.recordRequest(sess.ID, model.RequestRecord{Method: "POST", Path: "/v1/messages", StatusCode: 200, Model: "claude-opus-5-5", ModelUnmapped: true, ActiveProfileName: "gw"})
	if len(state.snapshot().UnmappedModels) != 1 {
		t.Fatal("setup: no unmapped model")
	}

	for _, target := range []string{"gpt-6", "*"} {
		status, out := postModelDefault(t, sess, state.profileToken, `{"target":"`+target+`"}`)
		if status != http.StatusOK || out["result"] != string(SwitchResultSwitched) {
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

func TestProfileModelDefaultRefusals(t *testing.T) {
	sess, state, _ := sessionOnGatewayProfile(t)
	if status, _ := postModelDefault(t, sess, "", `{"target":"gpt-6"}`); status != http.StatusForbidden {
		t.Fatalf("no token: %d", status)
	}
	if status, _ := postModelDefault(t, sess, state.profileToken, `{"target":"  "}`); status != http.StatusBadRequest {
		t.Fatalf("empty target: %d", status)
	}
	// A session without a profile has nowhere to save the choice.
	bare, bareState, _ := newSessionForProfileMutation(t)
	status, out := postModelDefault(t, bare, bareState.profileToken, `{"target":"gpt-6"}`)
	if status != http.StatusConflict || !strings.Contains(out["message"].(string), "--model-alias '*=gpt-6'") {
		t.Fatalf("no profile: %d %v", status, out)
	}
}
