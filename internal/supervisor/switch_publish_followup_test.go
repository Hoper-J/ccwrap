package supervisor

import (
	"sync"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// firstPartySeedReq is a first-party passthrough route used to seed a session
// before a switch. Mirrors the seed shape used elsewhere in switch_test.go.
func firstPartySeedReq(nativeTLS bool) model.SessionRouteRequest {
	return model.SessionRouteRequest{
		APIBaseURL:        "https://api.anthropic.com",
		RouteClass:        model.RouteClassFirstParty,
		RouteSource:       model.RouteSourceFallback,
		RouteConfigSource: "fallback_default",
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		AuthPolicy:        model.AuthPolicyFirstPartyPassthrough,
		AuthBootstrap:     model.AuthBootstrapNotNeeded,
		AuthBootstrapKind: model.AuthBootstrapKindNone,
		ExactUpstreamHost: "api.anthropic.com",
		ExactUpstreamBase: "https://api.anthropic.com",
		FailPolicy:        model.FailClosed,
		Egress:            model.EgressConfig{Mode: "direct", Source: "fallback"},
		NativeTLS:         nativeTLS,
	}
}

// TestSwitchProfile_ReDerivesUpstreamHostOnDifferentHostSwitch closes the
// fast-follow gap surfaced by the expert review: the production SwitchProfile
// publish goes through newResolved, which derives ExactUpstreamHost /
// ExactUpstreamBase / APIBaseURL FROM the candidate profile's base_url (via
// url.Parse + .Hostname()). Every existing successful-switch test reuses the
// SAME host (api.anthropic.com) for seed and target, so none proves the host
// is RE-DERIVED from a new profile rather than carried from the seed. This
// switches to a different-host gateway and asserts the projection moved.
func TestSwitchProfile_ReDerivesUpstreamHostOnDifferentHostSwitch(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	// Supply the gateway's key so the switch resolves to a clean injected-auth
	// route (not auth-missing); ParentEnv is the env the switch resolver reads.
	sv.launchCtx.Options.ParentEnv = append(sv.launchCtx.Options.ParentEnv, "SWITCH_HOST_TEST_KEY=sk-ant-test-not-a-real-key")

	// Seed: first-party passthrough at api.anthropic.com.
	if err := sv.setRoute(sess.public.ID, firstPartySeedReq(false)); err != nil {
		t.Fatalf("seed setRoute: %v", err)
	}
	if got := sess.snapshot().ExactUpstreamHost; got != "api.anthropic.com" {
		t.Fatalf("seed ExactUpstreamHost=%q, want api.anthropic.com", got)
	}

	// Switch to a DIFFERENT-host gateway profile.
	writeProfilesJSON(t, sv, `{"profiles":{"gw2":{"provider":"AcmeGW","base_url":"https://gw2.example.com/v2","auth":{"mode":"ccwrap_bearer","key_env":"SWITCH_HOST_TEST_KEY"},"egress":{"mode":"inherit"}}}}`)
	out := sv.SwitchProfile(sess.public.ID, "gw2")
	if out.Result != SwitchResultSwitched {
		t.Fatalf("Result=%q, want switched (ReasonCode=%q Message=%q)", out.Result, out.ReasonCode, out.Message)
	}

	// The published projection MUST be re-derived from the new base_url. A
	// switch path that carried the seed's host (or failed to re-run the
	// Hostname() derivation) would leave these at api.anthropic.com.
	snap := sess.snapshot()
	if snap.ExactUpstreamHost != "gw2.example.com" {
		t.Errorf("ExactUpstreamHost=%q after switch, want gw2.example.com (host not re-derived from new profile base_url)", snap.ExactUpstreamHost)
	}
	if snap.ExactUpstreamBase != "https://gw2.example.com/v2" {
		t.Errorf("ExactUpstreamBase=%q after switch, want https://gw2.example.com/v2", snap.ExactUpstreamBase)
	}
	if snap.APIBaseURL != "https://gw2.example.com/v2" {
		t.Errorf("APIBaseURL=%q after switch, want https://gw2.example.com/v2", snap.APIBaseURL)
	}
}

// TestSwitchProfile_PreservesNativeTLSBlockedAcrossSwitch closes the
// concurrency fast-follow (reviewer F1): no test drove recordNativeTLS
// concurrently with a SwitchProfile publish. deriveInto preserves a
// dial-written NativeTLS status via the read-write-back in
// currentDialStateLocked (dial-wins-else-seed). This interleaves dial blocks
// with a switch publish and asserts the block survives — under -race it also
// fences the concurrent sess.public.NativeTLS writers. A future deriveInto that
// unconditionally seeds (dropping the dial-wins rule) would flip the result to
// the toggle seed and fail here.
func TestSwitchProfile_PreservesNativeTLSBlockedAcrossSwitch(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	sp := sess.proxy // the real per-session proxy created by createSession
	if sp == nil {
		t.Fatal("setup: sess.proxy is nil")
	}

	// Seed a native-TLS route, then record a dial BLOCK.
	if err := sv.setRoute(sess.public.ID, firstPartySeedReq(true)); err != nil {
		t.Fatalf("seed setRoute: %v", err)
	}
	sp.recordNativeTLS(false, "handshake failed")
	if got := sess.snapshot().NativeTLS; got != "blocked: handshake failed" {
		t.Fatalf("setup: expected blocked status, got %q", got)
	}

	writeProfilesJSON(t, sv, switchTestProfilesJSON)

	// Concurrently hammer dial blocks while the switch publishes (deriveInto).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			sp.recordNativeTLS(false, "handshake failed")
		}
	}()
	out := sv.SwitchProfile(sess.public.ID, "alpha")
	wg.Wait()

	if out.Result != SwitchResultSwitched {
		t.Fatalf("Result=%q, want switched (ReasonCode=%q Message=%q)", out.Result, out.ReasonCode, out.Message)
	}
	if got := sess.snapshot().NativeTLS; got != "blocked: handshake failed" {
		t.Fatalf("switch publish must PRESERVE the dial-written block (dial-wins-else-seed); got %q (an unconditional-seed deriveInto would show %q)", got, initialNativeTLSPosture(true))
	}
}
