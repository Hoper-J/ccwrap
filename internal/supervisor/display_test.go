package supervisor

import (
	"net/http"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// The ribbon as data: every cell in order, each with its words; the Profile
// cell names the active profile or reads inherit-env.
func TestDisplayCells(t *testing.T) {
	sess := model.Session{
		RouteClass: model.RouteClassThirdPartyHidden, RouteSource: model.RouteSourceInheritedEnv,
		AuthPolicy:    model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap: model.AuthBootstrapPlaceholderActive, AuthBootstrapKind: model.AuthBootstrapKindBearer,
		ModelAliasCount: 1, RecentRequestCount: 9, RecentFailedCount: 2,
	}
	cells := displayCells(sess)
	byKey := map[string]displayCell{}
	var keys []string
	for _, c := range cells {
		byKey[c.Key] = c
		keys = append(keys, c.Key)
	}
	if got := len(keys); got != 7 {
		t.Fatalf("cells = %v, want route, auth, models, traffic, profile, bodies, egress", keys)
	}
	if c := byKey["route"]; c.Value != "Gateway" || c.Detail != "from environment" {
		t.Errorf("route = %+v", c)
	}
	if c := byKey["auth"]; c.Value != "CCWRAP-owned · fail-closed" || c.Detail != "placeholder injected" || c.State != "" {
		t.Errorf("auth = %+v", c)
	}
	// Traffic counts requests; its detail counts failed model requests and
	// is marked when there are any.
	if c := byKey["traffic"]; c.Value != "9 req" || c.Detail != "2 failed" || c.State != "failed" {
		t.Errorf("traffic = %+v", c)
	}
	if c := byKey["profile"]; c.Value != "inherit-env" || c.Detail != "no profile" || c.State != "inherit-env-static" {
		t.Errorf("profile without one = %+v", c)
	}
	sess.ActiveProfileName, sess.ActiveProfileProvider, sess.ModelAliasCount = "alpha", "Anthropic", 2
	if c := profileCell(sess); c.Value != "alpha" || c.Detail != "Anthropic · 2 aliases" || c.State != "active" {
		t.Errorf("active profile = %+v", c)
	}
	sess.NativeTLS = "active"
	if cells := displayCells(sess); len(cells) != 8 || cells[7].Key != "native_tls" {
		t.Errorf("native TLS adds a cell: %+v", cells)
	}
}

// The Auth cell keeps its usual words on a healthy session and turns into a
// MISSING warning when the route has no key, naming the env var when the
// profile takes its key from one.
func TestDisplayCellsAuth(t *testing.T) {
	auth := func(sess model.Session) displayCell {
		for _, c := range displayCells(sess) {
			if c.Key == "auth" {
				return c
			}
		}
		t.Fatal("no auth cell")
		return displayCell{}
	}
	healthy := auth(model.Session{AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed, AuthBootstrap: model.AuthBootstrapPlaceholderActive, AuthBootstrapKind: model.AuthBootstrapKindBearer})
	if healthy.State != "" || healthy.Value == "⚠ MISSING" {
		t.Errorf("healthy auth = %+v", healthy)
	}
	if c := auth(model.Session{ActiveProfileName: "local", AuthBootstrap: model.AuthBootstrapMissing, MissingAuthEnv: "ANTHROPIC_AUTH_TOKEN"}); c.State != "auth-missing" || c.Value != "⚠ MISSING" || c.Detail != `profile "local" needs $ANTHROPIC_AUTH_TOKEN` {
		t.Errorf("missing env = %+v", c)
	}
	if c := auth(model.Session{ActiveProfileName: "foo", AuthBootstrap: model.AuthBootstrapMissing}); c.State != "auth-missing" || c.Detail != `profile "foo" has no auth source configured` {
		t.Errorf("no auth source = %+v", c)
	}
}

// TestBodiesCellPresentation_Matrix locks the full 3-arg (captureBodies,
// captureBodiesUnmasked, captureTelemetry) presentation matrix. The Bodies
// cell summarizes TWO capture toggles (request bodies + telemetry bodies), so
// the value joins the present parts with " + " and the detail/state reflect
// the combination.
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

// TestNativeTLSCellPresentation_Matrix locks the nativeTLSCellPresentation
// contract: off (or empty) -> stdlib default; active -> mirroring; blocked:
// <reason> -> an error condition whose detail is the bare reason.
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

// Every request record falls in exactly one Activity class.
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

// The Models cell counts aliases and warns when Claude models went out with
// none.
func TestModelsCellPresentation(t *testing.T) {
	cases := []struct {
		sess                 model.Session
		value, detail, state string
	}{
		{model.Session{}, "default", "", ""},
		{model.Session{ModelAliasCount: 1}, "1 alias", "", "aliases-active"},
		{model.Session{ModelAliasCount: 2, UnmappedModels: []string{"claude-opus-5-5"}}, "2 aliases", "1 unaliased model sent", "aliases-warn"},
		{model.Session{ModelAliasCount: 1, UnmappedModels: []string{"a", "b"}}, "1 alias", "2 unaliased models sent", "aliases-warn"},
	}
	for _, tc := range cases {
		v, d, st := modelsCellPresentation(tc.sess)
		if v != tc.value || d != tc.detail || st != tc.state {
			t.Errorf("%+v: got %q %q %q", tc.sess, v, d, st)
		}
	}
}
