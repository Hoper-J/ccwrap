package preflight

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/envpolicy"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

// ProfileInput is the per-field overlay a selected provider profile
// contributes to resolution. A nil *ProfileInput means inherit-env:
// resolution is byte-identical to ccwrap's pre-feature env-only path.
//
// Per-field precedence (highest→lowest): an explicit
// individual flag (already populated into the matching Options field
// by cmd/ccwrap) wins; then this overlay; then env; then the fallback
// default. ResolveProfile therefore consults the overlay for a field
// ONLY when the corresponding explicit Options field is empty.
//
// Auth mirrors the schema's pointer shape (profiles.Profile.Auth):
// nil = "ccwrap does not own auth for this profile" (no header injected);
// non-nil = ccwrap injects an auth header per Mode/Key/KeyEnv.
type ProfileInput struct {
	Name            string
	Provider        string
	BaseURL         string
	Auth            *AuthSpec
	ModelAliases    map[string]string
	UpstreamHeaders map[string]string
	EgressMode      string // "" | "inherit" | "direct" | "http"
	EgressURL       string
}

// AuthSpec is the preflight-side mirror of profiles.AuthSpec. Kept as
// a separate type so the runtime layer can evolve independently of the
// on-disk schema (e.g. future fields exposed only to resolution). Mode
// is one of "" | "ccwrap_bearer" | "ccwrap_x_api_key" — "passthrough" is
// rejected at schema-validate time.
type AuthSpec struct {
	Mode   string
	Key    string
	KeyEnv string
}

// profilesAuthToPreflight converts a schema AuthSpec into a preflight
// AuthSpec. nil-in nil-out propagates the "no auth ownership" posture
// across the package boundary.
func profilesAuthToPreflight(in *profiles.AuthSpec) *AuthSpec {
	if in == nil {
		return nil
	}
	return &AuthSpec{
		Mode:   in.Mode,
		Key:    in.Key,
		KeyEnv: in.KeyEnv,
	}
}

// FromProfile builds a ProfileInput from a selected profile. nil-safe:
// a nil profile (inherit-env) returns nil. When p.Auth is nil — the
// profile expresses "ccwrap does not own auth" — in.Auth is also nil.
// profileAuthEnvKey already maps an empty mode to "" so
// applyProfileOverlay skips the auth-injection branch cleanly when the
// pointer is nil.
func FromProfile(p *profiles.Profile) *ProfileInput {
	if p == nil {
		return nil
	}
	return &ProfileInput{
		Name:            p.Name,
		Provider:        p.Provider,
		BaseURL:         p.BaseURL,
		Auth:            profilesAuthToPreflight(p.Auth),
		ModelAliases:    p.ModelAliases,
		UpstreamHeaders: p.UpstreamHeaders,
		EgressMode:      p.Egress.Mode,
		EgressURL:       p.Egress.URL,
	}
}

