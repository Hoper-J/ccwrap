package supervisor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

func TestWebUsesSpecTokens(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x"})
	html := buf.String()
	for _, tok := range []string{
		"--bg:#0a0a0a", "--surface:#101010", "--accent:#10b981",
		"--warn:#fbbf24", "--danger:#f43f5e", "--trace:#a78bfa",
	} {
		if !strings.Contains(html, tok) {
			t.Fatalf("missing theme token %q", tok)
		}
	}
	if strings.Contains(html, "#0d1117") || strings.Contains(html, "#4f8cff") {
		t.Fatalf("old GitHub-Primer tokens still present")
	}
	// No undefined CSS vars left behind by the reconciliation.
	for _, dead := range []string{"var(--panel)", "var(--panel2)", "var(--success)", "var(--line-2)"} {
		if strings.Contains(html, dead) {
			t.Fatalf("undefined legacy var still referenced: %s", dead)
		}
	}
}

func mustContainAllWeb(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("html missing %q", sub)
		}
	}
}

func TestWebHeroAndRibbon(t *testing.T) {
	page := webPageData{
		Title:        "CCWRAP Session",
		HeroState:    "Active",
		HeroVariant:  "active",
		HeroSentence: "Routing Claude Code through 50.18.84.244:3000. CCWRAP holds your gateway credentials and applies 1 model alias; Claude Code only sees logical names.",
		HeroMeta:     "claude pid 7374 · 2m ago",
		Ribbon: []webKV{
			{Label: "Route", Value: "Gateway"},
			{Label: "Auth", Value: "CCWRAP-owned · fail-closed"},
			{Label: "Models", Value: ""}, // ribbon invariant: empty → "—"
			{Label: "Traffic", Value: "9 · 0", Mono: true},
			{Label: "Profile", ValueHTML: template.HTML(`<span class="sp3-chip sp3-chip-inherit sp3-chip-inherit-static"><span class="sp3-chip-dot"></span>inherit-env <span class="sp3-chip-caret">▾</span></span>`), Detail: "no profiles.json", DataState: "inherit-env-static"},
		},
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()
	mustContainAllWeb(t, html,
		"Active", "CCWRAP holds your gateway credentials", "claude pid 7374",
		`data-variant="active"`, "Route", "Gateway", "Traffic",
	)
	if n := strings.Count(html, "data-ribbon-cell"); n != 5 {
		t.Fatalf("ribbon invariant: expected 5 cells, got %d", n)
	}
	if !strings.Contains(html, "—") {
		t.Fatalf("empty Models cell must render em-dash placeholder")
	}
	// Old 9-item summary grid must be gone from the main flow.
	if strings.Contains(html, `class="summary-grid"`) {
		t.Fatalf("old summary-grid must be replaced by the ribbon")
	}
}

func TestWebHeroVariant(t *testing.T) {
	// Two orthogonal dimensions: hero big-word is Health-driven (Ended wins).
	cases := []struct {
		name        string
		state       model.SessionState
		health      model.SessionHealth
		wantState   string
		wantVariant string
	}{
		{"ended wins over health", model.StateEnded, model.HealthError, "Ended", "ended"},
		{"error health", model.StateActive, model.HealthError, "Error", "error"},
		{"warn health", model.StateActive, model.HealthWarn, "Degraded", "degraded"},
		{"ok health", model.StateActive, model.HealthOK, "Active", "active"},
		{"empty health treated as ok", model.StateActive, "", "Active", "active"},
		{"attached ok", model.StateAttached, model.HealthOK, "Active", "active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotState, gotVariant := webHeroVariant(model.Session{State: c.state, Health: c.health})
			if gotState != c.wantState || gotVariant != c.wantVariant {
				t.Fatalf("state=%s health=%s: got (%q,%q), want (%q,%q)",
					c.state, c.health, gotState, gotVariant, c.wantState, c.wantVariant)
			}
		})
	}
}

func TestWebRibbonHasDetailSublines(t *testing.T) {
	// Every ribbon cell is a value plus a mono detail line.
	sess := model.Session{
		RouteClass: model.RouteClassThirdPartyHidden, RouteSource: model.RouteSourceInheritedEnv,
		AuthPolicy:    model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap: model.AuthBootstrapPlaceholderActive, AuthBootstrapKind: model.AuthBootstrapKindBearer,
		ModelAliasCount: 1, RecentRequestCount: 9, RecentErrorCount: 0,
	}
	rib := webRibbonFromSession(sess, "just now", false, 0)
	if len(rib) != 7 {
		t.Fatalf("ribbon must be 7 cells (Route/Auth/Models/Traffic/Profile + Bodies + Egress), got %d", len(rib))
	}
	byLabel := map[string]webKV{}
	for _, c := range rib {
		byLabel[c.Label] = c
	}
	if byLabel["Route"].Value != "Gateway" || byLabel["Route"].Detail != "from environment" {
		t.Fatalf("Route cell = %+v, want value=Gateway detail=from environment", byLabel["Route"])
	}
	if byLabel["Auth"].Value != "CCWRAP-owned · fail-closed" || byLabel["Auth"].Detail != "placeholder injected" {
		t.Fatalf("Auth cell = %+v", byLabel["Auth"])
	}
	if byLabel["Traffic"].Value != "9 · 0" || byLabel["Traffic"].Detail != "just now" {
		t.Fatalf("Traffic cell = %+v", byLabel["Traffic"])
	}
	// Models detail stays empty — data gap (no forward map).
	if byLabel["Models"].Detail != "" {
		t.Fatalf("Models detail must be empty, got %q", byLabel["Models"].Detail)
	}
	// And the detail line actually renders in the HTML.
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", Ribbon: rib})
	mustContainAllWeb(t, buf.String(), `class="d"`, "from environment", "placeholder injected", "just now")
}

func TestWebRibbon_ProfileCell_Active(t *testing.T) {
	sess := model.Session{ActiveProfileName: "alpha", ActiveProfileProvider: "Anthropic", ModelAliasCount: 2}
	ribbon := webRibbonFromSession(sess, "just now", true, 3)
	if len(ribbon) != 7 {
		t.Fatalf("ribbon len = %d, want 7 (Route/Auth/Models/Traffic/Profile + Bodies + Egress)", len(ribbon))
	}
	last := ribbon[4]
	if last.Label != "Profile" {
		t.Fatalf("5th cell Label = %q, want Profile", last.Label)
	}
	if last.DataState != "active" {
		t.Fatalf("DataState = %q, want active", last.DataState)
	}
	if !strings.Contains(string(last.ValueHTML), "alpha") {
		t.Fatalf("chip must contain profile name: %q", last.ValueHTML)
	}
	if !strings.Contains(string(last.ValueHTML), "sp3-chip") {
		t.Fatalf("chip must carry sp3-chip class: %q", last.ValueHTML)
	}
	if last.Detail != "Anthropic · 2 aliases" {
		t.Fatalf("Detail = %q, want \"Anthropic · 2 aliases\"", last.Detail)
	}
}

func TestWebRibbon_ProfileCell_InheritStatic(t *testing.T) {
	sess := model.Session{}                            // no active profile
	ribbon := webRibbonFromSession(sess, "", false, 0) // no profiles.json
	last := ribbon[4]
	if last.DataState != "inherit-env-static" {
		t.Fatalf("DataState = %q, want inherit-env-static", last.DataState)
	}
	if !strings.Contains(string(last.ValueHTML), "inherit-env") {
		t.Fatalf("chip must show inherit-env: %q", last.ValueHTML)
	}
	if last.Detail != "no profiles.json" {
		t.Fatalf("Detail = %q, want \"no profiles.json\"", last.Detail)
	}
}

func TestWebRibbon_ProfileCell_InheritClickable(t *testing.T) {
	sess := model.Session{}                           // no active
	ribbon := webRibbonFromSession(sess, "", true, 5) // profiles.json with 5 profiles
	last := ribbon[4]
	if last.DataState != "inherit-env-clickable" {
		t.Fatalf("DataState = %q, want inherit-env-clickable", last.DataState)
	}
	if !strings.Contains(last.Detail, "5") {
		t.Fatalf("Detail must reflect profile count: %q", last.Detail)
	}
}

// TestWebRibbon_AuthCell_Healthy locks the regression: when AuthBootstrap
// is NOT Missing the Auth cell renders the historical (value=HumanAuthPolicy,
// detail=HumanAuthBootstrap, DataState empty) tuple — byte-identical to the
// prior behavior. The new auth-missing branch must not bleed into normal
// sessions.
func TestWebRibbon_AuthCell_Healthy(t *testing.T) {
	sess := model.Session{
		AuthPolicy:        model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap:     model.AuthBootstrapPlaceholderActive,
		AuthBootstrapKind: model.AuthBootstrapKindBearer,
	}
	ribbon := webRibbonFromSession(sess, "", true, 0)
	auth := ribbon[1] // Route, Auth, Models, Traffic, Profile, Bodies
	if auth.Label != "Auth" {
		t.Fatalf("ribbon[1] not Auth: %+v", auth)
	}
	if auth.DataState != "" {
		t.Errorf("healthy Auth must have empty DataState; got %q", auth.DataState)
	}
	if strings.Contains(auth.Value, "MISSING") {
		t.Errorf("healthy Auth must not say MISSING; got %q", auth.Value)
	}
}

// TestWebRibbon_AuthCell_MissingCaseA — profile named a key_env that env
// doesn't have. Cell flips to danger state with the env name in detail.
func TestWebRibbon_AuthCell_MissingCaseA(t *testing.T) {
	sess := model.Session{
		ActiveProfileName: "local",
		AuthBootstrap:     model.AuthBootstrapMissing,
		MissingAuthEnv:    "ANTHROPIC_AUTH_TOKEN",
	}
	ribbon := webRibbonFromSession(sess, "", true, 0)
	auth := ribbon[1]
	if auth.DataState != "auth-missing" {
		t.Errorf("DataState = %q, want auth-missing", auth.DataState)
	}
	if !strings.Contains(auth.Value, "MISSING") {
		t.Errorf("Value must announce MISSING; got %q", auth.Value)
	}
	for _, want := range []string{`"local"`, "ANTHROPIC_AUTH_TOKEN"} {
		if !strings.Contains(auth.Detail, want) {
			t.Errorf("Detail missing %q; got %q", want, auth.Detail)
		}
	}
}

// TestWebRibbon_AuthCell_MissingCaseB — TPH route with no auth source named.
// Cell flips to danger state but detail uses the generic "no auth source"
// phrasing (no $env in it).
func TestWebRibbon_AuthCell_MissingCaseB(t *testing.T) {
	sess := model.Session{
		ActiveProfileName: "foo",
		AuthBootstrap:     model.AuthBootstrapMissing,
		MissingAuthEnv:    "", // Case B
	}
	ribbon := webRibbonFromSession(sess, "", true, 0)
	auth := ribbon[1]
	if auth.DataState != "auth-missing" {
		t.Errorf("DataState = %q, want auth-missing", auth.DataState)
	}
	if strings.Contains(auth.Detail, "$") {
		t.Errorf("Case B detail must not name a specific env; got %q", auth.Detail)
	}
	if !strings.Contains(auth.Detail, "no auth source configured") {
		t.Errorf("Case B detail should say 'no auth source configured'; got %q", auth.Detail)
	}
}

// TestWebRibbon_AuthCell_MissingRenderedHTML — the data-state token reaches
// the rendered HTML so the CSS danger-color rule applies.
func TestWebRibbon_AuthCell_MissingRenderedHTML(t *testing.T) {
	sess := model.Session{
		ActiveProfileName: "local",
		AuthBootstrap:     model.AuthBootstrapMissing,
		MissingAuthEnv:    "ANTHROPIC_AUTH_TOKEN",
	}
	rib := webRibbonFromSession(sess, "", true, 0)
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", Ribbon: rib})
	html := buf.String()
	if !strings.Contains(html, `data-state="auth-missing"`) {
		t.Errorf("HTML missing data-state=auth-missing")
	}
	if !strings.Contains(html, "MISSING") {
		t.Errorf("HTML missing MISSING marker")
	}
}

// TestUnifiedActivityRows_HeaderRedaction_HonoursUnmask locks the contract:
// unifiedActivityRows threads its `unmask` arg to headerAnnotation, which
// routes credential headers raw vs sentinel. Default (unmask=false)
// preserves the historical behavior.
func TestUnifiedActivityRows_HeaderRedaction_HonoursUnmask(t *testing.T) {
	rec := model.RequestRecord{
		Method: "POST",
		Path:   "/v1/messages",
		RequestHeaders: http.Header{
			"Authorization":     {"Bearer sk-REALCRED"},
			"Anthropic-Version": {"2023-06-01"},
		},
	}
	t.Run("masked (default)", func(t *testing.T) {
		rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 5, false, false)
		flat := ""
		for _, g := range rows[0].HeaderGroups {
			for _, r := range g.Rows {
				flat += r.Name + "|" + r.Value + "|"
			}
		}
		if strings.Contains(flat, "sk-REALCRED") {
			t.Fatalf("masked path must not leak credential; got %q", flat)
		}
		if !strings.Contains(flat, "‹redacted by ccwrap›") {
			t.Fatalf("masked path must emit sentinel; got %q", flat)
		}
	})
	t.Run("unmasked (CCWRAP_UNMASK_CREDENTIALS=1)", func(t *testing.T) {
		rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 5, false, true)
		flat := ""
		for _, g := range rows[0].HeaderGroups {
			for _, r := range g.Rows {
				flat += r.Name + "|" + r.Value + "|"
			}
		}
		if strings.Contains(flat, "‹redacted by ccwrap›") {
			t.Fatalf("unmasked path must NOT emit sentinel; got %q", flat)
		}
		if !strings.Contains(flat, "sk-REALCRED") {
			t.Fatalf("unmasked path must render credential raw; got %q", flat)
		}
	})
}

// TestSP3InlineScript_HeaderRedaction_UnmaskBranch — JS-side classifyHdr
// path reads state.session.capture_bodies_unmasked and skips the sentinel
// substitution when true. Pins the substring evidence that the unmask
// branch lives in the inline script.
func TestSP3InlineScript_HeaderRedaction_UnmaskBranch(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	if !strings.Contains(js, "capture_bodies_unmasked") {
		t.Errorf("inline JS must read state.session.capture_bodies_unmasked for header redaction")
	}
	if !regexp.MustCompile(`var\s+unmask\s*=`).MatchString(js) {
		t.Errorf("inline JS must compute an 'unmask' boolean from session.capture_bodies_unmasked")
	}
	if !regexp.MustCompile(`if\s*\(!unmask\)\s*disp\s*=\s*['"]‹redacted by ccwrap›['"]`).MatchString(js) {
		t.Errorf("inline JS must gate the sentinel substitution on !unmask")
	}
}

// TestSP3InlineScript_AuthCell_HandlesMissing locks the JS-side handler
// for the auth-missing wire signal: updateAuthCell must read
// session.auth_bootstrap === 'missing' and route to data-state=auth-missing
// with the right detail branching on missing_auth_env emptiness.
func TestSP3InlineScript_AuthCell_HandlesMissing(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	if !strings.Contains(js, "missing_auth_env") {
		t.Errorf("inline JS must read session.missing_auth_env")
	}
	if !strings.Contains(js, "auth-missing") {
		t.Errorf("inline JS must set the auth-missing dataset.state branch")
	}
	if !regexp.MustCompile(`auth_bootstrap\s*===\s*['"]missing['"]`).MatchString(js) {
		t.Errorf("inline JS must compare session.auth_bootstrap === 'missing'")
	}
}

// TestWebRibbon_BodiesCell_Off / _On lock the bodiesCellPresentation contract
// — the 6th ribbon cell renders Value "off"/"on", a contextual Detail, and
// the data-state attribute that the click handler reads to know which way
// to flip.
func TestWebRibbon_BodiesCell_Off(t *testing.T) {
	sess := model.Session{CaptureBodies: false}
	ribbon := webRibbonFromSession(sess, "", true, 0)
	if len(ribbon) != 7 {
		t.Fatalf("ribbon len = %d, want 7", len(ribbon))
	}
	bodies := ribbon[5]
	if bodies.Label != "Bodies" {
		t.Fatalf("6th cell Label = %q, want Bodies", bodies.Label)
	}
	if bodies.Value != "off" {
		t.Errorf("Value = %q, want off", bodies.Value)
	}
	if bodies.DataState != "bodies-off" {
		t.Errorf("DataState = %q, want bodies-off", bodies.DataState)
	}
	if !strings.Contains(bodies.Detail, "click") {
		t.Errorf("Detail must hint clickability when off: %q", bodies.Detail)
	}
}

func TestWebRibbon_BodiesCell_On(t *testing.T) {
	sess := model.Session{CaptureBodies: true}
	ribbon := webRibbonFromSession(sess, "", true, 0)
	bodies := ribbon[5]
	// With only request capture on, the summary value is the "request" part.
	if bodies.Value != "request" {
		t.Errorf("Value = %q, want request", bodies.Value)
	}
	if bodies.DataState != "bodies-on" {
		t.Errorf("DataState = %q, want bodies-on", bodies.DataState)
	}
	if !strings.Contains(strings.ToLower(bodies.Detail), "recording") {
		t.Errorf("Detail must indicate recording when on: %q", bodies.Detail)
	}
}

// TestWebRibbon_BodiesCell_Unmasked locks the 3-state machine on the Bodies
// cell. CCWRAP_UNMASK_CREDENTIALS=1 at launch flips a process-wide flag onto
// every session as CaptureBodiesUnmasked=true; when capture is ALSO on the
// ribbon must show a persistent danger-color "on ⚠ UNMASKED" marker so the
// user does not forget the env. Without capture on, the unmask flag is a
// no-op (nothing is captured, so nothing to redact).
func TestWebRibbon_BodiesCell_Unmasked(t *testing.T) {
	t.Run("unmask without capture is off", func(t *testing.T) {
		sess := model.Session{CaptureBodies: false, CaptureBodiesUnmasked: true}
		ribbon := webRibbonFromSession(sess, "", true, 0)
		bodies := ribbon[5]
		if bodies.DataState != "bodies-off" {
			t.Errorf("unmask=true without capture must show off; got %q", bodies.DataState)
		}
	})
	t.Run("capture without unmask is on", func(t *testing.T) {
		sess := model.Session{CaptureBodies: true, CaptureBodiesUnmasked: false}
		ribbon := webRibbonFromSession(sess, "", true, 0)
		bodies := ribbon[5]
		if bodies.DataState != "bodies-on" {
			t.Errorf("capture+masked = bodies-on; got %q", bodies.DataState)
		}
		if !strings.Contains(bodies.Detail, "redacted") {
			t.Errorf("masked-mode detail should mention redaction: %q", bodies.Detail)
		}
	})
	t.Run("capture + unmask = danger state", func(t *testing.T) {
		sess := model.Session{CaptureBodies: true, CaptureBodiesUnmasked: true}
		ribbon := webRibbonFromSession(sess, "", true, 0)
		bodies := ribbon[5]
		if bodies.DataState != "bodies-unmasked" {
			t.Errorf("DataState = %q, want bodies-unmasked", bodies.DataState)
		}
		// The summary value carries the ⚠ marker; the verbose UNMASKED copy now
		// lives in the detail line (the value is the compact "request + …" form).
		if !strings.Contains(bodies.Value, "⚠") {
			t.Errorf("Value must contain ⚠ marker; got %q", bodies.Value)
		}
		if !strings.Contains(bodies.Detail, "UNMASKED") {
			t.Errorf("Detail must contain UNMASKED marker; got %q", bodies.Detail)
		}
		if !strings.Contains(bodies.Detail, "CCWRAP_UNMASK_CREDENTIALS") {
			t.Errorf("Detail should name the env var; got %q", bodies.Detail)
		}
	})
}

// TestBodiesCellPresentation_Matrix locks the full 3-arg (captureBodies,
// captureBodiesUnmasked, captureTelemetry) presentation matrix. The Bodies
// cell now summarizes TWO capture toggles (request bodies + telemetry bodies),
// so the value joins the present parts with " + " and the detail/state reflect
// the combination. updateBodiesCell() in the inline JS mirrors this byte-for-byte.
func TestBodiesCellPresentation_Matrix(t *testing.T) {
	cases := []struct {
		name                             string
		bodies, unmasked, telemetry      bool
		wantValue, wantDetail, wantState string
	}{
		{
			name:       "off",
			wantValue:  "off",
			wantDetail: "click to choose what to capture",
			wantState:  "bodies-off",
		},
		{
			name:       "request only",
			bodies:     true,
			wantValue:  "request",
			wantDetail: "recording request + response bodies (credentials redacted)",
			wantState:  "bodies-on",
		},
		{
			name:       "request + unmasked",
			bodies:     true,
			unmasked:   true,
			wantValue:  "request ⚠",
			wantDetail: "UNMASKED — CCWRAP_UNMASK_CREDENTIALS=1; credentials in drawer + spill",
			wantState:  "bodies-unmasked",
		},
		{
			name:       "telemetry only",
			telemetry:  true,
			wantValue:  "telemetry",
			wantDetail: "capturing telemetry bodies (Datadog/Sentry)",
			wantState:  "bodies-on",
		},
		{
			name:       "request + telemetry",
			bodies:     true,
			telemetry:  true,
			wantValue:  "request + telemetry",
			wantDetail: "recording request + response + telemetry bodies (credentials redacted)",
			wantState:  "bodies-on",
		},
		{
			name:       "request + telemetry + unmasked",
			bodies:     true,
			telemetry:  true,
			unmasked:   true,
			wantValue:  "request + telemetry ⚠",
			wantDetail: "UNMASKED — CCWRAP_UNMASK_CREDENTIALS=1; credentials in drawer + spill",
			wantState:  "bodies-unmasked",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, detail, state := bodiesCellPresentation(tc.bodies, tc.unmasked, tc.telemetry)
			if value != tc.wantValue {
				t.Errorf("value = %q, want %q", value, tc.wantValue)
			}
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
		})
	}
}

// TestWebRibbon_BodiesCell_Telemetry confirms CaptureTelemetry threads from the
// session into the Bodies cell value/detail at the webRibbonFromSession call site.
func TestWebRibbon_BodiesCell_Telemetry(t *testing.T) {
	t.Run("telemetry only", func(t *testing.T) {
		sess := model.Session{CaptureTelemetry: true}
		bodies := webRibbonFromSession(sess, "", true, 0)[5]
		if bodies.Value != "telemetry" {
			t.Errorf("Value = %q, want telemetry", bodies.Value)
		}
		if bodies.DataState != "bodies-on" {
			t.Errorf("DataState = %q, want bodies-on", bodies.DataState)
		}
	})
	t.Run("request + telemetry", func(t *testing.T) {
		sess := model.Session{CaptureBodies: true, CaptureTelemetry: true}
		bodies := webRibbonFromSession(sess, "", true, 0)[5]
		if bodies.Value != "request + telemetry" {
			t.Errorf("Value = %q, want 'request + telemetry'", bodies.Value)
		}
	})
}

// TestWebRibbon_BodiesCell_UnmaskedRenderedHTML confirms the danger-state
// data-state token reaches the rendered HTML so the CSS rule binds.
func TestWebRibbon_BodiesCell_UnmaskedRenderedHTML(t *testing.T) {
	sess := model.Session{CaptureBodies: true, CaptureBodiesUnmasked: true}
	rib := webRibbonFromSession(sess, "", true, 0)
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", Ribbon: rib})
	html := buf.String()
	if !strings.Contains(html, `data-state="bodies-unmasked"`) {
		t.Errorf("HTML missing data-state=bodies-unmasked")
	}
	if !strings.Contains(html, "UNMASKED") {
		t.Errorf("HTML missing UNMASKED label")
	}
}

// TestWebRibbon_BodiesCell_RenderedHTML confirms the server first-paint emits
// the data-ribbon="Bodies" cell with data-state, value, and detail — the JS
// click handler queries by [data-ribbon="Bodies"] so this DOM hook must exist.
func TestWebRibbon_BodiesCell_RenderedHTML(t *testing.T) {
	for _, on := range []bool{false, true} {
		sess := model.Session{CaptureBodies: on}
		rib := webRibbonFromSession(sess, "", true, 0)
		var buf bytes.Buffer
		renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", Ribbon: rib})
		html := buf.String()
		if !strings.Contains(html, `data-ribbon="Bodies"`) {
			t.Errorf("on=%v: HTML missing data-ribbon=Bodies cell", on)
		}
		wantState := "bodies-off"
		if on {
			wantState = "bodies-on"
		}
		if !strings.Contains(html, `data-state="`+wantState+`"`) {
			t.Errorf("on=%v: HTML missing data-state=%q", on, wantState)
		}
	}
}

// TestNativeTLSCellPresentation_Matrix locks the nativeTLSCellPresentation
// contract: off (or empty) -> stdlib default; active -> mirroring; blocked:
// <reason> -> an error condition whose detail is the bare reason. The JS
// updateNativeTLSCell() mirrors this byte-for-byte (see the verbatim test
// below) so the SSE live patch never drifts from the server first paint.
func TestNativeTLSCellPresentation_Matrix(t *testing.T) {
	cases := []struct {
		nativeTLS                        string
		blocks                           int
		loaded                           bool
		wantValue, wantDetail, wantState string
	}{
		{"", 0, false, "off", "stdlib TLS (default)", "native-off"},
		{"off", 0, false, "off", "stdlib TLS (default)", "native-off"},
		{"active", 0, false, "active", "mirroring Claude Code TLS fingerprint", "native-active"},
		// active with a LOADED hello (CCWRAP_NATIVE_TLS_HELLO): the fingerprint
		// was supplied, not captured from Claude Code, so the detail says so.
		{"active", 0, true, "active", "mirroring loaded fingerprint", "native-active"},
		// active with prior block episodes: the count is surfaced so a healed
		// transient block leaves a visible trace.
		{"active", 1, false, "active", "mirroring · 1 prior block(s)", "native-active"},
		{"active", 3, false, "active", "mirroring · 3 prior block(s)", "native-active"},
		{"blocked: handshake error", 1, false, "blocked", "handshake error", "native-blocked"},
		{"blocked: x", 2, false, "blocked", "x", "native-blocked"},
	}
	for _, c := range cases {
		value, detail, state := nativeTLSCellPresentation(c.nativeTLS, c.blocks, c.loaded)
		if value != c.wantValue || detail != c.wantDetail || state != c.wantState {
			t.Errorf("nativeTLSCellPresentation(%q,%d,%t) = (%q,%q,%q), want (%q,%q,%q)",
				c.nativeTLS, c.blocks, c.loaded, value, detail, state, c.wantValue, c.wantDetail, c.wantState)
		}
	}
}

// TestWebRibbon_NativeTLSCell_Visibility — the NATIVE TLS cell is hidden when
// the feature is not in use (NativeTLS == "") and appended (after Egress) when
// the session carries any non-empty native_tls state.
func TestWebRibbon_NativeTLSCell_Visibility(t *testing.T) {
	// Not opted in: no NATIVE TLS cell, ribbon stays at 7.
	off := webRibbonFromSession(model.Session{}, "", true, 0)
	if len(off) != 7 {
		t.Fatalf("NativeTLS empty: ribbon len = %d, want 7 (cell hidden)", len(off))
	}
	for _, cell := range off {
		if cell.Label == "NATIVE TLS" {
			t.Fatalf("NativeTLS empty must NOT emit a NATIVE TLS cell")
		}
	}
	// Active: cell appended as an 8th cell after Egress.
	on := webRibbonFromSession(model.Session{NativeTLS: "active"}, "", true, 0)
	if len(on) != 8 {
		t.Fatalf("NativeTLS active: ribbon len = %d, want 8", len(on))
	}
	last := on[7]
	if last.Label != "NATIVE TLS" {
		t.Fatalf("8th cell Label = %q, want NATIVE TLS", last.Label)
	}
	if last.Value != "active" || last.DataState != "native-active" {
		t.Errorf("active cell = (%q,%q), want (active,native-active)", last.Value, last.DataState)
	}
	// Active with prior block episodes: the count is threaded into the detail so
	// a healed transient block leaves a visible trace.
	healed := webRibbonFromSession(model.Session{NativeTLS: "active", NativeTLSFallbacks: 2}, "", true, 0)
	if d := healed[7].Detail; d != "mirroring · 2 prior block(s)" {
		t.Errorf("active-with-blocks detail = %q, want the prior-block trace", d)
	}
	// Blocked is an error condition.
	fb := webRibbonFromSession(model.Session{NativeTLS: "blocked: boom"}, "", true, 0)
	if fb[7].DataState != "native-blocked" || fb[7].Value != "blocked" {
		t.Errorf("blocked cell = (%q,%q), want (blocked,native-blocked)", fb[7].Value, fb[7].DataState)
	}
	if fb[7].Detail != "boom" {
		t.Errorf("blocked detail = %q, want bare reason boom", fb[7].Detail)
	}
}

// TestWebRibbon_NativeTLSCell_RenderedHTML confirms the server first-paint emits
// the data-ribbon="NATIVE TLS" cell with its data-state — the JS live updater
// queries by [data-ribbon="NATIVE TLS"] so this DOM hook must exist.
func TestWebRibbon_NativeTLSCell_RenderedHTML(t *testing.T) {
	rib := webRibbonFromSession(model.Session{NativeTLS: "blocked: boom"}, "", true, 0)
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", Ribbon: rib})
	html := buf.String()
	if !strings.Contains(html, `data-ribbon="NATIVE TLS"`) {
		t.Errorf("HTML missing data-ribbon=NATIVE TLS cell")
	}
	if !strings.Contains(html, `data-state="native-blocked"`) {
		t.Errorf("HTML missing data-state=native-blocked")
	}
}

