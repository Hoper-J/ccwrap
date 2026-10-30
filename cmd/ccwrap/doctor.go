package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/certs"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/envpolicy"
	"github.com/Hoper-J/ccwrap/internal/localpeer"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/update"
)

// doctorChecks collects doctor rows in the order the checks run. Any fail
// makes the overall status fail; otherwise any warn makes it warn.
type doctorChecks struct {
	report  model.DoctorReport
	verbose bool
}

func (d *doctorChecks) addWithFields(name, status, summary, detail string, fields map[string]any) {
	d.report.Checks = append(d.report.Checks, model.DoctorCheck{Name: name, Status: status, Summary: summary, Detail: detail, Fields: fields})
	if status == "fail" {
		d.report.Overall = "fail"
	} else if status == "warn" && d.report.Overall == "ok" {
		d.report.Overall = "warn"
	}
}

func (d *doctorChecks) add(name, status, summary, detail string) {
	d.addWithFields(name, status, summary, detail, nil)
}

func doctorCommand(paths app.Paths, args []string) error {
	flagArgs, childArgs := splitDoubleDash(args)
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	verbose := fs.Bool("verbose", false, "verbose details")
	sessionID := fs.String("session", "", "inspect a specific session")
	egressProxy := fs.String("egress-proxy", "auto", "effective supervisor egress proxy mode/url for launch-contract checks (auto, direct, or absolute proxy URL)")
	profileFlag := fs.String("profile", "", "resolve as this profile would launch (default: profiles.json default; `inherit-env` forces the env-only path)")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	d := &doctorChecks{report: model.DoctorReport{Overall: "ok"}, verbose: *verbose}
	d.runtimeChecks(paths)
	parentEnv := os.Environ()
	envMap := preflight.ParentEnvMap(parentEnv)
	d.parentEnvChecks(envMap)
	profileInput, profileErr := d.profileCheck(paths, *profileFlag)
	inspect, inspectErr := settings.InspectLaunch(cwd, childArgs)
	if inspectErr != nil {
		d.add("settings_inspection", "fail", "unable to inspect Claude settings", inspectErr.Error())
		d.add("egress_proxy", "fail", "failed to resolve effective egress proxy", inspectErr.Error())
	} else {
		routeClass := d.routeChecks(parentEnv, envMap, *egressProxy, profileInput, inspect)
		d.settingsChecks(envMap, routeClass, inspect)
	}
	launchOpts := preflight.Options{
		Profile:          profileInput,
		EgressProxy:      *egressProxy,
		ParentEnv:        parentEnv,
		WorkingDirectory: cwd,
		ChildArgs:        childArgs,
	}
	d.launchChecks(launchOpts, profileErr, inspect, inspectErr, paths.StateDir)
	d.sessionChecks(paths, *sessionID)
	if *jsonOut {
		return printJSON(d.report)
	}
	printDoctorReport(d.report, *verbose)
	return nil
}

