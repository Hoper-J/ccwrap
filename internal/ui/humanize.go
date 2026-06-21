package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func HumanRouteSource(src model.RouteSource) string {
	switch src {
	case model.RouteSourceExplicit:
		return "Explicit"
	case model.RouteSourceInheritedEnv:
		return "From environment"
	case model.RouteSourceClaudeSettings:
		return "Claude settings"
	case model.RouteSourceFlagSettings:
		return "Flag settings"
	case model.RouteSourcePolicySettings:
		return "Policy settings"
	case model.RouteSourceFallback:
		return "Default"
	default:
		return humanWords(string(src), "Unknown")
	}
}

// HumanRoute renders a route-source label, appending the config
// source as a detail suffix when it adds informative content. Used by
// status / web summary renderers; the launch summary keeps its own
// inline format because it carries the route_class instead.
func HumanRoute(src model.RouteSource, configSource string) string {
	route := HumanRouteSource(src)
	if !routeConfigAddsDetail(src, configSource) {
		return route
	}
	return route + " · " + HumanConfigSource(configSource)
}

func routeConfigAddsDetail(src model.RouteSource, configSource string) bool {
	switch strings.TrimSpace(configSource) {
	case "", "none", "fallback_default":
		return false
	case "explicit":
		return src != model.RouteSourceExplicit
	case "inherited_env":
		return src != model.RouteSourceInheritedEnv
	case "flagSettings":
		return src != model.RouteSourceFlagSettings
	case "policySettings":
		return src != model.RouteSourcePolicySettings
	default:
		return true
	}
}

func HumanConfigSource(src string) string {
	switch strings.TrimSpace(src) {
	case "explicit":
		return "Explicit"
	case "inherited_env":
		return "Environment"
	case "globalConfig":
		return "Global settings"
	case "userSettings":
		return "User settings"
	case "flagSettings":
		return "Flag settings"
	case "policySettings":
		return "Policy settings"
	case "fallback_default":
		return "Default"
	case "none", "":
		return "None"
	default:
		return humanWords(src, "Unknown")
	}
}

func HumanAuth(mode model.AuthMode, source model.AuthSource) string {
	switch mode {
	case model.AuthModePassthrough:
		return "Passthrough"
	case model.AuthModeOverrideXAPIKey:
		return "X-API-Key"
	case model.AuthModeOverrideBearer:
		if source == model.AuthSourceClaudeOAuthToken {
			return "OAuth token"
		}
		return "Bearer token"
	case model.AuthModeUnsupported:
		return "Unsupported"
	default:
		return humanWords(string(mode), "Unknown")
	}
}

func HumanEgress(mode, source, summary string) string {
	summary = strings.TrimSpace(summary)
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "direct", "none":
		return "Direct"
	}
	prefix := ""
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "inherited_env":
		prefix = "From environment"
	case "claude_settings":
		prefix = "Claude settings"
	case "explicit_flag":
		prefix = "Explicit"
	case "none", "":
		prefix = ""
	default:
		prefix = humanWords(source, "")
	}
	if summary == "" || strings.EqualFold(summary, "direct") {
		if prefix != "" {
			return prefix
		}
		return "Direct"
	}
	if prefix != "" {
		return prefix + " · " + summary
	}
	return summary
}

func humanWords(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	raw = strings.ReplaceAll(raw, "_", " ")
	raw = strings.ReplaceAll(raw, "-", " ")
	parts := strings.Fields(raw)
	for i, part := range parts {
		switch strings.ToLower(part) {
		case "api":
			parts[i] = "API"
		case "oauth":
			parts[i] = "OAuth"
		case "x":
			parts[i] = "X"
		default:
			if part == "" {
				continue
			}
			parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
		}
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, " ")
}

// HumanRouteClass renders a route_class enum as a human-readable label.
// Wire format (manifests, --json) keeps the raw enum; this is human-only.
func HumanRouteClass(rc model.RouteClass) string {
	switch rc {
	case model.RouteClassFirstParty, "":
		return "Anthropic"
	case model.RouteClassThirdPartyHidden:
		return "Gateway"
	case model.RouteClassThirdPartyCompatible:
		return "Gateway · degraded"
	default:
		return humanWords(string(rc), "Anthropic")
	}
}