// TestSP3InlineScript_NativeTLSCell — the inline JS must carry an
// updateNativeTLSCell that reads state.session.native_tls, AND the exact
// value/detail/state strings the Go helper produces must appear verbatim in
// the JS body (the Go↔JS byte-equality discipline). patchSession must call it.
func TestSP3InlineScript_NativeTLSCell(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	if !strings.Contains(js, "function updateNativeTLSCell(") {
		t.Errorf("inline JS must define updateNativeTLSCell()")
	}
	if !strings.Contains(js, "state.session.native_tls") {
		t.Errorf("inline JS updateNativeTLSCell must read state.session.native_tls")
	}
	if !regexp.MustCompile(`patchSession[\s\S]*updateNativeTLSCell\(\)`).MatchString(js) {
		t.Errorf("patchSession must call updateNativeTLSCell() so the cell repaints on SSE session_updated")
	}
	// Byte-equality: every STATIC value/detail/state string the Go helper
	// emits must appear verbatim in the JS so the live patch can never drift.
	// (The blocked detail is the runtime reason, not a literal — excluded.)
	for _, in := range []string{"", "off", "active"} {
		v, d, s := nativeTLSCellPresentation(in, 0, false)
		for _, want := range []string{v, d, s} {
			if !strings.Contains(js, want) {
				t.Errorf("Go helper string %q (from %q) missing verbatim in JS", want, in)
			}
		}
	}
	// The blocked path's static strings must also appear verbatim: the value,
	// the data-state, and the "blocked: " prefix the JS strips to recover the
	// bare reason (mirrors strings.TrimPrefix in nativeTLSCellPresentation).
	for _, want := range []string{"blocked", "native-blocked", "blocked: "} {
		if !strings.Contains(js, want) {
			t.Errorf("blocked static string %q missing verbatim in JS", want)
		}
	}
	// The active-with-prior-blocks detail is runtime (count is variable), so its
	// static pieces must appear verbatim and the JS must read native_tls_fallbacks
	// (mirrors fmt.Sprintf("mirroring · %d prior block(s)", blocks) in Go).
	for _, want := range []string{"mirroring · ", " prior block(s)", "native_tls_fallbacks"} {
		if !strings.Contains(js, want) {
			t.Errorf("active-with-blocks static piece %q missing verbatim in JS", want)
		}
	}
	// The LOADED-hello detail (CCWRAP_NATIVE_TLS_HELLO) is a static literal and
	// the JS must read native_tls_loaded to select it (mirrors the loaded branch
	// in nativeTLSCellPresentation).
	for _, want := range []string{"mirroring loaded fingerprint", "native_tls_loaded"} {
		if !strings.Contains(js, want) {
			t.Errorf("loaded-fingerprint static piece %q missing verbatim in JS", want)
		}
	}
}

// TestRibbonCellKeyboardA11y locks keyboard operability of the interactive
// ribbon cells (Profile/Models/Bodies/NATIVE TLS): they must be focusable
// (tabindex), expose role=button, and activate on Enter/Space — not mouse-only.
func TestRibbonCellKeyboardA11y(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatal("inline script not found")
	}
	js := m[1]
	for _, want := range []string{"function makeRibbonCellActivable(", "role", "tabindex", "keydown", "aria-expanded"} {
		if !strings.Contains(js, want) {
			t.Errorf("keyboard-a11y JS missing %q", want)
		}
	}
	// the helper must be wired for each interactive cell
	if strings.Count(js, "makeRibbonCellActivable(") < 5 { // 1 def + >=4 calls
		t.Errorf("makeRibbonCellActivable must be called for all 4 interactive cells, found %d uses", strings.Count(js, "makeRibbonCellActivable("))
	}
}

// TestRibbonCellActivable_DescendantKeydown is the regression test for the bug
// where Space typed inside the profile create/edit popover (a DOM child of the
// profile cell) closed the popover and ate the keystroke: the cell's role=button
// keydown activation fired for keydowns bubbling up from the form inputs.
// Activation must happen ONLY when the cell itself is the event target, never a
// descendant control. Lifts the real makeRibbonCellActivable and runs it under a
// tiny bubbling-DOM shim in node.
func TestRibbonCellActivable_DescendantKeydown(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	js := inlineScript(t)
	const driver = `
function makeEl(){
  var listeners = {};
  var el = {
    parentNode: null, clickCount: 0,
    setAttribute: function(){}, getAttribute: function(){ return null; },
    addEventListener: function(type, fn){ (listeners[type] = listeners[type] || []).push(fn); },
    appendChild: function(child){ child.parentNode = el; return child; },
    click: function(){ el.clickCount++; },
    _listeners: listeners
  };
  return el;
}
function dispatchKeydown(target, key){
  var ev = { type: 'keydown', key: key, target: target, currentTarget: null, defaultPrevented: false, preventDefault: function(){ this.defaultPrevented = true; } };
  var node = target;
  while (node){ ev.currentTarget = node; var ls = node._listeners['keydown'] || []; for (var i = 0; i < ls.length; i++){ ls[i].call(node, ev); } node = node.parentNode; }
  return ev;
}
var cell = makeEl();
makeRibbonCellActivable(cell);
var input = makeEl();
cell.appendChild(input);
var fromDescendant = dispatchKeydown(input, ' ');
var afterDescendant = cell.clickCount;
var fromCell = dispatchKeydown(cell, ' ');
process.stdout.write(JSON.stringify({
  descendantClicks: afterDescendant,
  descendantPrevented: fromDescendant.defaultPrevented,
  cellActivates: cell.clickCount - afterDescendant,
  cellPrevented: fromCell.defaultPrevented
}));
`
	prog := reconstructFn(t, js, "makeRibbonCellActivable") + "\n" + driver
	cmd := exec.Command("node")
	cmd.Stdin = strings.NewReader(prog)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node run failed: %v\n%s", err, out)
	}
	var r struct {
		DescendantClicks    int  `json:"descendantClicks"`
		DescendantPrevented bool `json:"descendantPrevented"`
		CellActivates       int  `json:"cellActivates"`
		CellPrevented       bool `json:"cellPrevented"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("driver output not JSON: %v\n%s", err, out)
	}
	if r.DescendantClicks != 0 {
		t.Errorf("Space bubbling from a descendant input activated the cell %d time(s) — the popover would close and the edit be lost", r.DescendantClicks)
	}
	if r.DescendantPrevented {
		t.Errorf("Space from a descendant input was preventDefault'd — the character would never type into the field")
	}
	if r.CellActivates != 1 {
		t.Errorf("Space on the focused cell itself activated it %d time(s), want 1 (keyboard a11y must still work)", r.CellActivates)
	}
	if !r.CellPrevented {
		t.Errorf("Space on the focused cell was not preventDefault'd (the page would scroll)")
	}
}

// TestNativeTLSPopover_Contracts locks the clickable-cell popover affordance:
// the inline JS gains openNativeTLSPopover + closeAllRibbonPops + a /native-tls
// lazy fetch, all WITHOUT adding a second <script> and WITHOUT navigator.clipboard
// (the dashboard can be reached over a plain-HTTP tunnel where that API is undefined).
func TestNativeTLSPopover_Contracts(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	html := buf.String()
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("<script> count=%d want 2", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	js := m[1]
	for _, want := range []string{"openNativeTLSPopover", "closeAllRibbonPops", "/native-tls"} {
		if !strings.Contains(js, want) {
			t.Errorf("inline JS missing %q", want)
		}
	}
	if strings.Contains(js, "navigator.clipboard") {
		t.Error("must use select-to-copy, not navigator.clipboard")
	}
}

func TestWebRibbon_ProfileNameHTMLEscaped(t *testing.T) {
	// Profile name with HTML metacharacters must be escaped in ValueHTML.
	sess := model.Session{ActiveProfileName: `evil<script>"alert"</script>`, ActiveProfileProvider: "X"}
	ribbon := webRibbonFromSession(sess, "", true, 1)
	html := string(ribbon[4].ValueHTML)
	if strings.Contains(html, "<script>") {
		t.Fatalf("profile name MUST be escaped: %q", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("profile name must appear escape-encoded: %q", html)
	}
}

func TestWebActivityA11yAndSynthetic(t *testing.T) {
	page := webPageData{
		Title:         "x",
		LiveEnabled:   true,
		ActivityTitle: "Live activity",
		ActivityRows: []webRow{
			{Time: "15:58:59", Label: "POST", Main: "/v1/messages", Right: "200", Mono: true, Forwarded: true, Kind: "request"},
			{Time: "15:58:59", Label: "SYNTHETIC", Main: "/v1/mcp_servers", Right: "200", Mono: true, Forwarded: false, Kind: "request"},
			{Time: "15:58:55", Label: "trace", Main: "forwarding request", Right: "api → up", Kind: "trace"},
		},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()
	mustContainAllWeb(t, html,
		`aria-live="polite"`,
		`class="skip-link"`,
		`href="#activity-body"`, // skip target
		"SYNTHETIC",             // Web keeps full word
		`data-forwarded="true"`, // forwarded rail hook
		`data-kind="trace"`,     // trace coloring hook
		`role="list"`, `role="listitem"`,
	)
	// Honest list semantics (UI/UX review 方案A): the old role=table/row/cell
	// tree violated ARIA table ownership (cells under an unroled <summary>,
	// the .reqinspect inspector as a non-cell row child), so SR table
	// navigation half-worked at best. Rows are disclosure list items now;
	// the table roles must be fully gone.
	for _, gone := range []string{`role="table"`, `role="row"`, `role="cell"`} {
		if strings.Contains(html, gone) {
			t.Fatalf("table ARIA retired for list semantics; %q must not render", gone)
		}
	}
	// Web errors must surface SuggestedAction even when UpstreamHost set.
	erows := unifiedActivityRows(nil, []model.ErrorRecord{
		{UpstreamHost: "api.x", SuggestedAction: "retry without proxy", Summary: "dial failed"},
	}, nil, 8, false, false)
	if len(erows) != 1 || !strings.Contains(erows[0].Right, "retry without proxy") {
		t.Fatalf("error row must surface SuggestedAction, got %+v", erows)
	}
}

func TestWebEndedDisablesLive(t *testing.T) {
	ended := webPageData{Title: "x", HeroState: "Ended", HeroVariant: "ended", LiveEnabled: false}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, ended)
	if strings.Contains(buf.String(), `id="live-toggle"`) {
		t.Fatalf("ended session must not render the live-toggle button")
	}
	live := webPageData{Title: "x", HeroState: "Active", HeroVariant: "active", LiveEnabled: true}
	buf.Reset()
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, live)
	if !strings.Contains(buf.String(), `id="live-toggle"`) {
		t.Fatalf("live session must render the live-toggle button")
	}
}

func TestWebTopbarIconButtons(t *testing.T) {
	live := webPageData{Title: "x", HeroState: "Active", HeroVariant: "active", LiveEnabled: true}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, live)
	html := buf.String()
	// Refresh + live-toggle are SVG icon buttons (not bare text).
	mustContainAllWeb(t, html,
		`id="refresh-btn"`, `href="#i-refresh-cw"`, `aria-label="Refresh"`,
		`id="live-toggle"`, `href="#i-loader"`, `aria-label="Pause stream"`,
		`class="btn btn-icon"`,
	)
	// The accessible name lives in title/aria-label, never as bare button text.
	if strings.Contains(html, `>Refresh</button>`) {
		t.Fatalf("Refresh must be an icon button, not text")
	}
	if strings.Contains(html, `>Pause stream</button>`) {
		t.Fatalf("live-toggle must be an icon button, not text")
	}
	// The SVG inside .btn-icon needs an explicit CSS size; without it the
	// flex button collapses the icon to a ~2px sliver (width attr ignored
	// once it's a no-viewBox flex item). Mirror the other icon-button rules.
	mustContainAllWeb(t, html, `.btn-icon svg{display:block;width:15px;height:15px;flex:none}`)
}

func TestWebTopbarConnPillAndMenuIcons(t *testing.T) {
	live := webPageData{
		Title: "x", HeroState: "Active", HeroVariant: "active", LiveEnabled: true,
		Links: []webLink{
			{Label: "Health JSON", Href: "/healthz", Icon: "i-activity"},
			{Label: "Recent JSON", Href: "/recent", Icon: "i-clock"},
		},
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, live)
	html := buf.String()
	// Connection pill lives in the app-bar (id preserved for JS), not the hero.
	mustContainAllWeb(t, html, `class="state-pill app-stream-pill"`, `id="live-chip"`, `id="live-label"`)
	if strings.Contains(html, "hero-stream-pill") {
		t.Fatalf("stream pill must move out of the hero to the app-bar")
	}
	// Overflow menu items carry their design icons.
	mustContainAllWeb(t, html, `href="#i-activity"`, `href="#i-clock"`, "Health JSON", "Recent JSON")
}

func TestRenderedSessionPageScriptParses(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}

	page := webPageData{
		Title:         "CCWRAP Session",
		Heading:       "CCWRAP Session",
		Subtitle:      "Network diagnostics",
		LiveEnabled:   true,
		BootstrapB64:  bootstrapB64(pageBootstrap{EventsURL: "/events"}),
		ActivityTitle: "Recent activity",
	}

	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)

	matches := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(buf.Bytes())
	if len(matches) != 2 {
		t.Fatalf("script tag not found in rendered page")
	}

	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = bytes.NewReader(matches[1])
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rendered page script should parse: %v\n%s", err, out)
	}

	script := string(matches[1])
	if !bytes.Contains(matches[1], []byte("addEventListener('proxy_error'")) {
		t.Fatalf("rendered page script should listen for proxy_error events")
	}
	if bytes.Contains(matches[1], []byte("addEventListener('error'")) {
		t.Fatalf("rendered page script should reserve EventSource error for transport errors")
	}
	if !regexp.MustCompile(`(?m)es\.onerror\s*=`).MatchString(script) {
		t.Fatalf("rendered page script should still handle EventSource transport errors")
	}
}

type noopResponseWriter struct {
	header http.Header
	b      *bytes.Buffer
}

func (n noopResponseWriter) Header() http.Header         { return n.header }
func (n noopResponseWriter) Write(p []byte) (int, error) { return n.b.Write(p) }
func (n noopResponseWriter) WriteHeader(statusCode int)  {}

func TestLiveScriptUsesBootstrapDenyList(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]
	if !strings.Contains(js, "header_deny_list") {
		t.Fatalf("live script must consume bootstrap header_deny_list (single source)")
	}
	// The live header container is the header sub-accordion
	// (`<details class="req-sub"><summary>request headers</summary>
	// <div class="sub-body">…`); the structural string points at that
	// shape. The deny-list / ‹redacted by ccwrap› credential-redaction
	// security assertion is kept byte-verbatim (the real value is still
	// never emitted).
	mustContainAllWeb(t, js, "‹redacted by ccwrap›", `'req-sub';`, "request headers")
	// Lock the wiring location: the panel must reach the LIVE ACTIVITY
	// feed builder (activityRowData → headerAnnotate), not the
	// Requests-section builder.
	aStart := strings.Index(js, "function activityRowData")
	if aStart < 0 {
		t.Fatalf("activityRowData not found in live script")
	}
	rest := js[aStart+len("function activityRowData"):]
	end := strings.Index(rest, "\n  function ") // next top-level helper
	if end < 0 {
		end = len(rest)
	}
	if !strings.Contains(rest[:end], "headerAnnotate(") {
		t.Fatalf("activityRowData must call headerAnnotate (panel scoped to the live activity feed)")
	}
	if rStart := strings.Index(js, "function requestRowData"); rStart >= 0 {
		rRest := js[rStart:]
		if rEnd := strings.Index(rRest[1:], "\n  function "); rEnd >= 0 && strings.Contains(rRest[:rEnd+1], "headerGroups") {
			t.Fatalf("requestRowData must NOT carry the header panel (Requests-section list is not the header-panel surface)")
		}
	}
}

// TestInlineScriptUnifiedPatcherAndFilter locks the collapse: the
// four per-section patchers become ONE filter-aware Activity patcher
// that reads the Go-supplied rec.class (no JS re-derivation).
// Extraction + node --check reuse the exact mechanism of the canonical
// TestRenderedSessionPageScriptParses / TestLiveScriptUsesBootstrapDenyList
// (single bare <script> regexp; "node --check -" over its body; same
// node-absent skip behavior).
func TestInlineScriptUnifiedPatcherAndFilter(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	html := buf.String()

	// (1) Exactly ONE bare <script> + the ccwrap-bootstrap JSON node:
	// count of "<script" == 2; no new <script> (single-script contract).
	if n := strings.Count(html, `<script id="ccwrap-bootstrap"`); n != 1 {
		t.Fatalf("want exactly 1 ccwrap-bootstrap script node, got %d", n)
	}
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("want 2 total <script (bootstrap json + inline mirror), got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// (2) Extracted inline script still parses under node --check (same
	// mechanism + same node-absent skip as TestRenderedSessionPageScriptParses).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = strings.NewReader(js)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inline script should parse under node --check: %v\n%s", err, out)
	}

	// (3) The removed-section machinery is GONE (sections collapsed).
	for _, gone := range []string{
		"findSectionBody", "findDiagSubsectionBody", "createDiagSubsection",
		"patchRequests", "patchErrors", "patchTrace",
	} {
		if strings.Contains(js, gone) {
			t.Fatalf("D1 must remove per-section machinery, still present: %q", gone)
		}
	}

	// (4) Exactly ONE patchActivity( and it reads the Go-supplied class.
	if n := strings.Count(js, "function patchActivity("); n != 1 {
		t.Fatalf("want exactly 1 patchActivity definition, got %d", n)
	}
	if !strings.Contains(js, "rec.class") {
		t.Fatalf("Option X: live path must read rec.class (no JS re-derivation)")
	}

	// (5) Filter wiring is FILTER-AWARE CAPPING, not the old
	// cap-then-filter hide path. An earlier approach toggled a `.fhide`
	// class to hide non-matching rows, which buries /v1/messages under
	// >50 noise rows; the corrected contract is the INVERSE — the active
	// filter is read from #activity-filter and consulted (activeFilter)
	// to decide what is BUILT (conditional insert in patchActivity over
	// the Go-supplied data-class), and the .fhide/applyFilterTo hide
	// path is absent everywhere.
	mustContainAllWeb(t, js, "#activity-filter", "data-class", "activeFilter()")
	for _, gone := range []string{"fhide", "applyFilterTo", "applyFilterAll"} {
		if strings.Contains(html, gone) {
			t.Fatalf("filter-aware capping must remove the hide path; still present: %q", gone)
		}
	}

	// (6) Reused-unchanged anchors still present:
	// the global toggle listener + the body/header drawer builders.
	mustContainAllWeb(t, js,
		"document.addEventListener('toggle'",
		"renderBodyView", "headerPanelEl", "bodyDrawerEl",
	)
}

// TestActivityKindLabelFirstPaintLiveParity locks that the Activity
// "Kind" column (== webRow.Label) is single-sourced on the class for
// BOTH the Go server first-paint AND the JS live mirror, so the same
// forwarded-api/synthetic/tunnel record reads identically on first paint,
// after a live re-render, and after a reconnect rebuild.
//
// Divergence the fix addresses: Go's unifiedActivityRows already sets
// request rows' Label to the class string
// ("forwarded-api"/"synthetic"/"tunnel"), and error/trace rows to
// "error"/"trace"; the JS activityRowData request-family branch instead
// hardcoded the literal label 'request', while its error/trace branches
// already matched Go. The fix aligns JS to Go: derive the JS
// request-family label from the class arg (the filter-taxonomy-consistent
// first-paint behavior — the filter buttons are class-named). This test
// guards BOTH directions. Extraction + node --check reuse the exact
// mechanism of the canonical TestRenderedSessionPageScriptParses /
// TestInlineScriptUnifiedPatcherAndFilter (single bare <script> regexp;
// "<script" count == 2; "node --check -" over its body; same node-absent
// skip), and the JS scope is taken via the existing scriptFnBody helper.
func TestActivityKindLabelFirstPaintLiveParity(t *testing.T) {
	// (A) Go side — unifiedActivityRows via the REAL production builder.
	// Each request-family row's Label must equal its Class; error/trace
	// rows' Label must be "error"/"trace". Guards Go stays class-based
	// (should already PASS — Go is the correct, already-shipped side).
	reqs := []model.RequestRecord{
		{Method: "POST", Path: "/v1/messages"},         // → forwarded-api
		{Method: "GET", Synthetic: true},               // → synthetic
		{Method: http.MethodConnect, Path: "host:443"}, // → tunnel
	}
	errs := []model.ErrorRecord{{Summary: "boom"}}
	tr := []model.TraceRecord{{Summary: "started"}}
	rows := unifiedActivityRows(reqs, errs, tr, 50, false, false)
	if len(rows) != 5 {
		t.Fatalf("want 5 unified rows (3 request + 1 error + 1 trace), got %d", len(rows))
	}
	for _, r := range rows {
		switch r.Class {
		case "forwarded-api", "synthetic", "tunnel":
			if r.Label != r.Class {
				t.Fatalf("Go first-paint: request-family Kind label must be single-sourced on class; row class=%q got Label=%q want %q", r.Class, r.Label, r.Class)
			}
		case "error":
			if r.Label != "error" {
				t.Fatalf("Go first-paint: error row Label = %q, want \"error\"", r.Label)
			}
		case "trace":
			if r.Label != "trace" {
				t.Fatalf("Go first-paint: trace row Label = %q, want \"trace\"", r.Label)
			}
		default:
			t.Fatalf("unexpected row class %q", r.Class)
		}
	}

	// (B) JS side — extract the single bare <script> (same mechanism /
	// same "<script" count == 2 contract as the canonical script tests).
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	html := buf.String()
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("want 2 total <script (bootstrap json + inline mirror), got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// The activityRowData request-family branch must NOT emit the
	// hardcoded 'request' Kind label; it must derive the label from the
	// class arg (label: cls), mirroring Go's `Label: cls`. The
	// error/trace branches legitimately keep label: 'error'/'trace'
	// (class-consistent — class is "error"/"trace" there, matching Go),
	// so the negative assertion is scoped precisely to the
	// pre-fix request-family literal. scriptFnBody is the existing helper
	// that brace-matches a single function body.
	fn := scriptFnBody(t, js, "activityRowData")
	if strings.Contains(fn, "label: 'request'") {
		t.Fatalf("activityRowData must not hardcode the request-family Kind label 'request'; single-source it on the class (label: cls) to match Go's Label: cls")
	}
	if !strings.Contains(fn, "label: cls") {
		t.Fatalf("activityRowData request-family branch must derive the Kind label from the class arg (label: cls), mirroring Go's Label: cls")
	}
	// Sanity: the error/trace class-consistent literals are still present
	// (only the request-family literal is single-sourced; the JS error/
	// trace branches already matched Go and stay byte-identical).
	mustContainAllWeb(t, fn, "label: 'error'", "label: 'trace'")

	// (C) Extracted inline script still parses under node --check (same
	// mechanism + same node-absent skip as the canonical script tests).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = strings.NewReader(js)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inline script should parse under node --check: %v\n%s", err, out)
	}
}

func TestForwardedRowHeaderPanelRedacted(t *testing.T) {
	rec := model.RequestRecord{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		RequestHeaders: http.Header{
			"Authorization":     {"Bearer sk-PAGESECRET"},
			"Anthropic-Version": {"2023-06-01"},
			"X-Future-Thing":    {"shown-verbatim"},
		},
	}
	// Build via the REAL production builder for the unified Activity feed
	// (unifiedActivityRows). A forwarded-api row carries the header panel;
	// the unified Activity feed has no separate Requests section.
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 5, false, false)
	if len(rows) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(rows))
	}
	if len(rows[0].HeaderGroups) == 0 {
		t.Fatalf("forwarded activity row with headers must have HeaderGroups")
	}
	// Drawer doctrine: the expandable header drawer / body ref are
	// forwarded-api-ONLY. A non-forwarded-api row (synthetic/CONNECT) must
	// have NO HeaderGroups and NO BodyRefID — but it MUST still carry its
	// non-expandable reason note (security intent preserved: no header
	// bytes leak on non-forwarded rows).
	if synR := unifiedActivityRows([]model.RequestRecord{{Method: "GET", Synthetic: true}}, nil, nil, 12, false, false); len(synR[0].HeaderGroups) != 0 || synR[0].BodyRefID != "" || synR[0].HeaderNote == "" {
		t.Fatalf("non-forwarded-api (synthetic) row must carry NO header drawer / no body but still its reason note, got %+v", synR[0])
	}

	page := webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		ActivityRows: []webRow{rows[0]},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()

	// LOAD-BEARING: the secret must not appear anywhere in the page.
	if strings.Contains(html, "sk-PAGESECRET") {
		t.Fatalf("credential value leaked into rendered HTML")
	}
	mustContainAllWeb(t, html, "‹redacted by ccwrap›", "Authorization", "2023-06-01", "shown-verbatim")

	syn := unifiedActivityRows([]model.RequestRecord{{Method: "GET", Synthetic: true}}, nil, nil, 5, false, false)
	if len(syn[0].HeaderGroups) != 0 {
		t.Fatalf("synthetic row must not be expandable")
	}
	if syn[0].HeaderNote == "" {
		t.Fatalf("synthetic row must carry a non-expandable reason note")
	}
	conn := unifiedActivityRows([]model.RequestRecord{{Method: http.MethodConnect}}, nil, nil, 5, false, false)
	if len(conn[0].HeaderGroups) != 0 || conn[0].HeaderNote == "" {
		t.Fatalf("CONNECT row must be non-expandable with a reason")
	}
}

func TestBootstrapEmbedsDenyList(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title:        "x",
		LiveEnabled:  true, // the ccwrap-bootstrap node is gated by {{if .LiveEnabled}}
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	html := buf.String()
	m := regexp.MustCompile(`data-b64="([^"]+)"`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("bootstrap node not found")
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("bootstrap b64 decode: %v", err)
	}
	for _, want := range []string{"authorization", "x-api-key", "cookie", "proxy-authorization"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("bootstrap deny-list missing %q\n%s", want, raw)
		}
	}
}

func TestActivityRowExposesBodyEndpointWhenBodyRef(t *testing.T) {
	rec := model.RequestRecord{
		Method: "POST", Path: "/v1/messages",
		LogicalTargetHost: "api.anthropic.com",
		BodyRef:           &model.RequestBodyRef{ID: "abc123def4567890", Size: 88668, SHA256: "deadbeef"},
	}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 5, false, false)
	if len(rows) == 0 {
		t.Fatalf("no rows")
	}
	if rows[0].BodyRefID != "abc123def4567890" {
		t.Fatalf("row must expose BodyRefID for lazy fetch, got %+v", rows[0])
	}
	noref := unifiedActivityRows([]model.RequestRecord{{Method: "GET"}}, nil, nil, 5, false, false)
	if len(noref) == 0 || noref[0].BodyRefID != "" {
		t.Fatalf("row without BodyRef must not expose an id, got %+v", noref)
	}
	// The body affordance is request-only, exactly like HeaderGroups:
	// error/trace rows must never carry a BodyRefID.
	errRow := unifiedActivityRows(nil, []model.ErrorRecord{{Summary: "boom"}}, nil, 5, false, false)
	if len(errRow) == 0 || errRow[0].BodyRefID != "" {
		t.Fatalf("error row must not carry BodyRefID, got %+v", errRow)
	}
	trRow := unifiedActivityRows(nil, nil, []model.TraceRecord{{Summary: "tick"}}, 5, false, false)
	if len(trRow) == 0 || trRow[0].BodyRefID != "" {
		t.Fatalf("trace row must not carry BodyRefID, got %+v", trRow)
	}
}

// TestRenderedPageStillSingleBootstrapScriptWithBodyDrawer locks the
// LOAD-BEARING single-script contract: the body-drawer affordance must
// add ZERO new <script> nodes — only a collapsed nested
// <details class="req-sub body-drawer"> (nested in the forwarded row's
// reqinspect) carrying the hex data-reqid.
// Render+extract mirror the real
// node-check test (TestRenderedSessionPageScriptParses).
func TestRenderedPageStillSingleBootstrapScriptWithBodyDrawer(t *testing.T) {
	// The body-drawer is nested INSIDE the forwarded-row
	// `{{if .HeaderGroups}}` branch (web.go template), so a forwarded-api
	// row must carry captured request headers for the
	// `<details class="row">` + nested
	// `<details class="req-sub body-drawer">` to render. RequestHeaders is
	// the only added field; the BodyRef (id/size/sha) is unchanged, so the
	// "bytes/metadata never inlined" contract is asserted exactly.
	withRef := model.RequestRecord{
		Method: "POST", Path: "/v1/messages",
		LogicalTargetHost: "api.anthropic.com",
		RequestHeaders:    map[string][]string{"Content-Type": {"application/json"}},
		BodyRef:           &model.RequestBodyRef{ID: "abc123def4567890", Size: 88668, SHA256: "deadbeef"},
	}
	rows := unifiedActivityRows([]model.RequestRecord{withRef}, nil, nil, 5, false, false)

	page := webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		ActivityRows: []webRow{rows[0]},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()

	// (a) Exactly ONE ccwrap-bootstrap node.
	if n := strings.Count(html, `<script id="ccwrap-bootstrap"`); n != 1 {
		t.Fatalf("want exactly 1 ccwrap-bootstrap script node, got %d", n)
	}
	// (b) The TOTAL count of "<script" occurrences is unchanged: the
	// page carries exactly the bootstrap JSON node + the single inline
	// mirror script = 2. The body-drawer must add NONE.
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("body-drawer must add no <script>; want 2 total (bootstrap json + inline mirror), got %d", n)
	}
	// And the inline mirror must still be a single bare <script>…</script>
	// that parses under node --check (same mechanism as the canonical
	// node-check test, TestRenderedSessionPageScriptParses).
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	if _, err := exec.LookPath("node"); err == nil {
		cmd := exec.Command("node", "--check", "-")
		cmd.Stdin = strings.NewReader(m[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inline mirror script must still parse: %v\n%s", err, out)
		}
	}
	// (c) With a BodyRef the rendered HTML carries a collapsed
	// body-drawer whose data-reqid is the hex id (the id, NOT bytes).
	// The drawer element appears exactly once in this single-row page.
	// The body sub-accordion is nested under the row's reqinspect as
	// `<details class="req-sub body-drawer">` (data-reqid / hex-id /
	// "exactly once" semantics).
	wantEl := `<details class="req-sub body-drawer" data-reqid="abc123def4567890">`
	if !strings.Contains(html, wantEl) {
		t.Fatalf("row with BodyRef must render a body-drawer with data-reqid; html=%s", html)
	}
	if n := strings.Count(html, `<details class="req-sub body-drawer"`); n != 1 {
		t.Fatalf("want exactly 1 body-drawer element for a 1-row page, got %d", n)
	}
	if strings.Contains(html, "88668") || strings.Contains(html, "deadbeef") {
		t.Fatalf("body bytes/metadata must NOT be inlined into the page (C4 fetches lazily)")
	}

	// Without a BodyRef there must be no rendered body-drawer ELEMENT.
	// NOTE: assert on the element, not the bare token — ".body-drawer"
	// is also a static CSS class name in the always-present <style>
	// block (mirrors how the header-inspector tests scope to the
	// rendered element, not the stylesheet token). The element string
	// `<details class="req-sub body-drawer"` likewise never appears in
	// the <style> block, so the negative element-scoped assertion holds.
	noref := unifiedActivityRows([]model.RequestRecord{{Method: "GET", Path: "/health"}}, nil, nil, 5, false, false)
	if noref[0].BodyRefID != "" {
		t.Fatalf("no-BodyRef row must not carry BodyRefID, got %q", noref[0].BodyRefID)
	}
	page2 := webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		ActivityRows: []webRow{noref[0]},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	var buf2 bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf2}, page2)
	if strings.Contains(buf2.String(), `<details class="req-sub body-drawer"`) {
		t.Fatalf("row without BodyRef must NOT render a body-drawer element")
	}
}

