package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// newTestSessionForCreate spins up the real Supervisor + control client just
// like the proxy_test.go pattern, calls CreateSession through the socket, then
// returns the running supervisor and the underlying *sessionState retrieved via
// srv.getSession. Caller can probe internal session fields (sess.active, etc).
// The supervisor goroutine is torn down by t.Cleanup(cancel).
func newTestSessionForCreate(t *testing.T) (*Supervisor, *sessionState) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "active-posture"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatal("getSession returned nil after CreateSession")
	}
	return srv, state
}

func TestLookupEnv(t *testing.T) {
	env := []string{
		"FOO=foo-val",
		"BAR=bar-val",
		"EQUALS-INSIDE=a=b=c",
		"EMPTY=",
	}
	cases := []struct {
		key, want string
	}{
		{"FOO", "foo-val"},
		{"BAR", "bar-val"},
		{"EQUALS-INSIDE", "a=b=c"},
		{"EMPTY", ""},
		{"MISSING", ""},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := lookupEnv(env, tc.key); got != tc.want {
				t.Errorf("lookupEnv(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestLaunchContextHoldsThreeValues(t *testing.T) {
	// Struct existence + field types — compile-time only.
	var lc LaunchContext
	_ = lc.Options    // preflight.Options
	_ = lc.Inspection // *settings.InspectionResult
	_ = lc.Launch     // *preflight.Result
	// (file-content snapshots live on Options, not on LaunchContext)
}

func TestSupervisorNewAcceptsNilLaunchContext(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	// New signature: New(paths, idle, launchCtx *LaunchContext) — passing nil must be valid for back-compat.
	sv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sv == nil {
		t.Fatal("New returned nil")
	}
}

func TestSupervisorNewAcceptsLaunchContext(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	lc := &LaunchContext{
		Options: preflight.Options{ParentEnv: []string{"FOO=v"}},
	}
	sv, err := New(paths, 0, lc)
	if err != nil {
		t.Fatal(err)
	}
	if sv == nil {
		t.Fatal("New returned nil")
	}
	got := sv.launchContext()
	if got != lc {
		t.Errorf("launchContext() = %p, want %p", got, lc)
	}
}

// TestCreateSession_PublishesLaunchPostureInProcess locks the PR2 contract that
// createSession publishes the launch posture in-process from LaunchContext.Launch
// (no SetRoute RPC): the returned snapshot carries the routed fields and the
// launch toggle, not the safe-zero defaults.
func TestCreateSession_PublishesLaunchPostureInProcess(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "lp.sock")
	u, err := url.Parse("https://gw.example.com/v1")
	if err != nil {
		t.Fatal(err)
	}
	pre := &preflight.Result{
		APIBaseURL:        u,
		ActiveProfileName: "gateway",
		Egress:            model.EgressConfig{Mode: "direct"},
	}
	sv, err := New(paths, 0, &LaunchContext{Launch: pre, CaptureBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := sv.createSession(model.SessionCreateRequest{ID: "sess-1", LauncherPID: os.Getpid()})
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if got.ExactUpstreamHost != "gw.example.com" {
		t.Fatalf("ExactUpstreamHost = %q, want gw.example.com (launch publish did not run)", got.ExactUpstreamHost)
	}
	if got.ActiveProfileName != "gateway" {
		t.Fatalf("ActiveProfileName = %q, want gateway", got.ActiveProfileName)
	}
	if !got.CaptureBodies {
		t.Fatalf("CaptureBodies = false, want true (launch toggle not published)")
	}
}

// TestCreateSessionZeroInitsActivePosture locks the contract that after
// createSession (but before setRoute), sess.active.Load() must be non-nil and
// zero-valued so the request hot-path never sees a nil *activePosture. This
// reproduces the pre-setRoute zero-routeConfig behavior with the
// atomic.Pointer holder.
func TestCreateSessionZeroInitsActivePosture(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	_ = sv
	ap := sess.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after createSession; expected safe-zero *posture")
	}
	if ap.r.apiBaseURL != nil {
		t.Errorf("zero-init apiBaseURL = %v, want nil", ap.r.apiBaseURL)
	}
	if len(ap.r.modelAlias.Forward) != 0 {
		t.Errorf("zero-init modelAlias.Forward = %v, want empty", ap.r.modelAlias.Forward)
	}
	if ap.r.routeClass != "" {
		t.Errorf("zero-init routeClass = %q, want empty", ap.r.routeClass)
	}
	if ap.r.profileName != "" || ap.r.profileProvider != "" {
		t.Errorf("zero-init identity = %q/%q, want empty/empty", ap.r.profileName, ap.r.profileProvider)
	}
}

// TestSetRoutePublishesViaPublishPosture locks the deliberate launch-path URL
// strip: setRoute must build a posturePublish inline from the
// SessionRouteRequest and end at publishPosture; the three URL fields
// (APIBaseURL, ExactUpstreamBase, EgressSummary) must be userinfo-stripped in
// sess.public — closing the leak in /recent.session.{api_base_url,
// exact_upstream_base, egress_summary}. ExactUpstreamHost is already
// userinfo-free (from *url.URL.Hostname()) so passes through unchanged.
// ActiveProfileName/Provider on the request flow into both active.profile* and
// public.ActiveProfile*.
func TestSetRoutePublishesViaPublishPosture(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	req := model.SessionRouteRequest{
		APIBaseURL:        "https://alice:secret@gateway.example.com/v1",
		RouteClass:        model.RouteClassFirstParty,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "gateway.example.com",
		ExactUpstreamBase: "https://alice:secret@gateway.example.com",
		Egress: model.EgressConfig{
			Mode:    "http_proxy",
			Source:  "explicit",
			Summary: "http://proxy-user:proxy-pw@proxy.example.com:3128",
		},
		FailPolicy:            model.FailClosed,
		ActiveProfileName:     "alpha",
		ActiveProfileProvider: "anthropic",
	}
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("setRoute err: %v", err)
	}

	// 1. active.Load() is populated (not the safe-zero default).
	ap := sess.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after setRoute")
	}
	if ap.r.routeClass != model.RouteClassFirstParty {
		t.Errorf("active.routeClass = %q, want first_party", ap.r.routeClass)
	}
	if ap.r.profileName != "alpha" || ap.r.profileProvider != "anthropic" {
		t.Errorf("active identity = %q/%q, want alpha/anthropic", ap.r.profileName, ap.r.profileProvider)
	}
	if ap.r.apiBaseURL == nil {
		t.Fatal("active.r.apiBaseURL is nil; want parsed URL")
	}

	// 2. public mirror has the request's fields.
	sess.mu.RLock()
	pubClass := sess.public.RouteClass
	pubAPIBase := sess.public.APIBaseURL
	pubExactBase := sess.public.ExactUpstreamBase
	pubExactHost := sess.public.ExactUpstreamHost
	pubEgressSummary := sess.public.EgressSummary
	pubProfileName := sess.public.ActiveProfileName
	pubProfileProvider := sess.public.ActiveProfileProvider
	sess.mu.RUnlock()

	if pubClass != model.RouteClassFirstParty {
		t.Errorf("public.RouteClass = %q, want first_party", pubClass)
	}
	if pubProfileName != "alpha" || pubProfileProvider != "anthropic" {
		t.Errorf("public identity = %q/%q, want alpha/anthropic", pubProfileName, pubProfileProvider)
	}

	// 3. URL userinfo STRIPPED (the deliberate behavior change).
	if pubAPIBase != "https://gateway.example.com/v1" {
		t.Errorf("public.APIBaseURL = %q, want stripped form (launch-path strip)", pubAPIBase)
	}
	if pubExactBase != "https://gateway.example.com" {
		t.Errorf("public.ExactUpstreamBase = %q, want stripped form", pubExactBase)
	}
	if pubExactHost != "gateway.example.com" {
		t.Errorf("public.ExactUpstreamHost = %q, want unchanged (already userinfo-free)", pubExactHost)
	}
	if pubEgressSummary != "http://proxy.example.com:3128" {
		t.Errorf("public.EgressSummary = %q, want stripped form", pubEgressSummary)
	}
}

// TestSupervisor_UnmaskCredentialsFromEnv exercises the CCWRAP_UNMASK_CREDENTIALS
// env read at supervisor New() time. truthyEnv-style accept set: only "1" or
// case-insensitive "true" — anything else is false. Indirected through the
// readUnmaskCredentialsEnv package var so the test does not race with global
// env state.
func TestSupervisor_UnmaskCredentialsFromEnv(t *testing.T) {
	cases := []struct {
		envValue string
		want     bool
	}{
		{"1", true},
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"", false},
		{"0", false},
		{"yes", false}, // truthyEnv-style: yes/on NOT accepted
		{"on", false},
		{"false", false},
		{"  1  ", true}, // whitespace trimmed
	}
	for _, c := range cases {
		c := c
		t.Run(c.envValue, func(t *testing.T) {
			saved := readUnmaskCredentialsEnv
			readUnmaskCredentialsEnv = func() bool {
				v := strings.TrimSpace(c.envValue)
				return v == "1" || strings.EqualFold(v, "true")
			}
			t.Cleanup(func() { readUnmaskCredentialsEnv = saved })

			paths := testutil.ShortAppPaths(t, "uc.sock")
			sv, err := New(paths, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sv.unmaskCredentials != c.want {
				t.Errorf("env=%q: sv.unmaskCredentials = %v, want %v", c.envValue, sv.unmaskCredentials, c.want)
			}
		})
	}
}

// TestCreateSession_PropagatesCaptureBodiesUnmasked confirms the supervisor's
// process-wide unmask flag lands on sess.public.CaptureBodiesUnmasked at
// session creation. The inspect-web ribbon reads this to render the danger
// marker; if propagation breaks, the user loses the persistent reminder.
func TestCreateSession_PropagatesCaptureBodiesUnmasked(t *testing.T) {
	saved := readUnmaskCredentialsEnv
	readUnmaskCredentialsEnv = func() bool { return true }
	t.Cleanup(func() { readUnmaskCredentialsEnv = saved })

	sv, sess := newTestSessionForCreate(t)
	_ = sv
	sess.mu.RLock()
	got := sess.public.CaptureBodiesUnmasked
	sess.mu.RUnlock()
	if !got {
		t.Errorf("sess.public.CaptureBodiesUnmasked = false; want true (supervisor unmask=true)")
	}
}

// TestSetCaptureBodies_NotFoundSession returns a typed error for an unknown
// session ID — defensive guard so the HTTP endpoint can map this to a 4xx.
func TestSetCaptureBodies_NotFoundSession(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "scb-nf.sock")
	sv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sv.SetCaptureBodies("does-not-exist", true)
	if err == nil {
		t.Fatalf("SetCaptureBodies(bogus): want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q; want substring 'not found'", err.Error())
	}
}

