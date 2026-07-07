package supervisor

import (
	_ "embed"

	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

type pageBootstrap struct {
	EventsURL    string              `json:"events_url"`
	LastActivity string              `json:"last_activity,omitempty"`
	Session      *model.Session      `json:"session,omitempty"`
	Requests     []classifiedRecord  `json:"requests,omitempty"`
	Errors       []model.ErrorRecord `json:"errors,omitempty"`
	Trace        []model.TraceRecord `json:"trace,omitempty"`
	// HeaderDenyList is the single source of truth for credential
	// header redaction, emitted so the JS live-patch classifies
	// identically to the Go renderer — no drift.
	HeaderDenyList []string `json:"header_deny_list,omitempty"`
	ProfileToken   string   `json:"profile_token,omitempty"` // CSRF token
	// ClaudeSessionHeader is the request-header name the JS live-patch reads
	// to surface the Claude conversation id in the brandbar. Single-sourced
	// from claudeSessionHeader so client/server spellings cannot drift.
	ClaudeSessionHeader string `json:"claude_session_header,omitempty"`
}

type webPageData struct {
	Title        string
	Heading      string
	Subtitle     string
	SessionLabel string
	// FaviconHref is the data-URI favicon dot, colored by hero variant so
	// degraded/error/ended sessions read at tab level. Built ONLY by
	// faviconHref (fixed literal + hex from a fixed map — injection-safe);
	// renderWebPage defaults it to the active emerald when unset.
	FaviconHref template.URL
	// ClaudeSessionFull is the full Claude X-Claude-Code-Session-Id UUID for
	// the brandbar chip's title/aria-label/copy payload. Empty hides the chip.
	ClaudeSessionFull string
	HeroState         string  // "Active" / "Degraded" / "Error" / "Ended"
	HeroSentence      string  // ui.SessionPosture output
	HeroMeta          string  // "claude pid N · age" / "ended Xs ago · Y wall"
	HeroVariant       string  // active|degraded|ended — drives hero tint
	Ribbon            []webKV // exactly 4: Route, Auth, Models, Traffic
	Summary           []webKV // detailed fields, rendered in the config drawer
	Links             []webLink
	ActivityTitle     string
	ActivityEmpty     string
	ActivityRows      []webRow
	// Activity filter axis. Classes is the per-class button
	// data in fixed display order; DefaultClass is preselected.
	Classes             []webClassCount
	DefaultClass        string
	LiveEnabled         bool
	LastActivityLabel   string
	LastActivityRFC3339 string
	BootstrapB64        string
	// Profile cell variant inputs. HasProfilesFile is a single
	// os.Stat on profiles.DefaultPath at page-render time; ProfileCount is
	// len(file.Profiles) when the file loads cleanly, else 0.
	HasProfilesFile bool
	ProfileCount    int
}

type webKV struct {
	Label string
	Value string
	// chip rendering needs pre-escaped HTML (auto-escaped Value
	// can't carry a nested span structure). When ValueHTML is non-empty,
	// the template uses it instead of Value.
	ValueHTML template.HTML
	// DataState drives data-state="active|inherit-env-clickable|
	// inherit-env-static" on the cell wrapper. Empty for non-Profile cells.
	DataState string
	Detail    string // ribbon cell secondary mono line
	Href      string
	Mono      bool
}

type webLink struct {
	Label string
	Href  string
	Icon  string // optional sprite symbol id (e.g. "i-activity") shown in the overflow menu
}

type webClassCount struct {
	Class string // forwarded-api|synthetic|tunnel|error|trace|all
	Label string // "Forwarded API" etc.
	Count int
}

// webHeaderRow / webHeaderGroup are the web-local mirror of
// ui.HeaderRow / ui.HeaderGroup (internal/ui is strictly
// untouched). Redacted is set when the rendered Value is the
// redaction sentinel; it drives the red pill in the rendered output.
type webHeaderRow struct {
	Name, Value string
	Redacted    bool
}

type webHeaderGroup struct {
	Name string
	Rows []webHeaderRow
}

type webRow struct {
	Time      string
	Label     string
	Main      string
	Right     string
	Mono      bool
	Forwarded bool   // request that went to a real upstream → emerald rail
	Kind      string // "request"|"error"|"trace" for activity-feed coloring
	// HeaderGroups populated only for forwarded rows that captured
	// inbound headers. HeaderNote is the non-expandable
	// reason (synthetic / blind tunnel); empty when expandable.
	HeaderGroups []webHeaderGroup
	// HeaderSummary is the header chip summary line
	// (zero value here renders nothing).
	HeaderSummary template.HTML
	HeaderNote    string
	// BodyRefID is the hex id of a captured request body,
	// set request-only when rec.BodyRef != nil. It carries ONLY the id
	// — never the bytes; the body is fetched lazily from
	// /recent/body?id=<BodyRefID> by the inline mirror script.
	BodyRefID string
	// UpstreamBodyRefID points at the post-rewrite spilled body — what
	// the upstream actually receives after modelalias rewrite + system
	// block stripping. Empty when the request body was forwarded
	// unmodified (no model alias hit), in which case the inspector
	// drawer shows the single BodyRefID view.
	UpstreamBodyRefID string
	// ResponseBodyRefID is the hex id of a captured RESPONSE body
	// (rec.ResponseBodyRef.ID): on forwarded-api rows it renders the SSE-aware
	// resp-drawer, on telemetry rows the generic tele-drawer. Like BodyRefID it
	// carries ONLY the id; the bytes are fetched lazily from
	// /recent/body?id=<ResponseBodyRefID>.
	ResponseBodyRefID string
	// TelemetryDrawer marks a telemetry-class CONNECT row as EXPANDABLE
	// because it captured a request and/or response body. Such rows render
	// their own .row <details> with up to two .body-drawer.tele-drawer
	// sub-details (req/resp), rendered via the generic JSON view — distinct
	// from the forwarded-api header/Anthropic-body drawer.
	TelemetryDrawer bool
	// Class is the single-source classification:
	// forwarded-api|synthetic|tunnel|error|trace. The JS filter shows
	// rows whose Class ∈ the active filter; it never re-derives.
	Class string
	// StatusTier colors the DETAILS cell by HTTP status: "warn" (4xx) /
	// "err" (5xx) / "" (2xx/other). The status digits stay in the text as
	// the redundant carrier, so this is supplementary (WCAG 1.4.1). Request
	// rows only.
	StatusTier string
	// Category is the trace-row sub-type, currently only "profile_switch"; "" elsewhere.
	Category string
	// ProfileName/ProfileProvider are the per-row profile annotation. Empty when no active profile.
	ProfileName     string
	ProfileProvider string
	// switch-marker data-attrs (Category=="profile_switch" rows
	// only). For "switched"/"refused" outcomes use SwitchFrom/SwitchFromProvider
	// + SwitchTo/SwitchToProvider + SwitchClass. For "rejected" outcomes use
	// SwitchFrom/SwitchFromProvider + SwitchRequested + SwitchReason.
	SwitchFrom         string
	SwitchFromProvider string
	SwitchTo           string
	SwitchToProvider   string
	SwitchClass        string
	SwitchRequested    string
	SwitchReason       string
}

// web.tmpl is the entire dashboard: one self-contained html/template page.
// Its editing constraints ({{...}} parsed anywhere including JS comments,
// node-lifted script functions, JS/Go byte-equality, no innerHTML) are
// documented in its header comment.
//
//go:embed web.tmpl
var webTplSrc string

var webTpl = template.Must(template.New("page").Parse(webTplSrc))

func renderWebPage(w http.ResponseWriter, page webPageData) {
	if page.FaviconHref == "" {
		page.FaviconHref = faviconHref("active")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = webTpl.Execute(w, page)
}

func bootstrapB64(v pageBootstrap) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func webAuthBootstrap(sess model.Session) string {
	if sess.AuthBootstrap == "" {
		return string(model.AuthBootstrapNotNeeded)
	}
	if sess.AuthBootstrap == model.AuthBootstrapPlaceholderActive && sess.AuthBootstrapKind != "" && sess.AuthBootstrapKind != model.AuthBootstrapKindNone {
		return string(sess.AuthBootstrap) + ":" + string(sess.AuthBootstrapKind)
	}
	return string(sess.AuthBootstrap)
}

// webSummaryFromSession builds the Configuration details drawer rows:
// raw sources / fingerprints / full ID / PID for debug
// screenshots and tickets. The humanized State/Route/Auth/Models that
// used to live in the top stat grid now live in the hero + ribbon, so
// this is debug-detail only. Manifest path is not on model.Session
// (not plumbed through the control API); omitted here rather than
// fabricated.
func webSummaryFromSession(sess model.Session) []webKV {
	kv := []webKV{
		{Label: "Session ID", Value: sess.ID, Mono: true},
		{Label: "Supervisor PID", Value: fmt.Sprintf("%d", sess.SupervisorPID), Mono: true},
		{Label: "Upstream", Value: fallback(sess.ExactUpstreamBase, "<unconfigured>"), Mono: true},
		{Label: "Route source", Value: string(sess.RouteSource), Mono: true},
		{Label: "Route config source", Value: fallback(sess.RouteConfigSource, "none"), Mono: true},
		{Label: "Auth source", Value: string(sess.AuthSource), Mono: true},
		{Label: "Auth config source", Value: fallback(sess.AuthConfigSource, "none"), Mono: true},
		{Label: "Auth policy", Value: string(sess.AuthPolicy), Mono: true},
		{Label: "Bootstrap", Value: webAuthBootstrap(sess), Mono: true},
	}
	if sess.ModelAliasFingerprint != "" {
		kv = append(kv, webKV{Label: "Alias fingerprint", Value: sess.ModelAliasFingerprint, Mono: true})
	}
	if sess.UpstreamHeaderFingerprint != "" {
		kv = append(kv, webKV{Label: "Upstream-headers fingerprint", Value: sess.UpstreamHeaderFingerprint, Mono: true})
	}
	return kv
}

// webRibbonFromSession builds the 6-cell ribbon
// (Route / Auth / Models / Traffic / Profile / Bodies).
// hasProfilesFile + profileCount drive the Profile cell's three states
// (active / inherit-env-clickable / inherit-env-static). Profile name is
// html.EscapeString-applied; ValueHTML carries the chip markup. The
// Bodies cell renders the SessionRouteRequest.CaptureRequestBodies state
// reflected through sess.CaptureBodies — click → POST /capture/bodies
// hot-swaps the route via Supervisor.SetCaptureBodies; SSE session_updated
// patches the cell live.
func webRibbonFromSession(sess model.Session, lastActivityLabel string, hasProfilesFile bool, profileCount int) []webKV {
	// Models cell. The zero-alias state
	// renders "default" instead of empty/em-dash so the cell signals
	// "claude-code's own model names pass through unchanged" rather
	// than the ambiguous "no data". Non-zero state is clickable and
	// opens a small popover listing the alias forwards (handled in JS
	// via data-state="aliases-active").
	models := "default"
	modelsState := ""
	switch {
	case sess.ModelAliasCount == 1:
		models = "1 alias"
		modelsState = "aliases-active"
	case sess.ModelAliasCount > 1:
		models = fmt.Sprintf("%d aliases", sess.ModelAliasCount)
		modelsState = "aliases-active"
	}
	trafficLabel := "Traffic"
	if sess.State == model.StateEnded {
		trafficLabel = "Total"
	}
	chip := template.HTML(fmt.Sprintf(
		`<span class="sp3-chip%s"><span class="sp3-chip-dot"></span>%s <span class="sp3-chip-caret">▾</span></span>`,
		chipModeClass(sess, hasProfilesFile),
		html.EscapeString(profileNameOrInheritEnv(sess)),
	))
	bodiesValue, bodiesDetail, bodiesState := bodiesCellPresentation(sess.CaptureBodies, sess.CaptureBodiesUnmasked, sess.CaptureTelemetry)
	authValue, authDetail, authState := authCellPresentation(sess)
	egressValue, egressDetail, egressMono := egressCellPresentation(sess)
	cells := []webKV{
		{Label: "Route", Value: ui.HumanRouteClass(sess.RouteClass), Detail: strings.ToLower(ui.HumanRouteSource(sess.RouteSource))},
		{Label: "Auth", Value: authValue, Detail: authDetail, DataState: authState},
		{Label: "Models", Value: models, DataState: modelsState},
		{Label: trafficLabel, Value: fmt.Sprintf("%d · %d", sess.RecentRequestCount, sess.RecentErrorCount), Detail: lastActivityLabel, Mono: true},
		{Label: "Profile", ValueHTML: chip, Detail: profileDetail(sess, hasProfilesFile, profileCount), DataState: profileCellDataState(sess, hasProfilesFile)},
		{Label: "Bodies", Value: bodiesValue, Detail: bodiesDetail, DataState: bodiesState},
		{Label: "Egress", Value: egressValue, Detail: egressDetail, Mono: egressMono},
	}
	// NATIVE TLS cell — only when the feature is in use (native_tls non-empty).
	// Sessions not opted in carry "" and keep the historical 7-cell ribbon;
	// an opted-in session starts "active" so the cell is present from first
	// paint and updateNativeTLSCell() can patch active<->blocked live.
	if sess.NativeTLS != "" {
		ntValue, ntDetail, ntState := nativeTLSCellPresentation(sess.NativeTLS, sess.NativeTLSFallbacks, sess.NativeTLSLoaded)
		cells = append(cells, webKV{Label: "NATIVE TLS", Value: ntValue, Detail: ntDetail, DataState: ntState})
	}
	return cells
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
// Single source of truth so the server first paint and the JS
// live-patch updateNativeTLSCell() produce byte-equal output (no Go↔JS drift).
// The blocked detail is the bare reason (the "blocked: " prefix stripped).
func nativeTLSCellPresentation(nativeTLS string, blocks int, loaded bool) (value, detail, state string) {
	// Wording moved to ui.NativeTLSPresentation so the TUI summary line
	// shares it; this wrapper keeps the web-local name the byte-equality
	// tests and call sites pin.
	return ui.NativeTLSPresentation(nativeTLS, blocks, loaded)
}

// egressCellPresentation derives the (value, detail, mono) tuple for
// the posture ribbon's Egress cell. Surfaces what's currently invisible
// in the browser dashboard: the configured egress destination + source.
// The probe button + result line attach to this cell at runtime via the
// inline script's enhancePostureEgressCell() — no template changes
// needed for the live-probe feature.
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

// authCellPresentation derives the Auth ribbon cell's (value, detail, state)
// tuple. When AuthBootstrap==Missing the cell flips to a
// "auth-missing" danger state with a Case-A or Case-B detail message; in
// all other cases the historical Value=HumanAuthPolicy / Detail=
// HumanAuthBootstrap rendering is preserved verbatim (no behavior change
// for healthy sessions). The single Go helper is the truth source for
// both the server first paint (this function) and the JS live patch
// (updateAuthCell in the inline script) — keeps drift to zero.
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
// Single source of truth so the server-rendered first paint and the JS
// live-patch updateBodiesCell() produce byte-equal output (no Go↔JS drift).
//
// The cell now summarizes TWO independent capture toggles (request bodies +
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
	// Wording moved to ui.BodiesPresentation so the TUI capture line
	// shares it; this wrapper keeps the web-local name the byte-equality
	// tests and call sites pin.
	return ui.BodiesPresentation(captureBodies, captureBodiesUnmasked, captureTelemetry)
}

// profileNameOrInheritEnv returns the active profile name or the literal
// "inherit-env" when no profile is active.
func profileNameOrInheritEnv(sess model.Session) string {
	if sess.ActiveProfileName != "" {
		return sess.ActiveProfileName
	}
	return "inherit-env"
}

// chipModeClass returns the CSS class suffix for the chip. Active profile
// uses the bare `.sp3-chip` (accent); inherit-env splits into clickable vs
// static based on whether profiles.json exists.
func chipModeClass(sess model.Session, hasProfilesFile bool) string {
	if sess.ActiveProfileName != "" {
		return ""
	}
	if hasProfilesFile {
		return " sp3-chip-inherit sp3-chip-inherit-clickable"
	}
	return " sp3-chip-inherit sp3-chip-inherit-static"
}

// profileCellDataState returns the `data-state` attribute value that drives
// CSS rules + JS popover-eligibility decisions for the Profile cell.
func profileCellDataState(sess model.Session, hasProfilesFile bool) string {
	if sess.ActiveProfileName != "" {
		return "active"
	}
	if hasProfilesFile {
		return "inherit-env-clickable"
	}
	return "inherit-env-static"
}

// profileDetail returns the cell-detail line under the chip.
//
// Active mode:   "<provider> · <N> aliases"
// Inherit clickable: "<N> profiles available" (or "1 profile available")
// Inherit static:    "no profiles.json"
func profileDetail(sess model.Session, hasProfilesFile bool, profileCount int) string {
	if sess.ActiveProfileName != "" {
		provider := sess.ActiveProfileProvider
		switch sess.ModelAliasCount {
		case 0:
			return provider
		case 1:
			return provider + " · 1 alias"
		default:
			return fmt.Sprintf("%s · %d aliases", provider, sess.ModelAliasCount)
		}
	}
	if hasProfilesFile {
		switch profileCount {
		case 0:
			return "no profiles configured"
		case 1:
			return "1 profile available"
		default:
			return fmt.Sprintf("%d profiles available", profileCount)
		}
	}
	return "no profiles.json"
}

// faviconHref returns the data-URI favicon for a hero variant — the dot
// color mirrors the hero state so a degraded/error/ended session is visible
// at tab level before the page is even focused. template.URL is safe here:
// the only variable part is a hex literal from the fixed map below (the JS
// twin lives in updateHeroState; keep the two color maps in sync).
func faviconHref(variant string) template.URL {
	color := map[string]string{
		"active":   "10b981",
		"degraded": "fbbf24",
		"error":    "f43f5e",
		"ended":    "8f8f8f",
	}[variant]
	if color == "" {
		color = "10b981"
	}
	return template.URL("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Ccircle cx='8' cy='8' r='5' fill='%23" + color + "'/%3E%3C/svg%3E")
}

// webHeroVariant maps lifecycle + health to the hero state word + variant.
// Ended (lifecycle terminal) wins; otherwise the big-word is Health-driven:
// error→Error(rose), warn→Degraded(amber), ok/empty→Active(emerald).
func webHeroVariant(sess model.Session) (state, variant string) {
	if sess.State == model.StateEnded {
		return "Ended", "ended"
	}
	switch sess.Health {
	case model.HealthError:
		return "Error", "error"
	case model.HealthWarn:
		return "Degraded", "degraded"
	default: // ok or empty
		return "Active", "active"
	}
}

// lastWebError returns the newest error for the degraded hero "Last:"
// tail (slice is oldest-first, so the last element is newest).
func lastWebError(e []model.ErrorRecord) *model.ErrorRecord {
	if len(e) == 0 {
		return nil
	}
	return &e[len(e)-1]
}

// recordClass is THE single source of the request-class rule.
// Used by the unified builder (first paint) and by the request SSE
// serialization (Option X) so first-paint and live rows are classified
// identically — no Go↔JS drift. synthetic wins over CONNECT (a synthesized
// answer is never a real tunnel). Errors/trace are class "error"/"trace"
// assigned at their build sites (constant, not via this fn).
// defaultActivityClass picks the Activity tab shown on first paint:
// "forwarded-api" (the primary traffic) when there is any, else "all" — so a
// session whose only activity is trace/errors does NOT open on an empty
// "Forwarded API" tab and read as "No recent traffic" while other tabs have rows.
func defaultActivityClass(forwardedAPICount int) string {
	if forwardedAPICount == 0 {
		return "all"
	}
	return "forwarded-api"
}

func recordClass(rec model.RequestRecord) string {
	if rec.Synthetic {
		return "synthetic"
	}
	// Telemetry capture MITMs the allowlisted host and records the row with the
	// INNER request method (POST), not CONNECT — so classify by exact-host
	// FIRST, before the CONNECT check. Without this, a captured telemetry POST
	// falls through to "forwarded-api" and its request/response bodies render in
	// the wrong drawer (the telemetry generic-JSON drawer + filter chip key on
	// class == "telemetry"). This also classifies a blind-tunnel CONNECT to a
	// capture host (capture off) consistently, incl. anthropic.sentry.io, which
	// has no datadog suffix.
	if isTelemetryCaptureHost(rec.LogicalTargetHost) {
		return "telemetry"
	}
	if rec.Method == http.MethodConnect {
		// A blind-tunnel CONNECT (capture off) classifies as telemetry via the
		// broad suffix matcher (Datadog) for the inspect-web filter chip.
		if isTelemetryHost(rec.LogicalTargetHost) {
			return "telemetry"
		}
		return "tunnel"
	}
	return "forwarded-api"
}

// telemetryHostSuffixes is the verified set of non-Anthropic destinations
// that Claude Code reaches purely for telemetry/diagnostics. Split out of
// the generic "tunnel" class so the inspect-web filter bar can show them on
// their own chip — Datadog's 15s/per-100-events batching otherwise drowns
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

// classifiedRecord wraps a model.RequestRecord with its Go-derived class
// for the bootstrap + request SSE payload (Option X). model.RequestRecord
// is embedded so encoding/json flattens its fields and adds "class"; the
// model is NOT modified.
type classifiedRecord struct {
	Class string `json:"class"`
	model.RequestRecord
}

// headerAnnotation derives the expandability rule from a
// request record (single source for server first-paint AND the JS
// live mirror's equivalent). Expandable iff inbound headers were
// captured; otherwise an explicit non-expandable reason.
// headerAnnotation builds the request's header groups for inspect drawer
// rendering. unmask = sess.CaptureBodiesUnmasked — flips the credential-
// header redaction off when CCWRAP_UNMASK_CREDENTIALS=1 was set at launch.
// Default (unmask=false) is byte-identical to the masked behavior. The
// unmask state is launch-fixed and process-wide. Under the default,
// rec.RequestHeaders ALREADY carries masked credential values (masked
// store-side in recordRequest before the record reached the wire); this
// render-time redaction is defense in depth on top of that. Only under
// unmask does the record carry raw values, which this then renders.
func headerAnnotation(rec model.RequestRecord, unmask bool) (groups []ui.HeaderGroup, note string) {
	switch {
	case len(rec.RequestHeaders) > 0:
		return ui.RenderHeaderGroupsWithRedaction(rec.RequestHeaders, !unmask), ""
	case rec.Synthetic:
		return nil, "CCWRAP-generated, not Claude Code traffic"
	case rec.Method == http.MethodConnect:
		return nil, "encrypted tunnel — not intercepted; no headers visible"
	default:
		return nil, "no headers recorded"
	}
}

// toWebHeaderGroups maps the ui.HeaderGroup output of headerAnnotation
// onto the web-local webHeaderGroup (internal/ui is strictly
// untouched). It copies Name/Value verbatim and flags Redacted on the
// redaction sentinel so the renderer can show the red pill.
func toWebHeaderGroups(groups []ui.HeaderGroup) []webHeaderGroup {
	if groups == nil {
		return nil
	}
	out := make([]webHeaderGroup, len(groups))
	for i, g := range groups {
		wr := make([]webHeaderRow, len(g.Rows))
		for j, r := range g.Rows {
			wr[j] = webHeaderRow{Name: r.Name, Value: r.Value, Redacted: r.Value == "‹redacted by ccwrap›"}
		}
		out[i] = webHeaderGroup{Name: g.Name, Rows: wr}
	}
	return out
}

// filterActivityClass keeps only the records whose activity class matches
// class ("" or "all" keeps everything). Applied BEFORE unifiedActivityRows'
// newest-N cap so the first paint is the newest-N window OF THE CLASS —
// the server twin of the JS rebuild's filter-aware capping (a /v1/messages
// row is never buried under noise rows). Errors and trace are constant-
// classed, so they survive only their own filter.
func filterActivityClass(class string, requests []model.RequestRecord, errors []model.ErrorRecord, trace []model.TraceRecord) ([]model.RequestRecord, []model.ErrorRecord, []model.TraceRecord) {
	if class == "" || class == "all" {
		return requests, errors, trace
	}
	var reqs []model.RequestRecord
	for _, rec := range requests {
		if recordClass(rec) == class {
			reqs = append(reqs, rec)
		}
	}
	if class != "error" {
		errors = nil
	}
	if class != "trace" {
		trace = nil
	}
	return reqs, errors, trace
}

// unifiedActivityRows is the single source of Activity rows.
// It supersedes the four per-section row builders that were collapsed
// into it. Every row gets a Class; drawer wiring (HeaderGroups/
// HeaderNote/BodyRefID) is set ONLY on forwarded-api rows. The
// sort/tail/return idiom is newest-first then capped to n, the same
// ordering those builders produced.
// unifiedActivityRows builds Activity rows for first-paint render. unmask
// flows from sess.CaptureBodiesUnmasked (CCWRAP_UNMASK_CREDENTIALS=1 launch
// flag) through headerAnnotation so credential headers render raw when
// the user has opted in. Default (unmask=false) preserves the long-standing
// header redaction.
func unifiedActivityRows(requests []model.RequestRecord, errors []model.ErrorRecord, trace []model.TraceRecord, n int, includeSession, unmask bool) []webRow {
	type timedRow struct {
		when time.Time
		row  webRow
	}
	rows := make([]timedRow, 0, len(requests)+len(errors)+len(trace))
	for _, rec := range requests {
		cls := recordClass(rec)
		row := webRow{
			Time:       webClock(rec.Timestamp),
			Label:      cls,
			Main:       fmt.Sprintf("%s %s", rec.MethodLabel(), fallback(rec.Path, "/")),
			Right:      formatRequestDetail(rec, includeSession),
			Mono:       true,
			Forwarded:  cls == "forwarded-api",
			Kind:       "request",
			Class:      cls,
			StatusTier: statusTier(rec.StatusCode),
			// per-row profile annotation (omitempty in template).
			ProfileName:     rec.ActiveProfileName,
			ProfileProvider: rec.ActiveProfileProvider,
		}
		// EVERY request row carries its header
		// annotation note — synthetic/tunnel rows get the explicit
		// non-expandable reason; forwarded-api rows get "" (the drawer,
		// not a reason). The expandable drawer + body ref stay
		// forwarded-api-only: non-forwarded API
		// rows have neither drawer.
		groups, note := headerAnnotation(rec, unmask)
		row.HeaderNote = note
		if cls == "forwarded-api" {
			// Map ui.HeaderGroup → webHeaderGroup (internal/ui
			// untouched). Copy Name/Value; flag Redacted on the
			// redaction sentinel for the red pill. HeaderSummary is the
			// chip line: byte-equivalent to the live JS
			// headerPanelEl summary (headers→groups→redacted; only the
			// 3rd span has class="red"). Built ONLY from the fixed
			// format-string literal + three %d integers — no header
			// name/value or any user string — so template.HTML is
			// injection-safe. The credential value is never emitted: the
			// sentinel ‹redacted by ccwrap› was set upstream by
			// toWebHeaderGroups/headerAnnotation/ui.
			hg := toWebHeaderGroups(groups)
			var hsN, hsR int
			for _, g := range hg {
				hsN += len(g.Rows)
				for _, r := range g.Rows {
					if r.Redacted {
						hsR++
					}
				}
			}
			row.HeaderGroups = hg
			if len(hg) > 0 {
				row.HeaderSummary = template.HTML(fmt.Sprintf(
					`<div class="hdr-sum"><span>headers <b>%d</b></span><span>groups <b>%d</b></span><span class="red">redacted <b>%d</b></span></div>`,
					hsN, len(hg), hsR))
			}
			if rec.BodyRef != nil {
				row.BodyRefID = rec.BodyRef.ID
			}
			if rec.UpstreamBodyRef != nil {
				row.UpstreamBodyRefID = rec.UpstreamBodyRef.ID
			}
			if rec.ResponseBodyRef != nil {
				row.ResponseBodyRefID = rec.ResponseBodyRef.ID
			}
		}
		// Telemetry-MITM CONNECTs (allowlisted Datadog hosts) capture the
		// request + response bodies even though there are no headers. A
		// telemetry row with EITHER body becomes expandable via the generic
		// JSON drawer (separate from the forwarded-api header/body drawer).
		if cls == "telemetry" {
			if rec.BodyRef != nil {
				row.BodyRefID = rec.BodyRef.ID
			}
			if rec.ResponseBodyRef != nil {
				row.ResponseBodyRefID = rec.ResponseBodyRef.ID
			}
			row.TelemetryDrawer = rec.BodyRef != nil || rec.ResponseBodyRef != nil
		}
		rows = append(rows, timedRow{when: rec.Timestamp, row: row})
	}
	for _, rec := range errors {
		// The suggested action must always surface
		// — never hidden behind a present UpstreamHost. joinParts skips
		// empties, so class/severity context is preserved AND the action
		// surfaces (the suggested-action doctrine of this builder).
		right := joinParts(fallback(rec.ErrorClass, rec.Severity), rec.UpstreamHost, rec.SuggestedAction)
		if includeSession && rec.SessionID != "" {
			right = joinParts(right, "sess "+shortSessionID(rec.SessionID))
		}
		rows = append(rows, timedRow{when: rec.Timestamp, row: webRow{
			Time:  webClock(rec.Timestamp),
			Label: "error",
			Main:  fallback(rec.Summary, "proxy error"),
			Right: fallback(right, "check ccwrap doctor"),
			Kind:  "error",
			Class: "error",
		}})
	}
	for _, rec := range trace {
		right := joinParts(fallback(rec.Category, "trace"), fallback(rec.Detail, "—"))
		if includeSession && rec.SessionID != "" {
			right = joinParts(right, "sess "+shortSessionID(rec.SessionID))
		}
		row := webRow{
			Time:     webClock(rec.Timestamp),
			Label:    "trace",
			Main:     fallback(rec.Summary, "activity"),
			Right:    right,
			Kind:     "trace",
			Class:    "trace",
			Category: rec.Category, // propagate sub-type for CSS hook + JS structured render
		}
		// Narrow JSON.Unmarshal into typed struct when this is
		// a profile_switch trace. On parse failure (defensive — we control
		// the producer in switch.go), the row falls through with empty
		// switch fields and renders the raw Detail string in trace-purple.
		if rec.Category == "profile_switch" {
			var d struct {
				From         string `json:"from"`
				FromProvider string `json:"from_provider"`
				To           string `json:"to"`
				ToProvider   string `json:"to_provider"`
				Class        string `json:"class"`
				Requested    string `json:"requested"`
				Reason       string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(rec.Detail), &d); err == nil {
				row.SwitchFrom = d.From
				row.SwitchFromProvider = d.FromProvider
				row.SwitchTo = d.To
				row.SwitchToProvider = d.ToProvider
				row.SwitchClass = d.Class
				row.SwitchRequested = d.Requested
				row.SwitchReason = d.Reason
				// Human-readable Right for the no-JS first paint, single-
				// sourced from ui.HumanProfileSwitch (shared with the TUI
				// trace rows; the live decorator renderSwitchMarker repeats
				// the same wording when it rebuilds from the data-attrs).
				if h := ui.HumanProfileSwitch(rec.Detail); h != "" {
					row.Right = h
				}
			}
		}
		rows = append(rows, timedRow{when: rec.Timestamp, row: row})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].when.After(rows[j].when) })
	if len(rows) > n {
		rows = rows[:n]
	}
	out := make([]webRow, len(rows))
	for i, item := range rows {
		out[i] = item.row
	}
	return out
}