// TestInlineScriptRendersBodyOnDrawerToggle locks the lazy-fetch
// contract: the structured body renderer lives ENTIRELY inside the
// single existing inline mirror <script> (no new <script>), still parses
// under node --check (same mechanism as the canonical node-check test
// TestRenderedSessionPageScriptParses), and the script SOURCE carries
// the lazy-fetch wiring (toggle-gated, /recent/body?id= at click-time,
// once-per-drawer guard) plus the projection shape (anatomy, system
// cache chip, tools literal schema, messages arbitrary blocks, Raw
// hatch). We assert the script SOURCE, never the runtime body HTML.
func TestInlineScriptRendersBodyOnDrawerToggle(t *testing.T) {
	page := webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()

	// (a) Single-script contract unchanged: exactly one ccwrap-bootstrap
	// JSON node + one bare inline mirror <script> = 2 total "<script".
	if n := strings.Count(html, `<script id="ccwrap-bootstrap"`); n != 1 {
		t.Fatalf("want exactly 1 ccwrap-bootstrap script node, got %d", n)
	}
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("C4 renderer must add no <script>; want 2 total (bootstrap json + inline mirror), got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// (b) The inline mirror still parses under node --check (same
	// mechanism + same skip behavior as TestRenderedSessionPageScriptParses).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = strings.NewReader(js)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inline mirror script with C4 renderer must still parse: %v\n%s", err, out)
	}

	// (c) Lazy-fetch wiring: a delegated toggle listener gated on the
	// body-drawer class, fetching /recent/body?id= at click-time only,
	// with a once-per-drawer guard (data-loaded), and the eviction text
	// on error/404.
	mustContainAllWeb(t, js,
		"addEventListener('toggle'",
		"body-drawer",
		"'/recent/body?id='",
		"data-loaded",
		"body-panel",
		"body not retained",
	)
	// The fetch must be guarded so it runs once per drawer (the
	// data-loaded sentinel is read before it is set).
	gi := strings.Index(js, "getAttribute('data-loaded')")
	si := strings.Index(js, "setAttribute('data-loaded'")
	if gi < 0 || si < 0 || gi > si {
		t.Fatalf("C4 must guard the fetch once-per-drawer (read data-loaded before setting it); got read=%d set=%d", gi, si)
	}

	// (d) Projection shape present in the renderer source: anatomy
	// bar, system with a cache_control chip, tools with the LITERAL
	// input_schema (no synthesized signature), messages over arbitrary
	// content blocks, and the Raw JSON escape hatch.
	//
	// The cache chip is `.bv-chip`, and the `.bv-block` rail carries the
	// `.cc`/`.ccg` cache modifier (cache rail on system AND messages); the
	// assertions pin `bv-chip` and the rail (`bv-block` + the
	// `'ccg' : 'cc'` modifier). The cache_control projection contract is
	// unchanged.
	mustContainAllWeb(t, js,
		"renderBodyView",
		"body-anatomy",
		"doc.system",
		"cache_control",
		`'bv-chip'`,
		`'bv-block'`,
		`'ccg' : 'cc'`,
		"doc.tools",
		"input_schema",
		"doc.messages",
		"Raw JSON",
	)
	// Faithfulness guard: textContent only — the renderer must never
	// route fetched body bytes through innerHTML (XSS).
	if strings.Contains(js, "renderBodyView") {
		rs := strings.Index(js, "function renderBodyView")
		if rs >= 0 {
			rest := js[rs:]
			end := strings.Index(rest[1:], "\n  function ")
			if end < 0 {
				end = len(rest) - 1
			}
			if strings.Contains(rest[:end+1], ".innerHTML") {
				t.Fatalf("renderBodyView must use textContent only, never innerHTML (XSS)")
			}
		}
	}
}

// TestLiveMirrorReconstructsBodyDrawer locks the fix: the body-drawer
// affordance must be reconstructed for
// LIVE SSE-streamed request rows, not only on server-side first paint —
// reaching PARITY with the header drawer, which the live path already
// rebuilds (headerPanelEl). The suite asserts live behavior the same way
// TestLiveScriptUsesBootstrapDenyList does: extract the single inline
// mirror <script>, parse it under node --check (same mechanism + skip
// behavior as TestRenderedSessionPageScriptParses), then assert the
// script SOURCE wiring (no headless DOM). We mirror that technique here
// for the body case, exactly as the header-drawer live test does.
func TestLiveMirrorReconstructsBodyDrawer(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	html := buf.String()

	// Single-script contract unchanged: exactly one ccwrap-bootstrap JSON
	// node + one bare inline mirror <script> = 2 total "<script". The
	// live body-drawer reconstruction must add NO new <script>.
	if n := strings.Count(html, `<script id="ccwrap-bootstrap"`); n != 1 {
		t.Fatalf("want exactly 1 ccwrap-bootstrap script node, got %d", n)
	}
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("live body-drawer must add no <script>; want 2 total (bootstrap json + inline mirror), got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// The inline mirror still parses under node --check (same mechanism +
	// same skip behavior as TestRenderedSessionPageScriptParses).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = strings.NewReader(js)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inline mirror script with live body-drawer must still parse: %v\n%s", err, out)
	}

	// (a) activityRowData's request branch must copy the SSE record's
	// body_ref id onto the row object — gated to request rows exactly
	// like the header fields (headerAnnotate). Scope the assertion to
	// the activityRowData body, same slicing idiom as
	// TestLiveScriptUsesBootstrapDenyList.
	aStart := strings.Index(js, "function activityRowData")
	if aStart < 0 {
		t.Fatalf("activityRowData not found in live script")
	}
	aRest := js[aStart+len("function activityRowData"):]
	aEnd := strings.Index(aRest, "\n  function ") // next top-level helper
	if aEnd < 0 {
		aEnd = len(aRest)
	}
	aBody := aRest[:aEnd]
	if !strings.Contains(aBody, "body_ref") || !strings.Contains(aBody, "bodyRefId") {
		t.Fatalf("activityRowData request branch must copy rec.body_ref onto row.bodyRefId (live parity with the server template's BodyRefID)")
	}

	// (b) A bodyDrawerEl helper must build the SAME element the Go
	// template produces, via createElement/textContent (no innerHTML),
	// with data-reqid set through setAttribute (hex id, still not
	// interpolated into markup) — mirroring headerPanelEl's idiom.
	bStart := strings.Index(js, "function bodyDrawerEl")
	if bStart < 0 {
		t.Fatalf("bodyDrawerEl helper not found (live path must reconstruct the body-drawer, parity with headerPanelEl)")
	}
	bRest := js[bStart:]
	bEnd := strings.Index(bRest[1:], "\n  function ")
	if bEnd < 0 {
		bEnd = len(bRest) - 1
	}
	bBody := bRest[:bEnd+1]
	mustContainAllWeb(t, bBody,
		"row.bodyRefId",
		"createElement('details')",
		"req-sub body-drawer",
		"setAttribute('data-reqid'",
		"request body",
		"sub-body body-panel",
		"full request body — loading…",
	)
	if strings.Contains(bBody, ".innerHTML") {
		t.Fatalf("bodyDrawerEl must use textContent/setAttribute only, never innerHTML (mirrors headerPanelEl)")
	}

	// The substring-based trim-invariant arms are intentionally NOT here: a
	// substring/source-text guard structurally CANNOT catch the recurring
	// body-drawer-orphan class, because "orphan" is a runtime-DOM-shape
	// property and source-text reverts can preserve every asserted
	// substring while still orphaning. The trim-invariant guarantee instead
	// lives entirely in the behavioral TestPrependRowOrphanInvariantBehavioral
	// (it runs the real makeRowEl/prependRow + B′ self-heal against the
	// vendored DOM shim and asserts the pre-heal removal log). Blocks (a)
	// and (b) above remain the source-level checks for this test.
}

func TestRecordClassAndPartition(t *testing.T) {
	cases := []struct {
		rec  model.RequestRecord
		want string
	}{
		{model.RequestRecord{Method: "POST", Path: "/v1/messages"}, "forwarded-api"},
		{model.RequestRecord{Method: "GET", Path: "/v1/models"}, "forwarded-api"},
		{model.RequestRecord{Method: "POST", Path: "/api/event_logging/v2/batch", Synthetic: true}, "synthetic"},
		{model.RequestRecord{Method: http.MethodConnect, Path: "x:443"}, "tunnel"},
		{model.RequestRecord{Method: http.MethodConnect, Synthetic: true}, "synthetic"}, // synthetic wins over CONNECT
		// Datadog CONNECT (Claude Code telemetry batches every 15s / per 100
		// events) goes into its own class so it doesn't dominate the Tunnel
		// chip — verified in claude-code/src/services/analytics/datadog.ts:13.
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "http-intake.logs.us5.datadoghq.com"}, "telemetry"},
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "http-intake.logs.datadoghq.eu"}, "telemetry"},
		// Case-insensitive suffix match.
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "HTTP-INTAKE.LOGS.US5.DATADOGHQ.COM"}, "telemetry"},
		// Allowlisted telemetry host with NO datadog suffix (Sentry error
		// ingest) must ALSO classify as 'telemetry' so its captured bodies
		// render the telemetry drawer — it's on isTelemetryCaptureHost (the
		// MITM gate) but invisible to the suffix-only isTelemetryHost.
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "anthropic.sentry.io"}, "telemetry"},
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "ANTHROPIC.SENTRY.IO"}, "telemetry"},
		// Captured telemetry rows are recorded with the INNER request method
		// (POST/GET), NOT CONNECT — the transparent MITM terminates TLS and
		// records the decrypted request. These MUST classify as 'telemetry' by
		// exact-host, or the captured request/response bodies render as a
		// forwarded-api row (wrong drawer, response body hidden). Regression
		// guard for the class bug found by live-testing the merged feature.
		{model.RequestRecord{Method: "POST", Path: "/api/v2/logs", LogicalTargetHost: "http-intake.logs.us5.datadoghq.com"}, "telemetry"},
		{model.RequestRecord{Method: "POST", Path: "/api/1/store/", LogicalTargetHost: "anthropic.sentry.io"}, "telemetry"},
		{model.RequestRecord{Method: "GET", LogicalTargetHost: "http-intake.logs.us5.datadoghq.com"}, "telemetry"},
		// A genuine forwarded-api request (Anthropic API host) is unaffected.
		{model.RequestRecord{Method: "POST", Path: "/v1/messages", LogicalTargetHost: "api.anthropic.com"}, "forwarded-api"},
		// Non-telemetry CONNECT stays as 'tunnel' (regression guard — the
		// telemetry classifier must NOT swallow genuine blind tunnels).
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "github.com"}, "tunnel"},
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "api.github.com"}, "tunnel"},
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "platform.claude.com"}, "tunnel"},
		// Defensive: empty LogicalTargetHost falls back to 'tunnel'.
		{model.RequestRecord{Method: http.MethodConnect}, "tunnel"},
		// Defensive: a substring that's not a host SUFFIX must not match
		// (e.g., a domain that merely contains 'datadoghq.com' as a label).
		{model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "not-datadoghq.com.example.org"}, "tunnel"},
	}
	for _, c := range cases {
		if got := recordClass(c.rec); got != c.want {
			t.Fatalf("recordClass(%+v)=%q want %q", c.rec, got, c.want)
		}
	}
	reqs := []model.RequestRecord{
		{Method: "POST", Path: "/v1/messages"},
		{Method: "POST", Path: "/api/event_logging", Synthetic: true},
		{Method: http.MethodConnect, Path: "h:443"},
		{Method: "POST", Path: "/v1/messages", Synthetic: true},
		{Method: "GET", Path: "/v1/x"},
		{Method: http.MethodConnect, LogicalTargetHost: "http-intake.logs.us5.datadoghq.com"},
	}
	cnt := map[string]int{}
	for _, r := range reqs {
		cnt[recordClass(r)]++
	}
	if cnt["forwarded-api"]+cnt["synthetic"]+cnt["tunnel"]+cnt["telemetry"] != len(reqs) {
		t.Fatalf("partition not exhaustive: %v over %d", cnt, len(reqs))
	}
	for _, c := range []string{"forwarded-api", "synthetic", "tunnel", "telemetry"} {
		if cnt[c] == 0 {
			t.Fatalf("class %q never produced by the mixed fixture: %v", c, cnt)
		}
	}
}

// TestRenderedFilterBar_TelemetryChip locks the end-of-pipeline: when the
// page's Classes slice carries a Telemetry entry, the rendered filter-bar
// HTML emits the chip with the right data-filter token and visible count.
// Combined with TestRecordClassAndPartition (proves recordClass returns
// "telemetry" for Datadog) and proxy.go's literal slice entry, this pins
// the full Datadog-CONNECT → Telemetry chip pipeline.
func TestRenderedFilterBar_TelemetryChip(t *testing.T) {
	html := renderTestPage(t, webPageData{
		Title:         "x",
		LiveEnabled:   false,
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		BootstrapB64:  bootstrapB64(pageBootstrap{EventsURL: "/events"}),
		Classes: []webClassCount{
			{"all", "All", 7},
			{"forwarded-api", "Forwarded API", 2},
			{"synthetic", "Synthetic", 0},
			{"tunnel", "Tunnel", 1},
			{"telemetry", "Telemetry", 4},
			{"error", "Errors", 0},
			{"trace", "Trace", 0},
		},
	})
	mustContainAllWeb(t, html,
		`data-filter="telemetry"`,
		"Telemetry",
	)
}

func TestUnifiedActivityRowsClassAndDrawers(t *testing.T) {
	now := time.Now()
	reqs := []model.RequestRecord{
		{Timestamp: now.Add(-3 * time.Second), Method: "POST", Path: "/v1/messages",
			RequestHeaders: http.Header{"Anthropic-Version": {"2023-06-01"}},
			BodyRef:        &model.RequestBodyRef{ID: "abc123def4567890"}},
		{Timestamp: now.Add(-2 * time.Second), Method: "POST", Path: "/api/event_logging", Synthetic: true},
		{Timestamp: now.Add(-1 * time.Second), Method: http.MethodConnect, Path: "h:443"},
	}
	errs := []model.ErrorRecord{{Timestamp: now.Add(-4 * time.Second), Summary: "boom", ErrorClass: "x"}}
	tr := []model.TraceRecord{{Timestamp: now.Add(-5 * time.Second), Category: "mitm", Summary: "handshake"}}
	rows := unifiedActivityRows(reqs, errs, tr, 50, false, false)
	byClass := map[string]int{}
	var fapi *webRow
	for i := range rows {
		byClass[rows[i].Class]++
		if rows[i].Class == "forwarded-api" {
			fapi = &rows[i]
		}
	}
	if byClass["forwarded-api"] != 1 || byClass["synthetic"] != 1 || byClass["tunnel"] != 1 ||
		byClass["error"] != 1 || byClass["trace"] != 1 {
		t.Fatalf("class partition wrong: %v", byClass)
	}
	if byClass["forwarded-api"]+byClass["synthetic"]+byClass["tunnel"]+byClass["error"]+byClass["trace"] != len(rows) {
		t.Fatalf("not exhaustive: %v vs %d", byClass, len(rows))
	}
	if fapi == nil || len(fapi.HeaderGroups) == 0 || fapi.BodyRefID != "abc123def4567890" {
		t.Fatalf("forwarded-api row must carry header groups + BodyRefID: %+v", fapi)
	}
	// Synthetic/CONNECT rows must be non-expandable WITH an explicit
	// reason note. Non-forwarded-api rows have NO expandable drawer /
	// no body — but synthetic & tunnel rows MUST still carry their reason
	// note (an earlier `HeaderNote != ""` blanket clause here was a bug and
	// contradicted the sibling TestForwardedRowHeaderPanelRedacted, which
	// asserts the same note contract for the dedicated builder).
	// error/trace carry no note.
	for i := range rows {
		if rows[i].Class == "forwarded-api" {
			continue
		}
		if len(rows[i].HeaderGroups) != 0 || rows[i].BodyRefID != "" {
			t.Fatalf("non-forwarded-api row %+v must have NO expandable drawer / no body", rows[i])
		}
		switch rows[i].Class {
		case "synthetic":
			if rows[i].HeaderNote != "CCWRAP-generated, not Claude Code traffic" {
				t.Fatalf("synthetic row must carry its non-expandable reason note, got %q", rows[i].HeaderNote)
			}
		case "tunnel":
			if rows[i].HeaderNote != "encrypted tunnel — not intercepted; no headers visible" {
				t.Fatalf("tunnel row must carry its non-expandable reason note, got %q", rows[i].HeaderNote)
			}
		default: // error, trace
			if rows[i].HeaderNote != "" {
				t.Fatalf("%s row must have no header note, got %q", rows[i].Class, rows[i].HeaderNote)
			}
		}
	}
	if rows[0].Class != "tunnel" {
		t.Fatalf("expected newest-first (tunnel @ -1s first), got %q", rows[0].Class)
	}
}

// TestTelemetryRowBodyDrawer locks the telemetry-MITM body-drawer
// affordance: a telemetry-class CONNECT row that captured request +
// response bodies becomes EXPANDABLE (its own .row <details>) carrying two
// independent .body-drawer sub-details, each ALSO marked .tele-drawer
// (so the global toggle listener renders generic JSON, not the
// Anthropic-shaped body view). A telemetry row with NO captured bodies
// stays non-expandable (.row.nf, TelemetryDrawer==false). The body bytes /
// metadata are never inlined — only the hex ids (data-reqid) for lazy
// fetch.
func TestTelemetryRowBodyDrawer(t *testing.T) {
	// (1) Builder: a telemetry CONNECT with both body refs set must carry
	// TelemetryDrawer + the two ids, and NO HeaderGroups (no captured
	// headers on a tunnel).
	rec := model.RequestRecord{
		Method:            http.MethodConnect,
		LogicalTargetHost: "http-intake.logs.us5.datadoghq.com",
		BodyRef:           &model.RequestBodyRef{ID: "abc123def4567890", Size: 90909091, SHA256: "deadbeef"},
		ResponseBodyRef:   &model.RequestBodyRef{ID: "fed987cba6543210", Size: 80808081, SHA256: "feedface"},
	}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 5, false, false)
	if len(rows) == 0 {
		t.Fatalf("no rows")
	}
	r := rows[0]
	if r.Class != "telemetry" {
		t.Fatalf("row must classify as telemetry, got %q", r.Class)
	}
	if !r.TelemetryDrawer {
		t.Fatalf("telemetry row with body refs must set TelemetryDrawer, got %+v", r)
	}
	if r.BodyRefID != "abc123def4567890" || r.ResponseBodyRefID != "fed987cba6543210" {
		t.Fatalf("telemetry row must carry req+resp body ids, got BodyRefID=%q ResponseBodyRefID=%q", r.BodyRefID, r.ResponseBodyRefID)
	}
	if len(r.HeaderGroups) != 0 {
		t.Fatalf("telemetry row must have NO header groups (no captured headers on a tunnel), got %d", len(r.HeaderGroups))
	}

	// (2) Render: the page emits an EXPANDABLE telemetry row with two
	// .tele-drawer .body-drawer sub-details carrying the two hex data-reqids
	// (request + response). The bytes/metadata are never inlined.
	page := webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		ActivityRows: []webRow{r},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()

	if !strings.Contains(html, `<details class="row" role="listitem" data-forwarded="false" data-kind="request" data-class="telemetry"`) {
		t.Fatalf("telemetry row with bodies must render an expandable <details class=\"row\"> (data-class=telemetry); html=%s", html)
	}
	// Scope to the rendered ELEMENT opener (the bare token also appears as a
	// JS string literal inside the always-present inline mirror script and as
	// a CSS class in the <style> block).
	if n := strings.Count(html, `<details class="req-sub body-drawer tele-drawer"`); n != 2 {
		t.Fatalf("telemetry row must render exactly 2 tele-drawer sub-details (req+resp), got %d", n)
	}
	if !strings.Contains(html, `data-reqid="abc123def4567890"`) || !strings.Contains(html, `data-reqid="fed987cba6543210"`) {
		t.Fatalf("telemetry drawers must carry the req+resp hex data-reqids; html=%s", html)
	}
	if strings.Contains(html, "90909091") || strings.Contains(html, "deadbeef") || strings.Contains(html, "80808081") || strings.Contains(html, "feedface") {
		t.Fatalf("telemetry body bytes/metadata must NOT be inlined (fetched lazily)")
	}
	// Single-script contract unchanged.
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("telemetry drawer must add no <script>; want 2 total, got %d", n)
	}

	// (3) A telemetry CONNECT with NO captured bodies stays NON-expandable
	// (.row.nf, TelemetryDrawer==false, no drawer element).
	noBody := model.RequestRecord{Method: http.MethodConnect, LogicalTargetHost: "http-intake.logs.us5.datadoghq.com"}
	nrows := unifiedActivityRows([]model.RequestRecord{noBody}, nil, nil, 5, false, false)
	if len(nrows) == 0 {
		t.Fatalf("no rows")
	}
	if nrows[0].Class != "telemetry" || nrows[0].TelemetryDrawer {
		t.Fatalf("telemetry row with no bodies must NOT set TelemetryDrawer, got %+v", nrows[0])
	}
	if nrows[0].BodyRefID != "" || nrows[0].ResponseBodyRefID != "" {
		t.Fatalf("no-body telemetry row must carry no ids, got %+v", nrows[0])
	}
	page2 := webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		ActivityRows: []webRow{nrows[0]},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	var buf2 bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf2}, page2)
	h2 := buf2.String()
	// Scope to the rendered ELEMENT (the bare "tele-drawer" token is always
	// present in the inline script + <style> block).
	if strings.Contains(h2, `<details class="req-sub body-drawer tele-drawer"`) {
		t.Fatalf("no-body telemetry row must NOT render a tele-drawer element")
	}
	if !strings.Contains(h2, `<div class="row nf"`) {
		t.Fatalf("no-body telemetry row must stay non-expandable (.row.nf); html=%s", h2)
	}
}