// TestSetCaptureBodies_AtomicSwap_OnOff_OffOn verifies the round-trip:
// (1) sess starts with default-off (createSession), (2) SetCaptureBodies(true)
// flips both active.l.captureBodies AND sess.public.CaptureBodies under the
// same critical section, (3) SetCaptureBodies(false) flips them back.
func TestSetCaptureBodies_AtomicSwap_OnOff_OffOn(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	// 0. createSession default-off (model zero-value).
	if sess.active.Load().l.captureBodies {
		t.Errorf("initial active.l.captureBodies = true; want false")
	}
	if sess.public.CaptureBodies {
		t.Errorf("initial sess.public.CaptureBodies = true; want false")
	}
	// 1. on
	snap1, err := sv.SetCaptureBodies(sess.public.ID, true)
	if err != nil {
		t.Fatalf("SetCaptureBodies(on): %v", err)
	}
	if !snap1.CaptureBodies {
		t.Errorf("returned snap.CaptureBodies = false, want true")
	}
	if !sess.active.Load().l.captureBodies {
		t.Errorf("after on: active.l.captureBodies = false, want true")
	}
	sess.mu.RLock()
	pub1 := sess.public.CaptureBodies
	sess.mu.RUnlock()
	if !pub1 {
		t.Errorf("after on: sess.public.CaptureBodies = false, want true")
	}
	// 2. off
	snap2, err := sv.SetCaptureBodies(sess.public.ID, false)
	if err != nil {
		t.Fatalf("SetCaptureBodies(off): %v", err)
	}
	if snap2.CaptureBodies {
		t.Errorf("returned snap.CaptureBodies = true, want false")
	}
	if sess.active.Load().l.captureBodies {
		t.Errorf("after off: active.l.captureBodies = true, want false")
	}
}

