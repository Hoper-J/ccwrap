package supervisor

import (
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

var webTpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="{{.FaviconHref}}">
<title>{{.Title}}</title>
<style>
:root{--bg:#0a0a0a;--surface:#101010;--surface-2:#171717;--line:#1b1b1b;--border:#242424;--muted:#8f8f8f;--text-muted:#8f8f8f;--text-secondary:#b4b4b4;--text:#fafafa;--accent:#10b981;--accent-grad:#059669;--accent-soft:rgba(16,185,129,.12);--warn:#fbbf24;--danger:#f43f5e;--trace:#a78bfa;--info:#38bdf8}
*{box-sizing:border-box}html,body{margin:0;padding:0}body{background:var(--bg);color:var(--text);font:14px/1.5 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;font-variant-numeric:tabular-nums}a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}button{font:inherit}code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.wrap{max-width:1280px;margin:0 auto;padding:24px 20px 36px}.topbar{display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:12px 16px;margin-bottom:14px}.topbar-side{display:flex;flex-direction:column;align-items:flex-end;gap:8px}.actions{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}.brandbar{display:flex;align-items:center;gap:11px}.brandmark{width:22px;height:22px;border-radius:7px;background:var(--accent);display:flex;align-items:center;justify-content:center;box-shadow:0 0 0 1px rgba(16,185,129,.35),0 0 14px rgba(16,185,129,.4)}.brandmark i{width:8px;height:8px;border-radius:50%;background:#06140f}.wordmark{font-size:13px;font-weight:700;letter-spacing:.2em}.vr{width:1px;height:20px;background:var(--border)}.sesslabel{font-size:12px;color:var(--text-muted);letter-spacing:.02em}.sesslabel[data-full]{cursor:pointer}.btn-icon{padding:0;width:32px;height:32px;justify-content:center}.btn-icon svg{display:block;width:15px;height:15px;flex:none}.ovf-wrap{position:relative}.ovf-menu{position:absolute;top:calc(100% + 8px);right:0;min-width:180px;background:var(--surface);border:1px solid var(--border);border-radius:9px;box-shadow:0 12px 30px rgba(0,0,0,.6);padding:5px;z-index:50;display:flex;flex-direction:column}.ovf-menu[hidden]{display:none}.ovf-item{display:flex;align-items:center;gap:9px;padding:8px 10px;border-radius:6px;color:var(--text-secondary);font-size:13px}.ovf-item svg{width:14px;height:14px;flex:none;color:var(--text-muted)}.ovf-item:hover{background:var(--surface-2);color:var(--text);text-decoration:none}.visually-hidden{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0}.btn{appearance:none;background:var(--surface);border:1px solid var(--border);color:var(--text);padding:8px 12px;border-radius:8px;display:inline-flex;align-items:center;gap:8px;cursor:pointer}.btn:hover{background:var(--surface-2)}.btn:focus-visible,.sesslabel[data-full]:focus-visible,.filter-btn:focus-visible,.show-more button:focus-visible,.sp3-pop-row:focus-visible,.pop-test-btn:focus-visible,.pop-test-all-btn:focus-visible,.pop-action:focus-visible,.body-view-btn:focus-visible,.sp3-pop-edit-actions button:focus-visible,.pop-keysrc-sel:focus-visible,.egress-probe-btn:focus-visible,.egress-test-btn:focus-visible{outline:2px solid var(--accent);outline-offset:2px}.sp3-pop-row:focus-visible{outline-offset:-2px}
/* state-pill kept for the SSE live-chip (now lives inside hero-head as
   .hero-stream-pill, post-2026-05-26 — first-row stream-strip removed
   per user feedback that UPDATES/LAST ACTIVITY duplicated downstream UI). */
.state-pill{display:inline-flex;align-items:center;gap:8px;padding:6px 10px;border-radius:999px;border:1px solid var(--border);background:var(--surface-2);font-weight:700;text-transform:lowercase;width:max-content}.state-pill[data-state="connected"]{color:var(--accent)}.state-pill[data-state="connecting"],.state-pill[data-state="reconnecting"]{color:var(--warn)}.state-pill[data-state="paused"]{color:var(--warn)}.state-pill[data-state="error"],.state-pill[data-state="disconnected"]{color:var(--danger)}.state-pill[data-state="ended"]{color:var(--text-muted)}.state-dot{width:8px;height:8px;border-radius:50%;background:currentColor;box-shadow:0 0 12px currentColor}@media (prefers-reduced-motion:reduce){.state-dot{box-shadow:none}svg.spin{animation:none}}
.summary-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:10px;margin:0 0 16px}.stat{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:12px 14px;min-height:78px}.stat .k{font-size:11px;letter-spacing:.08em;text-transform:uppercase;color:var(--text-muted);margin-bottom:8px}.stat .v{font-size:14px;font-weight:600;word-break:break-word}
.hero{border:0;border-left:2px solid var(--accent);border-radius:0;padding:2px 0 2px 16px;margin:0 0 22px;background:none}.hero-kicker{font-size:10px;letter-spacing:.18em;text-transform:uppercase;color:var(--text-muted);font-weight:700;margin-bottom:10px}.hero[data-variant="active"]{border-left-color:var(--accent)}.hero[data-variant="degraded"]{border-left-color:var(--warn)}.hero[data-variant="error"]{border-left-color:var(--danger)}.hero[data-variant="ended"]{border-left-color:var(--text-muted)}.hero-head{display:flex;align-items:center;gap:10px;margin-bottom:12px}.hero-state{font-size:32px;font-weight:750;letter-spacing:-.03em;line-height:1}.hero[data-variant="active"] .hero-state{color:var(--accent)}.hero[data-variant="degraded"] .hero-state{color:var(--warn)}.hero[data-variant="error"] .hero-state{color:var(--danger)}.hero[data-variant="ended"] .hero-state{color:var(--text-muted)}.hero-dot{width:9px;height:9px;border-radius:50%;background:var(--accent);box-shadow:0 0 12px var(--accent)}.hero[data-variant="degraded"] .hero-dot{background:var(--warn);box-shadow:0 0 12px var(--warn)}.hero[data-variant="error"] .hero-dot{background:var(--danger);box-shadow:0 0 12px var(--danger)}.hero[data-variant="ended"] .hero-dot{background:var(--text-muted);box-shadow:none}.hero-meta{color:var(--text-muted);font-size:13px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.app-stream-pill{font-size:11px;padding:4px 10px;margin-right:2px}
.hero-body{color:var(--text-secondary);font-size:14px;line-height:1.55}.hero-sub{color:var(--text-muted);font-size:12px;margin-top:6px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}@media (prefers-reduced-motion:reduce){.hero-dot{box-shadow:none}}
.egress-test-btn{appearance:none;background:var(--surface-2);border:1px solid var(--border);color:var(--text);padding:0;border-radius:3px;cursor:pointer;width:22px;height:22px;display:inline-flex;align-items:center;justify-content:center;box-sizing:border-box;font-family:inherit;flex:none}.egress-test-btn:hover:not(:disabled){background:var(--surface)}.egress-test-btn:disabled{opacity:.6;cursor:default}.egress-test-btn svg{width:12px;height:12px;display:block}.egress-test-panel{font-size:11px;color:var(--text-muted);margin:3px 0 0 94px;display:flex;flex-wrap:wrap;gap:4px 12px}.egress-test-panel:empty{display:none}.egress-test-row-field{display:flex;gap:4px}.egress-test-key{color:var(--text-muted)}.egress-test-val{color:var(--text);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.egress-test-ok{color:var(--accent)}.egress-test-fail{color:var(--danger)}svg.spin{animation:ccwrap-spin 1s linear infinite;transform-origin:center}@keyframes ccwrap-spin{to{transform:rotate(360deg)}}.ribbon-cell[data-ribbon="Egress"]{position:relative}.egress-probe-btn{appearance:none;background:var(--surface-2);border:1px solid var(--border);color:var(--text-secondary);padding:0;border-radius:3px;cursor:pointer;width:20px;height:20px;display:inline-flex;align-items:center;justify-content:center;box-sizing:border-box;font-family:inherit;position:absolute;top:6px;right:6px;opacity:0;transition:opacity .15s ease}.ribbon-cell[data-ribbon="Egress"]:hover .egress-probe-btn,.ribbon-cell[data-ribbon="Egress"]:focus-within .egress-probe-btn,.egress-probe-btn:focus{opacity:1}.egress-probe-btn:hover:not(:disabled){background:var(--surface);color:var(--text)}.egress-probe-btn:disabled{opacity:.6;cursor:default}.egress-probe-btn svg{width:12px;height:12px;display:block}.egress-cell-result{font-size:10px;margin-top:3px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;word-break:break-word;line-height:1.4}.egress-cell-result:empty{display:none}.egress-cell-result-ok{color:var(--accent)}.egress-cell-result-fail{color:var(--danger)}
.ribbon{display:grid;grid-template-columns:repeat(7,1fr);margin:0 0 16px;background:var(--surface);border:1px solid var(--border);border-radius:11px}.ribbon-cell{background:var(--surface);border-right:1px solid var(--line);padding:13px 16px;min-height:80px;min-width:0;position:relative}.ribbon-cell:first-child{border-top-left-radius:11px;border-bottom-left-radius:11px}.ribbon-cell:last-child{border-right:0;border-top-right-radius:11px;border-bottom-right-radius:11px}.ribbon-cell:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}.ribbon-cell .k{font-size:10px;letter-spacing:.04em;text-transform:uppercase;color:var(--text-muted);margin-bottom:6px}.ribbon-cell .v{font-size:14px;font-weight:600;word-break:break-word}.ribbon-cell .v.muted{color:var(--text-muted);font-weight:400}.ribbon-cell .d{font-size:12px;color:var(--text-muted);margin-top:4px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;word-break:break-word}
.ribbon-cell[data-ribbon="Models"][data-state="aliases-active"]{cursor:pointer}
.ribbon-cell[data-ribbon="Models"][data-state="aliases-active"]:hover{background:var(--surface-2)}
.ribbon-cell[data-ribbon="Models"][data-state="aliases-active"] .v::after{content:" ▾";color:var(--text-muted);font-size:11px}
.sp3-models-pop{position:absolute;top:calc(100% + 8px);left:0;right:auto;min-width:280px;max-width:420px;z-index:6;background:var(--surface);border:1px solid var(--border);border-radius:10px;box-shadow:0 12px 30px rgba(0,0,0,.6);padding:10px 12px;font-size:11px}
.sp3-models-pop::before{content:"";position:absolute;top:-6px;left:18px;width:10px;height:10px;background:var(--surface);border-left:1px solid var(--border);border-top:1px solid var(--border);transform:rotate(45deg)}
.sp3-models-pop .head{color:var(--text-muted);font-size:10px;letter-spacing:.06em;text-transform:uppercase;margin-bottom:6px}
.sp3-models-pop table{width:100%;border-collapse:collapse;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.sp3-models-pop td{padding:3px 6px;color:var(--text);vertical-align:top}
.sp3-models-pop td.from{color:var(--text-secondary);white-space:nowrap}
.sp3-models-pop td.arrow{color:var(--text-muted);padding:3px 4px}
.sp3-models-pop td.to{color:var(--accent);word-break:break-all}
.bodies-pop{position:absolute;top:calc(100% + 8px);left:0;right:auto;min-width:280px;max-width:360px;z-index:6;background:var(--surface);border:1px solid var(--border);border-radius:10px;box-shadow:0 12px 30px rgba(0,0,0,.6);padding:8px;font-size:12px}
.bodies-pop::before{content:"";position:absolute;top:-6px;left:18px;width:10px;height:10px;background:var(--surface);border-left:1px solid var(--border);border-top:1px solid var(--border);transform:rotate(45deg)}
.bodies-pop .head{color:var(--text-muted);font-size:10px;letter-spacing:.06em;text-transform:uppercase;margin:2px 4px 6px}
.bodies-pop-row{display:flex;align-items:flex-start;gap:9px;padding:7px 8px;border-radius:7px;cursor:pointer}
.bodies-pop-row:hover{background:var(--surface-2)}
.bodies-pop-row.pending{opacity:.6;cursor:wait}
.bodies-pop-glyph{color:var(--text-muted);font-size:14px;line-height:1.3;flex:none}
.bodies-pop-row.on .bodies-pop-glyph{color:var(--accent)}
.bodies-pop-text{min-width:0}
.bodies-pop-label{color:var(--text);font-weight:600}
.bodies-pop-row.on .bodies-pop-label{color:var(--accent)}
.bodies-pop-desc{color:var(--text-muted);font-size:11px;margin-top:3px;line-height:1.4}
.ribbon-cell[data-ribbon="Profile"][data-state="active"]{border-color:#1d3a30;background:linear-gradient(180deg,var(--accent-soft),transparent 60%),var(--surface);cursor:pointer}
.ribbon-cell[data-ribbon="Profile"][data-state="inherit-env-clickable"]{cursor:pointer}
.ribbon-cell[data-ribbon="Profile"][data-state="inherit-env-static"]{border-style:dashed}
.ribbon-cell[data-ribbon="Bodies"]{cursor:pointer}
.ribbon-cell[data-ribbon="Bodies"]:hover{background:var(--surface-2)}
.ribbon-cell[data-ribbon="Bodies"][data-state="bodies-on"]{border-color:#1d3a30;background:linear-gradient(180deg,var(--accent-soft),transparent 60%),var(--surface)}
.ribbon-cell[data-ribbon="Bodies"][data-state="bodies-on"] .v{color:var(--accent)}
.ribbon-cell[data-ribbon="Bodies"][data-state="bodies-off"] .v{color:var(--text-muted)}
.ribbon-cell[data-ribbon="Bodies"][data-state="bodies-unmasked"]{border-color:#3a1d1d;background:linear-gradient(180deg,rgba(244,63,94,.15),transparent 60%),var(--surface)}
.ribbon-cell[data-ribbon="Bodies"][data-state="bodies-unmasked"] .v{color:var(--danger);font-weight:700}
.ribbon-cell[data-ribbon="Auth"][data-state="auth-missing"]{border-color:#3a2d1d;background:linear-gradient(180deg,rgba(251,191,36,.12),transparent 60%),var(--surface)}
.ribbon-cell[data-ribbon="Auth"][data-state="auth-missing"] .v{color:var(--warn);font-weight:700}
.ribbon-cell[data-ribbon="Egress"] .v{white-space:normal;overflow-wrap:anywhere;font-size:12px;line-height:1.35}
.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]){grid-template-columns:repeat(8,1fr)}
.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-active"] .v{color:var(--accent)}
.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-blocked"]{border-color:#3a1d1d;background:linear-gradient(180deg,rgba(244,63,94,.15),transparent 60%),var(--surface)}
.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-blocked"] .v{color:var(--danger);font-weight:700}
.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-active"],.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-blocked"]{cursor:pointer}
.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-active"] .v::after,.ribbon-cell[data-ribbon="NATIVE TLS"][data-state="native-blocked"] .v::after{content:" \25be";color:var(--text-muted);font-size:11px}
.native-tls-pop{min-width:300px;max-width:380px;right:0;left:auto}
.native-tls-pop .ntls-row{display:flex;align-items:baseline;gap:6px;padding:4px 4px}
.native-tls-pop .ntls-k{flex:none;color:var(--text-muted);font-size:10px;letter-spacing:.06em;text-transform:uppercase;min-width:62px}
.native-tls-pop .cmd{flex:1;min-width:0;padding:5px 7px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;color:var(--text);user-select:all;-webkit-user-select:all;cursor:copy;word-break:break-all}
.native-tls-pop .bv-dl{margin:6px 4px 4px}
.native-tls-pop .ntls-note{color:var(--text-muted);font-size:11px;margin:2px 4px 2px;line-height:1.4}
.sp3-chip{display:inline-flex;align-items:center;gap:6px;background:var(--accent-soft);color:var(--accent);border:1px solid var(--accent);border-radius:99px;padding:3px 10px;font-size:11px;text-transform:lowercase;font-weight:600}
.sp3-chip-dot{width:6px;height:6px;border-radius:50%;background:currentColor}
.sp3-chip-caret{font-size:9px}
.sp3-chip-inherit{background:var(--surface-2);color:var(--text-muted);border-color:var(--border)}
.sp3-chip-inherit-clickable:hover{color:var(--accent);border-color:var(--accent)}
.sp3-chip-inherit-static{cursor:default}
.sp3-chip.open{background:var(--accent);color:var(--bg)}
.sp3-pop{position:absolute;top:calc(100% + 8px);right:0;left:auto;width:480px;z-index:5;background:var(--surface);border:1px solid var(--border);border-radius:10px;box-shadow:0 12px 30px rgba(0,0,0,.6);padding:0;max-height:70vh;overflow:auto}
.sp3-pop::before{content:"";position:absolute;top:-6px;right:18px;width:10px;height:10px;background:var(--surface);border-left:1px solid var(--border);border-top:1px solid var(--border);transform:rotate(45deg)}
.sp3-pop-body{padding:10px 12px}
.sp3-pop-status{padding:8px 10px;color:var(--text-muted);font-size:12px;border-bottom:1px solid var(--line)}
.sp3-pop-group-head{font-size:10px;letter-spacing:.06em;text-transform:uppercase;color:var(--text-muted);margin:10px 0 4px}
.sp3-pop-row{display:grid;grid-template-columns:14px minmax(90px,140px) 1fr auto 22px 22px 22px 22px;gap:8px;align-items:center;padding:6px 4px;border-radius:6px;font-size:12px}
.sp3-pop-row:hover{background:var(--surface-2)}
.sp3-pop-row.pending{opacity:.6;cursor:wait}
.sp3-pop-row .pop-radio{color:var(--text-muted)}
.sp3-pop-row.active .pop-radio{color:var(--accent)}
.sp3-pop-row .pop-name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.sp3-pop-row .pop-host{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text-muted);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px}
.sp3-pop-row .pop-meta{color:var(--text-muted);font-size:11px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.sp3-pop-row .pop-test-btn{padding:1px 6px;font-size:10px;border-radius:3px;border:1px solid var(--border);background:var(--surface-2);color:var(--text-secondary);cursor:pointer;font-family:inherit}
.sp3-pop-row .pop-test-btn:hover{background:var(--surface);color:var(--text)}
.sp3-pop-row .pop-test-btn:disabled{opacity:.6;cursor:wait}
.sp3-pop .pop-footer-divider{border-top:1px solid var(--line);margin:8px 0}
.sp3-pop .pop-test-all-btn{display:flex;align-items:center;justify-content:center;width:100%;padding:4px 10px;border-radius:5px;border:1px solid var(--border);background:transparent;color:var(--text-secondary);cursor:pointer;font-family:inherit}
.sp3-pop .pop-test-all-btn:hover{background:var(--surface-2);color:var(--text)}
.sp3-pop .pop-test-all-btn:disabled{opacity:.6;cursor:wait}
.sp3-pop .pop-test-all-btn svg{width:14px;height:14px;display:block}
.sp3-pop .pop-test-chip{width:22px;height:22px;padding:0;border-radius:3px;display:inline-flex;align-items:center;justify-content:center;cursor:help;font-family:inherit;box-sizing:border-box}
.sp3-pop .pop-test-chip svg{width:14px;height:14px}
.sp3-pop .pop-test-chip[data-status="OK"]{background:#0a2e1f;color:var(--accent);border:1px solid #1d3a30}
.sp3-pop .pop-test-chip[data-status="SKIPPED"]{background:var(--surface-2);color:var(--text-muted);border:1px solid var(--border)}
.sp3-pop .pop-test-chip[data-status="AUTH_FAIL"]{background:#2e0a14;color:var(--danger);border:1px solid #3a1d27}
.sp3-pop .pop-test-chip[data-status="MODEL_404"]{background:#2e1f0a;color:var(--warn);border:1px solid #3a2d1d}
.sp3-pop .pop-test-chip[data-status="HTTP_4XX"]{background:#2e1f0a;color:var(--warn);border:1px solid #3a2d1d}
.sp3-pop .pop-test-chip[data-status="HTTP_5XX"]{background:#2e0a14;color:var(--danger);border:1px solid #3a1d27}
.sp3-pop .pop-test-chip[data-status="TIMEOUT"]{background:#2e0a14;color:var(--danger);border:1px solid #3a1d27}
.sp3-pop .pop-test-chip[data-status="NET_FAIL"]{background:#2e0a14;color:var(--danger);border:1px solid #3a1d27}
.sp3-pop .outcome{margin:10px 0 0;padding:9px 11px;border-radius:6px;font-size:12px;border:1px solid var(--border);background:var(--surface-2)}
.sp3-pop .outcome.success{border-color:#1d3a30;color:var(--accent)}
.sp3-pop .outcome.warn{border-color:#3a2d1d;color:var(--warn)}
.sp3-pop .outcome.danger{border-color:#3a1d27;color:var(--danger);white-space:pre-line}
.sp3-pop .outcome .dismiss{float:right;color:var(--text-muted);text-decoration:none;cursor:pointer}
.sp3-pop .outcome .outcome-reassure{color:var(--text-muted);margin-top:2px;font-size:11px}
.sp3-pop .outcome .cmd{margin-top:6px;padding:6px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;color:var(--text);user-select:all;-webkit-user-select:all;cursor:copy}
/* Row hover-reveal action buttons */
.sp3-pop-row .pop-action{width:22px;height:22px;border:1px solid var(--border);background:var(--surface-2);color:var(--text-secondary);border-radius:3px;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;padding:0;box-sizing:border-box;opacity:0;transition:opacity .15s ease;font-family:inherit}
.sp3-pop-row:hover .pop-action,.sp3-pop-row:focus-within .pop-action{opacity:1}
.sp3-pop-row .pop-action:hover{background:var(--surface);color:var(--text)}
.sp3-pop-row .pop-action.danger:hover{color:var(--danger)}
.sp3-pop-row .pop-action.is-default-state{opacity:1;background:#0a2e1f;border-color:#1d3a30;color:var(--accent)}
.sp3-pop-row .pop-action.is-default-state:hover{background:#0d3a26}
.sp3-pop-row .pop-action svg{width:14px;height:14px}
/* Edit / Add panel — slide-in replace */
.sp3-pop-edit-back{color:var(--accent);cursor:pointer;width:24px;height:24px;border:1px solid var(--border);background:var(--surface-2);border-radius:3px;display:inline-flex;align-items:center;justify-content:center;padding:0}
.sp3-pop-edit-back:hover{background:var(--surface)}
.sp3-pop-edit-back svg{width:14px;height:14px}
.sp3-pop-edit-head{display:flex;align-items:center;gap:10px;padding:8px 12px;border-bottom:1px solid var(--line);background:var(--surface);position:sticky;top:0;z-index:1}
.sp3-pop-edit-head .title{color:var(--text);font-weight:600;font-size:12px}
.sp3-pop-edit-err{background:#2e0a14;color:var(--danger);border-bottom:1px solid #3a1d27;padding:6px 12px;font-size:11px;white-space:pre-line}
.sp3-pop-edit-section{border-top:1px solid var(--line);padding:6px 12px}
.sp3-pop-edit-section:first-of-type{border-top:none}
.sp3-pop-edit-section-label{color:var(--text-muted);font-size:10px;font-weight:600;text-transform:uppercase;letter-spacing:.12em;margin-bottom:6px;padding-bottom:3px;border-bottom:1px solid var(--line)}
/* Auth toggle CSS removed — passthrough is now a dropdown option,
   no separate checkbox or disabled body state. */
.sp3-pop-edit-field{display:grid;grid-template-columns:88px 1fr auto;gap:6px;align-items:center;margin:3px 0}
.sp3-pop-edit-field label{color:var(--text-muted);font-size:11px}
.sp3-pop-edit-field input,.sp3-pop-edit-field select{background:var(--surface-dim,#0e1116);border:1px solid var(--border);color:var(--text);padding:3px 6px;border-radius:3px;font-family:inherit;font-size:11px;width:100%;box-sizing:border-box}
.sp3-pop-edit-field input::placeholder{color:#808080}
.sp3-pop-edit-field input.invalid,.sp3-pop-edit-field select.invalid{border-color:#3a1d27}
.sp3-pop-edit-field input:focus,.sp3-pop-edit-field select:focus,.sp3-pop-alias-row input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 1px var(--accent)}
.sp3-pop-edit-field input:disabled{color:var(--text-muted);opacity:.6;cursor:not-allowed}
.egress-mode-hint{margin:3px 0 0 94px;font-size:11px;color:var(--text-muted);line-height:1.4}
.egress-mode-hint:empty{display:none}
.auth-mode-hint{margin:3px 0 4px 94px;font-size:11px;color:var(--text-muted);line-height:1.4}
.auth-mode-hint:empty{display:none}
.sp3-pop-edit-field .pop-reveal{cursor:pointer;color:var(--text-secondary);background:var(--surface-2);border:1px solid var(--border);border-radius:3px;padding:1px 4px;font-size:9px;font-family:inherit;display:inline-flex;align-items:center;justify-content:center}
.sp3-pop-edit-field .pop-reveal svg{width:12px;height:12px}
.pop-keyfield{display:flex;min-width:0}
.pop-keyfield .pop-keysrc-sel{flex:none;width:auto;background:var(--surface-2);border:1px solid var(--border);color:var(--text-secondary);border-radius:3px 0 0 3px;border-right:0;padding:3px 6px;font-family:inherit;font-size:11px}
.pop-keyfield input{flex:1;min-width:0;border-radius:0 3px 3px 0}
.pop-keysrc-toggle{margin:5px 0 2px 94px;background:none;border:0;padding:0;color:var(--text-muted);font-size:11px;font-family:inherit;cursor:pointer;display:inline-flex;align-items:center;gap:4px}
.pop-keysrc-toggle::before{content:"↪";color:var(--text-muted)}
.pop-keysrc-toggle:hover{color:var(--accent)}
.sp3-pop-edit-actions{display:flex;gap:6px;align-items:center;padding:8px 12px;border-top:1px solid var(--line);background:var(--surface);position:sticky;bottom:0}
.sp3-pop-edit-actions .pop-set-default{flex:1;display:flex;align-items:center;gap:6px;color:var(--text-secondary);font-size:11px;cursor:pointer;user-select:none}
.sp3-pop-edit-actions .pop-set-default input[type=checkbox]{margin:0}
.sp3-pop-edit-actions button{border:1px solid var(--border);background:var(--surface-2);color:var(--text);padding:3px 12px;font-size:11px;border-radius:3px;cursor:pointer;font-family:inherit;display:inline-flex;align-items:center;gap:5px}
.sp3-pop-edit-actions button.primary{background:#0a2e1f;border-color:#1d3a30;color:#10b981}
.sp3-pop-edit-actions button.primary:hover{background:#0d3a26}
.sp3-pop-edit-actions button.primary:disabled{opacity:.4;cursor:not-allowed}
.sp3-pop-edit-actions button svg{width:12px;height:12px}
/* Model aliases editor (popover edit/add panel) */
.sp3-pop-aliases-list{display:flex;flex-direction:column;gap:4px;margin:4px 0}
.sp3-pop-aliases-empty{color:var(--text-muted);font-size:11px;font-style:italic;padding:2px 0 6px}
.sp3-pop-aliases-list:not(:empty) ~ .sp3-pop-aliases-empty{display:none}
.sp3-pop-alias-row{display:grid;grid-template-columns:1fr 16px 1fr 22px;gap:6px;align-items:center}
.sp3-pop-alias-row input{background:var(--surface-dim,#0e1116);border:1px solid var(--border);color:var(--text);padding:3px 6px;border-radius:3px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;width:100%;box-sizing:border-box}
.sp3-pop-alias-row .alias-arrow{color:var(--text-muted);text-align:center;font-size:11px}
.sp3-pop-alias-row .alias-rm{width:22px;height:22px;border:1px solid var(--border);background:var(--surface-2);color:var(--text-muted);border-radius:3px;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;padding:0;font-family:inherit;font-size:11px}
.sp3-pop-alias-row .alias-rm:hover{color:var(--danger);border-color:var(--danger)}
.sp3-pop-alias-add{margin-top:6px;background:transparent;border:1px dashed var(--border);color:var(--text-muted);padding:4px 10px;font-size:11px;border-radius:3px;cursor:pointer;font-family:inherit;width:100%}
.sp3-pop-alias-add:hover{color:var(--text);border-color:var(--text-muted)}
/* Rm inline-confirm: click trash once to arm (2s window), click again to delete.
   Replaces the modal-based type-to-confirm flow as of 2026-05-26 (user feedback). */
.sp3-pop-row .pop-action.danger.armed{opacity:1;background:var(--danger);color:#fff;border-color:var(--danger);animation:rm-armed-pulse .6s ease-in-out infinite alternate}
.sp3-pop-row .pop-action.danger.armed:hover{background:var(--danger);color:#fff}
@keyframes rm-armed-pulse{from{box-shadow:0 0 0 0 rgba(255,80,80,.45)}to{box-shadow:0 0 0 3px rgba(255,80,80,0)}}
/* Toast — top-right, fixed, fades out after 4s. Used for post-mutation status
   (delete success / warnings about active sessions / errors). Replaces alert(). */
.ccwrap-toast{position:fixed;top:16px;right:16px;background:var(--text);color:var(--bg);padding:8px 12px;border-radius:4px;font-size:12px;z-index:1000;box-shadow:0 2px 8px rgba(0,0,0,.25);opacity:1;transition:opacity 400ms;max-width:360px;line-height:1.4}
.ccwrap-toast[data-severity="warning"]{background:#3a2900;color:#ffd97a;border:1px solid #5a4300}
.ccwrap-toast[data-severity="error"]{background:var(--danger);color:#fff}
.ccwrap-toast.fade{opacity:0}
/* Footer + add */
.sp3-pop-footer-add{padding:7px 12px;border-top:1px solid var(--line);background:var(--surface);display:flex;justify-content:center}
.sp3-pop-footer-add button{border:1px solid #1d3a30;background:var(--accent-soft);color:var(--accent);padding:5px 18px;font-size:11px;font-weight:600;border-radius:4px;cursor:pointer;font-family:inherit;display:inline-flex;align-items:center;gap:6px}
.sp3-pop-footer-add button:hover{background:#0d3a26;color:var(--accent)}
.sp3-pop-footer-add button svg{width:12px;height:12px}
/* Success flash on edited row */
@keyframes sp3-flash-green{0%,100%{box-shadow:inset 0 0 0 0 var(--accent)}50%{box-shadow:inset 0 0 0 1px var(--accent)}}
.sp3-pop-row.flash-success{animation:sp3-flash-green 300ms ease}
.sp3-pop-row.fading-out{animation:sp3-fade-out 300ms ease forwards}
@keyframes sp3-fade-out{to{opacity:0;transform:translateY(-4px)}}
.rows .row[data-category="profile_switch"]{grid-template-columns:1fr;color:var(--trace);background:rgba(167,139,250,.04);border-left:2px solid var(--trace);padding:8px 12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
.ann{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;margin-left:6px}
@media (max-width:1280px){.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]){grid-template-columns:repeat(4,1fr);gap:10px;background:none;border:0}.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]) .ribbon-cell{border:1px solid var(--border);border-radius:11px}.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]) .ribbon-cell:last-child{border-right:1px solid var(--border)}}
@media (max-width:1024px){.ribbon{grid-template-columns:repeat(2,1fr);gap:10px;background:none;border:0}.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]){grid-template-columns:repeat(2,1fr)}.ribbon-cell{border:1px solid var(--border);border-radius:11px}.ribbon-cell:last-child{border-right:1px solid var(--border)}.ribbon-cell[data-ribbon="Profile"]{grid-column:1/-1}}
@media (max-width:820px){body{font-size:16px}.rows .row-head{display:none}.rows details.row>summary,.rows .row.nf{grid-template-columns:1fr;gap:3px;padding:10px 12px}.rows details.row>summary .rowchev{position:absolute;top:10px;right:12px}.rows .row.nf .rowchev{display:none}.rows .cell-time,.rows .cell-label{font-size:11px}.rows .cell-label{white-space:normal;overflow:visible;text-overflow:clip;overflow-wrap:anywhere}}
@media (max-width:600px){.ribbon-cell{min-height:56px;padding:10px 12px}.ribbon-cell[data-ribbon="Profile"]{order:-1}.sp3-pop{width:calc(100vw - 32px);right:8px;left:auto}.bodies-pop{min-width:0;max-width:calc(100vw - 32px);width:calc(100vw - 32px);left:8px;right:auto}.sp3-models-pop{min-width:0;max-width:calc(100vw - 32px)}.topbar{flex-direction:column;align-items:flex-start;gap:10px}.topbar-side{align-items:flex-start;width:100%}.actions{justify-content:flex-start}}
.panel,.section{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:14px 16px}.panel{margin-bottom:16px}.panel-head,.section-head{display:flex;justify-content:space-between;align-items:baseline;gap:16px;margin-bottom:12px}.panel-head h2,.section h2,.section-head h2{margin:0;font-size:14px;line-height:1.3}.hint{color:var(--text-muted);font-size:12px}.rows{border:1px solid var(--line);border-radius:8px;overflow:hidden}.row-head,.row{display:grid;grid-template-columns:92px 112px minmax(260px,1.8fr) minmax(220px,1.4fr);gap:10px;align-items:start;padding:10px 12px}.row-head{background:var(--surface-2);color:var(--text-muted);font-size:11px;letter-spacing:.08em;text-transform:uppercase;font-weight:700}.row-head>div,.row>div{min-width:0}.row + .row{border-top:1px solid var(--line)}.cell-time,.cell-label,.cell-right{color:var(--text-muted)}.cell-label{white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.cell-main{font-weight:600;word-break:break-word}.cell-right{word-break:break-word}.row[data-st="warn"] .cell-right{color:var(--warn)}.row[data-st="err"] .cell-right{color:var(--danger);font-weight:600}.list{display:grid;gap:8px}.empty{color:var(--text-muted);padding:8px 0}.stack{display:grid;gap:16px}.section summary{list-style:none;cursor:pointer}.section summary::-webkit-details-marker{display:none}.diagnostics{padding:0;overflow:hidden}.diagnostics summary{padding:14px 16px}.diagnostics[open] summary{border-bottom:1px solid var(--line)}.diag-content{padding:14px 16px}.diag-stack{display:grid;gap:14px}.subsection + .subsection{margin-top:14px}.subsection h3{margin:0 0 10px;font-size:13px;line-height:1.3}.section[hidden]{display:none !important}
.row[data-forwarded="true"]{border-left:2px solid var(--accent);background:linear-gradient(90deg,rgba(16,185,129,.04),transparent 80%)}.row[data-kind="trace"] .cell-label{color:var(--trace)}
.hdr-drawer{margin:2px 0 6px 92px}.hdr-drawer>summary{cursor:pointer;color:var(--text-muted);font-size:12px}.hdr-panel{padding:6px 0}.hdr-group-name{font-size:10px;letter-spacing:.06em;text-transform:uppercase;color:var(--text-muted);margin:14px 0 5px}.hdr-group:first-of-type .hdr-group-name{margin-top:4px}.hdr-row{display:grid;grid-template-columns:230px 1fr;gap:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;padding:2px 0}.hdr-k{color:var(--text-secondary)}.hdr-v{color:var(--text);overflow-wrap:anywhere}.hdr-v.redv{color:var(--text-muted)}.hdr-note{margin:2px 0 6px 92px;color:var(--text-muted);font-size:12px}
.body-drawer{margin:2px 0 6px 92px}.body-drawer>summary{cursor:pointer;color:var(--text-muted);font-size:12px}.body-panel{padding:6px 0;font-size:12px}.body-view-toggle{display:flex;gap:6px;margin-bottom:9px;align-items:center}.body-view-btn{padding:3px 11px;font-size:11px;border:1px solid var(--border);background:var(--surface-2);color:var(--text-secondary);border-radius:99px;cursor:pointer;font-family:inherit;font-weight:500}.body-view-btn.on{background:#0a2e1f;color:var(--accent);border-color:#1d3a30}.body-view-btn:hover:not(.on){background:var(--surface)}.body-view-hint{color:var(--text-muted);font-size:11px;margin-left:auto}.body-view-pane{display:none}.body-view-pane.on{display:block}.body-anatomy{display:flex;gap:7px;flex-wrap:wrap;margin:0 0 12px}.body-anatomy span{background:var(--surface-2);border:1px solid var(--line);border-radius:5px;padding:3px 8px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;color:var(--text-secondary)}.body-anatomy span b{color:var(--accent);font-weight:600}.body-panel pre{margin:5px 0 0;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:10.5px;color:var(--text-secondary);max-height:240px;overflow:auto;background:var(--bg);border:1px solid var(--line);border-radius:5px;padding:8px}
details.row{display:block;padding:0}details.row>summary{list-style:none;display:grid;grid-template-columns:16px 92px 112px minmax(260px,1.8fr) minmax(220px,1.4fr);gap:10px;align-items:start;padding:10px 12px;cursor:pointer;position:relative}details.row>summary::-webkit-details-marker{display:none}details.row>summary .rowchev{color:var(--text-muted);text-align:center}details.row>summary .rowchev::after{content:"\25B8"}details.row[open]>summary .rowchev::after{content:"\25BE"}details.row[open]>summary{background:var(--surface-2)}details.row>summary:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}.reqinspect{padding:8px 14px 12px 38px;background:var(--surface)}
.rows .row-head{grid-template-columns:16px 92px 112px minmax(260px,1.8fr) minmax(220px,1.4fr)}.row.nf{grid-template-columns:16px 92px 112px minmax(260px,1.8fr) minmax(220px,1.4fr)}.row.nf .rowchev{visibility:hidden}.nf-note{grid-column:1/-1;color:var(--text-muted);font-size:12px;font-style:italic}
details.req-sub{border:1px solid var(--line);border-radius:6px;margin:6px 0;background:var(--surface-2);overflow:hidden}details.req-sub>summary{list-style:none;cursor:pointer;font-size:12px;color:var(--text);padding:7px 11px}details.req-sub>summary::-webkit-details-marker{display:none}details.req-sub>summary::before{content:"\25B8  ";color:var(--text-muted)}details.req-sub[open]>summary::before{content:"\25BE  "}.req-sub .sub-body{padding:7px 11px;border-top:1px solid var(--line)}
.hdr-sum{display:flex;gap:7px;flex-wrap:wrap;margin:4px 0 12px}.hdr-sum span{background:var(--surface);border:1px solid var(--line);border-radius:5px;padding:3px 8px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;color:var(--text-secondary)}.hdr-sum span b{color:var(--accent)}.hdr-sum span.red b{color:var(--text-muted)}.hdr-redpill{display:inline-block;font-size:9px;text-transform:uppercase;letter-spacing:.03em;color:var(--text-muted);border:1px solid var(--border);background:var(--surface);border-radius:99px;padding:0 6px;margin-left:8px}
.bv-config{display:grid;grid-template-columns:max-content 1fr;gap:2px 16px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;background:var(--surface-2);border:1px solid var(--line);border-radius:6px;padding:9px 13px;margin:5px 0}.bv-config .ck{color:var(--text-muted)}.bv-config .cv{color:var(--text);word-break:break-all}.bv-seclabel{font-size:10px;letter-spacing:.06em;text-transform:uppercase;color:var(--text-muted);margin:11px 0 4px}.bv-block{border-left:3px solid var(--line);padding:4px 0 4px 10px;margin:7px 0;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px}.bv-block.cc{border-left-color:var(--warn)}.bv-block.ccg{border-left-color:var(--accent)}.bv-block .bh{color:var(--text-muted);margin-bottom:3px}.bv-chip{display:inline-block;font-size:9px;text-transform:uppercase;color:var(--warn);border:1px solid var(--warn);border-radius:99px;padding:0 6px;margin-left:7px}.bv-chip.g{color:var(--accent);border-color:var(--accent)}.bv-tool .tb{padding:7px 11px;border-top:1px solid var(--line)}.bv-dl{display:inline-block;margin:0 0 6px;font-size:11px;color:var(--accent);border:1px solid var(--accent);border-radius:5px;padding:1px 9px;cursor:pointer;text-decoration:none}.bv-acc{padding:7px 11px;border-top:1px solid var(--line)}.body-panel details{border:1px solid var(--line);border-radius:6px;margin:6px 0;background:var(--surface-2);overflow:hidden}.body-panel details>summary{list-style:none;cursor:pointer;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;color:var(--text);padding:7px 11px}.body-panel details>summary::-webkit-details-marker{display:none}.body-panel details>summary::before{content:"\25B8  ";color:var(--text-muted)}.body-panel details[open]>summary::before{content:"\25BE  "}.bv-tool>summary .tm{color:var(--text-muted)}.bv-tool .tb .bh{color:var(--text-muted);margin:6px 0 3px}.bv-role{color:var(--accent);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;margin:9px 0 3px}
.filter-bar{display:flex;gap:6px;flex-wrap:wrap;padding:8px 0}.filter-btn{background:var(--surface-2);color:var(--text-muted);border:1px solid var(--line);border-radius:99px;padding:4px 11px;font-size:12px;cursor:pointer}.filter-btn.on{background:#0a2e1f;color:var(--accent);border-color:#1d3a30}a.filter-btn{display:inline-block}a.filter-btn:hover{text-decoration:none;background:var(--surface)}.filter-n{color:var(--text-secondary)}.show-more{padding:8px 0}.show-more button{background:none;border:1px solid var(--line);color:var(--text-muted);border-radius:6px;padding:5px 12px;cursor:pointer;font-size:12px}
.bv-links{margin:6px 0}.bv-links .bv-dl{margin:0 8px 0 0}
.skip-link{position:absolute;left:-9999px;top:0;background:var(--surface-2);color:var(--text);padding:8px 12px;border:1px solid var(--border);border-radius:8px;z-index:10}.skip-link:focus{left:8px}
.btn{padding:10px 14px}
</style>
</head>
<body>
<svg width="0" height="0" style="position:absolute" aria-hidden="true">
  <defs>
    <symbol id="i-back" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M19 12H5"/><path d="M12 19l-7-7 7-7"/>
    </symbol>
    <symbol id="i-edit" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M12 20h9"/><path d="M16.5 3.5a2.121 2.121 0 1 1 3 3L7 19l-4 1 1-4L16.5 3.5z"/>
    </symbol>
    <symbol id="i-activity" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>
    </symbol>
    <symbol id="i-play" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <polygon points="5 3 19 12 5 21 5 3"/>
    </symbol>
    <symbol id="i-check-circle" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <circle cx="12" cy="12" r="10"/><polyline points="9 12 12 15 16 9"/>
    </symbol>
    <symbol id="i-trash" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <polyline points="3 6 5 6 21 6"/><path d="M19 6l-2 14a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/>
    </symbol>
    <symbol id="i-eye" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>
    </symbol>
    <symbol id="i-eye-off" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94"/><path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19"/><path d="M14.12 14.12a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/>
    </symbol>
    <symbol id="i-check" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
      <polyline points="20 6 9 17 4 12"/>
    </symbol>
    <symbol id="i-x" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>
    </symbol>
    <symbol id="i-plus" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
    </symbol>
    <symbol id="i-minus" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <line x1="5" y1="12" x2="19" y2="12"/>
    </symbol>
    <symbol id="i-alert-triangle" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>
    </symbol>
    <symbol id="i-clock" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>
    </symbol>
    <symbol id="i-refresh-cw" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10"/><path d="M20.49 15a9 9 0 0 1-14.85 3.36L1 14"/>
    </symbol>
    <symbol id="i-loader" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M21 12a9 9 0 1 1-6.219-8.56"/>
    </symbol>
    <symbol id="i-zap" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/>
    </symbol>
    <symbol id="i-more" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="5" r="1"></circle><circle cx="12" cy="12" r="1"></circle><circle cx="12" cy="19" r="1"></circle></symbol>
  </defs>
</svg>
<a href="#activity-body" class="skip-link">Skip to data</a>
<div class="wrap">
  <div class="topbar">
    <div class="brandbar">
      <span class="brandmark" aria-hidden="true"><i></i></span>
      <span class="wordmark">CCWRAP</span>
      <span class="vr" id="claude-sess-vr"{{if not .SessionLabel}} hidden{{end}}></span><span class="sesslabel mono" id="claude-sess-label"{{if not .SessionLabel}} hidden{{end}}{{if .ClaudeSessionFull}} title="{{.ClaudeSessionFull}}" aria-label="Claude Code session {{.ClaudeSessionFull}}" role="button" tabindex="0" data-full="{{.ClaudeSessionFull}}"{{end}}>{{.SessionLabel}}</span>
      <h1 class="visually-hidden">{{.Heading}}</h1>
      {{if .Subtitle}}<span class="visually-hidden">{{.Subtitle}}</span>{{end}}
    </div>
    <div class="topbar-side">
      <div class="actions">
        {{if .LiveEnabled}}<span class="state-pill app-stream-pill" id="live-chip" data-state="connecting"><span class="state-dot"></span><span id="live-label">connecting</span></span>{{end}}
        <button class="btn btn-icon" type="button" id="refresh-btn"{{if .LiveEnabled}} style="display:none"{{end}} onclick="window.location.reload()" title="Reload the dashboard" aria-label="Refresh"><svg aria-hidden="true"><use href="#i-refresh-cw" xlink:href="#i-refresh-cw"></use></svg></button>
        {{if .LiveEnabled}}<button class="btn btn-icon" type="button" id="live-toggle" title="Pause stream" aria-label="Pause stream"><svg class="spin" aria-hidden="true"><use href="#i-loader" xlink:href="#i-loader"></use></svg></button>{{end}}
        {{if .Links}}<div class="ovf-wrap"><button class="btn btn-icon" type="button" id="ovf-btn" aria-haspopup="true" aria-expanded="false" title="More"><svg aria-hidden="true"><use href="#i-more" xlink:href="#i-more"></use></svg></button><div class="ovf-menu" id="ovf-menu" hidden>{{range .Links}}<a class="ovf-item" href="{{.Href}}" target="_blank" rel="noopener">{{if .Icon}}<svg aria-hidden="true"><use href="#{{.Icon}}" xlink:href="#{{.Icon}}"></use></svg>{{end}}{{.Label}}</a>{{end}}</div></div>{{end}}
      </div>
    </div>
  </div>

  {{if .HeroSentence}}
  <div class="hero" id="hero" data-variant="{{.HeroVariant}}">
    <div class="hero-kicker">Session status</div>
    <div class="hero-head">
      <span class="hero-dot" id="hero-dot"></span>
      <span class="hero-state" id="hero-state">{{.HeroState}}</span>
      <span class="hero-meta" id="hero-meta">{{.HeroMeta}}</span>
    </div>
    <div class="hero-body" id="hero-body">{{.HeroSentence}}</div>
    {{if .LiveEnabled}}
    <div class="hero-sub" id="last-activity-cell" data-last-activity="{{.LastActivityRFC3339}}">last activity <span id="last-activity">{{.LastActivityLabel}}</span></div>
    {{end}}
  </div>
  {{end}}

  {{if .Ribbon}}
  <div class="ribbon" id="ribbon">
    {{range .Ribbon}}
    <div class="ribbon-cell" data-ribbon-cell data-ribbon="{{.Label}}"{{if .DataState}} data-state="{{.DataState}}"{{end}}><div class="k">{{.Label}}</div><div class="v {{if .Mono}}mono{{end}}{{if not (or .Value .ValueHTML)}} muted{{end}}" data-ribbon-value>{{if .ValueHTML}}{{.ValueHTML}}{{else if .Value}}{{.Value}}{{else}}—{{end}}</div>{{if .Detail}}<div class="d">{{.Detail}}</div>{{end}}</div>
    {{end}}
  </div>
  {{end}}

  <section class="panel" aria-live="polite">
    <div class="panel-head"><h2>{{.ActivityTitle}}</h2><div class="hint">newest first</div></div>
    {{if .Classes}}
    <div class="filter-bar" id="activity-filter"{{if .LiveEnabled}} role="tablist"{{end}}>
      {{if .LiveEnabled}}{{range .Classes}}<button class="filter-btn{{if eq .Class $.DefaultClass}} on{{end}}" id="filter-tab-{{.Class}}" data-filter="{{.Class}}" role="tab" aria-selected="{{if eq .Class $.DefaultClass}}true{{else}}false{{end}}" aria-controls="activity-body"{{if ne .Class $.DefaultClass}} tabindex="-1"{{end}}>{{.Label}} <span class="filter-n">{{.Count}}</span></button>{{end}}{{else}}{{range .Classes}}<a class="filter-btn{{if eq .Class $.DefaultClass}} on{{end}}" data-filter="{{.Class}}" href="?class={{.Class}}"{{if eq .Class $.DefaultClass}} aria-current="page"{{end}}>{{.Label}} <span class="filter-n">{{.Count}}</span></a>{{end}}{{end}}
    </div>
    {{end}}
    <div id="activity-body"{{if and .LiveEnabled .Classes}} role="tabpanel" aria-labelledby="filter-tab-{{.DefaultClass}}"{{end}}>
      {{if .ActivityRows}}
      <div class="rows" role="list">
        <div class="row-head" aria-hidden="true"><div></div><div>Time</div><div>Kind</div><div>Summary</div><div>Details</div></div>
        {{range .ActivityRows}}{{if .HeaderGroups}}<details class="row" role="listitem" data-forwarded="{{.Forwarded}}" data-kind="{{.Kind}}" data-class="{{.Class}}"{{if .StatusTier}} data-st="{{.StatusTier}}"{{end}}{{if .Category}} data-category="{{.Category}}"{{end}}{{if .ProfileName}} data-profile-name="{{.ProfileName}}" data-profile-provider="{{.ProfileProvider}}"{{end}}{{if .SwitchFrom}} data-switch-from="{{.SwitchFrom}}"{{end}}{{if .SwitchFromProvider}} data-switch-from-provider="{{.SwitchFromProvider}}"{{end}}{{if .SwitchTo}} data-switch-to="{{.SwitchTo}}"{{end}}{{if .SwitchToProvider}} data-switch-to-provider="{{.SwitchToProvider}}"{{end}}{{if .SwitchClass}} data-switch-class="{{.SwitchClass}}"{{end}}{{if .SwitchRequested}} data-switch-requested="{{.SwitchRequested}}"{{end}}{{if .SwitchReason}} data-switch-reason="{{.SwitchReason}}"{{end}}><summary><span class="rowchev" aria-hidden="true"></span><span class="cell-time">{{.Time}}</span><span class="cell-label" title="{{.Label}}">{{.Label}}</span><span class="cell-main {{if .Mono}}mono{{end}}">{{.Main}}</span><span class="cell-right">{{.Right}}</span></summary><div class="reqinspect"><details class="req-sub"><summary>request headers</summary><div class="sub-body">{{.HeaderSummary}}{{range .HeaderGroups}}<div class="hdr-group"><div class="hdr-group-name">{{.Name}}</div>{{range .Rows}}<div class="hdr-row"><span class="hdr-k mono">{{.Name}}</span><span class="hdr-v mono{{if .Redacted}} redv{{end}}">{{.Value}}{{if .Redacted}} <span class="hdr-redpill">redacted</span>{{end}}</span></div>{{end}}</div>{{end}}</div></details>{{if .BodyRefID}}{{if $.LiveEnabled}}<details class="req-sub body-drawer" data-reqid="{{.BodyRefID}}"{{if .UpstreamBodyRefID}} data-upstream-reqid="{{.UpstreamBodyRefID}}"{{end}}><summary>request body</summary><div class="sub-body body-panel">full request body — loading…</div></details>{{else}}<div class="bv-links"><a class="bv-dl" href="/recent/body?id={{.BodyRefID}}" target="_blank" rel="noopener">request body (raw JSON)</a>{{if .UpstreamBodyRefID}}<a class="bv-dl" href="/recent/body?id={{.UpstreamBodyRefID}}" target="_blank" rel="noopener">forwarded body (raw JSON)</a>{{end}}</div>{{end}}{{end}}{{if .ResponseBodyRefID}}{{if $.LiveEnabled}}<details class="req-sub body-drawer resp-drawer" data-reqid="{{.ResponseBodyRefID}}"><summary>response body</summary><div class="sub-body body-panel">response body — loading…</div></details>{{else}}<div class="bv-links"><a class="bv-dl" href="/recent/body?id={{.ResponseBodyRefID}}" target="_blank" rel="noopener">response body (raw)</a></div>{{end}}{{end}}</div></details>{{else if .TelemetryDrawer}}<details class="row" role="listitem" data-forwarded="false" data-kind="{{.Kind}}" data-class="{{.Class}}"{{if .StatusTier}} data-st="{{.StatusTier}}"{{end}}><summary><span class="rowchev" aria-hidden="true"></span><span class="cell-time">{{.Time}}</span><span class="cell-label" title="{{.Label}}">{{.Label}}</span><span class="cell-main {{if .Mono}}mono{{end}}">{{.Main}}</span><span class="cell-right">{{.Right}}</span></summary><div class="reqinspect">{{if .BodyRefID}}{{if $.LiveEnabled}}<details class="req-sub body-drawer tele-drawer" data-reqid="{{.BodyRefID}}"><summary>request body</summary><div class="sub-body body-panel">request body — loading…</div></details>{{else}}<div class="bv-links"><a class="bv-dl" href="/recent/body?id={{.BodyRefID}}" target="_blank" rel="noopener">request body (raw)</a></div>{{end}}{{end}}{{if .ResponseBodyRefID}}{{if $.LiveEnabled}}<details class="req-sub body-drawer tele-drawer" data-reqid="{{.ResponseBodyRefID}}"><summary>response body</summary><div class="sub-body body-panel">response body — loading…</div></details>{{else}}<div class="bv-links"><a class="bv-dl" href="/recent/body?id={{.ResponseBodyRefID}}" target="_blank" rel="noopener">response body (raw)</a></div>{{end}}{{end}}</div></details>{{else}}<div class="row nf" role="listitem" data-forwarded="{{.Forwarded}}" data-kind="{{.Kind}}" data-class="{{.Class}}"{{if .StatusTier}} data-st="{{.StatusTier}}"{{end}}{{if .Category}} data-category="{{.Category}}"{{end}}{{if .ProfileName}} data-profile-name="{{.ProfileName}}" data-profile-provider="{{.ProfileProvider}}"{{end}}{{if .SwitchFrom}} data-switch-from="{{.SwitchFrom}}"{{end}}{{if .SwitchFromProvider}} data-switch-from-provider="{{.SwitchFromProvider}}"{{end}}{{if .SwitchTo}} data-switch-to="{{.SwitchTo}}"{{end}}{{if .SwitchToProvider}} data-switch-to-provider="{{.SwitchToProvider}}"{{end}}{{if .SwitchClass}} data-switch-class="{{.SwitchClass}}"{{end}}{{if .SwitchRequested}} data-switch-requested="{{.SwitchRequested}}"{{end}}{{if .SwitchReason}} data-switch-reason="{{.SwitchReason}}"{{end}}><span class="rowchev" aria-hidden="true"></span><div class="cell-time">{{.Time}}</div><div class="cell-label" title="{{.Label}}">{{.Label}}</div><div class="cell-main {{if .Mono}}mono{{end}}">{{.Main}}</div><div class="cell-right">{{.Right}}</div>{{if .HeaderNote}}<div class="nf-note">{{.HeaderNote}}</div>{{end}}</div>{{end}}{{end}}
      </div>
      <div class="show-more" id="activity-more" hidden><button>show more</button></div>
      {{else}}
      <div class="empty">{{.ActivityEmpty}}</div>
      {{end}}
    </div>
  </section>

  {{if .Summary}}
  <details class="section diagnostics" id="config-panel">
    <summary>
      <div class="section-head" style="margin:0;"><h2>Configuration details</h2></div>
    </summary>
    <div class="diag-content">
      <div class="rows" role="list">
        {{range .Summary}}
        <div class="row" role="listitem" style="grid-template-columns:160px 1fr"><div class="cell-label">{{.Label}}</div><div class="cell-main mono">{{.Value}}</div></div>
        {{end}}
      </div>
    </div>
  </details>
  {{end}}
</div>

{{if .LiveEnabled}}
<script id="ccwrap-bootstrap" type="application/json" data-b64="{{.BootstrapB64}}"></script>
<script>
(()=>{
  // Overflow-menu toggle. Intentionally duplicated in the not-LiveEnabled
  // block lower down: ended (non-live) pages omit this whole live script
  // element yet still need the menu. Inlined here (not a second shared script
  // element) so the node --check tests' single-script-element extraction stays
  // valid. Keep the two copies in sync.
  (function(){
    var b=document.getElementById('ovf-btn'),m=document.getElementById('ovf-menu');
    if(!b||!m)return;
    function close(){m.hidden=true;b.setAttribute('aria-expanded','false');}
    b.addEventListener('click',function(e){e.stopPropagation();var open=m.hidden;m.hidden=!open;b.setAttribute('aria-expanded',String(open));});
    document.addEventListener('click',function(e){if(!m.hidden&&!m.contains(e.target)&&e.target!==b)close();});
    document.addEventListener('keydown',function(e){if(e.key==='Escape')close();});
  })();
  const bootNode = document.getElementById('ccwrap-bootstrap');
  if (!bootNode) return;
  let boot = {};
  try { boot = JSON.parse(atob(bootNode.dataset.b64 || '')); } catch (_) { boot = {}; }
  // CSRF token capture. Read ONCE; never written to the DOM.
  // Used as X-CCWRAP-Profile-Token header on every POST /profile/switch.
  var PROFILE_TOKEN = String(boot.profile_token || '');
  // The credential deny-list is single-sourced from the
  // Go renderer via the bootstrap; JS classifies identically (no
  // drift). Grouping below is the cosmetic hand-mirror of Go's
  // headerGroupOrder (security-critical part is classifyHdr only).
  var HEADER_DENY = new Set((boot.header_deny_list || []).map(function(s){ return String(s).toLowerCase(); }));
  function classifyHdr(n){ return HEADER_DENY.has(String(n).toLowerCase()) ? 'cred' : 'shown'; }
  // updateClaudeSession live-patches the brandbar chip from the latest
  // request that carries the Claude conversation header. Pure DOM via
  // textContent/setAttribute (third-party value, never innerHTML).
  function updateClaudeSession(rec){
    var rh = rec && rec.request_headers; if (!rh) return;
    var want = String(boot.claude_session_header || '').toLowerCase(); if (!want) return;
    var full = '';
    for (var k in rh){
      if (Object.prototype.hasOwnProperty.call(rh, k) && k.toLowerCase() === want){
        var v = rh[k]; full = Array.isArray(v) ? (v[0] || '') : String(v || ''); break;
      }
    }
    if (!full) return;
    var label = document.getElementById('claude-sess-label'); if (!label) return;
    if (label.getAttribute('data-full') === full) return;
    var vr = document.getElementById('claude-sess-vr');
    label.textContent = 'session ' + full.slice(0, 8);
    // Mirror the server-rendered <title> live so parallel session tabs
    // stay tellable apart as conversation ids arrive over SSE.
    document.title = 'CCWRAP · session ' + full.slice(0, 8);
    label.setAttribute('title', full);
    label.setAttribute('aria-label', 'Claude Code session ' + full);
    label.setAttribute('data-full', full);
    label.setAttribute('role', 'button');
    label.setAttribute('tabindex', '0');
    if (label.hasAttribute('hidden')) label.removeAttribute('hidden');
    if (vr && vr.hasAttribute('hidden')) vr.removeAttribute('hidden');
  }
  // copyText is THE copy primitive for the whole dashboard (session chip,
  // native-TLS fingerprint rows, relaunch command). The async Clipboard API
  // is intentionally NOT used: the dashboard can be served over a plain-HTTP
  // tunnel where that API is undefined (and the locked
  // TestNativeTLSPopover_Contracts bans it). The textarea + execCommand
  // select-to-copy fallback works in every context; the previous selection
  // is restored so user-select:all targets keep their visual selection.
  // Returns true when the copy command reported success so callers can
  // confirm via showToast (hoisted function declaration, defined below).
  function copyText(text){
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '0';
    ta.style.left = '0';
    ta.style.opacity = '0';
    ta.style.pointerEvents = 'none';
    document.body.appendChild(ta);
    var sel = document.getSelection();
    var prev = sel && sel.rangeCount > 0 ? sel.getRangeAt(0) : null;
    ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (_) {}
    document.body.removeChild(ta);
    if (prev && sel){ sel.removeAllRanges(); sel.addRange(prev); }
    return ok;
  }
  // Click / Enter / Space on the brandbar Claude-session chip copies the
  // full UUID and confirms with a toast (a silent copy is indistinguishable
  // from a dead control). The label element always exists (template renders
  // it, hidden when empty); the payload is read live from data-full.
  // Duplicated in the not-LiveEnabled ovf script block (ended pages render
  // the chip too); keep the two copies in sync.
  (function(){
    var el = document.getElementById('claude-sess-label'); if (!el) return;
    function copyClaude(){
      var full = el.getAttribute('data-full') || ''; if (!full) return;
      if (copyText(full)) showToast('session id copied', null);
    }
    el.addEventListener('click', copyClaude);
    el.addEventListener('keydown', function(e){
      if (e.target !== e.currentTarget) return; // only the chip itself, never a descendant
      if (e.key === 'Enter' || e.key === ' '){ e.preventDefault(); copyClaude(); }
    });
  })();
  var HDR_GROUPS = [
    ['Protocol & versioning', function(l){ return l.indexOf('anthropic-') === 0; }],
    ['Client identity', function(l){ return l === 'user-agent' || l.indexOf('x-stainless-') === 0 || l === 'x-app' || l === 'x-client-request-id' || l === 'x-claude-code-session-id'; }],
    ['Content negotiation', function(l){ return l === 'content-type' || l === 'accept' || l === 'accept-encoding'; }],
    ['Reliability', function(l){ return l === 'idempotency-key'; }],
    ['Credentials', null],
    ['Other', function(){ return true; }]
  ];
  function headerGroupsFor(h){
    if (!h) return null;
    var buckets = {};
    // When state.session.capture_bodies_unmasked is true
    // (CCWRAP_UNMASK_CREDENTIALS=1 at launch), credential headers render raw
    // with redacted=false. Single source for the unmask decision matches
    // headerAnnotation Go-side; both consult the same per-session flag so
    // SSE-live patch + server first-paint never drift mid-session.
    var unmask = !!(state.session && state.session.capture_bodies_unmasked);
    Object.keys(h).forEach(function(name){
      var lower = name.toLowerCase();
      var vals = h[name];
      var disp = Array.isArray(vals) ? vals.join(', ') : String(vals);
      var group = 'Other';
      var isCred = classifyHdr(name) === 'cred';
      if (isCred) {
        if (!unmask) disp = '‹redacted by ccwrap›';
        // unmask=true: keep disp as the raw joined value.
        group = 'Credentials';
      } else {
        for (var i = 0; i < HDR_GROUPS.length; i++){ var g = HDR_GROUPS[i]; if (g[1] && g[0] !== 'Other' && g[1](lower)) { group = g[0]; break; } }
      }
      // redacted flag drives the red-pill display. With unmask the value
      // is raw so there is no pill — redacted is false even for credential-
      // class headers.
      (buckets[group] = buckets[group] || []).push({ name: name, value: disp, redacted: isCred && !unmask });
    });
    var out = [];
    HDR_GROUPS.forEach(function(g){
      var rows = buckets[g[0]];
      if (!rows || !rows.length) return;
      rows.sort(function(a, b){ return a.name < b.name ? -1 : 1; });
      out.push({ name: g[0], rows: rows });
    });
    return out.length ? out : null;
  }
  // One collapsible "request headers" sub-accordion —
  // a summary line, role groups (stable order) as labels, mono rows,
  // credential value = neutral muted ‹redacted by ccwrap› + a neutral muted
  // "redacted" pill. Single-sourced deny-list (classifyHdr/HEADER_DENY);
  // the real value is NEVER emitted. Mirrors the Go first-paint markup.
  function headerPanelEl(row){
    if (row.headerGroups){
      var d = document.createElement('details'); d.className = 'req-sub';
      var sm = document.createElement('summary'); sm.textContent = 'request headers'; d.appendChild(sm);
      var p = document.createElement('div'); p.className = 'sub-body';
      var n = 0, red = 0;
      row.headerGroups.forEach(function(g){ g.rows.forEach(function(r){ n++; if (r.redacted) red++; }); });
      var sum = document.createElement('div'); sum.className = 'hdr-sum';
      function chip(txt, isRed){ var s = document.createElement('span'); if (isRed) s.className = 'red';
        var b = document.createElement('b'); b.textContent = String(txt.n);
        s.appendChild(document.createTextNode(txt.k + ' ')); s.appendChild(b); return s; }
      sum.appendChild(chip({k:'headers', n:n}, false));
      sum.appendChild(chip({k:'groups', n:row.headerGroups.length}, false));
      sum.appendChild(chip({k:'redacted', n:red}, true));
      p.appendChild(sum);
      row.headerGroups.forEach(function(g){
        var gd = document.createElement('div'); gd.className = 'hdr-group';
        var gn = document.createElement('div'); gn.className = 'hdr-group-name'; gn.textContent = g.name; gd.appendChild(gn);
        g.rows.forEach(function(r){
          var rr = document.createElement('div'); rr.className = 'hdr-row';
          var k = document.createElement('span'); k.className = 'hdr-k mono'; k.textContent = r.name;
          var v = document.createElement('span'); v.className = r.redacted ? 'hdr-v mono redv' : 'hdr-v mono'; v.textContent = r.value;
          if (r.redacted){ var pill = document.createElement('span'); pill.className = 'hdr-redpill'; pill.textContent = 'redacted'; v.appendChild(document.createTextNode(' ')); v.appendChild(pill); }
          rr.appendChild(k); rr.appendChild(v); gd.appendChild(rr);
        });
        p.appendChild(gd);
      });
      d.appendChild(p); return d;
    }
    return null;
  }
  // Builds the same DOM as the Go template's body-drawer,
  // appended as a NESTED child of the row's .reqinspect (alongside the
  // header sub-accordion) so the global toggle listener fetches and
  // renders it UNCHANGED. textContent only; data-reqid is a hex id but
  // still set via setAttribute, never interpolated into markup.
  function bodyDrawerEl(row){
    if (!row.bodyRefId) return null;
    var d=document.createElement('details'); d.className='req-sub body-drawer';
    d.setAttribute('data-reqid', row.bodyRefId);
    if (row.upstreamBodyRefId) d.setAttribute('data-upstream-reqid', row.upstreamBodyRefId);
    var sm=document.createElement('summary'); sm.textContent='request body'; d.appendChild(sm);
    var p=document.createElement('div'); p.className='sub-body body-panel'; p.textContent='full request body — loading…';
    d.appendChild(p); return d;
  }
  // respBodyDrawerEl builds the forwarded-row RESPONSE sub-drawer. A standard
  // .body-drawer (so the global toggle listener fetches data-reqid) PLUS a
  // resp-drawer marker so the listener renders via the SSE-aware
  // renderResponseView, not the request-shaped body view. Same DOM as the Go
  // template's resp-drawer branch. textContent/setAttribute only.
  function respBodyDrawerEl(row){
    if (!row.responseBodyRefId) return null;
    var d=document.createElement('details'); d.className='req-sub body-drawer resp-drawer';
    d.setAttribute('data-reqid', row.responseBodyRefId);
    var sm=document.createElement('summary'); sm.textContent='response body'; d.appendChild(sm);
    var p=document.createElement('div'); p.className='sub-body body-panel'; p.textContent='response body — loading…';
    d.appendChild(p); return d;
  }
  // teleBodyDrawerEl builds one telemetry sub-drawer (request or response).
  // A standard .body-drawer (so the global toggle listener fetches data-reqid)
  // PLUS a tele-drawer marker so the listener renders generic JSON, not the
  // Anthropic body view. Same DOM shape as the server template's telemetry
  // branch. textContent/setAttribute only.
  function teleBodyDrawerEl(label, id){
    var d=document.createElement('details'); d.className='req-sub body-drawer tele-drawer';
    d.setAttribute('data-reqid', id);
    var sm=document.createElement('summary'); sm.textContent=label; d.appendChild(sm);
    var p=document.createElement('div'); p.className='sub-body body-panel'; p.textContent=label + ' — loading…';
    d.appendChild(p); return d;
  }
  const liveChip = document.getElementById('live-chip');
  const liveLabel = document.getElementById('live-label');
  const lastActivityNode = document.getElementById('last-activity');
  const lastActivityCell = document.getElementById('last-activity-cell');
  const toggle = document.getElementById('live-toggle');
  function ribbonTrafficValue(){
    const cell = document.querySelector('[data-ribbon="Traffic"] [data-ribbon-value], [data-ribbon="Total"] [data-ribbon-value]');
    return cell || null;
  }
  var LIMITS = { activity: 50 };
  // RETAIN mirrors server.go maxSession{Requests,Errors,Trace}: state.requests/
  // errors/trace are a CLIENT MIRROR of the server's retention rings. Bootstrap
  // and /recent ship the full rings; SSE patchActivity trims back to these caps.
  // So refreshCounts (which counts state-array lengths) equals the server-
  // rendered filter counts on load, AND the arrays cannot grow without bound on
  // a long-lived session. Keep in lockstep with server.go (a test guards it).
  var RETAIN = { requests: 250, errors: 250, trace: 500 };

  const state = {
    session: boot.session || null,
    requests: Array.isArray(boot.requests) ? boot.requests.slice() : [],
    errors: Array.isArray(boot.errors) ? boot.errors.slice() : [],
    trace: Array.isArray(boot.trace) ? boot.trace.slice() : [],
  };
  let lastActivityAt = boot.last_activity || (lastActivityCell ? lastActivityCell.dataset.lastActivity : '');
  let es = null;
  let paused = false;
  let ended = false;
  let openedOnce = false;

  const esc = (value) => String(value == null ? '' : value)
    .replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;')
    .replaceAll('"','&quot;').replaceAll("'",'&#39;');

  function parseDate(value){
    if (!value) return null;
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? null : d;
  }
  function formatClock(value){
    const d = parseDate(value);
    if (!d) return '—';
    return d.toLocaleTimeString([], {hour12:false, hour:'2-digit', minute:'2-digit', second:'2-digit'});
  }
  function formatAge(value){
    const d = parseDate(value);
    if (!d) return 'idle';
    const diff = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    if (diff < 5) return 'just now';
    if (diff < 60) return diff + 's ago';
    if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
    if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
    return Math.floor(diff / 86400) + 'd ago';
  }
  function updateLastActivityLabel(){
    if (lastActivityNode) lastActivityNode.textContent = formatAge(lastActivityAt);
  }
  function setLastActivity(value){
    const next = parseDate(value);
    if (!next) return;
    const prev = parseDate(lastActivityAt);
    if (!prev || next.getTime() >= prev.getTime()) {
      lastActivityAt = next.toISOString();
      if (lastActivityCell) lastActivityCell.dataset.lastActivity = lastActivityAt;
      updateLastActivityLabel();
    }
  }
  function setLiveState(s, label){
    if (liveChip) liveChip.dataset.state = s;
    if (liveLabel) liveLabel.textContent = label;
    // Refresh is a manual full-reload fallback. With a healthy live stream it
    // is redundant (SSE patches the ribbon + activity and auto-resyncs on
    // reconnect), so surface it only when the stream is unhealthy or gone —
    // its real use: recover a dead/unsupported connection, or reload an ended
    // session. Hidden on connected/connecting/paused; shown on error/
    // reconnecting/ended. (Non-live ended sessions render it visible since
    // this script never runs.)
    var rb = document.getElementById('refresh-btn');
    if (rb) rb.style.display = (s === 'error' || s === 'reconnecting' || s === 'ended') ? '' : 'none';
  }
  function bumpUpdates(payloadTime){
    // post-Option-C: live-count DOM removed; only the last-activity
    // timestamp display still needs refreshing on each SSE event.
    setLastActivity(payloadTime);
  }

  function shortSession(id){ return id && id.length > 8 ? id.slice(0, 8) : (id || ''); }
  function routeLabel(value){
    switch (String(value || '').toLowerCase()) {
      case 'explicit': return 'Explicit';
      case 'inherited_env': return 'From environment';
      case 'fallback_default': return 'Default';
      default: return value || 'Unknown';
    }
  }
  function authLabel(mode, source){
    switch (String(mode || '').toLowerCase()) {
      case 'passthrough': return 'Passthrough';
      case 'override-x-api-key': return 'X-API-Key';
      case 'override-authorization-bearer':
        return String(source || '') === 'CLAUDE_CODE_OAUTH_TOKEN' ? 'OAuth token' : 'Bearer token';
      case 'unsupported': return 'Unsupported';
      default: return mode || 'Unknown';
    }
  }
  function egressLabel(mode, source, summary){
    const cleanSummary = String(summary || '').trim();
    const lowerMode = String(mode || '').toLowerCase();
    if (!lowerMode || lowerMode === 'direct' || lowerMode === 'none') return 'Direct';
    let prefix = '';
    switch (String(source || '').toLowerCase()) {
      case 'inherited_env': prefix = 'From environment'; break;
      case 'claude_settings': prefix = 'Claude settings'; break;
      case 'explicit_flag': prefix = 'Explicit'; break;
      case 'none': case '': prefix = ''; break;
      default:
        prefix = String(source || '').replaceAll('_',' ').replaceAll('-',' ');
        prefix = prefix.replace(/\b\w/g, ch => ch.toUpperCase());
    }
    if (!cleanSummary || cleanSummary.toLowerCase() === 'direct') return prefix || 'Direct';
    return prefix ? (prefix + ' · ' + cleanSummary) : cleanSummary;
  }

  // --- row builders (must match server-side Go formatters visually) ---

  function joinParts(parts){
    return parts.filter(p => p != null && String(p).trim() !== '').join(' · ');
  }
  function methodLabel(rec){
    if (rec.synthetic) return 'SYNTHETIC';
    return rec.method || 'REQUEST';
  }
  // headerAnnotate mirrors Go headerAnnotation (expandability
  // rule) for the live activity feed. classifyHdr (used by
  // headerGroupsFor) is single-sourced from the bootstrap deny-list.
  function headerAnnotate(d, rec){
    var rh = rec.request_headers;
    if (rh && Object.keys(rh).length) { d.headerGroups = headerGroupsFor(rh); }
    else if (rec.synthetic) { d.headerNote = 'CCWRAP-generated, not Claude Code traffic'; }
    else if ((rec.method || '') === 'CONNECT') { d.headerNote = 'encrypted tunnel — not intercepted; no headers visible'; }
    else { d.headerNote = 'no headers recorded'; }
    return d;
  }
  // ONE row builder, keyed by the Go-supplied class. It
  // mirrors unifiedActivityRows: every row carries class; kind is
  // derived from class; drawer wiring (headerGroups/headerNote/
  // bodyRefId) is set ONLY on forwarded-api rows — exactly like the
  // server first-paint, so live rows are byte-shape-identical.
  function activityRowData(rec, cls){
    var kind = cls === 'error' ? 'error' : cls === 'trace' ? 'trace' : 'request';
    if (kind === 'error') {
      return {
        ts: rec.timestamp,
        time: formatClock(rec.timestamp),
        label: 'error',
        main: rec.summary || 'proxy error',
        right: joinParts([rec.error_class || rec.severity, rec.upstream_host || rec.suggested_action || 'check ccwrap doctor']),
        kind: 'error',
        class: 'error',
      };
    }
    if (kind === 'trace') {
      var td = {
        ts: rec.timestamp,
        time: formatClock(rec.timestamp),
        label: 'trace',
        main: rec.summary || 'activity',
        right: joinParts([rec.category || 'trace', rec.detail || '—']),
        kind: 'trace',
        class: 'trace',
        category: rec.category || '',
      };
      if (rec.category === 'profile_switch') {
        try {
          var parsed = JSON.parse(rec.detail || '{}');
          td.switchFrom = String(parsed.from || '');
          td.switchFromProvider = String(parsed.from_provider || '');
          td.switchTo = String(parsed.to || '');
          td.switchToProvider = String(parsed.to_provider || '');
          td.switchClass = String(parsed.class || '');
          td.switchRequested = String(parsed.requested || '');
          td.switchReason = String(parsed.reason || '');
        } catch(_) { /* fall through with raw right cell */ }
      }
      return td;
    }
    var d = {
      ts: rec.timestamp,
      time: formatClock(rec.timestamp),
      label: cls,
      main: methodLabel(rec) + ' ' + (rec.path || '/'),
      right: joinParts([String(rec.status_code ?? ''), (rec.latency_ms || 0) + ' ms', rec.stream_state, rec.actual_upstream_host || rec.logical_target_host]),
      st: statusTier(rec.status_code),
      mono: true,
      forwarded: cls === 'forwarded-api',
      kind: 'request',
      class: cls,
    };
    // Drawer wiring is forwarded-api only (mirror the Go builder).
    if (cls === 'forwarded-api') {
      headerAnnotate(d, rec);
      // Request-only — the SSE record carries body_ref
      // (model.RequestBodyRef); copy ONLY its id, mirroring the
      // server template's BodyRefID.
      d.bodyRefId = (rec.body_ref && rec.body_ref.id) ? rec.body_ref.id : '';
      d.upstreamBodyRefId = (rec.upstream_body_ref && rec.upstream_body_ref.id) ? rec.upstream_body_ref.id : '';
      // Captured RESPONSE body — copy ONLY the id, mirroring the server
      // template's ResponseBodyRefID; rendered via the SSE-aware resp-drawer.
      d.responseBodyRefId = (rec.response_body_ref && rec.response_body_ref.id) ? rec.response_body_ref.id : '';
      // Forwarded-api rows carry the active profile at
      // request-time so prependRow can stamp data-profile-* attrs and
      // decorateProfileAnnotation can paint the hash-hued chip.
      d.profileName = String(rec.active_profile_name || '');
      d.profileProvider = String(rec.active_profile_provider || '');
    }
    // Telemetry CONNECTs carry no headers but DO carry captured request +
    // response bodies; copy ONLY the ids, mirroring the server template's
    // BodyRefID/ResponseBodyRefID. telemetryDrawer makes the row expandable.
    if (cls === 'telemetry') {
      d.bodyRefId = (rec.body_ref && rec.body_ref.id) ? rec.body_ref.id : '';
      d.responseBodyRefId = (rec.response_body_ref && rec.response_body_ref.id) ? rec.response_body_ref.id : '';
      d.telemetryDrawer = !!(d.bodyRefId || d.responseBodyRefId);
    }
    return d;
  }

  // --- DOM patch helpers ---

  const ROW_HEAD_HTML = '<div class="row-head" aria-hidden="true"><div></div><div>Time</div><div>Kind</div><div>Summary</div><div>Details</div></div>';

  // Row elements (cell/rowCells/makeRowEl) are built with createElement +
  // textContent/setAttribute only (no innerHTML) — injection-safe; the legacy
  // esc()+innerHTML row template was removed during the rewrite.
  function cell(spanTag, cls, txt, title){
    var e=document.createElement(spanTag); e.className=cls;
    if (title!=null) e.setAttribute('title', title);
    e.textContent = txt==null ? '' : txt; return e;
  }
  function rowCells(row, spanTag){
    var c=[ cell(spanTag,'rowchev','',null), cell(spanTag,'cell-time',row.time||'—',null),
            cell(spanTag,'cell-label',row.label||'',row.label||''),
            cell(spanTag,'cell-main'+(row.mono?' mono':''),row.main||'',null),
            cell(spanTag,'cell-right',row.right||'',null) ];
    c[0].setAttribute('aria-hidden','true'); return c;
  }
  // Stamp profile + switch-marker data-attrs on the new
  // row element so first-paint sweeps and SSE-append hooks can find
  // them via querySelectorAll('.row[data-profile-name]') and
  // '.row[data-category="profile_switch"]'. Pure setAttribute (no
  // innerHTML) — the decorators read these and paint via textContent.
  function stampSP3DataAttrs(el, row){
    if (!el || !row) return;
    if (row.profileName) el.setAttribute('data-profile-name', row.profileName);
    if (row.profileProvider) el.setAttribute('data-profile-provider', row.profileProvider);
    if (row.category) el.setAttribute('data-category', row.category);
    if (row.switchFrom) el.setAttribute('data-switch-from', row.switchFrom);
    if (row.switchFromProvider) el.setAttribute('data-switch-from-provider', row.switchFromProvider);
    if (row.switchTo) el.setAttribute('data-switch-to', row.switchTo);
    if (row.switchToProvider) el.setAttribute('data-switch-to-provider', row.switchToProvider);
    if (row.switchClass) el.setAttribute('data-switch-class', row.switchClass);
    if (row.switchRequested) el.setAttribute('data-switch-requested', row.switchRequested);
    if (row.switchReason) el.setAttribute('data-switch-reason', row.switchReason);
    if (row.st) el.setAttribute('data-st', row.st);
  }
  // statusTier mirrors the Go statusTier: 5xx -> 'err', 4xx -> 'warn', else ''.
  function statusTier(code){
    code = Number(code) || 0;
    if (code >= 500 && code < 600) return 'err';
    if (code >= 400 && code < 500) return 'warn';
    return '';
  }
  function makeRowEl(row){
    if (row.headerGroups){
      var d=document.createElement('details'); d.className='row'; d.setAttribute('role','listitem');
      if (row.ts) d.dataset.ts=row.ts;
      d.setAttribute('data-forwarded','true');
      if (row.kind) d.setAttribute('data-kind',row.kind);
      if (row.class) d.setAttribute('data-class',row.class);
      stampSP3DataAttrs(d, row);
      var sm=document.createElement('summary');
      rowCells(row,'span').forEach(function(c){ sm.appendChild(c); }); d.appendChild(sm);
      var insp=document.createElement('div'); insp.className='reqinspect';
      var hp=headerPanelEl(row); if (hp) insp.appendChild(hp);
      var bd=bodyDrawerEl(row); if (bd) insp.appendChild(bd);
      var rbd=respBodyDrawerEl(row); if (rbd) insp.appendChild(rbd);
      d.appendChild(insp); return d;
    }
    // Telemetry CONNECT with captured bodies: expandable row with up to two
    // .body-drawer.tele-drawer sub-details (req/resp), rendered via the
    // generic JSON view. Same DOM shape as the server template's telemetry
    // branch.
    if (row.telemetryDrawer){
      var dt=document.createElement('details'); dt.className='row'; dt.setAttribute('role','listitem');
      if (row.ts) dt.dataset.ts=row.ts;
      dt.setAttribute('data-forwarded','false');
      if (row.kind) dt.setAttribute('data-kind',row.kind);
      if (row.class) dt.setAttribute('data-class',row.class);
      stampSP3DataAttrs(dt, row);
      var smt=document.createElement('summary');
      rowCells(row,'span').forEach(function(c){ smt.appendChild(c); }); dt.appendChild(smt);
      var inspt=document.createElement('div'); inspt.className='reqinspect';
      if (row.bodyRefId) inspt.appendChild(teleBodyDrawerEl('request body', row.bodyRefId));
      if (row.responseBodyRefId) inspt.appendChild(teleBodyDrawerEl('response body', row.responseBodyRefId));
      dt.appendChild(inspt); return dt;
    }
    var el=document.createElement('div'); el.className='row nf'; el.setAttribute('role','listitem');
    if (row.ts) el.dataset.ts=row.ts;
    if (row.forwarded) el.setAttribute('data-forwarded','true');
    if (row.kind) el.setAttribute('data-kind',row.kind);
    if (row.class) el.setAttribute('data-class',row.class);
    stampSP3DataAttrs(el, row);
    rowCells(row,'div').forEach(function(c){ el.appendChild(c); });
    if (row.headerNote){ var n=document.createElement('div'); n.className='nf-note'; n.textContent=row.headerNote; el.appendChild(n); }
    return el;
  }

  function ensureRowsSkeleton(bodyEl){
    if (!bodyEl) return null;
    let rows = bodyEl.querySelector(':scope > .rows');
    if (!rows) {
      bodyEl.innerHTML = '';
      rows = document.createElement('div');
      rows.className = 'rows';
      rows.setAttribute('role', 'list');
      rows.innerHTML = ROW_HEAD_HTML;
      bodyEl.appendChild(rows);
    } else if (!rows.querySelector(':scope > .row-head')) {
      rows.insertAdjacentHTML('afterbegin', ROW_HEAD_HTML);
    }
    return rows;
  }

  function prependRow(bodyEl, row, limit){
    const rows = ensureRowsSkeleton(bodyEl);
    if (!rows) return;
    const el = makeRowEl(row); // ONE element per row (forwarded → <details class="row">, else <div class="row nf">)
    const head = rows.querySelector(':scope > .row-head');
    if (head && head.nextSibling) rows.insertBefore(el, head.nextSibling);
    else if (head) rows.appendChild(el);
    else rows.insertBefore(el, rows.firstChild);
    // Decorate the freshly-inserted row. Both helpers are
    // idempotent (.ann/.sp3-switch-rendered guards) and cheap, so it's
    // fine to call unconditionally — they no-op when data-attrs are absent.
    decorateProfileAnnotation(el);
    renderSwitchMarker(el);
    // INVARIANT: every Activity entry is exactly
    // ONE element with class "row" (a <details> for forwarded-api, else a
    // <div>); header/body drawers are NESTED inside it, never siblings.
    // The trim is a one-element-per-row slice — the contiguous-sibling-run
    // orphan bug class is structurally gone. Do NOT reintroduce sibling
    // drawers or a sibling-run while-trim.
    const children = rows.querySelectorAll(':scope > .row');
    for (let i = limit; i < children.length; i++) children[i].remove();
    // Whitelist self-heal: every direct child of
    // rows must be class "row-head" or "row"; remove any other (a sibling-
    // orphaned drawer from any mis-construction, spelling-independent —
    // whitelist, NOT a drawer-class blacklist). Static snapshot so removal
    // doesn't skip live-collection siblings. Pure .remove()+classList.
    const _kept = Array.prototype.slice.call(rows.children);
    for (let _i = 0; _i < _kept.length; _i++) {
      const _k = _kept[_i];
      if (!(_k.classList.contains('row') || _k.classList.contains('row-head'))) _k.remove();
    }
  }

  // --- ONE filter-aware Activity patcher ---
  // The four per-section patchers collapsed into this single path.
  // class is Go-supplied (rec.class on the SSE record); JS never
  // re-derives it. The filter is read from the #activity-filter bar
  // (which owns the bar's click/default/show-more behavior).

  function activeFilter(){
    var on = document.querySelector('#activity-filter .filter-btn.on');
    return on ? on.getAttribute('data-filter') : 'forwarded-api';
  }
  function refreshCounts(){
    var c = { 'all':0,'forwarded-api':0,'synthetic':0,'tunnel':0,'telemetry':0,'error':0,'trace':0 };
    (state.requests||[]).forEach(function(r){ c[(r && r.class) || 'forwarded-api']++; });
    c.error += (state.errors||[]).length; c.trace += (state.trace||[]).length;
    c.all = (state.requests||[]).length + (state.errors||[]).length + (state.trace||[]).length;
    var btns = document.querySelectorAll('#activity-filter .filter-btn');
    for (var i=0;i<btns.length;i++){ var f=btns[i].getAttribute('data-filter'); var n=btns[i].querySelector('.filter-n'); if(n) n.textContent = String(c[f]||0); }
  }
  // Filter-aware show-more visibility: total is the count of
  // RETAINED state entries matching the active filter (all when
  // 'all'); shown is the rendered .row count. The control appears only
  // when the active-class window has more retained rows than are
  // currently displayed — so "show more" deepens the active-class
  // window, never an all-class slice. Top-level (reachable from
  // patchActivity/rebuildActivityFromState); the #activity-more node is
  // looked up lazily so it is safe before/without the show-more block.
  function syncMore(){
    var moreWrap = document.getElementById('activity-more');
    if (!moreWrap) return;
    var f = activeFilter();
    var total;
    if (f === 'all') {
      total = (state.requests||[]).length + (state.errors||[]).length + (state.trace||[]).length;
    } else if (f === 'error') {
      total = (state.errors||[]).length;
    } else if (f === 'trace') {
      total = (state.trace||[]).length;
    } else {
      total = 0;
      (state.requests||[]).forEach(function(r){ if (((r && r.class) || 'forwarded-api') === f) total++; });
    }
    var shown = document.querySelectorAll('#activity-body .rows > .row').length;
    moreWrap.hidden = !(total > shown);
  }
  function patchActivity(rec, cls){
    // Mirror the server retention ring: unshift then trim to RETAIN so the
    // state arrays stay bounded (no unbounded growth on a long session) and
    // refreshCounts keeps matching the server-rendered counts.
    if (cls === 'error') { state.errors.unshift(rec); if (state.errors.length > RETAIN.errors) state.errors.length = RETAIN.errors; }
    else if (cls === 'trace') { state.trace.unshift(rec); if (state.trace.length > RETAIN.trace) state.trace.length = RETAIN.trace; }
    else { state.requests.unshift(rec); if (state.requests.length > RETAIN.requests) state.requests.length = RETAIN.requests; if (state.session) state.session.recent_request_count = (state.session.recent_request_count||0)+1; }
    // A newly-arrived row that does not match the active
    // filter is NOT shown but still counted. Insert a visible row ONLY
    // when it matches (or filter is 'all'); the homogeneous active-
    // class visible list makes prependRow's newest-N contiguous-sibling
    // trim exactly correct. No hide-after pass — filtering is what is
    // BUILT, not a CSS-hidden row (filter-aware capping).
    var f = activeFilter();
    var activityBody = document.getElementById('activity-body');
    if (activityBody && (f === 'all' || cls === f)) {
      prependRow(activityBody, activityRowData(rec, cls), LIMITS.activity);
    }
    refreshCounts();
    if (cls !== 'error' && cls !== 'trace') {
      updateTrafficCell();
      // Mirror the server invariant (recordRequest): a successful request heals
      // Health to ok EXCEPT while native-TLS is blocked. A request that
      // completed over a still-pooled good conn must NOT heal away the block
      // before a fresh mirror dial resumes, or the hero would show Active while
      // new connections are being fail-closed. state.session.native_tls ===
      // 'blocked' is the JS twin of sess.nativeTLSBlocked.
      if (state.session && state.session.native_tls !== 'blocked') {
        state.session.session_health = 'ok';
        updateHeroState(state.session);
      }
    } else if (cls === 'error' && state.session) {
      state.session.recent_error_count = (state.session.recent_error_count||0)+1;
      updateTrafficCell();
      var sev = rec && rec.severity;
      var h = (sev === 'warn' || (!sev && rec && rec.error_class === 'ccwrap_auth_missing')) ? 'warn' : 'error';
      state.session.session_health = h;
      updateHeroState(state.session);
    }
    syncMore();
  }

  // The 9-item summary grid was replaced by the hero + 4-cell ribbon.
  // The patcher reflects SSE session_updated in the Profile chip and
  // Route/Auth/Models cells without a full reload.
  function patchSession(session){
    if (!session) return;
    state.session = session;
    updateTrafficCell();
    updateHeroState(session);
    updateProfileCell();
    updateRouteCell();
    updateAuthCell();
    updateModelsCell();
    updateBodiesCell();
    updateEgressCell();
    updateNativeTLSCell();
  }
  // updateHeroState mirrors webHeroVariant (Go) so an SSE session_updated
  // repaints the hero big-word + variant live, not just on full reload.
  function updateHeroState(session){
    if (!session) return;
    var hero = document.getElementById('hero');
    var word = document.getElementById('hero-state');
    if (!hero || !word) return;
    var state, variant;
    if (session.state === 'ended') { state = 'Ended'; variant = 'ended'; }
    else {
      var h = session.session_health || 'ok';
      if (h === 'error') { state = 'Error'; variant = 'error'; }
      else if (h === 'warn') { state = 'Degraded'; variant = 'degraded'; }
      else { state = 'Active'; variant = 'active'; }
    }
    word.textContent = state;
    hero.setAttribute('data-variant', variant);
    // Favicon mirrors the variant (Go twin: faviconHref — keep the color
    // maps in sync) so a degraded/error session reads at tab level.
    var icon = document.querySelector('link[rel="icon"]');
    if (icon) {
      var fc = variant === 'degraded' ? '%23fbbf24' : variant === 'error' ? '%23f43f5e' : variant === 'ended' ? '%238f8f8f' : '%2310b981';
      icon.setAttribute('href', "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Ccircle cx='8' cy='8' r='5' fill='" + fc + "'/%3E%3C/svg%3E");
    }
  }
  // updateEgressCell mirrors egressCellPresentation (Go) so an SSE
  // session_updated repaints the Egress ribbon cell after a live profile
  // switch. Without it the cell shows stale exit info until a full reload —
  // a routing/security tool misreporting where traffic actually exits.
  // Touches ONLY the value (.v) and detail (.d)
  // elements so the runtime-attached probe button + result line survive.
  function updateEgressCell(){
    if (!state.session) return;
    var cell = ribbonCellEl('Egress');
    if (!cell) return;
    var v = cell.querySelector('[data-ribbon-value]');
    if (!v) return;
    var mode = String(state.session.egress_mode || '').trim().toLowerCase();
    var summary = String(state.session.egress_summary || '').trim();
    var value, mono = false;
    if (mode === '' || mode === 'direct' || mode === 'none') {
      value = 'Direct';
    } else if (summary !== '') {
      value = summary; mono = true;
    } else {
      value = mode;
    }
    v.textContent = value;
    v.title = value; // full value on hover (the cell nowrap+ellipsises long URLs)
    v.classList.toggle('mono', mono);
    v.classList.remove('muted'); // egress value is never empty
    var detail = '';
    switch (String(state.session.egress_source || '').trim().toLowerCase()) {
      case 'inherited_env': detail = 'from environment'; break;
      case 'claude_settings': detail = 'claude settings'; break;
      case 'explicit_flag': detail = 'explicit'; break;
    }
    var d = cell.querySelector('.d');
    if (detail) {
      if (!d) {
        // First paint had no source → no .d element; create one right after
        // the value so the source label can appear on a later switch.
        d = document.createElement('div');
        d.className = 'd';
        if (v.nextSibling) v.parentNode.insertBefore(d, v.nextSibling);
        else v.parentNode.appendChild(d);
      }
      d.textContent = detail;
    } else if (d) {
      d.textContent = '';
    }
  }
  function ribbonValueEl(label){
    var cell = document.querySelector('.ribbon-cell[data-ribbon="' + label + '"]');
    return cell ? cell.querySelector('[data-ribbon-value]') : null;
  }
  function ribbonDetailEl(label){
    var cell = document.querySelector('.ribbon-cell[data-ribbon="' + label + '"]');
    return cell ? cell.querySelector('.d') : null;
  }
  function ribbonCellEl(label){
    return document.querySelector('.ribbon-cell[data-ribbon="' + label + '"]');
  }
  function updateProfileCell(){
    if (!state.session) return;
    var cell = ribbonCellEl('Profile');
    if (!cell) return;
    var v = cell.querySelector('[data-ribbon-value]');
    var d = cell.querySelector('.d');
    var name = state.session.active_profile_name || '';
    var provider = state.session.active_profile_provider || '';
    var aliasCount = state.session.model_alias_count || 0;
    // Rebuild chip via textContent + DOM construction (NO innerHTML).
    if (v) {
      v.textContent = '';
      var chip = document.createElement('span');
      var chipName = name || 'inherit-env';
      chip.className = name ? 'sp3-chip' : 'sp3-chip sp3-chip-inherit ' +
        (cell.dataset.state === 'inherit-env-clickable' ? 'sp3-chip-inherit-clickable' : 'sp3-chip-inherit-static');
      var dot = document.createElement('span'); dot.className = 'sp3-chip-dot'; chip.appendChild(dot);
      chip.appendChild(document.createTextNode(' ' + chipName + ' '));
      var caret = document.createElement('span'); caret.className = 'sp3-chip-caret'; caret.textContent = '▾';
      chip.appendChild(caret);
      v.appendChild(chip);
    }
    if (d) {
      if (name) {
        var aliasPart = aliasCount === 1 ? ' · 1 alias' : aliasCount > 1 ? ' · ' + aliasCount + ' aliases' : '';
        d.textContent = provider + aliasPart;
      }
      // For inherit states, leave the detail line as the server-rendered one
      // (page-render-time stat'd profiles.json count); SSE doesn't carry it.
    }
    // data-state can flip between active <-> inherit-env-clickable as the
    // session switches; static-vs-clickable cannot change post-load (depends
    // on profiles.json presence, which is a page-render snapshot).
    if (name) cell.dataset.state = 'active';
    else if (cell.dataset.state === 'active') cell.dataset.state = 'inherit-env-clickable';
  }
  function updateRouteCell(){
    if (!state.session) return;
    var v = ribbonValueEl('Route');
    if (!v) return;
    var rc = state.session.route_class || '';
    // Map model.RouteClass* enum values to short human labels — same set
    // ui.HumanRouteClass produces server-side; defensive default echoes the
    // raw value.
    var map = {
      'first_party': 'first-party', 'first_party_passthrough': 'first-party',
      'third_party_hidden': 'third-party (hidden)', 'third_party_explicit': 'third-party'
    };
    v.textContent = map[rc] || rc || '—';
  }
  // updateAuthCell mirrors authCellPresentation in Go.
  // Two states: healthy (data-state cleared, value=auth_policy) and
  // auth-missing (data-state="auth-missing", value="⚠ MISSING", detail
  // names the env or "no auth source configured").
  function updateAuthCell(){
    if (!state.session) return;
    var cell = ribbonCellEl('Auth');
    if (!cell) return;
    var v = cell.querySelector('[data-ribbon-value]');
    var d = cell.querySelector('.d');
    var missing = (state.session.auth_bootstrap === 'missing');
    if (missing) {
      var profile = state.session.active_profile_name || 'inherit-env';
      var env = state.session.missing_auth_env || '';
      var detailMsg = env
        ? ('profile "' + profile + '" needs $' + env)
        : ('profile "' + profile + '" has no auth source configured');
      if (v) v.textContent = '⚠ MISSING';
      if (d) d.textContent = detailMsg;
      cell.dataset.state = 'auth-missing';
      return;
    }
    if (v) v.textContent = state.session.auth_policy || '—';
    // Restore Auth's detail to the historical HumanAuthBootstrap value on
    // recovery (Missing → not-missing transition via profile switch).
    // The full enum mapping is server-side only; JS uses a simplified
    // mapping for the common cases — first-paint Detail is authoritative,
    // SSE patches only clean up the chip.
    if (d) {
      var bs = state.session.auth_bootstrap || '';
      var bsKind = state.session.auth_bootstrap_kind || '';
      d.textContent = bs === 'placeholder_active'
        ? ('placeholder injected · ' + bsKind)
        : (bs === 'not_needed' ? '' : bs);
    }
    if (cell.dataset.state === 'auth-missing') cell.dataset.state = '';
  }
  // updateModelsCell mirrors webRibbonFromSession's Models cell logic so
  // SSE session_updated patches both the value text AND the
  // aliases-active data-state (which controls click affordance for the
  // mapping popover). Zero-count → "default" label and
  // no data-state; non-zero → "N alias(es)" + aliases-active + caret.
  function updateModelsCell(){
    if (!state.session) return;
    var cell = ribbonCellEl('Models');
    if (!cell) return;
    var v = cell.querySelector('[data-ribbon-value]');
    var n = state.session.model_alias_count || 0;
    if (v) {
      v.textContent = n === 1 ? '1 alias' : n > 1 ? n + ' aliases' : 'default';
    }
    if (n > 0) {
      cell.dataset.state = 'aliases-active';
    } else if (cell.dataset.state === 'aliases-active') {
      // Clear; closing any open Models popover at the same time so the
      // user isn't staring at stale alias mappings after a clear.
      cell.dataset.state = '';
      if (typeof closeModelsPop === 'function') closeModelsPop();
    }
  }
  // updateBodiesCell mirrors the Go bodiesCellPresentation matrix EXACTLY
  // (byte-equal value/detail/state) so the SSE live patch never drifts from
  // the server first paint. session.capture_bodies / .capture_bodies_unmasked
  // / .capture_telemetry are the source of truth (omitempty in wire —
  // absent == false). The cell summarizes two toggles — request bodies
  // (capture_bodies) and telemetry bodies (capture_telemetry).
  // 3 states: off / on / on-UNMASKED (red, CCWRAP_UNMASK_CREDENTIALS=1).
  function updateBodiesCell(){
    if (!state.session) return;
    var cell = ribbonCellEl('Bodies');
    if (!cell) return;
    var v = cell.querySelector('[data-ribbon-value]');
    var d = cell.querySelector('.d');
    var on = !!state.session.capture_bodies;
    var unmasked = !!state.session.capture_bodies_unmasked;
    var tel = !!state.session.capture_telemetry;
    if (!on && !tel) {
      if (v) v.textContent = 'off';
      if (d) d.textContent = 'click to choose what to capture';
      cell.dataset.state = 'bodies-off';
      return;
    }
    var parts = [];
    if (on) parts.push('request');
    if (tel) parts.push('telemetry');
    var value = parts.join(' + ');
    if (on && unmasked) {
      if (v) v.textContent = value + ' ⚠';
      if (d) d.textContent = 'UNMASKED — CCWRAP_UNMASK_CREDENTIALS=1; credentials in drawer + spill';
      cell.dataset.state = 'bodies-unmasked';
      return;
    }
    var detail;
    if (on && tel) {
      detail = 'recording request + response + telemetry bodies (credentials redacted)';
    } else if (on) {
      detail = 'recording request + response bodies (credentials redacted)';
    } else {
      detail = 'capturing telemetry bodies (Datadog/Sentry)';
    }
    if (v) v.textContent = value;
    if (d) d.textContent = detail;
    cell.dataset.state = 'bodies-on';
  }
  // updateNativeTLSCell mirrors the Go nativeTLSCellPresentation matrix EXACTLY
  // (byte-equal value/detail/state) so the SSE live patch never drifts from the
  // server first paint. state.session.native_tls is the source of truth:
  // off (or absent) / active / 'blocked: <reason>'. The cell only exists in
  // the DOM when the feature is in use, so this no-ops for non-opted-in
  // sessions; for an opted-in session it patches active<->blocked live.
  function updateNativeTLSCell(){
    if (!state.session) return;
    var cell = ribbonCellEl('NATIVE TLS');
    if (!cell) return;
    var v = cell.querySelector('[data-ribbon-value]');
    var d = cell.querySelector('.d');
    var nt = String(state.session.native_tls || '');
    var value, detail, dataState;
    if (nt === '' || nt === 'off') {
      value = 'off'; detail = 'stdlib TLS (default)'; dataState = 'native-off';
    } else if (nt === 'active') {
      value = 'active';
      var fb = Number(state.session.native_tls_fallbacks) || 0;
      if (fb > 0) {
        detail = 'mirroring · ' + fb + ' prior block(s)';
      } else if (state.session.native_tls_loaded) {
        detail = 'mirroring loaded fingerprint';
      } else {
        detail = 'mirroring Claude Code TLS fingerprint';
      }
      dataState = 'native-active';
    } else {
      value = 'blocked';
      detail = nt.indexOf('blocked: ') === 0 ? nt.slice('blocked: '.length) : nt;
      dataState = 'native-blocked';
    }
    if (v) v.textContent = value;
    if (d) d.textContent = detail;
    cell.dataset.state = dataState;
  }

  function updateTrafficCell(){
    if (!state.session) return;
    const v = ribbonTrafficValue();
    if (!v) return;
    const text = (state.session.recent_request_count || 0) + ' · ' + (state.session.recent_error_count || 0);
    if (v.textContent !== text) v.textContent = text;
  }

  // --- SSE wiring ---

  function markConnected(){
    if (paused || ended) return;
    if (!liveChip || liveChip.dataset.state === 'connected') return;
    setLiveState('connected', 'connected');
  }

  // Canonical filter-aware render of the SINGLE Activity list (also
  // the reconnect path). class is the Go-supplied
  // rec.class — JS never re-derives it; errors and trace are
  // constant-classed. Merge the FULL retained state, keep only the
  // active-class subset (all when 'all'), sort newest-first with the
  // same idiom as the Go unifiedActivityRows so order matches first
  // paint, THEN cap to LIMITS.activity. Filtering operates over the
  // full retained set BEFORE the cap (not slice-then-hide), so a
  // Forwarded API /v1/messages row is reachable even if >50 noise rows
  // arrived after it — it can never be buried.
  function rebuildActivityFromState(){
    var activityBody = document.getElementById('activity-body');
    if (!activityBody) return;
    var f = activeFilter();
    var rows = [];
    (state.requests||[]).forEach(function(rec){ rows.push({ts: rec.timestamp, cls: (rec && rec.class) || 'forwarded-api', rec: rec}); });
    (state.errors||[]).forEach(function(rec){ rows.push({ts: rec.timestamp, cls: 'error', rec: rec}); });
    (state.trace||[]).forEach(function(rec){ rows.push({ts: rec.timestamp, cls: 'trace', rec: rec}); });
    if (f !== 'all') rows = rows.filter(function(r){ return r.cls === f; });
    rows.sort(function(a, b){ return new Date(b.ts || 0) - new Date(a.ts || 0); });
    var top = rows.slice(0, LIMITS.activity);
    activityBody.textContent = '';
    if (top.length === 0) {
      var empty = document.createElement('div');
      empty.className = 'empty';
      empty.textContent = 'No recent traffic for this session yet — requests appear here live as Claude Code sends them.';
      activityBody.appendChild(empty);
      refreshCounts();
      syncMore();
      return;
    }
    for (var i = top.length - 1; i >= 0; i--) {
      prependRow(activityBody, activityRowData(top[i].rec, top[i].cls), LIMITS.activity);
    }
    // No post-rebuild per-row hide pass: the built list IS exactly the
    // filtered, newest-LIMITS.activity window (filter-aware capping).
    refreshCounts();
    syncMore();
  }

  async function resyncFromRecent(){
    try {
      const resp = await fetch('/recent', {headers:{'Accept':'application/json'}, cache:'no-store'});
      if (!resp.ok) return;
      const data = await resp.json();
      state.session = data.session || state.session;
      state.requests = Array.isArray(data.requests) ? data.requests.slice(0, RETAIN.requests) : [];
      state.errors = Array.isArray(data.errors) ? data.errors.slice(0, RETAIN.errors) : [];
      state.trace = Array.isArray(data.trace) ? data.trace.slice(0, RETAIN.trace) : [];
      patchSession(state.session);
      rebuildActivityFromState();
      // Replay the brandbar chip over recovered requests (oldest->newest) so a
      // conversation id that first appeared while we were disconnected is shown
      // — the live 'request' listener updates the chip per event, so the
      // reconnect path must too. updateClaudeSession is idempotent (data-full
      // short-circuit), so the repeated calls settle on the newest id once.
      for (var ci = state.requests.length - 1; ci >= 0; ci--) { updateClaudeSession(state.requests[ci]); }
    } catch (_) {}
  }

  function connectLive(){
    if (paused || ended) return;
    if (!window.EventSource) {
      setLiveState('error', 'unsupported');
      return;
    }
    if (es) es.close();
    setLiveState('connecting', 'connecting');
    const eventsURL = boot.events_url || '/events';
    es = new EventSource(eventsURL);
    es.onopen = () => {
      markConnected();
      updateLastActivityLabel();
      if (openedOnce) {
        // Reconnect path: the server may have dropped events while we were
        // offline (buffer evicted us) or during the TCP gap; re-sync state
        // from /recent so incremental patches keep a correct baseline.
        resyncFromRecent();
      }
      openedOnce = true;
    };
    es.onerror = () => {
      if (paused || ended) return;
      if (es && es.readyState === 2) {
        setLiveState('error', 'disconnected');
      } else {
        setLiveState('reconnecting', 'reconnecting');
      }
    };

    es.addEventListener('request', (msg) => {
      markConnected();
      let p = null; try { p = JSON.parse(msg.data); } catch (_) { return; }
      if (!p || !p.data) return;
      // rec.class is Go-supplied (eventForWire wraps the
      // record as classifiedRecord). '|| forwarded-api' is a
      // DEFENSIVE default only — JS never re-derives the class.
      patchActivity(p.data, p.data.class || 'forwarded-api');
      updateClaudeSession(p.data);
      bumpUpdates(p.time);
    });
    es.addEventListener('proxy_error', (msg) => {
      markConnected();
      let p = null; try { p = JSON.parse(msg.data); } catch (_) { return; }
      if (!p || !p.data) return;
      patchActivity(p.data, 'error');
      bumpUpdates(p.time);
    });
    es.addEventListener('trace', (msg) => {
      markConnected();
      let p = null; try { p = JSON.parse(msg.data); } catch (_) { return; }
      if (!p || !p.data) return;
      patchActivity(p.data, 'trace');
      bumpUpdates(p.time);
    });
    ['session_created','session_updated','session_attached'].forEach(t => {
      es.addEventListener(t, (msg) => {
        markConnected();
        let p = null; try { p = JSON.parse(msg.data); } catch (_) { return; }
        if (!p) return;
        if (p.data) {
          if (t === 'session_updated' && popState === 'pending') {
            // Buffer for reconciliation gate on fetch-fail.
            popPendingSSESnapshot = p.data;
          }
          patchSession(p.data);
          // If popover is OPEN (loaded) when a session_updated
          // arrives, refetch catalog to reflect any concurrent CLI switch.
          if (t === 'session_updated' && popState === 'loaded') {
            fetchCatalog();
          }
        }
        bumpUpdates(p.time);
      });
    });
    es.addEventListener('session_closed', (msg) => {
      let p = null; try { p = JSON.parse(msg.data); } catch (_) { p = null; }
      if (p && p.data) patchSession(p.data);
      if (p) bumpUpdates(p.time);
      ended = true;
      if (es) { es.close(); es = null; }
      setLiveState('ended', 'session ended');
      if (toggle) toggle.disabled = true;
    });
  }

  // Swap the live-toggle's icon + accessible name in place (the button holds
  // an <svg><use>, not text): loader (spinning) while live, play while paused.
  function setToggleIcon(isPaused) {
    if (!toggle) return;
    var label = isPaused ? 'Resume stream' : 'Pause stream';
    toggle.setAttribute('title', label);
    toggle.setAttribute('aria-label', label);
    var svg = toggle.querySelector('svg');
    var use = toggle.querySelector('use');
    if (svg) { if (isPaused) svg.classList.remove('spin'); else svg.classList.add('spin'); }
    if (use) {
      var ref = isPaused ? '#i-play' : '#i-loader';
      use.setAttribute('href', ref);
      use.setAttribute('xlink:href', ref);
    }
  }
  if (toggle) {
    toggle.addEventListener('click', () => {
      if (ended) return;
      paused = !paused;
      if (paused) {
        if (es) { es.close(); es = null; }
        setToggleIcon(true);
        setLiveState('paused', 'paused');
      } else {
        setToggleIcon(false);
        setLiveState('connecting', 'connecting');
        connectLive();
      }
    });
  }

  // --- lazy-fetch structured request-body renderer ---
  // A single delegated <toggle> listener (capture phase so it sees the
  // per-row <details class="body-drawer">). Body bytes are fetched
  // from /recent/body?id=<reqid> at FIRST expand only (data-loaded
  // guard); never inlined server-side. This is a browser reimpl of the
  // pure-Go internal/ui BodyView projection — same shape, no Go call.
  // textContent only (no innerHTML) — mirrors headerPanelEl's idiom.
  function bvAccordion(title, childNodes, open){
    var d = document.createElement('details');
    if (open) d.open = true;
    var s = document.createElement('summary'); s.textContent = title; d.appendChild(s);
    var b = document.createElement('div'); b.className = 'bv-acc';
    childNodes.forEach(function(c){ if (c) b.appendChild(c); });
    d.appendChild(b); return d;
  }
  function bvRaw(text){
    // Raw view of one block: collapsed <details> when large, else <pre>.
    if (text != null && String(text).length > 500){
      var rd = document.createElement('details');
      var rs = document.createElement('summary'); rs.textContent = 'raw (' + String(text).length + ' chars)'; rd.appendChild(rs);
      var rp = document.createElement('pre'); rp.textContent = String(text); rd.appendChild(rp);
      return rd;
    }
    var pre = document.createElement('pre'); pre.textContent = String(text == null ? '' : text); return pre;
  }
  function bvBlock(i, type, text, cc){
    var wrap = document.createElement('div'); wrap.className = 'bv-block';
    if (cc){ wrap.classList.add(cc.indexOf('/') >= 0 ? 'ccg' : 'cc'); }
    var head = document.createElement('div'); head.className = 'bh';
    head.textContent = '[' + i + '] ' + (type || '') + ' · ' + String(text == null ? '' : text).length + ' B';
    if (cc){ var chip = document.createElement('span'); chip.className = 'bv-chip' + (cc.indexOf('/') >= 0 ? ' g' : ''); chip.textContent = cc; head.appendChild(chip); }
    wrap.appendChild(head);
    wrap.appendChild(bvRaw(text));
    return wrap;
  }
  function bvTurn(role, blocksArray){
    var wrap = document.createElement('div'); wrap.className = 'bv-turn';
    var rh = document.createElement('div'); rh.className = 'bv-role'; rh.textContent = '◆ ' + (role || ''); wrap.appendChild(rh);
    blocksArray.forEach(function(b, i){
      var type = (b && b.type) || '';
      var text = (b && typeof b.text === 'string') ? b.text : JSON.stringify(b, null, 2);
      wrap.appendChild(bvBlock(i, type, text, bvCC(b)));
    });
    return wrap;
  }
  function bvSecLabel(t){ var d=document.createElement('div'); d.className='bv-seclabel'; d.textContent=t; return d; }
  function bvCC(b){ return (b && b.cache_control) ? (b.cache_control.type + (b.cache_control.scope ? '/' + b.cache_control.scope : '')) : ''; }
  function bvToolRow(t){
    var d=document.createElement('details'); d.className='bv-tool';
    var props=(t.input_schema && t.input_schema.properties) ? Object.keys(t.input_schema.properties) : [];
    var dlen=String(t.description==null?'':t.description).length;
    var tlen=JSON.stringify(t==null?{}:t).length;
    var s=document.createElement('summary'); s.appendChild(document.createTextNode((t.name||'?')+' '));
    var tm=document.createElement('span'); tm.className='tm'; tm.textContent=tlen+' B · '+props.length+' props'; s.appendChild(tm); d.appendChild(s);
    var b=document.createElement('div'); b.className='tb';
    var dd=document.createElement('details'); var ds=document.createElement('summary'); ds.textContent='description ('+dlen+' chars)'; dd.appendChild(ds);
    var dp=document.createElement('pre'); dp.textContent=String(t.description==null?'':t.description); dd.appendChild(dp); b.appendChild(dd);
    var sc=document.createElement('div'); sc.className='bh'; sc.textContent='input_schema'; b.appendChild(sc);
    var sp=document.createElement('pre'); sp.textContent=JSON.stringify(t.input_schema||{},null,2); b.appendChild(sp);
    d.appendChild(b); return d;
  }
  // Fixed READING order (not wire order) —
  // anatomy → config(scalars) → system → tools → messages →
  // other object keys → Raw(view+download). Routing rule: array
  // system/tools/messages → named sub-accordions; other non-scalar
  // → collapsible pretty-<pre> in reading order; scalars → config
  // strip; anatomy covers all. textContent/setAttribute only.
  function renderBodyView(panel, txt){
    panel.textContent = '';
    var raw = txt, doc;
    try { doc = JSON.parse(txt); } catch (_) { var pre=document.createElement('pre'); pre.textContent=raw; panel.appendChild(pre); return; }
    var total = raw.length, keys = Object.keys(doc);
    var anat = document.createElement('div'); anat.className = 'body-anatomy';
    keys.map(function(k){
      var seg = JSON.stringify(doc[k]).length;
      return { k: k, seg: seg, pct: total ? Math.round(seg*100/total) : 0 };
    }).sort(function(a,b){ return b.seg - a.seg; }).forEach(function(e){
      var s = document.createElement('span'); s.appendChild(document.createTextNode(e.k + ' '));
      var pb = document.createElement('b'); pb.textContent = (e.pct===0 && e.seg>0) ? '<1%' : e.pct + '%'; s.appendChild(pb); anat.appendChild(s);
    });
    panel.appendChild(anat);
    var scalars = keys.filter(function(k){ var v=doc[k]; return v===null || (typeof v!=='object'); });
    if (scalars.length){
      panel.appendChild(bvSecLabel('config'));
      var cfg = document.createElement('div'); cfg.className = 'bv-config';
      scalars.forEach(function(k){
        var ck=document.createElement('div'); ck.className='ck'; ck.textContent=k;
        var cv=document.createElement('div'); cv.className='cv'; cv.textContent=String(doc[k]);
        cfg.appendChild(ck); cfg.appendChild(cv);
      });
      panel.appendChild(cfg);
    }
    if (Array.isArray(doc.system)){
      panel.appendChild(bvAccordion('system [' + doc.system.length + ']', doc.system.map(function(b,i){
        return bvBlock(i, (b && b.type) || '', (b && typeof b.text==='string') ? b.text : JSON.stringify(b), bvCC(b));
      }), true));
    }
    if (Array.isArray(doc.tools)){
      panel.appendChild(bvAccordion('tools [' + doc.tools.length + ']', doc.tools.map(function(t){
        return bvToolRow(t);
      })));
    }
    if (Array.isArray(doc.messages)){
      panel.appendChild(bvAccordion('messages [' + doc.messages.length + ']', doc.messages.map(function(m){
        var c=m.content, blks=Array.isArray(c)?c:[{type:'text',text:String(c)}];
        return bvTurn(m.role || '', blks);
      })));
    }
    keys.forEach(function(k){
      if (k==='system'||k==='tools'||k==='messages') return;
      var v=doc[k]; if (v===null || typeof v!=='object') return;
      panel.appendChild(bvAccordion(k, [ (function(){ var p=document.createElement('pre'); p.textContent=JSON.stringify(v,null,2); return p; })() ]));
    });
    var rd = document.createElement('details');
    var rs = document.createElement('summary'); rs.textContent = 'Raw JSON'; rd.appendChild(rs);
    var racc = document.createElement('div'); racc.className = 'bv-acc';
    var dl = document.createElement('a'); dl.className='bv-dl'; dl.textContent='download request-body.json';
    dl.setAttribute('href', 'data:application/json,' + encodeURIComponent(raw));
    dl.setAttribute('download', 'request-body.json');
    racc.appendChild(dl);
    var rp = document.createElement('pre'); rp.textContent = raw; racc.appendChild(rp);
    rd.appendChild(racc);
    panel.appendChild(rd);
  }
  function fetchBodyInto(pane, id) {
    fetch('/recent/body?id=' + encodeURIComponent(id)).then(function(r){ if (!r.ok) throw 0; return r.text(); }).then(function(txt){
      pane.textContent = '';
      renderBodyView(pane, txt);
    }).catch(function(){ pane.textContent = 'body not retained (evicted, capture off, or write failed)'; });
  }
  // renderJsonView renders an arbitrary telemetry body: pretty-printed when
  // parseable JSON, raw text otherwise. textContent only (third-party data) —
  // NOT the Anthropic-shaped renderBodyView.
  function renderJsonView(panel, txt){
    panel.textContent = '';
    var pre = document.createElement('pre');
    try { pre.textContent = JSON.stringify(JSON.parse(txt), null, 2); }
    catch (_e) { pre.textContent = txt; }
    panel.appendChild(pre);
  }
  function fetchJsonInto(pane, id){
    fetch('/recent/body?id=' + encodeURIComponent(id)).then(function(r){ if (!r.ok) throw 0; return r.text(); }).then(function(txt){
      renderJsonView(pane, txt);
    }).catch(function(){ pane.textContent = 'body not retained (evicted, capture off, or write failed)'; });
  }
  // isSSEBody reports whether a captured RESPONSE body is an Anthropic
  // text/event-stream (the streaming /v1/messages case). A non-streaming
  // Message / error / redacted-OAuth body parses as JSON and is NOT SSE; a
  // ccwrap sentinel (‹…›) parses-fails and lacks event:/data: lines.
  function isSSEBody(txt){
    try { JSON.parse(txt); return false; } catch (_e) {}
    var s = String(txt == null ? '' : txt);
    return s.indexOf('event:') >= 0 && s.indexOf('data:') >= 0;
  }
  // parseSSE splits a raw SSE stream into {event, data} records (data lines of
  // one event are concatenated). Tolerates CRLF.
  function parseSSE(txt){
    var events = [];
    String(txt == null ? '' : txt).split(/\r?\n\r?\n/).forEach(function(block){
      var ev = { event: '', data: '' };
      block.split(/\r?\n/).forEach(function(line){
        if (line.indexOf('event:') === 0) ev.event = line.slice(6).trim();
        else if (line.indexOf('data:') === 0) ev.data += line.slice(5).trim();
      });
      if (ev.event || ev.data) events.push(ev);
    });
    return events;
  }
  // reassembleSSE folds an Anthropic message stream back into its final shape:
  // the assistant text (text_delta), any thinking (thinking_delta) / tool-input
  // (input_json_delta) deltas, plus message metadata. Pure (no DOM) so it is
  // unit-testable; renderSSEView paints the result.
  function reassembleSSE(txt){
    var events = parseSSE(txt);
    var out = { text: '', thinking: '', model: '', stop: '', outTok: 0, events: events.length };
    events.forEach(function(ev){
      var d; try { d = JSON.parse(ev.data); } catch (_e) { return; }
      if (!d || typeof d !== 'object') return;
      if (d.type === 'message_start' && d.message){
        out.model = d.message.model || out.model;
        if (d.message.usage && d.message.usage.output_tokens != null) out.outTok = d.message.usage.output_tokens;
      } else if (d.type === 'content_block_delta' && d.delta){
        if (typeof d.delta.text === 'string') out.text += d.delta.text;
        else if (typeof d.delta.thinking === 'string') out.thinking += d.delta.thinking;
        else if (typeof d.delta.partial_json === 'string') out.text += d.delta.partial_json;
      } else if (d.type === 'message_delta'){
        if (d.delta && d.delta.stop_reason) out.stop = d.delta.stop_reason;
        if (d.usage && d.usage.output_tokens != null) out.outTok = d.usage.output_tokens;
      }
    });
    return out;
  }
  // renderSSEView paints the reassembled stream: a small anatomy strip, the
  // (optional) thinking block, the final assistant text, and the raw SSE in a
  // collapsed escape hatch. textContent only.
  function renderSSEView(panel, txt){
    var r = reassembleSSE(txt);
    var anat = document.createElement('div'); anat.className = 'body-anatomy';
    function chip(k, v){ var s=document.createElement('span'); s.appendChild(document.createTextNode(k + ' ')); var b=document.createElement('b'); b.textContent = String(v); s.appendChild(b); anat.appendChild(s); }
    chip('events', r.events);
    if (r.model) chip('model', r.model);
    if (r.stop) chip('stop', r.stop);
    if (r.outTok) chip('out tok', r.outTok);
    panel.appendChild(anat);
    if (r.thinking){
      panel.appendChild(bvSecLabel('thinking'));
      var tp = document.createElement('pre'); tp.textContent = r.thinking; panel.appendChild(tp);
    }
    panel.appendChild(bvSecLabel('assistant text'));
    var pre = document.createElement('pre'); pre.textContent = r.text || '(no text content)'; panel.appendChild(pre);
    var rd = document.createElement('details');
    var rs = document.createElement('summary'); rs.textContent = 'Raw SSE (' + String(txt).length + ' chars)'; rd.appendChild(rs);
    var dl = document.createElement('a'); dl.className = 'bv-dl'; dl.textContent = 'download response.sse';
    dl.setAttribute('href', 'data:text/plain;charset=utf-8,' + encodeURIComponent(String(txt)));
    dl.setAttribute('download', 'response.sse');
    rd.appendChild(dl);
    var rp = document.createElement('pre'); rp.textContent = String(txt); rd.appendChild(rp);
    panel.appendChild(rd);
  }
  // renderResponseView renders a captured Anthropic RESPONSE body: SSE streams
  // are reassembled (renderSSEView), non-streaming JSON (Message / error /
  // redacted OAuth) is pretty-printed, and ccwrap sentinels / other text fall
  // through to raw via renderJsonView. This is the ON-DISK capture only — the
  // client's stream is never altered. textContent only.
  function renderResponseView(panel, txt){
    panel.textContent = '';
    if (isSSEBody(txt)) { renderSSEView(panel, txt); return; }
    renderJsonView(panel, txt);
  }
  function fetchResponseInto(pane, id){
    fetch('/recent/body?id=' + encodeURIComponent(id)).then(function(r){ if (!r.ok) throw 0; return r.text(); }).then(function(txt){
      renderResponseView(pane, txt);
    }).catch(function(){ pane.textContent = 'body not retained (evicted, capture off, or write failed)'; });
  }
  // When data-upstream-reqid is set, render a client/upstream toggle so the
  // user can compare what their CLI sent vs what hit the wire after
  // modelalias rewrite + system-block stripping. Both bodies fetch lazily on
  // first expand; toggle just swaps visibility.
  function renderBodyDrawerDualView(panel, clientId, upstreamId) {
    panel.textContent = '';
    var bar = document.createElement('div'); bar.className = 'body-view-toggle';
    var btnC = document.createElement('button'); btnC.type='button'; btnC.className='body-view-btn on'; btnC.textContent='received'; btnC.setAttribute('data-view','client');
    var btnU = document.createElement('button'); btnU.type='button'; btnU.className='body-view-btn'; btnU.textContent='forwarded'; btnU.setAttribute('data-view','upstream');
    var hint = document.createElement('span'); hint.className='body-view-hint'; hint.textContent='after alias + strip';
    bar.appendChild(btnC); bar.appendChild(btnU); bar.appendChild(hint);
    panel.appendChild(bar);
    var paneC = document.createElement('div'); paneC.className='body-view-pane on'; paneC.textContent='loading…';
    var paneU = document.createElement('div'); paneU.className='body-view-pane'; paneU.textContent='loading…';
    panel.appendChild(paneC); panel.appendChild(paneU);
    fetchBodyInto(paneC, clientId);
    fetchBodyInto(paneU, upstreamId);
    bar.addEventListener('click', function(ev){
      var b = ev.target && ev.target.closest ? ev.target.closest('.body-view-btn') : null;
      if (!b) return;
      var isClient = (b === btnC);
      btnC.classList.toggle('on', isClient);
      btnU.classList.toggle('on', !isClient);
      paneC.classList.toggle('on', isClient);
      paneU.classList.toggle('on', !isClient);
    });
  }
  document.addEventListener('toggle', function(e){
    var d = e.target;
    if (!(d && d.classList && d.classList.contains('body-drawer') && d.open)) return;
    if (d.getAttribute('data-loaded') === '1') return;
    d.setAttribute('data-loaded', '1');
    var clientId = d.getAttribute('data-reqid');
    var upstreamId = d.getAttribute('data-upstream-reqid');
    var panel = d.querySelector('.body-panel');
    // Response drawers (forwarded Anthropic rows) carry the captured RESPONSE
    // body — SSE-aware. Dispatch FIRST so a resp-drawer never falls through to
    // the request-body (dual/Anthropic) path.
    if (d.classList.contains('resp-drawer')) { fetchResponseInto(panel, d.getAttribute('data-reqid')); return; }
    // Telemetry drawers carry third-party JSON, not Anthropic messages:
    // render via the generic renderJsonView. Dispatch FIRST so a
    // tele-drawer never falls through to the dual/Anthropic-body path.
    if (d.classList.contains('tele-drawer')) { fetchJsonInto(panel, d.getAttribute('data-reqid')); return; }
    if (upstreamId) {
      renderBodyDrawerDualView(panel, clientId, upstreamId);
    } else {
      fetchBodyInto(panel, clientId);
    }
  }, true);

  // First paint: the server pre-marked the default-class button '.on'
  // (aria-selected="true"). The on-load rebuildActivityFromState() just
  // below rebuilds the Activity list as exactly the newest
  // LIMITS.activity records of that default class, drawn from the full
  // bootstrap state (filter-aware capping) — so the default Forwarded
  // API view shows /v1/messages even under heavy synthetic/CONNECT
  // noise. No interim unfiltered paint, no hide pass.
  updateLastActivityLabel();
  window.setInterval(updateLastActivityLabel, 1000);
  connectLive();

  // --- filter-bar interaction + default filter + show-more ---
  // refreshCounts/syncMore/rebuildActivityFromState/state/LIMITS are
  // defined above — REUSED here, never redefined or shadowed. The
  // visible window and show-more operate over the active-filter subset
  // of the full retained state, so filtering is what is BUILT (no
  // CSS-hidden rows, no per-row hide pass).
  var fbar = document.getElementById('activity-filter');
  if (fbar) fbar.addEventListener('click', function(e){
    var b = e.target && e.target.closest ? e.target.closest('.filter-btn') : null;
    if (!b) return;
    var btns = fbar.querySelectorAll('.filter-btn');
    for (var i = 0; i < btns.length; i++) {
      var sel = (btns[i] === b);
      btns[i].classList.toggle('on', sel);
      btns[i].setAttribute('aria-selected', sel ? 'true' : 'false');
      // Roving tabindex (APG tabs): only the selected tab sits in the
      // tab order; arrows (handler below) move between tabs.
      btns[i].setAttribute('tabindex', sel ? '0' : '-1');
      if (sel && btns[i].id) {
        var panel = document.getElementById('activity-body');
        if (panel) panel.setAttribute('aria-labelledby', btns[i].id);
      }
    }
    // Rebuild as the newest LIMITS.activity rows of the now-active
    // class from the FULL retained state (filter-aware capping), not a
    // slice-then-hide of the existing DOM.
    rebuildActivityFromState();
  });
  // Arrow-key roving for the tablist (APG tabs, automatic activation):
  // Left/Right wrap across the chips, Home/End jump; moving focus also
  // activates via the click handler above so list + aria stay in lockstep.
  if (fbar) fbar.addEventListener('keydown', function(e){
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight' && e.key !== 'Home' && e.key !== 'End') return;
    var btns = Array.prototype.slice.call(fbar.querySelectorAll('.filter-btn'));
    var idx = btns.indexOf(document.activeElement);
    if (idx < 0) return;
    e.preventDefault();
    var next = e.key === 'ArrowLeft' ? idx - 1 : e.key === 'ArrowRight' ? idx + 1 : e.key === 'Home' ? 0 : btns.length - 1;
    next = (next + btns.length) % btns.length;
    btns[next].focus();
    btns[next].click();
  });
  // On-load default-class render from the full bootstrap state
  // (rebuildActivityFromState does refreshCounts + syncMore
  // internally). Zero server change: the correct default-class window
  // is produced client-side immediately.
  rebuildActivityFromState();
  var moreWrap = document.getElementById('activity-more');
  if (moreWrap) {
    var moreBtn = moreWrap.querySelector('button');
    if (moreBtn) moreBtn.addEventListener('click', function(){
      // Deepen the active-class window newest-first by raising the cap
      // and reusing the canonical filter-aware merge+filter+sort+cap+
      // rebuild (which also refreshCounts() + syncMore()).
      LIMITS.activity += 50;
      rebuildActivityFromState();
      syncMore();
    });
    syncMore();
  }

  // ============================================================
  // Profile popover state machine, catalog + switch
  // endpoints, CSRF token plumbing, per-row hash-colored profile
  // annotations, switch-marker structured rendering. Single source
  // is the inline mirror — gated on LiveEnabled (this whole script
  // only runs for live sessions; ended sessions miss the polish, an
  // accepted limitation).
  // ============================================================

  // --- State machine ---
  var popState = 'closed';
  var popClickedName = null;
  var popPreClickActiveProfileName = null;
  var popPendingSSESnapshot = null;
  var popLastFetchAbort = null;
  var profileCell = null;
  var popoverEl = null;

  function providerHue(provider) {
    // Stable djb2-ish hash over the UTF-16 code units of provider.
    var h = 0, s = String(provider || '');
    for (var i = 0; i < s.length; i++) {
      h = (h * 31 + s.charCodeAt(i)) | 0;
    }
    // Constrain to 70-330deg: keeps annotation hues clear of the amber
    // warn (~45deg) and rose danger (~350deg) status hues, so a profile
    // chip can never read as a 4xx/5xx signal next to a status cell.
    return 70 + (((h % 260) + 260) % 260);
  }

  function setPopState(next) { popState = next; if (popoverEl) popoverEl.dataset.popState = next; }

  function ensurePopover() {
    if (popoverEl) return popoverEl;
    popoverEl = document.createElement('div');
    popoverEl.className = 'sp3-pop';
    popoverEl.dataset.popState = 'closed';
    popoverEl.addEventListener('click', function(ev){ ev.stopPropagation(); });
    profileCell.appendChild(popoverEl);
    return popoverEl;
  }

  function closePopover() {
    if (popLastFetchAbort) { try { popLastFetchAbort.abort(); } catch(_){} popLastFetchAbort = null; }
    if (popoverEl) { popoverEl.remove(); popoverEl = null; }
    setPopState('closed');
    popClickedName = null;
    popPreClickActiveProfileName = null;
    popPendingSSESnapshot = null;
  }

  function renderPopoverShell() {
    var body = document.createElement('div');
    body.className = 'sp3-pop-body';
    var status = document.createElement('div');
    status.className = 'sp3-pop-status';
    status.textContent = 'loading…';
    body.appendChild(status);
    ensurePopover();
    popoverEl.textContent = '';
    popoverEl.appendChild(body);
    return { body: body, status: status };
  }

  // --- icon-button helpers + popoverCtx extension stub ---
  // svgIcon builds a <svg><use href="#symbolId"></use></svg> referencing
  // the inline symbols defined at the top of the document.
  function svgIcon(symbolId) {
    var svg = document.createElementNS('http://www.w3.org/2000/svg','svg');
    var use = document.createElementNS('http://www.w3.org/2000/svg','use');
    use.setAttributeNS('http://www.w3.org/1999/xlink','xlink:href','#'+symbolId);
    use.setAttribute('href','#'+symbolId);
    svg.appendChild(use);
    return svg;
  }

  // iconButton builds a square 22px button with the .pop-action class plus
  // optional .danger / .is-default-state variants. Title doubles as aria-label.
  function iconButton(symbolId, title, opts) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'pop-action' + (opts && opts.danger ? ' danger' : '') + (opts && opts.defaultState ? ' is-default-state' : '');
    b.title = title;
    b.setAttribute('aria-label', title);
    b.appendChild(svgIcon(symbolId));
    return b;
  }

  // buildRowActionColumn returns the 4 trailing buttons for a profile row:
  // edit, test, default-slot (set/clear), remove. Click handlers route through
  // popoverCtx so later code can install real implementations
  // without touching this builder.
  function buildRowActionColumn(item, isDefault, ctx) {
    var editBtn = iconButton('i-edit', 'edit profile');
    editBtn.addEventListener('click', function(ev){
      ev.stopPropagation();
      ctx.openEditPanel(item);
    });

    var testBtn = iconButton('i-activity', 'test connectivity');
    testBtn.setAttribute('data-action', 'test-slot');
    testBtn.addEventListener('click', function(ev){
      ev.stopPropagation();
      onProfileTestClick(item.name, ctx.row, testBtn);
    });

    var defaultBtn;
    if (isDefault) {
      defaultBtn = iconButton('i-check-circle', 'currently the default (click to clear)', { defaultState: true });
      defaultBtn.addEventListener('click', function(ev){
        ev.stopPropagation();
        ctx.setDefaultProfile('inherit-env', ctx.row);
      });
    } else {
      defaultBtn = iconButton('i-play', 'activate (set as default + switch this session)');
      var nameRef = item.name;
      defaultBtn.addEventListener('click', function(ev){
        ev.stopPropagation();
        ctx.setDefaultProfile(nameRef, ctx.row);
      });
    }
    defaultBtn.setAttribute('data-action', 'default-slot');

    var rmBtn = iconButton('i-trash', 'remove profile', { danger: true });
    // Arm-then-confirm in-place. First click → button enters .armed state
    // (red pulse) for 2 seconds. Second click within the window → submit.
    // Replaces the legacy modal + type-to-confirm flow.
    var rmArmTimer = null;
    function rmDisarm() {
      rmBtn.classList.remove('armed');
      rmBtn.title = 'remove profile';
      if (rmArmTimer) { clearTimeout(rmArmTimer); rmArmTimer = null; }
    }
    rmBtn.addEventListener('click', function(ev){
      ev.stopPropagation();
      if (!rmBtn.classList.contains('armed')) {
        rmBtn.classList.add('armed');
        rmBtn.title = 'click again within 2s to confirm delete';
        rmArmTimer = setTimeout(rmDisarm, 2000);
      } else {
        rmDisarm();
        ctx.submitRm(item, isDefault);
      }
    });

    return [defaultBtn, testBtn, editBtn, rmBtn];
  }

  // popoverCtx is the shared action namespace populated when the popover
  // opens. Real entry points installed below replace the no-op stubs.
  // submitRm replaced openRmModal (modal removed in favor of
  // in-row arm-then-confirm + toast feedback).
  var popoverCtx = {
    openEditPanel: function(item){},
    openAddPanel: function(){},
    setDefaultProfile: function(name, rowEl){},
    submitRm: function(item, isDefault){},
  };

  // ============================================================
  // Real entry points (stubs replaced).
  // ============================================================
  popoverCtx.openEditPanel = function(item) {
    renderEditOrAddPanel(item, 'edit');
    // buildEditActions already stashes _origSnap, but reassign here in case
    // a caller (test, future event) renders without going through buildEditActions.
    if (popoverCtx.editTarget && popoverCtx.editTarget.form && !popoverCtx.editTarget._origSnap) {
      popoverCtx.editTarget._origSnap = captureFormSnapshot(popoverCtx.editTarget.form);
    }
  };

  popoverCtx.openAddPanel = function() {
    var emptyItem = {
      name: '', provider: '', base_url: '',
      auth: { mode: 'ccwrap_bearer' },
      egress_mode: 'inherit', egress_url: ''
    };
    renderEditOrAddPanel(emptyItem, 'add');
  };

  // "activate this profile" = set-default (file) + switch (live session).
  // Replaces the prior implicit row-click → switch flow. Sequenced: set-default
  // first (persistence) then switch (runtime). If switch fails (e.g. env
  // lacks credentials for inherit-env, or preflight rejects the new profile),
  // file.Default is still updated, but the user MUST be told the live session
  // did not move — otherwise the popover icons would deceptively indicate
  // "switched" while messages keep flowing through the old profile. The same
  // truthfulness requirement covers the mirror case: a switch whose ACK was
  // lost in transit must NOT be reported as failed when it actually landed.
  // The switch leg below arms the reconciliation gate (popState 'pending'
  // buffers session_updated snapshots) and a transport failure is judged by
  // reconcileFetchFailure AFTER the catalog refresh, never toasted blindly.
  popoverCtx.setDefaultProfile = function(name, rowEl) {
    fetch('/profile/set-default', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CCWRAP-Profile-Token': PROFILE_TOKEN,
      },
      body: JSON.stringify({ name: name }),
    })
    .then(function(resp){ return resp.json(); })
    .then(function(json){
      if (!json.ok) {
        showToast('failed to set default: ' + (json.message || json.kind || 'unknown'), 'error');
        return;
      }
      popoverCtx.defaultName = json.default;
      // Chain: also switch the live session to the same profile. Arm the
      // reconciliation gate first: while this POST is in flight, the SSE
      // session_updated handler buffers snapshots into popPendingSSESnapshot
      // (popState === 'pending') so a lost ACK can be reconciled against
      // what the server actually did.
      setPopState('pending');
      popClickedName = name;
      popPreClickActiveProfileName = (state.session && state.session.active_profile_name) || '';
      popPendingSSESnapshot = null;
      return fetch('/profile/switch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CCWRAP-Profile-Token': PROFILE_TOKEN },
        body: JSON.stringify({ name: name }),
      })
      .then(function(r){ return r.json(); })
      .then(function(outcome){
        // A definitive outcome arrived — disarm the gate. (Guarded so a
        // popover the user closed mid-flight stays 'closed'.)
        if (popState === 'pending') setPopState('loaded');
        popPendingSSESnapshot = null;
        var result = String((outcome && outcome.result) || '');
        if (result === 'switched') {
          // Mirror the live-session active marker without waiting for SSE.
          if (state.session) {
            state.session.active_profile_name = (outcome.view && outcome.view.name) || '';
            state.session.active_profile_provider = (outcome.view && outcome.view.provider) || '';
          }
          if (typeof updateProfileCell === 'function') updateProfileCell();
        } else {
          // Defer until after fetchCatalog re-renders so the structured
          // banner (with $ relaunch command) survives the popover refresh.
          // file.Default has been updated on disk — the live-session refusal
          // needs explicit attention, not a 4s toast.
          popoverCtx.pendingRefusedOutcome = { outcome: outcome, name: name };
        }
      })
      .catch(function(_err){
        // Transport failure (or unparseable reply) on the switch leg: do
        // NOT toast "switch failed" — the switch may have landed with only
        // the ACK lost. Defer the verdict to reconcileFetchFailure after
        // the catalog refresh below, so its outcome block survives the
        // popover shell re-render.
        popoverCtx.pendingSwitchReconcile = true;
      });
    })
    .then(function(){
      // The user may have dismissed the popover while the chain was in
      // flight; don't resurrect it (fetchCatalog re-creates the node).
      if (popState === 'closed') {
        popoverCtx.pendingRefusedOutcome = null;
        popoverCtx.pendingSwitchReconcile = false;
        return;
      }
      if (typeof fetchCatalog !== 'function') return;
      return fetchCatalog();
    })
    .then(function(){
      var pending = popoverCtx.pendingRefusedOutcome;
      if (pending) {
        popoverCtx.pendingRefusedOutcome = null;
        if (popState !== 'closed') {
          var newRow = popoverEl ? popoverEl.querySelector('[data-profile-name="' + pending.name + '"]') : null;
          renderOutcome(pending.outcome, newRow);
        }
      }
      // Lost-ACK reconciliation, post-refresh: the gate state armed above
      // lets reconcileFetchFailure judge the buffered SSE snapshot —
      // switch landed (ACK-only loss) → success block; a concurrent
      // switch → catalog refetch; genuinely dead → honest failure block.
      if (popoverCtx.pendingSwitchReconcile) {
        popoverCtx.pendingSwitchReconcile = false;
        if (popState !== 'closed') reconcileFetchFailure();
      }
    });
  };
  popoverCtx.submitRm = function(item, isDefault) {
    submitRmDirect(item, isDefault);
  };

  function renderCatalog(resp) {
    var shell = renderPopoverShell();
    shell.status.remove();
    if (resp.load_error) {
      var err = document.createElement('div');
      err.className = 'outcome danger';
      err.textContent = '✗ ' + resp.load_error;
      shell.body.appendChild(err);
      setPopState('load-error');
      return;
    }
    if (!resp.has_profiles_file) {
      var miss = document.createElement('div');
      miss.className = 'sp3-pop-status';
      miss.textContent = 'No profiles.json — create one at ' + String(resp.source || '');
      shell.body.appendChild(miss);
      setPopState('missing-file');
      return;
    }
    setPopState('loaded');
    // If no active profile, header status line.
    if (!resp.active_profile) {
      var hdr = document.createElement('div');
      hdr.className = 'sp3-pop-status';
      hdr.textContent = 'No profile active — inheriting from launch env.';
      shell.body.appendChild(hdr);
    }
    // Group by provider.
    var groups = {};
    (resp.items || []).forEach(function(it){
      (groups[it.provider] = groups[it.provider] || []).push(it);
    });
    Object.keys(groups).sort().forEach(function(provider){
      var head = document.createElement('div');
      head.className = 'sp3-pop-group-head';
      head.textContent = provider;
      shell.body.appendChild(head);
      groups[provider].forEach(function(it){
        var row = document.createElement('div');
        row.className = 'sp3-pop-row' + (resp.active_profile === it.name ? ' active' : '');
        row.dataset.profileName = it.name;
        var radio = document.createElement('span');
        radio.className = 'pop-radio';
        radio.textContent = resp.active_profile === it.name ? '●' : '○';
        var name = document.createElement('span');
        name.className = 'pop-name';
        name.textContent = it.name;
        var host = document.createElement('span');
        host.className = 'pop-host';
        host.textContent = it.base_url_host || '';
        var meta = document.createElement('span');
        meta.className = 'pop-meta';
        var bits = [];
        if (it.model_alias_count) bits.push(it.model_alias_count + 'a');
        if (it.has_inline_key) bits.push('key');
        if (it.has_key_env) bits.push('env');
        meta.textContent = bits.join(' ');
        row.appendChild(radio); row.appendChild(name); row.appendChild(host); row.appendChild(meta);
        // Row is keyboard-focusable so :focus-within reveals the
        // hidden action icons (parity with hover).
        row.setAttribute('tabindex', '0');
        // 4-icon action column (replaces single test button). isDefault is
        // checked against resp.default — the persistent file.Default from
        // profiles.json, NOT the live session's active_profile. The radio
        // dot above tracks the session; the 4th action icon (check-circle
        // vs play) tracks the file's default.
        var actions = buildRowActionColumn(it, it.name === resp.default, {
          row: row,
          openEditPanel: popoverCtx.openEditPanel,
          setDefaultProfile: popoverCtx.setDefaultProfile,
          submitRm: popoverCtx.submitRm,
        });
        for (var aIdx = 0; aIdx < actions.length; aIdx++) row.appendChild(actions[aIdx]);
        // Row click no longer triggers implicit switch — use the
        // default-slot button to activate (which now does set-default + switch).
        shell.body.appendChild(row);
      });
    });
    // The inherit-env special row was removed. The "official" profile
    // (auto-restored by EnsureOfficialProfile on every launch) occupies
    // that semantic slot and renders through the same per-row builder
    // as every other entry.

    // Footer with [Test all] button — appended after all profile rows.
    var divider = document.createElement('div');
    divider.className = 'pop-footer-divider';
    shell.body.appendChild(divider);

    var testAllBtn = document.createElement('button');
    testAllBtn.className = 'pop-test-all-btn';
    testAllBtn.type = 'button';
    testAllBtn.textContent = 'Test all';
    testAllBtn.addEventListener('click', function(ev){ ev.stopPropagation(); onProfileTestAllClick(testAllBtn); });
    shell.body.appendChild(testAllBtn);

    // Footer "+ new profile" button (popoverCtx.openAddPanel is wired
    // to the real panel-opening implementation).
    var addFooter = document.createElement('div');
    addFooter.className = 'sp3-pop-footer-add';
    var addBtn = document.createElement('button');
    addBtn.type = 'button';
    addBtn.title = 'create a new profile';
    addBtn.appendChild(svgIcon('i-plus'));
    addBtn.appendChild(document.createTextNode(' new profile'));
    addBtn.addEventListener('click', function(ev){
      ev.stopPropagation();
      popoverCtx.openAddPanel();
    });
    addFooter.appendChild(addBtn);
    shell.body.appendChild(addFooter);
  }

  function renderError(msg) {
    var shell = renderPopoverShell();
    shell.status.className = 'outcome danger';
    shell.status.textContent = '✗ ' + msg;
    var retry = document.createElement('a');
    retry.href = '#';
    retry.textContent = ' retry';
    retry.addEventListener('click', function(ev){ ev.preventDefault(); openPopover(); });
    shell.status.appendChild(retry);
    setPopState('error');
  }

  function fetchCatalog() {
    setPopState('loading');
    renderPopoverShell();
    if (popLastFetchAbort) { try { popLastFetchAbort.abort(); } catch(_){} }
    popLastFetchAbort = (typeof AbortController !== 'undefined') ? new AbortController() : null;
    var init = popLastFetchAbort ? { signal: popLastFetchAbort.signal, cache: 'no-store' } : { cache: 'no-store' };
    return fetch('/profile/catalog', init)
      .then(function(r){ if (!r.ok) throw new Error('http ' + r.status); return r.json(); })
      .then(function(resp){ renderCatalog(resp); })
      .catch(function(err){ if (err && err.name === 'AbortError') return; renderError('catalog fetch failed'); });
  }

  function openPopover() {
    if (popState !== 'closed' && popState !== 'error') return;
    closeAllRibbonPops();
    if (profileCell) profileCell.setAttribute('aria-expanded', 'true');
    fetchCatalog();
  }

  function onProfileTestClick(name, rowEl, btnEl) {
    if (!btnEl || btnEl.disabled) return;
    btnEl.disabled = true;
    btnEl.textContent = '…';
    btnEl.classList.add('testing');

    fetch('/profile/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CCWRAP-Profile-Token': PROFILE_TOKEN },
      body: JSON.stringify({ name: name })
    }).then(function(r){
      // Any non-2xx is plain text from http.Error (CSRF/format/lookup,
      // server-side marshal failure, etc.). Render as HTTP_4XX or
      // HTTP_5XX chip with the backend's error text in tooltip —
      // distinct from NET_FAIL (which is for actual transport failures).
      if (!r.ok) {
        return r.text().then(function(body){
          var chipStatus = (r.status >= 500) ? 'HTTP_5XX' : 'HTTP_4XX';
          return { profile: name, status: chipStatus, http_status: r.status, error: (body || '').trim() };
        });
      }
      return r.json();
    }).then(function(result){
      renderProfileTestChip(result, rowEl, btnEl);
    }).catch(function(err){
      renderProfileTestChip({ profile: name, status: 'NET_FAIL', error: 'fetch failed: ' + (err && err.message) }, rowEl, btnEl);
    });
  }

  function renderProfileTestChip(result, rowEl, btnEl) {
    // Replace the [test] button with a status chip in-place.
    // The chip is an icon-only square (22x22), color-coded by data-status.
    // Textual status + latency goes to the title attr (hover tooltip).
    var chip = document.createElement('span');
    chip.className = 'pop-test-chip';
    var status = String(result.status || 'UNKNOWN');
    chip.dataset.status = status;
    chip.appendChild(svgIcon(statusToIcon(status)));
    chip.title = formatChipLabel(result);
    if (result.error) {
      chip.title += ' — ' + String(result.error);
    } else if (result.skipped_reason) {
      chip.title += ' — ' + String(result.skipped_reason);
    }
    chip.setAttribute('aria-label', chip.title);
    // Swap the button for the chip in the row.
    if (btnEl && btnEl.parentNode) {
      btnEl.parentNode.replaceChild(chip, btnEl);
    }
  }

  function statusToIcon(status) {
    switch (status) {
      case 'OK':         return 'i-check';
      case 'SKIPPED':    return 'i-minus';
      case 'AUTH_FAIL':  return 'i-x';
      case 'MODEL_404':  return 'i-alert-triangle';
      case 'HTTP_4XX':   return 'i-alert-triangle';
      case 'HTTP_5XX':   return 'i-x';
      case 'TIMEOUT':    return 'i-clock';
      case 'NET_FAIL':   return 'i-x';
      default:           return 'i-alert-triangle';
    }
  }

  function formatChipLabel(result) {
    var status = String(result.status || 'UNKNOWN');
    var ms = result.latency_ms;
    var http = result.http_status;
    switch (status) {
      case 'OK': return 'OK ' + (ms != null ? ms + 'ms' : '');
      case 'SKIPPED': return 'SKIPPED';
      case 'AUTH_FAIL': return 'AUTH ' + (http || '');
      case 'MODEL_404': return 'MODEL 404';
      case 'HTTP_4XX': return 'HTTP ' + (http || '4xx');
      case 'HTTP_5XX': return 'HTTP ' + (http || '5xx');
      case 'TIMEOUT': return 'TIMEOUT';
      case 'NET_FAIL': return 'NET ERR';
      default: return status;
    }
  }

  // ---- Popover egress self-test (T12) ---------------------------------
  //
  // The [test] button appended next to the egress url field probes the
  // DRAFT EgressSpec (form values, not saved state) by POSTing
  // {name, egress_override: {mode, url}} to /profile/test-egress.
  // Renders ProbeEgressResult into a structured DOM panel — textContent
  // ONLY for every field that flows from upstream/ipinfo (especially
  // result.org which is third-party, unsanitized).

  function appendEgressTestButton(parentEl, item) {
    // The button slots into the egress_url field's trailing "auto"
    // grid column. appendField() creates the field as a 3-column grid
    // (88px label + 1fr input + auto) and appends an empty <span> as
    // the auto-column placeholder. We put the button INSIDE that span
    // so we don't add a 4th grid item and wrap to a new row.
    var urlField = parentEl.querySelector('[data-field="egress_url"]');
    if (!urlField) return;
    var fieldRow = urlField.closest('.sp3-pop-edit-field');
    if (!fieldRow) return;
    var actionSlot = fieldRow.lastElementChild;
    if (!actionSlot || actionSlot.tagName !== 'SPAN') return;

    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'egress-test-btn';
    btn.title = 'test egress';
    btn.setAttribute('aria-label', 'test egress');
    btn.appendChild(svgIcon('i-zap'));
    btn.dataset.profileName = (item && item.name) || '';
    actionSlot.appendChild(btn);

    // Result panel sits below the field row, full width inside the
    // section. Empty → display:none via .egress-test-panel:empty.
    var panel = document.createElement('div');
    panel.className = 'egress-test-panel';
    panel.setAttribute('aria-live', 'polite');
    fieldRow.insertAdjacentElement('afterend', panel);

    btn.addEventListener('click', function () {
      onEgressTestClick((item && item.name) || '', panel, btn);
    });
  }

  // egressOverrideForTest builds the egress_override the popover [test]
  // button sends. The URL is cleared for non-url-bearing modes
  // (inherit/direct/none) so [test] agrees with [save]: the mode-change
  // listener only updates the URL input's placeholder, never its value, so
  // after switching to inherit the input still holds the previous proxy URL.
  // Sending {mode:inherit, url:http://…} hits ValidateEgressSpec's 422
  // ("url must be empty when mode=inherit") for a draft [save] would accept.
  // url-bearing modes: http / socks5 / socks5h.
  // egressIsProxyMode is the predicate for "this egress mode carries a url"
  // (http / socks5 / socks5h). inherit / direct / none have no url, so the url
  // input is disabled for them in the editor. NOTE: egressOverrideForTest below
  // intentionally inlines the same check rather than calling this helper — a
  // behavioral test (TestSP3InlineScript_EgressOverrideForTest_*) extracts and
  // runs that function's body in ISOLATION via node, so it must be self-contained.
  function egressIsProxyMode(mode) {
    var m = mode || 'inherit';
    return m === 'http' || m === 'socks5' || m === 'socks5h';
  }
  function egressOverrideForTest(mode, url) {
    var m = mode || 'inherit';
    var urlBearing = (m === 'http' || m === 'socks5' || m === 'socks5h');
    return { mode: m, url: urlBearing ? (url || '') : '' };
  }

  // egressModeHint returns one plain-language line describing the selected egress
  // mode, shown muted under the "via" field. It makes "inherit" self-explanatory
  // (the egress analog of the old "inline" jargon) WITHOUT widening the glued
  // mode <select>. Pure -> node-testable.
  function egressModeHint(mode) {
    switch (mode) {
      case 'inherit': return 'follows the session egress — env proxy or direct';
      case 'direct': return 'no proxy — connects straight to the API';
      case 'http': return 'routes through an HTTP CONNECT proxy';
      case 'socks5': return 'SOCKS5 proxy — DNS resolved locally';
      case 'socks5h': return 'SOCKS5 proxy — DNS resolved at the proxy';
      default: return '';
    }
  }

  // authModeHint returns one plain-language line describing where the credential
  // goes for the selected auth mode — shown muted under the mode select. The
  // option labels stay terse + provider-neutral (Bearer token / x-api-key); this
  // hint names the exact header injected AND the equivalent env-var slot
  // (familiar to Claude Code users), which is clearer than the raw
  // ccwrap_bearer/ccwrap_x_api_key wire values without implying the upstream is
  // Anthropic (profiles routinely point at other providers). Pure -> node-testable.
  function authModeHint(mode) {
    switch (mode) {
      case 'ccwrap_bearer': return 'sent as Authorization: Bearer … · env ANTHROPIC_AUTH_TOKEN';
      case 'ccwrap_x_api_key': return 'sent as x-api-key: … · env ANTHROPIC_API_KEY';
      case 'passthrough': return 'no injection — Claude credentials pass straight through';
      default: return '';
    }
  }

  // egressRowValid reports whether the egress mode/url pair is complete: a proxy
  // mode (http/socks5/socks5h) requires a non-empty url; inherit/direct don't.
  // Calls egressIsProxyMode — a node behavioral test lifts BOTH (egressIsProxyMode
  // is self-contained), so this stays DRY rather than re-inlining the predicate.
  function egressRowValid(mode, url) {
    if (egressIsProxyMode(mode)) return String(url || '').trim().length > 0;
    return true;
  }

  // egressFieldValid reads the live egress fields from the form and applies
  // egressRowValid — the save gate used by isAddFormReady + the edit dirty check,
  // so a proxy mode with no endpoint can't be persisted.
  function egressFieldValid(form) {
    var m = form.querySelector('[data-field="egress_mode"]');
    var u = form.querySelector('[data-field="egress_url"]');
    return !m || !u || egressRowValid(m.value, u.value);
  }
  function onEgressTestClick(name, panelEl, btnEl) {
    if (!btnEl || btnEl.disabled) return;
    // Build the draft override from current form values so the popover
    // can validate edits before [save] is clicked.
    var modeEl = document.querySelector('[data-field="egress_mode"]');
    var urlEl = document.querySelector('[data-field="egress_url"]');
    var override = null;
    if (modeEl) {
      override = egressOverrideForTest(modeEl.value, urlEl ? urlEl.value : '');
    }
    btnEl.disabled = true;
    btnEl.replaceChildren(svgIcon('i-loader'));
    btnEl.firstChild.classList.add('spin');
    btnEl.title = 'testing…';
    while (panelEl.firstChild) panelEl.removeChild(panelEl.firstChild);

    fetch('/profile/test-egress', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CCWRAP-Profile-Token': PROFILE_TOKEN
      },
      body: JSON.stringify({ name: name, egress_override: override })
    })
      .then(function (r) {
        // Any non-2xx is plain text from http.Error (CSRF, validation,
        // not-found, marshal failure, etc.). Reading via .text() avoids
        // a SyntaxError from JSON parsing a non-JSON body — which would
        // surface as a misleading "Unexpected token..." in the result
        // panel instead of the server's actual diagnostic.
        if (!r.ok) {
          return r.text().then(function (body) {
            return { ok: false, body: { error: (body || '').trim() } };
          });
        }
        return r.json().then(function (b) { return { ok: true, body: b }; });
      })
      .then(function (resp) {
        renderEgressTestResult(panelEl, resp.body, resp.ok);
      })
      .catch(function (err) {
        renderEgressTestError(panelEl, (err && err.message) ? err.message : String(err));
      })
      .then(function () {
        btnEl.disabled = false;
        btnEl.replaceChildren(svgIcon('i-zap'));
        btnEl.title = 'test egress';
      });
  }

  // countryFlag prefixes a 2-letter ISO country code (ipinfo's country
  // field, e.g. "US") with its regional-indicator flag emoji: "US" -> "🇺🇸 US".
  // Anything that is not exactly two A-Z letters passes through unchanged, so
  // non-ISO values / blanks degrade gracefully to plain text. (Deliberate
  // deviation from the design system's no-emoji rule, per user request.)
  function countryFlag(code) {
    var c = String(code || '').trim();
    if (!/^[A-Za-z]{2}$/.test(c)) return c;
    var up = c.toUpperCase();
    var flag = String.fromCodePoint(0x1F1E6 + up.charCodeAt(0) - 65) +
               String.fromCodePoint(0x1F1E6 + up.charCodeAt(1) - 65);
    return flag + ' ' + up;
  }

  function renderEgressTestResult(panelEl, result, httpOK) {
    while (panelEl.firstChild) panelEl.removeChild(panelEl.firstChild);
    if (!httpOK) {
      var failLine = document.createElement('div');
      failLine.className = 'egress-test-fail';
      failLine.textContent = '✗ ' + ((result && result.error) ? String(result.error) : String(result));
      panelEl.appendChild(failLine);
      return;
    }
    var statusLine = document.createElement('div');
    var status = String(result.status || 'UNKNOWN');
    var sym = status === 'OK' ? '✓' : '✗';
    statusLine.className = status === 'OK' ? 'egress-test-ok' : 'egress-test-fail';
    // latency_ms is always present on the wire (server-side json:
    // "latency_ms" without omitempty); 0 is a real value (sub-ms probe
    // or pre-network failure timing). Coerce defensively for malformed
    // input so the user always sees a number rather than "—".
    var latMs = (typeof result.latency_ms === 'number') ? result.latency_ms : 0;
    statusLine.textContent = sym + ' ' + status + ' · ' + latMs + ' ms';
    panelEl.appendChild(statusLine);

    function row(label, value) {
      if (!value) return;
      var r = document.createElement('div');
      r.className = 'egress-test-row-field';
      var k = document.createElement('span');
      k.className = 'egress-test-key';
      k.textContent = label;
      var v = document.createElement('span');
      v.className = 'egress-test-val';
      // textContent — never innerHTML for third-party data (org, etc.).
      v.textContent = String(value);
      r.appendChild(k);
      r.appendChild(v);
      panelEl.appendChild(r);
    }

    if (status === 'OK') {
      row('IP', result.public_ip);
      var geoParts = [];
      if (result.city) geoParts.push(result.city);
      if (result.region) geoParts.push(result.region);
      if (result.country) geoParts.push(countryFlag(result.country));
      if (geoParts.length > 0) row('geo', geoParts.join(', '));
      row('org', result.org);
    }
    row('via', result.egress_via);
    if (result.err) row('note', result.err);
  }

  function renderEgressTestError(panelEl, msg) {
    while (panelEl.firstChild) panelEl.removeChild(panelEl.firstChild);
    var p = document.createElement('div');
    p.className = 'egress-test-fail';
    p.textContent = '✗ network error: ' + msg;
    panelEl.appendChild(p);
  }

  // enhancePostureEgressCell: progressive enhancement of the existing
  // posture ribbon's egress cell. Adds a hover-revealed probe icon-
  // button that surfaces the active session's actual exit IP / geo /
  // latency inline, condensed into a single line below the existing
  // value (which keeps showing the configured URL).
  //
  // No new permanent UI surface — the button is opacity:0 until the
  // cell is hovered or focused (matches the .pop-action reveal pattern
  // used elsewhere). The result line is :empty → display:none so it
  // collapses when no probe has run.
  function enhancePostureEgressCell() {
    var cell = document.querySelector('.ribbon-cell[data-ribbon="Egress"]');
    if (!cell) return;
    if (cell.dataset.egressEnhanced === '1') return; // idempotent
    cell.dataset.egressEnhanced = '1';

    // Full egress value on hover — the cell nowrap+ellipsises long proxy URLs.
    var vEl = cell.querySelector('[data-ribbon-value]');
    if (vEl) vEl.title = (vEl.textContent || '').trim();

    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'egress-probe-btn';
    btn.title = 'probe live egress';
    btn.setAttribute('aria-label', 'probe live egress');
    btn.appendChild(svgIcon('i-zap'));
    cell.appendChild(btn);

    var resultLine = document.createElement('div');
    resultLine.className = 'egress-cell-result';
    resultLine.setAttribute('aria-live', 'polite');
    cell.appendChild(resultLine);

    btn.addEventListener('click', function () {
      if (btn.disabled) return;
      btn.disabled = true;
      btn.replaceChildren(svgIcon('i-loader'));
      btn.firstChild.classList.add('spin');
      btn.title = 'probing…';
      resultLine.replaceChildren();

      fetch('/profile/test-egress', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CCWRAP-Profile-Token': PROFILE_TOKEN
        },
        body: JSON.stringify({ name: '<active-session>' })
      })
        .then(function (r) {
          // Any non-2xx is plain text from http.Error — same rationale
          // as the popover handler. Reading 5xx as JSON would throw.
          if (!r.ok) {
            return r.text().then(function (body) {
              return { ok: false, body: { error: (body || '').trim() } };
            });
          }
          return r.json().then(function (b) { return { ok: true, body: b }; });
        })
        .then(function (resp) { renderEgressCellResult(resultLine, resp.body, resp.ok); })
        .catch(function (err) {
          renderEgressCellResult(resultLine, { error: (err && err.message) ? err.message : String(err) }, false);
        })
        .then(function () {
          btn.disabled = false;
          btn.replaceChildren(svgIcon('i-zap'));
          btn.title = 'probe live egress';
          btn.setAttribute('aria-label', 'probe live egress');
        });
    });
  }

  // renderEgressCellResult writes a single condensed line:
  //   ✓ 52.34.x.x · Seattle, WA, US · AS16509 Amazon · 142ms
  //   ✗ NET_FAIL · dial tcp ...
  // textContent only — third-party data (org, geo) passes through
  // unsanitized so innerHTML interpolation is never used.
  function renderEgressCellResult(el, result, httpOK) {
    el.replaceChildren();
    if (!httpOK || !result) {
      var fail = document.createElement('span');
      fail.className = 'egress-cell-result-fail';
      fail.textContent = '✗ ' + ((result && result.error) ? String(result.error) : String(result));
      el.appendChild(fail);
      return;
    }
    var status = String(result.status || 'UNKNOWN');
    var parts = [];
    // latency_ms is always serialized — 0 is a real value (see
    // renderEgressTestResult for context).
    var cellLatMs = (typeof result.latency_ms === 'number') ? result.latency_ms : 0;
    if (status === 'OK') {
      if (result.public_ip) parts.push(String(result.public_ip));
      var geo = [];
      if (result.city) geo.push(String(result.city));
      if (result.region) geo.push(String(result.region));
      if (result.country) geo.push(countryFlag(result.country));
      if (geo.length) parts.push(geo.join(', '));
      if (result.org) parts.push(String(result.org));
      parts.push(cellLatMs + 'ms');
    } else {
      parts.push(status);
      parts.push(cellLatMs + 'ms');
      if (result.err) parts.push(String(result.err));
    }
    var sym = document.createElement('span');
    sym.className = status === 'OK' ? 'egress-cell-result-ok' : 'egress-cell-result-fail';
    sym.textContent = (status === 'OK' ? '✓ ' : '✗ ') + parts.join(' · ');
    el.appendChild(sym);
  }

  var testAllAbort = null;

  // testAllMaxConcurrency caps simultaneous in-flight /profile/test
  // requests during a Test-all fan-out. Without the cap, 30+ profiles
  // would each spawn a 15s-timeout server-side probe goroutine + an
  // outbound TLS handshake at the same moment — meaningful resource
  // pressure on a constrained box and easy fd exhaustion. UI feel:
  // results trickle in groups of 4, which is also less overwhelming
  // visually than 30 chips flipping at once.
  var testAllMaxConcurrency = 4;

  // runPromisesWithLimit dispatches runOne(task) for each task,
  // keeping at most "limit" invocations in flight at once. Returns a
  // Promise that resolves when every task has settled.
  function runPromisesWithLimit(items, limit, runOne) {
    return new Promise(function(resolve) {
      var total = items.length;
      if (total === 0) { resolve(); return; }
      var idx = 0;
      var done = 0;
      function pump() {
        while (idx < total) {
          if ((idx - done) >= limit) return;
          var i = idx++;
          Promise.resolve(runOne(items[i])).then(function(){
            done++;
            if (done === total) resolve();
            else pump();
          });
        }
      }
      pump();
    });
  }

  function onProfileTestAllClick(btnEl) {
    // Abort any in-flight test-all (re-click = restart, not queue).
    if (testAllAbort) { try { testAllAbort.abort(); } catch(_){} }
    testAllAbort = (typeof AbortController !== 'undefined') ? new AbortController() : null;
    var signal = testAllAbort ? testAllAbort.signal : undefined;

    btnEl.disabled = true;
    var originalText = btnEl.textContent;
    btnEl.textContent = 'Testing…';

    // Phase 1: collect testable rows + do their DOM setup synchronously
    // (state-machine button swap). Phase 2 runs the fetches through a
    // concurrency-bounded pool so we never have more than
    // testAllMaxConcurrency outbound probes in flight.
    var rows = (popoverEl || document).querySelectorAll('.sp3-pop-row');
    var tasks = [];
    rows.forEach(function(rowEl){
      var name = rowEl.dataset.profileName || (function(){
        var n = rowEl.querySelector('.pop-name');
        return n ? n.textContent : '';
      })();
      if (!name) return;
      // Search for either the icon button (data-action="test-slot")
      // OR an existing chip from a previous probe. The legacy selector
      // (.pop-test-btn) no longer matches since real-profile rows now use
      // .pop-action icon buttons.
      var existing = rowEl.querySelector('[data-action="test-slot"], .pop-test-chip');
      // Recreate a fresh test button to drive the state machine — this
      // also overwrites any existing chip ("force fresh").
      var newBtn = iconButton('i-activity', 'test connectivity');
      newBtn.setAttribute('data-action', 'test-slot');
      newBtn.textContent = '…';
      newBtn.disabled = true;
      if (existing && existing.parentNode) {
        existing.parentNode.replaceChild(newBtn, existing);
      } else {
        // No existing [test] button (row was rendered without one, e.g.
        // inherit-env in OAuth mode where env has no credentials). Skip —
        // it's deliberately untestable.
        return;
      }
      tasks.push({ rowEl: rowEl, name: name, newBtn: newBtn });
    });

    function runOneTask(task) {
      return fetch('/profile/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CCWRAP-Profile-Token': PROFILE_TOKEN },
        body: JSON.stringify({ name: task.name }),
        signal: signal,
      }).then(function(r){
        // Any non-2xx is plain text from http.Error — render with the
        // backend's error text in tooltip, status HTTP_4XX/5XX based
        // on actual code. Distinct from NET_FAIL (transport failures).
        if (!r.ok) {
          return r.text().then(function(body){
            var chipStatus = (r.status >= 500) ? 'HTTP_5XX' : 'HTTP_4XX';
            return { profile: task.name, status: chipStatus, http_status: r.status, error: (body || '').trim() };
          });
        }
        return r.json();
      }).then(function(result){
        renderProfileTestChip(result, task.rowEl, task.newBtn);
      }).catch(function(err){
        if (err && err.name === 'AbortError') return;
        renderProfileTestChip({ profile: task.name, status: 'NET_FAIL', error: 'fetch failed: ' + (err && err.message) }, task.rowEl, task.newBtn);
      });
    }

    runPromisesWithLimit(tasks, testAllMaxConcurrency, runOneTask).then(function(){
      btnEl.disabled = false;
      btnEl.textContent = originalText;
      testAllAbort = null;
    }).catch(function(){
      btnEl.disabled = false;
      btnEl.textContent = originalText;
      testAllAbort = null;
    });
  }

  // onCatalogRowClick is currently UNWIRED: row-body click was retired in
  // favor of the explicit play/default action icons. It is kept (not
  // deleted) because the reconciliation gate it drives — pending state,
  // popPendingSSESnapshot buffering, reconcileFetchFailure — is a pinned
  // contract (web tests assert the machinery) and the intended home for a
  // future re-wire of any in-popover switch path.
  function onCatalogRowClick(name, rowEl) {
    if (popState !== 'loaded') return;
    setPopState('pending');
    popClickedName = name;
    popPreClickActiveProfileName = (state.session && state.session.active_profile_name) || '';
    popPendingSSESnapshot = null;
    rowEl.classList.add('pending');
    var radio = rowEl.querySelector('.pop-radio');
    if (radio) radio.textContent = '…';

    fetch('/profile/switch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CCWRAP-Profile-Token': PROFILE_TOKEN },
      body: JSON.stringify({ name: name })
    }).then(function(r){
      if (!r.ok && r.status !== 200) throw new Error('http ' + r.status);
      return r.json();
    }).then(function(outcome){
      renderOutcome(outcome, rowEl);
    }).catch(function(err){
      reconcileFetchFailure();
    });
  }

  function renderOutcome(outcome, rowEl) {
    setPopState('loaded');
    var block = document.createElement('div');
    block.className = 'outcome';
    var result = String(outcome.result || '');
    if (result === 'switched') {
      block.className += ' success';
      var msg = '✓ switched — active profile is now ' + ((outcome.view && outcome.view.name) || popClickedName || 'inherit-env');
      block.textContent = msg;
      // Patch chip/cell immediately (don't wait for SSE).
      if (state.session) {
        state.session.active_profile_name = (outcome.view && outcome.view.name) || '';
        state.session.active_profile_provider = (outcome.view && outcome.view.provider) || '';
      }
      updateProfileCell();
      popPendingSSESnapshot = null;
    } else if (result === 'refused_needs_relaunch') {
      block.className += ' warn';
      var targetName = (outcome.view && outcome.view.name) || popClickedName || '';
      var activeName = (state.session && state.session.active_profile_name) || '';
      var headline = document.createElement('div');
      headline.appendChild(document.createTextNode('⚠ relaunch required — switch to '));
      var tb = document.createElement('b');
      tb.textContent = '"' + targetName + '"';
      headline.appendChild(tb);
      headline.appendChild(document.createTextNode(' needs first-party auth.'));
      block.appendChild(headline);
      if (activeName) {
        var reassure = document.createElement('div');
        reassure.className = 'outcome-reassure';
        reassure.appendChild(document.createTextNode('Active profile remains '));
        var ab = document.createElement('b');
        ab.textContent = '"' + activeName + '"';
        reassure.appendChild(ab);
        reassure.appendChild(document.createTextNode('.'));
        block.appendChild(reassure);
      }
      var cmd = document.createElement('div');
      cmd.className = 'cmd';
      cmd.textContent = '$ ccwrap --profile ' + targetName;
      cmd.title = 'click to copy the relaunch command';
      cmd.addEventListener('click', function(ev){
        ev.stopPropagation();
        // Copy WITHOUT the "$ " prompt prefix — paste-ready.
        if (copyText('ccwrap --profile ' + targetName)) showToast('relaunch command copied', null);
      });
      block.appendChild(cmd);
    } else if (result === 'rejected_invalid') {
      block.className += ' danger';
      block.textContent = '✗ rejected — ' + (outcome.message || 'invalid switch');
    } else if (result === 'no_such_session') {
      block.className += ' danger';
      block.textContent = '✗ session ended — reload page';
    } else if (result === 'no_profiles_file') {
      // File disappeared between popover open and switch.
      block.className += ' danger';
      block.textContent = '✗ profiles.json missing';
      setPopState('missing-file');
    } else if (result === 'unknown_profile') {
      block.className += ' danger';
      block.textContent = '✗ unknown profile — catalog stale, refetching';
      setTimeout(fetchCatalog, 0);
    } else {
      block.className += ' danger';
      block.textContent = '✗ ' + (outcome.message || result || 'switch failed');
    }
    var dismiss = document.createElement('a');
    dismiss.className = 'dismiss';
    dismiss.href = '#';
    dismiss.textContent = 'dismiss ×';
    dismiss.addEventListener('click', function(ev){ ev.preventDefault(); block.remove(); });
    block.appendChild(dismiss);
    if (popoverEl) popoverEl.appendChild(block);
    if (rowEl) {
      rowEl.classList.remove('pending');
      var radio = rowEl.querySelector('.pop-radio');
      if (radio) {
        // Normalize inherit-env: backend session.active_profile_name
        // is '' for inherit-env mode, while the inherit-env row's dataset.profileName
        // is the 'inherit-env' literal. Match both representations.
        var snapAP = (state.session && state.session.active_profile_name) || '';
        var rowAP = rowEl.dataset.profileName || '';
        var active = (snapAP === rowAP) || (snapAP === '' && rowAP === 'inherit-env');
        radio.textContent = active ? '●' : '○';
      }
    }
  }

  function reconcileFetchFailure() {
    // Reconciliation gate: if an SSE snapshot arrived
    // during the in-flight POST and its ActiveProfileName matches what the
    // user clicked, the switch DID land (the HTTP ACK was the only thing
    // that dropped). Apply the buffered snapshot and render success.
    // Normalize inherit-env: snapshot.active_profile_name is ''
    // for inherit-env mode; popClickedName is 'inherit-env' literal. Treat
    // both representations as matching for the success-via-buffer branch.
    if (popPendingSSESnapshot && (function(){
        var snap = popPendingSSESnapshot.active_profile_name || '';
        var click = popClickedName || '';
        return (snap === click) || (snap === '' && click === 'inherit-env');
    })()) {
      setPopState('loaded');
      if (state.session) {
        state.session.active_profile_name = popPendingSSESnapshot.active_profile_name || '';
        state.session.active_profile_provider = popPendingSSESnapshot.active_profile_provider || '';
      }
      updateProfileCell();
      var block = document.createElement('div');
      block.className = 'outcome success';
      block.textContent = '✓ switch applied — local network blip during ACK';
      if (popoverEl) popoverEl.appendChild(block);
      popPendingSSESnapshot = null;
      return;
    }
    // Compare to pre-click value: if SSE shows a DIFFERENT active profile
    // than what was on the page at click-instant, ANOTHER concurrent switch
    // landed — refetch the catalog rather than render stale.
    if (popPendingSSESnapshot &&
        (popPendingSSESnapshot.active_profile_name || '') !== (popPreClickActiveProfileName || '')) {
      fetchCatalog();
      return;
    }
    // Otherwise: no SSE arrived OR server posture truly unchanged.
    setPopState('loaded');
    var dblock = document.createElement('div');
    dblock.className = 'outcome danger';
    dblock.textContent = '✗ switch failed — check network and retry';
    if (popoverEl) popoverEl.appendChild(dblock);
  }

  // ============================================================
  // Edit/add panel rendering: panel scaffolding + form construction,
  // dirty detection + cancel/save actions row, submit handler +
  // validation rendering. popoverCtx (declared above) gets the
  // openEditPanel / openAddPanel entry points wired to these functions.
  // ============================================================

  function panelBack() {
    // Restore the list view from cached children. popoverEl typically holds
    // a single .sp3-pop-body child (built by renderPopoverShell), which we
    // cached on entry to the panel.
    if (!popoverCtx.listChildrenCache) return;
    popoverEl.replaceChildren.apply(popoverEl, popoverCtx.listChildrenCache);
    popoverCtx.editTarget = null;
  }

  function renderEditOrAddPanel(item, mode) {
    // mode: 'edit' or 'add'
    // Cache existing children if not already cached.
    if (!popoverCtx.listChildrenCache) {
      popoverCtx.listChildrenCache = Array.prototype.slice.call(popoverEl.children);
    }
    popoverEl.replaceChildren();

    // Build back-row
    var head = document.createElement('div');
    head.className = 'sp3-pop-edit-head';
    var backBtn = document.createElement('button');
    backBtn.type = 'button';
    backBtn.className = 'sp3-pop-edit-back';
    backBtn.title = 'back to list';
    backBtn.appendChild(svgIcon('i-back'));
    backBtn.addEventListener('click', panelBack);
    head.appendChild(backBtn);
    var title = document.createElement('span');
    title.className = 'title';
    title.textContent = mode === 'add' ? 'New profile' : item.name;
    head.appendChild(title);
    // Removed the "EDIT" / "ADD" badge — the back arrow + the
    // changed title ("New profile" vs profile name) already convey the
    // mode, the badge was redundant noise.
    popoverEl.appendChild(head);

    // Build error block (initially empty + hidden)
    var errBlock = document.createElement('div');
    errBlock.className = 'sp3-pop-edit-err';
    errBlock.style.display = 'none';
    popoverEl.appendChild(errBlock);

    // Build form body
    var form = buildEditForm(item, mode);
    popoverEl.appendChild(form);

    // editTarget is created before actions so buildEditActions can stash
    // the original snapshot onto it (used by collectEditPayload diff).
    popoverCtx.editTarget = { item: item, mode: mode, form: form, errBlock: errBlock, actionsEl: null };

    // Actions row — populated with cancel + save buttons + dirty detection.
    var actions = buildEditActions(mode, form, errBlock);
    popoverEl.appendChild(actions);
    popoverCtx.editTarget.actionsEl = actions;

    // Focus first input
    var firstInput = form.querySelector('input:not([disabled]), select');
    if (firstInput) firstInput.focus();
  }

  function buildEditForm(item, mode) {
    var body = document.createElement('div');
    body.className = 'sp3-pop-edit-body';

    // Identity section
    var idSection = document.createElement('div');
    idSection.className = 'sp3-pop-edit-section';
    appendSectionLabel(idSection, 'Identity');
    if (mode === 'add') {
      appendField(idSection, 'name *', { name: 'name', dataField: 'name', placeholder: 'e.g. glm, anthropic' });
    }
    appendField(idSection, 'provider', { name: 'provider', dataField: 'provider', value: item.provider || '', placeholder: 'optional group label' });
    appendField(idSection, mode === 'add' ? 'base_url *' : 'base_url', { name: 'base_url', dataField: 'base_url', value: item.base_url || '', placeholder: 'https://api.example.com' });
    body.appendChild(idSection);

    // Auth section
    var authSection = document.createElement('div');
    authSection.className = 'sp3-pop-edit-section';
    appendSectionLabel(authSection, 'Auth');

    // Single mode dropdown — "passthrough" is just a mode value, not a
    // separate toggle. When mode = passthrough, the key-source + key
    // input rows hide entirely (display:none) and the save payload
    // emits auth:null. When mode = ccwrap_*, the material rows show
    // and a full auth block is sent. Replaces the old
    // checkbox + .sp3-pop-auth-body[data-disabled] pattern.
    var initialAuthMode = (mode === 'add')
      ? 'ccwrap_bearer'
      : (item.auth ? item.auth.mode : 'passthrough');
    // Wire values stay ccwrap_* (submit logic reads them); the labels array
    // shows provider-neutral display text. The header/env-slot detail lives in
    // the hint line below, not in the option text.
    var authModeSelect = appendSelect(authSection, mode === 'add' ? 'mode *' : 'mode', 'auth_mode',
      ['passthrough', 'ccwrap_bearer', 'ccwrap_x_api_key'], initialAuthMode,
      ['passthrough', 'Bearer token', 'x-api-key']);

    // Muted hint under the mode select — names the exact header the credential
    // rides in + the equivalent env-var slot, so the terse labels stay clear.
    var authHint = document.createElement('div');
    authHint.className = 'auth-mode-hint';
    authSection.appendChild(authHint);
    function refreshAuthHint() { authHint.textContent = authModeHint(authModeSelect.value); }
    refreshAuthHint();

    // Auth material — key source + key input. Hidden when mode is
    // passthrough; the form snapshot still captures any values entered,
    // so flipping back to ccwrap_* restores them.
    var authMaterial = document.createElement('div');
    authMaterial.className = 'pop-auth-material';
    appendKeyField(authMaterial, mode);
    authMaterial.style.display = (initialAuthMode === 'passthrough') ? 'none' : '';
    authSection.appendChild(authMaterial);

    authModeSelect.addEventListener('change', function () {
      authMaterial.style.display = (authModeSelect.value === 'passthrough') ? 'none' : '';
      refreshAuthHint();
    });
    body.appendChild(authSection);

    // Model aliases section. Rows of <from> → <to> + delete; "+ add"
    // button appends a fresh empty row. Serialized to a hidden field for
    // snapshot diffing; collected into payload on submit.
    var aliasSection = document.createElement('div');
    aliasSection.className = 'sp3-pop-edit-section sp3-pop-aliases';
    appendSectionLabel(aliasSection, 'Model aliases');
    var aliasList = document.createElement('div');
    aliasList.className = 'sp3-pop-aliases-list';
    aliasSection.appendChild(aliasList);
    // P10: empty-state hint — only visible when no alias rows exist.
    // CSS hides it once a row is appended (via :empty + ~ selector); the
    // serializer doesn't need to know about it.
    var aliasEmpty = document.createElement('div');
    aliasEmpty.className = 'sp3-pop-aliases-empty';
    aliasEmpty.textContent = 'no alias overrides — Claude’s model names pass through unchanged';
    aliasSection.appendChild(aliasEmpty);
    var aliasHidden = document.createElement('input');
    aliasHidden.type = 'hidden';
    aliasHidden.setAttribute('data-field', 'model_aliases_serialized');
    aliasHidden.value = '';
    aliasSection.appendChild(aliasHidden);
    var aliasAddBtn = document.createElement('button');
    aliasAddBtn.type = 'button';
    aliasAddBtn.className = 'sp3-pop-alias-add';
    aliasAddBtn.textContent = '+ add alias';
    aliasAddBtn.addEventListener('click', function(ev){
      ev.preventDefault();
      appendAliasRow(aliasList, aliasHidden, '', '');
    });
    aliasSection.appendChild(aliasAddBtn);
    // Prefill from item.model_aliases (object or undefined). Sorted by key
    // for deterministic row order.
    var initialAliases = (item && item.model_aliases) || {};
    var keys = Object.keys(initialAliases).sort();
    for (var ai = 0; ai < keys.length; ai++) {
      appendAliasRow(aliasList, aliasHidden, keys[ai], initialAliases[keys[ai]]);
    }
    serializeAliasHidden(aliasList, aliasHidden);
    body.appendChild(aliasSection);

    // Egress section
    var routeSection = document.createElement('div');
    routeSection.className = 'sp3-pop-edit-section';
    appendSectionLabel(routeSection, 'Egress');
    var initialEgressMode = item.egress_mode || 'inherit';
    // Merged "via [mode][url]" row: the mode select is glued to the url input
    // as one control (design system merged field), instead of two stacked rows.
    var egF = document.createElement('div');
    egF.className = 'sp3-pop-edit-field';
    var egLbl = document.createElement('label');
    egLbl.textContent = 'via';
    egF.appendChild(egLbl);
    var egMerged = document.createElement('div');
    egMerged.className = 'pop-keyfield';
    var egSel = document.createElement('select');
    egSel.className = 'pop-keysrc-sel';
    egSel.setAttribute('data-field', 'egress_mode');
    var egModes = ['inherit','direct','http','socks5','socks5h'];
    for (var emi = 0; emi < egModes.length; emi++) {
      var emo = document.createElement('option');
      emo.value = egModes[emi];
      emo.textContent = egModes[emi];
      egSel.appendChild(emo);
    }
    egSel.value = initialEgressMode;
    egMerged.appendChild(egSel);
    var egUrl = document.createElement('input');
    egUrl.name = 'egress_url';
    egUrl.setAttribute('data-field', 'egress_url');
    egUrl.value = item.egress_url || '';
    egUrl.placeholder = egressURLPlaceholder(initialEgressMode);
    // Non-proxy modes (inherit/direct) carry no url — disable the input so a
    // value can't be entered that test/save would silently ignore.
    egUrl.disabled = !egressIsProxyMode(initialEgressMode);
    egMerged.appendChild(egUrl);
    egF.appendChild(egMerged);
    // Trailing auto-column placeholder span — appendEgressTestButton slots the
    // zap [test] button INSIDE this span (it requires the row's last child to
    // be a <span>, matching appendField's 3-column grid shape).
    egF.appendChild(document.createElement('span'));
    routeSection.appendChild(egF);
    // Muted hint line under the field — describes the selected mode in plain
    // words so "inherit" is self-explanatory without widening the glued select.
    var egHint = document.createElement('div');
    egHint.className = 'egress-mode-hint';
    var modeSelect = routeSection.querySelector('[data-field="egress_mode"]');
    var urlInput = routeSection.querySelector('[data-field="egress_url"]');
    // refreshEgress repaints the mode hint and flags an incomplete proxy row
    // (proxy mode + empty url) as .invalid; the save gate (egressFieldValid)
    // keeps that incomplete row from being persisted.
    function refreshEgress() {
      if (!modeSelect || !urlInput) return;
      egHint.textContent = egressModeHint(modeSelect.value);
      urlInput.classList.toggle('invalid', !egressRowValid(modeSelect.value, urlInput.value));
    }
    if (modeSelect && urlInput) {
      modeSelect.addEventListener('change', function () {
        // Dynamic placeholder + enabled state tailored to the selected scheme:
        // only proxy modes carry a url (disabling inherit/direct removes the
        // "filled a url but test runs inherit" ambiguity).
        urlInput.placeholder = egressURLPlaceholder(modeSelect.value);
        urlInput.disabled = !egressIsProxyMode(modeSelect.value);
        refreshEgress();
      });
      urlInput.addEventListener('input', refreshEgress);
    }
    // Egress self-test button — probes the draft EgressSpec (current form
    // values) without saving, so the user can validate proxy edits before
    // committing them via [save].
    appendEgressTestButton(routeSection, item);
    // Slot the mode hint between the field row and the test-result panel
    // (appendEgressTestButton inserts that panel immediately after egF).
    egF.insertAdjacentElement('afterend', egHint);
    refreshEgress();
    body.appendChild(routeSection);

    return body;
  }

  // appendAliasRow appends one <from> → <to> [×] row to listEl and wires
  // input/click handlers to re-serialize into hiddenEl on any change.
  function appendAliasRow(listEl, hiddenEl, fromVal, toVal) {
    var row = document.createElement('div');
    row.className = 'sp3-pop-alias-row';
    var fromInput = document.createElement('input');
    fromInput.className = 'alias-from';
    fromInput.placeholder = 'claude name (e.g. sonnet)';
    fromInput.value = fromVal || '';
    var arrow = document.createElement('span');
    arrow.className = 'alias-arrow';
    arrow.textContent = '→';
    var toInput = document.createElement('input');
    toInput.className = 'alias-to';
    toInput.placeholder = 'upstream model';
    toInput.value = toVal || '';
    var rmBtn = document.createElement('button');
    rmBtn.type = 'button';
    rmBtn.className = 'alias-rm';
    rmBtn.title = 'remove this alias';
    rmBtn.textContent = '✕';
    function reserialize() { serializeAliasHidden(listEl, hiddenEl); }
    fromInput.addEventListener('input', reserialize);
    toInput.addEventListener('input', reserialize);
    rmBtn.addEventListener('click', function(ev){
      ev.preventDefault();
      row.remove();
      reserialize();
    });
    row.appendChild(fromInput);
    row.appendChild(arrow);
    row.appendChild(toInput);
    row.appendChild(rmBtn);
    listEl.appendChild(row);
  }

  // serializeAliasHidden writes a sorted key=value;...  string into the
  // hidden input so captureFormSnapshot/isFormDirty pick up alias edits
  // through the same data-field machinery as other inputs. Dispatches an
  // input event so form-level listeners (recomputeDirty) re-evaluate the
  // save button — programmatic value sets don't fire input natively,
  // which would leave save disabled after a row-delete (or any other
  // path that mutates the hidden field without a user input).
  function serializeAliasHidden(listEl, hiddenEl) {
    var pairs = [];
    var rows = listEl.querySelectorAll('.sp3-pop-alias-row');
    for (var i = 0; i < rows.length; i++) {
      var from = rows[i].querySelector('.alias-from').value.trim();
      var to = rows[i].querySelector('.alias-to').value.trim();
      if (from === '' && to === '') continue; // empty row, ignore
      pairs.push(from + '=' + to);
    }
    pairs.sort();
    var next = pairs.join(';');
    if (hiddenEl.value === next) return;
    hiddenEl.value = next;
    hiddenEl.dispatchEvent(new Event('input', { bubbles: true }));
  }

  // collectAliasesFromForm reads the active alias rows into a plain
  // object {from: to} suitable for the submit payload. Skips fully-empty
  // rows; preserves partial rows so server-side validation can reject.
  function collectAliasesFromForm(form) {
    var out = {};
    var rows = form.querySelectorAll('.sp3-pop-alias-row');
    for (var i = 0; i < rows.length; i++) {
      var from = rows[i].querySelector('.alias-from').value.trim();
      var to = rows[i].querySelector('.alias-to').value.trim();
      if (from === '' && to === '') continue;
      out[from] = to;
    }
    return out;
  }

  function appendSectionLabel(parent, text) {
    var lbl = document.createElement('div');
    lbl.className = 'sp3-pop-edit-section-label';
    lbl.textContent = text;
    parent.appendChild(lbl);
  }

  function appendField(parent, labelText, opts) {
    var f = document.createElement('div');
    f.className = 'sp3-pop-edit-field';
    var lbl = document.createElement('label');
    lbl.textContent = labelText;
    f.appendChild(lbl);
    var input = document.createElement('input');
    input.name = opts.name;
    input.setAttribute('data-field', opts.dataField);
    if (opts.placeholder) input.placeholder = opts.placeholder;
    if (opts.value !== undefined) input.value = opts.value;
    f.appendChild(input);
    // Third grid column placeholder
    f.appendChild(document.createElement('span'));
    parent.appendChild(f);
    return input;
  }

  // appendSelect builds a labelled <select>. options are the wire VALUES; an
  // optional parallel labels array supplies friendlier display text per option
  // (the value still drives data-field/.value, so submit logic is unaffected).
  function appendSelect(parent, labelText, fieldName, options, value, labels) {
    var f = document.createElement('div');
    f.className = 'sp3-pop-edit-field';
    var lbl = document.createElement('label');
    lbl.textContent = labelText;
    f.appendChild(lbl);
    var sel = document.createElement('select');
    sel.name = fieldName;
    sel.setAttribute('data-field', fieldName);
    for (var i = 0; i < options.length; i++) {
      var opt = document.createElement('option');
      opt.value = options[i];
      opt.textContent = (labels && labels[i] != null) ? labels[i] : options[i];
      if (options[i] === value) opt.selected = true;
      sel.appendChild(opt);
    }
    f.appendChild(sel);
    f.appendChild(document.createElement('span'));
    parent.appendChild(f);
    return sel;
  }

  // egressURLPlaceholder returns one scheme-tailored example URL for the
  // egress url input. The mode dropdown drives this via a change listener
  // so the placeholder always reflects the currently-selected scheme.
  function egressURLPlaceholder(mode) {
    switch (mode) {
      case 'http': return 'http://proxy:8080';
      case 'socks5': return 'socks5://proxy:1080';
      case 'socks5h': return 'socks5h://proxy:1080';
      default: return '';
    }
  }

  // applyKeySourceMode sets every source-dependent property of the key field for
  // the given source ('inline' | 'env_var') and mode ('add' | 'edit'). It is the
  // SINGLE source of truth for the label, placeholder, input type, hidden source
  // value, eye visibility, and toggle copy — called on initial build AND on every
  // toggle. Pure w.r.t. the passed els object (no DOM lookups), which keeps it
  // node-testable in isolation.
  //   inline  -> masked secret  (type=password, eye shown), "paste secret here"
  //   env_var -> clear-text NAME (type=text, eye hidden), "env var name (e.g. ...)"
  function applyKeySourceMode(els, source, mode) {
    var isEnv = source === 'env_var';
    var add = mode === 'add';
    els.hidden.value = source;
    els.label.textContent = (isEnv ? 'env var' : 'key') + (add ? ' *' : '');
    els.input.placeholder = isEnv
      ? (add ? 'env var name (e.g. OPENAI_API_KEY)' : 'env var name, blank keeps current')
      : (add ? 'paste secret here' : 'blank keeps the stored key');
    els.input.type = isEnv ? 'text' : 'password';
    els.reveal.hidden = isEnv;
    els.toggle.textContent = isEnv ? 'enter the key directly' : 'use an environment variable';
  }

  // appendKeyField builds the key row plus a progressive-disclosure source
  // toggle. DEFAULT state is just a "key" input — you paste the secret. A quiet
  // toggle below switches the SOURCE to an environment variable; only then does
  // the field become "env var" (you type the variable NAME, shown in clear text
  // since a name isn't a secret). There is NO source <select>: the current
  // source lives in a hidden [data-field=key_source] input ('inline'|'env_var')
  // that collectEditPayload / isAddFormReady / the form snapshot all read
  // UNCHANGED. In EDIT mode a BLANK input preserves the stored secret (submit
  // omits key/key_env when empty; the backend treats absent key_source as a
  // no-op). In ADD mode a value is required.
  function appendKeyField(parent, mode) {
    var f = document.createElement('div');
    f.className = 'sp3-pop-edit-field';
    f.setAttribute('data-field-wrapper', 'auth_key_pair');
    var lbl = document.createElement('label');
    f.appendChild(lbl);

    var input = document.createElement('input');
    input.name = 'auth_key';
    input.setAttribute('data-field', 'auth_key');
    f.appendChild(input);

    // Reveal eye — key mode only (applyKeySourceMode hides it for env_var, where
    // the variable NAME is shown in clear text). Masks/unmasks the secret.
    var reveal = document.createElement('button');
    reveal.type = 'button';
    reveal.className = 'pop-reveal';
    reveal.title = 'show key value (caution: visible on screen)';
    reveal.appendChild(svgIcon('i-eye'));
    reveal.addEventListener('click', function(ev){
      ev.preventDefault();
      var nextHidden = input.type === 'text';
      input.type = nextHidden ? 'password' : 'text';
      reveal.replaceChildren(svgIcon(nextHidden ? 'i-eye' : 'i-eye-off'));
      reveal.title = nextHidden ? 'show key value (caution: visible on screen)' : 'hide key value';
    });
    f.appendChild(reveal);
    parent.appendChild(f);

    // Hidden source field — the wire contract every downstream reader expects.
    var hidden = document.createElement('input');
    hidden.type = 'hidden';
    hidden.setAttribute('data-field', 'key_source');
    parent.appendChild(hidden);

    // Quiet progressive-disclosure toggle, aligned under the input column.
    var toggle = document.createElement('button');
    toggle.type = 'button';
    toggle.className = 'pop-keysrc-toggle';
    var els = { label: lbl, input: input, hidden: hidden, reveal: reveal, toggle: toggle };
    toggle.addEventListener('click', function(ev){
      ev.preventDefault();
      var next = hidden.value === 'env_var' ? 'inline' : 'env_var';
      applyKeySourceMode(els, next, mode);
      // Returning to key mode re-masks: reset the reveal icon to match type.
      if (next === 'inline') {
        reveal.replaceChildren(svgIcon('i-eye'));
        reveal.title = 'show key value (caution: visible on screen)';
      }
      // Programmatic value changes don't fire 'input' — dispatch so the form's
      // recomputeDirty (and add-form readiness) re-evaluates the save button.
      hidden.dispatchEvent(new Event('input', { bubbles: true }));
    });
    parent.appendChild(toggle);

    // Initial paint — default source is inline (key). Blank-keep semantics make
    // this safe even for a stored env_var profile (an untouched blank field
    // preserves whatever is on disk).
    applyKeySourceMode(els, 'inline', mode);
  }

  function appendCheckbox(parent, fieldName, labelText) {
    var wrapper = document.createElement('label');
    wrapper.style.display = 'flex';
    wrapper.style.gap = '8px';
    wrapper.style.alignItems = 'center';
    wrapper.style.color = 'var(--text)';
    wrapper.style.fontSize = '10px';
    wrapper.style.padding = '4px 0';
    var cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.name = fieldName;
    cb.setAttribute('data-field', fieldName);
    wrapper.appendChild(cb);
    wrapper.appendChild(document.createTextNode(labelText));
    parent.appendChild(wrapper);
  }


  // ============================================================
  // Edit-panel actions row + dirty detection.
  // ============================================================
  // buildEditActions wires the cancel + save buttons and listens on the
  // form for changes that flip the save button's disabled state.
  function buildEditActions(mode, form, errBlock) {
    var actions = document.createElement('div');
    actions.className = 'sp3-pop-edit-actions';

    // set-default checkbox lives in the action row (left side) so it
    // visually couples with [save] — it's a commit-time decision, not a
    // form field. flex:1 on the label pushes cancel/save to the right.
    var setDefaultLabel = document.createElement('label');
    setDefaultLabel.className = 'pop-set-default';
    var setDefaultCb = document.createElement('input');
    setDefaultCb.type = 'checkbox';
    setDefaultCb.name = 'set_default';
    setDefaultCb.setAttribute('data-field', 'set_default');
    setDefaultLabel.appendChild(setDefaultCb);
    setDefaultLabel.appendChild(document.createTextNode('set as default for next launch'));
    setDefaultLabel.title = 'sets the default profile in profiles.json (used by the next ccwrap launch). It does not switch the current session — use the activate button on a profile row to switch this session.';
    actions.appendChild(setDefaultLabel);

    var cancelBtn = document.createElement('button');
    cancelBtn.type = 'button';
    cancelBtn.title = 'discard changes';
    cancelBtn.appendChild(svgIcon('i-x'));
    cancelBtn.appendChild(document.createTextNode(' cancel'));
    cancelBtn.addEventListener('click', panelBack);
    actions.appendChild(cancelBtn);

    var saveBtn = document.createElement('button');
    saveBtn.type = 'button';
    saveBtn.className = 'primary';
    saveBtn.disabled = mode === 'edit';   // edit starts non-dirty; add gates on required fields
    saveBtn.title = mode === 'add' ? 'create profile' : 'save changes';
    saveBtn.appendChild(svgIcon(mode === 'add' ? 'i-plus' : 'i-check'));
    saveBtn.appendChild(document.createTextNode(mode === 'add' ? ' create' : ' save'));
    saveBtn.addEventListener('click', function(ev){
      ev.preventDefault();
      submitEditPanel(mode, form, errBlock, saveBtn);
    });
    actions.appendChild(saveBtn);

    // Dirty detection: bind to inputs + selects + key-source buttons + checkbox.
    var snapshot = captureFormSnapshot(form);
    if (popoverCtx.editTarget) {
      popoverCtx.editTarget._origSnap = snapshot;
    }
    function recomputeDirty() {
      if (mode === 'edit') {
        // set_default lives outside form, so its toggle isn't in
        // snapshot/current. Treat checked → dirty (its initial state is
        // always unchecked for edit, so this is a clean overlay).
        var dirty = isFormDirty(form, snapshot) || setDefaultCb.checked;
        // ...but never let an incomplete proxy egress row (proxy mode + empty
        // url) be saved, even when other fields are dirty.
        saveBtn.disabled = !dirty || !egressFieldValid(form);
      } else {
        // For add, save is enabled when required fields are non-empty.
        saveBtn.disabled = !isAddFormReady(form);
      }
    }
    form.addEventListener('input', recomputeDirty);
    form.addEventListener('change', recomputeDirty);
    // key_source is a <select> — its change bubbles to the form 'change'
    // listener above, so no separate wiring is needed.
    // set_default checkbox lives in actions (outside form); wire its
    // change event directly so toggling it makes the save button dirty.
    setDefaultCb.addEventListener('change', recomputeDirty);

    return actions;
  }

  function captureFormSnapshot(form) {
    var snap = {};
    var inputs = form.querySelectorAll('[data-field]');
    for (var i = 0; i < inputs.length; i++) {
      var el = inputs[i];
      var key = el.getAttribute('data-field');
      if (el.tagName === 'INPUT' && el.type === 'checkbox') {
        snap[key] = el.checked;
      } else {
        snap[key] = el.value;
      }
    }
    return snap;
  }

  function isFormDirty(form, snapshot) {
    var current = captureFormSnapshot(form);
    for (var k in snapshot) {
      if (snapshot[k] !== current[k]) {
        return true;
      }
    }
    return false;
  }

  function isAddFormReady(form) {
    var name = form.querySelector('[data-field="name"]');
    var baseURL = form.querySelector('[data-field="base_url"]');
    if (!name || !baseURL) return false;
    if (!name.value.trim() || !baseURL.value.trim()) return false;
    // Egress: a proxy mode (http/socks5/socks5h) requires a url. Gate here so
    // an incomplete proxy row can't create a profile that can't route.
    if (!egressFieldValid(form)) return false;
    // Auth gating driven by mode dropdown. "passthrough" → no key
    // required (payload.auth = null). Other modes require key + source.
    var modeSel = form.querySelector('[data-field="auth_mode"]');
    if (!modeSel || !modeSel.value) return false;
    if (modeSel.value === 'passthrough') return true;
    var src = form.querySelector('[data-field="key_source"]');
    if (!src) return false;
    var srcVal = src.value;
    var key = form.querySelector('[data-field="auth_key"]');
    if (srcVal === 'inline' && (!key || !key.value)) return false;
    if (srcVal === 'env_var' && (!key || !key.value)) return false;
    return true;
  }

  // ============================================================
  // Submit handler + validation render + row splice helpers.
  // ============================================================
  // submitEditPanel POSTs to /profile/{add,edit}, dispatches success or
  // surfaces validation errors via renderEditError. The CSRF header uses
  // the PROFILE_TOKEN closure var (same as /profile/switch and
  // /capture/bodies — no per-popover token plumbing needed).
  function submitEditPanel(mode, form, errBlock, saveBtn) {
    saveBtn.disabled = true;
    var payload = collectEditPayload(form, mode);
    var endpoint = mode === 'add' ? '/profile/add' : '/profile/edit';

    fetch(endpoint, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CCWRAP-Profile-Token': PROFILE_TOKEN,
      },
      body: JSON.stringify(payload),
    })
    .then(function(resp){
      return resp.json().then(function(json){ return { status: resp.status, json: json }; });
    })
    .then(function(res){
      if (!res.json.ok) {
        renderEditError(errBlock, form, res.json);
        saveBtn.disabled = false;
        return;
      }
      onEditSuccess(res.json, mode);
    })
    .catch(function(err){
      errBlock.style.display = 'block';
      errBlock.textContent = 'request failed: ' + (err && err.message ? err.message : 'unknown');
      saveBtn.disabled = false;
    });
  }

  // collectEditPayload mirrors the wire contract in handle_profile_{add,edit}.go:
  //   add  → full envelope (provider/base_url/auth{mode,key|key_env}/egress/set_default)
  //   edit → partial diff: only fields that changed vs. _origSnap
  function collectEditPayload(form, mode) {
    var get = function(name) { return form.querySelector('[data-field="'+name+'"]'); };
    var val = function(name) { var el = get(name); return el ? el.value : ''; };
    var checked = function(name) {
      var el = get(name);
      // set_default lives in actionsEl (sibling of form), not in form.
      if (!el && popoverCtx.editTarget && popoverCtx.editTarget.actionsEl) {
        el = popoverCtx.editTarget.actionsEl.querySelector('[data-field="'+name+'"]');
      }
      return el ? el.checked : false;
    };

    var payload = { name: val('name') || (popoverCtx.editTarget && popoverCtx.editTarget.item.name) };

    // auth_mode == "passthrough" is the no-auth-block
    // sentinel. Replaces the old auth_present checkbox + auth.mode pair.
    var authMode = val('auth_mode');
    var authOn = authMode !== 'passthrough';

    if (mode === 'add') {
      payload.provider = val('provider');
      payload.base_url = val('base_url');
      if (authOn) {
        payload.auth = { mode: authMode };
        var src = form.querySelector('[data-field="key_source"]');
        if (src) {
          var srcVal = src.value;
          if (srcVal === 'inline') payload.auth.key = val('auth_key');
          if (srcVal === 'env_var') payload.auth.key_env = val('auth_key');
        }
      } else {
        payload.auth = null;
      }
      payload.egress = { mode: val('egress_mode'), url: val('egress_url') };
      // model_aliases (add): always send (may be empty); server treats
      // omitted same as empty. Explicit empty creates a profile with no
      // aliases — natural for the "default" cell state.
      payload.model_aliases = collectAliasesFromForm(form);
      payload.set_default = checked('set_default');
    } else {
      // edit: only send fields that differ from snapshot.
      var snap = (popoverCtx.editTarget && popoverCtx.editTarget._origSnap) || captureFormSnapshot(form);
      var current = captureFormSnapshot(form);
      if (current.provider !== snap.provider) payload.provider = current.provider;
      if (current.base_url !== snap.base_url) payload.base_url = current.base_url;

      // 3-state semantics driven by auth_mode value.
      // "passthrough" stands in for the old auth_present=off state.
      //   on → on:   partial-auth diff (mode/key change)
      //   on → off:  payload.auth = null (remove existing block)
      //   off → on:  payload.auth = full new block (add a block)
      //   off → off: omit auth field (no change)
      var snapOn = snap.auth_mode !== 'passthrough';
      if (authOn && !snapOn) {
        // off → on: send full new block
        var newAuth = { mode: authMode };
        var sr = form.querySelector('[data-field="key_source"]');
        if (sr) {
          var srv = sr.value;
          if (srv === 'inline') newAuth.key = val('auth_key');
          if (srv === 'env_var') newAuth.key_env = val('auth_key');
        }
        payload.auth = newAuth;
      } else if (!authOn && snapOn) {
        // on → off: explicit null to remove
        payload.auth = null;
      } else if (authOn && snapOn) {
        // both on: partial diff (mode or key changed)
        var auth = {};
        var sentAuth = false;
        if (current.auth_mode !== snap.auth_mode) { auth.mode = current.auth_mode; sentAuth = true; }
        // Key preserve rule: a BLANK key input means "keep the stored secret"
        // (there is no separate "unchanged" option anymore). Only send
        // key_source + key when the user actually typed a new value; an empty
        // input omits them, and the backend leaves the stored Key/KeyEnv as-is.
        var src2 = form.querySelector('[data-field="key_source"]');
        var keyVal = val('auth_key');
        if (src2 && keyVal) {
          auth.key_source = src2.value;
          if (src2.value === 'inline') auth.key = keyVal;
          if (src2.value === 'env_var') auth.key_env = keyVal;
          sentAuth = true;
        }
        if (sentAuth) payload.auth = auth;
      }
      // else: both off → omit auth (no change)

      var egress = {};
      var sentEgress = false;
      if (current.egress_mode !== snap.egress_mode) { egress.mode = current.egress_mode; sentEgress = true; }
      if (current.egress_url !== snap.egress_url) { egress.url = current.egress_url; sentEgress = true; }
      if (sentEgress) payload.egress = egress;

      // model_aliases (edit): if the serialized-hidden value changed from
      // snapshot, send the full new map (or {} to clear). Otherwise omit
      // → server keeps existing aliases. Pointer-3-state on the server
      // (handle_profile_edit.go) does the rest.
      if (current.model_aliases_serialized !== snap.model_aliases_serialized) {
        payload.model_aliases = collectAliasesFromForm(form);
      }

      // set_default checkbox lives in actionsEl, outside captureFormSnapshot's
      // form-scoped scan. Read it directly via the same actionsEl lookup as the
      // add-mode collector.
      if (checked('set_default')) payload.set_default = true;
    }
    return payload;
  }

  // renderEditError paints inline field-level borders + an aggregated message.
  // Use a data-field attribute match, NOT direct CSS selector
  // interpolation. error_paths come from profiles.ParseErrors and look like
  // "profiles.<name>.<field>" — strip the prefix and sanitize the remainder
  // before feeding to querySelector.
  function renderEditError(errBlock, form, resp) {
    errBlock.textContent = resp.message || ('error: ' + (resp.kind || 'unknown'));
    errBlock.style.display = 'block';
    // Clear stale red borders.
    var allFields = form.querySelectorAll('[data-field]');
    for (var i = 0; i < allFields.length; i++) {
      allFields[i].classList.remove('invalid');
    }
    if (resp.error_paths && resp.error_paths.length) {
      var pathRe = /^profiles\.[^.]+\.(.+)$/;
      for (var j = 0; j < resp.error_paths.length; j++) {
        var m = pathRe.exec(resp.error_paths[j]);
        if (!m) continue;
        var fieldName = m[1].replace(/\./g, '_');
        // Safe attribute match — fieldName is from regex capture group; sanitize further.
        var safeName = fieldName.replace(/[^a-z0-9_]/gi, '');
        if (!safeName) continue;
        var el = form.querySelector('[data-field="' + safeName + '"]');
        if (el) el.classList.add('invalid');
      }
    }
  }

  // onEditSuccess closes the panel, refreshes the catalog so the row reflects
  // the new state, and flashes the affected row. The splice helpers fall back
  // to a full catalog refetch (fetchCatalog) — simpler than rebuilding a
  // single row and stays consistent with the existing catalog render path.
  //
  // When an edit targets the live-active profile, chain a
  // /profile/switch to the same name so the supervisor re-resolves with
  // the new on-disk state. Without this, fields like model_aliases edited
  // through the popover update the file + the catalog but not the
  // ribbon's Models / Route / Auth cells (which read live session state).
  // ClassifyTransition guarantees same-name switch never crosses the
  // PlaceholderActive ↔ FirstPartyPassthrough boundary, so this is
  // always RelaunchLive — no banner needed.
  function onEditSuccess(resp, mode) {
    panelBack();
    if (resp.item) {
      if (mode === 'add') {
        insertProfileRow(resp.item);
      } else {
        splicePopoverRow(resp.item);
        maybeChainSwitchOnActiveEdit(resp.item.name);
      }
      redecorateAllDefaultIcons(resp.default);
      flashSuccessOutline(resp.item.name);
    }
    popoverCtx.defaultName = resp.default;
  }

  // maybeChainSwitchOnActiveEdit: if the just-edited profile is the
  // session's currently-active profile, fire-and-forget a switch so
  // session state picks up the new config. Failures (rare —
  // alias/base_url/etc. shouldn't fail) surface via toast.
  function maybeChainSwitchOnActiveEdit(name) {
    var active = (state.session && state.session.active_profile_name) || '';
    if (!name || name !== active) return;
    fetch('/profile/switch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CCWRAP-Profile-Token': PROFILE_TOKEN },
      body: JSON.stringify({ name: name }),
    })
    .then(function(r){ return r.json(); })
    .then(function(outcome){
      var result = String((outcome && outcome.result) || '');
      if (result === 'switched') {
        if (outcome.view && state.session) {
          state.session.active_profile_name = outcome.view.name || state.session.active_profile_name;
          state.session.active_profile_provider = outcome.view.provider_label || state.session.active_profile_provider;
          // The full session payload arrives via SSE session_updated;
          // for immediate ribbon refresh of derived cells (Models, Route,
          // Auth) we rely on that. updateProfileCell handles its own.
        }
        if (typeof updateProfileCell === 'function') updateProfileCell();
      } else if (result === 'refused_needs_relaunch') {
        // Theoretically unreachable for same-name switch (no auth-bootstrap
        // delta), but surface defensively rather than silently desync.
        showToast('edit saved on disk — restart ccwrap to apply to live session', 'warning');
      }
    })
    .catch(function(err){
      showToast('edit saved on disk — switch to apply: ' + (err && err.message ? err.message : 'unknown'), 'warning');
    });
  }

  function splicePopoverRow(item) {
    // For v1, simplest approach: trigger a full popover re-render via
    // fetchCatalog. The catalog response gives us a fresh row state.
    if (typeof fetchCatalog === 'function') fetchCatalog();
  }

  function insertProfileRow(item) {
    // Same: trigger a refetch to render the new profile.
    if (typeof fetchCatalog === 'function') fetchCatalog();
  }

  function removePopoverRow(name) {
    var row = popoverEl && popoverEl.querySelector('.sp3-pop-row[data-profile-name="' + cssEscape(name) + '"]');
    if (!row) return;
    row.classList.add('fading-out');
    setTimeout(function(){
      if (row.parentNode) row.remove();
      if (typeof fetchCatalog === 'function') fetchCatalog();
    }, 320);
  }

  // showToast renders a transient notification at the top-right of the
  // viewport (4s; fade-out begins at 3.5s). Replaces alert() for
  // post-mutation status. Single toast at a time — new toast displaces
  // any existing one.
  function showToast(text, severity) {
    var existing = document.querySelector('.ccwrap-toast');
    if (existing) existing.remove();
    var toast = document.createElement('div');
    toast.className = 'ccwrap-toast';
    // role=status (implicit aria-live=polite) — the toast is the ONLY
    // feedback channel for delete/copy outcomes, so it must be announced
    // to screen readers without stealing focus.
    toast.setAttribute('role', 'status');
    if (severity) toast.setAttribute('data-severity', severity);
    toast.textContent = text;
    document.body.appendChild(toast);
    setTimeout(function(){ if (toast.parentNode) toast.classList.add('fade'); }, 3500);
    setTimeout(function(){ if (toast.parentNode) toast.remove(); }, 4000);
  }

  // submitRmDirect POSTs /profile/rm and routes the outcome to a toast
  // (success / active-session warning / error). Called from the row's
  // arm-then-confirm rm handler — no modal involved.
  function submitRmDirect(item, isDefault) {
    fetch('/profile/rm', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CCWRAP-Profile-Token': PROFILE_TOKEN,
      },
      body: JSON.stringify({ name: item.name, confirm_name: item.name }),
    })
    .then(function(resp){ return resp.json(); })
    .then(function(json){
      if (json.ok) {
        removePopoverRow(item.name);
        popoverCtx.defaultName = json.default;
        if (typeof fetchCatalog === 'function') fetchCatalog();
        var msg = 'removed ' + item.name;
        if (json.active_sessions && json.active_sessions.length) {
          msg += ' — still active in ' + json.active_sessions.length +
                 ' session(s) until switch or relaunch';
          showToast(msg, 'warning');
        } else {
          showToast(msg, null);
        }
      } else {
        showToast('failed to delete: ' + (json.message || json.kind), 'error');
      }
    })
    .catch(function(err){
      showToast('request failed: ' + (err && err.message ? err.message : 'unknown'), 'error');
    });
  }

  function flashSuccessOutline(name) {
    // Defer until catalog refetch settles (row may not exist yet after splice).
    setTimeout(function(){
      var row = popoverEl && popoverEl.querySelector('.sp3-pop-row[data-profile-name="' + cssEscape(name) + '"]');
      if (!row) return;
      row.classList.add('flash-success');
      setTimeout(function(){ row.classList.remove('flash-success'); }, 320);
    }, 50);
  }

  function redecorateAllDefaultIcons(defaultName) {
    // For v1, also rely on catalog refetch from the splice/insert helpers.
    // The fresh render sets check-circle vs play based on resp.default.
    popoverCtx.defaultName = defaultName;
  }

  function cssEscape(s) {
    return String(s).replace(/[^a-zA-Z0-9_-]/g, function(ch){ return '\\' + ch; });
  }

  // makeRibbonCellActivable makes an interactive ribbon cell keyboard- and
  // AT-operable: it exposes role=button + tabindex so the cell takes focus and
  // is announced as a button, advertises aria-haspopup=dialog (it opens a
  // popover), and seeds aria-expanded=false. The keydown handler maps
  // Enter/Space to cell.click() so activation routes through the SAME
  // (state-gated) click handler that the mouse uses — an inert-state cell's
  // click handler early-returns exactly as today, so no logic is duplicated.
  function makeRibbonCellActivable(cell){
    if (!cell) return;
    cell.setAttribute('role', 'button');
    cell.setAttribute('tabindex', '0');
    cell.setAttribute('aria-haspopup', 'dialog');
    cell.setAttribute('aria-expanded', 'false');
    cell.addEventListener('keydown', function(ev){
      // Activate only when the cell button itself is focused, not a focusable
      // descendant: the Profile popover (with its create/edit form) mounts
      // inside the cell, so without this guard a Space/Enter in a form input
      // bubbles here, gets eaten (preventDefault) and toggles the popover shut.
      // currentTarget is the cell (the handler's element); target is the focused
      // node — they differ exactly when a descendant control is focused.
      if (ev.target !== ev.currentTarget) return;
      if (ev.key === 'Enter' || ev.key === ' ' || ev.key === 'Spacebar'){
        ev.preventDefault();
        cell.click();
      }
    });
  }

  // Click on the Profile ribbon cell opens the popover (when eligible).
  function attachProfileCellHandlers() {
    profileCell = document.querySelector('.ribbon-cell[data-ribbon="Profile"]');
    if (!profileCell) return;
    var state2 = profileCell.dataset.state || '';
    if (state2 === 'inherit-env-static') return; // not clickable
    profileCell.addEventListener('click', function(ev){
      ev.stopPropagation();
      if (popState === 'closed') openPopover();
      else closePopover();
    });
    makeRibbonCellActivable(profileCell);
    document.addEventListener('click', function(){ if (popState !== 'closed') closePopover(); });
    document.addEventListener('keydown', function(ev){ if (ev.key === 'Escape' && popState !== 'closed') closePopover(); });
  }
  attachProfileCellHandlers();

  // Click on the Models ribbon cell (when data-state="aliases-active")
  // opens a small popover listing the alias forward map. Closes on
  // outside-click / Esc. Reads from state.session.model_alias_forward
  // (no fetch). Makes the aliases
  // discoverable without grepping config files.
  var modelsCell = null;
  var modelsPop = null;
  function attachModelsCellHandlers(){
    modelsCell = document.querySelector('.ribbon-cell[data-ribbon="Models"]');
    if (!modelsCell) return;
    modelsCell.addEventListener('click', function(ev){
      if (modelsCell.dataset.state !== 'aliases-active') return;
      ev.stopPropagation();
      if (modelsPop && modelsPop.parentNode) {
        closeModelsPop();
      } else {
        openModelsPop();
      }
    });
    makeRibbonCellActivable(modelsCell);
    document.addEventListener('click', function(){ if (modelsPop) closeModelsPop(); });
    document.addEventListener('keydown', function(ev){ if (ev.key === 'Escape' && modelsPop) closeModelsPop(); });
  }
  function openModelsPop(){
    if (!modelsCell) return;
    closeAllRibbonPops();
    var forward = (state.session && state.session.model_alias_forward) || {};
    var keys = Object.keys(forward).sort();
    if (keys.length === 0) return;
    modelsPop = document.createElement('div');
    modelsPop.className = 'sp3-models-pop';
    modelsPop.addEventListener('click', function(ev){ ev.stopPropagation(); });
    var head = document.createElement('div');
    head.className = 'head';
    head.textContent = keys.length + ' alias' + (keys.length === 1 ? '' : 'es') + ' — claude name → upstream model';
    modelsPop.appendChild(head);
    var tbl = document.createElement('table');
    for (var i = 0; i < keys.length; i++) {
      var tr = document.createElement('tr');
      var tdFrom = document.createElement('td'); tdFrom.className = 'from'; tdFrom.textContent = keys[i];
      var tdArrow = document.createElement('td'); tdArrow.className = 'arrow'; tdArrow.textContent = '→';
      var tdTo = document.createElement('td'); tdTo.className = 'to'; tdTo.textContent = forward[keys[i]];
      tr.appendChild(tdFrom); tr.appendChild(tdArrow); tr.appendChild(tdTo);
      tbl.appendChild(tr);
    }
    modelsPop.appendChild(tbl);
    modelsCell.appendChild(modelsPop);
    modelsCell.setAttribute('aria-expanded', 'true');
  }
  function closeModelsPop(){
    if (modelsPop && modelsPop.parentNode) modelsPop.parentNode.removeChild(modelsPop);
    modelsPop = null;
  }
  attachModelsCellHandlers();

  // Clicking the Bodies ribbon cell opens a small popover (mirror of the Models
  // cell popover: stopPropagation, toggle open/close, dismiss on document-click
  // + Escape). The popover hosts TWO capture toggles: "request bodies"
  // (POST /capture/bodies) and "telemetry bodies" (POST /capture/telemetry).
  // Each toggle is optimistic: flip state.session, re-render the cell via
  // updateBodiesCell() + the popover rows, and revert on error. The CSRF token
  // is the same PROFILE_TOKEN closure var used by /profile/switch.
  var bodiesCell = null;
  var bodiesPop = null;
  function attachBodiesCellHandlers(){
    bodiesCell = ribbonCellEl('Bodies');
    if (!bodiesCell) return;
    bodiesCell.addEventListener('click', function(ev){
      ev.stopPropagation();
      if (bodiesPop && bodiesPop.parentNode) {
        closeBodiesPop();
      } else {
        openBodiesPop();
      }
    });
    makeRibbonCellActivable(bodiesCell);
    document.addEventListener('click', function(){ if (bodiesPop) closeBodiesPop(); });
    document.addEventListener('keydown', function(ev){ if (ev.key === 'Escape' && bodiesPop) closeBodiesPop(); });
  }
  // bodiesToggleRow builds one toggle row reflecting on/off via the 'on' class +
  // a checkbox-like glyph (textContent only). Clicking POSTs {enable:!current}
  // to url, optimistically mutates state.session[field], re-renders, reverts on
  // error. desc is an optional one-line neutral description under the label.
  function bodiesToggleRow(label, desc, field, url){
    var sess = state.session || {};
    var current = !!sess[field];
    var row = document.createElement('div');
    row.className = 'bodies-pop-row' + (current ? ' on' : '');
    var glyph = document.createElement('span');
    glyph.className = 'bodies-pop-glyph';
    glyph.textContent = current ? '☑' : '☐';
    row.appendChild(glyph);
    var text = document.createElement('div');
    text.className = 'bodies-pop-text';
    var lab = document.createElement('div');
    lab.className = 'bodies-pop-label';
    lab.textContent = label;
    text.appendChild(lab);
    if (desc) {
      var dd = document.createElement('div');
      dd.className = 'bodies-pop-desc';
      dd.textContent = desc;
      text.appendChild(dd);
    }
    row.appendChild(text);
    row.addEventListener('click', function(ev){
      ev.stopPropagation();
      if (row.classList.contains('pending')) return;
      var prev = !!(state.session && state.session[field]);
      var next = !prev;
      row.classList.add('pending');
      fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CCWRAP-Profile-Token': PROFILE_TOKEN },
        body: JSON.stringify({ enable: next })
      }).then(function(r){
        if (!r.ok) throw new Error('http ' + r.status);
        return r.json();
      }).then(function(out){
        var settled = !!(out && out.enabled);
        if (state.session) state.session[field] = settled;
        updateBodiesCell();
        if (bodiesPop) buildBodiesPop();
      }).catch(function(_err){
        // Revert: never leave the row lying about state.
        if (state.session) state.session[field] = prev;
        updateBodiesCell();
        if (bodiesPop) buildBodiesPop();
      });
    });
    return row;
  }
  function buildBodiesPop(){
    if (!bodiesPop) return;
    bodiesPop.replaceChildren();
    var head = document.createElement('div');
    head.className = 'head';
    head.textContent = 'capture';
    bodiesPop.appendChild(head);
    bodiesPop.appendChild(bodiesToggleRow(
      'request + response bodies',
      'capture Anthropic API request + streamed response bodies (credentials redacted)',
      'capture_bodies', '/capture/bodies'));
    bodiesPop.appendChild(bodiesToggleRow(
      'telemetry bodies',
      'decrypt + capture what Claude Code sends to Datadog/Sentry (otherwise blind-tunneled)',
      'capture_telemetry', '/capture/telemetry'));
  }
  function openBodiesPop(){
    if (!bodiesCell) return;
    closeAllRibbonPops();
    bodiesPop = document.createElement('div');
    bodiesPop.className = 'bodies-pop';
    bodiesPop.addEventListener('click', function(ev){ ev.stopPropagation(); });
    buildBodiesPop();
    bodiesCell.appendChild(bodiesPop);
    bodiesCell.setAttribute('aria-expanded', 'true');
  }
  function closeBodiesPop(){
    if (bodiesPop && bodiesPop.parentNode) bodiesPop.parentNode.removeChild(bodiesPop);
    bodiesPop = null;
  }
  attachBodiesCellHandlers();

  // closeAllRibbonPops dismisses every ribbon popover so opening any one ribbon
  // popover closes the others (single open popover at a time). It routes through
  // each popover's REAL closer so owner state (popState/popoverEl, bodiesPop,
  // modelsPop) stays consistent instead of leaving orphaned nodes behind. Each
  // closer is guarded and a safe no-op when its popover is already closed. The
  // native-tls popover has no state variable, so its node is removed directly.
  function closeAllRibbonPops(){
    try { closePopover(); } catch(e){}
    try { closeBodiesPop(); } catch(e){}
    try { closeModelsPop(); } catch(e){}
    var ns = document.querySelectorAll('.native-tls-pop');
    for (var i = 0; i < ns.length; i++) { ns[i].remove(); }
    // All popovers are now gone — reset aria-expanded on every activable cell so
    // AT announces them as collapsed (each opener re-asserts true on open).
    var cells = document.querySelectorAll('.ribbon-cell[aria-expanded]');
    for (var j = 0; j < cells.length; j++) { cells[j].setAttribute('aria-expanded', 'false'); }
  }
  // openNativeTLSPopover lazily fetches /native-tls and renders the captured
  // ClientHello fingerprints (JA3/JA4/peetprint) as select-to-copy .cmd spans
  // plus a real-route download link for the raw clienthello.bin. Origin-fragile
  // navigator.clipboard is deliberately avoided (the dashboard can be reached
  // over a plain-HTTP tunnel/LAN). All DOM is built via createElement/textContent
  // (NO innerHTML); a not-captured or fetch-failure path sets a single plain
  // textContent message with no dead download link and no infinite spinner.
  function openNativeTLSPopover(cell){
    closeAllRibbonPops();
    var pop = document.createElement('div');
    pop.className = 'native-tls-pop bodies-pop';
    pop.addEventListener('click', function(ev){ ev.stopPropagation(); });
    var loading = document.createElement('div');
    loading.className = 'ntls-note';
    loading.textContent = 'loading…';
    pop.appendChild(loading);
    cell.appendChild(pop);
    cell.setAttribute('aria-expanded', 'true');
    fetch('/native-tls').then(function(r){ return r.json(); }).then(function(d){
      pop.textContent = '';
      if (!d || !d.captured) {
        var e = document.createElement('div');
        e.className = 'ntls-note';
        e.textContent = 'no ClientHello captured yet';
        pop.appendChild(e);
        return;
      }
      // cmdRow captures label+value per row (a bare var-loop handler would
      // close over the final index). Click copies the fingerprint via the
      // shared copyText and confirms with a toast; user-select:all still
      // paints the selection as visual feedback.
      function cmdRow(label, value){
        var row = document.createElement('div'); row.className = 'ntls-row';
        var k = document.createElement('span'); k.className = 'ntls-k'; k.textContent = label;
        var v = document.createElement('span'); v.className = 'cmd';
        v.title = 'click to copy';
        v.textContent = value;
        v.addEventListener('click', function(ev){
          ev.stopPropagation();
          if (copyText(value)) showToast(label + ' copied', null);
        });
        row.appendChild(k); row.appendChild(v);
        return row;
      }
      var rows = [['JA3', d.ja3], ['JA4', d.ja4], ['peetprint', d.peetprint]];
      for (var i = 0; i < rows.length; i++) {
        if (!rows[i][1]) continue;
        pop.appendChild(cmdRow(rows[i][0], rows[i][1]));
      }
      var dl = document.createElement('a'); dl.className = 'bv-dl';
      dl.textContent = 'download clienthello.bin';
      dl.setAttribute('href', '/native-tls/clienthello.bin');
      dl.setAttribute('download', 'clienthello.bin');
      pop.appendChild(dl);
      if (d.note) {
        var note = document.createElement('div'); note.className = 'ntls-note';
        note.textContent = d.note;
        pop.appendChild(note);
      }
    }).catch(function(){ pop.textContent = ''; var f = document.createElement('div'); f.className = 'ntls-note'; f.textContent = 'fetch failed'; pop.appendChild(f); });
  }
  // Click the NATIVE TLS ribbon cell (only when it carries captured data —
  // data-state native-active or native-blocked, matching the CSS caret/cursor
  // gate) to toggle the fingerprint popover. The cell is absent for non-opted-in
  // sessions and inert (no caret/handler) in the native-off state.
  function attachNativeTLSCellHandlers(){
    var ntlsCell = ribbonCellEl('NATIVE TLS');
    if (!ntlsCell) return;
    ntlsCell.addEventListener('click', function(ev){
      var st = ntlsCell.dataset.state;
      if (st !== 'native-active' && st !== 'native-blocked') return;
      ev.stopPropagation();
      if (ntlsCell.querySelector('.native-tls-pop')) { closeAllRibbonPops(); return; }
      openNativeTLSPopover(ntlsCell);
    });
    makeRibbonCellActivable(ntlsCell);
    document.addEventListener('click', function(){ closeAllRibbonPops(); });
    document.addEventListener('keydown', function(ev){ if (ev.key === 'Escape') closeAllRibbonPops(); });
  }
  attachNativeTLSCellHandlers();

  // --- Stubs ---
  // patchSession + updateProfileCell + updateRouteCell +
  // updateAuthCell + updateModelsCell are defined above (see ribbonCellEl helpers).
  // Outcome-block reconciliation against SSE is not yet implemented.
  function applyOutcomeBlock(_outcome){ /* not yet implemented */ }

  // Per-row profile annotation + switch-marker
  // structured rendering. textContent only; never .innerHTML. Idempotent
  // so prependRow can call unconditionally on every fresh row.
  function decorateProfileAnnotation(rowEl) {
    if (!rowEl) return;
    var name = rowEl.dataset.profileName;
    if (!name) return;
    // Idempotent — don't re-add on patch.
    if (rowEl.querySelector('.ann')) return;
    var provider = rowEl.dataset.profileProvider || '';
    var rightCell = rowEl.querySelector('.cell-right');
    if (!rightCell) return;
    var sep = document.createTextNode(' · ');
    var span = document.createElement('span');
    span.className = 'ann';
    span.style.color = 'hsl(' + providerHue(provider) + ',60%,65%)';
    span.textContent = name;
    rightCell.appendChild(sep);
    rightCell.appendChild(span);
  }

  // First-paint sweep: decorate all server-rendered rows that carry the data-attr.
  function decorateAllProfileAnnotations() {
    var rows = document.querySelectorAll('.row[data-profile-name]');
    for (var i = 0; i < rows.length; i++) decorateProfileAnnotation(rows[i]);
  }
  decorateAllProfileAnnotations();

  // Switch-marker structured rendering (live SSE path + first-paint sweep).
  function renderSwitchMarker(rowEl) {
    if (!rowEl || rowEl.dataset.category !== 'profile_switch') return;
    // Idempotent.
    if (rowEl.querySelector('.sp3-switch-rendered')) return;
    var to = rowEl.dataset.switchTo || '';
    var from = rowEl.dataset.switchFrom || '';
    var fromProvider = rowEl.dataset.switchFromProvider || '';
    var toProvider = rowEl.dataset.switchToProvider || '';
    var cls = rowEl.dataset.switchClass || '';
    var requested = rowEl.dataset.switchRequested || '';
    var reason = rowEl.dataset.switchReason || '';
    // Clear existing cell content and rebuild a single-row textual marker.
    rowEl.textContent = '';
    var marker = document.createElement('div');
    marker.className = 'sp3-switch-rendered';
    function badge(text, hueProvider){
      var s = document.createElement('span');
      s.className = 'ann';
      s.style.color = 'hsl(' + providerHue(hueProvider || '') + ',60%,65%)';
      s.textContent = text;
      return s;
    }
    if (to) {
      // Switched or refused.
      marker.appendChild(document.createTextNode(cls === 'needs_relaunch' ? 'refused ' : 'switched '));
      marker.appendChild(badge('[' + from + ']', fromProvider));
      marker.appendChild(document.createTextNode(' → '));
      marker.appendChild(badge('[' + to + ']', toProvider));
      if (cls === 'needs_relaunch') marker.appendChild(document.createTextNode(' · needs relaunch'));
      else marker.appendChild(document.createTextNode(' · live'));
    } else {
      // Rejected.
      marker.appendChild(document.createTextNode('rejected '));
      marker.appendChild(badge('[' + from + ']', fromProvider));
      marker.appendChild(document.createTextNode(' ✗ ' + requested + ' (' + reason + ')'));
    }
    rowEl.appendChild(marker);
  }

  function renderAllSwitchMarkers() {
    var rows = document.querySelectorAll('.row[data-category="profile_switch"]');
    for (var i = 0; i < rows.length; i++) renderSwitchMarker(rows[i]);
  }
  renderAllSwitchMarkers();

  // T13: install the egress ribbon at the top of the dashboard root.
  // No auto-probe — the ribbon shows '(unprobed)' until the user clicks.
  enhancePostureEgressCell();
})();
</script>
{{end}}
{{if not .LiveEnabled}}
<script id="ccwrap-ovf">
/* Duplicate of the overflow-menu toggle in the live script above; ended pages
   carry no live script element. Keep both copies in sync. */
(function(){
  var b=document.getElementById('ovf-btn'),m=document.getElementById('ovf-menu');
  if(!b||!m)return;
  function close(){m.hidden=true;b.setAttribute('aria-expanded','false');}
  b.addEventListener('click',function(e){e.stopPropagation();var open=m.hidden;m.hidden=!open;b.setAttribute('aria-expanded',String(open));});
  document.addEventListener('click',function(e){if(!m.hidden&&!m.contains(e.target)&&e.target!==b)close();});
  document.addEventListener('keydown',function(e){if(e.key==='Escape')close();});
})();
/* Claude-session chip copy — duplicate of the live script's copyClaude
   handler; without it the server-rendered role=button chip is a dead
   control on ended pages. Same execCommand select-to-copy fallback (the
   async Clipboard API is banned: plain-HTTP tunnels lack it) and the same
   toast confirmation (role=status so AT announces it). Adding a shared
   script element instead would break the single-script extraction tests,
   so this stays an inline copy. Keep both copies in sync. */
(function(){
  var el = document.getElementById('claude-sess-label'); if (!el) return;
  function miniToast(text){
    var existing = document.querySelector('.ccwrap-toast');
    if (existing) existing.remove();
    var toast = document.createElement('div');
    toast.className = 'ccwrap-toast';
    toast.setAttribute('role', 'status');
    toast.textContent = text;
    document.body.appendChild(toast);
    setTimeout(function(){ if (toast.parentNode) toast.classList.add('fade'); }, 3500);
    setTimeout(function(){ if (toast.parentNode) toast.remove(); }, 4000);
  }
  function copyClaude(){
    var full = el.getAttribute('data-full') || ''; if (!full) return;
    var ta = document.createElement('textarea');
    ta.value = full;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '0';
    ta.style.left = '0';
    ta.style.opacity = '0';
    ta.style.pointerEvents = 'none';
    document.body.appendChild(ta);
    var sel = document.getSelection();
    var prev = sel && sel.rangeCount > 0 ? sel.getRangeAt(0) : null;
    ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (_) {}
    document.body.removeChild(ta);
    if (prev && sel){ sel.removeAllRanges(); sel.addRange(prev); }
    if (ok) miniToast('session id copied');
  }
  el.addEventListener('click', copyClaude);
  el.addEventListener('keydown', function(e){
    if (e.target !== e.currentTarget) return; // only the chip itself, never a descendant
    if (e.key === 'Enter' || e.key === ' '){ e.preventDefault(); copyClaude(); }
  });
})();
</script>
{{end}}
</body>
</html>`))

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