// TestInlineScriptTelemetryJsonView locks the LIVE telemetry-drawer +
// generic JSON renderer in the single inline mirror <script>: it defines
// renderJsonView / fetchJsonInto / teleBodyDrawerEl, activityRowData reads
// rec.response_body_ref, the global toggle listener routes .tele-drawer to
// renderJsonView (NOT the Anthropic-shaped renderBodyView), and the JSON
// renderer is textContent-only (third-party data — no innerHTML). We
// assert the script SOURCE, never runtime HTML (mirrors the body-drawer
// live tests).
func TestInlineScriptTelemetryJsonView(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	html := buf.String()
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("telemetry JSON view must add no <script>; want 2 total, got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// (a) The three new helpers exist.
	mustContainAllWeb(t, js,
		"function renderJsonView",
		"function fetchJsonInto",
		"function teleBodyDrawerEl",
	)

	// (b) activityRowData reads rec.response_body_ref (live parity with the
	// server template's ResponseBodyRefID), gated to the telemetry branch.
	aBody := scriptFnBody(t, js, "activityRowData")
	mustContainAllWeb(t, aBody, "response_body_ref", "responseBodyRefId", "telemetryDrawer")

	// (c) The toggle listener routes .tele-drawer to renderJsonView (via
	// fetchJsonInto) BEFORE the dual-view/forwarded branch, and NOT to
	// renderBodyView.
	tBody := scriptFnBody(t, js, "renderJsonView")
	if strings.Contains(tBody, "renderBodyView") {
		t.Fatalf("renderJsonView must not delegate to the Anthropic-shaped renderBodyView")
	}
	// The toggle listener must dispatch tele-drawer to the JSON fetcher.
	li := strings.Index(js, "addEventListener('toggle'")
	if li < 0 {
		t.Fatalf("toggle listener not found")
	}
	lRest := js[li:]
	lEnd := strings.Index(lRest, "}, true);")
	if lEnd < 0 {
		t.Fatalf("toggle listener close not found")
	}
	lBody := lRest[:lEnd]
	mustContainAllWeb(t, lBody, "tele-drawer", "fetchJsonInto")

	// (d) teleBodyDrawerEl builds a .body-drawer.tele-drawer via
	// createElement/textContent/setAttribute (no innerHTML).
	teBody := scriptFnBody(t, js, "teleBodyDrawerEl")
	mustContainAllWeb(t, teBody,
		"req-sub body-drawer tele-drawer",
		"setAttribute('data-reqid'",
		"sub-body body-panel",
	)
	if strings.Contains(teBody, ".innerHTML") {
		t.Fatalf("teleBodyDrawerEl must use textContent/setAttribute only, never innerHTML")
	}

	// (e) renderJsonView is textContent-only (third-party telemetry data).
	if strings.Contains(tBody, ".innerHTML") {
		t.Fatalf("renderJsonView must use textContent only, never innerHTML (third-party data)")
	}
	if !strings.Contains(tBody, "textContent") {
		t.Fatalf("renderJsonView must render via textContent")
	}

	// (f) fetchJsonInto fetches /recent/body?id= and routes to renderJsonView.
	fBody := scriptFnBody(t, js, "fetchJsonInto")
	mustContainAllWeb(t, fBody, "'/recent/body?id='", "renderJsonView")
}

// TestUnifiedActivityRowsSpec52AndHeaderNoteParity is the missing DIRECT
// contract test for the production builder unifiedActivityRows. The header
// contract tests elsewhere historically asserted against the four
// per-section row builders that were since collapsed into (and removed
// in favor of) unifiedActivityRows, so two regressions in
// unifiedActivityRows went unguarded:
//
//   - The error row's suggested action must always surface — never hidden
//     behind a present UpstreamHost. The contract is
//     joinParts(UpstreamHost, SuggestedAction); unifiedActivityRows used
//     fallback(UpstreamHost, SuggestedAction), silently dropping
//     SuggestedAction.
//   - Synthetic/CONNECT rows are non-expandable with an explicit reason.
//     Synthetic and Tunnel rows must carry their reason NOTE while still
//     having NO expandable drawer / no body. The contract is that the note
//     is set for every request via headerAnnotation; unifiedActivityRows
//     only set it for forwarded-api.
//
// Subtests so each clause is independently observable. Sibling of
// TestForwardedRowHeaderPanelRedacted (which asserts the equivalent note
// contract via unifiedActivityRows) and TestUnifiedActivityRowsClassAndDrawers
// — same real model.* construction + webRow assertions, no invented helpers.
func TestUnifiedActivityRowsSpec52AndHeaderNoteParity(t *testing.T) {
	t.Run("spec_5_2_suggested_action_always_surfaces", func(t *testing.T) {
		// ErrorClass/Severity intentionally empty so the Label half is
		// not at issue; the regression is specifically that
		// SuggestedAction is dropped when UpstreamHost is present.
		errs := []model.ErrorRecord{{UpstreamHost: "api.x", SuggestedAction: "retry without proxy"}}
		rows := unifiedActivityRows(nil, errs, nil, 50, false, false)
		if len(rows) != 1 {
			t.Fatalf("want 1 error row, got %d", len(rows))
		}
		if rows[0].Class != "error" || rows[0].Kind != "error" {
			t.Fatalf("want error class/kind, got class=%q kind=%q", rows[0].Class, rows[0].Kind)
		}
		if !strings.Contains(rows[0].Right, "retry without proxy") {
			t.Fatalf("suggested action must always surface, never hidden behind UpstreamHost; Right=%q", rows[0].Right)
		}
		if !strings.Contains(rows[0].Right, "api.x") {
			t.Fatalf("UpstreamHost context must be preserved alongside the action; Right=%q", rows[0].Right)
		}
	})

	t.Run("spec_5_synthetic_reason_note_no_drawer", func(t *testing.T) {
		reqs := []model.RequestRecord{{Method: "GET", Path: "/api/event_logging", Synthetic: true}}
		rows := unifiedActivityRows(reqs, nil, nil, 50, false, false)
		if len(rows) != 1 {
			t.Fatalf("want 1 request row, got %d", len(rows))
		}
		if rows[0].Class != "synthetic" {
			t.Fatalf("want synthetic class, got %q", rows[0].Class)
		}
		if rows[0].HeaderNote != "CCWRAP-generated, not Claude Code traffic" {
			t.Fatalf("synthetic row must carry its non-expandable reason note, got %q", rows[0].HeaderNote)
		}
		if len(rows[0].HeaderGroups) != 0 || rows[0].BodyRefID != "" {
			t.Fatalf("synthetic row must have NO expandable drawer / no body: %+v", rows[0])
		}
	})

	t.Run("spec_5_tunnel_reason_note_no_drawer", func(t *testing.T) {
		reqs := []model.RequestRecord{{Method: http.MethodConnect, Path: "h:443"}}
		rows := unifiedActivityRows(reqs, nil, nil, 50, false, false)
		if len(rows) != 1 {
			t.Fatalf("want 1 request row, got %d", len(rows))
		}
		if rows[0].Class != "tunnel" {
			t.Fatalf("want tunnel class, got %q", rows[0].Class)
		}
		if rows[0].HeaderNote != "encrypted tunnel — not intercepted; no headers visible" {
			t.Fatalf("tunnel row must carry its non-expandable reason note, got %q", rows[0].HeaderNote)
		}
		if len(rows[0].HeaderGroups) != 0 || rows[0].BodyRefID != "" {
			t.Fatalf("tunnel row must have NO expandable drawer / no body: %+v", rows[0])
		}
	})

	t.Run("forwarded_api_drawer_unchanged", func(t *testing.T) {
		reqs := []model.RequestRecord{{
			Method: "POST", Path: "/v1/messages",
			RequestHeaders: http.Header{"Anthropic-Version": {"2023-06-01"}},
			BodyRef:        &model.RequestBodyRef{ID: "abc123def4567890"},
		}}
		rows := unifiedActivityRows(reqs, nil, nil, 50, false, false)
		if len(rows) != 1 {
			t.Fatalf("want 1 request row, got %d", len(rows))
		}
		if rows[0].Class != "forwarded-api" {
			t.Fatalf("want forwarded-api class, got %q", rows[0].Class)
		}
		if len(rows[0].HeaderGroups) == 0 {
			t.Fatalf("forwarded-api row with captured headers must have HeaderGroups: %+v", rows[0])
		}
		// headerAnnotation returns "" when headers are present — the
		// reason note is mutually exclusive with the expandable drawer.
		if rows[0].HeaderNote != "" {
			t.Fatalf("forwarded-api row with headers must have an empty HeaderNote (drawer, not reason), got %q", rows[0].HeaderNote)
		}
		if rows[0].BodyRefID != "abc123def4567890" {
			t.Fatalf("forwarded-api row must expose its BodyRefID for lazy fetch, got %q", rows[0].BodyRefID)
		}
	})
}

// TestHandleInfoPageSingleActivityNoSections is the wiring contract for
// the info page: a mixed record set must (1) feed the single Activity list
// via unifiedActivityRows (newest-first, every class in ONE list — no
// per-class sections), and (2) the embedded ccwrap-bootstrap must carry
// classifiedRecord requests so the live JS classifies identically to first
// paint. This test asserts the bootstrap value the way handleInfoPage
// computes it — bootstrapB64(pageBootstrap{...}) — rather than the full
// HTML. bootstrapB64 + base64-decode mirror TestBootstrapEmbedsDenyList.
func TestHandleInfoPageSingleActivityNoSections(t *testing.T) {
	now := time.Now()
	requests := []model.RequestRecord{
		{Timestamp: now.Add(-4 * time.Second), Method: http.MethodPost, Path: "/v1/messages",
			RequestHeaders: http.Header{"Anthropic-Version": {"2023-06-01"}}},
		{Timestamp: now.Add(-3 * time.Second), Method: http.MethodPost, Path: "/api/event_logging/v2/batch", Synthetic: true},
		{Timestamp: now.Add(-2 * time.Second), Method: http.MethodConnect, Path: "h:443"},
	}
	errs := []model.ErrorRecord{{Timestamp: now.Add(-5 * time.Second), Summary: "boom", ErrorClass: "x"}}
	tr := []model.TraceRecord{{Timestamp: now.Add(-1 * time.Second), Category: "mitm", Summary: "handshake"}}

	// (1) Single Activity list — exactly what handleInfoPage builds:
	// unifiedActivityRows over (requests, errors, trace, 50, false).
	const activityCap = 50
	activityRows := unifiedActivityRows(requests, errs, tr, activityCap, false, false)
	if len(activityRows) != len(requests)+len(errs)+len(tr) {
		t.Fatalf("unifiedActivityRows must feed ONE list with every record: got %d rows, want %d", len(activityRows), len(requests)+len(errs)+len(tr))
	}
	if activityRows[0].Class != "trace" { // trace @ -1s is most recent
		t.Fatalf("Activity list must be newest-first; row0.Class = %q, want trace", activityRows[0].Class)
	}
	byClass := map[string]int{}
	for i := range activityRows {
		byClass[activityRows[i].Class]++
	}
	for _, c := range []string{"forwarded-api", "synthetic", "tunnel", "error", "trace"} {
		if byClass[c] != 1 {
			t.Fatalf("class %q must appear exactly once in the single Activity list, got %v", c, byClass)
		}
	}

	// (2) Classified bootstrap — exactly what handleInfoPage builds:
	// each model.RequestRecord wrapped as classifiedRecord{Class,...}.
	bootRequests := make([]classifiedRecord, 0, len(requests))
	for _, rec := range requests {
		bootRequests = append(bootRequests, classifiedRecord{Class: recordClass(rec), RequestRecord: rec})
	}
	b64 := bootstrapB64(pageBootstrap{
		EventsURL:      "/events",
		Requests:       bootRequests,
		Errors:         errs,
		Trace:          tr,
		HeaderDenyList: ui.CredentialDenyList(),
	})
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("bootstrap b64 decode: %v", err)
	}
	var decoded struct {
		Requests []struct {
			Class  string `json:"class"`
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("bootstrap JSON: %v\n%s", err, raw)
	}
	if len(decoded.Requests) != len(requests) {
		t.Fatalf("bootstrap requests len = %d, want %d", len(decoded.Requests), len(requests))
	}
	// requests[0].class == recordClass; the embedded
	// model.RequestRecord is still flattened (method/path survive).
	if decoded.Requests[0].Class == "" || decoded.Requests[0].Class != recordClass(requests[0]) {
		t.Fatalf("bootstrap requests[0].class = %q, want %q", decoded.Requests[0].Class, recordClass(requests[0]))
	}
	if decoded.Requests[0].Method != http.MethodPost || !strings.Contains(decoded.Requests[0].Path, "/v1/messages") {
		t.Fatalf("bootstrap requests[0] lost embedded record fields: %+v", decoded.Requests[0])
	}
	for i := range decoded.Requests {
		if decoded.Requests[i].Class != recordClass(requests[i]) {
			t.Fatalf("bootstrap requests[%d].class = %q, want %q (recordClass single source)", i, decoded.Requests[i].Class, recordClass(requests[i]))
		}
	}
}

// renderTestPage renders a webPageData to HTML via the real production
// renderWebPage path (same noopResponseWriter buffer mechanism every
// other test in this file uses) and returns the rendered markup.
func renderTestPage(t *testing.T, page webPageData) string {
	t.Helper()
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	return buf.String()
}

func TestActivityFilterBarRendered(t *testing.T) {
	// LiveEnabled is false so the {{if .LiveEnabled}} inline-mirror
	// <script> block is NOT emitted: the template surface (Activity
	// section, filter bar, rows, body-drawer, config-panel) is entirely
	// OUTSIDE that block, so every `want` token still renders. The only
	// residual source of the removed-section string literals
	// (data-section="Errors", id="sections-stack", id="diagnostics-panel")
	// is the inline script, so the `gone` assertions verify that the
	// TEMPLATE no longer emits any removed section.
	html := renderTestPage(t, webPageData{
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		// Live page: the body-drawer element asserted below is live-gated
		// (non-live pages render raw-JSON links instead).
		LiveEnabled: true,
		// Summary set so the {{if .Summary}} config-panel renders — the
		// `id="config-panel"` assertion below verifies it was preserved
		// and relocated after the Activity section.
		Summary: []webKV{{Label: "Route", Value: "api.example"}},
		Classes: []webClassCount{
			{"all", "All", 9}, {"forwarded-api", "Forwarded API", 3},
			{"synthetic", "Synthetic", 4}, {"tunnel", "Tunnel", 2},
			{"error", "Errors", 0}, {"trace", "Trace", 0},
		},
		ActivityRows: []webRow{
			// The body-drawer is nested inside the forwarded row's
			// `{{if .HeaderGroups}}` branch, so a forwarded-api row needs
			// captured headers for the `<details class="row">` + nested
			// `<details class="req-sub body-drawer">` to render. BodyRefID
			// is set so the data-reqid token is asserted byte-exact.
			{Time: "19:00:00", Label: "forwarded-api", Main: "POST /v1/messages", Class: "forwarded-api",
				HeaderGroups: []webHeaderGroup{{Name: "General", Rows: []webHeaderRow{{Name: "content-type", Value: "application/json"}}}},
				BodyRefID:    "abc123def4567890"},
			{Time: "19:00:01", Label: "synthetic", Main: "POST /api/event_logging", Class: "synthetic"},
		},
	})
	for _, want := range []string{
		`data-filter="forwarded-api"`, `Forwarded API`,
		`data-class="forwarded-api"`, `data-class="synthetic"`,
		// The body-drawer element is the nested
		// `<details class="req-sub body-drawer">`. data-reqid / data-class /
		// data-filter / config-panel filter-bar tokens are checked here too.
		`data-reqid="abc123def4567890"`, `class="req-sub body-drawer"`,
		`id="config-panel"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered page missing %q", want)
		}
	}
	for _, gone := range []string{`data-section="Requests"`, `data-section="Errors"`, `id="diagnostics-panel"`, `id="sections-stack"`} {
		if strings.Contains(html, gone) {
			t.Fatalf("removed section still present: %q", gone)
		}
	}
}

// TestInlineScriptFilterBarWiring locks the filter bar: a click handler
// (toggles `on` + `aria-selected`, re-applies the filter to every row),
// an on-load default-filter init keyed off the server DefaultClass
// (Forwarded API), and a #activity-more show-more that reveals more
// retained rows. Extraction + node --check reuse the exact mechanism of
// the canonical TestRenderedSessionPageScriptParses /
// TestInlineScriptUnifiedPatcherAndFilter (single bare <script> regexp;
// "node --check -" over its body; same node-absent skip behavior). The
// a11y half uses the real renderTestPage render path.
func TestInlineScriptFilterBarWiring(t *testing.T) {
	// Render via the real production path with Classes+DefaultClass set
	// AND LiveEnabled:true so BOTH the template filter bar and the inline
	// mirror <script> are emitted.
	html := renderTestPage(t, webPageData{
		Title: "x", LiveEnabled: true,
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		BootstrapB64:  bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
		Classes: []webClassCount{
			{"all", "All", 9}, {"forwarded-api", "Forwarded API", 3},
			{"synthetic", "Synthetic", 4}, {"tunnel", "Tunnel", 2},
			{"error", "Errors", 0}, {"trace", "Trace", 0},
		},
		ActivityRows: []webRow{
			{Time: "19:00:00", Label: "forwarded-api", Main: "POST /v1/messages", Class: "forwarded-api"},
			{Time: "19:00:01", Label: "synthetic", Main: "POST /api/event_logging", Class: "synthetic"},
		},
	})

	// (a11y, rendered template) The $.DefaultClass button carries
	// aria-selected="true"; every other class button carries
	// aria-selected="false". role="tab" is KEPT (minimal ARIA contract).
	mustContainAllWeb(t, html,
		`data-filter="forwarded-api" role="tab" aria-selected="true"`,
		`data-filter="synthetic" role="tab" aria-selected="false"`,
		`data-filter="all" role="tab" aria-selected="false"`,
	)

	// Exactly ONE bare <script> + the ccwrap-bootstrap JSON node:
	// count of "<script" == 2; no new <script> (single-script contract).
	if n := strings.Count(html, `<script id="ccwrap-bootstrap"`); n != 1 {
		t.Fatalf("want exactly 1 ccwrap-bootstrap script node, got %d", n)
	}
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("want 2 total <script (bootstrap json + inline mirror), got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// Extracted inline script still parses under node --check (same
	// mechanism + same node-absent skip as TestRenderedSessionPageScriptParses).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = strings.NewReader(js)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inline script should parse under node --check: %v\n%s", err, out)
	}

	// (1) A click handler bound to #activity-filter .filter-btn that
	// toggles the `on` class AND sets aria-selected (a11y completion),
	// then REBUILDS the Activity list filter-aware. An earlier approach
	// re-applied the filter to existing DOM rows via applyFilterAll()
	// (cap-then-filter, which buries /v1/messages); the corrected
	// contract is that the click triggers rebuildActivityFromState() so
	// the visible window is the newest LIMITS.activity rows of the
	// now-active class drawn from the FULL retained state. The
	// `on`+aria-selected lockstep is unchanged.
	if !strings.Contains(js, "getElementById('activity-filter')") {
		t.Fatalf("the filter bar must bind a click handler to #activity-filter")
	}
	mustContainAllWeb(t, js,
		".filter-btn",
		"classList.toggle('on'",
		"setAttribute('aria-selected'",
		"rebuildActivityFromState()",
	)

	// (2) The cap-then-filter hide path is GONE: no applyFilterAll /
	// applyFilterTo helpers, no fhide class toggling, no .row.fhide CSS
	// rule. Filtering is enforced by what is BUILT (filter-aware
	// capping), not by hiding — so the injection surface only shrinks.
	for _, gone := range []string{"applyFilterAll", "applyFilterTo", "fhide", ".row.fhide"} {
		if strings.Contains(html, gone) {
			t.Fatalf("filter-aware capping must remove the hide path; still present: %q", gone)
		}
	}

	// (3) On-load init: rebuildActivityFromState() runs once on first
	// paint (after connectLive, before the IIFE close) so the default-
	// class window is built from the full bootstrap state immediately
	// (it does refreshCounts + syncMore internally). An earlier approach
	// called applyFilterAll()+refreshCounts() to hide non-default rows;
	// this builds the correct default-class window instead (so
	// /v1/messages is never buried).
	tail := js[strings.LastIndex(js, "connectLive();"):]
	if !strings.Contains(tail, "rebuildActivityFromState();") {
		t.Fatalf("D3 must call rebuildActivityFromState() once on load (after connectLive)")
	}
	if strings.Contains(tail, "applyFilterAll(") {
		t.Fatalf("D3 on-load must NOT use the removed applyFilterAll hide path")
	}

	// (4) Show-more: a #activity-more handler that DEEPENS the active-
	// class window. It REUSES the canonical filter-aware
	// rebuildActivityFromState (merge+filter+sort+cap+rebuild) after
	// bumping LIMITS.activity, and a filter-aware syncMore() hides the
	// control when the active-class retained set has nothing more.
	mustContainAllWeb(t, js,
		"getElementById('activity-more')",
		"function syncMore(",
		"LIMITS.activity += 50",
		"rebuildActivityFromState()",
		"syncMore()",
	)
	if n := strings.Count(js, "function rebuildActivityFromState("); n != 1 {
		t.Fatalf("must REUSE (not redefine) rebuildActivityFromState; defs=%d", n)
	}
	if n := strings.Count(js, "function syncMore("); n != 1 {
		t.Fatalf("must REUSE (not redefine) syncMore; defs=%d", n)
	}

	// (5) No new innerHTML/eval/Function( introduced in the appended init
	// region (the sanctioned safe API is setAttribute('aria-selected',…)).
	if strings.Contains(tail, "innerHTML") || strings.Contains(tail, "eval(") || strings.Contains(tail, "Function(") {
		t.Fatalf("D2 appended init must not introduce innerHTML/eval/Function(")
	}

	// (6) Reused-unchanged anchors still present:
	// the global toggle listener + the body/header drawer builders.
	mustContainAllWeb(t, js,
		"document.addEventListener('toggle'",
		"renderBodyView", "headerPanelEl", "bodyDrawerEl",
	)
}

// scriptFnBody returns the source of the JS function `name` (its `{...}`
// brace-balanced body) extracted from the inline script `js`. It is a
// structural source-contract helper (Go-only, no new toolchain) used to
// assert ordering WITHIN a single function — e.g. that the active-class
// filter is applied BEFORE the LIMITS.activity slice in
// rebuildActivityFromState (cap-then-filter would slice first).
func scriptFnBody(t *testing.T, js, name string) string {
	t.Helper()
	sig := "function " + name + "("
	i := strings.Index(js, sig)
	if i < 0 {
		t.Fatalf("function %s( not found in inline script", name)
	}
	open := strings.IndexByte(js[i:], '{')
	if open < 0 {
		t.Fatalf("function %s: opening brace not found", name)
	}
	open += i
	depth := 0
	for j := open; j < len(js); j++ {
		switch js[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[open : j+1]
			}
		}
	}
	t.Fatalf("function %s: unbalanced braces", name)
	return ""
}

// TestActivityFilterAwareCappingNotBuried is the regression test for the
// cap-then-filter defect: the server first-paint and the client
// rebuildActivityFromState took the newest LIMITS.activity rows ACROSS ALL
// classes then merely .fhide-hid the non-matching ones, so a forwarded-api
// /v1/messages row with >50 newer noise rows fell out of the all-class
// slice and was absent from the DOM — /v1/messages can be buried, and
// filtering must operate over the full retained set, not just the visible
// 50. The fix is filter-aware capping: the visible window AND show-more
// operate over the active-filter subset of the full retained state. This
// test asserts the rewritten script encodes that contract. Extraction +
// node --check reuse the exact mechanism of the canonical
// TestRenderedSessionPageScriptParses / TestInlineScriptFilterBarWiring
// (single bare <script> regexp; "node --check -" over its body; same
// node-absent skip behavior).
func TestActivityFilterAwareCappingNotBuried(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		BootstrapB64:  bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
		Classes: []webClassCount{
			{"all", "All", 9}, {"forwarded-api", "Forwarded API", 3},
			{"synthetic", "Synthetic", 4}, {"tunnel", "Tunnel", 2},
			{"error", "Errors", 0}, {"trace", "Trace", 0},
		},
		ActivityRows: []webRow{
			{Time: "19:00:00", Label: "forwarded-api", Main: "POST /v1/messages", Class: "forwarded-api"},
		},
	})
	html := buf.String()

	// (1) Exactly ONE bare <script> + the ccwrap-bootstrap JSON node:
	// count of "<script" == 2; no new <script> (single-script contract).
	if n := strings.Count(html, `<script id="ccwrap-bootstrap"`); n != 1 {
		t.Fatalf("want exactly 1 ccwrap-bootstrap script node, got %d", n)
	}
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("want 2 total <script (bootstrap json + inline mirror), got %d", n)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	// (2) The cap-then-filter hide path is GONE everywhere: no
	// applyFilterTo / applyFilterAll JS, no fhide class toggling, no
	// .row.fhide CSS rule. Filtering is enforced by what is BUILT, not
	// by hiding — so the injection surface only shrinks.
	for _, gone := range []string{"applyFilterTo", "applyFilterAll", "fhide", ".row.fhide"} {
		if strings.Contains(html, gone) {
			t.Fatalf("filter-aware capping must remove the hide path; still present: %q", gone)
		}
	}

	// (3) rebuildActivityFromState is filter-aware: the active-class
	// filter (activeFilter()) is applied to the merged FULL set BEFORE
	// the LIMITS.activity slice. Cap-then-filter sliced first and never
	// referenced activeFilter() in this function (it used a post-rebuild
	// per-row applyFilterTo loop) — so both the presence AND the
	// before-slice ordering would fail under that approach.
	rb := scriptFnBody(t, js, "rebuildActivityFromState")
	af := strings.Index(rb, "activeFilter()")
	if af < 0 {
		t.Fatalf("rebuildActivityFromState must consult activeFilter() (filter-aware capping), not slice-then-hide")
	}
	sl := strings.Index(rb, "slice(0, LIMITS.activity)")
	if sl < 0 {
		t.Fatalf("rebuildActivityFromState must still cap to LIMITS.activity via slice(0, LIMITS.activity)")
	}
	if !(af < sl) {
		t.Fatalf("filter must be applied BEFORE the LIMITS.activity slice (filter-then-slice), not after (cap-then-filter): activeFilter()@%d slice@%d", af, sl)
	}
	// The post-rebuild per-row hide loop must be gone (the built list IS
	// already exactly the filtered window).
	if strings.Contains(rb, "querySelectorAll") {
		t.Fatalf("rebuildActivityFromState must not re-query+filter rows after building (the built list is the filtered window)")
	}

	// (4) patchActivity is counted-not-shown: it conditionally
	// inserts only when the row matches the active filter (or filter is
	// 'all'); a non-matching live row is counted (refreshCounts) but NOT
	// inserted as a visible row. Assert the conditional-insert branch on
	// activeFilter() exists and the unconditional insert + hide-after is
	// gone.
	pa := scriptFnBody(t, js, "patchActivity")
	if !strings.Contains(pa, "activeFilter()") {
		t.Fatalf("patchActivity must gate the visible insert on activeFilter() (counted-not-shown)")
	}
	if !strings.Contains(pa, "prependRow(") {
		t.Fatalf("patchActivity must still prependRow the matching live row (live-append path)")
	}
	if strings.Contains(pa, "applyFilterTo") {
		t.Fatalf("patchActivity must not hide-after-insert (no applyFilterTo)")
	}
	if !strings.Contains(pa, "refreshCounts()") {
		t.Fatalf("patchActivity must still refreshCounts() for non-matching (counted) rows")
	}

	// (5) Extracted inline script still parses under node --check (same
	// mechanism + same node-absent skip as TestRenderedSessionPageScriptParses).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = strings.NewReader(js)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inline script should parse under node --check: %v\n%s", err, out)
	}

	// (6) Reused-unchanged anchors still present:
	// the global toggle listener + the body/header drawer builders +
	// the prependRow builder itself (still exercised by the live-append
	// path). Note: html/template's script-context processing strips //
	// comments from rendered <script> bodies, so the byte-identical
	// guarantee for prependRow's trim INVARIANT is enforced by source
	// review + the TestLiveMirrorReconstructsBodyDrawer regression guard,
	// not a rendered-HTML grep.
	mustContainAllWeb(t, js,
		"document.addEventListener('toggle'",
		"renderBodyView", "headerPanelEl", "bodyDrawerEl",
		"function prependRow(",
	)
}

// sampleForwardedActivityPage returns a webPageData carrying one
// forwarded-api ActivityRows entry with non-empty HeaderGroups (one
// credential row whose Value is the redaction sentinel) and a
// non-empty BodyRefID. Field pattern copied from
// TestActivityFilterBarRendered / TestUnifiedActivityRowsClassAndDrawers
// (webRow + ui.HeaderGroup/ui.HeaderRow literals).
func sampleForwardedActivityPage(t *testing.T) webPageData {
	t.Helper()
	return webPageData{
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		// Body drawers are live-page affordances (the lazy-fetch listener
		// lives in the live script); non-live pages render raw-JSON links
		// instead, pinned by TestEndedPageHonestActivityControls.
		LiveEnabled: true,
		ActivityRows: []webRow{
			{Time: "19:00:00", Label: "forwarded-api", Main: "POST /v1/messages", Class: "forwarded-api",
				HeaderGroups: []webHeaderGroup{{Name: "Credentials", Rows: []webHeaderRow{{Name: "Authorization", Value: "‹redacted by ccwrap›", Redacted: true}}}},
				BodyRefID:    "abc123def4567890"},
		},
	}
}

func TestRealignCSSClassesPresent(t *testing.T) {
	html := renderTestPage(t, sampleForwardedActivityPage(t))
	mustContainAllWeb(t, html,
		"details.row>summary", ".rowchev", ".reqinspect",
		"details.req-sub", ".hdr-sum", ".hdr-redpill",
		".bv-config", ".bv-seclabel", ".bv-tool .tb", ".bv-dl",
		".bv-block.cc", ".bv-block.ccg")
}

// sampleSyntheticActivityPage is the non-forwarded sibling of
// sampleForwardedActivityPage: one ActivityRows webRow classed
// "synthetic" with the explicit non-expandable HeaderNote and NO
// HeaderGroups / NO BodyRefID — a plain non-expandable row.
func sampleSyntheticActivityPage(t *testing.T) webPageData {
	t.Helper()
	return webPageData{
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		ActivityRows: []webRow{
			{Time: "19:00:01", Label: "synthetic", Main: "GET /ccwrap/ping", Class: "synthetic",
				HeaderNote: "CCWRAP-generated, not Claude Code traffic"},
		},
	}
}

// sampleForwardedNoBodyPage mirrors sampleForwardedActivityPage exactly
// (one forwarded-api webRow with non-empty HeaderGroups ⇒ the
// `{{if .HeaderGroups}}` branch renders the `<details class="row">`
// disclosure + nested `request headers` sub-accordion) but drops the
// BodyRefID field, so the inner `{{if .BodyRefID}}` body sub-accordion
// is NOT emitted (a forwarded GET with no captured body shows headers but
// no body drawer). Same webRow / webHeaderGroup literal pattern as the
// existing sample* helpers.
func sampleForwardedNoBodyPage(t *testing.T) webPageData {
	t.Helper()
	return webPageData{
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		ActivityRows: []webRow{
			{Time: "19:00:00", Label: "forwarded-api", Main: "GET /v1/models", Class: "forwarded-api",
				HeaderGroups: []webHeaderGroup{{Name: "Credentials", Rows: []webHeaderRow{{Name: "Authorization", Value: "‹redacted by ccwrap›", Redacted: true}}}}},
		},
	}
}

// sampleTunnelActivityPage is the non-forwarded CONNECT/tunnel sibling
// of sampleSyntheticActivityPage: one ActivityRows webRow classed
// "tunnel" with the verbatim blind-tunnel HeaderNote and NO
// HeaderGroups / NO BodyRefID — a plain non-expandable row
// (renders `<div class="row nf">`, never `<details class="row">`).
func sampleTunnelActivityPage(t *testing.T) webPageData {
	t.Helper()
	return webPageData{
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		ActivityRows: []webRow{
			{Time: "19:00:02", Label: "tunnel", Main: "CONNECT api.anthropic.com:443", Class: "tunnel",
				HeaderNote: "encrypted tunnel — not intercepted; no headers visible"},
		},
	}
}

func TestForwardedRowIsDetailsDisclosure(t *testing.T) {
	html := renderTestPage(t, sampleForwardedActivityPage(t))
	mustContainAllWeb(t, html,
		`<details class="row"`, `<summary`, `class="rowchev"`,
		`<div class="reqinspect">`,
		`<details class="req-sub"><summary>request headers</summary>`,
		`<details class="req-sub body-drawer" data-reqid=`,
		`<summary>request body</summary>`)
	h2 := renderTestPage(t, sampleSyntheticActivityPage(t))
	mustContainAllWeb(t, h2, `<div class="row nf"`, `class="nf-note"`, "CCWRAP-generated, not Claude Code traffic")
	// Scope to rendered ELEMENTS, not bare tokens: ".body-drawer" is also
	// a static class name in the always-present <style> block (same
	// gotcha documented in TestRenderedPageStillSingleBootstrapScriptWithBodyDrawer);
	// the synthetic row must emit neither the row-level disclosure element
	// nor the body-drawer element.
	if strings.Contains(h2, `<details class="row"`) || strings.Contains(h2, `<details class="req-sub body-drawer"`) {
		t.Fatalf("synthetic row must be a plain non-expandable row")
	}
}

// liveScriptPage is the canonical LiveEnabled:true webPageData whose
// {{if .LiveEnabled}} block emits exactly the ccwrap-bootstrap JSON node +
// the single bare inline <script> mirror. Built to match the render page
// of TestPrependRowOrphanInvariantBehavioral (Title/LiveEnabled +
// BootstrapB64:bootstrapB64(pageBootstrap{EventsURL:"/events",
// HeaderDenyList:ui.CredentialDenyList()})). The sample*ActivityPage
// literals do NOT set LiveEnabled, so the inline script + the
// single-script sweep require this dedicated page (the renderBodyView
// source contract is about the static inline script and is independent
// of which Activity rows are present).
func liveScriptPage() webPageData {
	return webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
}

func TestRealignSpec8Structural(t *testing.T) {
	// extractInlineScript / nodeCheck plumbing filled inline from the
	// REAL idiom: extraction mirrors TestPrependRowOrphanInvariantBehavioral
	// — renderWebPage into a noopResponseWriter buffer over a
	// LiveEnabled:true page, then
	// regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch
	// (len==2 else Fatalf), js := m[1]. nodeCheck mirrors
	// TestHeaderPanelOptionAStructure — exec.LookPath("node") skip, then
	// `node --check -` over the body via
	// cmd.Stdin = strings.NewReader(js), Fatalf on CombinedOutput error.
	extractInlineScript := func() string {
		t.Helper()
		var buf bytes.Buffer
		renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, liveScriptPage())
		m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
		if len(m) != 2 {
			t.Fatalf("single bare <script> not found")
		}
		return m[1]
	}
	nodeCheck := func(js string) {
		t.Helper()
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("node not available")
		}
		cmd := exec.Command("node", "--check", "-")
		cmd.Stdin = strings.NewReader(js)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node --check failed: %v\n%s", err, out)
		}
	}

	// forwarded row: <details class="row"> only; nested headers always, body iff body_ref
	fwd := renderTestPage(t, sampleForwardedActivityPage(t))
	mustContainAllWeb(t, fwd, `<details class="row"`, `<details class="req-sub"><summary>request headers</summary>`,
		`<details class="req-sub body-drawer" data-reqid=`, `class="rowchev"`)
	// forwarded GET (no body_ref): headers sub-accordion present, body absent.
	// Scope the negative to the rendered body sub-accordion ELEMENT, not the
	// bare token "body-drawer": ".body-drawer" is ALSO a static class name in
	// the always-present <style> block (web.go `.body-drawer{…}`), so the
	// bare token is in EVERY rendered page regardless of body_ref — the same
	// gotcha documented in TestForwardedRowIsDetailsDisclosure and
	// TestRenderedPageStillSingleBootstrapScriptWithBodyDrawer.
	// `<details class="req-sub body-drawer"` is the body-drawer element
	// ({{if .BodyRefID}} branch); asserting its ABSENCE is the literal
	// stated intent ("no body_ref ⇒ no body sub-accordion") and is strictly
	// stronger than (never weaker than) the unsatisfiable bare-token check.
	nob := renderTestPage(t, sampleForwardedNoBodyPage(t))
	if strings.Contains(nob, `<details class="req-sub body-drawer"`) {
		t.Fatalf("no body_ref ⇒ no body sub-accordion")
	}
	mustContainAllWeb(t, nob, `request headers`)
	// synthetic & CONNECT non-expandable + exact reason notes; error/trace plain
	syn := renderTestPage(t, sampleSyntheticActivityPage(t))
	mustContainAllWeb(t, syn, `<div class="row nf"`, "CCWRAP-generated, not Claude Code traffic")
	con := renderTestPage(t, sampleTunnelActivityPage(t))
	mustContainAllWeb(t, con, "encrypted tunnel — not intercepted; no headers visible")
	for _, h := range []string{syn, con} {
		if strings.Contains(h, `<details class="row"`) {
			t.Fatalf("non-forwarded rows must not be <details>")
		}
	}
	// body view source contract
	js := extractInlineScript()
	nodeCheck(js)
	rb := scriptFnBody(t, js, "renderBodyView")
	mustContainAllWeb(t, rb, "bv-config", "bvSecLabel(", "bvToolRow(", "bvCC(b)", "bv-dl", "download")
	// single-script + no new innerHTML + deny-list single source (sweep).
	// Swept over the same LiveEnabled:true page the inline <script> is
	// parsed from (the only page that emits scripts; the literal sample*
	// pages set no LiveEnabled ⇒ a count of 0 there asserts nothing). This
	// is the same single-script contract as
	// TestActivityFilterAwareCappingNotBuried: strictly stronger than
	// counting on a script-less page, never weaker.
	var sweep bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &sweep}, liveScriptPage())
	if strings.Count(sweep.String(), "<script") != 2 {
		t.Fatalf("exactly one bare <script> + ccwrap-bootstrap node")
	}
}

// TestRowHeadAndNfNoteGridAlignment guards two grid-layout regressions the
// structural suite originally missed: the layout has a leading 16px chevron
// column, so the `.rows .row-head` / `.row.nf` grid is 5-track.
// (1) BOTH row-head sources — the Go first-paint
// `<div class="row-head">` AND the JS `ROW_HEAD_HTML` live-skeleton const —
// must emit 5 role="cell" cells (leading spacer + Time/Kind/Summary/Details),
// matching a forwarded row's 5-cell <summary>; a 4-cell row-head jams "Time"
// into the 16px chevron track. (2) `.nf-note` is the 6th child of the 5-col
// `.row.nf` grid, so it needs `grid-column:1/-1` or the non-forwarded reason
// note collapses into the 16px chevron column (one word per line).
func TestRowHeadAndNfNoteGridAlignment(t *testing.T) {
	page := renderTestPage(t, sampleForwardedActivityPage(t))
	// The table ARIA was retired for honest list semantics (UI/UX review
	// 方案A), so the 5-track contract is counted structurally now: column
	// <div>s in the row-head, cell <span>s in a row summary.
	countIn := func(re, hay, token, what string) int {
		m := regexp.MustCompile(re).FindStringSubmatch(hay)
		if m == nil {
			t.Fatalf("%s: pattern %q not found in rendered output", what, re)
		}
		return strings.Count(m[1], token)
	}
	// (1a) Go first-paint row-head: exactly 5 column divs.
	if n := countIn(`(?s)<div class="row-head"[^>]*>(.*?)</div></div>`, page, "<div", "Go row-head"); n != 5 {
		t.Fatalf("Go template .row-head has %d column divs; the realigned grid is 5-track — want 5 (leading chevron spacer + Time/Kind/Summary/Details)", n)
	}
	// (1b) a forwarded row's <summary> has 5 cell spans — the row-head MUST match.
	if n := countIn(`(?s)<details class="row"[^>]*><summary>(.*?)</summary>`, page, "<span", "forwarded row summary"); n != 5 {
		t.Fatalf("forwarded row <summary> has %d cell spans; want 5 (rowchev + 4) — row-head/data column misalignment", n)
	}
	// (1c) the JS ROW_HEAD_HTML live-skeleton const must ALSO be 5-cell
	// (the live Activity list's row-head is built from here, not the Go
	// one); the literal includes the container div, hence the -1.
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, liveScriptPage())
	if n := countIn(`ROW_HEAD_HTML\s*=\s*'([^']*)'`, buf.String(), "<div", "JS ROW_HEAD_HTML") - 1; n != 5 {
		t.Fatalf("JS ROW_HEAD_HTML has %d column divs; want 5 to match the 5-track grid + the Go row-head (else live rows misalign)", n)
	}
	// (2) .nf-note must span all tracks; the grid it spans is genuinely 5-track.
	mustContainAllWeb(t, page,
		`.nf-note{grid-column:1/-1`,
		`.rows .row-head{grid-template-columns:16px 92px 112px`,
		`.row.nf{grid-template-columns:16px 92px 112px`)
}

