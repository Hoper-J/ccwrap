package preflight

import (
	"net/url"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

// UncoveredModel is Claude Code's starting model when the route maps model
// names but has no alias (or default) for it: its requests reach the
// upstream under the Claude name.
type UncoveredModel struct {
	Model  string
	Source string // "--model", "ANTHROPIC_MODEL", or e.g. "user settings"
}

// ClaudeMainModel is the model Claude Code starts with, by its own
// precedence: --model, then ANTHROPIC_MODEL, then the "model" setting, and
// where it came from ("user settings", ...). "" means Claude Code's built-in
// default, which ccwrap cannot see.
func ClaudeMainModel(childArgs []string, modelEnv map[string]string, inspect *settings.InspectionResult) (value, source string) {
	if v := modelFlag(childArgs); v != "" {
		return v, "--model"
	}
	if v := strings.TrimSpace(modelEnv["ANTHROPIC_MODEL"]); v != "" {
		return v, "ANTHROPIC_MODEL"
	}
	value, source = settings.ClaudeModelSetting(inspect)
	return value, settingsSourceLabel(source)
}

func modelFlag(args []string) string {
	var value string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return value
		case a == "--model" && i+1 < len(args):
			value = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(a, "--model="):
			value = strings.TrimSpace(strings.TrimPrefix(a, "--model="))
		}
	}
	return value
}

func uncoveredMainModel(routeClass model.RouteClass, alias modelalias.Config, childArgs []string, modelEnv map[string]string, inspect *settings.InspectionResult) *UncoveredModel {
	if routeClass == model.RouteClassFirstParty || !alias.Enabled() {
		return nil
	}
	value, source := ClaudeMainModel(childArgs, modelEnv, inspect)
	if value == "" || alias.Covers(value) {
		return nil
	}
	return &UncoveredModel{Model: value, Source: source}
}

// DoubledV1Base reports a third-party base URL ending in /v1. Claude Code
// appends /v1/messages to its base URL, so such a base sends requests to
// /v1/v1/messages (gateway docs often quote the OpenAI-style /v1 form).
// fixed is the base without the /v1 and without userinfo.
func DoubledV1Base(base *url.URL) (fixed string, ok bool) {
	if base == nil {
		return "", false
	}
	p := strings.TrimRight(base.Path, "/")
	if !strings.HasSuffix(p, "/v1") {
		return "", false
	}
	u := *base
	u.User = nil
	u.Path = strings.TrimSuffix(p, "/v1")
	u.RawPath = ""
	return u.String(), true
}