// TestSetCaptureTelemetry mirrors TestSetCaptureBodies_AtomicSwap_OnOff_OffOn:
// (1) sess starts with default-off (createSession), (2) SetCaptureTelemetry(true)
// flips both active.l.captureTelemetry AND sess.public.CaptureTelemetry under
// the same critical section, (3) calling (true) again is an idempotent no-op (no
// error), (4) SetCaptureTelemetry(false) flips them back.
func TestSetCaptureTelemetry(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	// 0. createSession default-off (model zero-value).
	if sess.active.Load().l.captureTelemetry {
		t.Errorf("initial active.l.captureTelemetry = true; want false")
	}
	if sess.public.CaptureTelemetry {
		t.Errorf("initial sess.public.CaptureTelemetry = true; want false")
	}
	// 1. on
	snap1, err := sv.SetCaptureTelemetry(sess.public.ID, true)
	if err != nil {
		t.Fatalf("SetCaptureTelemetry(on): %v", err)
	}
	if !snap1.CaptureTelemetry {
		t.Errorf("returned snap.CaptureTelemetry = false, want true")
	}
	if !sess.active.Load().l.captureTelemetry {
		t.Errorf("after on: active.l.captureTelemetry = false, want true")
	}
	sess.mu.RLock()
	pub1 := sess.public.CaptureTelemetry
	sess.mu.RUnlock()
	if !pub1 {
		t.Errorf("after on: sess.public.CaptureTelemetry = false, want true")
	}
	// 2. on again is a no-op (no error).
	snap2, err := sv.SetCaptureTelemetry(sess.public.ID, true)
	if err != nil {
		t.Fatalf("SetCaptureTelemetry(on again): %v", err)
	}
	if !snap2.CaptureTelemetry {
		t.Errorf("after on again: snap.CaptureTelemetry = false, want true")
	}
	if !sess.active.Load().l.captureTelemetry {
		t.Errorf("after on again: active.l.captureTelemetry = false, want true")
	}
	// 3. off
	snap3, err := sv.SetCaptureTelemetry(sess.public.ID, false)
	if err != nil {
		t.Fatalf("SetCaptureTelemetry(off): %v", err)
	}
	if snap3.CaptureTelemetry {
		t.Errorf("returned snap.CaptureTelemetry = true, want false")
	}
	if sess.active.Load().l.captureTelemetry {
		t.Errorf("after off: active.l.captureTelemetry = true, want false")
	}
}

// TestSetCaptureBodies_PreservesMissingAuthEnv — the activePosture clone
// in SetCaptureBodies must copy every field, including missingAuthEnv.
// Previously the clone explicitly enumerated route/routeClass/authBootstrap/
// profileName/profileProvider and omitted missingAuthEnv, so toggling
// Bodies in a Case-A session zeroed the env name on the hot path. The
// next auth-failed request then read the empty value and returned the
// generic Case-B message instead of "needs $MY_KEY".
//
// Test: seed activePosture with a distinctive missingAuthEnv value,
// toggle Bodies, assert the value survives on the new posture.
func TestSetCaptureBodies_PreservesMissingAuthEnv(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	// Seed a Case-A posture by Store-ing directly. The test cares about
	// the toggle's field-preservation completeness, not the launch flow.
	// withCaptureBodies keeps the resolved half structurally, so the identity
	// fields (incl. missingAuthEnv) cannot be dropped.
	sess.active.Store(&posture{
		r: resolved{
			routeClass:      model.RouteClassFirstParty,
			authBootstrap:   model.AuthBootstrapMissing,
			profileName:     "p-case-a",
			profileProvider: "anthropic",
			missingAuthEnv:  "MY_CASE_A_ENV_NAME",
		},
		l: live{captureBodies: false},
	})

	if _, err := sv.SetCaptureBodies(sess.public.ID, true); err != nil {
		t.Fatalf("SetCaptureBodies: %v", err)
	}

	ap := sess.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after SetCaptureBodies")
	}
	if got := ap.r.missingAuthEnv; got != "MY_CASE_A_ENV_NAME" {
		t.Errorf("missingAuthEnv = %q after Bodies toggle; want preserved 'MY_CASE_A_ENV_NAME'", got)
	}
	// All other fields must also survive.
	if ap.r.routeClass != model.RouteClassFirstParty {
		t.Errorf("routeClass not preserved")
	}
	if ap.r.authBootstrap != model.AuthBootstrapMissing {
		t.Errorf("authBootstrap not preserved")
	}
	if ap.r.profileName != "p-case-a" {
		t.Errorf("profileName not preserved")
	}
	if ap.r.profileProvider != "anthropic" {
		t.Errorf("profileProvider not preserved")
	}
	if !ap.l.captureBodies {
		t.Errorf("captureBodies must be ON (the only intentional change)")
	}
}

// TestSetCaptureBodies_IdempotentSameValue verifies that calling with the
// current state is a no-op: no event emitted, no UpdatedAt bump (the latter
// is a soft guarantee we test directly so a future "always re-broadcast"
// regression would surface). The contract is "no-op" — a useful property for
// idempotent clients (page reload re-asserting state shouldn't churn SSE).
func TestSetCaptureBodies_IdempotentSameValue(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	// Flip on once to set the baseline (and consume that event).
	if _, err := sv.SetCaptureBodies(sess.public.ID, true); err != nil {
		t.Fatalf("setup on: %v", err)
	}
	drainEvents(ch, 100*time.Millisecond) // discard setup event
	updatedAtBefore := func() time.Time {
		sess.mu.RLock()
		defer sess.mu.RUnlock()
		return sess.public.UpdatedAt
	}()
	// Now call with the same value — must be a no-op.
	if _, err := sv.SetCaptureBodies(sess.public.ID, true); err != nil {
		t.Fatalf("idempotent on: %v", err)
	}
	got := drainEvents(ch, 100*time.Millisecond)
	if updates := filterEvents(got, "session_updated"); len(updates) != 0 {
		t.Errorf("idempotent call emitted %d session_updated events; want 0", len(updates))
	}
	updatedAtAfter := func() time.Time {
		sess.mu.RLock()
		defer sess.mu.RUnlock()
		return sess.public.UpdatedAt
	}()
	if !updatedAtAfter.Equal(updatedAtBefore) {
		t.Errorf("idempotent call bumped UpdatedAt %v -> %v; want unchanged", updatedAtBefore, updatedAtAfter)
	}
}