func latestActivityTimestamp(requests []model.RequestRecord, errors []model.ErrorRecord, trace []model.TraceRecord) time.Time {
	latest := time.Time{}
	for _, rec := range requests {
		if rec.Timestamp.After(latest) {
			latest = rec.Timestamp
		}
	}
	for _, rec := range errors {
		if rec.Timestamp.After(latest) {
			latest = rec.Timestamp
		}
	}
	for _, rec := range trace {
		if rec.Timestamp.After(latest) {
			latest = rec.Timestamp
		}
	}
	return latest
}

func activityAgeLabel(ts time.Time) string {
	if ts.IsZero() {
		return "idle"
	}
	age := time.Since(ts)
	switch {
	case age < 5*time.Second:
		return "just now"
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

// uptimeLabel formats a duration since ts as "Xs/Xm/Xh/Xd" without
// the "ago" suffix — for "up 5m" style display in the hero meta line.
// Zero time → empty string (caller decides whether to render).
func uptimeLabel(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	age := time.Since(ts)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
}

func activityRFC3339(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Format(time.RFC3339Nano)
}

func webClock(ts time.Time) string {
	if ts.IsZero() {
		return "—"
	}
	return ts.Local().Format("15:04:05")
}

func formatRequestDetail(rec model.RequestRecord, includeSession bool) string {
	right := joinParts(
		fmt.Sprintf("%d", rec.StatusCode),
		fmt.Sprintf("%d ms", rec.LatencyMS),
		string(rec.StreamState),
		fallback(rec.ActualUpstreamHost, rec.LogicalTargetHost),
	)
	if includeSession && rec.SessionID != "" {
		right = joinParts(right, "sess "+shortSessionID(rec.SessionID))
	}
	return right
}

// statusTier maps an HTTP status code to a DETAILS-cell color tier:
// "err" for 5xx, "warn" for 4xx, "" otherwise (2xx and non-status rows stay
// the default muted color). Kept in sync with the JS statusTier in the inline
// dashboard script.
func statusTier(code int) string {
	switch {
	case code >= 500 && code < 600:
		return "err"
	case code >= 400 && code < 500:
		return "warn"
	default:
		return ""
	}
}

func joinParts(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, " · ")
}

func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// webActivityEmptyLive is the live-session empty-Activity message — states
// the fact AND what will happen, instead of a bare "nothing here". The JS
// rebuild paints the SAME string client-side (rebuildActivityFromState's
// literal); a P3 contract test keeps the two byte-equal. Ended pages get a
// past-tense variant set in handleInfoPage (nothing is "appearing" there).
const webActivityEmptyLive = "No recent traffic for this session yet — requests appear here live as Claude Code sends them."

// claudeSessionHeader / latestClaudeSessionID moved to internal/ui
// (ClaudeSessionHeader / LatestClaudeSessionID) so the TUI header can share
// them; these aliases keep the web-local names the bootstrap plumbing and
// tests pin. The JS live-patch still reads the spelling from the bootstrap,
// so client/server cannot drift.
const claudeSessionHeader = ui.ClaudeSessionHeader

func latestClaudeSessionID(records []model.RequestRecord) string {
	return ui.LatestClaudeSessionID(records)
}

func proxyInfoURL(addr string) string {
	if addr == "" {
		return ""
	}
	return "http://" + addr + "/"
}

func fallback(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func tailRequestsForWeb(in []model.RequestRecord, n int) []model.RequestRecord {
	if len(in) == 0 {
		return []model.RequestRecord{}
	}
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailErrorsForWeb(in []model.ErrorRecord, n int) []model.ErrorRecord {
	if len(in) == 0 {
		return []model.ErrorRecord{}
	}
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailTraceForWeb(in []model.TraceRecord, n int) []model.TraceRecord {
	if len(in) == 0 {
		return []model.TraceRecord{}
	}
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}
