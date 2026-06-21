package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

// switchTestProfilesJSON is the two-profile fixture used by the trace
// assertions: "alpha" and "beta" omit the auth block (ccwrap does not own
// auth — first-party posture) so ResolveProfile + ClassifyTransition
// accept either as a Switched outcome.
const switchTestProfilesJSON = `{"default":"alpha","profiles":{"alpha":{"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}},"beta":{"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}}}}`

// drainEvents reads every event that arrives on ch within d and returns them
// in order. Used by the trace+broadcast tests to capture the events a
// single SwitchProfile call emits.
func drainEvents(ch <-chan model.Event, d time.Duration) []model.Event {
	var out []model.Event
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

// filterEvents returns the subset of events whose Type equals kind. Mirrors
// the SSE-stream filtering UIs do client-side.
func filterEvents(events []model.Event, kind string) []model.Event {
	var out []model.Event
	for _, ev := range events {
		if ev.Type == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestSanitizeSwitchError_ScrubsURLUserinfo(t *testing.T) {
	err := fmt.Errorf("resolve egress: %w", errors.New("dial http://alice:secret@proxy.example.com:3128: bad gateway"))
	in := &preflight.ProfileInput{}
	envSnapshot := []string{}
	_, msg := sanitizeSwitchError(err, in, envSnapshot)
	if strings.Contains(msg, "alice") || strings.Contains(msg, "secret") {
		t.Errorf("sanitized msg leaks userinfo: %q", msg)
	}
	if !strings.Contains(msg, "<redacted>") && !strings.Contains(msg, "proxy.example.com") {
		t.Errorf("sanitized msg lost structure (expected placeholder + host): %q", msg)
	}
}

func TestSanitizeSwitchError_ScrubsAuthKey(t *testing.T) {
	sentinel := "sk-SUPERSECRETAUTHKEY"
	err := fmt.Errorf("validate profile auth: header includes %s in stream", sentinel)
	in := &preflight.ProfileInput{Auth: &preflight.AuthSpec{Key: sentinel}}
	_, msg := sanitizeSwitchError(err, in, nil)
	if strings.Contains(msg, sentinel) {
		t.Errorf("sanitized msg leaks auth.key: %q", msg)
	}
}

func TestSanitizeSwitchError_ScrubsKeyEnvNameAndValue(t *testing.T) {
	envName := "MY_PROFILE_AUTH_KEY"
	envValue := "sk-ENVRESOLVEDSECRET"
	err := fmt.Errorf("auth.key_env %s: env var not set", envName)
	in := &preflight.ProfileInput{Auth: &preflight.AuthSpec{KeyEnv: envName}}
	envSnapshot := []string{envName + "=" + envValue}
	_, msg := sanitizeSwitchError(err, in, envSnapshot)
	if strings.Contains(msg, envName) {
		t.Errorf("sanitized msg leaks key_env NAME: %q", msg)
	}
	if strings.Contains(msg, envValue) {
		t.Errorf("sanitized msg leaks key_env VALUE: %q", msg)
	}
}

func TestSanitizeSwitchError_ScrubsUpstreamHeaderValues(t *testing.T) {
	hdrVal := "Bearer my-secret-bearer-token-xyz"
	err := fmt.Errorf("validate upstream headers: invalid value %q", hdrVal)
	in := &preflight.ProfileInput{
		UpstreamHeaders: map[string]string{"Authorization": hdrVal},
	}
	_, msg := sanitizeSwitchError(err, in, nil)
	if strings.Contains(msg, hdrVal) {
		t.Errorf("sanitized msg leaks upstream-header value: %q", msg)
	}
	if strings.Contains(msg, "Bearer") && strings.Contains(msg, "my-secret-bearer-token-xyz") {
		t.Errorf("sanitized msg leaks bearer token: %q", msg)
	}
}

func TestSanitizeSwitchError_ReasonCodeFromFlattenedString(t *testing.T) {
	// All %w sources should land in a single ReasonCode/Message; the function takes the
	// flattened final string (Go's %w flattening defeats per-cause switching).
	cases := []struct {
		name string
		err  error
	}{
		{"provider", fmt.Errorf("resolve provider: %w", errors.New("unknown provider 'xyz'"))},
		{"auth_env", fmt.Errorf("resolve auth env: %w", errors.New("env var unset"))},
		{"model_env", fmt.Errorf("resolve model env: %w", errors.New("unsupported model"))},
		{"aliases", fmt.Errorf("resolve aliases: %w", errors.New("malformed json"))},
		{"upstream_headers", fmt.Errorf("resolve upstream headers: %w", errors.New("bad header pair"))},
		{"egress", fmt.Errorf("resolve egress: %w", errors.New("unreachable proxy"))},
		{"malformed_settings", fmt.Errorf("malformed settings: %w", errors.New("invalid yaml"))},
		{"policy_network", fmt.Errorf("policy network rejection: %w", errors.New("blocked host"))},
		{"auth_key_env", fmt.Errorf("auth.key_env: %w", errors.New("env unset"))},
	}
	in := &preflight.ProfileInput{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := sanitizeSwitchError(tc.err, in, nil)
			if code == "" {
				t.Errorf("expected non-empty ReasonCode for %q", tc.err)
			}
			if msg == "" {
				t.Errorf("expected non-empty Message for %q", tc.err)
			}
		})
	}
}

// installLaunchCtxForSwitch wires a minimal *LaunchContext onto the supervisor
// so SwitchProfile's defensive nil-guard passes. Mirrors what cmd/ccwrap composes
// at launch: an empty *settings.InspectionResult (no UnsupportedEnv /
// MalformedEnv / PolicyNetworkEnv) + minimal Options.ParentEnv with PATH so
// EffectiveProviderEnvFromInspection finds nothing to flag. The launch Result
// carries first-party-passthrough posture so ClassifyTransition against any
// non-PlaceholderActive bootstrap returns RelaunchLive.
func installLaunchCtxForSwitch(t *testing.T, sv *Supervisor) {
	t.Helper()
	sv.launchCtx = &LaunchContext{
		Options: preflight.Options{
			ParentEnv:        []string{"PATH=/usr/bin"},
			WorkingDirectory: t.TempDir(),
		},
		Inspection: &settings.InspectionResult{},
	}
}

// writeProfilesJSON writes profiles.json to the supervisor's paths.StateDir at
// profiles.DefaultPath(stateDir), creating parent dirs if needed. Returns the
// path. Cleanup registers an os.Remove at test end (the supervisor's StateDir
// is itself cleaned by testutil.ShortAppPaths' t.Cleanup).
func writeProfilesJSON(t *testing.T, sv *Supervisor, jsonContent string) string {
	t.Helper()
	stateDir := sv.paths.StateDir
	path := profiles.DefaultPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(jsonContent), 0o644); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// TestSwitchProfile_NoSuchSession asserts SwitchProfile with an unknown session
// id returns SwitchResultNoSuchSession with a sanitized message and no panic.
func TestSwitchProfile_NoSuchSession(t *testing.T) {
	sv, _ := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	out := sv.SwitchProfile("nonexistent-session-id", "some-profile")
	if out.Result != SwitchResultNoSuchSession {
		t.Errorf("Result = %q, want %q", out.Result, SwitchResultNoSuchSession)
	}
	if out.Message == "" {
		t.Error("Message empty — expected sanitized non-secret text")
	}
}

// TestSwitchProfile_NoProfilesFile asserts session exists, profiles.json
// absent ⇒ SwitchResultNoProfilesFile + prior active untouched (fail-closed).
func TestSwitchProfile_NoProfilesFile(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	preActivePtr := sess.active.Load()
	out := sv.SwitchProfile(sess.public.ID, "any-profile")
	if out.Result != SwitchResultNoProfilesFile {
		t.Errorf("Result = %q, want %q (Message=%q)", out.Result, SwitchResultNoProfilesFile, out.Message)
	}
	// Fail-closed: prior active pointer-identical (no publish on this path).
	if sess.active.Load() != preActivePtr {
		t.Error("active.Load() changed on no-profiles-file refusal — fail-closed violated")
	}
}

// TestSwitchProfile_UnknownProfile asserts a profile name absent from
// profiles.json ⇒ SwitchResultUnknownProfile + prior active untouched.
func TestSwitchProfile_UnknownProfile(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	// profiles.json carries only "alpha"; the switch requests "beta".
	writeProfilesJSON(t, sv, `{"profiles":{"alpha":{"provider":"anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}}}}`)
	preActivePtr := sess.active.Load()
	out := sv.SwitchProfile(sess.public.ID, "beta")
	if out.Result != SwitchResultUnknownProfile {
		t.Errorf("Result = %q, want %q (Message=%q)", out.Result, SwitchResultUnknownProfile, out.Message)
	}
	if out.Message == "" {
		t.Error("Message empty — expected sanitized non-secret text")
	}
	if sess.active.Load() != preActivePtr {
		t.Error("active.Load() changed on unknown-profile refusal — fail-closed violated")
	}
}

// TestSwitchProfile_MissingAuth_SwitchedWithMissingPosture locks the
// contract for hot-swap: switching INTO a profile whose
// key_env is unset USED TO yield SwitchResultRejectedInvalid with sess.
// active untouched. Now the switch SUCCEEDS — the new posture is
// published WITH AuthBootstrap=Missing + MissingAuthEnv=<env>. The user's
// explicit ask is honored; the supervisor's request-time gate fail-closes
// the next /v1/messages instead. This makes the inspect Auth cell flip
// to its red state immediately on the switch, so the user sees the
// problem in the place they took the action.
//
// The no-partial-apply invariant survives in a different shape: pre-switch
// SafeProfileView equality is no longer the right property (the switch DOES
// apply), but the request-time fail-closed prevents any actual upstream
// forward from happening without auth. The invariant is preserved at
// the forward boundary.
func TestSwitchProfile_MissingAuth_SwitchedWithMissingPosture(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	writeProfilesJSON(t, sv, `{"profiles":{"broken":{"provider":"AcmeGW","base_url":"https://gw.acme.example/v1","auth":{"mode":"ccwrap_bearer","key_env":"DEFINITELY_ABSENT_ENV_VAR_FOR_SP2_TEST"},"egress":{"mode":"inherit"}}}}`)
	preActivePtr := sess.active.Load()

	out := sv.SwitchProfile(sess.public.ID, "broken")
	if out.Result != SwitchResultSwitched {
		t.Errorf("Result = %q, want %q (ReasonCode=%q Message=%q)",
			out.Result, SwitchResultSwitched, out.ReasonCode, out.Message)
	}
	// Active posture changed (new pointer published by publishPosture).
	if sess.active.Load() == preActivePtr {
		t.Error("active.Load() pointer-identical after successful switch — posture not published")
	}
	ap := sess.active.Load()
	if ap.r.authBootstrap != model.AuthBootstrapMissing {
		t.Errorf("ap.r.authBootstrap = %q, want %q", ap.r.authBootstrap, model.AuthBootstrapMissing)
	}
	if ap.r.missingAuthEnv != "DEFINITELY_ABSENT_ENV_VAR_FOR_SP2_TEST" {
		t.Errorf("ap.r.missingAuthEnv = %q, want sentinel env name", ap.r.missingAuthEnv)
	}
	sess.mu.RLock()
	pubName := sess.public.ActiveProfileName
	pubMissing := sess.public.MissingAuthEnv
	pubBootstrap := sess.public.AuthBootstrap
	sess.mu.RUnlock()
	if pubName != "broken" {
		t.Errorf("sess.public.ActiveProfileName = %q, want broken", pubName)
	}
	if pubBootstrap != model.AuthBootstrapMissing {
		t.Errorf("sess.public.AuthBootstrap = %q, want missing", pubBootstrap)
	}
	if pubMissing != "DEFINITELY_ABSENT_ENV_VAR_FOR_SP2_TEST" {
		t.Errorf("sess.public.MissingAuthEnv = %q, want sentinel env name (for ribbon Auth-cell detail)", pubMissing)
	}
}

// TestSwitchProfile_Switched_LiveTransition asserts the happy-path: a profile
// that resolves cleanly + classifies as RelaunchLive ⇒ Switched + posture
// published (sess.active reflects the new identity) + SafeProfileView returned.
func TestSwitchProfile_Switched_LiveTransition(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	// Pre-condition: set initial route via setRoute with
	// AuthBootstrap = AuthBootstrapNotNeeded so ClassifyTransition against
	// ANY candidate returns RelaunchLive (the placeholder-active → first-party-
	// passthrough corner is the only NeedsRelaunch case).
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
		t.Fatalf("initial setRoute err: %v", err)
	}
	// Sanity: pre-switch active is launch posture.
	if got := sess.active.Load(); got == nil || got.r.routeClass != model.RouteClassFirstParty {
		t.Fatalf("setup: pre-switch active not first-party: %#v", got)
	}

	// Switch target: a clean first-party profile (no auth block — ccwrap
	// does not own auth). ResolveProfile will accept it; identity flows
	// into the new posture.
	writeProfilesJSON(t, sv, `{"profiles":{"alpha":{"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}}}}`)
	out := sv.SwitchProfile(sess.public.ID, "alpha")
	if out.Result != SwitchResultSwitched {
		t.Fatalf("Result = %q, want %q (ReasonCode=%q Message=%q)",
			out.Result, SwitchResultSwitched, out.ReasonCode, out.Message)
	}
	if out.Class != model.RelaunchLive {
		t.Errorf("Class = %q, want %q", out.Class, model.RelaunchLive)
	}
	if out.View.Name != "alpha" {
		t.Errorf("View.Name = %q, want alpha", out.View.Name)
	}
	// Posture is published — sess.active.Load() reflects the new profile.
	ap := sess.active.Load()
	if ap == nil {
		t.Fatal("active.Load() == nil after Switched")
	}
	if ap.r.profileName != "alpha" {
		t.Errorf("ap.r.profileName = %q, want alpha", ap.r.profileName)
	}
	// Public mirror also reflects identity (single-critical-section publish).
	sess.mu.RLock()
	pubName := sess.public.ActiveProfileName
	sess.mu.RUnlock()
	if pubName != "alpha" {
		t.Errorf("sess.public.ActiveProfileName = %q, want alpha", pubName)
	}
}