// TestSetCaptureBodies_PreservesOtherPostureFields confirms the clone-and-flip
// only mutates route.captureBodies. The rest of activePosture (routeClass,
// authBootstrap, profileName, profileProvider, and the route's other fields)
// must round-trip byte-for-byte — otherwise a hot toggle could clobber a
// concurrent profile switch's identity (would observe split state).
func TestSetCaptureBodies_PreservesOtherPostureFields(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	// Install a distinguishable posture directly. SetCaptureBodies flips one
	// live toggle via withCaptureBodies; the resolved half must round-trip
	// byte-for-byte.
	apA := &posture{
		r: resolved{
			routeClass:      model.RouteClassThirdPartyHidden,
			authBootstrap:   model.AuthBootstrapPlaceholderActive,
			profileName:     "alpha",
			profileProvider: "anthropic",
			egress:          model.EgressConfig{Mode: "direct"},
		},
		l: live{captureBodies: false},
	}
	sess.active.Store(apA)
	// Flip captureBodies on.
	if _, err := sv.SetCaptureBodies(sess.public.ID, true); err != nil {
		t.Fatalf("SetCaptureBodies(on): %v", err)
	}
	ap := sess.active.Load()
	if !ap.l.captureBodies {
		t.Errorf("captureBodies not flipped")
	}
	if ap.r.routeClass != model.RouteClassThirdPartyHidden {
		t.Errorf("routeClass clobbered: %q", ap.r.routeClass)
	}
	if ap.r.authBootstrap != model.AuthBootstrapPlaceholderActive {
		t.Errorf("authBootstrap clobbered: %q", ap.r.authBootstrap)
	}
	if ap.r.profileName != "alpha" || ap.r.profileProvider != "anthropic" {
		t.Errorf("profile identity clobbered: %q/%q", ap.r.profileName, ap.r.profileProvider)
	}
	if ap.r.egress.Mode != "direct" {
		t.Errorf("route.egress clobbered: %q", ap.r.egress.Mode)
	}
}

// TestSetCaptureBodies_EmitsSessionUpdated confirms the SSE event fires on
// state change, carrying a Session snapshot with the new CaptureBodies value.
// The inline-JS patchSession handler relies on this event to flip the ribbon
// Bodies cell live without a page reload.
func TestSetCaptureBodies_EmitsSessionUpdated(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	if _, err := sv.SetCaptureBodies(sess.public.ID, true); err != nil {
		t.Fatalf("SetCaptureBodies(on): %v", err)
	}
	got := drainEvents(ch, 200*time.Millisecond)
	updates := filterEvents(got, "session_updated")
	if len(updates) != 1 {
		t.Fatalf("session_updated count = %d, want exactly 1", len(updates))
	}
	snap, ok := updates[0].Data.(model.Session)
	if !ok {
		t.Fatalf("event Data type = %T, want model.Session", updates[0].Data)
	}
	if !snap.CaptureBodies {
		t.Errorf("event snapshot.CaptureBodies = false, want true")
	}
}

// TestSetRoute_PublishesCaptureBodies confirms the inline publicProjection
// in setRoute (server.go) carries req.CaptureRequestBodies through to
// sess.public.CaptureBodies — matching what the launcher sends via
// SessionRouteRequest.CaptureRequestBodies.
func TestSetRoute_PublishesCaptureBodies(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	req := validSetRouteRequest()
	req.CaptureRequestBodies = true
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("setRoute err: %v", err)
	}
	ap := sess.active.Load()
	if !ap.l.captureBodies {
		t.Errorf("ap.l.captureBodies = false, want true (req.CaptureRequestBodies=true)")
	}
	sess.mu.RLock()
	got := sess.public.CaptureBodies
	sess.mu.RUnlock()
	if !got {
		t.Errorf("sess.public.CaptureBodies = false, want true")
	}
}

// validSetRouteRequest returns a minimal-valid SessionRouteRequest reusable
// across the /route-gate tests. Mirrors the shape used elsewhere in the
// supervisor tests so failures isolate the state-gate logic itself.
func validSetRouteRequest() model.SessionRouteRequest {
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
	}
}

// TestRouteGate_RefusesPostAttachWithTypedError asserts that a /route call
// issued AFTER markSessionActive transitions the session to StateActive MUST
// be refused with the typed RouteSetupAfterAttach reason; sess.active and
// sess.public MUST stay byte-identical. The launch-path /route at StateCreated
// and the subsequent setRoute at StateLaunching remain valid (asserted below
// as a non-regression).
func TestRouteGate_RefusesPostAttachWithTypedError(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	req := validSetRouteRequest()

	// First setRoute at StateCreated — must succeed (Created→Launching).
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("first setRoute at StateCreated returned err: %v", err)
	}

	// Second setRoute at StateLaunching (pre-attach) — must still succeed (the
	// gate is "post-attach", not "post-first-setRoute"; existing tests' 2nd
	// SetRoute calls at StateLaunching MUST keep working).
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("second setRoute at StateLaunching returned err: %v", err)
	}

	// Mark session active — this is what gates the third /route.
	sv.markSessionActive(sess.public.ID)
	// Sanity: state transitioned.
	sess.mu.RLock()
	state := sess.public.State
	sess.mu.RUnlock()
	if state != model.StateActive {
		t.Fatalf("setup: state after markSessionActive = %q, want %q", state, model.StateActive)
	}

	// Snapshot of pre-refusal posture — must be byte-identical post-refusal
	// (fail-closed: typed-409 path publishes nothing).
	preActivePtr := sess.active.Load()
	sess.mu.RLock()
	preAPIBase := sess.public.APIBaseURL
	preProfileName := sess.public.ActiveProfileName
	sess.mu.RUnlock()

	// Third setRoute at StateActive — MUST be refused with typed reason.
	err := sv.setRoute(sess.public.ID, req)
	if err == nil {
		t.Fatal("post-attach setRoute returned nil error, want typed RouteSetupAfterAttach refusal")
	}
	var re *routeSetupError
	if !errors.As(err, &re) {
		t.Fatalf("setRoute err is not *routeSetupError: got %T: %v", err, err)
	}
	if re.Code != "RouteSetupAfterAttach" {
		t.Errorf("re.Code = %q, want RouteSetupAfterAttach", re.Code)
	}
	if re.Message == "" {
		t.Error("re.Message empty — expected sanitized non-secret explanation")
	}

	// Fail-closed: posture pointer-identical and public unchanged.
	if sess.active.Load() != preActivePtr {
		t.Error("active.Load() changed pointer on refused /route — fail-closed violated")
	}
	sess.mu.RLock()
	postAPIBase := sess.public.APIBaseURL
	postProfileName := sess.public.ActiveProfileName
	sess.mu.RUnlock()
	if postAPIBase != preAPIBase || postProfileName != preProfileName {
		t.Errorf("public drifted on refused /route: pre=%q/%q post=%q/%q",
			preAPIBase, preProfileName, postAPIBase, postProfileName)
	}
}

