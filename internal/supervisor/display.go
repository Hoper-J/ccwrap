package supervisor

import (
	_ "embed"

	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// trafficState marks the Traffic cell when model requests failed, so the
// "N failed" line stands out.
func trafficState(failed int) string {
	if failed > 0 {
		return "failed"
	}
	return ""
}

// authBootstrapLabel is the auth bootstrap as the configuration shows it:
// the state, with the placeholder's kind when one is injected.
func authBootstrapLabel(sess model.Session) string {
	if sess.AuthBootstrap == "" {
		return string(model.AuthBootstrapNotNeeded)
	}
	if sess.AuthBootstrap == model.AuthBootstrapPlaceholderActive && sess.AuthBootstrapKind != "" && sess.AuthBootstrapKind != model.AuthBootstrapKindNone {
		return string(sess.AuthBootstrap) + ":" + string(sess.AuthBootstrapKind)
	}
	return string(sess.AuthBootstrap)
}

// configRows are the Configuration details: raw sources, fingerprints,
// the full ID and PID, for debug screenshots and tickets. The humanized
// route, auth and models live in the posture sentence and the ribbon.
func configRows(sess model.Session) []displayKV {
	kv := []displayKV{
		{Label: "Session ID", Value: sess.ID},
		{Label: "Supervisor PID", Value: fmt.Sprintf("%d", sess.SupervisorPID)},
		{Label: "Upstream", Value: fallback(sess.ExactUpstreamBase, "<unconfigured>")},
		{Label: "Route source", Value: string(sess.RouteSource)},
		{Label: "Route config source", Value: fallback(sess.RouteConfigSource, "none")},
		{Label: "Auth source", Value: string(sess.AuthSource)},
		{Label: "Auth config source", Value: fallback(sess.AuthConfigSource, "none")},
		{Label: "Auth policy", Value: string(sess.AuthPolicy)},
		{Label: "Bootstrap", Value: authBootstrapLabel(sess)},
	}
	if sess.ModelAliasFingerprint != "" {
		kv = append(kv, displayKV{Label: "Alias fingerprint", Value: sess.ModelAliasFingerprint})
	}
	if sess.UpstreamHeaderFingerprint != "" {
		kv = append(kv, displayKV{Label: "Upstream-headers fingerprint", Value: sess.UpstreamHeaderFingerprint})
	}
	return kv
}

// nativeTLSCellPresentation maps the session native_tls field (+ block-episode
// count) to a ribbon cell. off -> not opted in; active -> mirroring CC's
// fingerprint; blocked -> the mirror failed and the request was blocked
// fail-closed (an error condition — no degraded fingerprint is ever sent). When
// active AND blocks>0, the detail records the prior block episodes so a healed
// transient block leaves a visible trace (the count would otherwise vanish on
// recovery). When active with no prior blocks AND loaded (the mirrored hello
// came from CCWRAP_NATIVE_TLS_HELLO, not captured from Claude Code), the detail
// says "loaded" instead of falsely claiming the Claude Code fingerprint.
// The blocked detail is the bare reason (the "blocked: " prefix stripped).
// The wording lives in ui.NativeTLSPresentation, shared with the TUI.
func nativeTLSCellPresentation(nativeTLS string, blocks int, loaded bool) (value, detail, state string) {
	return ui.NativeTLSPresentation(nativeTLS, blocks, loaded)
}

// egressCellPresentation derives the (value, detail, mono) tuple for
// the ribbon's Egress cell: the configured egress destination + source.
//
// Values:
//
//	"Direct"                 — no proxy / mode unset
//	"http://proxy:8080"      — proxy URL (mono-styled for terminal feel)
//	"socks5h"                — fallback when mode is set but URL is empty
//
// Detail surfaces where the egress came from (claude settings / env /
// explicit flag) so a user with a surprising "Direct" can trace why.
func egressCellPresentation(sess model.Session) (value, detail string, mono bool) {
	mode := strings.ToLower(strings.TrimSpace(sess.EgressMode))
	switch mode {
	case "", "direct", "none":
		value = "Direct"
	default:
		if s := strings.TrimSpace(sess.EgressSummary); s != "" {
			value = s
			mono = true
		} else {
			value = mode
		}
	}
	switch strings.ToLower(strings.TrimSpace(sess.EgressSource)) {
	case "inherited_env":
		detail = "from environment"
	case "claude_settings":
		detail = "claude settings"
	case "explicit_flag":
		detail = "explicit"
	}
	return value, detail, mono
}

// routeCellPresentation is the Route ribbon cell's value and detail.
func routeCellPresentation(sess model.Session) (value, detail string) {
	return ui.HumanRouteClass(sess.RouteClass), strings.ToLower(ui.HumanRouteSource(sess.RouteSource))
}

// sessionForWire is a session as API clients get it (GET /api/v1/session
// and the session.updated event): the model plus its words, so every
// client shows the session the same way.
type sessionForWire struct {
	model.Session
	Display sessionDisplay `json:"display"`
}

type sessionDisplay struct {
	// Posture and PostureNote are the posture sentence in two parts: the
	// route sentence and the "— N issues recorded since launch." note.
	Posture     string `json:"posture"`
	PostureNote string `json:"posture_note,omitempty"`
	// Badge sums up the open issues for a session list (issueBadge).
	Badge string `json:"badge,omitempty"`
	// Cells are the ribbon, in order; Config the configuration details.
	Cells  []displayCell `json:"cells"`
	Config []displayKV   `json:"config"`
}

func sessionWithDisplay(sess model.Session) sessionForWire {
	w := sessionForWire{Session: sess}
	w.Display.Posture, w.Display.PostureNote = ui.SessionPostureParts(&sess)
	w.Display.Badge = issueBadge(sess)
	w.Display.Cells = displayCells(sess)
	w.Display.Config = configRows(sess)
	return w
}

// displayCell is one ribbon cell as an API client gets it. Key names the
// cell (route, auth, models, traffic, profile, bodies, egress, native_tls);
// State is the cell's data-state (amber/red variants, "" for plain).
type displayCell struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
	State  string `json:"state,omitempty"`
	Mono   bool   `json:"mono,omitempty"`
}