// runtimeChecks covers the local machine: directories, CA, ports, updates.
func (d *doctorChecks) runtimeChecks(paths app.Paths) {
	if err := app.EnsurePaths(paths); err != nil {
		d.add("paths", "fail", "runtime/state directories unavailable", err.Error())
	} else {
		d.add("paths", "pass", "runtime and state directories ready", maybeDetail(d.verbose, fmt.Sprintf("runtime=%s state=%s", paths.RuntimeDir, paths.StateDir)))
	}
	ca := certs.NewManager(paths)
	if err := ca.EnsureCA(); err != nil {
		d.add("ca", "fail", "local CA unavailable", err.Error())
	} else {
		status := ca.BundleStatus()
		detail := fmt.Sprintf("mode=%s bundle=%s system_roots=%t ccwrap_root=%t", status.Mode, status.BundlePath, status.HasSystemCA, status.HasCCWRAPRoot)
		fields := map[string]any{
			"mode":         status.Mode,
			"bundle_path":  status.BundlePath,
			"system_roots": status.HasSystemCA,
			"ccwrap_root":  status.HasCCWRAPRoot,
		}
		if status.Mode == "ccwrap_only" || !status.HasSystemCA {
			reason := status.SystemCAWarning
			if reason == "" {
				reason = "system CA bundle not discoverable on this host"
			}
			fields["system_ca_warning"] = reason
			warnDetail := reason + "; child tools (curl, git, python) will trust only the ccwrap root. Install OS trust-store roots or set CCWRAP_SYSTEM_CA_BUNDLE to restore public-host TLS."
			if d.verbose {
				warnDetail = detail + " ; " + warnDetail
			}
			d.addWithFields("ca", "warn", "local CA bundle ready but falling back to ccwrap-only (no system roots)", warnDetail, fields)
		} else {
			d.addWithFields("ca", "pass", "local composite CA bundle is ready", maybeDetail(d.verbose, detail), fields)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		d.add("session_listener", "fail", "session proxy port cannot bind", err.Error())
	} else {
		_ = ln.Close()
		d.add("session_listener", "pass", "session proxy port can bind", "")
	}
	addLocalAccessCheck(d.add, os.Getenv, localpeer.SelfTest)
	// Update awareness: cache-only read — doctor never hits the network
	// for this row; `ccwrap version --check` / `ccwrap upgrade` are the
	// explicit network probes.
	if update.Disabled(os.Getenv, false) {
		d.add("update", "pass", "update check disabled (CCWRAP_NO_UPDATE_CHECK)", "")
	} else if c, ok := update.LoadCache(paths.StateDir); !ok {
		d.add("update", "pass", "no update-check cache yet (first check runs on next launch)", "")
	} else {
		age := time.Since(c.CheckedAt).Round(time.Minute)
		cur := versionString()
		if update.Eligible(cur) && update.Newer(cur, c.Latest) {
			d.add("update", "warn", fmt.Sprintf("update available: %s → %s (run `ccwrap upgrade`)", cur, c.Latest), maybeDetail(d.verbose, fmt.Sprintf("checked %s ago", age)))
		} else if !update.Eligible(cur) {
			d.add("update", "pass", fmt.Sprintf("update notices off for dev build (%s)", cur), maybeDetail(d.verbose, fmt.Sprintf("latest=%s checked %s ago", c.Latest, age)))
		} else {
			d.add("update", "pass", fmt.Sprintf("ccwrap %s is current", cur), maybeDetail(d.verbose, fmt.Sprintf("latest=%s checked %s ago", c.Latest, age)))
		}
	}
}

// parentEnvChecks covers the shell env ccwrap was started from.
func (d *doctorChecks) parentEnvChecks(envMap map[string]string) {
	if upstream := strings.TrimSpace(envMap["ANTHROPIC_BASE_URL"]); upstream != "" {
		if u, _, err := preflight.ResolveAPIBase("", upstream); err != nil {
			d.add("inherited_upstream", "fail", "ANTHROPIC_BASE_URL is invalid", err.Error())
		} else {
			d.add("inherited_upstream", "pass", "ANTHROPIC_BASE_URL is valid and will be captured", maybeDetail(d.verbose, u.String()))
		}
	} else {
		d.add("inherited_upstream", "pass", "no inherited ANTHROPIC_BASE_URL; fallback default will be used", "")
	}
	// Provider-selection transparency: ccwrap scrubs every CLAUDE_CODE_USE_*
	// provider switch from the child env to keep getAPIProvider()==='firstParty'
	// (the first-party posture the gateway path depends on). When a user has set
	// one truthy intending another provider, surface that the selection is
	// being overridden — ccwrap does not route to those providers.
	var providerSelect []string
	for _, k := range envpolicy.ProviderSelectionKeys() {
		if v := strings.ToLower(strings.TrimSpace(envMap[k])); v == "1" || v == "true" || v == "yes" || v == "on" {
			providerSelect = append(providerSelect, k)
		}
	}
	if len(providerSelect) > 0 {
		d.add("provider_selection", "pass", "provider-selection env (Bedrock/Vertex/Foundry/…) is set but will be overridden to keep the first-party posture; ccwrap does not route to those providers", strings.Join(providerSelect, ", "))
	} else {
		d.add("provider_selection", "pass", "no provider-selection env (Bedrock/Vertex/Foundry/…) to override", "")
	}
	unsupported := preflight.DetectUnsupportedEnv(envMap)
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		d.add("unsupported_env", "fail", "unsupported auth/tunnel env detected", strings.Join(unsupported, ", "))
	} else {
		d.add("unsupported_env", "pass", "no unsupported file-descriptor, unix-socket, or custom oauth env detected", "")
	}
	if mode, source, _, err := preflight.ResolveAuth(envMap); err != nil {
		d.add("parent_env_auth_sources", "fail", "conflicting parent-env upstream auth sources detected", err.Error())
	} else {
		d.add("parent_env_auth_sources", "pass", "parent-env upstream auth sources resolved", maybeDetail(d.verbose, fmt.Sprintf("mode=%s source=%s", mode, source)))
	}
}