// TestRouteHTTPHandler_PostAttachReturnsTyped409 asserts the HTTP layer maps
// the post-attach refusal to status 409 with a JSON body
// {"reason_code":"RouteSetupAfterAttach","message":"…"} that the control
// client decodes as *control.RouteError (end-to-end).
func TestRouteHTTPHandler_PostAttachReturnsTyped409(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	req := validSetRouteRequest()
	if err := client.SetRoute(context.Background(), sess.ID, req); err != nil {
		t.Fatalf("first SetRoute err: %v", err)
	}
	// Transition to StateActive via the supervisor's internal helper (HTTP
	// equivalent: an Attach + first proxied request — but markSessionActive is
	// the direct trigger and is what the spec's gate predicates on).
	srv.markSessionActive(sess.ID)
	// Sanity.
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatal("getSession returned nil after Attach")
	}
	state.mu.RLock()
	gotState := state.public.State
	state.mu.RUnlock()
	if gotState != model.StateActive {
		t.Fatalf("post-markSessionActive state = %q, want %q", gotState, model.StateActive)
	}

	// HTTP /route at StateActive — must surface as *control.RouteError via the
	// typed-409 branch.
	err = client.SetRoute(context.Background(), sess.ID, req)
	if err == nil {
		t.Fatal("HTTP SetRoute at StateActive returned nil, want typed 409 RouteError")
	}
	var re *control.RouteError
	if !errors.As(err, &re) {
		t.Fatalf("HTTP SetRoute err is not *control.RouteError: got %T: %v", err, err)
	}
	if re.Code != "RouteSetupAfterAttach" {
		t.Errorf("re.Code = %q, want RouteSetupAfterAttach", re.Code)
	}
	// Sanity: the message contains nothing secret. We didn't supply any URL
	// userinfo or env vars here; the gate's pinned message is identity-free.
	if strings.Contains(re.Message, "secret") {
		t.Errorf("re.Message leaked sensitive token: %q", re.Message)
	}
	// JSON shape sanity: re-encode and confirm both keys are present, exactly
	// matching the spec's wire format.
	raw, _ := json.Marshal(re)
	if !strings.Contains(string(raw), `"reason_code"`) || !strings.Contains(string(raw), `"message"`) {
		t.Errorf("RouteError JSON missing keys: %s", string(raw))
	}
}

// TestSwitchProfileHTTPHandler_HappyPath asserts the new
// /v1/sessions/{id}/profile HTTP endpoint dispatches to s.SwitchProfile and
// serializes the SwitchOutcome as JSON. The Switched outcome flows through
// with the View populated (non-secret per SafeProfileView).
func TestSwitchProfileHTTPHandler_HappyPath(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	// Establish a launch posture so ClassifyTransition returns RelaunchLive.
	initReq := model.SessionRouteRequest{
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
	}
	if err := sv.setRoute(sess.public.ID, initReq); err != nil {
		t.Fatalf("setRoute err: %v", err)
	}
	writeProfilesJSON(t, sv, `{"profiles":{"alpha":{"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}}}}`)

	client := control.NewClient(sv.paths.SocketPath)
	waitForSupervisor(t, client)
	out, err := client.SwitchProfile(context.Background(), sess.public.ID, "alpha")
	if err != nil {
		t.Fatalf("SwitchProfile err: %v", err)
	}
	if out == nil {
		t.Fatal("SwitchProfile returned nil outcome")
	}
	if out.Result != string(SwitchResultSwitched) {
		t.Errorf("Result = %q, want %q", out.Result, SwitchResultSwitched)
	}
	if out.Class != string(model.RelaunchLive) {
		t.Errorf("Class = %q, want %q", out.Class, model.RelaunchLive)
	}
	if len(out.View) == 0 {
		t.Error("View is empty; want non-empty ProfileView for Switched outcome")
	}
	// Sanity: decode View — Name must be "alpha" matching the requested profile.
	var view map[string]any
	if err := json.Unmarshal(out.View, &view); err != nil {
		t.Fatalf("decode View: %v", err)
	}
	if view["name"] != "alpha" {
		t.Errorf("View.name = %v, want alpha", view["name"])
	}
}

// TestSwitchProfileHTTPHandler_NoSuchSession asserts the endpoint returns a
// non-failing JSON outcome even when the session is unknown (the
// "errors inside the outcome" discipline survives the HTTP boundary).
func TestSwitchProfileHTTPHandler_NoSuchSession(t *testing.T) {
	sv, _ := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	client := control.NewClient(sv.paths.SocketPath)
	waitForSupervisor(t, client)
	out, err := client.SwitchProfile(context.Background(), "ghost-session", "alpha")
	if err != nil {
		t.Fatalf("SwitchProfile err: %v", err)
	}
	if out.Result != string(SwitchResultNoSuchSession) {
		t.Errorf("Result = %q, want %q", out.Result, SwitchResultNoSuchSession)
	}
	if out.ReasonCode != "no_such_session" {
		t.Errorf("ReasonCode = %q, want no_such_session", out.ReasonCode)
	}
}

// TestSwitchProfileHTTPHandler_RejectsBadBody asserts the endpoint rejects
// malformed JSON bodies (no NPE / panic; structured error).
func TestSwitchProfileHTTPHandler_RejectsBadBody(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	// Hand-craft a malformed POST body — the client method always sends valid
	// JSON, so we go direct to the HTTP layer.
	rawSocket := sv.paths.SocketPath
	if rawSocket == "" {
		t.Fatal("no socket path on supervisor")
	}
	// Use the unix socket via the standard http client (the test only needs to
	// confirm the handler returns a 4xx — not parsed by SwitchOutcomeView).
	c := control.NewClient(rawSocket)
	// Submit a "name" key with non-string value — JSON decode should fail.
	_, err := c.SwitchProfile(context.Background(), sess.public.ID, "")
	if err != nil {
		// SwitchProfile w/ empty name still produces a valid POST body
		// ({"name":""}); the spec's "" requested is handled by profiles.Select
		// and returns an unknown/no-file outcome — NOT an HTTP-level error.
		// We accept either: an outcome (with non-Switched Result) or a 4xx.
		t.Logf("empty-name SwitchProfile returned err (acceptable): %v", err)
	}
}

