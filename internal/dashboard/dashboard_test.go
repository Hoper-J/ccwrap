package dashboard

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

func TestNormalizeViewDiagnosticsAliases(t *testing.T) {
	cases := map[string]string{
		"overview":    "overview",
		"requests":    "requests",
		"errors":      "errors",
		"diagnostics": "diagnostics",
		"trace":       "diagnostics",
		"":            "overview",
	}
	for in, want := range cases {
		if got := normalizeView(in); got != want {
			t.Fatalf("normalizeView(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRenderOverviewLinesMatchesSinglePageStructure(t *testing.T) {
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	sess := &model.Session{
		State:              model.StateActive,
		ExactUpstreamBase:  "https://api.example.test",
		EgressMode:         "http",
		EgressSource:       "inherited_env",
		EgressSummary:      "http://corp-proxy:8080",
		RouteSource:        model.RouteSourceInheritedEnv,
		AuthMode:           model.AuthModeOverrideBearer,
		AuthSource:         model.AuthSourceAnthropicToken,
		RecentRequestCount: 2,
		RecentErrorCount:   0,
	}
	reqs := []model.RequestRecord{{
		Timestamp:          now,
		Method:             "POST",
		Path:               "/v1/messages",
		StatusCode:         200,
		LatencyMS:          842,
		StreamState:        model.StreamStateSSE,
		ActualUpstreamHost: "api.example.test",
	}}
	trace := []model.TraceRecord{{
		Timestamp: now.Add(-time.Second),
		Category:  "route",
		Summary:   "upstream selected",
		Detail:    "api.example.test",
	}}

	lines := renderOverviewLines(ui.New(false), sess, reqs, nil, trace)
	joined := strings.Join(lines, "\n")
	// Summary fields moved to buildFrameFromSnapshot (rendered before every
	// view); renderOverviewLines is now the body only.
	mustContain := []string{"Recent activity", "Requests", "Network diagnostics"}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Fatalf("overview missing %q\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Summary") {
		t.Fatalf("overview body must NOT include the old Summary block (summary moved to the frame header)\n%s", joined)
	}
	if strings.Contains(joined, "Errors") {
		t.Fatalf("overview should omit empty Errors section\n%s", joined)
	}
	if strings.Contains(joined, "Proxy") || strings.Contains(joined, "Fail") {
		t.Fatalf("overview should not reintroduce removed summary keys\n%s", joined)
	}
}

func mustContainLines(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("frame missing %q\n---\n%s", sub, s)
		}
	}
}

func TestFrameHeaderHeroAndPollStrip(t *testing.T) {
	pal := ui.New(false)
	snap := snapshot{
		activeCount: 1, staleCount: 0, hasCurrent: true,
		session: &model.Session{
			ID: "74c5fcaf812ff6d5", State: model.StateActive, ClaudePID: 7374,
			RouteClass: model.RouteClassThirdPartyHidden, ExactUpstreamHost: "50.18.84.244:3000",
			AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed, ModelAliasCount: 1,
			RecentRequestCount: 9,
		},
	}
	for _, view := range []string{"overview", "requests", "errors", "diagnostics"} {
		lines := buildFrameFromSnapshot("/runtime/x", snap, pal, view, 700*time.Millisecond, time.Second, 110)
		j := strings.Join(lines, "\n")
		mustContainLines(t, j,
			"ccwrap · 1 active · 0 stale",
			"74c5fcaf", "active",
			"CCWRAP holds your gateway credentials", // hero from SessionPosture, every view
			"poll 700ms",                            // poll vocabulary
			"route", "Gateway", "auth",              // summary fields in every view
		)
		if strings.Contains(j, "stream  connected") {
			t.Fatalf("[%s] TUI must not use SSE 'stream connected' vocabulary", view)
		}
		if strings.Contains(j, "/runtime/x") {
			t.Fatalf("[%s] runtime path must leave the always-on header", view)
		}
		if strings.Contains(j, "74c5fcaf812ff6d5") {
			t.Fatalf("[%s] full ID must not appear", view)
		}
	}
	// TUI uses dim "SYNTH GET", NOT Web's "SYNTHETIC".
	synSnap := snap
	synSnap.requests = []model.RequestRecord{{Method: "GET", Synthetic: true, Path: "/v1/mcp_servers", StatusCode: 200}}
	sj := strings.Join(buildFrameFromSnapshot("/r", synSnap, pal, "requests", time.Second, time.Second, 110), "\n")
	if !strings.Contains(sj, "SYNTH GET") {
		t.Fatalf("TUI must render 'SYNTH GET' (ui.ShortMethodLabel), got:\n%s", sj)
	}
	if strings.Contains(sj, "SYNTHETIC") {
		t.Fatalf("TUI must NOT render Web-style 'SYNTHETIC'")
	}
}

func TestFrameFooterRealKeys(t *testing.T) {
	pal := ui.New(false)
	base := &model.Session{ID: "abc", State: model.StateActive}
	l1 := buildFrameFromSnapshot("/r", snapshot{hasCurrent: true, session: base}, pal, "overview", time.Second, time.Second, 100)
	j1 := strings.Join(l1, "\n")
	mustContainLines(t, j1, "[1-4] view", "[r] refresh", "[q] quit")
	if strings.Contains(j1, "switch view with --view") {
		t.Fatalf("old restart-required footer must be gone")
	}
	l2 := buildFrameFromSnapshot("/r", snapshot{hasCurrent: true, session: base, staleCount: 2,
		errors: []model.ErrorRecord{{Summary: "x"}}}, pal, "overview", time.Second, time.Second, 100)
	j2 := strings.Join(l2, "\n")
	mustContainLines(t, j2, "[g] gc stale", "[e] focus errors")
}

func TestErrorRowsSurfaceSuggestedAction(t *testing.T) {
	rows := renderErrorRows(ui.New(false), []model.ErrorRecord{
		{ErrorClass: "dial", UpstreamHost: "api.x", Summary: "blind tunnel dial failed", SuggestedAction: "check upstream reachability"},
	}, 6)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "check upstream reachability") {
		t.Fatalf("TUI error row must surface SuggestedAction, got:\n%s", joined)
	}
}