// HumanAuthPolicy renders an auth_policy enum as a human-readable label.
func HumanAuthPolicy(p model.AuthPolicy) string {
	switch p {
	case model.AuthPolicyCCWRAPOverrideFailClosed:
		return "CCWRAP-owned · fail-closed"
	case model.AuthPolicyCCWRAPOverride:
		return "CCWRAP-owned"
	case model.AuthPolicyFirstPartyPassthrough, "":
		return "Passthrough"
	case model.AuthPolicyUnsafePassthrough:
		return "Unsafe passthrough"
	default:
		return humanWords(string(p), "Passthrough")
	}
}

// HumanProfileSwitch renders a profile_switch trace Detail (the JSON the
// switch path records) as the one human sentence shared by every surface:
//
//	switched [a] → [b] · live
//	refused [a] → [b] · needs relaunch
//	rejected [a] ✗ requested (reason)
//
// Returns "" when detail does not parse or carries neither a target nor a
// request — callers fall back to their raw rendering. Wording is the
// single source for the web first paint (unifiedActivityRows) and the TUI
// trace rows; the web live decorator (renderSwitchMarker, JS) repeats the
// same words — keep all three in lockstep.
func HumanProfileSwitch(detail string) string {
	var d struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Class     string `json:"class"`
		Requested string `json:"requested"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return ""
	}
	switch {
	case d.To != "" && d.Class == "needs_relaunch":
		return fmt.Sprintf("refused [%s] → [%s] · needs relaunch", d.From, d.To)
	case d.To != "":
		return fmt.Sprintf("switched [%s] → [%s] · live", d.From, d.To)
	case d.Requested != "":
		return fmt.Sprintf("rejected [%s] ✗ %s (%s)", d.From, d.Requested, d.Reason)
	default:
		return ""
	}
}

// NativeTLSPresentation maps the session native_tls field (+ block-episode
// count + loaded flag) to a (value, detail, state) tuple. state is one of
// native-off / native-active / native-blocked. Single wording source for
// the web ribbon cell (and its JS live-patch twin) and the TUI summary
// line. See the supervisor wrapper for the per-state rationale.
func NativeTLSPresentation(nativeTLS string, blocks int, loaded bool) (value, detail, state string) {
	switch {
	case nativeTLS == "" || nativeTLS == "off":
		return "off", "stdlib TLS (default)", "native-off"
	case nativeTLS == "active":
		if blocks > 0 {
			return "active", fmt.Sprintf("mirroring · %d prior block(s)", blocks), "native-active"
		}
		if loaded {
			return "active", "mirroring loaded fingerprint", "native-active"
		}
		return "active", "mirroring Claude Code TLS fingerprint", "native-active"
	default: // blocked: <reason>
		return "blocked", strings.TrimPrefix(nativeTLS, "blocked: "), "native-blocked"
	}
}

// BodiesPresentation maps the capture toggles to a (value, detail, state)
// tuple; state is bodies-off / bodies-on / bodies-unmasked. Single wording
// source for the web Bodies cell (and its JS twin) and the TUI capture
// line. The unmasked state applies only when request capture is also on.
func BodiesPresentation(captureBodies, captureBodiesUnmasked, captureTelemetry bool) (value, detail, state string) {
	if !captureBodies && !captureTelemetry {
		return "off", "click to choose what to capture", "bodies-off"
	}
	parts := make([]string, 0, 2)
	if captureBodies {
		parts = append(parts, "request")
	}
	if captureTelemetry {
		parts = append(parts, "telemetry")
	}
	value = strings.Join(parts, " + ")
	if captureBodies && captureBodiesUnmasked {
		return value + " ⚠", "UNMASKED — CCWRAP_UNMASK_CREDENTIALS=1; credentials in drawer + spill", "bodies-unmasked"
	}
	if captureBodies && captureTelemetry {
		detail = "recording request + response + telemetry bodies (credentials redacted)"
	} else if captureBodies {
		detail = "recording request + response bodies (credentials redacted)"
	} else {
		detail = "capturing telemetry bodies (Datadog/Sentry)"
	}
	return value, detail, "bodies-on"
}

// HumanAuthBootstrap renders the auth bootstrap state as a human-readable
// label. "placeholder injected" deliberately drops the kind suffix —
// the hero sentence and auth label already carry bearer/x-api-key.
func HumanAuthBootstrap(b model.AuthBootstrap, _ model.AuthBootstrapKind) string {
	switch b {
	case model.AuthBootstrapPlaceholderActive:
		return "placeholder injected"
	case model.AuthBootstrapNotNeeded, "":
		return "none"
	case model.AuthBootstrapMissing:
		return "missing"
	default:
		return humanWords(string(b), "none")
	}
}
