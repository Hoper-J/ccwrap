package ui

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestHumanRouteSource(t *testing.T) {
	cases := map[model.RouteSource]string{
		model.RouteSourceExplicit:        "Explicit",
		model.RouteSourceInheritedEnv:    "From environment",
		model.RouteSourceFallback:        "Default",
		model.RouteSource("custom_mode"): "Custom Mode",
	}
	for in, want := range cases {
		if got := HumanRouteSource(in); got != want {
			t.Fatalf("HumanRouteSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanAuth(t *testing.T) {
	cases := []struct {
		mode   model.AuthMode
		source model.AuthSource
		want   string
	}{
		{model.AuthModePassthrough, model.AuthSourceNone, "Passthrough"},
		{model.AuthModeOverrideXAPIKey, model.AuthSourceAnthropicAPIKey, "X-API-Key"},
		{model.AuthModeOverrideBearer, model.AuthSourceAnthropicToken, "Bearer token"},
		{model.AuthModeOverrideBearer, model.AuthSourceClaudeOAuthToken, "OAuth token"},
		{model.AuthModeUnsupported, model.AuthSourceNone, "Unsupported"},
		{model.AuthMode("custom_mode"), model.AuthSourceNone, "Custom Mode"},
	}
	for _, tc := range cases {
		if got := HumanAuth(tc.mode, tc.source); got != tc.want {
			t.Fatalf("HumanAuth(%q, %q) = %q, want %q", tc.mode, tc.source, got, tc.want)
		}
	}
}

func TestHumanEgress(t *testing.T) {
	cases := []struct {
		mode    string
		source  string
		summary string
		want    string
	}{
		{"direct", "none", "direct", "Direct"},
		{"auto", "inherited_env", "http://corp-proxy:8080", "From environment · http://corp-proxy:8080"},
		{"auto", "claude_settings", "http://corp-proxy:8080", "Claude settings · http://corp-proxy:8080"},
		{"explicit", "explicit_flag", "http://proxy.example:3128", "Explicit · http://proxy.example:3128"},
		{"explicit", "", "http://proxy.example:3128", "http://proxy.example:3128"},
	}
	for _, tc := range cases {
		if got := HumanEgress(tc.mode, tc.source, tc.summary); got != tc.want {
			t.Fatalf("HumanEgress(%q, %q, %q) = %q, want %q", tc.mode, tc.source, tc.summary, got, tc.want)
		}
	}
}

func TestHumanRouteAddsOnlyUsefulConfigDetail(t *testing.T) {
	cases := []struct {
		source       model.RouteSource
		configSource string
		want         string
	}{
		{model.RouteSourceExplicit, "explicit", "Explicit"},
		{model.RouteSourceInheritedEnv, "inherited_env", "From environment"},
		{model.RouteSourceFlagSettings, "flagSettings", "Flag settings"},
		{model.RouteSourcePolicySettings, "policySettings", "Policy settings"},
		{model.RouteSourceFallback, "fallback_default", "Default"},
		{model.RouteSourceClaudeSettings, "userSettings", "Claude settings · User settings"},
		{model.RouteSourceClaudeSettings, "globalConfig", "Claude settings · Global settings"},
		{model.RouteSource("custom_mode"), "userSettings", "Custom Mode · User settings"},
	}
	for _, tc := range cases {
		if got := HumanRoute(tc.source, tc.configSource); got != tc.want {
			t.Fatalf("HumanRoute(%q, %q) = %q, want %q", tc.source, tc.configSource, got, tc.want)
		}
	}
}

func TestHumanConfigSource(t *testing.T) {
	cases := map[string]string{
		"inherited_env":    "Environment",
		"globalConfig":     "Global settings",
		"userSettings":     "User settings",
		"flagSettings":     "Flag settings",
		"policySettings":   "Policy settings",
		"fallback_default": "Default",
		"":                 "None",
	}
	for in, want := range cases {
		if got := HumanConfigSource(in); got != want {
			t.Fatalf("HumanConfigSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanRouteClass(t *testing.T) {
	cases := map[model.RouteClass]string{
		model.RouteClassFirstParty:           "Anthropic",
		model.RouteClassThirdPartyHidden:     "Gateway",
		model.RouteClassThirdPartyCompatible: "Gateway · degraded",
		model.RouteClass(""):                 "Anthropic",
		model.RouteClass("future_mode"):      "Future Mode",
	}
	for in, want := range cases {
		if got := HumanRouteClass(in); got != want {
			t.Fatalf("HumanRouteClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanAuthPolicy(t *testing.T) {
	cases := map[model.AuthPolicy]string{
		model.AuthPolicyCCWRAPOverrideFailClosed: "CCWRAP-owned · fail-closed",
		model.AuthPolicyCCWRAPOverride:           "CCWRAP-owned",
		model.AuthPolicyFirstPartyPassthrough:    "Passthrough",
		model.AuthPolicyUnsafePassthrough:        "Unsafe passthrough",
		model.AuthPolicy(""):                     "Passthrough",
		model.AuthPolicy("future_pol"):           "Future Pol",
	}
	for in, want := range cases {
		if got := HumanAuthPolicy(in); got != want {
			t.Fatalf("HumanAuthPolicy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanAuthBootstrap(t *testing.T) {
	cases := []struct {
		b    model.AuthBootstrap
		kind model.AuthBootstrapKind
		want string
	}{
		{model.AuthBootstrapPlaceholderActive, model.AuthBootstrapKindBearer, "placeholder injected"},
		{model.AuthBootstrapPlaceholderActive, model.AuthBootstrapKindXAPIKey, "placeholder injected"},
		{model.AuthBootstrapNotNeeded, model.AuthBootstrapKindNone, "none"},
		{model.AuthBootstrapMissing, model.AuthBootstrapKindBearer, "missing"},
		{model.AuthBootstrap(""), model.AuthBootstrapKindNone, "none"},
	}
	for _, tc := range cases {
		if got := HumanAuthBootstrap(tc.b, tc.kind); got != tc.want {
			t.Fatalf("HumanAuthBootstrap(%q,%q) = %q, want %q", tc.b, tc.kind, got, tc.want)
		}
	}
}
