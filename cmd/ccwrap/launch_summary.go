package main

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

// wrapHero soft-wraps a sentence at width on word boundaries.
// Returns one entry per visual line (no leading indent — caller adds it).
func wrapHero(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
		} else {
			line += " " + w
		}
	}
	return append(out, line)
}

// authMissingRecoverHint returns the one-line recovery instruction shown when a
// launch proceeds with no usable upstream auth. Shared by the full banner and
// the --quiet one-liner so the wording can't drift.
func authMissingRecoverHint(missingAuthEnv string) string {
	if missingAuthEnv == "" {
		return "edit profiles.json to add auth.key_env, switch profile via inspect, or use `ccwrap --profile inherit-env`"
	}
	return "set $" + missingAuthEnv + ", switch profile via inspect, or use `ccwrap --profile inherit-env`"
}

// quietSummaryLine renders the one-line launch summary used by --quiet /
// CCWRAP_QUIET: where traffic exits (host) plus the inspect URL, with the
// profile name and a degraded marker when relevant. The full multi-row banner
// is suppressed in quiet mode.
func quietSummaryLine(pal ui.Palette, host, profileName string, degraded bool, proxyURL string) string {
	line := pal.Bold("ccwrap") + " → " + host
	if strings.TrimSpace(profileName) != "" {
		line += " · " + profileName
	}
	if degraded {
		line += " · " + pal.Status("degraded")
	}
	line += " · " + pal.Dim("inspect") + " " + proxyURL
	return line
}

func printLaunchSummary(sessionID, proxyURL, upstream string, routeSource model.RouteSource, routeClass model.RouteClass, authMode model.AuthMode, authSource model.AuthSource, authPolicy model.AuthPolicy, authBootstrap model.AuthBootstrap, authBootstrapKind model.AuthBootstrapKind, egressMode, egressSource, egressSummary string, alias modelalias.Config, headers upstreamheaders.Config, pid int, profileName, profileProvider, missingAuthEnv string) {
	pal := ui.New(ui.IsTerminal(os.Stderr))

	// Build a transient session so the hero sentence comes from the
	// single source (ui.SessionPosture) shared with status/web/TUI.
	host := upstream
	if u, err := url.Parse(upstream); err == nil && u.Host != "" {
		host = u.Host
	}
	posture := ui.SessionPosture(&model.Session{
		RouteClass:        routeClass,
		ExactUpstreamHost: host,
		AuthPolicy:        authPolicy,
		ModelAliasCount:   alias.Count(),
		State:             model.StateActive,
	}) // launch summary has no traffic/errors yet

	var state string
	if routeClass == model.RouteClassThirdPartyCompatible || authPolicy == model.AuthPolicyUnsafePassthrough {
		state = pal.Status("ready") + pal.Status(" · degraded")
	} else {
		state = pal.Status("ready")
	}

	// Model alias: 1 → "1 alias · logical → provider";
	// >1 → "N aliases · first → firstval, +N more" (deterministic:
	// keys sorted so output is stable across runs).
	modelsLine := "0 aliases"
	if alias.Enabled() && alias.Count() > 0 {
		keys := aliasKeysForDisplay(alias.Forward)
		first := keys[0]
		if alias.Count() == 1 {
			modelsLine = "1 alias · " + aliasKeyLabel(first) + " → " + alias.Forward[first]
		} else {
			modelsLine = fmt.Sprintf("%d aliases · %s → %s, +%d more",
				alias.Count(), aliasKeyLabel(first), alias.Forward[first], alias.Count()-1)
		}
	}

	lines := []string{pal.Bold("ccwrap") + "  " + state, ""}
	// The hero sentence wraps. cmd/ccwrap has no stderr width detection;
	// wrap at a fixed 76 cols, matching the ASCII block width.
	for _, hl := range wrapHero(posture, 76) {
		lines = append(lines, "  "+hl)
	}
	// auth = HumanAuthPolicy, plus HumanAuthBootstrap ONLY when meaningful
	// (not the suppressed "none"/not-needed). The HumanAuth scheme prefix
	// is intentionally dropped — it duplicated HumanAuthPolicy (first-party
	// passthrough rendered the degenerate "Passthrough · Passthrough ·
	// none") and the scheme is available in --verbose / the Web config
	// drawer.
	// When preflight resolved auth as Missing (active profile asked for
	// ccwrap-owned auth but no source available), the auth row shows a
	// danger-prefixed line + the banner gains a recovery hint at the
	// bottom. Branches on missingAuthEnv emptiness: non-empty = Case A
	// (concrete env), empty = Case B (no source configured).
	authMissing := authBootstrap == model.AuthBootstrapMissing
	authLine := ui.HumanAuthPolicy(authPolicy)
	if authMissing {
		profileLabel := profileName
		if strings.TrimSpace(profileLabel) == "" {
			profileLabel = "(no profile)"
		}
		if missingAuthEnv != "" {
			authLine = "⚠ MISSING — profile " + strconv.Quote(profileLabel) + " needs $" + missingAuthEnv
		} else {
			authLine = "⚠ MISSING — profile " + strconv.Quote(profileLabel) + " has no auth source configured"
		}
	} else if bs := ui.HumanAuthBootstrap(authBootstrap, authBootstrapKind); bs != "" && bs != "none" {
		authLine += " · " + bs
	}
	lines = append(lines, "",
		"  "+pal.Dim("session")+"   "+pal.Cyan(ui.ShortID(sessionID))+" · "+fmt.Sprintf("pid %d", pid),
		// No separate `proxy` row — the proxy endpoint IS the `inspect`
		// endpoint (one row carries that URL, below).
		"  "+pal.Dim("upstream")+"  "+host+" · "+ui.HumanRouteClass(routeClass)+" · "+ui.HumanRouteSource(routeSource),
	)
	// When a profile is active, one extra row names it (name · Provider),
	// in the existing label/`·` grid — no new color/vocabulary.
	// inherit-env (empty name) prints NO profile row: the zero-touch look
	// is unchanged. Never a credential — only the non-secret
	// name/provider.
	if strings.TrimSpace(profileName) != "" {
		profileLine := profileName
		if strings.TrimSpace(profileProvider) != "" {
			profileLine += " · " + profileProvider
		}
		lines = append(lines, "  "+pal.Dim("profile")+"   "+profileLine)
	}
	lines = append(lines,
		"  "+pal.Dim("auth")+"      "+authLine,
		"  "+pal.Dim("models")+"    "+modelsLine,
	)
	if len(headers.Headers) > 0 {
		lines = append(lines, "  "+pal.Dim("headers")+"   "+fmt.Sprintf("%d · %s", len(headers.Headers), headers.Fingerprint))
	}
	if eg := ui.HumanEgress(egressMode, egressSource, egressSummary); eg != "Direct" {
		lines = append(lines, "  "+pal.Dim("egress")+"    "+eg)
	}
	lines = append(lines, "  "+pal.Dim("inspect")+"   "+proxyURL+"  ·  ccwrap dashboard (open in browser)")
	// When launch proceeds with missing auth, a one-time banner-bottom
	// warning lists the concrete recovery options. Single blank line
	// above for visual separation from the inspect row.
	if authMissing {
		lines = append(lines, "",
			"  ⚠ Requests will fail until you "+authMissingRecoverHint(missingAuthEnv)+".",
		)
	}

	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}

