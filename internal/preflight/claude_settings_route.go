package preflight

import (
	"strings"

	"github.com/Hoper-J/ccwrap/internal/envpolicy"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

// RouteBypass is one place in Claude Code's own configuration that would send
// its API requests past ccwrap.
type RouteBypass struct {
	// Source is the kind of file, e.g. "project settings".
	Source string
	// Where names the file, e.g. "project settings /x/.claude/settings.json".
	Where string
	// What names the entry, e.g. "ANTHROPIC_BASE_URL (https://gw.example)".
	What string
	// Profile marks an Anthropic SDK profile rather than a settings file.
	Profile bool
	// Endpoint marks a base URL rather than a provider switch.
	Endpoint bool
}

func (b RouteBypass) String() string { return b.Where + ": " + b.What }

// RouteBypassError rejects a launch in which Claude Code keeps its own
// credentials and its own configuration would take its requests past ccwrap.
// Error lists every such place, then how to clear each kind.
type RouteBypassError struct {
	Bypasses []RouteBypass
	// ThirdParty is set on third-party routes, where ccwrap-owned auth puts
	// the placeholder in the child and Claude Code then ignores them all.
	ThirdParty bool
}

func (e *RouteBypassError) Error() string {
	var b strings.Builder
	b.WriteString("Claude Code's own config would send its requests past ccwrap:")
	var inSettings, inProfile, endpoint bool
	for _, x := range e.Bypasses {
		b.WriteString("\n  - " + x.String())
		inSettings = inSettings || !x.Profile
		inProfile = inProfile || x.Profile
		endpoint = endpoint || x.Endpoint
	}
	b.WriteString("\nTo launch, clear each of them:")
	if inSettings {
		b.WriteString("\n  - settings: delete the key, or override it in a file that takes precedence, e.g. the project's .claude/settings.local.json (ANTHROPIC_BASE_URL=https://api.anthropic.com, a provider switch =0)")
	}
	if inProfile {
		b.WriteString("\n  - profile: remove its base_url, select another profile with ANTHROPIC_PROFILE, or point ANTHROPIC_CONFIG_DIR at a directory without it")
	}
	switch {
	case e.ThirdParty:
		b.WriteString("\nOr give this route ccwrap-owned auth: Claude Code then ignores all of them.")
	case endpoint:
		b.WriteString("\nOr, to use that endpoint, set it up as a ccwrap profile with its own auth.")
	}
	return b.String()
}

// LaunchBypass rejects a launch in which Claude Code keeps its own credentials
// and its own configuration would send its requests past ccwrap: routing env
// in its settings files (ClaudeSettingsBypasses) or an Anthropic SDK profile
// whose base_url leaves the Anthropic API (LaunchAnthropicProfile). With
// ccwrap's placeholder in the child, Claude Code ignores both. It belongs to
// the launch path only: the child's credential env is fixed at spawn, so a
// live profile switch cannot change the answer.
func LaunchBypass(pre *Result, opts Options, inspect *settings.InspectionResult) error {
	if pre == nil || pre.AuthBootstrap == model.AuthBootstrapPlaceholderActive {
		return nil
	}
	bypasses := ClaudeSettingsBypasses(inspect)
	if p := LaunchAnthropicProfile(opts, inspect); p.LeavesAnthropicAPI() {
		bypasses = append(bypasses, p.bypass())
	}
	if len(bypasses) == 0 {
		return nil
	}
	return &RouteBypassError{Bypasses: bypasses, ThirdParty: pre.RouteClass != model.RouteClassFirstParty}
}

// ClaudeSettingsBypasses lists the routing env Claude Code takes from its own
// settings files, merged by its precedence (settings.ClaudeAppliedEnv), that
// moves its API traffic off the Anthropic API: an ANTHROPIC_BASE_URL elsewhere,
// or a CLAUDE_CODE_USE_* provider switch that is not off. ccwrap strips these
// keys from the child env and from its generated settings, but only ccwrap's
// placeholder posture makes Claude Code drop them from its settings files:
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST declares the provider host-managed.
func ClaudeSettingsBypasses(inspect *settings.InspectionResult) []RouteBypass {
	keys := append([]string{"ANTHROPIC_BASE_URL"}, envpolicy.ProviderSelectionKeys()...)
	applied := settings.ClaudeAppliedEnv(inspect, keys)
	var out []RouteBypass
	for _, k := range keys {
		a, ok := applied[k]
		if !ok || !movesClaudeOffAnthropic(k, a.Value) {
			continue
		}
		label := settingsSourceLabel(a.Source)
		b := RouteBypass{Source: label, Where: label + " " + a.Path, What: k}
		if k == "ANTHROPIC_BASE_URL" {
			b.What += " (" + (&AnthropicProfile{BaseURL: strings.TrimSpace(a.Value)}).Target() + ")"
			b.Endpoint = true
		}
		out = append(out, b)
	}
	return out
}

// movesClaudeOffAnthropic reports whether a Claude settings env entry moves
// Claude Code's API traffic off the Anthropic API host ccwrap intercepts: an
// ANTHROPIC_BASE_URL elsewhere, or a provider switch that is not explicitly
// off.
func movesClaudeOffAnthropic(key, value string) bool {
	value = strings.TrimSpace(value)
	if key == "ANTHROPIC_BASE_URL" {
		return leavesAnthropicAPI(value)
	}
	switch strings.ToLower(value) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// settingsSourceLabel names a Claude settings source the way users know it.
func settingsSourceLabel(source string) string {
	switch source {
	case "userSettings":
		return "user settings"
	case "projectSettings":
		return "project settings"
	case "localSettings":
		return "local settings"
	case "policySettings":
		return "managed settings"
	case "globalConfig":
		return "global config"
	}
	return source
}