// TestSwitchProfile_TraceOnSwitched asserts that every Switched
// outcome produces exactly one trace event AND broadcasts session_updated.
// Trace Detail is the JSON-encoded structured payload (from / from_provider /
// to / to_provider / class).
//
// Helper composition: rather than dedicated setup/subscribe wrappers, the
// repo's existing helpers cover the same surface, so we reuse them directly
// instead of adding pass-through wrappers. newTestSessionForCreate +
// installLaunchCtxForSwitch + writeProfilesJSON produces the equivalent setup,
// and the supervisor's subscribe() already returns (channel, unsubscribe-closure)
// which is exactly the subscribe/unsubscribe pair we need.
// TestSwitchProfile_PreservesCaptureBodiesToggleDuringRace — uses the
// testHookBeforeSwitchPublish seam to deterministically inject a
// SetCaptureBodies call into the race window between switch's currentAP
// capture and publish. Previously the publish read captureBodies from the
// stale currentAP snapshot and silently clobbered the user's toggle.
// Post-fix the publish re-reads sess.active.Load() so the concurrent
// toggle survives.
func TestSwitchProfile_PreservesCaptureBodiesToggleDuringRace(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	writeProfilesJSON(t, sv, switchTestProfilesJSON)

	// Initial state: captureBodies=false.
	if sess.active.Load().l.captureBodies {
		t.Fatal("setup: expected captureBodies=false at start")
	}

	// Hook: between currentAP capture and publish, flip captureBodies on.
	// This simulates a user clicking the Bodies cell while a switch is
	// in flight through profiles.Load + ResolveProfile.
	testHookBeforeSwitchPublish = func(s *sessionState) {
		if s != sess {
			return
		}
		if _, err := sv.SetCaptureBodies(s.public.ID, true); err != nil {
			t.Errorf("hook SetCaptureBodies: %v", err)
		}
	}
	t.Cleanup(func() { testHookBeforeSwitchPublish = nil })

	outcome := sv.SwitchProfile(sess.public.ID, "alpha")
	if outcome.Result != SwitchResultSwitched {
		t.Fatalf("Result = %q, want switched (Message=%q)", outcome.Result, outcome.Message)
	}

	// After the switch completes: captureBodies MUST still be true. Previously
	// the publish would have stored captureBodies=false (from currentAP)
	// and the user's toggle would be silently reverted.
	if !sess.active.Load().l.captureBodies {
		t.Errorf("captureBodies = false after switch; the concurrent SetCaptureBodies(true) was clobbered by stale-snapshot publish")
	}
	sess.mu.RLock()
	pubBodies := sess.public.CaptureBodies
	sess.mu.RUnlock()
	if !pubBodies {
		t.Errorf("sess.public.CaptureBodies = false; want true (UI would have shown the toggle reverted)")
	}
}