// aliasKeysForDisplay sorts alias keys, with the default rule last.
func aliasKeysForDisplay(forward map[string]string) []string {
	keys := make([]string, 0, len(forward))
	for k := range forward {
		if k != modelalias.DefaultKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if _, ok := forward[modelalias.DefaultKey]; ok {
		keys = append(keys, modelalias.DefaultKey)
	}
	return keys
}

func aliasKeyLabel(key string) string {
	if key == modelalias.DefaultKey {
		return "other Claude models"
	}
	return key
}

// launchWarningLines are the banner's config warnings, each with its
// one-line fix: a base URL ending in /v1, then an unaliased starting model.
func launchWarningLines(pre *preflight.Result) []string {
	return append(baseURLLines(pre), modelCoverageLines(pre)...)
}

// baseURLLines warns about a third-party base URL ending in /v1.
func baseURLLines(pre *preflight.Result) []string {
	if pre.RouteClass == model.RouteClassFirstParty {
		return nil
	}
	fixed, ok := preflight.DoubledV1Base(pre.APIBaseURL)
	if !ok {
		return nil
	}
	fix := "set the base URL to " + fixed
	if pre.ActiveProfileName != "" {
		fix = "ccwrap profile edit " + pre.ActiveProfileName + " --base-url " + fixed
	}
	return []string{
		"  ⚠ The base URL ends in /v1, and Claude Code adds /v1 itself: requests would go to /v1/v1/messages.",
		"    Drop the /v1: " + fix,
	}
}

// modelCoverageLines warns that Claude Code starts on a model this route
// has no alias for, and shows the one-line fix.
func modelCoverageLines(pre *preflight.Result) []string {
	u := pre.UncoveredModel
	if u == nil {
		return nil
	}
	target := "<model>"
	if keys := aliasKeysForDisplay(pre.ModelAlias.Forward); len(keys) > 0 {
		target = pre.ModelAlias.Forward[keys[0]]
	}
	fix := fmt.Sprintf("--model-alias '%s=%s'", modelalias.DefaultKey, target)
	if pre.ActiveProfileName != "" {
		fix = "ccwrap profile edit " + pre.ActiveProfileName + " " + fix
	}
	return []string{
		fmt.Sprintf("  ⚠ Claude Code's model %s (from %s) has no alias on this route; the upstream gets that name as-is.", u.Model, u.Source),
		"    Send every unaliased Claude model to one target: " + fix,
	}
}