// profileCheck resolves the SAME profile overlay an actual launch would apply
// (--profile, else the profiles.json default, else inherit-env) so the
// effective-upstream / auth / hidden-contract / launch-contract checks reflect
// what `ccwrap` would really do — not the env-only path.
func (d *doctorChecks) profileCheck(paths app.Paths, requested string) (*preflight.ProfileInput, error) {
	profileInput, err := resolveLaunchProfile(profiles.DefaultPath(paths.StateDir), requested)
	if err != nil {
		d.add("profile", "fail", "could not resolve the requested launch profile", err.Error())
	} else if profileInput == nil {
		d.add("profile", "pass", "inherit-env (no profile overlay; env + Claude settings only)", maybeDetail(d.verbose, fallbackText(strings.TrimSpace(requested), "no --profile, no profiles.json default")))
	} else {
		upstream := strings.TrimSpace(profileInput.BaseURL)
		d.add("profile", "pass", fmt.Sprintf("launch profile overlay: %s", profileInput.Name), maybeDetail(d.verbose, fmt.Sprintf("base_url=%s owns_auth=%t", fallbackText(upstream, "(inherit)"), profileInput.Auth != nil)))
	}
	return profileInput, err
}

// routeChecks resolves where requests go and who supplies the credential,
// as the launch would. It returns the route class for the settings checks.
func (d *doctorChecks) routeChecks(parentEnv []string, envMap map[string]string, egressProxy string, profileInput *preflight.ProfileInput, inspect *settings.InspectionResult) model.RouteClass {
	routeClass := model.RouteClassFirstParty
	authMode := model.AuthModePassthrough
	profileUpstream := ""
	if profileInput != nil {
		profileUpstream = strings.TrimSpace(profileInput.BaseURL)
	}
	egressCfg, notes, err := preflight.ResolveEgressFromInspection(egressProxy, parentEnv, inspect)
	if err != nil {
		d.add("egress_proxy", "fail", "failed to resolve effective egress proxy", err.Error())
	} else {
		detail := egress.Summary(egressCfg)
		if len(notes) > 0 {
			detail += " ; " + strings.Join(notes, "; ")
		}
		d.add("egress_proxy", "pass", "effective supervisor egress proxy configuration resolved", maybeDetail(d.verbose, detail))
	}
	active := strings.Join(inspect.ActiveSources, ", ")
	if active == "" {
		active = "none"
	}
	d.add("active_setting_sources", "pass", "active Claude setting sources resolved", maybeDetail(d.verbose, active))
	if u, routeSource, configSource, err := preflight.ResolveAPIBaseFromInspection(profileUpstream, parentEnv, inspect); err != nil {
		d.add("effective_upstream", "fail", "effective provider upstream is invalid", err.Error())
	} else {
		routeClass = preflight.ClassifyRoute(u)
		d.add("effective_upstream", "pass", "effective provider upstream resolved", maybeDetail(d.verbose, fmt.Sprintf("url=%s route=%s source=%s class=%s", u.String(), routeSource, configSource, routeClass)))
	}
	if profileInput != nil && profileInput.Auth != nil {
		// The active profile OWNS upstream auth — that beats any ambient/settings
		// source at launch, so the effective auth is the profile's, not passthrough.
		if strings.EqualFold(strings.TrimSpace(profileInput.Auth.Mode), "ccwrap_x_api_key") {
			authMode = model.AuthModeOverrideXAPIKey
		} else {
			authMode = model.AuthModeOverrideBearer
		}
		d.add("auth_sources", "pass", "upstream auth is owned by the active profile", maybeDetail(d.verbose, fmt.Sprintf("mode=%s source=profile:%s", authMode, profileInput.Name)))
	} else if mode, source, configSource, _, err := preflight.ResolveAuthFromInspection(parentEnv, inspect); err != nil {
		d.add("auth_sources", "fail", "conflicting effective upstream auth sources detected", err.Error())
	} else {
		authMode = mode
		d.add("auth_sources", "pass", "effective upstream auth source resolved", maybeDetail(d.verbose, fmt.Sprintf("mode=%s source=%s config_source=%s", mode, source, configSource)))
	}
	ccwrapUpstream := strings.TrimRight(strings.TrimSpace(envMap["CCWRAP_UPSTREAM"]), "/")
	var anthBase, anthSource string
	if provider, _ := settings.EffectiveProviderEnvFromInspection(parentEnv, inspect); provider != nil {
		anthBase = strings.TrimRight(strings.TrimSpace(provider.Env["ANTHROPIC_BASE_URL"]), "/")
		if provider.KeySources != nil {
			anthSource = provider.KeySources["ANTHROPIC_BASE_URL"]
		}
	}
	if ccwrapUpstream != "" && anthBase != "" && !strings.EqualFold(ccwrapUpstream, anthBase) {
		detail := fmt.Sprintf("CCWRAP_UPSTREAM wins; ANTHROPIC_BASE_URL (source=%s) is silently ignored", fallbackText(anthSource, "unknown"))
		d.add("upstream_inputs", "warn", "CCWRAP_UPSTREAM and ANTHROPIC_BASE_URL set with different values", detail)
	} else {
		d.add("upstream_inputs", "pass", "no CCWRAP_UPSTREAM / ANTHROPIC_BASE_URL conflict", "")
	}
	if routeClass == model.RouteClassThirdPartyHidden && authMode == model.AuthModePassthrough {
		d.add("hidden_auth_contract", "fail", "third-party hidden mode requires ccwrap-owned upstream auth", "Refusing to forward Claude-side authentication to third-party upstream. Provide ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN as compatibility input or CCWRAP_UPSTREAM_* when available.")
	} else if routeClass == model.RouteClassThirdPartyHidden {
		d.add("hidden_auth_contract", "pass", "third-party hidden auth will be ccwrap-owned and fail-closed", maybeDetail(d.verbose, fmt.Sprintf("auth_mode=%s bootstrap=placeholder", authMode)))
	} else {
		d.add("hidden_auth_contract", "pass", "first-party route does not require hidden auth fail-closed", "")
	}
	return routeClass
}