// TestBodyV2SystemOpenAndAnatomyChips guards two request-body behaviors:
// (1) the `system` sub-accordion is OPEN by default — and ONLY system
// (tools/messages/other-keys/Raw stay collapsed) — so expanding `request
// body` immediately surfaces the system blocks + cache rails instead of a
// wall of closed accordions; (2) the anatomy bar renders as a bordered
// mono chip strip with the % in accent, not a plain text run.
func TestBodyV2SystemOpenAndAnatomyChips(t *testing.T) {
	page := renderTestPage(t, sampleForwardedActivityPage(t))
	// Anatomy chip CSS (.anat → app tokens) + accent %.
	mustContainAllWeb(t, page,
		`.body-anatomy{display:flex`,
		`.body-anatomy span{background:var(--surface-2);border:1px solid var(--line)`,
		`.body-anatomy span b{color:var(--accent)`)
	// Body <pre> is a contained code box (dark bg, border, radius, pad,
	// max-height scroll, wrap) — not bare, horizontally-overflowing text.
	mustContainAllWeb(t, page,
		`.body-panel pre{`,
		`white-space:pre-wrap`, `max-height:240px`, `overflow:auto`,
		`border:1px solid var(--line)`, `border-radius:5px`, `padding:8px`)
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, liveScriptPage())
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]
	// bvAccordion opens iff its 3rd (open) arg is truthy.
	mustContainAllWeb(t, js, "function bvAccordion(title, childNodes, open)", "if (open) d.open")
	rb := scriptFnBody(t, js, "renderBodyView")
	// ONLY system opens by default: exactly one `, true)` in
	// renderBodyView, on the system call (tools/messages/other-keys/Raw
	// pass no open arg → stay collapsed).
	mustContainAllWeb(t, rb, "bvAccordion('system [' + doc.system.length + ']'", "}), true)")
	if n := strings.Count(rb, ", true)"); n != 1 {
		t.Fatalf("renderBodyView opens %d accordions by default; want exactly 1 (system only — tools/messages/other-keys/Raw must stay collapsed)", n)
	}
	// Anatomy builder: createElement <b> accent (no innerHTML), sorted by
	// descending byte-share and rendering a present-but-rounds-to-0 key as
	// "<1%" (not "0%", which is indistinguishable from absent). The earlier
	// fixed Object.keys-order `pct + '%'` assignment is gone.
	mustContainAllWeb(t, rb, "body-anatomy", "createElement('b')",
		".sort(function(a,b){ return b.seg - a.seg; })",
		"(e.pct===0 && e.seg>0) ? '<1%' : e.pct + '%'")
	if strings.Contains(rb, "pb.textContent = pct + ") {
		t.Fatalf("anatomy must be byte-share-sorted with <1%% precision; the fixed-order pct assignment must be gone")
	}
	// Section accordion summaries are BARE: the mockup's "— …" suffixes
	// were the prototype author's annotations, NOT production copy. Guard
	// they never ship.
	for _, proto := range []string{
		"— ordered blocks + cache rail", "— each tool collapsible",
		"— turn → blocks, mirrors system", "Raw JSON — exact spill bytes"} {
		if strings.Contains(rb, proto) {
			t.Fatalf("renderBodyView ships the mockup's prototype-annotation summary suffix %q — section summaries must be bare (system [N]/tools [N]/messages [N]/Raw JSON)", proto)
		}
	}
	// Fidelity (mockup parity): config + block monospace; section bodies
	// inset via .bv-acc; header rows a rigid 2-col mono grid; ALL body-view
	// <details> styled (not bare browser disclosures) like the mockup's
	// generic details/summary.
	mustContainAllWeb(t, page,
		`.bv-config{display:grid;grid-template-columns:max-content 1fr;gap:2px 16px;font-family:ui-monospace`,
		`.bv-block{border-left:3px solid var(--line);padding:4px 0 4px 10px;margin:7px 0;font-family:ui-monospace`,
		`.bv-acc{padding:7px 11px;border-top:1px solid var(--line)}`,
		`.hdr-row{display:grid;grid-template-columns:230px 1fr;gap:12px;font-family:ui-monospace`,
		`.body-panel details{border:1px solid var(--line);border-radius:6px;margin:6px 0;background:var(--surface-2)`,
		`.body-panel details>summary::before{content:"\25B8`)
	// tool rows must NOT have a bespoke box (mockup styles generic
	// details{} uniformly; .toolrow only adds the muted .tm). The
	// divergent details.bv-tool{…radius:5px;background:var(--surface)…}
	// rule is gone — guard it never returns.
	if strings.Contains(page, "details.bv-tool{") {
		t.Fatalf("tool rows must use the uniform .body-panel details look (mockup toolrow), not a bespoke details.bv-tool{…} box")
	}
	acc := scriptFnBody(t, js, "bvAccordion")
	mustContainAllWeb(t, acc, "b.className = 'bv-acc'", "b.appendChild(c)", "d.appendChild(b)")
	mustContainAllWeb(t, rb, "racc.className = 'bv-acc'", "racc.appendChild(rp)")
	// Tools row (mockup `toolrow` parity): summary = name + muted .tm
	// "‹toolBytes› B · ‹props› props" where the size is the WHOLE tool's
	// serialized length (tlen), a distinct metric — NOT the description
	// length (dlen) reused (which also feeds the nested
	// `description (‹dlen› chars)` — that was the duplicated source). The
	// input_schema caption is bare (the "(literal, pretty)"
	// implementation-rationale parenthetical is not user copy — same class
	// as the stripped prototype suffixes).
	mustContainAllWeb(t, page, `.bv-tool>summary .tm{color:var(--text-muted)}`)
	tr := scriptFnBody(t, js, "bvToolRow")
	mustContainAllWeb(t, tr,
		"tm.className='tm'",
		"tlen=JSON.stringify(t==null?{}:t).length",
		"tlen+' B · '+props.length+' props'",
		"'description ('+dlen+' chars)'",
		"sc.className='bh'", "sc.textContent='input_schema'")
	if strings.Contains(tr, "dlen+' B · ") {
		t.Fatalf("tool summary size must be the whole tool's serialized length (tlen), not the description length (dlen) reused — that duplicated `description (N chars)` from the same source")
	}
	if strings.Contains(tr, "(literal, pretty)") {
		t.Fatalf("input_schema caption must be bare `input_schema` — the (literal, pretty) implementation-rationale parenthetical is not user-facing copy")
	}
	// Messages (mockup parity): role line is "◆ ‹role›" styled accent
	// mono (.bv-role, mockup .role); bvRaw collapses raw text >500 chars
	// (mockup rawBlock), not >600.
	mustContainAllWeb(t, page, `.bv-role{color:var(--accent);font-family:ui-monospace`)
	mustContainAllWeb(t, scriptFnBody(t, js, "bvTurn"), "'◆ ' + (role || '')")
	rawFn := scriptFnBody(t, js, "bvRaw")
	mustContainAllWeb(t, rawFn, "String(text).length > 500")
	if strings.Contains(rawFn, "String(text).length > 600") {
		t.Fatalf("bvRaw must collapse raw text at >500 chars (mockup rawBlock), not >600")
	}
	// Headers (mockup parity): summary chips mono (mockup .hsum span) +
	// 4px 0 12px strip margin; group labels .06em / 14px 0 5px with
	// :first-of-type tightening (mockup .seclabel); redacted value neutral
	// muted (.hdr-v.redv, mockup .hv.redv). Full contiguous rule strings so
	// a regression to the earlier values (no mono / 0 0 9px / .04em /
	// 6px 0 2px / no :first-of-type / no .redv) fails RED.
	mustContainAllWeb(t, page,
		`.hdr-sum span{background:var(--surface);border:1px solid var(--line);border-radius:5px;padding:3px 8px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;color:var(--text-secondary)}`,
		`.hdr-sum{display:flex;gap:7px;flex-wrap:wrap;margin:4px 0 12px}`,
		`.hdr-group-name{font-size:10px;letter-spacing:.06em;text-transform:uppercase;color:var(--text-muted);margin:14px 0 5px}`,
		`.hdr-group:first-of-type .hdr-group-name{margin-top:4px}`,
		`.hdr-v.redv{color:var(--text-muted)}`)
	// Redacted value muted in BOTH render paths (first-paint/live parity):
	// the JS live mirror (headerPanelEl) must add `redv` for redacted rows,
	// not unconditionally className 'hdr-v mono'.
	mustContainAllWeb(t, scriptFnBody(t, js, "headerPanelEl"),
		"r.redacted ? 'hdr-v mono redv' : 'hdr-v mono'")
	// Section-header de-duplication: renderBodyView calls bvSecLabel
	// EXACTLY ONCE (the non-collapsible `config` scalar strip). Every
	// collapsible section uses its accordion <summary> as the sole
	// header — the redundant pre-accordion
	// bvSecLabel(system/tools/messages/k/raw) calls are gone (was 6 calls,
	// now 1).
	if n := strings.Count(rb, "bvSecLabel("); n != 1 {
		t.Fatalf("renderBodyView must call bvSecLabel exactly once (config only; a collapsible section's <summary> is its sole header — no duplicate label), got %d", n)
	}
	mustContainAllWeb(t, rb, "panel.appendChild(bvSecLabel('config'))")
	for _, gone := range []string{
		"bvSecLabel('system [", "bvSecLabel('tools [",
		"bvSecLabel('messages [", "bvSecLabel(k)", "bvSecLabel('raw')"} {
		if strings.Contains(rb, gone) {
			t.Fatalf("redundant pre-accordion label %q must be removed (the accordion <summary> is the section header)", gone)
		}
	}
	// Header value wrap: .hdr-v uses overflow-wrap:anywhere (wrap at
	// natural break points first; break inside a token only when a single
	// token overflows), NOT word-break:break-all (fragmented every long
	// value mid-token).
	mustContainAllWeb(t, page, ".hdr-v{color:var(--text);overflow-wrap:anywhere}")
	if strings.Contains(page, ".hdr-v{color:var(--text);word-break:break-all}") {
		t.Fatalf(".hdr-v must use overflow-wrap:anywhere, not the mid-token-fragmenting word-break:break-all (scoped to .hdr-v; .cv unaffected)")
	}
}

func TestHeaderPanelOptionAStructure(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	page := webPageData{Title: "t", Heading: "h", Subtitle: "s", LiveEnabled: true, BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}), ActivityTitle: "Activity"}
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(buf.Bytes())
	if len(m) != 2 {
		t.Fatalf("script tag not found")
	}
	js := string(m[1])
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = bytes.NewReader(m[1])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
	body := scriptFnBody(t, js, "headerPanelEl")
	mustContainAllWeb(t, body, "req-sub", "hdr-sum", "request headers", "groups", "redacted", "hdr-group-name", "hdr-redpill")
	mustContainAllWeb(t, js, "‹redacted by ccwrap›", "HEADER_DENY", "boot.header_deny_list")
}

func TestFirstPaintHeadersOptionAParity(t *testing.T) {
	// The sampleForwardedActivityPage helper is a hand-built webPageData
	// LITERAL whose webRow sets HeaderGroups directly and never flows
	// through the production builder, so its HeaderSummary is the zero
	// value forever — that path cannot exercise the builder code under the
	// builder-site-only contract (renderWebPage is a pure template Execute
	// with no normalization hook). Instead drive the REAL first-paint path:
	// feed a model.RequestRecord with a redacted credential header through
	// the production builder unifiedActivityRows (same proven idiom as
	// TestForwardedRowHeaderPanelRedacted) and render the builder-produced
	// row. This exercises the exact builder code and is strictly stronger
	// than the literal path.
	rec := model.RequestRecord{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		RequestHeaders: http.Header{
			"Authorization":     {"Bearer sk-ant-PAGESECRET"},
			"Anthropic-Version": {"2023-06-01"},
		},
	}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 5, false, false)
	if len(rows) != 1 {
		t.Fatalf("want 1 activity row from the real builder, got %d", len(rows))
	}
	html := renderTestPage(t, webPageData{
		ActivityTitle: "Activity",
		DefaultClass:  "forwarded-api",
		ActivityRows:  []webRow{rows[0]},
	})
	mustContainAllWeb(t, html,
		`<div class="hdr-sum">`, `headers `, `groups `, `redacted `,
		`<span class="hdr-redpill">redacted</span>`, "‹redacted by ccwrap›",
		`<div class="hdr-group-name">`,
		// Redacted value carries the muted `redv` class in real
		// first-paint HTML (mockup .hv.redv; parity with the JS mirror).
		`<span class="hdr-v mono redv">`)
	if strings.Contains(html, "Bearer ") || strings.Contains(html, "sk-ant") {
		t.Fatalf("credential VALUE leaked into first-paint HTML")
	}
}

func TestBodyViewReadingOrderAndRouting(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	page := webPageData{Title: "t", Heading: "h", Subtitle: "s", LiveEnabled: true, BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}), ActivityTitle: "Activity"}
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(buf.Bytes())
	if len(m) != 2 {
		t.Fatalf("script tag not found")
	}
	js := string(m[1])
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = bytes.NewReader(m[1])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
	rb := scriptFnBody(t, js, "renderBodyView")
	for _, pair := range [][2]string{{"body-anatomy", "bv-config"}, {"bv-config", "'system'"}, {"'system'", "'tools'"}, {"'tools'", "'messages'"}, {"'messages'", "Raw JSON"}, {"Raw JSON", "bv-dl"}} {
		i0, i1 := strings.Index(rb, pair[0]), strings.Index(rb, pair[1])
		if i0 < 0 || i1 < 0 || i0 > i1 {
			t.Fatalf("reading order broken: %q must precede %q", pair[0], pair[1])
		}
	}
	// bv-config / bv-dl classNames are set inline in renderBodyView's body;
	// the 'bv-seclabel' className is factored into the bvSecLabel helper.
	// renderBodyView must PROVABLY use that scaffold, so assert the
	// helper-call in the body and the className in the script.
	mustContainAllWeb(t, rb, "bv-config", "bvSecLabel(", "bv-dl")
	mustContainAllWeb(t, js, "bv-config", "bv-seclabel", "bv-dl")
}

func TestBodyViewCacheRailOnSystemAndMessages(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	page := webPageData{Title: "t", Heading: "h", Subtitle: "s", LiveEnabled: true, BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}), ActivityTitle: "Activity"}
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(buf.Bytes())
	if len(m) != 2 {
		t.Fatalf("script tag not found")
	}
	js := string(m[1])
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = bytes.NewReader(m[1])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
	blk := scriptFnBody(t, js, "bvBlock")
	mustContainAllWeb(t, blk, "bv-chip", "bv-block", "classList")
	if !strings.Contains(blk, "'cc'") || !strings.Contains(blk, "'ccg'") {
		t.Fatalf("bvBlock must set the cache rail class (cc / ccg)")
	}
	turn := scriptFnBody(t, js, "bvTurn")
	if !strings.Contains(turn, "bvCC(") {
		t.Fatalf("bvTurn must pass the message block cache_control (bvCC)")
	}
}

func TestLiveForwardedRowIsDetailsWithNestedSubs(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	page := webPageData{Title: "t", Heading: "h", Subtitle: "s", LiveEnabled: true, BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}), ActivityTitle: "Activity"}
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(buf.Bytes())
	if len(m) != 2 {
		t.Fatalf("script tag not found")
	}
	js := string(m[1])
	cmd := exec.Command("node", "--check", "-")
	cmd.Stdin = bytes.NewReader(m[1])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
	// The row-element builder is the cell/rowCells/makeRowEl trio (makeRowEl
	// composes its summary cells via rowCells, where 'rowchev' et al. live).
	// Assert every mandated token over that contiguous builder unit — same
	// node-checked js + mustContainAllWeb idiom, scoped from `function cell(`
	// through the close of makeRowEl (strictly stronger than makeRowEl alone;
	// no assertion weakened).
	cs := strings.Index(js, "function cell(")
	if cs < 0 {
		t.Fatalf("function cell( not found in inline script")
	}
	mkEnd := strings.Index(js[cs:], "function makeRowEl(")
	if mkEnd < 0 {
		t.Fatalf("function makeRowEl( not found in inline script")
	}
	mk := js[cs:cs+mkEnd] + scriptFnBody(t, js, "makeRowEl")
	mustContainAllWeb(t, mk, "details", "'row'", "summary", "rowchev", "reqinspect", "headerPanelEl", "bodyDrawerEl", "'row nf'", "nf-note")
	pr := scriptFnBody(t, js, "prependRow")
	if strings.Contains(pr, "hdr-drawer") || strings.Contains(pr, "while (sib") {
		t.Fatalf("prependRow must be one-element-per-row — no contiguous hdr/body sibling-run while-trim")
	}
	mustContainAllWeb(t, pr, "querySelectorAll(':scope > .row')")
	// "no innerHTML in the renderer". html/template strips ALL JS comments
	// from the rendered <script>, so a sentinel-comment-presence check on
	// `js` is infeasible by construction; guard the invariant BEHAVIORALLY
	// on the real rendered builder unit `mk` (strictly stronger — a comment
	// can lie; absent innerHTML/esc() in cell/rowCells/makeRowEl cannot).
	// The web.go source sentinel comment is source documentation only.
	mustContainAllWeb(t, mk, "createElement", "textContent")
	if strings.Contains(mk, "innerHTML") || strings.Contains(mk, "esc(") {
		t.Fatalf("cell/rowCells/makeRowEl must build via createElement+textContent only — no innerHTML/esc() row template")
	}
	rc := scriptFnBody(t, js, "rowCells")
	mustContainAllWeb(t, rc, "aria-hidden") // rowchev hidden from a11y tree — live↔first-paint parity
}

// domShimJS is the vendored, test-only DOM shim (the single behavioral
// exception — no npm/jsdom; `node` is already a test dependency via the
// existing `node --check` idiom). It faithfully implements ONLY the DOM
// surface the rendered ensureRowsSkeleton / makeRowEl / rowCells / cell /
// bodyDrawerEl / headerPanelEl / prependRow actually use (mirroring the
// exact surface those functions in web.go touch): createElement/
// createTextNode, className kept in sync with
// classList (contains/add), setAttribute, textContent setter, dataset,
// appendChild / insertBefore WITH DocumentFragment splice-on-insert (the
// principal-risk semantic — appending/inserting a fragment moves ALL its
// children, not the fragment node), firstChild, childNodes/children,
// nextSibling/nextElementSibling, remove(), querySelector/querySelectorAll
// for ':scope > .rows' / ':scope > .row-head' / ':scope > .row' (direct-
// child class match), innerHTML assignment (empty string or ROW_HEAD_HTML) and
// insertAdjacentHTML('afterbegin', …) via a minimal recursive HTML parser
// (the only HTML strings the code feeds are ROW_HEAD_HTML and the
// battery's '<i></i>'), document.createDocumentFragment(),
// document.createRange().createContextualFragment(html) and
// document.createElement('template').content — all behave as fragments
// (the adversarial battery exercises them). Every node removal records the
// removed node's class tokens into the test-visible __removed log. The
// shim's correctness is PROVEN by the adversarial revert battery
// (TestPrependRowOrphanInvariantBehavioralBatteryReverts is run out of
// band via the worktree harness), not by inspection.
const domShimJS = `
'use strict';
var __removed = []; // pre-heal removal log: class tokens of every removed node
function __tok(n){ return (n && n._cls) ? Object.keys(n._cls) : []; }
var DOC_FRAG = 11;
function ClassList(node){ this._n = node; }
ClassList.prototype.contains = function(c){ return !!this._n._cls[c]; };
ClassList.prototype.add = function(c){ if(c){ this._n._cls[c]=true; this._n._syncClassName(); } };
function Node(tag){
  this.tagName = tag ? String(tag).toUpperCase() : null;
  this.nodeType = tag === '#text' ? 3 : (tag === '#frag' ? DOC_FRAG : 1);
  this._cls = {};
  this._attrs = {};
  this.childNodes = [];
  this.parentNode = null;
  this.dataset = {};
  this._text = '';
  this.classList = new ClassList(this);
  this._className = '';
}
Node.prototype._syncClassName = function(){
  this._className = Object.keys(this._cls).join(' ');
};
Object.defineProperty(Node.prototype, 'className', {
  get: function(){ return this._className; },
  set: function(v){
    this._cls = {};
    String(v == null ? '' : v).split(/\s+/).forEach(function(t){ if(t) this._cls[t]=true; }, this);
    this._syncClassName();
  }
});
Object.defineProperty(Node.prototype, 'textContent', {
  get: function(){
    if (this.nodeType === 3) return this._text;
    return this.childNodes.map(function(c){ return c.textContent; }).join('');
  },
  set: function(v){
    if (this.nodeType === 3){ this._text = String(v == null ? '' : v); return; }
    // textContent= replaces all children with a single text node
    this.childNodes.slice().forEach(function(c){ c.parentNode = null; });
    this.childNodes = [];
    var t = new Node('#text'); t._text = String(v == null ? '' : v); t.parentNode = this;
    this.childNodes.push(t);
  }
});
Object.defineProperty(Node.prototype, 'children', {
  get: function(){ return this.childNodes.filter(function(c){ return c.nodeType === 1; }); }
});
Object.defineProperty(Node.prototype, 'firstChild', {
  get: function(){ return this.childNodes.length ? this.childNodes[0] : null; }
});
Object.defineProperty(Node.prototype, 'nextSibling', {
  get: function(){
    if (!this.parentNode) return null;
    var s = this.parentNode.childNodes, i = s.indexOf(this);
    return (i >= 0 && i + 1 < s.length) ? s[i+1] : null;
  }
});
Object.defineProperty(Node.prototype, 'nextElementSibling', {
  get: function(){
    var n = this.nextSibling;
    while (n && n.nodeType !== 1) n = n.nextSibling;
    return n || null;
  }
});
Node.prototype.setAttribute = function(k, v){
  this._attrs[k] = String(v);
  if (k === 'class'){ this.className = v; return; }
  var m = /^data-(.+)$/.exec(k);
  if (m){
    var camel = m[1].replace(/-([a-z])/g, function(_, c){ return c.toUpperCase(); });
    this.dataset[camel] = String(v);
  }
};
Node.prototype.getAttribute = function(k){ return Object.prototype.hasOwnProperty.call(this._attrs, k) ? this._attrs[k] : null; };
function __detach(n){
  if (n.parentNode){
    var s = n.parentNode.childNodes, i = s.indexOf(n);
    if (i >= 0) s.splice(i, 1);
    n.parentNode = null;
  }
}
// appendChild / insertBefore implement DocumentFragment splice-on-insert:
// inserting a fragment moves ALL of its children (not the fragment node).
Node.prototype.appendChild = function(child){
  if (child.nodeType === DOC_FRAG){
    child.childNodes.slice().forEach(function(c){ this.appendChild(c); }, this);
    return child;
  }
  __detach(child);
  child.parentNode = this;
  this.childNodes.push(child);
  return child;
};
Node.prototype.insertBefore = function(node, ref){
  if (node.nodeType === DOC_FRAG){
    node.childNodes.slice().forEach(function(c){ this.insertBefore(c, ref); }, this);
    return node;
  }
  __detach(node);
  node.parentNode = this;
  if (ref == null){ this.childNodes.push(node); return node; }
  var i = this.childNodes.indexOf(ref);
  if (i < 0) this.childNodes.push(node);
  else this.childNodes.splice(i, 0, node);
  return node;
};
Node.prototype.remove = function(){
  __removed.push(__tok(this)); // pre-heal removal log (records BEFORE detach)
  __detach(this);
};
function __scopeSel(sel){
  var m = /^:scope >\s*\.(\S+)$/.exec(sel);
  return m ? m[1] : null;
}
Node.prototype.querySelector = function(sel){
  var cls = __scopeSel(sel);
  if (cls == null) throw new Error('shim querySelector only supports ":scope > .<class>", got ' + sel);
  var kids = this.children;
  for (var i = 0; i < kids.length; i++) if (kids[i]._cls[cls]) return kids[i];
  return null;
};
Node.prototype.querySelectorAll = function(sel){
  var cls = __scopeSel(sel);
  if (cls == null) throw new Error('shim querySelectorAll only supports ":scope > .<class>", got ' + sel);
  return this.children.filter(function(k){ return k._cls[cls]; });
};
// Minimal recursive HTML parser — ONLY the strings the code feeds
// innerHTML/insertAdjacentHTML/createContextualFragment: ROW_HEAD_HTML
// (nested <div class=… role=…>text</div>) and the battery's '<i></i>'.
// Anything broader throws (forces us to keep the shim honest).
function __parseHTML(html){
  var frag = new Node('#frag');
  var stack = [frag];
  var re = /<\/?[a-zA-Z][^>]*>|[^<]+/g, m;
  while ((m = re.exec(html)) !== null){
    var tok = m[0];
    if (tok[0] !== '<'){
      // NOTE: whitespace-only text nodes are intentionally dropped; this is
      // safe ONLY because ROW_HEAD_HTML has no inter-tag whitespace — keep it
      // that way (pretty-printing ROW_HEAD_HTML would desync firstChild/
      // nextSibling vs a real browser).
      var txt = tok.replace(/^\s+|\s+$/g, '');
      if (txt){ var tn = new Node('#text'); tn._text = txt; tn.parentNode = stack[stack.length-1]; stack[stack.length-1].childNodes.push(tn); }
      continue;
    }
    if (tok[1] === '/'){ stack.pop(); continue; }
    var tm = /^<([a-zA-Z]+)((?:\s+[a-zA-Z-]+="[^"]*")*)\s*\/?>$/.exec(tok);
    if (!tm) throw new Error('shim HTML parser: unsupported token ' + tok);
    var el = new Node(tm[1]);
    var am = /([a-zA-Z-]+)="([^"]*)"/g, a;
    while ((a = am.exec(tm[2])) !== null) el.setAttribute(a[1], a[2]);
    el.parentNode = stack[stack.length-1];
    stack[stack.length-1].childNodes.push(el);
    if (!/\/>$/.test(tok)) stack.push(el);
  }
  return frag;
}
Object.defineProperty(Node.prototype, 'innerHTML', {
  get: function(){ return ''; },
  set: function(v){
    this.childNodes.slice().forEach(function(c){ c.parentNode = null; });
    this.childNodes = [];
    if (v === '' || v == null) return;
    this.appendChild(__parseHTML(String(v)));
  }
});
Node.prototype.insertAdjacentHTML = function(pos, html){
  var frag = __parseHTML(String(html));
  if (pos === 'afterbegin') this.insertBefore(frag, this.firstChild);
  else if (pos === 'beforeend') this.appendChild(frag);
  else throw new Error('shim insertAdjacentHTML: unsupported position ' + pos);
};
// <template>.content is a fragment whose insertion splices its children.
Object.defineProperty(Node.prototype, 'content', {
  get: function(){
    if (this.tagName === 'TEMPLATE'){
      if (!this._content) this._content = new Node('#frag');
      return this._content;
    }
    return undefined;
  }
});
var document = {
  createElement: function(tag){ return new Node(tag); },
  createTextNode: function(t){ var n = new Node('#text'); n._text = String(t == null ? '' : t); return n; },
  createDocumentFragment: function(){ return new Node('#frag'); },
  createRange: function(){ return { createContextualFragment: function(html){ return __parseHTML(String(html)); } }; }
};
`