// ambientAuthEnvKeys is the exact set resolveAuthWithSources consults for
// upstream auth (CCWRAP-owned first, then Claude's first-party creds). When
// a profile declares auth ownership, applyProfileOverlay strips all of
// these so none can preempt the profile or leak a first-party credential to
// a third-party upstream. NOTE: this is the env-var vector. A trusted
// Claude settings file carrying an auth key in its `env` block is a
// separate, rarer vector merged downstream in EffectiveProviderEnvFromInspection
// and is not closed here (documented follow-up).
var ambientAuthEnvKeys = []string{
	"CCWRAP_UPSTREAM_API_KEY",
	"CCWRAP_UPSTREAM_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// presentAmbientAuthKeys returns the ambient auth env keys actually set in
// env (used only to compose the "ignored your env auth" note).
func presentAmbientAuthKeys(env []string) []string {
	set := toMap(env)
	var present []string
	for _, k := range ambientAuthEnvKeys {
		if strings.TrimSpace(set[k]) != "" {
			present = append(present, k)
		}
	}
	return present
}

// stripAmbientAuthEnv returns env without any KEY=VALUE entry whose KEY is
// an ambient auth source. Used when a profile owns auth so its credential
// is the sole upstream auth the resolver sees.
func stripAmbientAuthEnv(env []string) []string {
	drop := make(map[string]struct{}, len(ambientAuthEnvKeys))
	for _, k := range ambientAuthEnvKeys {
		drop[k] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 {
			if _, skip := drop[kv[:eq]]; skip {
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}

// profileAuthEnvKey maps a profile auth mode to the env key the
// existing resolveAuthWithSources path already understands, so a
// profile's credential flows through the SAME hidden-auth/fail-closed
// validation as an env-provided one (no new auth code path).
func profileAuthEnvKey(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "ccwrap_bearer":
		return "ANTHROPIC_AUTH_TOKEN"
	case "ccwrap_x_api_key":
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}

// applyProfileOverlay returns possibly-overridden copies of the
// resolution inputs. An explicit Options field always wins (it is
// non-empty only when the user passed the matching flag); the profile
// fills a field only when the explicit field is empty; env/fallback
// remain the lowest tier (handled downstream unchanged).
//
// missingAuthEnv reports the Case-A "key_env named but env missing"
// situation: the active profile asked ccwrap to bootstrap auth from a named
// env var, but the var is unset. This used to be a hard error refusing to
// launch; now we signal it through the missingAuthEnv return so
// ResolveProfile can mark the Result with AuthBootstrap=Missing +
// MissingAuthEnv=<name>, and the supervisor fail-closes at request time
// instead. Empty string means "no Case-A detected" (either no profile, no
// key_env, or key_env value present).
func applyProfileOverlay(opts Options) (upstream string, parentEnv []string, modelAliasFile string, modelAliasPairs []string, upstreamHeaderFile string, upstreamHeaderPairs []string, egressProxy string, missingAuthEnv string, profileAuthKey string, err error) {
	upstream = opts.Upstream
	parentEnv = opts.ParentEnv
	modelAliasFile = opts.ModelAliasFile
	modelAliasPairs = opts.ModelAliasPairs
	upstreamHeaderFile = opts.UpstreamHeaderFile
	upstreamHeaderPairs = opts.UpstreamHeaderPairs
	egressProxy = opts.EgressProxy

	p := opts.Profile
	if p == nil {
		return
	}

	if strings.TrimSpace(upstream) == "" && strings.TrimSpace(p.BaseURL) != "" {
		upstream = p.BaseURL
	}

	// Auth: a profile that DECLARES auth (p.Auth != nil) OWNS the upstream
	// credential — it is authoritative, not a fallback. Ambient env auth
	// (ANTHROPIC_*/CLAUDE_* first-party creds AND CCWRAP_UPSTREAM_* upstream
	// creds) must NOT preempt it: the two explicit ways to use env auth are
	// inherit-env (a nil profile) and auth:null (this branch is skipped).
	//
	// This used to gate injection behind `!explicitAuth` — i.e. the profile
	// key was a FALLBACK that any ambient auth env var silently overrode.
	// That contradicted this file's own documented "profile > env"
	// precedence and was confirmed live to (a) make a correctly-configured
	// switched gateway profile fail auth, and worse (b) ship a first-party
	// ANTHROPIC_AUTH_TOKEN to a third-party gateway. Now: resolve the
	// profile's secret, strip every ambient auth source, then inject the
	// profile's credential as the sole upstream auth the resolver sees.
	//
	// key_env (Case A): when the profile names a key_env that the env lacks,
	// we strip ambient auth anyway and inject nothing → downstream
	// hiddenAuthContract marks AuthBootstrap=Missing and the supervisor
	// fail-closes at request time. The ambient token is NOT used as a
	// fallback (that was the leak).
	if p.Auth != nil {
		envMap := toMap(parentEnv)
		// Resolve the profile secret BEFORE stripping, so a key_env that
		// happens to name one of the ambient keys is still read.
		secret := strings.TrimSpace(p.Auth.Key)
		if secret == "" && strings.TrimSpace(p.Auth.KeyEnv) != "" {
			secret = strings.TrimSpace(envMap[strings.TrimSpace(p.Auth.KeyEnv)])
			if secret == "" {
				missingAuthEnv = strings.TrimSpace(p.Auth.KeyEnv)
			}
		}
		parentEnv = stripAmbientAuthEnv(parentEnv)
		if key := profileAuthEnvKey(p.Auth.Mode); key != "" && secret != "" {
			parentEnv = append(parentEnv, key+"="+secret)
			profileAuthKey = key
		}
	}

	// Model aliases: profile contributes a pairs slice only when no
	// explicit alias flag/file was given (explicit wins).
	if strings.TrimSpace(modelAliasFile) == "" && len(modelAliasPairs) == 0 && len(p.ModelAliases) > 0 {
		keys := make([]string, 0, len(p.ModelAliases))
		for k := range p.ModelAliases {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, k+"="+p.ModelAliases[k])
		}
		modelAliasPairs = pairs
	}

	// Upstream headers: same precedence shape.
	if strings.TrimSpace(upstreamHeaderFile) == "" && len(upstreamHeaderPairs) == 0 && len(p.UpstreamHeaders) > 0 {
		keys := make([]string, 0, len(p.UpstreamHeaders))
		for k := range p.UpstreamHeaders {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		hpairs := make([]string, 0, len(keys))
		for _, k := range keys {
			hpairs = append(hpairs, k+"="+p.UpstreamHeaders[k])
		}
		upstreamHeaderPairs = hpairs
	}

	// Egress: "inherit"/"" leaves the existing auto/env path. "direct"
	// and "http|socks5|socks5h <url>" become the explicit egress flag
	// value the existing ResolveEgressFromInspection already accepts,
	// unless an explicit --egress-proxy (non-"auto") was given.
	if strings.TrimSpace(egressProxy) == "" || strings.EqualFold(strings.TrimSpace(egressProxy), "auto") {
		switch strings.TrimSpace(strings.ToLower(p.EgressMode)) {
		case "", "inherit":
			// leave as-is (auto/env)
		case "direct":
			egressProxy = "direct"
		case "http", "socks5", "socks5h":
			if u := strings.TrimSpace(p.EgressURL); u != "" {
				egressProxy = u
			}
		}
	}
	return
}

// ResolveProfile is the single resolver shared by the launch path and
// the live-switch path. opts.Profile is the selected-profile
// overlay (nil => inherit-env, byte-identical to ccwrap's pre-feature
// env-only resolution). This is the verbatim pre-feature resolution +
// hidden-auth/fail-closed validation body, with the profile overlay
// applied to its inputs first. A rejected profile returns an error
// and NO Result — nothing partially applies.
func ResolveProfile(opts Options, inspect *settings.InspectionResult) (*Result, error) {
	upstream, parentEnv, modelAliasFile, modelAliasPairs, upstreamHeaderFile, upstreamHeaderPairs, egressProxy, missingAuthEnv, profileAuthKey, overlayErr := applyProfileOverlay(opts)
	if overlayErr != nil {
		return nil, overlayErr
	}

	env := toMap(parentEnv)
	unsupported := detectUnsupported(env)
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, fmt.Errorf("unsupported auth/tunnel env detected: %s — set proxying via --egress-proxy (or a profile's egress) and upstream auth via a profile / CCWRAP_UPSTREAM_*, not these env vars", strings.Join(unsupported, ", "))
	}
	if inspect == nil {
		var err error
		inspect, err = settings.InspectLaunch(opts.WorkingDirectory, opts.ChildArgs)
		if err != nil {
			return nil, err
		}
	}
	if len(inspect.UnsupportedEnv) > 0 {
		return nil, fmt.Errorf("unsupported auth/tunnel env detected in Claude settings: %s — remove it from your Claude settings; configure proxying via --egress-proxy and upstream auth via a profile / CCWRAP_UPSTREAM_*", formatSettingConflicts(inspect.UnsupportedEnv))
	}
	if len(inspect.MalformedEnv) > 0 {
		return nil, fmt.Errorf("malformed env object detected in Claude settings: %s", formatMalformedEnvIssues(inspect.MalformedEnv))
	}
	if len(inspect.PolicyNetworkEnv) > 0 {
		return nil, policyNetworkEnvError(inspect.PolicyNetworkEnv)
	}

	provider, err := settings.EffectiveProviderEnvFromInspection(parentEnv, inspect)
	if err != nil {
		return nil, fmt.Errorf("resolve provider/auth env: %w", err)
	}
	if provider == nil {
		provider = &settings.EffectiveProviderEnvResult{Env: env}
	}
	// A profile that OWNS auth is authoritative for the upstream credential:
	// trusted Claude settings (the globalConfig/userSettings/flagSettings/
	// policySettings env blocks) must not contribute or clobber it.
	// EffectiveProviderEnvFromInspection merges trusted-settings provider auth
	// on top of parentEnv, which would otherwise overwrite the profile-injected
	// key (same key) and ship a first-party settings token to a third-party
	// gateway — the settings-file twin of the 4297f1d ambient-env leak. Drop
	// the settings-sourced credential keys and reassert the profile's injected
	// one (still held in env from the applyProfileOverlay injection above).
	// These are exactly the credential keys resolveAuthWithSources reads from
	// provider.Env — NOT the base-URL key in providerRoutingAuthKeys.
	if profileAuthKey != "" {
		for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
			delete(provider.Env, k)
			delete(provider.KeySources, k)
		}
		if v := strings.TrimSpace(env[profileAuthKey]); v != "" {
			if provider.Env == nil {
				provider.Env = map[string]string{}
			}
			if provider.KeySources == nil {
				provider.KeySources = map[string]string{}
			}
			provider.Env[profileAuthKey] = v
			provider.KeySources[profileAuthKey] = "inherited_env"
		}
	}
	modelEnv, err := settings.EffectiveModelEnvFromInspection(parentEnv, inspect)
	if err != nil {
		return nil, fmt.Errorf("resolve model env: %w", err)
	}
	if modelEnv == nil {
		modelEnv = &settings.EffectiveModelEnvResult{}
	}
	apiBaseURL, routeSource, routeConfigSource, err := resolveAPIBaseWithSources(upstream, env, provider.Env, provider.KeySources)
	if err != nil {
		return nil, err
	}
	baseRouteClass := ClassifyRoute(apiBaseURL)
	thirdPartyRoute := baseRouteClass == model.RouteClassThirdPartyHidden
	strictModelAlias := thirdPartyRoute && !opts.AllowProviderModelPassthrough
	if strictModelAlias {
		if visible := claudeVisibleModelOverrides(inspect); len(visible) > 0 {
			return nil, fmt.Errorf("Claude-visible model alias/modelOverrides conflict with third-party hidden mode; move mappings to CCWRAP-only flag/env modelAliases: %s", formatSettingConflicts(visible))
		}
	}
	aliasCfg, _, err := modelalias.Resolve(modelalias.ResolveOptions{
		ExplicitFile:             modelAliasFile,
		ExplicitFileContent:      opts.ModelAliasExplicitFileContent,
		ExplicitPairs:            modelAliasPairs,
		ParentEnv:                parentEnv,
		EnvFileContent:           opts.ModelAliasEnvFileContent,
		FlagSettings:             nilSafeFlagSettings(inspect),
		Strict:                   strictModelAlias,
		ProviderModelPassthrough: opts.AllowProviderModelPassthrough,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve model aliases: %w", err)
	}
	// First-party (canonical Anthropic) permits Claude→Claude remaps —
	// e.g. claude-opus-4-8 → claude-fable-5: the rewritten target is a model
	// api.anthropic.com can serve, and the response normalizer restores the
	// logical id so Claude Code still sees what it asked for. But a
	// provider-routed target (Bedrock/Vertex/ARN/path/tagged id) can never
	// resolve at the official endpoint, so reject it fail-closed here rather
	// than let it silently 404 at the upstream. (Third-party routes keep the
	// fuller strict gate above; this is the first-party-only narrowing.)
	if aliasCfg.Enabled() && baseRouteClass == model.RouteClassFirstParty {
		var providerSpecific []string
		for logical, provider := range aliasCfg.Forward {
			if modelalias.LooksProviderSpecific(provider) {
				providerSpecific = append(providerSpecific, logical+"="+provider)
			}
		}
		if len(providerSpecific) > 0 {
			sort.Strings(providerSpecific)
			return nil, fmt.Errorf("first-party route (%s) cannot alias to provider-specific model IDs: %s; use a Claude model as the alias target, or point at a third-party upstream", apiBaseURL.String(), strings.Join(providerSpecific, ", "))
		}
	}
	upstreamHeaderCfg, _, err := upstreamheaders.Resolve(upstreamheaders.ResolveOptions{
		ExplicitFile:        upstreamHeaderFile,
		ExplicitFileContent: opts.UpstreamHeaderExplicitFileContent,
		ExplicitPairs:       upstreamHeaderPairs,
		ParentEnv:           parentEnv,
		EnvFileContent:      opts.UpstreamHeaderEnvFileContent,
		FlagSettings:        nilSafeFlagSettings(inspect),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve upstream headers: %w", err)
	}
	rewrittenArgs := rewrittenChildArgs(inspect.ParsedFlagSettings, opts.ChildArgs)
	var normalizedArgModels []modelalias.Normalization
	var normalizedEnvModels []modelalias.Normalization
	if strictModelAlias {
		var unresolvedArgs []string
		rewrittenArgs, normalizedArgModels, unresolvedArgs = modelalias.NormalizeClaudeModelArgs(rewrittenArgs, aliasCfg)
		if len(unresolvedArgs) > 0 {
			return nil, fmt.Errorf("provider-specific model passed via --model in third-party hidden mode without a matching CCWRAP reverse alias: %s; configure --model-alias LOGICAL=PROVIDER or pass --allow-provider-model-passthrough", strings.Join(unresolvedArgs, ", "))
		}
		normalizedModelEnv, envNormalizations, unresolvedEnv := modelalias.NormalizeModelEnv(modelEnv.Env, aliasCfg, envpolicy.IsModelPreferenceKey)
		if len(unresolvedEnv) > 0 {
			return nil, fmt.Errorf("provider-specific model env conflicts with third-party hidden mode without a matching CCWRAP reverse alias: %s; configure CCWRAP modelAliases or pass --allow-provider-model-passthrough", formatStringMapKeys(unresolvedEnv))
		}
		modelEnv.Env = normalizedModelEnv
		normalizedEnvModels = envNormalizations
	}
	mode, source, authConfigSource, override, err := resolveAuthWithSources(env, provider.Env, provider.KeySources)
	if err != nil {
		return nil, err
	}
	parentCustomAuthHeaderEnv := inheritedCustomAuthHeaderConflicts(env)
	customAuthHeaderEnv := appendEnvConflicts(inspect.CustomAuthHeaderEnv, parentCustomAuthHeaderEnv)
	authPolicy, authBootstrap, authBootstrapKind, authBootstrapEnvKey := hiddenAuthContract(thirdPartyRoute, opts.AllowAuthPassthroughToThirdParty, mode)
	routeClass := baseRouteClass
	if thirdPartyRoute && (opts.AllowProviderModelPassthrough || (opts.AllowAuthPassthroughToThirdParty && mode == model.AuthModePassthrough)) {
		routeClass = model.RouteClassThirdPartyCompatible
	}
	if thirdPartyRoute && !opts.AllowAuthPassthroughToThirdParty {
		if mode == model.AuthModePassthrough {
			// Case B: third-party-hidden + no auth source found. Used to
			// hard-error here; now we DON'T return — the downstream
			// hiddenAuthContract already marks AuthBootstrap=Missing
			// (preflight.go) in this same condition, and the supervisor
			// fail-closes at request time. Result carries
			// MissingAuthEnv = "" (no specific env to suggest) since the
			// profile didn't name one. Empty missingAuthEnv distinguishes
			// Case B from Case A in UI messaging.
			//
			// The other three refusals below are KEPT as hard errors —
			// they are configuration conflicts (file content) that the user
			// must fix before any launch is meaningful; they are not "env
			// rotated out from under us" situations and inspect tools alone
			// don't help fix them.
			_ = missingAuthEnv // already captured from overlay; will be assigned to Result below
		}
		if len(inspect.APIKeyHelperHits) > 0 {
			return nil, fmt.Errorf("apiKeyHelper is a Claude-side auth path and is blocked in third-party hidden mode: %s", strings.Join(inspect.APIKeyHelperHits, ", "))
		}
		if len(inspect.DangerousShellSettings) > 0 {
			return nil, fmt.Errorf("shell-exec settings are blocked in third-party hidden mode (they run at request time and could leak credentials or inject auth-like headers): %s", formatSettingConflicts(inspect.DangerousShellSettings))
		}
		if len(customAuthHeaderEnv) > 0 {
			return nil, fmt.Errorf("auth-like ANTHROPIC_CUSTOM_HEADERS are blocked in third-party hidden mode; move gateway headers to CCWRAP-owned upstream header config: %s", formatSettingConflicts(customAuthHeaderEnv))
		}
	}
	egressCfg, notes, err := ResolveEgressFromInspection(egressProxy, parentEnv, inspect)
	if err != nil {
		return nil, fmt.Errorf("resolve egress proxy: %w", err)
	}
	if len(provider.ContributingSources) > 0 {
		notes = append(notes, "Claude settings provider/auth sources: "+strings.Join(provider.ContributingSources, ", "))
	}
	if len(provider.IgnoredProjectScopedProviderEnv) > 0 {
		notes = append(notes, "Ignored project/local Claude settings provider/auth env before trust: "+formatSettingConflicts(provider.IgnoredProjectScopedProviderEnv))
	}
	if len(modelEnv.ContributingSources) > 0 {
		notes = append(notes, "Claude settings model env sources preserved by CCWRAP: "+strings.Join(modelEnv.ContributingSources, ", "))
	}
	if len(modelEnv.IgnoredProjectScopedModelEnv) > 0 {
		notes = append(notes, "Ignored project/local Claude settings model env before trust: "+formatSettingConflicts(modelEnv.IgnoredProjectScopedModelEnv))
	}
	if aliasCfg.Enabled() {
		notes = append(notes, fmt.Sprintf("CCWRAP model aliases enabled: count=%d source=%s fingerprint=%s", aliasCfg.Count(), aliasCfg.Source, aliasCfg.Fingerprint))
	}
	if len(upstreamHeaderCfg.Headers) > 0 {
		notes = append(notes, fmt.Sprintf("CCWRAP upstream headers enabled: count=%d source=%s fingerprint=%s", len(upstreamHeaderCfg.Headers), upstreamHeaderCfg.Source, upstreamHeaderCfg.Fingerprint))
	}
	if len(normalizedArgModels) > 0 || len(normalizedEnvModels) > 0 {
		notes = append(notes, fmt.Sprintf("Normalized provider-specific Claude-facing model IDs through reverse CCWRAP aliases: args=%d env=%d", len(normalizedArgModels), len(normalizedEnvModels)))
	}
	if aliasCfg.ProviderModelPassthrough && routeClass == model.RouteClassThirdPartyCompatible {
		notes = append(notes, "Provider model passthrough is enabled; route_class=third_party_compatible and Claude may see provider-specific model IDs.")
	}
	if thirdPartyRoute && opts.AllowAuthPassthroughToThirdParty && mode == model.AuthModePassthrough {
		notes = append(notes, "Unsafe auth passthrough is enabled; route_class=third_party_compatible and Claude-side auth may reach third-party upstream.")
	}
	if authBootstrap == model.AuthBootstrapPlaceholderActive {
		notes = append(notes, fmt.Sprintf("Third-party hidden auth bootstrap active: kind=%s", authBootstrapKind))
	}
	// Tell the user when their ambient auth env was deliberately ignored
	// because the active profile owns auth — otherwise "I exported a key but
	// it's not being used" reads as a bug. (inherit-env / auth:null are the
	// ways to make env auth win.)
	if opts.Profile != nil && opts.Profile.Auth != nil {
		if ignored := presentAmbientAuthKeys(opts.ParentEnv); len(ignored) > 0 {
			notes = append(notes, fmt.Sprintf("Ignored ambient auth env (%s) because profile %q owns auth; use inherit-env or a profile with auth:null to let the environment supply credentials.", strings.Join(ignored, ", "), opts.Profile.Name))
		}
	}
	return &Result{
		APIBaseURL:             apiBaseURL,
		RouteSource:            routeSource,
		RouteConfigSource:      routeConfigSource,
		RouteClass:             routeClass,
		AuthMode:               mode,
		AuthSource:             source,
		AuthConfigSource:       authConfigSource,
		AuthPolicy:             authPolicy,
		AuthBootstrap:          authBootstrap,
		AuthBootstrapKind:      authBootstrapKind,
		AuthBootstrapEnvKey:    authBootstrapEnvKey,
		OverrideAuth:           override,
		ActiveSources:          inspect.ActiveSources,
		APIKeyHelperHits:       inspect.APIKeyHelperHits,
		DangerousShellSettings: inspect.DangerousShellSettings,
		UnsupportedEnv:         unsupported,
		UnsupportedSettings:    inspect.UnsupportedEnv,
		MalformedSettingsEnv:   inspect.MalformedEnv,
		OverriddenNetworkEnv:   inspect.OverriddenNetworkEnv,
		PolicyNetworkEnv:       inspect.PolicyNetworkEnv,
		CustomAuthHeaderEnv:    customAuthHeaderEnv,
		ModelOverrideHits:      inspect.ModelOverrideHits,
		ParsedFlagSettings:     inspect.ParsedFlagSettings,
		RewrittenChildArgs:     rewrittenArgs,
		Egress:                 egressCfg,
		ModelAlias:             aliasCfg,
		UpstreamHeaders:        upstreamHeaderCfg,
		ModelEnv:               modelEnv.Env,
		ModelConfigSources:     modelEnv.ContributingSources,
		Notes:                  notes,
		ActiveProfileName:      profileName(opts.Profile),
		ActiveProfileProvider:  profileProvider(opts.Profile),
		// Empty (no auth missing) is the common case. Non-empty
		// for Case A (profile named a key_env that env lacks); zero-value
		// (still empty) for Case B (no key_env named, no source found) —
		// AuthBootstrap == Missing carries the signal in both cases.
		MissingAuthEnv: missingAuthEnv,
	}, nil
}

// ClassifyTransition classifies a posture transition (current →
// candidate). needs_relaunch IFF Claude was started
// with only a placeholder (hidden/CCWRAP-owned bootstrap) and the
// candidate needs Claude's own first-party credentials forwarded
// (first-party passthrough). Every other transition — gateway↔gateway,
// key swap, model swap, egress swap, first-party→gateway — is a pure
// ccwrap-side live hot-rebind. nil current (first launch) is always live.
func ClassifyTransition(current, candidate *Result) model.RelaunchClass {
	if current == nil || candidate == nil {
		return model.RelaunchLive
	}
	if current.AuthBootstrap == model.AuthBootstrapPlaceholderActive &&
		candidate.AuthPolicy == model.AuthPolicyFirstPartyPassthrough {
		return model.RelaunchNeedsRelaunch
	}
	return model.RelaunchLive
}

// ProfileView is the non-secret snapshot of a resolved posture. It
// carries everything the inspect switcher / posture timeline render
// (provider/group label, base_url host, model-alias count, egress
// summary, active-profile identity, relaunch-required classification,
// auth policy) and NEVER a credential value.
//
// JSON tags use snake_case for stable wire format across the supervisor's
// SwitchOutcome serialization and the control package's SwitchOutcomeView
// mirror. The control client decodes View as json.RawMessage; CLI/UI
// re-decode into a typed shape when needed.
type ProfileView struct {
	Name            string              `json:"name,omitempty"`
	ProviderLabel   string              `json:"provider_label,omitempty"`
	BaseURLHost     string              `json:"base_url_host,omitempty"`
	ModelAliasCount int                 `json:"model_alias_count,omitempty"`
	EgressSummary   string              `json:"egress_summary,omitempty"`
	RelaunchClass   model.RelaunchClass `json:"relaunch_class,omitempty"`
	AuthPolicy      model.AuthPolicy    `json:"auth_policy,omitempty"`
}

// ProfileView builds the non-secret snapshot from a resolved Result.
// RelaunchClass is computed vs a nil "current" (first launch / no
// prior posture); callers recompute it against the live posture via
// ClassifyTransition. profileName is taken from the active profile
// overlay when present (carried on the Result is not needed — the
// caller passes the active *ProfileInput's identity through the
// Result fields it already has). Here Name/ProviderLabel derive from
// the resolved upstream when no profile drove it.
func (r *Result) ProfileView() ProfileView {
	if r == nil {
		return ProfileView{}
	}
	host := ""
	if r.APIBaseURL != nil {
		host = r.APIBaseURL.Host
	}
	v := ProfileView{
		BaseURLHost:     host,
		ModelAliasCount: r.ModelAlias.Count(),
		EgressSummary:   r.Egress.Summary,
		RelaunchClass:   ClassifyTransition(nil, r),
		AuthPolicy:      r.AuthPolicy,
	}
	if strings.TrimSpace(v.EgressSummary) == "" {
		if r.Egress.Mode == "direct" || (r.Egress.HTTPProxy == "" && r.Egress.HTTPSProxy == "") {
			v.EgressSummary = "direct"
		}
	}
	if r.ActiveProfileName != "" {
		v.Name = r.ActiveProfileName
		if r.ActiveProfileProvider != "" {
			v.ProviderLabel = r.ActiveProfileName + " (" + r.ActiveProfileProvider + ")"
		} else {
			v.ProviderLabel = r.ActiveProfileName
		}
	}
	return v
}

func profileName(p *ProfileInput) string {
	if p == nil {
		return ""
	}
	return p.Name
}

func profileProvider(p *ProfileInput) string {
	if p == nil {
		return ""
	}
	return p.Provider
}