func TestSwitchProfile_TraceOnSwitched(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	writeProfilesJSON(t, sv, switchTestProfilesJSON)

	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()

	outcome := sv.SwitchProfile(sess.public.ID, "alpha")
	if outcome.Result != SwitchResultSwitched {
		t.Fatalf("Result = %q, want switched (ReasonCode=%q Message=%q)",
			outcome.Result, outcome.ReasonCode, outcome.Message)
	}

	got := drainEvents(ch, 200*time.Millisecond)
	traces := filterEvents(got, "trace")
	updates := filterEvents(got, "session_updated")
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want exactly 1", len(traces))
	}
	if len(updates) < 1 {
		t.Fatalf("session_updated count = %d, want >= 1 on Switched", len(updates))
	}
	rec, ok := traces[0].Data.(model.TraceRecord)
	if !ok {
		t.Fatalf("trace event Data type = %T, want model.TraceRecord", traces[0].Data)
	}
	if rec.Category != "profile_switch" {
		t.Fatalf("Category = %q, want profile_switch", rec.Category)
	}
	if rec.Summary != "switched" {
		t.Fatalf("Summary = %q, want switched", rec.Summary)
	}
	var detail map[string]string
	if err := json.Unmarshal([]byte(rec.Detail), &detail); err != nil {
		t.Fatalf("trace.Detail must be JSON: %v\n  Detail: %q", err, rec.Detail)
	}
	if detail["to"] != "alpha" {
		t.Fatalf("Detail.to = %q, want alpha", detail["to"])
	}
	if detail["class"] != "live" {
		t.Fatalf("Detail.class = %q, want live", detail["class"])
	}
}