// scriptSpan returns the verbatim rendered source from the start of
// `function <from>(` through the matching close brace of `function <to>(`
// (inclusive). Unlike a hand-picked function list, a contiguous SPAN also
// captures ANY helper a battery revert hoists between those functions —
// e.g. the "helper-hoist" evasion inserts `function _sp(a,b){…}`
// immediately before `function prependRow(`, which falls inside the
// cell→prependRow span. Lifting the span (not an enumerated set) is what
// makes the harness faithful to that evasion class.
func scriptSpan(t *testing.T, js, from, to string) string {
	t.Helper()
	start := strings.Index(js, "function "+from+"(")
	if start < 0 {
		t.Fatalf("function %s( not found in inline script", from)
	}
	ti := strings.Index(js, "function "+to+"(")
	if ti < 0 {
		t.Fatalf("function %s( not found in inline script", to)
	}
	open := strings.IndexByte(js[ti:], '{')
	if open < 0 {
		t.Fatalf("function %s: opening brace not found", to)
	}
	open += ti
	depth := 0
	for j := open; j < len(js); j++ {
		switch js[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[start : j+1]
			}
		}
	}
	t.Fatalf("function %s: unbalanced braces", to)
	return ""
}

// liftRenderedFns extracts, VERBATIM from the rendered <script>, exactly
// the code the trim invariant depends on, to run against the vendored DOM
// shim (we do NOT eval the whole inline script — it wires EventSource/DOM
// at module top level; the shim deliberately models only the small DOM
// surface these functions use). Lifting the rendered source (not a re-
// implementation) is what makes B′ and every battery revert exercised
// exactly as-shipped. The lifted set is the precise call-closure
// (verified by the cross-helper call-graph): ROW_HEAD_HTML; the two
// upstream, self-contained builders headerPanelEl + bodyDrawerEl (called
// by makeRowEl, declared earlier, calling no other lifted helper); then
// the CONTIGUOUS span function cell( … through the close of prependRow —
// which contains cell/rowCells/makeRowEl/ensureRowsSkeleton/prependRow AND
// any helper a revert hoists within it (e.g. an injected `_sp`). No
// battery revert introduces a helper outside this universe.
//
// FORWARD CONSTRAINT (keep this true or extend the lift): This guard
// executes the REAL rendered script functions; its faithfulness depends on
// the lifted set being the exact call-closure. Currently that holds
// because (1) headerPanelEl and bodyDrawerEl are self-contained (they call
// no other inline-script helper) and (2) cell, rowCells, makeRowEl and
// prependRow form a single contiguous source span (function cell( …
// through the close of function prependRow(), so scriptSpan captures any
// helper hoisted between them — incl. the helper-hoist evasion). If a
// future change makes a builder call another helper, splits that span, or
// hoists a dependency outside it: EXTEND the lifted set accordingly. Never
// let the lift silently pass with an incomplete closure — the orphan bug
// this guards has recurred repeatedly.
func liftRenderedFns(t *testing.T, js string) string {
	t.Helper()
	var b strings.Builder
	// ROW_HEAD_HTML const (single line in the source).
	for _, line := range strings.Split(js, "\n") {
		if strings.Contains(line, "const ROW_HEAD_HTML") {
			b.WriteString(strings.TrimSpace(line))
			b.WriteString("\n")
			break
		}
	}
	if !strings.Contains(b.String(), "ROW_HEAD_HTML") {
		t.Fatalf("ROW_HEAD_HTML const not found in rendered script")
	}
	// Upstream self-contained builders, individually (they sit far above
	// the cell→prependRow span and call no other lifted helper).
	//
	// Extension to the FORWARD CONSTRAINT: prependRow now calls
	// decorateProfileAnnotation + renderSwitchMarker after insertion
	// (idempotent decorators that paint via textContent on the
	// freshly-stamped data-attrs). Both call providerHue. They live BELOW
	// the cell→prependRow span, so the lift must include them — else the
	// shim run RED-s with ReferenceError, masking the genuine orphan
	// invariant. Adding them keeps the harness exercising the REAL
	// rendered script as-shipped (no stubbing).
	for _, name := range []string{"headerPanelEl", "bodyDrawerEl", "respBodyDrawerEl", "teleBodyDrawerEl", "providerHue", "decorateProfileAnnotation", "renderSwitchMarker"} {
		body := scriptFnBody(t, js, name)
		sig := "function " + name + "("
		i := strings.Index(js, sig)
		params := js[i+len(sig):]
		params = params[:strings.IndexByte(params, ')')]
		b.WriteString("function ")
		b.WriteString(name)
		b.WriteString("(")
		b.WriteString(params)
		b.WriteString(")")
		b.WriteString(body)
		b.WriteString("\n")
	}
	// Verbatim contiguous span: cell … prependRow (captures any revert-
	// hoisted helper placed between them — the helper-hoist evasion class).
	b.WriteString(scriptSpan(t, js, "cell", "prependRow"))
	b.WriteString("\n")
	return b.String()
}

// TestPrependRowOrphanInvariantBehavioral is the behavioral trim-invariant
// guard — the guarantee that replaces the structurally-incomplete
// substring/source-text guard (a substring scan cannot catch the
// runtime-DOM-shape "body-drawer-orphan" class; many committable reverts
// stayed green against such a guard). It runs the ACTUAL rendered
// ensureRowsSkeleton/makeRowEl/rowCells/cell/bodyDrawerEl/headerPanelEl/
// prependRow (incl. B′ as-shipped) via the already-present `node` against
// the vendored DOM shim (domShimJS) — the single behavioral exception
// (no npm/jsdom).
//
// Scenario: LIMIT+3 forwarded SSE-shaped rows (headerGroups non-empty +
// bodyRefId set, plus time/label/main/right/kind/class/ts — the exact
// fields activityRowData/makeRowEl read) prepended one by one. Asserts:
//
//	(i)  post-state invariant — every direct child of the .rows container
//	     is .row or .row-head, and the .row count == LIMIT. (Catches a B′
//	     deletion: the orphan would persist.)
//	(ii) pre-heal removal log — every node removed during the whole run
//	     had a `row` class token. Correct construction nests the drawer so
//	     the trim removes only over-limit .row; ANY orphan-producing
//	     revert (single-if/property-stash/fragment/any-spelling) makes B′
//	     remove a non-.row ⟹ RED *even though B′ self-heals* (so B′ is a
//	     defense-in-depth runtime guarantee, never a license to ship
//	     orphan construction). Spelling-independent (a runtime property).
//
// The shim's faithfulness is the principal-risk trust boundary; it is
// proven by the adversarial revert battery (property-stash,
// createContextualFragment, <template>.content, helper-hoist, the
// DocumentFragment evasion, full-while) — every revert RED, correct
// GREEN — run via the worktree harness.
func TestPrependRowOrphanInvariantBehavioral(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	page := webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	}
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]

	lifted := liftRenderedFns(t, js)

	// SSE-record-shaped forwarded rows: exactly the fields makeRowEl /
	// rowCells / bodyDrawerEl / headerPanelEl read. headerGroups non-empty
	// + bodyRefId set ⟹ makeRowEl takes the <details class="row"> branch
	// and nests headerPanelEl + bodyDrawerEl inside .reqinspect.
	const limit = 3
	scenario := `
var LIMIT = ` + strconv.Itoa(limit) + `;
function mkRow(i){
  return {
    ts: '2026-05-18T00:00:0' + (i % 10) + 'Z',
    time: '00:00:0' + (i % 10),
    label: 'forwarded-api',
    main: 'POST /v1/messages #' + i,
    right: '200 · 12 ms',
    mono: true,
    forwarded: true,
    kind: 'request',
    class: 'forwarded-api',
    headerGroups: [ { name: 'Protocol & versioning', rows: [ { name: 'anthropic-version', value: '2023-06-01', redacted: false } ] } ],
    bodyRefId: 'deadbeefcafebabe' + i
  };
}
var bodyEl = document.createElement('div');
for (var i = 0; i < LIMIT + 3; i++) prependRow(bodyEl, mkRow(i), LIMIT);
var rows = bodyEl.querySelector(':scope > .rows');
var directChildClasses = (rows ? rows.children : []).map(function(c){ return Object.keys(c._cls); });
var rowCount = rows ? rows.querySelectorAll(':scope > .row').length : -1;
var removedNonRow = __removed.filter(function(toks){ return toks.indexOf('row') < 0; });
process.stdout.write(JSON.stringify({
  directChildClasses: directChildClasses,
  rowCount: rowCount,
  removedNonRow: removedNonRow,
  removedAll: __removed
}));
`
	driver := domShimJS + "\n" + lifted + "\n" + scenario

	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("behavioral driver failed to run: %v\n--- driver ---\n%s\n--- output ---\n%s", err, driver, out)
	}

	var res struct {
		DirectChildClasses [][]string `json:"directChildClasses"`
		RowCount           int        `json:"rowCount"`
		RemovedNonRow      [][]string `json:"removedNonRow"`
		RemovedAll         [][]string `json:"removedAll"`
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("could not parse node driver result: %v\nraw: %s", jerr, out)
	}

	// (i) post-state invariant: every direct child of `rows` is .row /
	// .row-head and the .row count == LIMIT (a B′ deletion would leave the
	// orphan here; an over-/under-trim would move rowCount off LIMIT).
	for _, toks := range res.DirectChildClasses {
		set := map[string]bool{}
		for _, tk := range toks {
			set[tk] = true
		}
		if !set["row"] && !set["row-head"] {
			t.Fatalf("post-state invariant (i): a direct child of .rows is neither .row nor .row-head: classes=%v (all direct children=%v) — body-drawer orphaned as a `rows` sibling", toks, res.DirectChildClasses)
		}
	}
	if res.RowCount != limit {
		t.Fatalf("post-state invariant (i): .row count == LIMIT expected, got %d (want %d); direct children=%v", res.RowCount, limit, res.DirectChildClasses)
	}

	// (ii) pre-heal removal log: correct construction nests the drawer, so
	// the trim removes ONLY over-limit .row nodes. ANY orphan-producing
	// revert makes B′ (or a sibling-run trim) remove a non-.row node ⟹ this
	// fails RED *despite B′ self-healing the post-state*. Spelling-
	// independent: it is a runtime-DOM property, not a source substring.
	if len(res.RemovedNonRow) != 0 {
		t.Fatalf("pre-heal removal log (ii): every removed node must carry a `row` class token (correct code never orphans; B′ removing a non-.row ⟹ a genuine orphan-producing construction regression). Non-.row removals=%v ; full removal log=%v", res.RemovedNonRow, res.RemovedAll)
	}
	// Sanity: the trim must actually have fired (LIMIT+3 inserted, LIMIT
	// kept ⟹ ≥3 .row removals), else the scenario is not exercising trim.
	if len(res.RemovedAll) < 3 {
		t.Fatalf("scenario sanity: expected ≥3 trim removals (LIMIT+3 inserted, LIMIT kept), got %d — the behavioral guard is not exercising the trim", len(res.RemovedAll))
	}
}

func TestWebKVHasValueHTMLAndDataState(t *testing.T) {
	// Compile-time guard: the field set must compile against this struct literal.
	var _ = webKV{Label: "L", Value: "V", ValueHTML: template.HTML(""), DataState: "active"}
}

func TestWebRowHasSP3Fields(t *testing.T) {
	var _ = webRow{Category: "profile_switch", ProfileName: "n", ProfileProvider: "p",
		SwitchFrom: "a", SwitchFromProvider: "ap", SwitchTo: "b", SwitchToProvider: "bp",
		SwitchClass: "live", SwitchRequested: "r", SwitchReason: "x"}
}

func TestWebPageDataHasProfileFields(t *testing.T) {
	var _ = webPageData{HasProfilesFile: true, ProfileCount: 3}
}

func TestUnifiedActivityRows_SwitchMarker_Switched(t *testing.T) {
	detail := `{"from":"alpha","from_provider":"Anthropic","to":"beta","to_provider":"OpenAI","class":"live"}`
	rows := unifiedActivityRows(nil, nil, []model.TraceRecord{{
		Timestamp: time.Now(),
		SessionID: "s1",
		Category:  "profile_switch",
		Summary:   "switched",
		Detail:    detail,
	}}, 10, false, false)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Class != "trace" || r.Kind != "trace" {
		t.Fatalf("Class/Kind = %q/%q, want trace/trace (5-class IA preserved)", r.Class, r.Kind)
	}
	if r.Category != "profile_switch" {
		t.Fatalf("Category = %q, want profile_switch", r.Category)
	}
	if r.SwitchFrom != "alpha" || r.SwitchTo != "beta" || r.SwitchClass != "live" {
		t.Fatalf("switch fields: from=%q to=%q class=%q", r.SwitchFrom, r.SwitchTo, r.SwitchClass)
	}
	if r.SwitchFromProvider != "Anthropic" || r.SwitchToProvider != "OpenAI" {
		t.Fatalf("provider fields: from_provider=%q to_provider=%q", r.SwitchFromProvider, r.SwitchToProvider)
	}
}

func TestUnifiedActivityRows_SwitchMarker_Rejected(t *testing.T) {
	detail := `{"from":"alpha","from_provider":"Anthropic","requested":"broken","reason":"auth_key_env"}`
	rows := unifiedActivityRows(nil, nil, []model.TraceRecord{{
		Timestamp: time.Now(), SessionID: "s1",
		Category: "profile_switch", Summary: "rejected", Detail: detail,
	}}, 10, false, false)
	r := rows[0]
	if r.SwitchRequested != "broken" || r.SwitchReason != "auth_key_env" {
		t.Fatalf("rejected fields: requested=%q reason=%q", r.SwitchRequested, r.SwitchReason)
	}
	if r.SwitchTo != "" || r.SwitchToProvider != "" {
		t.Fatalf("rejected must not carry to/to_provider: %+v", r)
	}
}

func TestUnifiedActivityRows_TraceNotProfileSwitch_LeavesFieldsEmpty(t *testing.T) {
	rows := unifiedActivityRows(nil, nil, []model.TraceRecord{{
		Timestamp: time.Now(), SessionID: "s1",
		Category: "route", Summary: "session route configured", Detail: "https://x",
	}}, 10, false, false)
	r := rows[0]
	if r.Category != "route" {
		t.Fatalf("Category = %q, want route (propagated verbatim)", r.Category)
	}
	if r.SwitchFrom != "" || r.SwitchTo != "" {
		t.Fatalf("non-profile_switch traces must not populate switch fields: %+v", r)
	}
}

func TestUnifiedActivityRows_PerRowProfileDataAttrs(t *testing.T) {
	rec := model.RequestRecord{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		ActiveProfileName: "alpha", ActiveProfileProvider: "Anthropic",
	}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 10, false, false)
	r := rows[0]
	if r.ProfileName != "alpha" || r.ProfileProvider != "Anthropic" {
		t.Fatalf("per-row profile fields: name=%q provider=%q", r.ProfileName, r.ProfileProvider)
	}
}

func TestUnifiedActivityRows_NoProfile_NoAnnotation(t *testing.T) {
	rec := model.RequestRecord{Method: "POST", Path: "/v1/messages", StatusCode: 200}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 10, false, false)
	r := rows[0]
	if r.ProfileName != "" || r.ProfileProvider != "" {
		t.Fatalf("no-profile request must not populate annotation: %+v", r)
	}
}

func TestRenderedActivityRow_PerRowProfileDataAttrs(t *testing.T) {
	page := webPageData{
		Title: "x", LiveEnabled: false,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
		ActivityRows: []webRow{{
			Time: "12:00:00", Label: "forwarded-api", Main: "POST /v1/messages",
			Right: "200", Forwarded: true, Kind: "request", Class: "forwarded-api",
			ProfileName: "alpha", ProfileProvider: "Anthropic",
		}},
		Classes:       []webClassCount{{"all", "All", 1}, {"forwarded-api", "Forwarded API", 1}},
		DefaultClass:  "forwarded-api",
		ActivityTitle: "Activity",
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()
	if !strings.Contains(html, `data-profile-name="alpha"`) {
		t.Fatalf("row must carry data-profile-name attr: %s", html)
	}
	if !strings.Contains(html, `data-profile-provider="Anthropic"`) {
		t.Fatalf("row must carry data-profile-provider attr: %s", html)
	}
}

func TestRenderedActivityRow_SwitchMarkerDataAttrs(t *testing.T) {
	page := webPageData{
		Title: "x", LiveEnabled: false,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
		ActivityRows: []webRow{{
			Time: "12:00:00", Label: "trace", Main: "switched", Right: "profile_switch · ...",
			Kind: "trace", Class: "trace", Category: "profile_switch",
			SwitchFrom: "alpha", SwitchFromProvider: "Anthropic",
			SwitchTo: "beta", SwitchToProvider: "OpenAI", SwitchClass: "live",
		}},
		ActivityTitle: "Activity",
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()
	for _, want := range []string{
		`data-category="profile_switch"`,
		`data-switch-from="alpha"`,
		`data-switch-from-provider="Anthropic"`,
		`data-switch-to="beta"`,
		`data-switch-to-provider="OpenAI"`,
		`data-switch-class="live"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("switch-marker row missing %q\n  html: %s", want, html)
		}
	}
}

func TestRenderedActivityRow_NoProfile_NoDataAttrs(t *testing.T) {
	page := webPageData{
		Title: "x", LiveEnabled: false,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
		ActivityRows: []webRow{{
			Time: "12:00:00", Label: "forwarded-api", Main: "POST /",
			Forwarded: true, Kind: "request", Class: "forwarded-api",
		}},
		ActivityTitle: "Activity",
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()
	if strings.Contains(html, "data-profile-name=") {
		t.Fatalf("no-profile row must NOT emit data-profile-name attr: %s", html)
	}
}

// TestSP3InlineScript_BootstrapProfileToken locks the CSRF token capture:
// the inline script reads boot.profile_token ONCE into a closure variable
// and never writes it back to the DOM. The variable name is PROFILE_TOKEN
// (uppercase).
func TestSP3InlineScript_BootstrapProfileToken(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", ProfileToken: "deadbeef"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	if !strings.Contains(js, "boot.profile_token") {
		t.Fatalf("inline script must read boot.profile_token")
	}
	if !regexp.MustCompile(`var\s+PROFILE_TOKEN\s*=\s*(?:String\()?\s*boot\.profile_token`).MatchString(js) &&
		!regexp.MustCompile(`const\s+PROFILE_TOKEN\s*=\s*(?:String\()?\s*boot\.profile_token`).MatchString(js) {
		t.Fatalf("inline script must capture boot.profile_token into PROFILE_TOKEN closure")
	}
	// Token must never be re-exposed via innerHTML/textContent on a DOM node.
	if regexp.MustCompile(`(\.textContent\s*=\s*PROFILE_TOKEN|\.innerHTML\s*=\s*PROFILE_TOKEN|setAttribute\(\s*[^)]*PROFILE_TOKEN)`).MatchString(js) {
		t.Fatalf("PROFILE_TOKEN must not be written to DOM")
	}
}

// TestSP3InlineScript_PatchSessionExtended locks the extension: the
// existing Traffic-only patchSession must also patch Profile/Route/Auth/
// Models cells when state.session updates (so SSE session_updated reflects
// in chip + ribbon without full reload).
func TestSP3InlineScript_PatchSessionExtended(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	js := m[1]
	for _, fn := range []string{"updateProfileCell", "updateRouteCell", "updateAuthCell", "updateModelsCell"} {
		if !strings.Contains(js, fn) {
			t.Errorf("inline JS must define %s (patchSession extension)", fn)
		}
	}
	if !regexp.MustCompile(`function\s+patchSession\b[\s\S]*updateProfileCell\(`).MatchString(js) {
		t.Fatalf("patchSession must call updateProfileCell")
	}
}

// TestSP3InlineScript_ReconciliationGate locks the fetch-failure path of
// the switch state machine: it must consult the pendingSSESnapshot buffer
// with the 3-branch comparison.
func TestSP3InlineScript_ReconciliationGate(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	js := m[1]
	// Three required identifiers:
	for _, name := range []string{
		"popPendingSSESnapshot",
		"popClickedName",
		"popPreClickActiveProfileName",
	} {
		if !strings.Contains(js, name) {
			t.Errorf("inline JS missing reconciliation-gate identifier %q", name)
		}
	}
	if !regexp.MustCompile(`popPendingSSESnapshot[\s\S]{0,200}=== ?\(?popClickedName`).MatchString(js) &&
		!regexp.MustCompile(`popClickedName[\s\S]{0,200}=== ?\(?popPendingSSESnapshot`).MatchString(js) {
		if !strings.Contains(js, "popPendingSSESnapshot.active_profile_name") || !strings.Contains(js, "popClickedName") {
			t.Errorf("reconciliation gate branch-1 not found")
		}
	}
	if !strings.Contains(js, "popPreClickActiveProfileName") {
		t.Errorf("reconciliation gate branch-2 must compare to popPreClickActiveProfileName")
	}
}

// TestSP3InlineScript_SSEEventBuffersDuringPending locks that the
// session_updated SSE handler MUST write to popPendingSSESnapshot when
// popState === 'pending' (do not unconditionally apply — and do not
// unconditionally ignore).
func TestSP3InlineScript_SSEEventBuffersDuringPending(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	js := m[1]
	if !regexp.MustCompile(`popState\s*===\s*['"]pending['"][\s\S]{0,200}popPendingSSESnapshot\s*=`).MatchString(js) {
		t.Fatalf("session_updated handler must assign popPendingSSESnapshot when popState=='pending'")
	}
}

func TestSP3InlineScript_ProviderHueAndAnnotation(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	js := m[1]
	if !regexp.MustCompile(`function\s+providerHue\b`).MatchString(js) {
		t.Fatalf("inline JS must define providerHue")
	}
	if !regexp.MustCompile(`function\s+decorateProfileAnnotation\b`).MatchString(js) {
		t.Fatalf("inline JS must define decorateProfileAnnotation")
	}
	annStart := strings.Index(js, "function decorateProfileAnnotation")
	rest := js[annStart : annStart+1500]
	if strings.Contains(rest, ".innerHTML") {
		t.Fatalf("decorateProfileAnnotation must NOT use innerHTML")
	}
	if !strings.Contains(rest, "textContent") {
		t.Fatalf("decorateProfileAnnotation must use textContent")
	}
}

// TestSP3InlineScript_PendingSSEThenFetchFail is the race-fix structural
// regression: the inline JS must contain the exact branch where the
// reconciliation gate consults popPendingSSESnapshot AFTER a fetch
// failure, applies the buffered snapshot, and renders outcome.success.
// JS-only assertion (no runtime exercise) — the structural code path
// must exist in the inline script.
func TestSP3InlineScript_PendingSSEThenFetchFail(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	js := m[1]
	if !regexp.MustCompile(`function\s+reconcileFetchFailure\b`).MatchString(js) {
		t.Fatalf("reconcileFetchFailure function must exist")
	}
	idx := strings.Index(js, "function reconcileFetchFailure")
	body := js[idx : idx+2500]
	for _, want := range []string{
		"popPendingSSESnapshot",
		"popClickedName",
		"popPreClickActiveProfileName",
		"switch applied",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reconcileFetchFailure body missing %q", want)
		}
	}
	if !strings.Contains(js, "reconcileFetchFailure()") {
		t.Fatalf("/profile/switch fetch catch must call reconcileFetchFailure()")
	}
}

// TestSP3InlineScript_BodiesCell_PatchAndClick locks the inline JS for the
// runtime capture toggle: (1) updateBodiesCell exists and is wired into
// patchSession (so SSE session_updated flips the cell live), (2) the click
// handler POSTs /capture/bodies with the CSRF token, (3) the pending
// visual state is set then settled.
func TestSP3InlineScript_BodiesCell_PatchAndClick(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", ProfileToken: "deadbeef"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	// (1) updateBodiesCell wired into patchSession.
	if !regexp.MustCompile(`function\s+updateBodiesCell\b`).MatchString(js) {
		t.Errorf("inline JS must define updateBodiesCell")
	}
	if !regexp.MustCompile(`function\s+patchSession\b[\s\S]*updateBodiesCell\(`).MatchString(js) {
		t.Errorf("patchSession must call updateBodiesCell (SSE-driven live patch)")
	}
	// (2) clicking the cell OPENS a popover (mirror of the Models cell popover)
	// rather than directly toggling. The popover hosts two capture toggles.
	if !strings.Contains(js, "attachBodiesCellHandlers") {
		t.Errorf("inline JS must define attachBodiesCellHandlers")
	}
	if !strings.Contains(js, "openBodiesPop") {
		t.Errorf("inline JS must define openBodiesPop (cell click opens a popover)")
	}
	if !strings.Contains(js, "closeBodiesPop") {
		t.Errorf("inline JS must define closeBodiesPop (popover dismiss)")
	}
	// (3) both capture endpoints POSTed from the two toggle rows, each with the
	// CSRF token header.
	if !strings.Contains(js, "'/capture/bodies'") && !strings.Contains(js, `"/capture/bodies"`) {
		t.Errorf("request-bodies toggle must POST /capture/bodies")
	}
	if !strings.Contains(js, "'/capture/telemetry'") && !strings.Contains(js, `"/capture/telemetry"`) {
		t.Errorf("telemetry toggle must POST /capture/telemetry")
	}
	if !strings.Contains(js, "X-CCWRAP-Profile-Token") || !strings.Contains(js, "PROFILE_TOKEN") {
		t.Errorf("toggle POSTs must send X-CCWRAP-Profile-Token: PROFILE_TOKEN header")
	}
	// (4) updateBodiesCell consumes both wire fields and emits the join summary.
	if !strings.Contains(js, "capture_bodies") {
		t.Errorf("updateBodiesCell must read session.capture_bodies wire field")
	}
	if !strings.Contains(js, "capture_telemetry") {
		t.Errorf("updateBodiesCell / telemetry toggle must read session.capture_telemetry wire field")
	}
	if !strings.Contains(js, "request + response + telemetry") {
		t.Errorf("updateBodiesCell must emit the 'request + response + telemetry' combined summary")
	}
}

// TestWebMarkup_BodiesDetailConsistent pins the body-capture copy to ccwrap's
// ACTUAL behavior: capture_bodies records REQUEST + RESPONSE bodies (the
// Anthropic MITM path tees the response stream; see responsetee.go). It also
// enforces Go↔JS lockstep: every detail string the Go helper
// bodiesCellPresentation can emit must ALSO appear verbatim in the inline JS
// updateBodiesCell (which now drives all post-toggle copy via state mutation +
// re-render rather than writing detail strings inline at click time).
func TestWebMarkup_BodiesDetailConsistent(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "recording request + response bodies (credentials redacted)") {
		t.Fatalf("bodies copy must reflect that capture records request + response bodies")
	}
	// Drive the Go helper over its full input space and assert each unique
	// DETAIL string the server can render also exists verbatim in the inline JS
	// mirror — so the live patch never drifts from the first paint. (Value
	// strings are composed in both sides from the same building blocks below —
	// strings.Join(parts," + ") in Go, parts.join(' + ') in JS — so a contiguous
	// substring match would be a false negative for the joined/⚠ forms.)
	for _, b := range []bool{false, true} {
		for _, u := range []bool{false, true} {
			for _, tel := range []bool{false, true} {
				_, detail, _ := bodiesCellPresentation(b, u, tel)
				if !strings.Contains(body, detail) {
					t.Errorf("inline JS missing Go-emitted detail %q (b=%v u=%v tel=%v)", detail, b, u, tel)
				}
			}
		}
	}
	// Value building blocks must match between the Go join and the JS join.
	for _, frag := range []string{"'request'", "'telemetry'", "' + '", "' ⚠'"} {
		if !strings.Contains(body, frag) {
			t.Errorf("inline JS missing value-building fragment %q used by updateBodiesCell", frag)
		}
	}
}

// TestSP3InlineScript_NoInheritEnvRow — the popover no longer renders a
// special "inherit-env" row at the bottom of the catalog. The "official"
// profile (auto-restored by EnsureOfficialProfile on every launch)
// occupies that semantic slot and renders through the same per-row builder
// as every other entry.
func TestSP3InlineScript_NoInheritEnvRow(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	// The legacy "env-live" annotation strings are gone — there's no row
	// to host them.
	if strings.Contains(js, "env: canonical Anthropic") {
		t.Errorf("inline JS still contains the inherit-env env-live annotation; it should have been removed")
	}
	if strings.Contains(js, "'env-live'") || strings.Contains(js, `"env-live"`) {
		t.Errorf("inline JS still emits the 'env-live' badge; it should have been removed")
	}
}

// TestSP3InlineScript_BodiesCell_UnmaskedState locks the JS handling of the
// CCWRAP_UNMASK_CREDENTIALS=1 wire field: updateBodiesCell must read
// session.capture_bodies_unmasked and route to the bodies-unmasked
// dataset.state when both capture + unmask are true. Without this branch
// the danger-color cell would never render live (only on first paint).
func TestSP3InlineScript_BodiesCell_UnmaskedState(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", ProfileToken: "deadbeef"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	if !strings.Contains(js, "capture_bodies_unmasked") {
		t.Errorf("inline JS must read session.capture_bodies_unmasked")
	}
	if !strings.Contains(js, "bodies-unmasked") {
		t.Errorf("inline JS must set the bodies-unmasked dataset.state branch")
	}
	if !strings.Contains(js, "UNMASKED") {
		t.Errorf("inline JS must surface the UNMASKED marker text")
	}
}

func TestWebInlineCSS_OutcomeDangerPreservesNewlines(t *testing.T) {
	// .outcome.danger must have white-space: pre-line so multi-line
	// ParseErrors content renders with line breaks instead of collapsing
	// to a single jumble.
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	html := buf.String()
	// Look for `.outcome.danger{...white-space:pre-line...}` (commas/order may vary)
	idx := strings.Index(html, ".outcome.danger")
	if idx < 0 {
		t.Fatal(".outcome.danger CSS rule not found")
	}
	// Read up to 200 chars after the selector
	end := idx + 200
	if end > len(html) {
		end = len(html)
	}
	region := html[idx:end]
	if !strings.Contains(region, "white-space:pre-line") && !strings.Contains(region, "white-space: pre-line") {
		t.Errorf(".outcome.danger missing white-space:pre-line; rule region:\n%s", region)
	}
}

func TestProfileRibbon_EmptyClickable_RendersNoProfilesConfigured(t *testing.T) {
	// "file exists, profiles map is empty" → HasProfilesFile=true, profileCount=0.
	// profileDetail must return "no profiles configured" (web.go).
	sess := model.Session{} // no active profile
	ribbon := webRibbonFromSession(sess, "", true, 0)
	last := ribbon[4]
	if last.DataState != "inherit-env-clickable" {
		t.Fatalf("DataState = %q, want inherit-env-clickable", last.DataState)
	}
	if last.Detail != "no profiles configured" {
		t.Fatalf("Detail = %q, want \"no profiles configured\"", last.Detail)
	}
}

// captureRenderedDashboardHTML returns the full rendered dashboard HTML
// (template + always-on <svg> symbols + inline <style> + the gated
// {{if .LiveEnabled}} inline <script> body that builds the profile
// popover). LiveEnabled=true is required because the popover JS — which
// emits literal strings like 'inline'/'env_var', the
// password-input init, the reveal tooltip, the type-to-confirm modal
// body, and the " new profile" footer label — lives entirely inside that
// gated block. The markup-pin tests target both the always-on surface
// (SVG symbol bank + popover CSS) and the gated JS.
func captureRenderedDashboardHTML(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title:        "x",
		LiveEnabled:  true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	return buf.String()
}

// Markup pin tests for the profile popover. These are static substring
// matches: they won't catch JS interactivity bugs (click handlers, dirty
// detection) but they WILL catch accidental removal of a CSS class, an SVG
// symbol, a CSS keyframe name, or the key-source enum literals in the
// inline JS.

func TestWebMarkup_PopoverIconSymbols(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	for _, id := range []string{
		"i-back", "i-edit", "i-activity", "i-play", "i-check-circle",
		"i-trash", "i-eye", "i-eye-off", "i-check", "i-x", "i-plus",
	} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("symbol %q missing from rendered HTML", id)
		}
	}
}

func TestWebMarkup_RowActionColumn_HoverOnly(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, ".sp3-pop-row .pop-action") {
		t.Fatalf("pop-action CSS rule missing")
	}
	if !strings.Contains(body, "opacity:0") {
		t.Fatalf("opacity 0 default missing (looking for opacity:0)")
	}
	if !strings.Contains(body, ".sp3-pop-row:hover .pop-action") {
		t.Fatalf("hover reveal selector missing")
	}
	if !strings.Contains(body, ":focus-within .pop-action") {
		t.Fatalf("focus-within reveal selector missing (a11y)")
	}
}

func TestWebMarkup_DefaultStateAlwaysVisible(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "pop-action.is-default-state") {
		t.Fatalf("is-default-state class missing")
	}
	if !strings.Contains(body, "opacity:1") {
		t.Fatalf("is-default-state opacity:1 missing")
	}
}

func TestWebMarkup_EditPanelStructure(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "sp3-pop-edit-section") ||
		!strings.Contains(body, "sp3-pop-edit-section-label") ||
		!strings.Contains(body, "sp3-pop-edit-field") {
		t.Fatalf("edit section / label / field CSS classes missing")
	}
}

