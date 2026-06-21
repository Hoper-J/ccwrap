package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// testHookBeforeSwitchPublish is a deterministic seam for race tests. nil in
// production; invoked exactly once per SwitchProfile entry, AFTER currentAP
// was captured at the top of the handler and AFTER the disk-IO phase
// (profiles.Load / ResolveProfile) but BEFORE the live-posture re-read
// that drives the publish step. Tests set this to a func that calls
// SetCaptureBodies to interleave a Bodies toggle into the race window —
// without the re-read fix, the publish would silently clobber the
// concurrent SetCaptureBodies. The seam fires with sess as argument so
// the hook can target a specific session.
var testHookBeforeSwitchPublish func(sess *sessionState)

// SwitchResult is the per-switch outcome enum.
type SwitchResult string

const (
	SwitchResultSwitched             SwitchResult = "switched"
	SwitchResultRefusedNeedsRelaunch SwitchResult = "refused_needs_relaunch"
	SwitchResultRejectedInvalid      SwitchResult = "rejected_invalid"
	SwitchResultNoSuchSession        SwitchResult = "no_such_session"
	SwitchResultNoProfilesFile       SwitchResult = "no_profiles_file"
	SwitchResultUnknownProfile       SwitchResult = "unknown_profile"
)

// SwitchOutcome is the SwitchProfile return value. Errors are INSIDE the outcome —
// never returned/logged raw across any boundary.
//
// JSON tags use snake_case to match the repo wire convention (model/types.go)
// and the SwitchOutcomeView mirror type in internal/control. The same outcome
// shape serializes to the CLI / web UI verbatim, so adding/renaming a field
// here must be coordinated with the client mirror.
type SwitchOutcome struct {
	Result     SwitchResult          `json:"result"`
	Class      model.RelaunchClass   `json:"class,omitempty"`
	View       preflight.ProfileView `json:"view,omitempty"` // ALREADY userinfo-stripped via preflight.SafeProfileView
	ReasonCode string                `json:"reason_code,omitempty"`
	Message    string                `json:"message,omitempty"`
}