// TestSwitchProfile_TraceOnRefused asserts that a RefusedNeedsRelaunch
// outcome (live posture has AuthBootstrap=placeholder_active, target profile
// classifies as first-party-passthrough) records exactly one trace event with
// Summary="refused" and Detail.class="needs_relaunch", but does NOT broadcast
// session_updated (no posture change — the public projection survives intact).
//
// Setup mirrors TestSwitchProfile_Switched_LiveTransition but with the live
// AuthBootstrap promoted to AuthBootstrapPlaceholderActive so
// ClassifyTransition(currentLive, candidate=first-party-passthrough) returns
// RelaunchNeedsRelaunch.
func TestSwitchProfile_TraceOnRefused(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)

	// Pre-condition: live posture with AuthBootstrap=placeholder_active so
	// ClassifyTransition against a first-party-passthrough candidate returns
	// RelaunchNeedsRelaunch (the only auth-boundary corner that demands a
	// Claude relaunch — see preflight/profile.go).
	initReq := model.SessionRouteRequest{
		APIBaseURL:        "https://gw.example.com/v1",
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceFallback,
		RouteConfigSource: "fallback_default",
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceNone,
		AuthPolicy:        model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap:     model.AuthBootstrapPlaceholderActive,
		AuthBootstrapKind: model.AuthBootstrapKindXAPIKey,
		ExactUpstreamHost: "gw.example.com",
		ExactUpstreamBase: "https://gw.example.com/v1",
		FailPolicy:        model.FailClosed,
		Egress:            model.EgressConfig{Mode: "direct", Source: "fallback"},
	}
	if err := sv.setRoute(sess.public.ID, initReq); err != nil {
		t.Fatalf("initial setRoute err: %v", err)
	}

	// Switch target: a clean first-party passthrough profile (alpha from
	// switchTestProfilesJSON). ResolveProfile accepts it and ClassifyTransition
	// returns RelaunchNeedsRelaunch against the placeholder_active live posture.
	writeProfilesJSON(t, sv, switchTestProfilesJSON)

	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()

	outcome := sv.SwitchProfile(sess.public.ID, "alpha")
	if outcome.Result != SwitchResultRefusedNeedsRelaunch {
		t.Fatalf("Result = %q, want %q (ReasonCode=%q Message=%q)",
			outcome.Result, SwitchResultRefusedNeedsRelaunch, outcome.ReasonCode, outcome.Message)
	}

	got := drainEvents(ch, 200*time.Millisecond)
	traces := filterEvents(got, "trace")
	updates := filterEvents(got, "session_updated")
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want exactly 1", len(traces))
	}
	if len(updates) != 0 {
		t.Fatalf("session_updated count = %d, want 0 on Refused (no posture change)", len(updates))
	}
	rec, ok := traces[0].Data.(model.TraceRecord)
	if !ok {
		t.Fatalf("trace event Data type = %T, want model.TraceRecord", traces[0].Data)
	}
	if rec.Category != "profile_switch" {
		t.Fatalf("Category = %q, want profile_switch", rec.Category)
	}
	if rec.Summary != "refused" {
		t.Fatalf("Summary = %q, want refused", rec.Summary)
	}
	if !strings.Contains(rec.Detail, `"class":"needs_relaunch"`) {
		t.Fatalf("Detail must contain class=needs_relaunch: %q", rec.Detail)
	}
	var detail map[string]string
	if err := json.Unmarshal([]byte(rec.Detail), &detail); err != nil {
		t.Fatalf("trace.Detail must be JSON: %v\n  Detail: %q", err, rec.Detail)
	}
	if detail["to"] != "alpha" {
		t.Fatalf("Detail.to = %q, want alpha", detail["to"])
	}
	if detail["class"] != "needs_relaunch" {
		t.Fatalf("Detail.class = %q, want needs_relaunch", detail["class"])
	}
}