type displayKV struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// displayCells is the session's ribbon as data: each cell's label, value,
// detail and state, in order.
func displayCells(sess model.Session) []displayCell {
	routeValue, routeDetail := routeCellPresentation(sess)
	authValue, authDetail, authState := authCellPresentation(sess)
	modelsValue, modelsDetail, modelsState := modelsCellPresentation(sess)
	bodiesValue, bodiesDetail, bodiesState := bodiesCellPresentation(sess.CaptureBodies, sess.CaptureBodiesUnmasked, sess.CaptureTelemetry)
	egressValue, egressDetail, egressMono := egressCellPresentation(sess)
	trafficLabel := "Traffic"
	if sess.State == model.StateEnded {
		trafficLabel = "Total"
	}
	cells := []displayCell{
		{Key: "route", Label: "Route", Value: routeValue, Detail: routeDetail},
		{Key: "auth", Label: "Auth", Value: authValue, Detail: authDetail, State: authState},
		{Key: "models", Label: "Models", Value: modelsValue, Detail: modelsDetail, State: modelsState},
		{Key: "traffic", Label: trafficLabel, Value: fmt.Sprintf("%d req", sess.RecentRequestCount), Detail: fmt.Sprintf("%d failed", sess.RecentFailedCount), State: trafficState(sess.RecentFailedCount), Mono: true},
		profileCell(sess),
		{Key: "bodies", Label: "Bodies", Value: bodiesValue, Detail: bodiesDetail, State: bodiesState},
		{Key: "egress", Label: "Egress", Value: egressValue, Detail: egressDetail, Mono: egressMono},
	}
	if sess.NativeTLS != "" {
		v, d, st := nativeTLSCellPresentation(sess.NativeTLS, sess.NativeTLSFallbacks, sess.NativeTLSLoaded)
		cells = append(cells, displayCell{Key: "native_tls", Label: "NATIVE TLS", Value: v, Detail: d, State: st})
	}
	return cells
}

// profileCell is the Profile cell: the active profile with its provider and
// alias count, or inherit-env (no profile).
func profileCell(sess model.Session) displayCell {
	if sess.ActiveProfileName == "" {
		return displayCell{Key: "profile", Label: "Profile", Value: "inherit-env", Detail: "no profile", State: "inherit-env-static"}
	}
	detail := sess.ActiveProfileProvider
	switch n := sess.ModelAliasCount; {
	case n == 1:
		detail += " · 1 alias"
	case n > 1:
		detail += fmt.Sprintf(" · %d aliases", n)
	}
	return displayCell{Key: "profile", Label: "Profile", Value: sess.ActiveProfileName, Detail: detail, State: "active"}
}

// modelsCellPresentation is the Models ribbon cell. No aliases reads
// "default" (Claude Code's names pass through) rather than an ambiguous
// blank. With aliases the cell opens a popover listing them; when the
// session sent Claude models with no alias, it turns amber and the popover
// leads with the fix.
func modelsCellPresentation(sess model.Session) (value, detail, state string) {
	switch {
	case sess.ModelAliasCount == 1:
		value, state = "1 alias", "aliases-active"
	case sess.ModelAliasCount > 1:
		value, state = fmt.Sprintf("%d aliases", sess.ModelAliasCount), "aliases-active"
	default:
		return "default", "", ""
	}
	switch n := len(sess.UnmappedModels); {
	case n == 1:
		detail, state = "1 unaliased model sent", "aliases-warn"
	case n > 1:
		detail, state = fmt.Sprintf("%d unaliased models sent", n), "aliases-warn"
	}
	return value, detail, state
}