// settingsChecks flags Claude settings that ccwrap overrides, drops, or refuses.
func (d *doctorChecks) settingsChecks(envMap map[string]string, routeClass model.RouteClass, inspect *settings.InspectionResult) {
	if len(inspect.UnsupportedEnv) > 0 {
		d.add("settings_unsupported_env", "fail", "unsupported auth/tunnel env detected in Claude settings", formatEnvConflicts(inspect.UnsupportedEnv))
	} else {
		d.add("settings_unsupported_env", "pass", "no unsupported auth/tunnel env in active Claude settings", "")
	}
	if len(inspect.MalformedEnv) > 0 {
		d.add("settings_malformed_env", "fail", "malformed env object detected in active Claude settings", formatMalformedEnvIssues(inspect.MalformedEnv))
	} else {
		d.add("settings_malformed_env", "pass", "no malformed env objects in active Claude settings", "")
	}
	if len(inspect.OverriddenNetworkEnv) > 0 {
		d.add("settings_overridden_network_env", "pass", "ccwrap will override network/trust env from non-policy Claude settings", formatEnvConflicts(inspect.OverriddenNetworkEnv))
	} else {
		d.add("settings_overridden_network_env", "pass", "no overridden network/trust env in non-policy Claude settings", "")
	}
	if len(inspect.PolicyNetworkEnv) > 0 {
		detail := formatEnvConflicts(inspect.PolicyNetworkEnv) + "; detectable local/cache policy-managed network/trust env are unsupported with stock Claude Code under ccwrap; ccwrap only inspects local/cache policy sources pre-launch, and remote managed settings, MDM, and HKCU may still exist and remain unsupported; move proxy settings to --egress-proxy, launcher shell env, or non-policy user/global Claude settings, install enterprise CA trust in the host OS trust store, and remove these keys from managed policy settings"
		d.add("settings_policy_network_env", "fail", "detectable local/cache policy-managed network/trust env are unsupported with stock Claude Code under ccwrap", detail)
	} else {
		d.add("settings_policy_network_env", "pass", "no detectable local/cache policy-managed network/trust env in active Claude settings", maybeDetail(d.verbose, "Best-effort local/cache inspection only; remote managed settings, MDM, and HKCU may still exist and remain unsupported."))
	}
	if len(inspect.APIKeyHelperHits) > 0 {
		detail := "apiKeyHelper is a Claude-side auth path. It is blocked for third-party hidden mode; future ccwrap-side upstreamAuthHelper must be owned and executed by ccwrap only."
		if d.verbose {
			detail += " sources=" + strings.Join(inspect.APIKeyHelperHits, ", ")
		}
		if routeClass == model.RouteClassThirdPartyHidden {
			d.add("api_key_helper", "fail", "apiKeyHelper is blocked in third-party hidden mode", detail)
		} else {
			d.add("api_key_helper", "warn", "Claude-side dynamic auth helper detected", detail)
		}
	} else {
		d.add("api_key_helper", "pass", "no apiKeyHelper detected in active Claude settings", "")
	}
	if len(inspect.DangerousShellSettings) > 0 {
		detail := "Claude shell-exec settings run commands at request time and can leak credentials or inject auth-like headers. They are blocked for third-party hidden mode."
		if d.verbose {
			detail += " sources=" + formatEnvConflicts(inspect.DangerousShellSettings)
		}
		if routeClass == model.RouteClassThirdPartyHidden {
			d.add("dangerous_shell_settings", "fail", "shell-exec settings are blocked in third-party hidden mode", detail)
		} else {
			d.add("dangerous_shell_settings", "warn", "Claude shell-exec settings detected", detail)
		}
	} else {
		d.add("dangerous_shell_settings", "pass", "no non-apiKeyHelper shell-exec settings detected", "")
	}
	if len(inspect.CCWRAPInternalEnvHits) > 0 {
		detail := "CCWRAP_* env keys in Claude settings are launcher-process inputs only; they are silently dropped from the generated session settings file. Set them in the shell or systemd unit instead."
		if d.verbose {
			detail += " sources=" + formatEnvConflicts(inspect.CCWRAPInternalEnvHits)
		}
		d.add("ccwrap_internal_keys_in_settings", "warn", "CCWRAP_* env keys in Claude settings are silently dropped", detail)
	} else {
		d.add("ccwrap_internal_keys_in_settings", "pass", "no CCWRAP_* env keys in Claude settings", "")
	}
	customHeaderHits := inspect.CustomAuthHeaderEnv
	if inherited := inheritedCustomHeadersDoctorConflict(envMap); len(inherited) > 0 {
		customHeaderHits = append(customHeaderHits, inherited...)
	}
	if len(customHeaderHits) > 0 {
		detail := "ANTHROPIC_CUSTOM_HEADERS contains auth-like header names; values are not shown. These headers are blocked for third-party hidden mode; use ccwrap-owned upstream headers when available."
		if d.verbose {
			detail += " sources=" + formatEnvConflicts(customHeaderHits)
		}
		if routeClass == model.RouteClassThirdPartyHidden {
			d.add("custom_headers_auth", "fail", "auth-like ANTHROPIC_CUSTOM_HEADERS are blocked in third-party hidden mode", detail)
		} else {
			d.add("custom_headers_auth", "warn", "auth-like headers detected in ANTHROPIC_CUSTOM_HEADERS", detail)
		}
	} else {
		d.add("custom_headers_auth", "pass", "no auth-like ANTHROPIC_CUSTOM_HEADERS detected", "")
	}
	if inspect.ParsedFlagSettings != nil && inspect.ParsedFlagSettings.OriginalArgValue != "" {
		summary := inspect.ParsedFlagSettings.Path
		if inspect.ParsedFlagSettings.Inline {
			summary = "inline JSON"
		}
		d.add("flag_settings", "pass", "Claude --settings flag will be merged into a ccwrap session settings file", maybeDetail(d.verbose, summary))
	} else {
		d.add("flag_settings", "pass", "no user-provided Claude --settings flag detected", "")
	}
}

