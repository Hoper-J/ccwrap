package ui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// TestStatusEndedIsNeutral pins the TUI/CLI severity doctrine: ended is a
// lifecycle terminal state, not a failure — it renders dim (matching the
// web hero's muted ended variant), never danger-red. fail/error keep red.
func TestStatusEndedIsNeutral(t *testing.T) {
	p := New(true)
	if got := p.Status("ended"); got != "\033[2mended\033[0m" {
		t.Fatalf("Status(ended) = %q, want dim (neutral), not a severity color", got)
	}
	if got := p.Status("error"); got != "\033[31merror\033[0m" {
		t.Fatalf("Status(error) = %q, want red", got)
	}
	if got := p.Status("degraded"); got != "\033[33mdegraded\033[0m" {
		t.Fatalf("Status(degraded) = %q, want yellow", got)
	}
	if got := p.Status("active"); got != "\033[32mactive\033[0m" {
		t.Fatalf("Status(active) = %q, want green", got)
	}
}

// TestHumanProfileSwitch pins the shared switch-trace wording — the single
// source for the web first paint, the TUI trace rows, and (by convention)
// the JS renderSwitchMarker decorator.
func TestHumanProfileSwitch(t *testing.T) {
	cases := []struct{ detail, want string }{
		{`{"from":"a","to":"b","class":"live"}`, "switched [a] → [b] · live"},
		{`{"from":"a","to":"b","class":"needs_relaunch"}`, "refused [a] → [b] · needs relaunch"},
		{`{"from":"a","requested":"x","reason":"r"}`, "rejected [a] ✗ x (r)"},
		{`not-json`, ""},
		{`{}`, ""},
	}
	for _, c := range cases {
		if got := HumanProfileSwitch(c.detail); got != c.want {
			t.Fatalf("HumanProfileSwitch(%q) = %q, want %q", c.detail, got, c.want)
		}
	}
}

// TestSharedPresentations spot-checks the tuples that moved here from the
// supervisor so the web cell and the TUI summary line stay word-identical
// (the supervisor wrappers + their byte-equality JS-twin tests still pin
// the full matrices).
func TestSharedPresentations(t *testing.T) {
	if v, d, st := NativeTLSPresentation("blocked: mirror dial failed", 1, false); v != "blocked" || d != "mirror dial failed" || st != "native-blocked" {
		t.Fatalf("blocked tuple = (%q,%q,%q)", v, d, st)
	}
	if v, _, st := NativeTLSPresentation("active", 0, false); v != "active" || st != "native-active" {
		t.Fatalf("active tuple = (%q,_,%q)", v, st)
	}
	if v, d, st := BodiesPresentation(true, true, false); v != "request ⚠" || st != "bodies-unmasked" || !strings.Contains(d, "UNMASKED") {
		t.Fatalf("unmasked tuple = (%q,%q,%q)", v, d, st)
	}
	if v, _, st := BodiesPresentation(true, false, true); v != "request + telemetry" || st != "bodies-on" {
		t.Fatalf("on tuple = (%q,_,%q)", v, st)
	}
}

// TestLatestClaudeSessionID pins the newest-first scan + the single-source
// header constant the web bootstrap and the TUI header both consume.
func TestLatestClaudeSessionID(t *testing.T) {
	if ClaudeSessionHeader != "X-Claude-Code-Session-Id" {
		t.Fatalf("header constant drifted: %q", ClaudeSessionHeader)
	}
	recs := []model.RequestRecord{
		{RequestHeaders: http.Header{ClaudeSessionHeader: {"older-id"}}},
		{RequestHeaders: http.Header{"User-Agent": {"x"}}},
		{RequestHeaders: http.Header{ClaudeSessionHeader: {"newest-id"}}},
		{}, // CONNECT-style row with no headers
	}
	if got := LatestClaudeSessionID(recs); got != "newest-id" {
		t.Fatalf("LatestClaudeSessionID = %q, want newest-id", got)
	}
	if got := LatestClaudeSessionID(nil); got != "" {
		t.Fatalf("empty input must return empty, got %q", got)
	}
}