// urlUserinfoRegex matches "scheme://user[:pw]@host" in a free-form string.
// Conservative: requires a recognizable scheme prefix + userinfo + @ + non-@ host start.
// Anchored to avoid false positives on email addresses or path fragments containing "@".
var urlUserinfoRegex = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s/@]+@[^\s/]+`)

// sanitizeSwitchError scrubs secret-bearing substrings from the flattened error
// string and returns a (ReasonCode, redactedMsg) pair safe for control-payload
// and CLI emission. Operates on the flattened final string (Go's
// %w flattening defeats per-cause type-switching). Scrubs to stable placeholders:
//   - URL userinfo: scheme://user[:pw]@host → scheme://<redacted>@host
//   - in.Auth.Key (literal): → <redacted-auth-key>
//   - in.Auth.KeyEnv NAME: → <redacted-env-name>
//   - resolved value of in.Auth.KeyEnv (looked up once in envSnapshot): → <redacted-key-env-value>
//   - upstream-header VALUES (from in.UpstreamHeaders): → <redacted-header-value>
//
// Coverage: all %w sources ResolveProfile wraps — provider/auth-env, model-env,
// aliases, upstream-headers, egress, unsupported, malformed-settings,
// policy-network, auth-key-env.
func sanitizeSwitchError(err error, in *preflight.ProfileInput, envSnapshot []string) (reasonCode string, redactedMsg string) {
	if err == nil {
		return "", ""
	}
	// Multi-error validation report fast-path. Mirrors the catalog
	// sanitizer — manually build message from perr.Items so the
	// source file path never reaches SwitchOutcome.Message (which
	// flows to the browser via control-plane). Pre-empts
	// classifySwitchErrorReason's substring regex (e.g. a "key_env"
	// path token would otherwise be mis-tagged "auth_key_env_error").
	var perr *profiles.ParseErrors
	if errors.As(err, &perr) && len(perr.Items) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "profiles.json invalid: %d errors", len(perr.Items))
		for _, it := range perr.Items {
			fmt.Fprintf(&b, "\n  - %s", it.Error())
		}
		return "validation_error", b.String()
	}
	rawMsg := err.Error()
	msg := rawMsg

	// 1. URL userinfo strip (scheme://user[:pw]@ → scheme://<redacted>@):
	msg = scrubURLUserinfo(msg)

	// 2. in.Auth.Key literal — scrub byte-for-byte:
	if in != nil && in.Auth != nil && strings.TrimSpace(in.Auth.Key) != "" {
		msg = strings.ReplaceAll(msg, in.Auth.Key, "<redacted-auth-key>")
	}

	// 3. in.Auth.KeyEnv name + its resolved value:
	if in != nil && in.Auth != nil && strings.TrimSpace(in.Auth.KeyEnv) != "" {
		envName := in.Auth.KeyEnv
		envValue := lookupEnvSnapshot(envSnapshot, envName)
		// Scrub value FIRST (longer string, more specific), then name.
		if envValue != "" {
			msg = strings.ReplaceAll(msg, envValue, "<redacted-key-env-value>")
		}
		msg = strings.ReplaceAll(msg, envName, "<redacted-env-name>")
	}

	// 4. Upstream-header values (from in.UpstreamHeaders map):
	if in != nil {
		for _, val := range in.UpstreamHeaders {
			if strings.TrimSpace(val) == "" {
				continue
			}
			msg = strings.ReplaceAll(msg, val, "<redacted-header-value>")
		}
	}

	// 5. Reason code derivation — best-effort tagging based on the original
	//    (unsanitized) wrap prefix. Defaults to "resolve_error" if no pattern matches.
	reasonCode = classifySwitchErrorReason(rawMsg)

	return reasonCode, msg
}

// scrubURLUserinfo rewrites every scheme://user[:pw]@host occurrence in s to
// scheme://<redacted>@host.
func scrubURLUserinfo(s string) string {
	return urlUserinfoRegex.ReplaceAllStringFunc(s, func(m string) string {
		// m looks like "scheme://user[:pw]@host..."; replace the userinfo portion.
		schemeEnd := strings.Index(m, "://")
		if schemeEnd < 0 {
			return m
		}
		atIdx := strings.Index(m[schemeEnd+3:], "@")
		if atIdx < 0 {
			return m
		}
		return m[:schemeEnd+3] + "<redacted>@" + m[schemeEnd+3+atIdx+1:]
	})
}

// lookupEnvSnapshot finds the value for name in an os.Environ()-shape
// ["KEY=VALUE", ...] slice. Returns "" if not found or unset. Used SOLELY for
// scrub purposes — never logged.
func lookupEnvSnapshot(envSnapshot []string, name string) string {
	if name == "" {
		return ""
	}
	prefix := name + "="
	for _, kv := range envSnapshot {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// classifySwitchErrorReason returns a stable, non-secret reason code based on
// the original (unsanitized) error string's wrap prefix. The caller sanitizes
// the message separately. Best-effort; defaults to "resolve_error".
func classifySwitchErrorReason(rawMsg string) string {
	lower := strings.ToLower(rawMsg)
	switch {
	case strings.Contains(lower, "auth.key_env"), strings.Contains(lower, "key_env"):
		return "auth_key_env_error"
	case strings.Contains(lower, "resolve provider"), strings.Contains(lower, "unknown provider"):
		return "provider_error"
	case strings.Contains(lower, "resolve auth"), strings.Contains(lower, "auth env"), strings.Contains(lower, "auth_env"):
		return "auth_error"
	case strings.Contains(lower, "resolve model"), strings.Contains(lower, "model env"):
		return "model_error"
	case strings.Contains(lower, "alias"):
		return "alias_error"
	case strings.Contains(lower, "upstream header"):
		return "upstream_header_error"
	case strings.Contains(lower, "egress"), strings.Contains(lower, "proxy"):
		return "egress_error"
	case strings.Contains(lower, "malformed settings"), strings.Contains(lower, "settings"):
		return "settings_error"
	case strings.Contains(lower, "policy"), strings.Contains(lower, "blocked"):
		return "policy_error"
	case strings.Contains(lower, "unsupported"):
		return "unsupported_error"
	default:
		return "resolve_error"
	}
}

// recordRejectedSwitchTrace emits the "rejected" trace event with
// the structured fields {from, from_provider, requested, reason}. Called
// from each of the three RejectedInvalid branches in SwitchProfile (Step 1
// Load malformed, Step 1 Select collapse-to-rejected, Step 2 ResolveProfile
// fail). No session_updated broadcast: posture is unchanged on every
// rejected path (fail-closed). Trace-exempt outcomes
// (UnknownProfile, NoProfilesFile, NoSuchSession, no_launch_context) bypass
// this helper entirely.
//
// All four fields are NON-SECRET identity / classification: previous name +
// provider come from the live posture's profileName/profileProvider (already
// userinfo-safe at publish); requested is the caller's literal name
// (display-only — never used as a path component); reason is the stable
// ReasonCode from sanitizeSwitchError (e.g. "auth_key_env_error"), never the
// flattened error message. We still funnel the marshaled detail string
// through stripUserinfoString as defense-in-depth, mirroring the Switched /
// Refused trace pipeline.
func (s *Supervisor) recordRejectedSwitchTrace(sess *sessionState, requested, reason, prevName, prevProvider string) {
	detailJSON, _ := json.Marshal(map[string]string{
		"from":          prevName,
		"from_provider": prevProvider,
		"requested":     requested,
		"reason":        reason,
	})
	s.recordTrace(sess.public.ID, model.TraceRecord{
		Timestamp: time.Now().UTC(),
		SessionID: sess.public.ID,
		Category:  "profile_switch",
		Summary:   "rejected",
		Detail:    stripUserinfoString(string(detailJSON)),
	})
}

// SwitchProfile re-resolves the session's routing posture under the named
// profile and publishes the new posture atomically (or refuses, leaking
// nothing). This is the central correctness gate. Errors are INSIDE the
// SwitchOutcome — never returned/logged raw across any boundary; every error
// path runs through sanitizeSwitchError.
//
// Per-session switch mutex (sess.switchMu) serializes concurrent calls so
// validate → classify → publish cannot race. The mutex is held across
// the full procedure but is independent of sess.mu (which gates the single-
// critical-section publish itself).
//
// Fail-closed — the central correctness invariant: any error at
// any step leaves sess.active / sess.public / transports UNTOUCHED. The prior
// posture survives intact, pointer-identically.
//
// Steps:
//
//  1. profiles.Load + (*File).Select(requested). Missing-file / unknown /
//     ambiguous-group ⇒ matching SwitchResult + sanitized message; nothing
//     published. inherit-env ⇒ Profile:nil (full launch Options + Inspection
//     + file-content snapshots reused).
//
//  2. Build opts with Profile swapped; preflight.ResolveProfile(opts, inspect).
//     err ⇒ RejectedInvalid via sanitizeSwitchError; nothing published.
//
//  3. preflight.ClassifyTransition(currentLive, candidate).
//     RelaunchNeedsRelaunch ⇒ RefusedNeedsRelaunch + pinned non-secret
//     advice; nothing published.
//
//  4. RelaunchLive ⇒ old.withResolved(newResolved(pre)) (new resolved ⊕ live
//     carried forward) → atomic Store + EAGER deriveInto (one sess.mu.Lock
//     critical section) → drainSupersededTransports(newKey) iff egress key
//     changed → Switched + SafeProfileView(pre).
func (s *Supervisor) SwitchProfile(sessionID, requested string) SwitchOutcome {
	// Step 0: session lookup. NoSuchSession is the only result that bypasses
	// the switch mutex (there is no session to serialize on).
	sess := s.getSession(sessionID)
	if sess == nil {
		return SwitchOutcome{
			Result:     SwitchResultNoSuchSession,
			ReasonCode: "no_such_session",
			Message:    "session not found",
		}
	}
	sess.switchMu.Lock()
	defer sess.switchMu.Unlock()

	// Hoist the active-posture capture above ALL early-return branches so
	// each outcome path can derive previousProfileName/Provider. createSession
	// installs a safe-zero *posture (server.go), so currentAP is non-nil
	// in production paths; the nil-guard mirrors the existing pattern below
	// for parity.
	currentAP := sess.active.Load()
	previousProfileName := "inherit-env"
	previousProfileProvider := ""
	if currentAP != nil {
		if currentAP.r.profileName != "" {
			previousProfileName = currentAP.r.profileName
		}
		previousProfileProvider = currentAP.r.profileProvider
	}

	// Defensive: LaunchContext is required for byte-faithful resolution. In
	// production it is installed by cmd/ccwrap at startup; a nil here means
	// the supervisor was created without a launch context (e.g. doctor mode)
	// and switch is unsupported. Fail-closed via a sanitized rejection.
	if s.launchCtx == nil {
		return SwitchOutcome{
			Result:     SwitchResultRejectedInvalid,
			ReasonCode: "no_launch_context",
			Message:    "switch unavailable: launch context not retained",
		}
	}

	// Step 1: profiles.Load(DefaultPath) → (*File).Select(requested). Missing
	// file is NOT an error from Load (returns (nil, nil)); only a malformed
	// profiles.json error surfaces here.
	path := profiles.DefaultPath(s.paths.StateDir)
	file, err := profiles.Load(path)
	if err != nil {
		// Malformed profiles.json — surface as RejectedInvalid (the profile
		// payload was unparseable / unreadable for non-not-exist reasons).
		code, msg := sanitizeSwitchError(err, &preflight.ProfileInput{}, s.launchCtx.Options.ParentEnv)
		// Emit one rejected trace before returning. No broadcast.
		s.recordRejectedSwitchTrace(sess, requested, code, previousProfileName, previousProfileProvider)
		return SwitchOutcome{
			Result:     SwitchResultRejectedInvalid,
			ReasonCode: code,
			Message:    msg,
		}
	}
	selected, _, selErr := file.Select(requested)
	if selErr != nil {
		code, msg := sanitizeSwitchError(selErr, &preflight.ProfileInput{}, s.launchCtx.Options.ParentEnv)
		// Disambiguate result type by whether profiles.json existed:
		//   file == nil  ⇒ caller asked for an explicit profile but no
		//                  profiles.json is configured (NoProfilesFile).
		//   file != nil  ⇒ unknown name OR ambiguous-group (UnknownProfile;
		//                  ambiguous-group collapses to UnknownProfile here).
		result := SwitchResultUnknownProfile
		if file == nil {
			result = SwitchResultNoProfilesFile
		}
		// Forward-looking trace gate. The current Select-error
		// classification collapses to UnknownProfile / NoProfilesFile (both
		// trace-exempt caller errors), so the guard does not fire today. The
		// conditional is intentional: a future refactor that promotes Select
		// failures to RejectedInvalid will pick up the trace emission for free,
		// while UnknownProfile typos stay silent. NoProfilesFile is similarly
		// skipped (no profiles configured is a configuration state, not a
		// switch rejection).
		if result == SwitchResultRejectedInvalid {
			s.recordRejectedSwitchTrace(sess, requested, code, previousProfileName, previousProfileProvider)
		}
		return SwitchOutcome{
			Result:     result,
			ReasonCode: code,
			Message:    msg,
		}
	}

	// Step 2: build opts with Profile swapped; resolve. ResolveProfile runs
	// the identical code path as at launch — only file I/O is bypassed via
	// the precomputed content snapshots. The 4-tier precedence holds.
	opts := s.launchCtx.Options
	opts.Profile = preflight.FromProfile(selected) // nil for inherit-env
	profileInput := opts.Profile
	if profileInput == nil {
		profileInput = &preflight.ProfileInput{}
	}
	pre, err := preflight.ResolveProfile(opts, s.launchCtx.Inspection)
	if err != nil {
		code, msg := sanitizeSwitchError(err, profileInput, s.launchCtx.Options.ParentEnv)
		// Emit one rejected trace before returning. No broadcast.
		s.recordRejectedSwitchTrace(sess, requested, code, previousProfileName, previousProfileProvider)
		return SwitchOutcome{
			Result:     SwitchResultRejectedInvalid,
			ReasonCode: code,
			Message:    msg,
		}
	}

	// Step 3: classify transition against the LIVE posture's authBootstrap.
	// Defense: sess.active.Load() is non-nil post-createSession (the safe-zero
	// install), and a SwitchProfile call between createSession and the first
	// setRoute sees AuthBootstrap == "" (zero RelaunchClass input), which
	// classifies as RelaunchLive — the live-publish path. No nil deref.
	// currentAP was hoisted above for previousProfile* derivation; only
	// the AuthBootstrap projection for ClassifyTransition is built here.
	current := &preflight.Result{}
	if currentAP != nil {
		current.AuthBootstrap = currentAP.r.authBootstrap
	}
	class := preflight.ClassifyTransition(current, pre)
	if class == model.RelaunchNeedsRelaunch {
		// Pinned non-secret advice: identity only, no credentials,
		// no URL userinfo. SafeProfileView already userinfo-strips
		// EgressSummary — we use the request name (callers' literal) for the
		// `--profile` hint.
		view := preflight.SafeProfileView(pre)
		advice := fmt.Sprintf("switch to %q needs first-party auth; relaunch with --profile %s", view.Name, strings.TrimSpace(requested))

		// Emit one trace on the RefusedNeedsRelaunch outcome.
		// NO session_updated broadcast: posture is unchanged (fail-closed),
		// so subscribers must not observe a public-projection refresh.
		// Detail JSON shape matches the Switched-path trace (from / from_provider
		// / to / to_provider / class) for SSE consumer parity, but with
		// class="needs_relaunch". All five fields are NON-SECRET identity; the
		// marshaled string still passes through stripUserinfoString as
		// defense-in-depth (parity with the rest of the trace pipeline in server.go).
		toName := view.Name
		if toName == "" {
			toName = "inherit-env"
		}
		detailJSON, _ := json.Marshal(map[string]string{
			"from":          previousProfileName,
			"from_provider": previousProfileProvider,
			"to":            toName,
			"to_provider":   view.ProviderLabel,
			"class":         "needs_relaunch",
		})
		s.recordTrace(sess.public.ID, model.TraceRecord{
			Timestamp: time.Now().UTC(),
			SessionID: sess.public.ID,
			Category:  "profile_switch",
			Summary:   "refused",
			Detail:    stripUserinfoString(string(detailJSON)),
		})
		return SwitchOutcome{
			Result:     SwitchResultRefusedNeedsRelaunch,
			Class:      class,
			View:       view,
			ReasonCode: "needs_relaunch",
			Message:    advice,
		}
	}

	// Step 4: RelaunchLive — publish the new resolved half while carrying the
	// live half forward. The live toggles (captureBodies, captureTelemetry,
	// nativeTLS) are launch-time booleans, not profile-driven knobs, so a switch
	// must PRESERVE whatever is live rather than reset them. withResolved keeps
	// p.l structurally — there is no preserve flag and no field-by-field clone,
	// so a field (e.g. nativeTLS) can never be forgotten. The expensive
	// Result→resolved mapping runs OUTSIDE the lock; the Store + EAGER deriveInto
	// happen in one sess.mu critical section, so a concurrent SetCapture* (which
	// also takes sess.mu) lands entirely before or after, never torn.
	//
	// oldKey/newKey preserve the exact drain semantics: oldKey is the LIVE egress
	// at switch start (the posture loaded under the lock here, == the currentAP
	// snapshot's egress — a concurrent SetCapture* cannot change egress), newKey
	// is the new resolved egress. A concurrent toggle that re-Stored a posture
	// between the hoisted currentAP and here is read by old=sess.active.Load()
	// inside the lock, so its live half (and thus egress) is the one carried.
	if testHookBeforeSwitchPublish != nil {
		testHookBeforeSwitchPublish(sess)
	}
	newR := newResolved(pre) // expensive mapping, OUTSIDE the lock
	var oldKey, newKey string
	sess.mu.Lock()
	old := sess.active.Load()
	np := old.withResolved(newR) // new resolved ⊕ live carried forward (preserve is structural)
	sess.active.Store(&np)
	sess.public.UpdatedAt = time.Now()
	oldKey = egressTransportKey(old.r.egress)
	newKey = egressTransportKey(np.r.egress)
	np.deriveInto(&sess.public, sess.currentDialStateLocked())
	sess.mu.Unlock()
	if oldKey != newKey && sess.proxy != nil {
		sess.proxy.drainSupersededTransports(newKey)
	}

	// Emit one trace + one session_updated on the Switched outcome.
	// Detail is the JSON-encoded structured payload (from/from_provider/to/
	// to_provider/class). All five fields are NON-SECRET profile identity and
	// transition kind; we still funnel the marshaled string through
	// stripUserinfoString to defense-in-depth-strip any URL userinfo that might
	// have leaked in through a provider/name field (none of the inputs carry
	// URLs today, but the strip is cheap and keeps the trace contract aligned
	// with the rest of the trace pipeline in server.go). Marshal cannot
	// fail for map[string]string of literal strings, so the error is dropped.
	view := preflight.SafeProfileView(pre)
	toName := view.Name
	if toName == "" {
		toName = "inherit-env"
	}
	detailJSON, _ := json.Marshal(map[string]string{
		"from":          previousProfileName,
		"from_provider": previousProfileProvider,
		"to":            toName,
		"to_provider":   view.ProviderLabel,
		"class":         "live",
	})
	s.recordTrace(sess.public.ID, model.TraceRecord{
		Timestamp: time.Now().UTC(),
		SessionID: sess.public.ID,
		Category:  "profile_switch",
		Summary:   "switched",
		Detail:    stripUserinfoString(string(detailJSON)),
	})
	s.broadcast("session_updated", sess.public.ID, sess.snapshot())
	return SwitchOutcome{
		Result: SwitchResultSwitched,
		Class:  class,
		View:   view,
	}
}