// launchChecks runs the launch preflight and the checks that need its result.
func (d *doctorChecks) launchChecks(launchOpts preflight.Options, profileErr error, inspect *settings.InspectionResult, inspectErr error, stateDir string) {
	var launchPre *preflight.Result
	if profileErr != nil {
		d.add("launch_contract", "fail", "current launch would be rejected: could not resolve the launch profile", profileErr.Error())
	} else if pre, err := preflight.RunWithInspection(launchOpts, inspect); err != nil {
		d.add("launch_contract", "fail", "current launch would be rejected by ccwrap preflight", err.Error())
	} else {
		launchPre = pre
		if err := preflight.LaunchBypass(pre, launchOpts, inspect); err != nil {
			d.add("launch_contract", "fail", "current launch would be rejected: Claude Code's own config would take its requests past ccwrap", err.Error())
		} else {
			d.add("launch_contract", "pass", "current launch satisfies ccwrap MITM preflight checks", "")
		}
	}
	// A third-party base URL that can't work: an OpenAI-compatible endpoint,
	// or one ending in /v1 (the version segment doubles).
	if launchPre != nil && launchPre.RouteClass != model.RouteClassFirstParty {
		if lines := baseURLLines(launchPre); len(lines) > 0 {
			d.add("upstream_path", "warn", strings.TrimPrefix(strings.TrimSpace(lines[0]), "⚠ "), strings.TrimSpace(lines[len(lines)-1]))
		} else {
			d.add("upstream_path", "pass", "the base URL leaves the /v1 prefix to Claude Code", "")
		}
	}
	// On a route that maps model names, Claude Code's starting model needs an
	// alias, or the upstream gets the Claude name.
	if launchPre != nil {
		alias := launchPre.ModelAlias
		mainModel, mainSource := preflight.ClaudeMainModel(launchPre.RewrittenChildArgs, launchPre.ModelEnv, inspect)
		seen := seenUnaliasedModels(launchPre, stateDir)
		switch {
		case launchPre.RouteClass == model.RouteClassFirstParty || !alias.Enabled():
			d.add("model_aliases", "pass", "this route passes Claude Code's model names through unchanged", "")
		case launchPre.UncoveredModel != nil:
			u := launchPre.UncoveredModel
			d.add("model_aliases", "warn", fmt.Sprintf("Claude Code's model %s (from %s) has no alias on this route", u.Model, u.Source), trimLines(aliasFixLines(alias, launchPre.ActiveProfileName)))
		case len(seen) > 0:
			d.add("model_aliases", "warn", fmt.Sprintf("Claude Code sent %s with no alias on %s before", strings.Join(seen, ", "), launchPre.ActiveProfileName), trimLines(aliasFixLines(alias, launchPre.ActiveProfileName)))
		case alias.Default() != "":
			d.add("model_aliases", "pass", "Claude models without their own alias go to "+alias.Default(), "")
		case alias.KeepsNames():
			d.add("model_aliases", "pass", "Claude models without their own alias keep their Claude names on this route", "")
		case mainModel != "":
			d.add("model_aliases", "pass", fmt.Sprintf("Claude Code's model %s (from %s) has an alias on this route", mainModel, mainSource), "")
		default:
			d.add("model_aliases", "pass", "Claude Code starts on its built-in default model; any Claude model without an alias reaches the upstream unchanged", "")
		}
	}
	// Anthropic SDK credential profiles are a sign-in (and base URL) source
	// Claude Code reads on its own; see preflight.LaunchBypass.
	switch p := preflight.LaunchAnthropicProfile(launchOpts, inspect); {
	case p == nil:
		d.add("anthropic_profile", "pass", "Claude Code has no Anthropic SDK profile to sign in with", "")
	case !p.LeavesAnthropicAPI():
		d.add("anthropic_profile", "pass", fmt.Sprintf("Claude Code can sign in with Anthropic profile %q; its requests stay on the Anthropic API", p.Name), maybeDetail(d.verbose, p.Path))
	case launchPre != nil && launchPre.AuthBootstrap == model.AuthBootstrapPlaceholderActive:
		d.add("anthropic_profile", "pass", fmt.Sprintf("Anthropic profile %q sets base_url %s, which Claude Code ignores on this route: ccwrap supplies the credential", p.Name, p.Target()), maybeDetail(d.verbose, p.Path))
	default:
		d.add("anthropic_profile", "fail", fmt.Sprintf("Anthropic profile %q sets base_url %s: Claude Code can send its requests there, past ccwrap", p.Name, p.Target()), maybeDetail(d.verbose, p.Path))
	}
	// So can routing env in Claude Code's own settings files; see
	// preflight.LaunchBypass.
	if inspectErr == nil {
		bypasses := preflight.ClaudeSettingsBypasses(inspect)
		var found, files []string
		for _, b := range bypasses {
			found = append(found, b.What+" in "+b.Source)
			files = append(files, b.String())
		}
		switch {
		case len(bypasses) == 0:
			d.add("claude_settings_route", "pass", "no Claude Code settings move its requests off the Anthropic API", "")
		case launchPre != nil && launchPre.AuthBootstrap == model.AuthBootstrapPlaceholderActive:
			d.add("claude_settings_route", "pass", "Claude Code ignores these on this route, as ccwrap supplies the credential: "+strings.Join(found, ", "), maybeDetail(d.verbose, strings.Join(files, "\n")))
		default:
			d.add("claude_settings_route", "fail", "Claude Code's settings would send its requests past ccwrap: "+strings.Join(found, ", "), strings.Join(files, "\n"))
		}
	}
}