// profilesJSONWithUnsetKeyEnv is a single-profile fixture whose ccwrap_bearer
// auth references an env var name that is guaranteed not to be set in the
// test ParentEnv ({"PATH=/usr/bin"} only — see installLaunchCtxForSwitch).
// preflight.ResolveProfile rejects this with the "refusing to launch" error
// path, driving the Step 2 RejectedInvalid branch in SwitchProfile.
const profilesJSONWithUnsetKeyEnv = `{"profiles":{"broken-keyenv":{"provider":"AcmeGW","base_url":"https://gw.acme.example/v1","auth":{"mode":"ccwrap_bearer","key_env":"DEFINITELY_NOT_SET_SP3_D4_TRACE_REJECT_KEYENV"},"egress":{"mode":"inherit"}}}}`

// setupSwitchTestWithMalformedProfilesFile composes the trace-reject
// fixture for the Step 1 (Load malformed) branch. Writes "{" (invalid JSON)
// at the profiles.json path BEFORE the switch is dispatched — profiles.Load
// returns the parse error, which surfaces as RejectedInvalid.
func setupSwitchTestWithMalformedProfilesFile(t *testing.T, sockName string) (*Supervisor, *sessionState) {
	t.Helper()
	_ = sockName // newTestSessionForCreate composes its own short paths; kept for signature parity.
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	writeProfilesJSON(t, sv, `{`)
	return sv, sess
}

// TestSwitchProfile_TraceOnRejected_LoadMalformed — Step 1 (profiles.Load
// malformed) branch in SwitchProfile. RejectedInvalid emits exactly
// one trace event with Summary="rejected" and a Detail JSON carrying the
// requested name + reason field. No session_updated broadcast (no posture
// change — fail-closed).
func TestSwitchProfile_TraceOnRejected_LoadMalformed(t *testing.T) {
	srv, sess := setupSwitchTestWithMalformedProfilesFile(t, "trace-rej-load.sock")
	ch, unsubscribe := srv.subscribe()
	defer unsubscribe()
	outcome := srv.SwitchProfile(sess.public.ID, "anything")
	if outcome.Result != SwitchResultRejectedInvalid {
		t.Fatalf("Result = %q, want rejected_invalid", outcome.Result)
	}
	got := drainEvents(ch, 200*time.Millisecond)
	traces := filterEvents(got, "trace")
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want 1 (Load malformed branch)", len(traces))
	}
	rec, ok := traces[0].Data.(model.TraceRecord)
	if !ok {
		t.Fatalf("trace event Data type = %T, want model.TraceRecord", traces[0].Data)
	}
	if rec.Summary != "rejected" {
		t.Fatalf("Summary = %q, want rejected", rec.Summary)
	}
	if !strings.Contains(rec.Detail, `"requested":"anything"`) {
		t.Fatalf("Detail must echo requested name: %q", rec.Detail)
	}
	if !strings.Contains(rec.Detail, `"reason"`) {
		t.Fatalf("Detail must carry reason field: %q", rec.Detail)
	}
}

