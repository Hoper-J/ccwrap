package preflight

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

// Claude Code can also sign in with an Anthropic SDK credential profile, from
// the profile store Anthropic's SDKs and CLIs share: configs/<name>.json under
// ANTHROPIC_CONFIG_DIR, else $XDG_CONFIG_HOME/anthropic, else
// ~/.config/anthropic. A profile may carry its own base_url, and Claude Code
// then sends its API requests there (ANTHROPIC_BASE_URL || base_url) instead
// of to api.anthropic.com, where ccwrap applies the route. No env var is
// needed: the profile named in active_config, else "default", is picked up on
// its own, so the spawn scrub cannot close this path.
//
// Claude Code skips profiles whenever ANTHROPIC_AUTH_TOKEN or
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST is set, and ccwrap's placeholder posture
// sets both. Every other posture leaves the child its own credentials:
// first-party passthrough or override, and third-party routes without
// ccwrap-owned auth.

// anthropicProfileEnvKeys are the env keys Claude Code's profile selection reads.
var anthropicProfileEnvKeys = []string{
	"ANTHROPIC_PROFILE",
	"ANTHROPIC_CONFIG_DIR",
	"XDG_CONFIG_HOME",
	"HOME",
	"ANTHROPIC_FEDERATION_RULE_ID",
	"ANTHROPIC_ORGANIZATION_ID",
}

// AnthropicProfile is an Anthropic SDK credential profile Claude Code can sign
// in with.
type AnthropicProfile struct {
	Name       string
	Path       string // its configs/<name>.json
	SelectedBy string // "ANTHROPIC_PROFILE", "active_config" or "default"
	BaseURL    string // "" when the profile sets none
}

// LeavesAnthropicAPI reports whether the profile sends Claude Code's requests
// somewhere other than an Anthropic API host, i.e. past ccwrap's route.
func (p *AnthropicProfile) LeavesAnthropicAPI() bool {
	return p != nil && leavesAnthropicAPI(p.BaseURL)
}

// leavesAnthropicAPI reports whether a base URL Claude Code would use points
// anywhere but an Anthropic API host, where ccwrap intercepts and routes. One
// without a parsable host counts as leaving; "" means the default, which stays.
func leavesAnthropicAPI(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	u, err := url.Parse(baseURL)
	return err != nil || ClassifyRoute(u) != model.RouteClassFirstParty
}

// Target is the base_url reduced to scheme://host for display, so userinfo or
// query secrets in it never reach a message.
func (p *AnthropicProfile) Target() string {
	if u, err := url.Parse(p.BaseURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "a URL without a host"
}

// ActiveAnthropicProfile mirrors Claude Code's profile selection (2.1.296) over
// env; relative paths resolve against cwd, Claude Code's working directory.
// ANTHROPIC_PROFILE names the profile outright; otherwise workload identity
// federation configured in env (ANTHROPIC_FEDERATION_RULE_ID plus
// ANTHROPIC_ORGANIZATION_ID) wins, and it takes its base URL from
// ANTHROPIC_BASE_URL alone; otherwise the profile named in active_config, else
// "default". A profile counts only with authentication.type oidc_federation,
// or user_oauth with a non-empty credentials file. Returns nil when Claude
// Code has no profile to sign in with.
func ActiveAnthropicProfile(env map[string]string, cwd string) *AnthropicProfile {
	dir := anthropicConfigDir(env, cwd)
	if name := strings.TrimSpace(env["ANTHROPIC_PROFILE"]); name != "" {
		if dir == "" {
			return nil
		}
		return loadAnthropicProfile(dir, name, "ANTHROPIC_PROFILE", cwd)
	}
	if strings.TrimSpace(env["ANTHROPIC_FEDERATION_RULE_ID"]) != "" && strings.TrimSpace(env["ANTHROPIC_ORGANIZATION_ID"]) != "" {
		return nil
	}
	if dir == "" {
		return nil
	}
	name, selectedBy := "default", "default"
	if raw, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil {
		if n := strings.TrimSpace(string(raw)); n != "" {
			name, selectedBy = n, "active_config"
		}
	}
	return loadAnthropicProfile(dir, name, selectedBy, cwd)
}

// LaunchAnthropicProfile is ActiveAnthropicProfile for a launch: the parent
// env overlaid by the trusted Claude settings env, as the child will see it.
func LaunchAnthropicProfile(opts Options, inspect *settings.InspectionResult) *AnthropicProfile {
	env := settings.TrustedEnvValues(opts.ParentEnv, inspect, anthropicProfileEnvKeys)
	return ActiveAnthropicProfile(env, opts.WorkingDirectory)
}

// bypass describes a profile that LeavesAnthropicAPI as a RouteBypass.
func (p *AnthropicProfile) bypass() RouteBypass {
	return RouteBypass{
		Source:   "Anthropic profile",
		Where:    fmt.Sprintf("Anthropic profile %q (%s, %s)", p.Name, p.Path, p.selection()),
		What:     "base_url " + p.Target(),
		Profile:  true,
		Endpoint: true,
	}
}

func (p *AnthropicProfile) selection() string {
	switch p.SelectedBy {
	case "ANTHROPIC_PROFILE":
		return "selected by ANTHROPIC_PROFILE"
	case "active_config":
		return "named in active_config"
	default:
		return "the default profile"
	}
}

func anthropicConfigDir(env map[string]string, cwd string) string {
	dir := strings.TrimSpace(env["ANTHROPIC_CONFIG_DIR"])
	if dir == "" {
		if xdg := strings.TrimSpace(env["XDG_CONFIG_HOME"]); xdg != "" {
			dir = filepath.Join(xdg, "anthropic")
		} else if home := strings.TrimSpace(env["HOME"]); home != "" {
			dir = filepath.Join(home, ".config", "anthropic")
		}
	}
	return underDir(cwd, dir)
}

func underDir(cwd, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

func loadAnthropicProfile(dir, name, selectedBy, cwd string) *AnthropicProfile {
	path := filepath.Join(dir, "configs", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		BaseURL        any `json:"base_url"`
		Authentication struct {
			Type            any `json:"type"`
			CredentialsPath any `json:"credentials_path"`
		} `json:"authentication"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	switch doc.Authentication.Type {
	case "oidc_federation":
	case "user_oauth":
		creds := filepath.Join(dir, "credentials", name+".json")
		if doc.Authentication.CredentialsPath != nil {
			p, ok := doc.Authentication.CredentialsPath.(string)
			if !ok {
				return nil
			}
			creds = underDir(cwd, p)
		}
		if b, err := os.ReadFile(creds); err != nil || strings.TrimSpace(string(b)) == "" {
			return nil
		}
	default:
		return nil
	}
	base, _ := doc.BaseURL.(string)
	return &AnthropicProfile{Name: name, Path: path, SelectedBy: selectedBy, BaseURL: strings.TrimSpace(base)}
}
