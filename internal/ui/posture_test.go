package ui

import (
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestSessionPosture(t *testing.T) {
	fixed := time.Date(2026, 5, 2, 15, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return fixed }
	defer func() { nowFunc = time.Now }()

	lastErr := &model.ErrorRecord{Summary: "blind tunnel dial failed", Timestamp: fixed.Add(-8 * time.Second)}

	cases := []struct {
		name    string
		sess    *model.Session
		lastErr *model.ErrorRecord
		want    string
	}{
		{
			name: "first-party default",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough},
			want: "Routing Claude Code through api.anthropic.com with your local credentials.",
		},
		{
			name: "first-party override auth",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed},
			want: "Routing Claude Code through api.anthropic.com with CCWRAP-owned auth.",
		},
		{
			name: "third-party hidden no alias",
			sess: &model.Session{RouteClass: model.RouteClassThirdPartyHidden, ExactUpstreamHost: "50.18.84.244:3000", AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed, ModelAliasCount: 0},
			want: "Routing Claude Code through 50.18.84.244:3000. CCWRAP holds your gateway credentials and applies 0 model aliases; Claude Code only sees logical names.",
		},
		{
			name: "third-party hidden one alias",
			sess: &model.Session{RouteClass: model.RouteClassThirdPartyHidden, ExactUpstreamHost: "50.18.84.244:3000", AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed, ModelAliasCount: 1},
			want: "Routing Claude Code through 50.18.84.244:3000. CCWRAP holds your gateway credentials and applies 1 model alias; Claude Code only sees logical names.",
		},
		{
			name: "warn health, count only (no lastErr — e.g. ccwrap status)",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough, Health: model.HealthWarn, RecentErrorCount: 2},
			want: "Routing Claude Code through api.anthropic.com with your local credentials. — 2 errors recorded since launch.",
		},
		{
			name:    "error health with last error (Web/TUI)",
			sess:    &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough, Health: model.HealthError, RecentErrorCount: 2},
			lastErr: lastErr,
			want:    "Routing Claude Code through api.anthropic.com with your local credentials. — 2 errors recorded since launch. Last: blind tunnel dial failed 8s ago.",
		},
		{
			name: "healed (ok) still shows cumulative count",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough, Health: model.HealthOK, RecentErrorCount: 2},
			want: "Routing Claude Code through api.anthropic.com with your local credentials. — 2 errors recorded since launch.",
		},
		{
			// There is no distinct hero sentence for
			// third_party_compatible; it shares the gateway sentence
			// with hidden (factually a CCWRAP-cred gateway). This case
			// LOCKS that inferred-but-intentional behavior.
			name: "third-party compatible shares gateway sentence",
			sess: &model.Session{RouteClass: model.RouteClassThirdPartyCompatible, ExactUpstreamHost: "50.18.84.244:3000", AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed, ModelAliasCount: 2},
			want: "Routing Claude Code through 50.18.84.244:3000. CCWRAP holds your gateway credentials and applies 2 model aliases; Claude Code only sees logical names.",
		},
		{
			name: "ended (reasonless — accepted deviation)",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", State: model.StateEnded},
			want: "Session closed. Final state preserved below.",
		},
	}
	for _, tc := range cases {
		if got := SessionPosture(tc.sess, tc.lastErr); got != tc.want {
			t.Fatalf("%s: SessionPosture = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := SessionPosture(nil, nil); got != "" {
		t.Fatalf("SessionPosture(nil) = %q, want empty", got)
	}
}

func TestHumanAge(t *testing.T) {
	base := time.Date(2026, 5, 2, 15, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return base }
	defer func() { nowFunc = time.Now }()
	cases := map[time.Duration]string{
		8 * time.Second:  "8s ago",
		90 * time.Second: "1m ago",
		3 * time.Hour:    "3h ago",
		50 * time.Hour:   "2d ago",
		-5 * time.Second: "0s ago", // clock-skew clamp
	}
	for d, want := range cases {
		if got := humanAge(base.Add(-d)); got != want {
			t.Fatalf("humanAge(-%s) = %q, want %q", d, got, want)
		}
	}
}