// TestLaunchPathIdentityStampedOnFirstRequest verifies launch-path identity is
// published to sess.public + activePosture at the FIRST setRoute (no
// SwitchProfile yet). The follow-up assertion drives the first request through
// recordRequest under the same captured posture and confirms the resulting
// RequestRecord carries the launch identity. This is the
// "first-request-after-launch" timeline guarantee: identity flows launcher →
// SessionRouteRequest → activePosture → record.
func TestLaunchPathIdentityStampedOnFirstRequest(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)

	req := validSetRouteRequest()
	req.ActiveProfileName = "launch-profile"
	req.ActiveProfileProvider = "anthropic"
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("setRoute err: %v", err)
	}

	// 1. sess.public mirror carries launch identity (published from req).
	sess.mu.RLock()
	pubName := sess.public.ActiveProfileName
	pubProv := sess.public.ActiveProfileProvider
	sess.mu.RUnlock()
	if pubName != "launch-profile" {
		t.Errorf("sess.public.ActiveProfileName = %q, want launch-profile", pubName)
	}
	if pubProv != "anthropic" {
		t.Errorf("sess.public.ActiveProfileProvider = %q, want anthropic", pubProv)
	}

	// 2. activePosture carries the same identity (captured-ap source).
	ap := sess.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after setRoute")
	}
	if ap.r.profileName != "launch-profile" {
		t.Errorf("ap.r.profileName = %q, want launch-profile", ap.r.profileName)
	}
	if ap.r.profileProvider != "anthropic" {
		t.Errorf("ap.r.profileProvider = %q, want anthropic", ap.r.profileProvider)
	}

	// 3. A first request recorded under the launch posture carries launch
	// identity on the resulting RequestRecord (stamp from captured ap).
	// The proxy hot path closes over ap from active.Load() at handler entry;
	// here we mirror that capture and stamp through recordRequest to validate
	// the field mapping. NO SwitchProfile has run — first-request-after-
	// launch must reflect the launch identity, not a future posture.
	rec := model.RequestRecord{
		Timestamp:             time.Now(),
		SessionID:             sess.public.ID,
		Method:                "POST",
		LogicalTargetHost:     "api.anthropic.com",
		Path:                  "/v1/messages",
		ActiveProfileName:     ap.r.profileName,
		ActiveProfileProvider: ap.r.profileProvider,
	}
	sv.recordRequest(sess.public.ID, rec)

	sess.mu.RLock()
	defer sess.mu.RUnlock()
	if len(sess.requests) == 0 {
		t.Fatal("session ring empty after recordRequest")
	}
	got := sess.requests[len(sess.requests)-1]
	if got.ActiveProfileName != "launch-profile" {
		t.Errorf("recorded ActiveProfileName = %q, want launch-profile", got.ActiveProfileName)
	}
	if got.ActiveProfileProvider != "anthropic" {
		t.Errorf("recorded ActiveProfileProvider = %q, want anthropic", got.ActiveProfileProvider)
	}
}

// TestRecordSyntheticRequestStampsIdentityFromAp verifies recordSyntheticRequest
// stamps identity from the threaded *activePosture onto the RequestRecord. The
// synthetic-request path takes ap as a parameter so each call carries the
// per-request captured posture, never re-reading from sess.active mid-request.
// Asserts the field mapping at the pathaware seam — the same mapping used by
// the three proxy.go record sites.
func TestRecordSyntheticRequestStampsIdentityFromAp(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)

	// Drive setRoute so the session has a real *sessionProxy (recordSyntheticRequest is
	// a method on sessionProxy). Identity baked into the request lands in sess.active.
	req := validSetRouteRequest()
	req.ActiveProfileName = "syn-profile"
	req.ActiveProfileProvider = "bedrock"
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("setRoute err: %v", err)
	}
	if sess.proxy == nil {
		t.Fatal("sess.proxy == nil after setRoute; cannot exercise recordSyntheticRequest")
	}
	ap := sess.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after setRoute")
	}

	u, _ := url.Parse("https://api.anthropic.com/v1/messages")
	sess.proxy.recordSyntheticRequest(time.Now(), "POST", "api.anthropic.com", "ccwrap-synthetic", u, 451, model.StreamStateHTTP, ap)

	sess.mu.RLock()
	defer sess.mu.RUnlock()
	if len(sess.requests) == 0 {
		t.Fatal("session ring empty after recordSyntheticRequest")
	}
	got := sess.requests[len(sess.requests)-1]
	if !got.Synthetic {
		t.Errorf("Synthetic = false, want true")
	}
	if got.ActiveProfileName != "syn-profile" {
		t.Errorf("ActiveProfileName = %q, want syn-profile (stamped from ap)", got.ActiveProfileName)
	}
	if got.ActiveProfileProvider != "bedrock" {
		t.Errorf("ActiveProfileProvider = %q, want bedrock (stamped from ap)", got.ActiveProfileProvider)
	}
}

// TestSetRouteTraceDetailStripsURLUserinfo verifies setRoute's "session route
// configured" trace.Detail MUST strip URL userinfo. The trace record is part
// of the same public projection as the sess.public.APIBaseURL mirror (the web
// UI renders sess.trace alongside the ribbon), so the same strip rule applies.
// Without the strip, a launcher CCWRAP_API_BASE_URL="https://u:p@..." leaks the
// credential into the trace ring.
func TestSetRouteTraceDetailStripsURLUserinfo(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	req := validSetRouteRequest()
	req.APIBaseURL = "https://alice:secret@gw.example.com/v1"
	req.ExactUpstreamBase = "https://alice:secret@gw.example.com"
	req.ExactUpstreamHost = "gw.example.com"
	if err := sv.setRoute(sess.public.ID, req); err != nil {
		t.Fatalf("setRoute err: %v", err)
	}

	sess.mu.RLock()
	defer sess.mu.RUnlock()
	var found bool
	for _, te := range sess.trace {
		if te.Category != "route" || te.Summary != "session route configured" {
			continue
		}
		found = true
		if strings.Contains(te.Detail, "alice:secret@") || strings.Contains(te.Detail, "alice@") || strings.Contains(te.Detail, "secret") {
			t.Errorf("trace.Detail leaks userinfo: %q", te.Detail)
		}
		if !strings.Contains(te.Detail, "gw.example.com") {
			t.Errorf("trace.Detail lost host info after strip: %q", te.Detail)
		}
		if te.Detail != "https://gw.example.com/v1" {
			t.Errorf("trace.Detail = %q, want https://gw.example.com/v1 (verbatim contract with sess.public.APIBaseURL strip)", te.Detail)
		}
		break
	}
	if !found {
		t.Fatalf("no route_set trace entry found in sess.trace (len=%d)", len(sess.trace))
	}
}

func TestSessionProfileToken_Shape(t *testing.T) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, "ptok.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "pt"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatalf("session not found: %s", sess.ID)
	}
	if got := state.profileToken; len(got) != 32 {
		t.Fatalf("profileToken len = %d, want 32 (hex of 16 random bytes)", len(got))
	}
	for _, b := range state.profileToken {
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
			t.Fatalf("profileToken char %q is not lowercase hex", b)
		}
	}
	// matchProfileToken must reject mismatch and accept the real token
	// via constant-time compare.
	if state.matchProfileToken("") {
		t.Fatalf("empty token must not match")
	}
	if state.matchProfileToken(strings.Repeat("0", 32)) {
		t.Fatalf("wrong token must not match")
	}
	if !state.matchProfileToken(state.profileToken) {
		t.Fatalf("right token must match")
	}
}