func TestChooseDiscoveredEmpty(t *testing.T) {
	if chooseDiscovered(nil, "") != nil {
		t.Fatal("expected nil for empty sessions")
	}
	if chooseDiscovered([]model.DiscoveredSession{}, "anything") != nil {
		t.Fatal("expected nil for empty sessions with filter")
	}
}

func TestChooseDiscoveredExplicitIDMatch(t *testing.T) {
	sessions := []model.DiscoveredSession{
		{Manifest: model.SessionManifest{SessionID: "aaa", CreatedAt: time.Now()}},
		{Manifest: model.SessionManifest{SessionID: "bbb", CreatedAt: time.Now().Add(-time.Hour)}},
	}
	got := chooseDiscovered(sessions, "bbb")
	if got == nil || got.Manifest.SessionID != "bbb" {
		t.Fatalf("expected bbb, got %+v", got)
	}
}

func TestChooseDiscoveredExplicitIDMissingFallsBackToFirst(t *testing.T) {
	sessions := []model.DiscoveredSession{
		{Manifest: model.SessionManifest{SessionID: "aaa", CreatedAt: time.Now()}},
		{Manifest: model.SessionManifest{SessionID: "bbb", CreatedAt: time.Now().Add(-time.Hour)}},
	}
	got := chooseDiscovered(sessions, "does-not-exist")
	if got == nil || got.Manifest.SessionID != "aaa" {
		t.Fatalf("expected fallback to first session (aaa), got %+v", got)
	}
}

func TestChooseDiscoveredAutoPicksFirst(t *testing.T) {
	sessions := []model.DiscoveredSession{
		{Manifest: model.SessionManifest{SessionID: "newest"}},
		{Manifest: model.SessionManifest{SessionID: "older"}},
	}
	got := chooseDiscovered(sessions, "")
	if got == nil || got.Manifest.SessionID != "newest" {
		t.Fatalf("expected newest, got %+v", got)
	}
	if sessions[0].Manifest.SessionID != "newest" {
		t.Fatal("chooseDiscovered should not reorder the caller's slice")
	}
}

