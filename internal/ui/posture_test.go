package ui

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestSessionPosture(t *testing.T) {
	cases := []struct {
		name string
		sess *model.Session
		want string
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
			// Only the count: the error list carries the details.
			name: "warn health, count only",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough, Health: model.HealthWarn, RecentErrorCount: 2},
			want: "Routing Claude Code through api.anthropic.com with your local credentials. — 2 issues recorded since launch.",
		},
		{
			name: "one issue",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough, Health: model.HealthError, RecentErrorCount: 1},
			want: "Routing Claude Code through api.anthropic.com with your local credentials. — 1 issue recorded since launch.",
		},
		{
			name: "healed (ok) still shows cumulative count",
			sess: &model.Session{RouteClass: model.RouteClassFirstParty, ExactUpstreamHost: "api.anthropic.com", AuthPolicy: model.AuthPolicyFirstPartyPassthrough, Health: model.HealthOK, RecentErrorCount: 2},
			want: "Routing Claude Code through api.anthropic.com with your local credentials. — 2 issues recorded since launch.",
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
		if got := SessionPosture(tc.sess); got != tc.want {
			t.Fatalf("%s: SessionPosture = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := SessionPosture(nil); got != "" {
		t.Fatalf("SessionPosture(nil) = %q, want empty", got)
	}
}