// TestWebMarkup_EditPanelInstrumentAligned pins the profile editor's visual
// alignment to the Instrument design system: emerald single-accent (no blue
// info-fill), the egress "via" row as a SELECT glued to the url input as a
// merged field (.pop-keyfield/.pop-keysrc-sel — not a button group),
// placeholder color, and sticky head/actions. (The key field is a
// progressive-disclosure control — see TestWebMarkup_KeySourceProgressiveDisclosure.)
// The alias sub-area is intentionally left in its current style.
func TestWebMarkup_EditPanelInstrumentAligned(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// emerald, not blue, for the primary action
	mustContainAllWeb(t, body,
		".sp3-pop-edit-actions button.primary{background:#0a2e1f;border-color:#1d3a30;color:#10b981}",
	)
	// the old blue info-fill must be gone from the editor
	for _, blue := range []string{"#1d3a5a", "#2d4d70", "#b8c8e0"} {
		if strings.Contains(body, blue) {
			t.Fatalf("editor still uses blue info-fill %s; must be emerald", blue)
		}
	}
	// key-source is a SELECT merged with the key input (design merged field),
	// NOT the old segmented button group.
	mustContainAllWeb(t, body,
		".pop-keyfield{display:flex;min-width:0}",
		".pop-keysrc-sel{",
	)
	if strings.Contains(body, ".pop-keysrc button + button") {
		t.Fatalf("key-source must be a select merged field, not a segmented button group")
	}
	// placeholder color + sticky head/actions. #808080 is a deliberate
	// deviation from the kit's #5a5a5a (2.7:1 on the field bg — below any
	// readable floor); the P3 contrast pass raised it (TestP3PolishContracts).
	mustContainAllWeb(t, body,
		".sp3-pop-edit-field input::placeholder{color:#808080}",
		"position:sticky;top:0",
		"position:sticky;bottom:0",
	)
	// egress is a merged "via [mode][url]" row: both data-fields still present
	// (collectEditPayload + dynamic placeholder + the zap test button depend on
	// them), label "via", glued in the same .pop-keyfield as the key field.
	mustContainAllWeb(t, body,
		"egress_mode", "egress_url", "'via'",
		"egressURLPlaceholder", "appendEgressTestButton",
	)
	// Egress ambiguity fix: the url input is DISABLED for non-proxy modes
	// (inherit/direct), so the user can't enter a url that test/save would
	// silently ignore. The mode-change handler toggles .disabled via the shared
	// egressIsProxyMode predicate, and the initial state is set too.
	mustContainAllWeb(t, body,
		"function egressIsProxyMode",
		"egUrl.disabled",
		"urlInput.disabled",
	)
}

func TestWebMarkup_AddButtonInFooter(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "sp3-pop-footer-add") {
		t.Fatalf("footer add class missing")
	}
	if !strings.Contains(body, "new profile") {
		t.Fatalf("'new profile' label missing")
	}
}

// TestWebMarkup_RmInlineConfirm pins the rm UX: in-row arm-then-confirm
// (no modal) + toast for feedback. Replaces the legacy type-to-confirm
// modal test.
func TestWebMarkup_RmInlineConfirm(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// Modal classes must be gone.
	if strings.Contains(body, "sp3-pop-modal") {
		t.Errorf("modal class sp3-pop-modal should be removed in favor of inline confirm")
	}
	if strings.Contains(body, "type-name") {
		t.Errorf("type-to-confirm input class should be removed")
	}
	// New armed-state styling for the rm icon.
	if !strings.Contains(body, "pop-action.danger.armed") {
		t.Errorf("inline-confirm armed style missing")
	}
	// Toast infrastructure for post-mutation feedback.
	if !strings.Contains(body, "ccwrap-toast") {
		t.Errorf("toast helper styling missing")
	}
	if !strings.Contains(body, "showToast") {
		t.Errorf("showToast helper function missing")
	}
}

// TestWebMarkup_ModelsCellLivePatchesDataState pins the fix:
// updateModelsCell must update both the value text AND the
// aliases-active data-state on SSE session_updated. Without the
// data-state update, the ribbon Models cell stays unclickable even
// after a chain-switch publishes new alias state.
func TestWebMarkup_ModelsCellLivePatchesDataState(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "cell.dataset.state = 'aliases-active'") {
		t.Errorf("updateModelsCell must set cell.dataset.state='aliases-active' on live patch")
	}
	if !strings.Contains(body, "v.textContent = n === 1 ? '1 alias' : n > 1 ? n + ' aliases' : 'default'") {
		t.Errorf("updateModelsCell must mirror the 'default' label for zero-count")
	}
}

// TestWebMarkup_AliasSerializeDispatchesInput pins the fix:
// serializeAliasHidden must dispatch an 'input' event after programmatic
// value sets, otherwise the form-level dirty detector (recomputeDirty)
// won't see deletions / clears — save button stays disabled.
func TestWebMarkup_AliasSerializeDispatchesInput(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "hiddenEl.dispatchEvent(new Event('input'") {
		t.Errorf("serializeAliasHidden must dispatch synthetic input event for dirty detection")
	}
}

// TestWebMarkup_ChainSwitchOnActiveEdit pins the chain-switch behavior:
// when an edit targets the live-active profile, the inline JS fires
// /profile/switch to the same name so session state picks up the new
// config (model_aliases, base_url, etc. → ribbon cells refresh).
func TestWebMarkup_ChainSwitchOnActiveEdit(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "maybeChainSwitchOnActiveEdit") {
		t.Errorf("maybeChainSwitchOnActiveEdit JS helper missing")
	}
	if !strings.Contains(body, "active_profile_name") {
		t.Errorf("chain-switch must compare against state.session.active_profile_name")
	}
}

// TestWebMarkup_AliasEditorInEditPanel pins the alias editor in the
// popover edit/add panel: a "Model aliases" section with add/delete row
// helpers + hidden serialized field for snapshot diffing.
func TestWebMarkup_AliasEditorInEditPanel(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	for _, s := range []string{
		"sp3-pop-aliases",          // section class
		"sp3-pop-alias-row",        // each row
		"sp3-pop-alias-add",        // + add alias button
		"model_aliases_serialized", // hidden snapshot field
		"appendAliasRow",           // JS helper
		"collectAliasesFromForm",   // JS helper
	} {
		if !strings.Contains(body, s) {
			t.Errorf("alias editor missing %q in inline JS/CSS", s)
		}
	}
}

// TestWebMarkup_ModelsCellDefaultLabelAndPopover pins the Models cell UX:
// zero-alias state renders "default" (not the legacy em-dash); non-zero
// state gets data-state="aliases-active" and a caret affordance; a click
// handler opens .sp3-models-pop reading from
// state.session.model_alias_forward.
func TestWebMarkup_ModelsCellDefaultLabelAndPopover(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "sp3-models-pop") {
		t.Errorf(".sp3-models-pop popover styling missing")
	}
	if !strings.Contains(body, "aliases-active") {
		t.Errorf("aliases-active data-state class missing")
	}
	if !strings.Contains(body, "attachModelsCellHandlers") {
		t.Errorf("attachModelsCellHandlers JS missing")
	}
	if !strings.Contains(body, "model_alias_forward") {
		t.Errorf("model_alias_forward wire field reference missing in inline JS")
	}
}

// TestWebMarkup_StreamPillAlwaysShown pins the behavior: the SSE state pill in
// the app-bar top-right is ALWAYS visible (per the design system's persistent
// connection pill with a glowing dot), including data-state="connected". The
// earlier "hide when connected" rule was dropped on user request 2026-06-01.
func TestWebMarkup_StreamPillAlwaysShown(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if strings.Contains(body, ".app-stream-pill[data-state=\"connected\"]{display:none}") {
		t.Errorf("connected-state stream pill must NOT be hidden — pill is always shown now")
	}
	// The pill must exist in the DOM (JS drives data-state across all states).
	if !strings.Contains(body, "app-stream-pill") {
		t.Errorf("app-stream-pill class missing — JS still drives data-state for all states")
	}
}

// TestWebMarkup_KeySourceProgressiveDisclosure pins design B's key-source
// control: there is NO source <select>. The DEFAULT state is a plain "key"
// input (paste the secret); a quiet .pop-keysrc-toggle switches the SOURCE to
// an environment variable via applyKeySourceMode, which retargets the same
// field to a variable NAME (clear text, eye hidden). The current source lives
// in a HIDDEN [data-field=key_source] input ('inline'|'env_var') that the
// submit / readiness / snapshot logic reads unchanged. The old
// "unchanged"/"keep current" option stays gone; a blank key in edit mode
// preserves the stored secret (backend handle_profile_edit.go no-op).
func TestWebMarkup_KeySourceProgressiveDisclosure(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// Wire values for the hidden source field + downstream comparisons.
	if !strings.Contains(body, "'inline'") || !strings.Contains(body, "'env_var'") {
		t.Fatalf("key source enum values inline/env_var missing")
	}
	// The removed third option must not reappear in the key-source control.
	if strings.Contains(body, "'unchanged'") || strings.Contains(body, "keep current") {
		t.Fatalf("key-source 'unchanged'/'keep current' must be removed (blank key = keep)")
	}
	// Progressive-disclosure pieces: the single-source-of-truth helper, the
	// quiet toggle, its copy, the env-var example placeholder, and the hidden
	// source field (a hidden input, NOT a select).
	mustContainAllWeb(t, body,
		"function applyKeySourceMode",
		"pop-keysrc-toggle",
		"use an environment variable",
		"env var name (e.g. OPENAI_API_KEY)",
		"'data-field', 'key_source'",
	)
	// Credential invariant: edit-mode submit only sends key when the input is
	// non-empty, so a blank edit preserves the stored secret. Pin the guard.
	if !strings.Contains(body, "auth_key") {
		t.Fatalf("auth_key field reference missing")
	}
	if !regexp.MustCompile(`keyVal\b`).MatchString(body) {
		t.Fatalf("collectEditPayload must read a keyVal local to guard blank-key-preserve")
	}
}

func TestWebMarkup_KeyInputTypePassword(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// Design B: the key field masks the secret by default — applyKeySourceMode
	// sets input.type to 'password' for the inline (key) source and 'text' for
	// env_var (a variable NAME isn't secret, so it's shown in clear text). The
	// reveal eye flips password<->text in place. Pin both the source-driven
	// masking and the reveal tooltip.
	mustContainAllWeb(t, body,
		"els.input.type = isEnv ? 'text' : 'password'",
		"input.type = nextHidden ? 'password' : 'text'",
		"show key value",
	)
}

// TestSP3InlineScript_KeySourceMode runs the ACTUAL rendered applyKeySourceMode
// helper — design B's single source of truth for the key field's source-
// dependent state. The control has NO source <select>: the current source
// lives in a hidden [data-field=key_source] input ('inline'|'env_var') and a
// quiet progressive-disclosure toggle flips it. For each (source, mode) the
// helper sets label / placeholder / input.type / hidden.value / reveal.hidden /
// toggle copy. env_var shows the variable NAME in clear text (type=text, eye
// hidden) since a name isn't a secret; inline masks the secret (type=password,
// eye shown). Toggling back to inline re-masks. The helper is pure w.r.t. the
// passed els object (no DOM lookups), so it runs in node against plain objects.
func TestSP3InlineScript_KeySourceMode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	body := scriptFnBody(t, m[1], "applyKeySourceMode")
	driver := "function applyKeySourceMode(els, source, mode)" + body + `
function fresh(){ return {label:{}, input:{}, hidden:{}, reveal:{}, toggle:{}}; }
function snap(e){ return {label:e.label.textContent, ph:e.input.placeholder, type:e.input.type, src:e.hidden.value, eyeHidden:!!e.reveal.hidden, toggle:e.toggle.textContent}; }
var out = {};
var e = fresh(); applyKeySourceMode(e,'inline','edit'); out.editInline = snap(e);
applyKeySourceMode(e,'env_var','edit'); out.editEnv = snap(e);
applyKeySourceMode(e,'inline','edit'); out.editBack = snap(e);
var a = fresh(); applyKeySourceMode(a,'inline','add'); out.addInline = snap(a);
applyKeySourceMode(a,'env_var','add'); out.addEnv = snap(a);
process.stdout.write(JSON.stringify(out));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	var res map[string]struct {
		Label     string `json:"label"`
		PH        string `json:"ph"`
		Type      string `json:"type"`
		Src       string `json:"src"`
		EyeHidden bool   `json:"eyeHidden"`
		Toggle    string `json:"toggle"`
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("parse node output: %v\nraw: %s", jerr, out)
	}
	// edit + inline: masked secret, eye shown, label "key", no required star.
	if c := res["editInline"]; c.Type != "password" || c.Src != "inline" || c.Label != "key" || c.EyeHidden || c.PH != "blank keeps the stored key" {
		t.Errorf("editInline wrong: %+v", c)
	}
	// edit + env_var: clear-text var name, eye hidden, label "env var".
	if c := res["editEnv"]; c.Type != "text" || c.Src != "env_var" || c.Label != "env var" || !c.EyeHidden || !strings.Contains(c.PH, "env var name") {
		t.Errorf("editEnv wrong: %+v", c)
	}
	// toggling back restores masked key mode (re-mask is part of the helper).
	if c := res["editBack"]; c.Type != "password" || c.Src != "inline" || c.EyeHidden {
		t.Errorf("editBack did not restore key mode: %+v", c)
	}
	// add mode: required star on the label, paste-secret / env-name-example copy.
	if c := res["addInline"]; c.Label != "key *" || c.PH != "paste secret here" {
		t.Errorf("addInline wrong: %+v", c)
	}
	if c := res["addEnv"]; c.Label != "env var *" || !strings.Contains(c.PH, "OPENAI_API_KEY") {
		t.Errorf("addEnv wrong: %+v", c)
	}
}

// TestWebMarkup_EditFieldFocusRing pins the emerald focus treatment on the
// profile editor's inputs/selects. With no custom :focus rule the browser
// paints its native (macOS blue) focus ring — jarring in the all-emerald
// Instrument UI and immediately visible because the panel auto-focuses a field
// on open. The rule must recolor focus to the single accent for every editor
// control: identity/auth/egress fields, their selects, and the alias rows.
func TestWebMarkup_EditFieldFocusRing(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	mustContainAllWeb(t, body,
		".sp3-pop-edit-field input:focus",
		".sp3-pop-edit-field select:focus",
		".sp3-pop-alias-row input:focus",
	)
	focusRule := regexp.MustCompile(`\.sp3-pop-edit-field input:focus[^}]*\{[^}]*\}`).FindString(body)
	if focusRule == "" {
		t.Fatalf("no .sp3-pop-edit-field input:focus rule found")
	}
	if !strings.Contains(focusRule, "var(--accent)") {
		t.Fatalf("focus must use var(--accent) (no native blue ring), got: %s", focusRule)
	}
}

// TestWebMarkup_DisabledFieldMuted pins the disabled-input treatment. The
// egress url is disabled for non-proxy modes (inherit/direct); with no
// :disabled rule it renders identically to an enabled field (full-white text,
// opacity 1) — an empty box that silently rejects input with no visual cue.
// Mute it (parity with the kit's .iv-keyfield input:disabled).
func TestWebMarkup_DisabledFieldMuted(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	rule := regexp.MustCompile(`\.sp3-pop-edit-field input:disabled[^}]*\{[^}]*\}`).FindString(body)
	if rule == "" {
		t.Fatalf("no .sp3-pop-edit-field input:disabled rule found")
	}
	if !strings.Contains(rule, "var(--text-muted)") || !strings.Contains(rule, "opacity:") {
		t.Fatalf("disabled rule must mute color + reduce opacity, got: %s", rule)
	}
}

// TestWebMarkup_EgressModeHintAndGate pins the egress clarity pass: a muted
// .egress-mode-hint line (driven by egressModeHint) makes "inherit" self-
// explanatory, and the save path gates on egressFieldValid/egressRowValid so a
// proxy mode with no url can't be persisted. Behavioral coverage of the helper
// outputs lives in TestSP3InlineScript_EgressModeHintAndValidity.
func TestWebMarkup_EgressModeHintAndGate(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	mustContainAllWeb(t, body,
		".egress-mode-hint{",        // hint CSS
		"function egressModeHint",   // per-mode hint text
		"function egressRowValid",   // proxy-requires-url predicate
		"function egressFieldValid", // form-level save gate
		"egress-mode-hint",          // hint element wired in the egress builder
	)
	// The save gate must apply in BOTH add (isAddFormReady) and edit
	// (recomputeDirty) paths.
	if !strings.Contains(body, "!egressFieldValid(form)") {
		t.Fatalf("isAddFormReady must gate on egressFieldValid")
	}
	if !strings.Contains(body, "!dirty || !egressFieldValid(form)") {
		t.Fatalf("edit save gate must include egressFieldValid")
	}
}

// TestSP3InlineScript_AuthModeHint runs the ACTUAL rendered authModeHint helper.
// The auth_mode select keeps terse provider-neutral labels (Bearer token /
// x-api-key); this hint names the exact header the credential rides in AND the
// equivalent env-var slot (familiar to Claude Code users) — clearer than the
// raw ccwrap_bearer/ccwrap_x_api_key wire values without claiming Anthropic.
func TestSP3InlineScript_AuthModeHint(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	js := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(js) != 2 {
		t.Fatalf("inline script not found")
	}
	driver := "function authModeHint(mode)" + scriptFnBody(t, js[1], "authModeHint") + `
var out = { bearer: authModeHint('ccwrap_bearer'), xapi: authModeHint('ccwrap_x_api_key'),
  pass: authModeHint('passthrough'), blank: authModeHint('') };
process.stdout.write(JSON.stringify(out));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	var res struct {
		Bearer, Xapi, Pass, Blank string
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("parse node output: %v\nraw: %s", jerr, out)
	}
	// bearer: names the Authorization: Bearer header + its env-var slot.
	if !strings.Contains(res.Bearer, "Authorization: Bearer") || !strings.Contains(res.Bearer, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("bearer hint must name the header + env var, got %q", res.Bearer)
	}
	// x_api_key: names the x-api-key header + its env-var slot.
	if !strings.Contains(res.Xapi, "x-api-key") || !strings.Contains(res.Xapi, "ANTHROPIC_API_KEY") {
		t.Errorf("x_api_key hint must name the header + env var, got %q", res.Xapi)
	}
	// passthrough: explains no injection.
	if !strings.Contains(res.Pass, "no injection") {
		t.Errorf("passthrough hint wrong: %q", res.Pass)
	}
	if res.Blank != "" {
		t.Errorf("unknown-mode hint must be empty, got %q", res.Blank)
	}
}

// TestWebMarkup_AuthModeLabelsAndHint pins the auth-mode label clarity: the
// select keeps the wire values (passthrough/ccwrap_bearer/ccwrap_x_api_key) but
// DISPLAYS provider-neutral labels (Bearer token / x-api-key), with a muted
// .auth-mode-hint line driven by authModeHint.
func TestWebMarkup_AuthModeLabelsAndHint(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// wire values unchanged (collectEditPayload/isAddFormReady read these).
	mustContainAllWeb(t, body, "'passthrough', 'ccwrap_bearer', 'ccwrap_x_api_key'")
	// friendly display labels passed as the appendSelect labels array.
	mustContainAllWeb(t, body, "'Bearer token', 'x-api-key'")
	// hint helper + element + CSS.
	mustContainAllWeb(t, body,
		"function authModeHint",
		".auth-mode-hint{",
		"auth-mode-hint",
	)
}

// cssMediaBlock returns the balanced body of the FIRST
// `@media (max-width:<w>){ ... }` block (e.g. w="820px"), so a test can assert
// what rules live in a SPECIFIC breakpoint rather than anywhere in the CSS.
func cssMediaBlock(css, w string) string {
	marker := "@media (max-width:" + w + "){"
	i := strings.Index(css, marker)
	if i < 0 {
		return ""
	}
	j := i + len(marker)
	depth := 1
	for k := j; k < len(css); k++ {
		switch css[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[j:k]
			}
		}
	}
	return css[j:]
}

// TestWebMarkup_ResponsiveBreakpoints pins the responsive fixes:
//   - #2: the ribbon collapses to a clean 2-col card grid below 1024 (the old
//     3-col stage stranded cells because 7 cells + a full-width Profile don't
//     tile into 3 columns).
//   - #1: activity rows STACK at <=820 (correct summary/.row.nf selectors), not
//     only at <=600 — closing the 600-820 band where cell-right was clipped.
//   - #3: the app-bar stacks LEFT-aligned only at <=600; the premature, centered
//     <=820 topbar column (which floated the brand/connected pill centered) is
//     gone.
func TestWebMarkup_ResponsiveBreakpoints(t *testing.T) {
	body := captureRenderedDashboardHTML(t)

	// #2 — ribbon 2-col, and no leftover 3-col stage anywhere.
	b1024 := cssMediaBlock(body, "1024px")
	if !strings.Contains(b1024, ".ribbon{grid-template-columns:repeat(2,1fr)") {
		t.Fatalf("ribbon must collapse to 2 columns at <=1024, got block: %s", b1024)
	}
	if strings.Contains(body, "grid-template-columns:repeat(3,1fr)") {
		t.Fatalf("the ragged 3-col ribbon stage must be removed")
	}
	// #4 — the 8-col native-tls :has() rule (specificity 0,3,0) MUST be overridden
	// INSIDE the narrow breakpoints, else it beats the plain .ribbon{} media rule
	// (0,1,0) and forces repeat(8,1fr) at all widths (cells collapse to one char
	// per line on mobile). Same-specificity override in a max-width block wins.
	if !strings.Contains(b1024, `.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]){grid-template-columns:repeat(2,1fr)`) {
		t.Fatalf("the 8-col :has() ribbon rule must be overridden to 2-col at <=1024, got: %s", b1024)
	}

	// #1 — rows stack at <=820 using the selectors that actually target the
	// grid container (the <summary> / .row.nf), not the inert `.row` shorthand.
	b820 := cssMediaBlock(body, "820px")
	if !strings.Contains(b820, ".rows details.row>summary,.rows .row.nf{grid-template-columns:1fr") {
		t.Fatalf("activity rows must stack at <=820 (600-820 clip band), got block: %s", b820)
	}
	if strings.Contains(b820, ".topbar{flex-direction:column}") {
		t.Fatalf("topbar must NOT stack at <=820 — that is what centered the brand/pill")
	}

	// #3 — topbar stacks left-aligned at <=600 only.
	b600 := cssMediaBlock(body, "600px")
	if !strings.Contains(b600, ".topbar{flex-direction:column;align-items:flex-start") {
		t.Fatalf("topbar must stack left-aligned (not centered) at <=600, got block: %s", b600)
	}
	// The ribbon stays 2-col down to mobile — short status values don't need full
	// width, and 1-col stacks ~8 tall cards before Activity. The <=1024 :has()
	// override (repeat(2,1fr)) cascades to <=600; <=600 must NOT re-collapse to
	// 1fr, and uses a tighter cell min-height.
	if strings.Contains(b600, ".ribbon{grid-template-columns:1fr") ||
		strings.Contains(b600, `.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]){grid-template-columns:1fr`) {
		t.Fatalf("ribbon must NOT collapse to 1 column at <=600 (stays 2-col), got: %s", b600)
	}
	if !strings.Contains(b600, ".ribbon-cell{min-height:56px") {
		t.Fatalf("ribbon cells must use a tighter min-height at <=600, got: %s", b600)
	}
}

// TestDefaultActivityClass locks the Activity-tab default: open on "forwarded-api"
// only when there IS api traffic, else "all" — a session whose only activity is
// trace/errors must not open on an empty "Forwarded API" tab and read as
// "No recent traffic" while other tabs have rows.
func TestDefaultActivityClass(t *testing.T) {
	if g := defaultActivityClass(0); g != "all" {
		t.Errorf("0 forwarded-api -> %q, want all", g)
	}
	if g := defaultActivityClass(3); g != "forwarded-api" {
		t.Errorf("3 forwarded-api -> %q, want forwarded-api", g)
	}
}

// TestWebMarkup_RibbonAndPillPolish pins two layout fixes:
//   - EGRESS ribbon value WRAPS to show the full egress URL (truncation hid it
//     behind hover-only). overflow-wrap:anywhere wraps a long proxy URL cleanly
//     at any char; a smaller mono size keeps it compact. The value's title is
//     still set so the full URL is also recoverable on hover.
//   - The connecting/reconnecting connection pill is amber (--warn, transitional),
//     not emerald — emerald (connected's color) made a dropped-and-retrying stream
//     look healthy. The state-dot glow follows the pill color (currentColor).
func TestWebMarkup_RibbonAndPillPolish(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// #1 — EGRESS value WRAPS (shows the full URL) instead of nowrap+ellipsis.
	if !strings.Contains(body, `.ribbon-cell[data-ribbon="Egress"] .v{white-space:normal;overflow-wrap:anywhere;`) {
		t.Fatalf("EGRESS value must wrap (white-space:normal;overflow-wrap:anywhere) to show the full URL, not truncate")
	}
	if strings.Contains(body, `.ribbon-cell[data-ribbon="Egress"] .v{white-space:nowrap`) {
		t.Fatalf("EGRESS value must no longer be nowrap/ellipsis (it hid the full URL)")
	}
	if !strings.Contains(body, "v.title = value") {
		t.Fatalf("updateEgressCell must set the value title (hover-to-see-full when ellipsised)")
	}
	// #2 — connecting/reconnecting pill amber, old emerald rule gone.
	if !strings.Contains(body, `.state-pill[data-state="connecting"],.state-pill[data-state="reconnecting"]{color:var(--warn)}`) {
		t.Fatalf("connecting/reconnecting pill must be amber (--warn), not emerald")
	}
	if strings.Contains(body, `.state-pill[data-state="connecting"],.state-pill[data-state="reconnecting"]{color:var(--accent)}`) {
		t.Fatalf("the old emerald connecting/reconnecting pill rule must be removed")
	}
	// state-dot glow follows the state color rather than a hardcoded emerald.
	if !strings.Contains(body, "box-shadow:0 0 12px currentColor") {
		t.Fatalf("state-dot glow must use currentColor so it matches each pill state")
	}
}