func TestBuildFrameFromSnapshotNoSessions(t *testing.T) {
	pal := ui.New(false)
	snap := snapshot{activeCount: 0, staleCount: 0, hasCurrent: false}
	lines := buildFrameFromSnapshot("/runtime", snap, pal, "overview", time.Second, time.Second, 120)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "No live sessions.") {
		t.Fatalf("expected no-sessions prompt, got:\n%s", joined)
	}
	if strings.Contains(joined, "other sessions") {
		t.Fatalf("expected no other-sessions line, got:\n%s", joined)
	}
}

func TestBuildFrameFromSnapshotControlUnavailable(t *testing.T) {
	pal := ui.New(false)
	snap := snapshot{
		activeCount:        1,
		hasCurrent:         true,
		current:            model.DiscoveredSession{Manifest: model.SessionManifest{SessionID: "abc123", ExactUpstreamBase: "https://api.example.test", EgressMode: "direct"}},
		session:            nil,
		controlUnavailable: true,
		refreshError:       "socket gone",
	}
	lines := buildFrameFromSnapshot("/runtime", snap, pal, "overview", time.Second, time.Second, 120)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "abc123") {
		t.Fatalf("expected session id in degraded header, got:\n%s", joined)
	}
	if !strings.Contains(joined, "socket gone") {
		t.Fatalf("expected refresh error in degraded prose, got:\n%s", joined)
	}
}

func TestBuildFrameFromSnapshotSubFetchWarning(t *testing.T) {
	pal := ui.New(false)
	snap := snapshot{
		activeCount:  1,
		hasCurrent:   true,
		current:      model.DiscoveredSession{Manifest: model.SessionManifest{SessionID: "abc"}},
		session:      &model.Session{ID: "abc", State: model.StateActive},
		refreshError: "trace fetch failed",
		requests:     []model.RequestRecord{},
	}
	lines := buildFrameFromSnapshot("/runtime", snap, pal, "overview", time.Second, time.Second, 120)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "refresh warning") {
		t.Fatalf("expected refresh warning line, got:\n%s", joined)
	}
	if !strings.Contains(joined, "abc") {
		t.Fatalf("expected session id in header, got:\n%s", joined)
	}
	if !strings.Contains(joined, "route") {
		t.Fatalf("expected summary fields (route) to render despite warning, got:\n%s", joined)
	}
}

func TestBuildFrameFromSnapshotMultiSessionSelector(t *testing.T) {
	pal := ui.New(false)
	snap := snapshot{
		activeCount: 3,
		hasCurrent:  true,
		current:     model.DiscoveredSession{Manifest: model.SessionManifest{SessionID: "pri"}},
		session:     &model.Session{ID: "pri", State: model.StateActive},
		otherIDs:    []string{"alt-1", "alt-2"},
	}
	lines := buildFrameFromSnapshot("/runtime", snap, pal, "overview", time.Second, time.Second, 120)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "other") {
		t.Fatalf("expected other-sessions line (label 'other'), got:\n%s", joined)
	}
	if !strings.Contains(joined, "alt-1") || !strings.Contains(joined, "alt-2") {
		t.Fatalf("expected other session ids listed, got:\n%s", joined)
	}
	// other-sessions is copy-only/non-interactive (switch via
	// `ccwrap dashboard --session <ID>`); the footer is now real keys, not
	// the old "--session ID to pin" hint.
	if !strings.Contains(joined, "[1-4] view") {
		t.Fatalf("expected real-key footer, got:\n%s", joined)
	}
	if strings.Contains(joined, "--session ID to pin") {
		t.Fatalf("old pre-redesign footer hint must be gone, got:\n%s", joined)
	}
}

func TestRendererDrawFullRedrawAndNoOp(t *testing.T) {
	var buf bytes.Buffer
	r := newRendererWithMode(&buf, 80, true)
	r.Draw([]string{"line-A", "line-B", "line-C"})
	first := buf.String()
	if !strings.Contains(first, "\033[1;1H\033[J") {
		t.Fatalf("expected clear-to-end escape at row 1 on first draw, got:\n%q", first)
	}
	for _, want := range []string{"line-A", "line-B", "line-C"} {
		if !strings.Contains(first, want) {
			t.Fatalf("expected %q in output, got:\n%q", want, first)
		}
	}

	buf.Reset()
	r.Draw([]string{"line-A", "line-B", "line-C"})
	if buf.Len() != 0 {
		t.Fatalf("expected no output when frame unchanged, got:\n%q", buf.String())
	}
}