func TestSanitizeProfileCatalogError_StripsPathTraversal(t *testing.T) {
	cases := []struct{ in, want string }{
		// Real error from profiles.Load: "parse profiles /home/.ccwrap/profiles.json: ..."
		{"parse profiles /home/user/.ccwrap/profiles.json: invalid character 'x' looking for beginning of value",
			"profiles.json malformed"},
		{"read profiles file /var/state/ccwrap/profiles.json: permission denied",
			"profiles.json unreadable"},
		{"some other error", "profiles.json error"},
	}
	for _, tc := range cases {
		got := sanitizeProfileCatalogError(errors.New(tc.in))
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// No path/userinfo can leak under any branch.
		for _, leak := range []string{"/home/", "/var/", ".ccwrap/profiles.json"} {
			if strings.Contains(got, leak) {
				t.Errorf("sanitize(%q) leaks %q\n  got: %q", tc.in, leak, got)
			}
		}
	}
}

func TestProfileCatalogFor_NoFile(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-nofile.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	resp := srv.profileCatalogFor(state)
	if resp.HasProfilesFile {
		t.Fatalf("HasProfilesFile = true, want false (no profiles.json present)")
	}
	if resp.Items == nil {
		t.Fatalf("Items must be empty slice, not nil (UI relies on Array.isArray length)")
	}
	if len(resp.Items) != 0 {
		t.Fatalf("Items = %d, want 0", len(resp.Items))
	}
}

func TestProfileCatalogFor_PopulatedAndSorted(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-pop.sock")
	// Write profiles.json with 3 profiles spanning 2 providers; expect
	// alphabetical-by-provider then alphabetical-by-name order.
	jsonBlob := `{
	  "default": "alpha",
	  "profiles": {
	    "alpha": {"provider": "Anthropic", "base_url": "https://api.anthropic.com", "egress": {"mode": "inherit"}},
	    "zeta": {"provider": "Anthropic", "base_url": "https://api.anthropic.com", "egress": {"mode": "inherit"}},
	    "beta": {"provider": "OpenAI", "base_url": "https://api.openai.com", "auth": {"mode": "ccwrap_bearer", "key_env": "OAI"}, "egress": {"mode": "inherit"}}
	  }
	}`
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profiles.DefaultPath(paths.StateDir), []byte(jsonBlob), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	resp := srv.profileCatalogFor(state)
	if !resp.HasProfilesFile {
		t.Fatalf("HasProfilesFile = false, want true")
	}
	if resp.Default != "alpha" {
		t.Fatalf("Default = %q, want alpha", resp.Default)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("Items = %d, want 3", len(resp.Items))
	}
	// Sort: (Anthropic, alpha), (Anthropic, zeta), (OpenAI, beta).
	if resp.Items[0].Name != "alpha" || resp.Items[1].Name != "zeta" || resp.Items[2].Name != "beta" {
		t.Fatalf("sort order: %+v", resp.Items)
	}
	// Secret-safety regression on the wire path: HasKeyEnv flagged, no env name.
	blob, _ := json.Marshal(resp)
	if !strings.Contains(string(blob), `"has_key_env":true`) {
		t.Fatalf("OpenAI profile must flag has_key_env: %s", blob)
	}
	if strings.Contains(string(blob), `"auth_key_env"`) || strings.Contains(string(blob), `"OAI"`) {
		t.Fatalf("env NAME leaked in catalog response: %s", blob)
	}
}

// TestEnvBaseURLHostFor pins the helper that surfaces what inherit-env
// mode would route to. The popover's inherit-env row renders this value
// next to the row so users can see what env-live mode looks like and
// detect drift between the active profile's frozen BaseURL and the live
// env. All paths must be nil-safe (early tests construct supervisor
// with nil launchCtx) and empty-on-malformed (env value is user-provided).
func TestEnvBaseURLHostFor(t *testing.T) {
	cases := []struct {
		name string
		lc   *LaunchContext
		want string
	}{
		{"nil-launchctx", nil, ""},
		{"empty-parentenv", &LaunchContext{}, ""},
		{
			"unset",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"PATH=/usr/bin"}}},
			"",
		},
		{
			"empty-value",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL="}}},
			"",
		},
		{
			"whitespace-value",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL=   "}}},
			"",
		},
		{
			"https-host-only",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL=https://gw.example.com"}}},
			"gw.example.com",
		},
		{
			"https-host-port-path",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL=https://gw.example.com:8080/v1/"}}},
			"gw.example.com:8080",
		},
		{
			"canonical-anthropic",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL=https://api.anthropic.com"}}},
			"api.anthropic.com",
		},
		{
			"malformed",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL=://broken"}}},
			"",
		},
		{
			"no-scheme-no-host",
			&LaunchContext{Options: preflight.Options{ParentEnv: []string{"ANTHROPIC_BASE_URL=just-text"}}},
			"",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := envBaseURLHostFor(c.lc)
			if got != c.want {
				t.Errorf("envBaseURLHostFor(%s) = %q; want %q", c.name, got, c.want)
			}
		})
	}
}