func TestWebMarkup_SuccessFlashOutline_300ms(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	if !strings.Contains(body, "@keyframes sp3-flash-green") {
		t.Fatalf("flash-green keyframes missing")
	}
	if !strings.Contains(body, "flash-success") {
		t.Fatalf("flash-success class missing")
	}
}

func TestWebMarkup_BodyDrawer_DualViewToggle(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// Inline JS — toggle rendering + dual-pane visibility logic.
	if !strings.Contains(body, "renderBodyDrawerDualView") {
		t.Fatalf("renderBodyDrawerDualView function missing")
	}
	if !strings.Contains(body, "'received'") || !strings.Contains(body, "'forwarded'") {
		t.Fatalf("toggle label strings missing (received / forwarded)")
	}
	if !strings.Contains(body, "data-upstream-reqid") {
		t.Fatalf("inline JS must read data-upstream-reqid")
	}
	if !strings.Contains(body, "after alias + strip") {
		t.Fatalf("hint label explaining the transformation must be present")
	}
	// CSS — pane visibility via .on class.
	if !strings.Contains(body, ".body-view-pane{display:none}") {
		t.Fatalf("body-view-pane base display:none rule missing")
	}
	if !strings.Contains(body, ".body-view-pane.on{display:block}") {
		t.Fatalf("body-view-pane.on display:block rule missing")
	}
	// SSE mirror — patchSession-built rows must also propagate upstream id.
	if !strings.Contains(body, "rec.upstream_body_ref") {
		t.Fatalf("SSE row builder must copy rec.upstream_body_ref.id onto the JS row")
	}
}

func TestWebMarkup_NeedsRelaunchBannerStructured(t *testing.T) {
	body := captureRenderedDashboardHTML(t)
	// Structured banner: headline + reassurance + click-to-copy $ command.
	if !strings.Contains(body, "needs first-party auth") {
		t.Fatalf("structured banner headline copy missing")
	}
	if !strings.Contains(body, "Active profile remains ") {
		t.Fatalf("reassurance line missing — user must see which profile stays active")
	}
	if !strings.Contains(body, "'$ ccwrap --profile '") {
		t.Fatalf("$ ccwrap --profile command construction missing from inline JS")
	}
	if !strings.Contains(body, "user-select:all") {
		t.Fatalf(".cmd must be user-select:all so a single click selects the whole command for copy")
	}
	// The previous flat textContent path must be gone.
	if strings.Contains(body, "switch requires Claude relaunch") {
		t.Fatalf("legacy single-line fallback text still present; refused_needs_relaunch render should be structured now")
	}
}

// TestSP3InlineScript_EgressCell_PatchedOnSSE — the SSE session_updated
// patcher (patchSession) repainted Traffic/Profile/Route/Auth/Models/Bodies
// but NOT the Egress cell. publishPosture refreshes
// egress_mode/egress_source/egress_summary and the session_updated event
// carries them, but the Egress ribbon cell kept showing stale exit info
// after a live profile switch — a routing/security tool misreporting where
// traffic exits until a full reload. patchSession must call an Egress-cell
// updater that reads the egress_* fields. (The regression is a literal
// missing call — substring-catchable, same as the sibling Auth/Bodies cell
// updaters; see TestSP3InlineScript_AuthCell_HandlesMissing.)
func TestSP3InlineScript_EgressCell_PatchedOnSSE(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]

	// patchSession must invoke the egress-cell updater.
	patchBody := scriptFnBody(t, js, "patchSession")
	if !strings.Contains(patchBody, "updateEgressCell()") {
		t.Errorf("patchSession must call updateEgressCell() so the Egress cell repaints on SSE session_updated; body=\n%s", patchBody)
	}

	// updateEgressCell must read the egress posture fields the SSE event carries.
	egressBody := scriptFnBody(t, js, "updateEgressCell")
	for _, field := range []string{"egress_mode", "egress_summary", "egress_source"} {
		if !strings.Contains(egressBody, field) {
			t.Errorf("updateEgressCell must read session.%s", field)
		}
	}
	// It must locate the Egress ribbon cell and write the value element.
	if !strings.Contains(egressBody, `'Egress'`) && !strings.Contains(egressBody, `"Egress"`) {
		t.Errorf("updateEgressCell must target the Egress ribbon cell")
	}
	if !strings.Contains(egressBody, "data-ribbon-value") && !strings.Contains(egressBody, "ribbonValueEl") {
		t.Errorf("updateEgressCell must write the ribbon value element")
	}
}

// TestSP3InlineScript_EgressOverrideForTest_ClearsURLForNonURLModes — the
// popover [test] button built its egress_override from the
// raw form values, sending urlInput.value even after the user switched mode
// to inherit/direct (the mode-change listener only updates the placeholder,
// never clears the value). The probe then sent {mode:inherit, url:http://…},
// which ValidateEgressSpec rejects with 422 — a confusing error for a draft
// that [save] would accept (save clears the URL for non-url-bearing modes).
// egressOverrideForTest must null the URL for inherit/direct/none so [test]
// and [save] agree on the same form state. Runs the ACTUAL rendered helper
// via node (pure function, no DOM needed).
func TestSP3InlineScript_EgressOverrideForTest_ClearsURLForNonURLModes(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	js := m[1]
	body := scriptFnBody(t, js, "egressOverrideForTest")
	driver := "function egressOverrideForTest(mode, url)" + body + `
var cases = [
  {mode:'inherit', url:'http://p:8080', wantURL:''},
  {mode:'direct',  url:'http://p:8080', wantURL:''},
  {mode:'none',    url:'http://p:8080', wantURL:''},
  {mode:'http',    url:'http://p:8080', wantURL:'http://p:8080'},
  {mode:'socks5',  url:'socks5://p:1080', wantURL:'socks5://p:1080'},
  {mode:'socks5h', url:'socks5h://p:1080', wantURL:'socks5h://p:1080'},
];
var out = cases.map(function(c){ var o = egressOverrideForTest(c.mode, c.url); return {mode:o.mode, url:o.url, wantURL:c.wantURL}; });
process.stdout.write(JSON.stringify(out));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	var res []struct {
		Mode    string `json:"mode"`
		URL     string `json:"url"`
		WantURL string `json:"wantURL"`
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("parse node output: %v\nraw: %s", jerr, out)
	}
	for _, r := range res {
		if r.URL != r.WantURL {
			t.Errorf("egressOverrideForTest(%q, ...) url = %q, want %q", r.Mode, r.URL, r.WantURL)
		}
	}
}

func TestInlineScriptUpdatesHeroOnSSE(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "CCWRAP Session", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	body := buf.String()
	mustContainAllWeb(t, body,
		"function updateHeroState", "updateHeroState(",
		"session_health", "getElementById('hero-state')", "setAttribute('data-variant'",
	)
}

// TestInlineScriptPatchActivityUpdatesHero pins the review-gap fix: the
// normal request/error SSE flow (routed to patchActivity) must repaint the
// hero live by updating state.session.session_health and calling
// updateHeroState — mirroring backend Health so the live hero matches a
// page reload. The error branch keys off rec.severity ('warn' vs 'error').
func TestInlineScriptPatchActivityUpdatesHero(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "CCWRAP Session", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	body := buf.String()
	mustContainAllWeb(t, body,
		"state.session.session_health",
		"updateHeroState(state.session)",
		"rec.severity",
	)
}

func TestWebTopbarBrandAndOverflow(t *testing.T) {
	page := webPageData{
		Title: "CCWRAP Session", Heading: "CCWRAP Session",
		Subtitle: "demo", SessionLabel: "session a3f17c2e",
		Links: []webLink{{Label: "Health JSON", Href: "/healthz"}, {Label: "Recent JSON", Href: "/recent"}},
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, page)
	html := buf.String()
	mustContainAllWeb(t, html,
		`class="brandmark"`, `class="wordmark"`, "CCWRAP",
		"session a3f17c2e",
		`id="ovf-btn"`, `id="ovf-menu"`, `href="#i-more"`,
		`href="/healthz"`, `href="/recent"`,
		"Health JSON", "Recent JSON",
	)
}

func TestLatestClaudeSessionID(t *testing.T) {
	full := "f524c250-4d22-49c2-a0d8-c30f2a7e67c4"
	older := "00000000-1111-2222-3333-444444444444"
	recs := []model.RequestRecord{
		{RequestHeaders: http.Header{claudeSessionHeader: {older}}},
		{RequestHeaders: http.Header{"Anthropic-Version": {"2023-06-01"}}}, // no header
		{RequestHeaders: http.Header{claudeSessionHeader: {full}}},         // newest carrying it
		{RequestHeaders: http.Header{"User-Agent": {"x"}}},                 // newest overall, no header
	}
	if got := latestClaudeSessionID(recs); got != full {
		t.Fatalf("latestClaudeSessionID = %q, want %q", got, full)
	}
	if got := latestClaudeSessionID(nil); got != "" {
		t.Fatalf("nil input: got %q, want empty", got)
	}
	if got := latestClaudeSessionID([]model.RequestRecord{{RequestHeaders: http.Header{"X": {"y"}}}}); got != "" {
		t.Fatalf("no header: got %q, want empty", got)
	}
	// nil header map is safe (http.Header.Get is nil-safe).
	if got := latestClaudeSessionID([]model.RequestRecord{{}}); got != "" {
		t.Fatalf("nil header: got %q, want empty", got)
	}
}

func TestBootstrapCarriesClaudeSessionHeader(t *testing.T) {
	b64 := bootstrapB64(pageBootstrap{EventsURL: "/events", ClaudeSessionHeader: claudeSessionHeader})
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	var got struct {
		ClaudeSessionHeader string `json:"claude_session_header"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	if got.ClaudeSessionHeader != "X-Claude-Code-Session-Id" {
		t.Fatalf("bootstrap claude_session_header = %q, want X-Claude-Code-Session-Id", got.ClaudeSessionHeader)
	}
}

func TestWebBrandbarClaudeSessionChip(t *testing.T) {
	full := "f524c250-4d22-49c2-a0d8-c30f2a7e67c4"

	// Present: chip shows the short label and carries full UUID + a11y/copy attrs.
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", SessionLabel: "session f524c250", ClaudeSessionFull: full,
	})
	html := buf.String()
	mustContainAllWeb(t, html,
		`id="claude-sess-vr"`,
		`id="claude-sess-label"`,
		"session f524c250",
		`title="`+full+`"`,
		`aria-label="Claude Code session `+full+`"`,
		`data-full="`+full+`"`,
		`role="button"`,
	)

	// Absent: both spans render but hidden, with no copy/a11y attrs.
	var buf2 bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf2}, webPageData{Title: "x"})
	html2 := buf2.String()
	mustContainAllWeb(t, html2,
		`id="claude-sess-vr" hidden`,
		`id="claude-sess-label" hidden`,
	)
	if strings.Contains(html2, "data-full=") {
		t.Fatalf("hidden chip must not carry data-full")
	}
}

func TestWebClaudeSessionLiveWiring(t *testing.T) {
	body := renderTestPage(t, webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Activity",
		BootstrapB64: bootstrapB64(pageBootstrap{
			EventsURL: "/events", ClaudeSessionHeader: claudeSessionHeader,
			HeaderDenyList: ui.CredentialDenyList(),
		}),
	})
	mustContainAllWeb(t, body,
		"function updateClaudeSession(",
		"boot.claude_session_header",
		"updateClaudeSession(p.data)",
		"getElementById('claude-sess-label')",
	)
	// Faithfulness: the live-patch must never use innerHTML (third-party value).
	fn := reconstructFn(t, inlineScript(t), "updateClaudeSession")
	if strings.Contains(fn, "innerHTML") {
		t.Fatalf("updateClaudeSession must use textContent/setAttribute only, never innerHTML")
	}
	// The whole rendered inline script must still parse under node.
	if _, err := exec.LookPath("node"); err == nil {
		cmd := exec.Command("node", "--check", "-")
		cmd.Stdin = strings.NewReader(inlineScript(t))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inline script failed node --check: %v\n%s", err, out)
		}
	}
}

func TestWebClaudeSessionCopyWiring(t *testing.T) {
	body := renderTestPage(t, webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Activity",
		BootstrapB64: bootstrapB64(pageBootstrap{
			EventsURL: "/events", ClaudeSessionHeader: claudeSessionHeader,
			HeaderDenyList: ui.CredentialDenyList(),
		}),
	})
	mustContainAllWeb(t, body,
		"document.execCommand('copy')",
		"createElement('textarea')",
		"getAttribute('data-full')",
		"addEventListener('click'",
	)
	// Honor the locked plain-HTTP doctrine (TestNativeTLSPopover_Contracts):
	// no navigator.clipboard anywhere in the rendered page.
	if strings.Contains(body, "navigator.clipboard") {
		t.Fatalf("must use execCommand select-to-copy, not navigator.clipboard (plain-HTTP tunnel safety)")
	}
}

func TestUpdateClaudeSessionBehavioral(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	fn := reconstructFn(t, inlineScript(t), "updateClaudeSession")
	driver := `
function FakeEl(a){ this._attr = a || {}; this.textContent = ''; }
FakeEl.prototype.getAttribute = function(k){ return (k in this._attr) ? this._attr[k] : null; };
FakeEl.prototype.setAttribute = function(k,v){ this._attr[k] = String(v); };
FakeEl.prototype.hasAttribute = function(k){ return (k in this._attr); };
FakeEl.prototype.removeAttribute = function(k){ delete this._attr[k]; };
var label = new FakeEl({ hidden: '' }), vr = new FakeEl({ hidden: '' });
var document = { getElementById: function(id){ return id === 'claude-sess-label' ? label : id === 'claude-sess-vr' ? vr : null; } };
var boot = { claude_session_header: 'X-Claude-Code-Session-Id' };
updateClaudeSession({ request_headers: { 'X-Claude-Code-Session-Id': ['f524c250-4d22-49c2-a0d8-c30f2a7e67c4'] } });
var ok1 = label.textContent === 'session f524c250'
  && label.getAttribute('title') === 'f524c250-4d22-49c2-a0d8-c30f2a7e67c4'
  && label.getAttribute('data-full') === 'f524c250-4d22-49c2-a0d8-c30f2a7e67c4'
  && !label.hasAttribute('hidden') && !vr.hasAttribute('hidden');
updateClaudeSession({ request_headers: { 'X-Claude-Code-Session-Id': ['9a01bb7e-0000-0000-0000-000000000000'] } });
var ok2 = label.textContent === 'session 9a01bb7e';
updateClaudeSession({ request_headers: { 'User-Agent': ['x'] } });
var ok3 = label.textContent === 'session 9a01bb7e';
console.log((ok1 && ok2 && ok3) ? 'PASS' : ('FAIL text=' + label.textContent + ' title=' + label.getAttribute('title')));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(fn + "\n" + driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("behavioral assertion failed: %s", out)
	}
}

func TestWebBrandbarChipFocusAffordance(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", SessionLabel: "session f524c250",
		ClaudeSessionFull: "f524c250-4d22-49c2-a0d8-c30f2a7e67c4",
	})
	html := buf.String()
	mustContainAllWeb(t, html,
		".sesslabel[data-full]",
		".sesslabel[data-full]:focus-visible",
	)
	if !strings.Contains(html, ".sesslabel[data-full]{cursor:pointer}") {
		t.Fatalf("interactive chip must show a pointer cursor")
	}
}

// TestWebRetainMirrorsServerCaps guards the RETAIN/server-cap lockstep: the JS
// activity-state arrays mirror the server retention rings, so refreshCounts
// (which counts state-array lengths) matches the server-rendered filter counts
// on load and SSE patches stay bounded. If a server cap changes without
// updating the JS RETAIN literal, counts and growth drift silently.
func TestWebRetainMirrorsServerCaps(t *testing.T) {
	js := inlineScript(t)
	want := fmt.Sprintf("var RETAIN = { requests: %d, errors: %d, trace: %d };",
		maxSessionRequests, maxSessionErrors, maxSessionTrace)
	if !strings.Contains(js, want) {
		t.Errorf("web RETAIN must mirror server retention caps; want substring:\n  %s", want)
	}
}

// TestPatchActivityHealGatedOnNativeTLS guards the health fake-heal fix: a
// successful request must NOT heal Health to ok while native-TLS is blocked
// (the JS twin of server recordRequest's !nativeTLSBlocked gate). Otherwise the
// hero shows Active while new connections are being fail-closed.
func TestPatchActivityHealGatedOnNativeTLS(t *testing.T) {
	js := inlineScript(t)
	if !strings.Contains(js, "state.session.native_tls !== 'blocked'") {
		t.Errorf("patchActivity must gate the health heal on native_tls !== 'blocked' (no fake-heal while blocked)")
	}
}

// TestResyncReplaysClaudeChip guards that the reconnect resync refreshes the
// brandbar Claude-session chip: the live request listener updates it per event,
// so resyncFromRecent must too, or an id that first appeared while disconnected
// stays hidden until the next live request.
func TestResyncReplaysClaudeChip(t *testing.T) {
	js := inlineScript(t)
	re := regexp.MustCompile(`(?s)async function resyncFromRecent\(\).*?updateClaudeSession\(`)
	if !re.MatchString(js) {
		t.Errorf("resyncFromRecent must call updateClaudeSession to refresh the chip on reconnect")
	}
}

// TestEndedPageHonestActivityControls pins the ended/non-live Activity
// surface: every control the live script powers either works without JS or
// is not rendered. (a) filter chips render as plain links carrying ?class=
// (the no-JS filter path; handleInfoPage filters server-side) — not dead
// <button>s; (b) body/response drawers are NOT rendered (the lazy-fetch
// toggle listener is live-only — a drawer here would sit on "loading…"
// forever); raw-JSON links to /recent/body?id= take their place; (c) the
// header sub-accordion stays (pure HTML disclosure, no JS needed).
func TestEndedPageHonestActivityControls(t *testing.T) {
	rec := model.RequestRecord{
		Timestamp:       time.Now(),
		Method:          "POST",
		Path:            "/v1/messages",
		RequestHeaders:  http.Header{"Anthropic-Version": {"2023-06-01"}},
		BodyRef:         &model.RequestBodyRef{ID: "aaaa000011112222"},
		UpstreamBodyRef: &model.RequestBodyRef{ID: "bbbb000011112222"},
		ResponseBodyRef: &model.RequestBodyRef{ID: "cccc000011112222"},
	}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 10, false, false)
	html := renderTestPage(t, webPageData{
		ActivityTitle: "Activity",
		LiveEnabled:   false,
		DefaultClass:  "forwarded-api",
		Classes:       []webClassCount{{"all", "All", 1}, {"forwarded-api", "Forwarded API", 1}},
		ActivityRows:  rows,
	})
	mustContainAllWeb(t, html,
		`<a class="filter-btn" data-filter="all" href="?class=all"`,
		`<a class="filter-btn on" data-filter="forwarded-api" href="?class=forwarded-api"`,
		`href="/recent/body?id=aaaa000011112222"`,
		`href="/recent/body?id=bbbb000011112222"`,
		`href="/recent/body?id=cccc000011112222"`,
		`<summary>request headers</summary>`,
	)
	if strings.Contains(html, `<button class="filter-btn`) {
		t.Fatalf("non-live page must not render dead filter <button>s")
	}
	for _, gone := range []string{`<details class="req-sub body-drawer"`, "loading…"} {
		if strings.Contains(html, gone) {
			t.Fatalf("non-live page must not render live-only drawers (%q present)", gone)
		}
	}
}

// TestUnifiedActivityRows_SwitchMarker_HumanRight pins the no-JS first
// paint of profile_switch rows: Right is the human renderSwitchMarker
// wording (the live decorator rebuilds from data-attrs, so live pages are
// unaffected), never the raw Detail JSON; unparseable Detail falls back.
func TestUnifiedActivityRows_SwitchMarker_HumanRight(t *testing.T) {
	mk := func(detail string) webRow {
		rows := unifiedActivityRows(nil, nil, []model.TraceRecord{{
			Timestamp: time.Now(), Category: "profile_switch", Summary: "switch", Detail: detail,
		}}, 5, false, false)
		return rows[0]
	}
	if got := mk(`{"from":"a","to":"b","class":"live"}`).Right; got != "switched [a] → [b] · live" {
		t.Fatalf("switched Right = %q", got)
	}
	if got := mk(`{"from":"a","to":"b","class":"needs_relaunch"}`).Right; got != "refused [a] → [b] · needs relaunch" {
		t.Fatalf("refused Right = %q", got)
	}
	if got := mk(`{"from":"a","requested":"x","reason":"r"}`).Right; got != "rejected [a] ✗ x (r)" {
		t.Fatalf("rejected Right = %q", got)
	}
	if got := mk(`not-json`).Right; !strings.Contains(got, "not-json") {
		t.Fatalf("unparseable Detail must fall back to the raw rendering, got %q", got)
	}
}

// TestP2PolishContracts pins the P2 UX batch: (a) toasts carry role=status
// (implicit aria-live=polite) — the only feedback channel for delete/copy
// outcomes must be announced to AT; (b) ONE shared copyText helper backs
// the session chip, the native-TLS fingerprint rows, and the relaunch
// command, each confirming via toast (a silent copy reads as a dead
// control); (c) ≤820px keeps the forwarded-row disclosure chevron
// (absolute-positioned, out of the 1-col grid) instead of hiding the only
// expand affordance; (d) the Models popover gets the same ≤600px viewport
// clamp as the other popovers; (e) profile rows no longer advertise a
// row-body click (cursor:pointer removed — activation lives on the
// action icons); (f) the ovf copy of the chip handler stays in sync
// (toast + role=status on ended pages too).
func TestP2PolishContracts(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Activity",
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	html := buf.String()
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]
	// (a)+(b) shared copy primitive + toast confirmations.
	mustContainAllWeb(t, js,
		"function copyText(",
		"toast.setAttribute('role', 'status')",
		"showToast('session id copied'",
		"' copied'",                 // fingerprint rows: label + ' copied'
		"'relaunch command copied'", // outcome cmd
	)
	// (c) chevron survives the ≤820px collapse as an absolute overlay.
	mustContainAllWeb(t, html,
		".rows details.row>summary .rowchev{position:absolute",
		// .row.nf has no disclosure — its spacer stays hidden.
		".rows .row.nf .rowchev{display:none}",
	)
	// (d) Models popover viewport clamp.
	mustContainAllWeb(t, html, ".sp3-models-pop{min-width:0;max-width:calc(100vw - 32px)}")
	// (e) no row-body click affordance on profile rows.
	if regexp.MustCompile(`\.sp3-pop-row\{[^}]*cursor:pointer`).MatchString(html) {
		t.Fatalf("profile rows must not advertise a row-body click (cursor:pointer)")
	}
	// (f) ended pages: the ovf duplicate confirms + announces too.
	var ebuf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &ebuf}, webPageData{
		Title: "x", LiveEnabled: false,
	})
	ehtml := ebuf.String()
	mustContainAllWeb(t, ehtml,
		"miniToast('session id copied')",
		"toast.setAttribute('role', 'status')",
	)
}

// TestP3PolishContracts pins the P3 batch. (a) faviconHref colors the tab
// dot by hero variant (JS twin in updateHeroState; live retitle in
// updateClaudeSession). (b) the live filter bar is a COMPLETE APG tab
// pattern: per-tab ids, aria-controls, roving tabindex, tabpanel wiring,
// arrow-key activation. (c) non-live chips are plain links — no tab roles,
// aria-current marks the selection. (d) auth-missing renders in the warn
// (amber) tone matching its Health classification — no more amber hero
// next to a rose cell for the same condition. (e) an 8-cell (NATIVE TLS)
// ribbon collapses to a 4-col card grid at <=1280px instead of cramming 8
// joined columns. (f) providerHue avoids the warn/danger hue bands.
// (g) the empty-Activity copy is byte-equal Go<->JS (webActivityEmptyLive).
// (h) profile-editor type floor is 10/11px with a readable placeholder.
func TestP3PolishContracts(t *testing.T) {
	// (a) favicon variants + default.
	for variant, hex := range map[string]string{
		"active": "10b981", "degraded": "fbbf24", "error": "f43f5e", "ended": "8f8f8f", "": "10b981",
	} {
		href := string(faviconHref(variant))
		if !strings.HasPrefix(href, "data:image/svg+xml,") || !strings.Contains(href, "%23"+hex) {
			t.Fatalf("faviconHref(%q) = %q, want data-URI with %%23%s", variant, href, hex)
		}
	}
	live := renderTestPage(t, webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Activity",
		DefaultClass: "forwarded-api",
		Classes: []webClassCount{
			{"all", "All", 1}, {"forwarded-api", "Forwarded API", 1},
		},
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(live)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	js := m[1]
	// (b) complete tab pattern on the live page.
	mustContainAllWeb(t, live,
		`id="filter-tab-forwarded-api"`,
		`aria-selected="true" aria-controls="activity-body">`,
		`aria-selected="false" aria-controls="activity-body" tabindex="-1"`,
		`id="activity-body" role="tabpanel" aria-labelledby="filter-tab-forwarded-api"`,
	)
	mustContainAllWeb(t, js,
		"setAttribute('tabindex', sel ? '0' : '-1')",
		"'ArrowRight'", "'Home'", "'End'",
		"setAttribute('aria-labelledby'",
	)
	// (a-live) favicon + title live mirrors.
	mustContainAllWeb(t, js,
		"document.title = 'CCWRAP · session ' + full.slice(0, 8)",
		"'%23fbbf24'",
		`link[rel="icon"]`,
	)
	// (f) hue band guard. (g) empty-copy byte sync with the Go const.
	mustContainAllWeb(t, js,
		"return 70 + (((h % 260) + 260) % 260)",
		webActivityEmptyLive,
	)
	// (d) auth-missing warn tone; (e) 8-cell ribbon early collapse;
	// (h) editor type floor.
	mustContainAllWeb(t, live,
		`.ribbon-cell[data-ribbon="Auth"][data-state="auth-missing"] .v{color:var(--warn)`,
		`@media (max-width:1280px){.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]){grid-template-columns:repeat(4,1fr)`,
		`.sp3-pop-edit-field label{color:var(--text-muted);font-size:11px}`,
		`.sp3-pop-edit-field input::placeholder{color:#808080}`,
	)
	// (c) non-live: plain links, no tab roles, aria-current selection.
	ended := renderTestPage(t, webPageData{
		Title: "x", LiveEnabled: false, ActivityTitle: "Activity",
		DefaultClass: "forwarded-api",
		Classes: []webClassCount{
			{"all", "All", 1}, {"forwarded-api", "Forwarded API", 1},
		},
	})
	mustContainAllWeb(t, ended, `aria-current="page"`)
	for _, gone := range []string{`role="tab"`, `role="tablist"`, `role="tabpanel"`} {
		if strings.Contains(ended, gone) {
			t.Fatalf("non-live filter chips are links, not tabs; %q must not render", gone)
		}
	}
}

// TestSwitchLegArmsReconciliationGate locks the 方案A re-wire: the
// reconciliation gate (popState 'pending' buffers session_updated
// snapshots; reconcileFetchFailure judges a lost ACK) is ARMED by the
// live activation path — setDefaultProfile's switch leg — not merely
// defined. The transport-failure catch must defer to the post-refresh
// reconcile step instead of toasting a potentially-lying "switch failed".
func TestSwitchLegArmsReconciliationGate(t *testing.T) {
	js := inlineScript(t)
	fnStart := strings.Index(js, "popoverCtx.setDefaultProfile = function")
	if fnStart < 0 {
		t.Fatalf("setDefaultProfile missing from inline script")
	}
	end := fnStart + 8000
	if end > len(js) {
		end = len(js)
	}
	body := js[fnStart:end]
	mustContainAllWeb(t, body,
		"setPopState('pending')",
		"popPreClickActiveProfileName = (state.session && state.session.active_profile_name) || ''",
		"popPendingSSESnapshot = null",
		"popoverCtx.pendingSwitchReconcile = true",
		"reconcileFetchFailure()",
	)
	// The blind transport-failure toast is gone — a lost ACK is judged by
	// the gate, never reported as failure while the switch may have landed.
	if strings.Contains(body, "showToast('switch failed") {
		t.Fatalf("transport failures on the switch leg must route through reconcileFetchFailure, not a blind toast")
	}
}

// TestFilterActivityClass pins the server twin of the JS rebuild's
// filter-aware capping: the class filter runs over the FULL retained
// inputs BEFORE unifiedActivityRows' newest-N cap.
func TestFilterActivityClass(t *testing.T) {
	reqs := []model.RequestRecord{
		{Method: "POST", Path: "/v1/messages"},
		{Method: "POST", Path: "/x", Synthetic: true},
		{Method: "CONNECT", LogicalTargetHost: "mcp.example.com"},
	}
	errs := []model.ErrorRecord{{Summary: "boom"}}
	tr := []model.TraceRecord{{Category: "route"}}
	if r, e, tt := filterActivityClass("all", reqs, errs, tr); len(r) != 3 || len(e) != 1 || len(tt) != 1 {
		t.Fatalf("all must pass everything through: %d/%d/%d", len(r), len(e), len(tt))
	}
	if r, e, tt := filterActivityClass("forwarded-api", reqs, errs, tr); len(r) != 1 || len(e) != 0 || len(tt) != 0 {
		t.Fatalf("forwarded-api: got %d/%d/%d", len(r), len(e), len(tt))
	}
	if r, e, tt := filterActivityClass("error", reqs, errs, tr); len(r) != 0 || len(e) != 1 || len(tt) != 0 {
		t.Fatalf("error: got %d/%d/%d", len(r), len(e), len(tt))
	}
	if r, e, tt := filterActivityClass("trace", reqs, errs, tr); len(r) != 0 || len(e) != 0 || len(tt) != 1 {
		t.Fatalf("trace: got %d/%d/%d", len(r), len(e), len(tt))
	}
}