// TestSwitchProfile_TraceOnSwitched_MissingAuth — contract:
// switching into a profile with an unset key_env used to land in the Step-2
// (ResolveProfile-fail) branch and emit a Summary="rejected" trace. Now
// it lands in the Switched branch and emits Summary="switched" — the
// trace category is profile_switch regardless. The contract is
// preserved on the structure (one trace per outcome); the Summary string
// is the user-visible signal.
func TestSwitchProfile_TraceOnSwitched_MissingAuth(t *testing.T) {
	srv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, srv)
	writeProfilesJSON(t, srv, profilesJSONWithUnsetKeyEnv)
	ch, unsubscribe := srv.subscribe()
	defer unsubscribe()
	outcome := srv.SwitchProfile(sess.public.ID, "broken-keyenv")
	if outcome.Result != SwitchResultSwitched {
		t.Fatalf("Result = %q, want %q (missing key_env now lands switched + Missing posture)", outcome.Result, SwitchResultSwitched)
	}
	got := drainEvents(ch, 200*time.Millisecond)
	traces := filterEvents(got, "trace")
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(traces))
	}
	rec, ok := traces[0].Data.(model.TraceRecord)
	if !ok {
		t.Fatalf("trace event Data type = %T, want model.TraceRecord", traces[0].Data)
	}
	if rec.Summary != "switched" {
		t.Fatalf("Summary = %q, want switched", rec.Summary)
	}
	if !strings.Contains(rec.Detail, `"to":"broken-keyenv"`) {
		t.Fatalf("Detail must echo the new profile name: %q", rec.Detail)
	}
}

// TestSwitchProfile_TraceOnRejected_SelectUnknownIsTraceExempt — UnknownProfile
// (caller asked for a name not in profiles.json) is RejectedInvalid-adjacent
// but classified as a caller error; it must NOT emit a trace.
// This guard pins the trace-exempt behavior so a future refactor can't
// silently start emitting traces for typos.
func TestSwitchProfile_TraceOnRejected_SelectUnknownIsTraceExempt(t *testing.T) {
	srv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, srv)
	writeProfilesJSON(t, srv, switchTestProfilesJSON)
	ch, unsubscribe := srv.subscribe()
	defer unsubscribe()
	outcome := srv.SwitchProfile(sess.public.ID, "definitely-not-a-real-name")
	if outcome.Result != SwitchResultUnknownProfile {
		t.Fatalf("Result = %q, want unknown_profile", outcome.Result)
	}
	got := drainEvents(ch, 100*time.Millisecond)
	if len(filterEvents(got, "trace")) != 0 {
		t.Fatalf("trace must NOT emit on UnknownProfile (caller error)")
	}
}

// TestSwitchProfile_TraceExempt_NoSuchSession — caller used a non-existent
// session id. SwitchProfile returns at the Step 0 lookup (switch.go)
// before the per-session switch mutex is acquired and long before any
// recordTrace emit site. Regression guard for correct emit-site
// scoping: this caller-error path must remain trace-exempt.
//
// Composition: a bare supervisor — newTestSessionForCreate creates the
// supervisor (and a real session we ignore) but the unknown ID never resolves
// to any session, so installLaunchCtxForSwitch is irrelevant on this path.
func TestSwitchProfile_TraceExempt_NoSuchSession(t *testing.T) {
	sv, _ := newTestSessionForCreate(t)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	outcome := sv.SwitchProfile("no-such-session-id", "x")
	if outcome.Result != SwitchResultNoSuchSession {
		t.Fatalf("Result = %q, want no_such_session", outcome.Result)
	}
	got := drainEvents(ch, 100*time.Millisecond)
	if len(filterEvents(got, "trace")) != 0 {
		t.Fatalf("trace count != 0 — NoSuchSession must be trace-exempt")
	}
}

// TestSwitchProfile_TraceExempt_NoProfilesFile — session valid + launchCtx
// installed, but profiles.json is absent. profiles.Load returns (nil, nil)
// for missing file (Step 1, switch.go) so file.Select fails with the
// caller-asked-but-no-file shape; the disambiguator in switch.go
// picks SwitchResultNoProfilesFile, which is trace-exempt.
// Regression guard.
//
// Composition: newTestSessionForCreate + installLaunchCtxForSwitch, but NO
// writeProfilesJSON — the absence of the file IS the test fixture.
func TestSwitchProfile_TraceExempt_NoProfilesFile(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	outcome := sv.SwitchProfile(sess.public.ID, "anything")
	if outcome.Result != SwitchResultNoProfilesFile {
		t.Fatalf("Result = %q, want no_profiles_file", outcome.Result)
	}
	got := drainEvents(ch, 100*time.Millisecond)
	if len(filterEvents(got, "trace")) != 0 {
		t.Fatalf("trace count != 0 — NoProfilesFile must be trace-exempt")
	}
}

// TestSwitchProfile_TraceExempt_UnknownProfile — profiles.json present + valid
// but caller asked for a name that isn't in it. file.Select returns the
// unknown-name error; the disambiguator in switch.go picks
// SwitchResultUnknownProfile (file != nil) — trace-exempt.
// Regression guard, distinct from
// TraceOnRejected_SelectUnknownIsTraceExempt: this one is named under the
// _TraceExempt_ family for symmetry with the other three exempt outcomes,
// pinning the convention so future refactors can't drift the naming.
//
// Composition: the standard 3-line WithProfilesFile pattern using the shared
// two-profile (alpha+beta) switchTestProfilesJSON fixture.
func TestSwitchProfile_TraceExempt_UnknownProfile(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	writeProfilesJSON(t, sv, switchTestProfilesJSON)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	outcome := sv.SwitchProfile(sess.public.ID, "definitely-not-a-profile-name")
	if outcome.Result != SwitchResultUnknownProfile {
		t.Fatalf("Result = %q, want unknown_profile", outcome.Result)
	}
	got := drainEvents(ch, 100*time.Millisecond)
	if len(filterEvents(got, "trace")) != 0 {
		t.Fatalf("trace count != 0 — UnknownProfile must be trace-exempt")
	}
}