// authCellPresentation derives the Auth ribbon cell's (value, detail, state)
// tuple. When AuthBootstrap==Missing the cell flips to a
// "auth-missing" danger state with a Case-A or Case-B detail message; in
// all other cases the historical Value=HumanAuthPolicy / Detail=
// HumanAuthBootstrap rendering is preserved verbatim (no behavior change
// for healthy sessions).
//
// State machine:
//
//	""              → healthy session (default; data-state attribute absent
//	                  so existing CSS rules apply unchanged)
//	"auth-missing"  → AuthBootstrap == Missing; cell renders red
func authCellPresentation(sess model.Session) (value, detail, state string) {
	if sess.AuthBootstrap == model.AuthBootstrapMissing {
		profile := sess.ActiveProfileName
		if strings.TrimSpace(profile) == "" {
			profile = "inherit-env"
		}
		if sess.MissingAuthEnv != "" {
			return "⚠ MISSING", fmt.Sprintf("profile %q needs $%s", profile, sess.MissingAuthEnv), "auth-missing"
		}
		return "⚠ MISSING", fmt.Sprintf("profile %q has no auth source configured", profile), "auth-missing"
	}
	return ui.HumanAuthPolicy(sess.AuthPolicy), ui.HumanAuthBootstrap(sess.AuthBootstrap, sess.AuthBootstrapKind), ""
}

// bodiesCellPresentation derives the Bodies ribbon cell's projection values
// from the captureBodies + captureBodiesUnmasked + captureTelemetry flags.
//
// The cell summarizes TWO independent capture toggles (request bodies +
// telemetry bodies, surfaced via the click-to-open popover). The value is the
// " + " join of whichever toggles are on; the detail/state describe the combo.
//
// State machine:
//
//	off                 — nothing captured (default)
//	bodies-on           — request and/or telemetry capture on, credentials redacted
//	bodies-unmasked     — request capture on + CCWRAP_UNMASK_CREDENTIALS=1 in env;
//	                      OAuth refresh_token etc. appear in plaintext in the
//	                      inspect drawer. Persistent danger-color marker (⚠) so a
//	                      forgotten env stays visible. (Telemetry is never unmasked.)
//
// The unmasked state only applies when request capture is also on — without
// it there is nothing to redact and the env flag is a no-op.
func bodiesCellPresentation(captureBodies, captureBodiesUnmasked, captureTelemetry bool) (value, detail, state string) {
	// The wording lives in ui.BodiesPresentation, shared with the TUI.
	return ui.BodiesPresentation(captureBodies, captureBodiesUnmasked, captureTelemetry)
}

func recordClass(rec model.RequestRecord) string {
	if rec.Synthetic {
		return "synthetic"
	}
	// Telemetry capture MITMs the allowlisted host and records the row with the
	// INNER request method (POST), not CONNECT — so classify by exact-host
	// FIRST, before the CONNECT check. Without this, a captured telemetry POST
	// falls through to "forwarded-api" and its request/response bodies render in
	// the wrong drawer (the telemetry JSON drawer and the Activity filter key on
	// class == "telemetry"). This also classifies a blind-tunnel CONNECT to a
	// capture host (capture off) consistently, incl. anthropic.sentry.io, which
	// has no datadog suffix.
	if isTelemetryCaptureHost(rec.LogicalTargetHost) {
		return "telemetry"
	}
	if rec.Method == http.MethodConnect {
		// A blind-tunnel CONNECT (capture off) classifies as telemetry via the
		// broad suffix matcher (Datadog) for the Activity filter.
		if isTelemetryHost(rec.LogicalTargetHost) {
			return "telemetry"
		}
		return "tunnel"
	}
	return "forwarded-api"
}

// telemetryHostSuffixes is the verified set of non-Anthropic destinations
// that Claude Code reaches purely for telemetry/diagnostics. Split out of
// the generic "tunnel" class so the Activity filter can show them on
// their own tab — Datadog's 15s/per-100-events batching otherwise drowns
// out genuine blind tunnels (GitHub OAuth, MCP server CONNECTs).
//
// Source: claude-code/src/services/analytics/datadog.ts (Datadog logs
// ingestion). The design audit also investigated Statsig and
// ruled it out — no @statsig SDK is bundled in Claude Code 2.1.x; only
// console.statsig.com appears as a doc URL.
//
// Suffix match anchored with a leading dot so substring lookalikes such
// as "not-datadoghq.com.example.org" do NOT classify as telemetry.
var telemetryHostSuffixes = []string{
	".datadoghq.com",
	".datadoghq.eu",
}

// isTelemetryHost reports whether host is a known Claude Code telemetry
// destination. Case-insensitive suffix match against telemetryHostSuffixes.
func isTelemetryHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, suf := range telemetryHostSuffixes {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// classifiedRecord wraps a model.RequestRecord with its class for the
// activity API and the request.added event. model.RequestRecord is embedded
// so encoding/json flattens its fields and adds "class"; the model is NOT
// modified.
type classifiedRecord struct {
	Class string `json:"class"`
	model.RequestRecord
}

func fallback(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func tailRequests(in []model.RequestRecord, n int) []model.RequestRecord {
	if len(in) == 0 {
		return []model.RequestRecord{}
	}
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailErrors(in []model.ErrorRecord, n int) []model.ErrorRecord {
	if len(in) == 0 {
		return []model.ErrorRecord{}
	}
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailTrace(in []model.TraceRecord, n int) []model.TraceRecord {
	if len(in) == 0 {
		return []model.TraceRecord{}
	}
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}