func TestRendererDrawTailClearOnShrink(t *testing.T) {
	var buf bytes.Buffer
	r := newRendererWithMode(&buf, 80, true)
	r.Draw([]string{"a", "b", "c", "d", "e"})
	buf.Reset()
	r.Draw([]string{"a", "b", "c"})
	out := buf.String()
	if !strings.Contains(out, "\033[4;1H\033[J") {
		t.Fatalf("expected clear-to-end at row 4 when shrinking, got:\n%q", out)
	}
	if strings.Contains(out, "d") || strings.Contains(out, "e") {
		t.Fatalf("expected removed lines absent from redraw, got:\n%q", out)
	}
}

func TestRendererResizeForcesFullRedraw(t *testing.T) {
	var buf bytes.Buffer
	r := newRendererWithMode(&buf, 80, true)
	r.Draw([]string{"a", "b"})
	buf.Reset()
	r.resize(40)
	r.Draw([]string{"a", "b"})
	out := buf.String()
	if !strings.Contains(out, "\033[1;1H\033[J") {
		t.Fatalf("expected full redraw after resize (identical content should still replay), got:\n%q", out)
	}
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected content %q present in redraw, got:\n%q", want, out)
		}
	}
}

func TestSessionHeaderShowsStateAcrossViews(t *testing.T) {
	pal := ui.New(false)
	sess := &model.Session{ID: "abc", State: model.StateActive, Health: model.HealthError}
	snap := snapshot{
		activeCount: 1,
		hasCurrent:  true,
		current:     model.DiscoveredSession{Manifest: model.SessionManifest{SessionID: "abc"}},
		session:     sess,
	}
	for _, view := range []string{"overview", "requests", "errors", "diagnostics"} {
		lines := buildFrameFromSnapshot("/runtime", snap, pal, view, time.Second, time.Second, 120)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "error") {
			t.Fatalf("view=%s did not show session health %q:\n%s", view, "error", joined)
		}
	}
}

func TestRendererNonTTYWritesPlainText(t *testing.T) {
	var buf bytes.Buffer
	r := newRendererWithMode(&buf, 80, false)
	r.Draw([]string{"line-A", "line-B"})
	out := buf.String()
	if strings.Contains(out, "\033[") {
		t.Fatalf("non-TTY output should not contain ANSI escapes, got %q", out)
	}
	for _, want := range []string{"line-A", "line-B"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in plain-text output, got %q", want, out)
		}
	}
	buf.Reset()
	r.Draw([]string{"line-A", "line-B"})
	if buf.Len() != 0 {
		t.Fatalf("non-TTY renderer should still skip identical frames, got %q", buf.String())
	}
}

func TestFormatOtherIDs(t *testing.T) {
	got := formatOtherIDs([]string{"a", "b"}, 4)
	if got != "a b" {
		t.Fatalf("got %q want %q", got, "a b")
	}
	got = formatOtherIDs([]string{"a", "b", "c", "d", "e", "f"}, 4)
	if got != "a b c d +2 more" {
		t.Fatalf("got %q", got)
	}
	if formatOtherIDs(nil, 4) != "" {
		t.Fatal("expected empty string for nil ids")
	}
}

func TestClampANSIWidth(t *testing.T) {
	// Plain text truncates to the visible column count.
	if got := clampANSIWidth("abcdefghij", 4); got != "abcd" {
		t.Errorf("plain truncate: got %q want %q", got, "abcd")
	}
	// Shorter-than-width text is unchanged.
	if got := clampANSIWidth("abc", 10); got != "abc" {
		t.Errorf("no-op: got %q", got)
	}
	// ANSI SGR codes have zero width and survive; the visible payload is
	// counted, and a reset is appended so color cannot bleed forward.
	styled := "\033[2mhello world\033[0m"
	got := clampANSIWidth(styled, 5)
	if !strings.Contains(got, "\033[2m") {
		t.Errorf("style escape dropped: %q", got)
	}
	if !strings.Contains(got, "hello") || strings.Contains(got, "world") {
		t.Errorf("visible truncation wrong (want 'hello' not 'world'): %q", got)
	}
	if !strings.HasSuffix(got, "\033[0m") {
		t.Errorf("styled+truncated row must end in a reset: %q", got)
	}
	// Visible width really is 5 (escapes excluded).
	if vis := visibleLen(got); vis != 5 {
		t.Errorf("visible width = %d, want 5 (%q)", vis, got)
	}
	if clampANSIWidth("abc", 0) != "" {
		t.Error("w<=0 must clamp to empty")
	}
}