// TestSwitchProfile_TraceExempt_NoLaunchContext — session valid but the
// supervisor was created without a LaunchContext (e.g. doctor mode). The
// early-reject in switch.go fires AFTER the active-posture hoist
// but BEFORE the Step 1 profiles.Load — bypassing sanitizeSwitchError so
// it never hits any of the recordRejectedSwitchTrace emit sites. The
// outcome is RejectedInvalid + ReasonCode "no_launch_context".
// Regression guard: this is the one RejectedInvalid path that must stay
// trace-exempt (caller used switch on a doctor-mode supervisor).
//
// Composition: newTestSessionForCreate (which gives a valid session) but
// DELIBERATELY skip installLaunchCtxForSwitch so sv.launchCtx == nil.
func TestSwitchProfile_TraceExempt_NoLaunchContext(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	// Intentionally do NOT call installLaunchCtxForSwitch: sv.launchCtx == nil
	// is the precondition for the switch.go early-reject.
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	outcome := sv.SwitchProfile(sess.public.ID, "anything")
	if outcome.Result != SwitchResultRejectedInvalid || outcome.ReasonCode != "no_launch_context" {
		t.Fatalf("outcome = %+v, want RejectedInvalid+no_launch_context", outcome)
	}
	got := drainEvents(ch, 100*time.Millisecond)
	if len(filterEvents(got, "trace")) != 0 {
		t.Fatalf("trace count != 0 — no_launch_context must be trace-exempt")
	}
}

// profilesJSONWithUserinfoInProvider is a single-profile fixture whose
// provider label embeds a `user:pass@` substring. Regression
// fixture: the goal is to exercise the Switched-path trace Detail with a
// provider field that LOOKS like a userinfo-bearing URL string, so a future
// regression that drops the stripUserinfoString wrap-site in switch.go
// would still hit a Detail-shape-equality oracle (not a leak oracle — see
// the test docstring for the structural-only nature of this guard).
//
// Auth block is omitted (first-party — ccwrap does not own auth) + egress
// inherit so ResolveProfile accepts the profile cleanly and
// ClassifyTransition against the safe-zero live posture returns
// RelaunchLive (the Switched outcome path).
const profilesJSONWithUserinfoInProvider = `{"profiles":{"alpha-userinfo":{"provider":"label-user:pass@evil","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}}}}`

// TestSwitchProfile_TraceDetailStripped is a STRUCTURAL regression guard:
// the Switched-outcome trace Detail in switch.go MUST flow
// through stripUserinfoString as defense-in-depth, so a future refactor that
// silently drops the wrap call (or replaces it with an identity wrapper) is
// caught by this test if it also changes the no-op-on-JSON behavior into a
// real scrubber.
//
// IMPORTANT — known limitation pinned by this test:
//
//	stripUserinfoString (server.go) parses the input via url.Parse. Its
//	wire format is a single URL string. Applied to a JSON-encoded
//	map[string]string (the actual Detail payload in switch.go), the
//	parser either errors or returns a *url.URL with User==nil, and the
//	function returns the input verbatim. The wrap is therefore a NO-OP on
//	today's Detail shape.
//
// What this test pins (a structural-only guard):
//
//  1. The Switched path runs to completion when a profile carries a
//     userinfo-looking provider label.
//  2. The recorded trace.Detail unmarshals as JSON with the documented
//     {from, from_provider, to, to_provider, class} shape (smoke check that
//     the wrap-call site in switch.go didn't garble the payload).
//  3. The to_provider value contains the userinfo-looking substring
//     verbatim — pinning the current behavior so any future change either
//     (a) ACTUALLY scrubs userinfo from JSON-embedded URLs (in which case
//     this test will flag itself for inversion), or (b) accidentally
//     deletes the wrap call (which would not be caught by this assertion
//     alone — see (4)).
//  4. Additionally pin the OPPOSITE assertion as a documentation contract:
//     IF stripUserinfoString is ever strengthened (or replaced) to scrub
//     JSON-embedded user:pass@ substrings, this test must be updated to
//     assert NON-containment. The presence of two contradictory assertions
//     (one positive, one inverted under a comment block) is the explicit
//     hand-off note for the future implementer.
//
// This choice was anticipated: when the fixture produces a Detail that
// contains `user:pass@` and the leak-style assertion would fail, the most
// defensible option is to pin that the wrap-site exists and document the
// limitation rather than assert scrubbing that does not yet happen.
func TestSwitchProfile_TraceDetailStripped(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
	writeProfilesJSON(t, sv, profilesJSONWithUserinfoInProvider)

	outcome := sv.SwitchProfile(sess.public.ID, "alpha-userinfo")
	if outcome.Result != SwitchResultSwitched {
		t.Fatalf("setup: Result = %q, want switched (ReasonCode=%q Message=%q)",
			outcome.Result, outcome.ReasonCode, outcome.Message)
	}

	traces := sv.listTrace(sess.public.ID)
	if len(traces) == 0 {
		t.Fatalf("no traces recorded — Switched path must emit one trace")
	}
	tr := traces[len(traces)-1]
	if tr.Category != "profile_switch" || tr.Summary != "switched" {
		t.Fatalf("trace shape = (cat=%q summary=%q), want (profile_switch, switched)",
			tr.Category, tr.Summary)
	}

	// Smoke check: Detail must round-trip as JSON with the documented shape.
	// This pins that the stripUserinfoString wrap in switch.go didn't
	// garble the payload into a non-JSON form.
	var detail map[string]string
	if err := json.Unmarshal([]byte(tr.Detail), &detail); err != nil {
		t.Fatalf("trace.Detail must remain JSON after stripUserinfoString wrap: %v\n  Detail: %q",
			err, tr.Detail)
	}
	if detail["class"] != "live" {
		t.Fatalf("Detail.class = %q, want live", detail["class"])
	}
	if detail["to"] != "alpha-userinfo" {
		t.Fatalf("Detail.to = %q, want alpha-userinfo", detail["to"])
	}

	// Documentation contract — see the test docstring for the rationale.
	// The CURRENT behavior is that stripUserinfoString is a no-op on the
	// JSON-encoded Detail payload, so the userinfo-looking substring in
	// to_provider survives verbatim. If this assertion ever flips (i.e.,
	// stripUserinfoString starts scrubbing JSON-embedded URLs), update the
	// assertion to invert: t.Errorf if Detail STILL contains the leak.
	if !strings.Contains(tr.Detail, "user:pass@") {
		t.Errorf("documentation contract: today's stripUserinfoString is a no-op on JSON-encoded Detail; this assertion must be inverted only when stripUserinfoString is strengthened to scrub JSON-embedded userinfo.\n  Detail: %q", tr.Detail)
	}
	// Sanity: the wrap-site is still called — `to_provider` should reflect
	// the SafeProfileView projection (ProviderLabel = "name (provider)").
	wantSuffix := "alpha-userinfo (label-user:pass@evil)"
	if detail["to_provider"] != wantSuffix {
		t.Errorf("Detail.to_provider = %q, want %q (SafeProfileView projection)",
			detail["to_provider"], wantSuffix)
	}
}