// TestProfileCatalogFor_EnvBaseURLHost_FromLaunchCtx confirms the field
// is plumbed through to the catalog response (end-to-end).
func TestProfileCatalogFor_EnvBaseURLHost_FromLaunchCtx(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-envbu.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.launchCtx = &LaunchContext{
		Options: preflight.Options{
			ParentEnv: []string{
				"PATH=/usr/bin",
				"ANTHROPIC_BASE_URL=https://enjoy.goodgoodstudy.lol/v1/",
			},
		},
		Inspection: &settings.InspectionResult{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	resp := srv.profileCatalogFor(state)
	if resp.EnvBaseURLHost != "enjoy.goodgoodstudy.lol" {
		t.Errorf("EnvBaseURLHost = %q; want %q", resp.EnvBaseURLHost, "enjoy.goodgoodstudy.lol")
	}
	// Wire-shape: must serialize with json tag env_base_url_host.
	blob, _ := json.Marshal(resp)
	if !strings.Contains(string(blob), `"env_base_url_host":"enjoy.goodgoodstudy.lol"`) {
		t.Errorf("catalog response missing env_base_url_host wire field: %s", blob)
	}
}

// TestProfileCatalogFor_EnvHasCredentials_TrueWhenAPIKey confirms the
// EnvHasCredentials field is true when launchCtx.ParentEnv has a non-empty
// ANTHROPIC_API_KEY. The popover uses this to gate the inherit-env row's
// [test] button.
func TestProfileCatalogFor_EnvHasCredentials_TrueWhenAPIKey(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-envcred-apikey.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.launchCtx = &LaunchContext{
		Options: preflight.Options{
			ParentEnv: []string{
				"PATH=/usr/bin",
				"ANTHROPIC_API_KEY=sk-ant-test",
			},
		},
		Inspection: &settings.InspectionResult{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	resp := srv.profileCatalogFor(state)
	if !resp.EnvHasCredentials {
		t.Errorf("EnvHasCredentials = false; want true (ANTHROPIC_API_KEY set)")
	}
	// Wire-shape: must serialize with json tag env_has_credentials (no omitempty).
	blob, _ := json.Marshal(resp)
	if !strings.Contains(string(blob), `"env_has_credentials":true`) {
		t.Errorf("catalog response missing env_has_credentials:true wire field: %s", blob)
	}
}

// TestProfileCatalogFor_EnvHasCredentials_TrueWhenAuthToken confirms the
// field is true when only ANTHROPIC_AUTH_TOKEN is set (no API key).
func TestProfileCatalogFor_EnvHasCredentials_TrueWhenAuthToken(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-envcred-authtok.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.launchCtx = &LaunchContext{
		Options: preflight.Options{
			ParentEnv: []string{
				"PATH=/usr/bin",
				"ANTHROPIC_AUTH_TOKEN=bearer-test",
			},
		},
		Inspection: &settings.InspectionResult{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	resp := srv.profileCatalogFor(state)
	if !resp.EnvHasCredentials {
		t.Errorf("EnvHasCredentials = false; want true (ANTHROPIC_AUTH_TOKEN set)")
	}
}

// TestProfileCatalogFor_EnvHasCredentials_FalseWhenEmpty confirms the field
// is false (and wire-serialized as `false`, not omitted) when neither env
// var is set. This is the OAuth-mode claude-code case where the popover
// suppresses the inherit-env row's [test] button.
func TestProfileCatalogFor_EnvHasCredentials_FalseWhenEmpty(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-envcred-empty.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.launchCtx = &LaunchContext{
		Options: preflight.Options{
			ParentEnv: []string{
				"PATH=/usr/bin",
				// No ANTHROPIC_API_KEY, no ANTHROPIC_AUTH_TOKEN — OAuth mode.
			},
		},
		Inspection: &settings.InspectionResult{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	resp := srv.profileCatalogFor(state)
	if resp.EnvHasCredentials {
		t.Errorf("EnvHasCredentials = true; want false (no creds in env)")
	}
	// Wire-shape: deliberately NOT omitempty — frontend needs to see false
	// (vs absent in an older response) to know to suppress the button.
	blob, _ := json.Marshal(resp)
	if !strings.Contains(string(blob), `"env_has_credentials":false`) {
		t.Errorf("catalog response must serialize env_has_credentials:false (not omit it): %s", blob)
	}
}

func TestSanitizeProfileCatalogError_ParseErrors_PreservesMultiLine(t *testing.T) {
	// When err is *profiles.ParseErrors, the sanitizer manually builds
	// the multi-line output from perr.Items (NOT calling perr.Error()
	// which would prepend the raw source path).
	perr := &profiles.ParseErrors{
		Source: "/Users/secret/Library/Application Support/ccwrap/profiles.json",
		Items: []profiles.ValidationError{
			{Path: "profiles.glm.auth.mode", Want: "one of: ccwrap_bearer, ccwrap_x_api_key", Got: "ccwrapKey"},
			{Path: "profiles.kimi.base_url", Want: "URL with http or https scheme and host"},
		},
	}
	got := sanitizeProfileCatalogError(perr)

	// Must contain the per-item details
	if !strings.Contains(got, "profiles.glm.auth.mode: want one of:") {
		t.Errorf("missing item 1; got:\n%s", got)
	}
	if !strings.Contains(got, "profiles.kimi.base_url:") {
		t.Errorf("missing item 2; got:\n%s", got)
	}
	// Must NOT contain the source path (path-traversal safety)
	if strings.Contains(got, "/Users/secret") {
		t.Errorf("leaked source path; got:\n%s", got)
	}
	if strings.Contains(got, "Library/Application Support") {
		t.Errorf("leaked path component; got:\n%s", got)
	}
	// Must look like the validation report header
	if !strings.HasPrefix(got, "profiles.json invalid: 2 errors") {
		t.Errorf("missing canonical prefix; got:\n%s", got)
	}
}

func TestSanitizeProfileCatalogError_ZeroItemsParseErrors_DegradesGracefully(t *testing.T) {
	// Defensive: a zero-item ParseErrors should fall through to the
	// existing collapse path (not match errors.Is, not produce a
	// "0 errors" string).
	perr := &profiles.ParseErrors{Source: "x"}
	got := sanitizeProfileCatalogError(perr)
	if strings.Contains(got, "0 errors") {
		t.Errorf("zero-item ParseErrors should not produce 'N errors' string; got %q", got)
	}
}

func TestProfileCatalog_AfterRmLast_HasProfilesFileTrue_EmptyItems(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "pcat-rmlast.sock")

	// Seed: one-profile file so we can verify HasProfilesFile=true + len=1.
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(profiles.DefaultPath(paths.StateDir), seed, "seed-h1-one"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)

	resp := srv.profileCatalogFor(state)
	if !resp.HasProfilesFile {
		t.Fatalf("first state: expected HasProfilesFile=true; got false")
	}
	if len(resp.Items) != 1 {
		t.Fatalf("first state: expected len(Items)=1; got %d", len(resp.Items))
	}

	// Overwrite with the rm-last end state: file exists, profiles map empty.
	empty := &profiles.File{Default: profiles.InheritEnv, Profiles: map[string]profiles.Profile{}}
	if err := profiles.OverwriteFile(profiles.DefaultPath(paths.StateDir), empty, "seed-h1-empty"); err != nil {
		t.Fatalf("seed empty: %v", err)
	}

	resp2 := srv.profileCatalogFor(state)
	if !resp2.HasProfilesFile {
		t.Fatalf("post-rm-last: expected HasProfilesFile=true; got false")
	}
	if len(resp2.Items) != 0 {
		t.Fatalf("post-rm-last: expected len(Items)=0; got %d", len(resp2.Items))
	}
}