// visibleLen counts non-escape runes — test helper mirroring clampANSIWidth's
// width accounting.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			}
			i = j
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// TestTUIDoctrineAlignment pins the TUI↔web alignment batch from the
// 2026-06 UI/UX review: (a) Health renders the web vocabulary
// (active/degraded/error/ended), with ended NEUTRAL (dim), never red;
// (b) the header says "up Xm" (uptime), not "Xm ago" (last-seen);
// (c) the summary always carries the active profile, and surfaces native
// TLS + capture state — including the standing UNMASKED danger marker and
// the amber auth-missing diagnosis naming the env var; (d) profile_switch
// trace rows render the shared human sentence (ui.HumanProfileSwitch),
// never raw Detail JSON; (e) the Claude conversation id appears alongside
// the ccwrap id, matching the web brandbar's identity vocabulary.
func TestTUIDoctrineAlignment(t *testing.T) {
	pal := ui.New(false)
	sess := &model.Session{
		ID: "f3a9c2d81b4e4a7f", State: model.StateActive, Health: model.HealthWarn,
		ClaudePID: 4822, CreatedAt: time.Now().Add(-37 * time.Minute),
		RouteClass: model.RouteClassThirdPartyHidden, ExactUpstreamHost: "gw.example.com",
		AuthPolicy:    model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap: model.AuthBootstrapMissing, MissingAuthEnv: "CCWRAP_UPSTREAM_AUTH_TOKEN",
		ActiveProfileName: "gateway", ActiveProfileProvider: "openrouter",
		NativeTLS: "blocked: mirror dial failed", NativeTLSFallbacks: 1,
		CaptureBodies: true, CaptureBodiesUnmasked: true,
	}
	snap := snapshot{
		activeCount: 1, hasCurrent: true, session: sess,
		requests: []model.RequestRecord{{
			Timestamp: time.Now(), Method: "POST", Path: "/v1/messages", StatusCode: 429,
			RequestHeaders: map[string][]string{ui.ClaudeSessionHeader: {"9b2f4e6a-8c1d-4f3b-a7e9-5d2c8b1f4a6e"}},
		}},
		trace: []model.TraceRecord{{
			Timestamp: time.Now(), Category: "profile_switch", Summary: "switched",
			Detail: `{"from":"official","to":"gateway","class":"live"}`,
		}},
	}
	j := strings.Join(buildFrameFromSnapshot("/r", snap, pal, "diagnostics", time.Second, time.Second, 160), "\n")
	mustContainLines(t, j,
		"degraded",                // web vocabulary, not raw "warn"
		"up 37m",                  // uptime wording, not "37m ago"
		"claude session 9b2f4e6a", // conversation id beside the ccwrap id
		"profile   gateway · openrouter",
		"blocked · mirror dial failed",           // native TLS line, fail-closed visible
		"UNMASKED — CCWRAP_UNMASK_CREDENTIALS=1", // standing capture warning
		"⚠ missing — needs $CCWRAP_UPSTREAM_AUTH_TOKEN",
		"switched [official] → [gateway] · live", // shared switch sentence
	)
	if strings.Contains(j, `{"from":"official"`) {
		t.Fatalf("profile_switch rows must render the human sentence, not raw Detail JSON\n%s", j)
	}
	if strings.Contains(j, "37m ago") {
		t.Fatalf("header must say uptime (up 37m), not a last-seen style \"37m ago\"\n%s", j)
	}

	// Ended session: neutral vocabulary; colored run shows dim, not red.
	endedSess := &model.Session{ID: "abc", State: model.StateEnded}
	colored := ui.New(true)
	hj := strings.Join(renderSessionHeader(colored, endedSess, ""), "\n")
	if !strings.Contains(hj, "\033[2mended\033[0m") {
		t.Fatalf("ended must render dim/neutral, got: %q", hj)
	}
	if strings.Contains(hj, "\033[31m") {
		t.Fatalf("ended must not render red: %q", hj)
	}
}