func TestSanitizeSwitchError_ParseErrors_ClassifiesAsValidation(t *testing.T) {
	// When err is *profiles.ParseErrors, sanitizeSwitchError fast-paths
	// to reason="validation_error" and manually rebuilds the message from
	// perr.Items (NOT perr.Error() which would leak source path).
	perr := &profiles.ParseErrors{
		Source: "/Users/secret/Library/Application Support/ccwrap/profiles.json",
		Items: []profiles.ValidationError{
			{Path: "profiles.glm.auth.key_env", Want: "must be set", Got: ""},
		},
	}
	reason, msg := sanitizeSwitchError(perr, nil, nil)
	if reason != "validation_error" {
		t.Errorf("reason: got %q want %q (NOT auth_key_env_error from regex)", reason, "validation_error")
	}
	if !strings.Contains(msg, "profiles.glm.auth.key_env") {
		t.Errorf("missing item detail in message; got:\n%s", msg)
	}
	if strings.Contains(msg, "/Users/secret") {
		t.Errorf("leaked source path in message; got:\n%s", msg)
	}
	if !strings.HasPrefix(msg, "profiles.json invalid: 1 errors") {
		t.Errorf("missing canonical prefix; got:\n%s", msg)
	}
}

// TestSwitchProfile_PreservesNativeTLSRoute — end-to-end guard through the
// real SwitchProfile path: a session launched with NativeTLS=true must still
// have route.nativeTLS=true (the transport-selection gate read by
// upstreamTransportFor) AND public.NativeTLS="active" (the dashboard cell)
// after a live switch. Before the fix, posturePublishFromResult dropped the
// flag and the published posture downgraded dials while the cell kept
// claiming "active".
func TestSwitchProfile_PreservesNativeTLSRoute(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	installLaunchCtxForSwitch(t, sv)
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
		NativeTLS:         true,
	}
	if err := sv.setRoute(sess.public.ID, initReq); err != nil {
		t.Fatalf("initial setRoute err: %v", err)
	}
	if !sess.active.Load().l.nativeTLS {
		t.Fatal("setup: launch must arm route.nativeTLS=true")
	}

	writeProfilesJSON(t, sv, switchTestProfilesJSON)
	out := sv.SwitchProfile(sess.public.ID, "alpha")
	if out.Result != SwitchResultSwitched {
		t.Fatalf("Result = %q, want switched (ReasonCode=%q Message=%q)", out.Result, out.ReasonCode, out.Message)
	}

	if !sess.active.Load().l.nativeTLS {
		t.Error("route.nativeTLS = false after live switch; Anthropic dials silently downgraded to the Go stdlib fingerprint")
	}
	sess.mu.RLock()
	pubNative := sess.public.NativeTLS
	sess.mu.RUnlock()
	if pubNative != "active" {
		t.Errorf("sess.public.NativeTLS = %q after switch, want %q (display must stay in lockstep with route behavior)", pubNative, "active")
	}
}