// sessionChecks scans session manifests and, with --session, checks that one.
func (d *doctorChecks) sessionChecks(paths app.Paths, sessionID string) {
	discovered, err := discovery.Scan(paths)
	if err != nil {
		d.add("discovery", "fail", "unable to scan session manifests", err.Error())
	} else {
		stale := 0
		for _, ds := range discovered {
			if ds.Stale || !ds.Reachable {
				stale++
			}
		}
		d.add("discovery", "pass", fmt.Sprintf("found %d session manifests", len(discovered)), maybeDetail(d.verbose, fmt.Sprintf("reachable=%d stale=%d", len(discovered)-stale, stale)))
	}
	if sessionID != "" {
		ds, err := discovery.Find(paths, sessionID)
		if err != nil {
			d.add("session", "fail", "session manifest not found", err.Error())
		} else if ds.Stale {
			d.add("session", "warn", "session manifest is stale", ds.Error)
		} else if ds.Reachable {
			d.add("session", "pass", "session control socket is reachable", maybeDetail(d.verbose, ds.Manifest.ControlSocket))
		} else {
			d.add("session", "warn", "session manifest exists but control socket is not reachable", ds.Error)
		}
	}
}

// printDoctorReport prints the checks by group, then an overall line naming
// every failing or warning check.
func printDoctorReport(report model.DoctorReport, verbose bool) {
	pal := ui.New(ui.IsTerminal(os.Stdout))

	passed, warned, failed := 0, 0, 0
	for _, c := range report.Checks {
		switch c.Status {
		case "pass":
			passed++
		case "warn":
			warned++
		case "fail":
			failed++
		}
	}
	fmt.Printf("%s — %s · %s · %s\n",
		pal.Bold("ccwrap doctor"),
		pal.Green(fmt.Sprintf("%d passed", passed)),
		pal.Yellow(fmt.Sprintf("%d warn", warned)),
		pal.Red(fmt.Sprintf("%d fail", failed)),
	)

	byGroup := map[string][]model.DoctorCheck{}
	for _, c := range report.Checks {
		g := doctorGroupForCheck(c.Name)
		byGroup[g] = append(byGroup[g], c)
	}
	nameW := 0
	for _, c := range report.Checks {
		if len(c.Name) > nameW {
			nameW = len(c.Name)
		}
	}
	for _, g := range doctorGroupOrder {
		checks := byGroup[g]
		if len(checks) == 0 {
			continue
		}
		fmt.Printf("\n  %s\n", pal.Bold(g))
		for _, c := range checks {
			fmt.Printf("    %s %-*s  %s\n", ui.StatusGlyph(pal, c.Status), nameW, c.Name, c.Summary)
			if verbose && c.Detail != "" {
				pad := "      " + strings.Repeat(" ", nameW)
				fmt.Printf("%s%s\n", pad, strings.ReplaceAll(c.Detail, "\n", "\n"+pad))
			}
		}
	}

	// Overall: is ACTIONABLE — it names the specific failing/warning
	// checks, not a generic "see above".
	var warnNames, failNames []string
	for _, c := range report.Checks {
		switch c.Status {
		case "warn":
			warnNames = append(warnNames, c.Name)
		case "fail":
			failNames = append(failNames, c.Name)
		}
	}
	switch report.Overall {
	case "ok":
		fmt.Printf("\nOverall: %s — all checks passed.\n", pal.Green("ok"))
	case "warn":
		fmt.Printf("\nOverall: %s — %d warning(s): %s. Run with --verbose for remediation detail.\n",
			pal.Yellow("warn"), len(warnNames), strings.Join(warnNames, ", "))
	default:
		fmt.Printf("\nOverall: %s — %d failure(s): %s. Resolve before launch (--verbose for detail).\n",
			pal.Red("fail"), len(failNames), strings.Join(failNames, ", "))
	}
}

func inheritedCustomHeadersDoctorConflict(env map[string]string) []settings.EnvConflict {
	if len(settings.AuthLikeCustomHeaderNames(env["ANTHROPIC_CUSTOM_HEADERS"])) == 0 {
		return nil
	}
	return []settings.EnvConflict{{Source: "inherited_env", Path: "process env", Keys: []string{"ANTHROPIC_CUSTOM_HEADERS"}}}

}

func formatEnvConflicts(conflicts []settings.EnvConflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", conflict.Source, conflict.Path, strings.Join(conflict.Keys, ", ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func formatMalformedEnvIssues(issues []settings.MalformedEnvIssue) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", issue.Source, issue.Path, issue.Error))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func maybeDetail(verbose bool, detail string) string {
	if !verbose {
		return ""
	}
	return detail
}

func fallbackText(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// trimLines joins indented output lines into a doctor detail.
func trimLines(lines []string) string {
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}
