package preflight

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/envpolicy"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

type Options struct {
	Upstream                         string
	EgressProxy                      string
	ParentEnv                        []string
	WorkingDirectory                 string
	ChildArgs                        []string
	ModelAliasFile                   string
	ModelAliasPairs                  []string
	AllowProviderModelPassthrough    bool
	AllowAuthPassthroughToThirdParty bool
	UpstreamHeaderFile               string
	UpstreamHeaderPairs              []string
	// Profile is the per-field overlay contributed by a selected
	// provider profile. nil => inherit-env: behavior is exactly
	// the env-only path (byte-identical Result). The launch path
	// (cmd/ccwrap) sets this; the live-swap path reuses ResolveProfile.
	Profile *ProfileInput
	// File-content snapshots for byte-faithful switch resolution. Populated by
	// the launcher at launch (cmd/ccwrap/main.go composeLaunch); the doctor
	// command leaves them nil ⇒ disk-read fallback. When non-empty, the
	// resolver MUST use the content and MUST NOT read disk for that tier input.
	ModelAliasExplicitFileContent     []byte // overrides ModelAliasFile reads
	ModelAliasEnvFileContent          []byte // overrides CCWRAP_MODEL_ALIASES_FILE-path reads
	UpstreamHeaderExplicitFileContent []byte // overrides UpstreamHeaderFile reads
	UpstreamHeaderEnvFileContent      []byte // overrides CCWRAP_UPSTREAM_HEADERS_FILE-path reads
}

type Result struct {
	APIBaseURL             *url.URL
	RouteSource            model.RouteSource
	RouteConfigSource      string
	RouteClass             model.RouteClass
	AuthMode               model.AuthMode
	AuthSource             model.AuthSource
	AuthConfigSource       string
	AuthPolicy             model.AuthPolicy
	AuthBootstrap          model.AuthBootstrap
	AuthBootstrapKind      model.AuthBootstrapKind
	AuthBootstrapEnvKey    string
	OverrideAuth           *model.AuthOverride
	ActiveSources          []string
	APIKeyHelperHits       []string
	DangerousShellSettings []settings.EnvConflict
	UnsupportedEnv         []string
	UnsupportedSettings    []settings.EnvConflict
	MalformedSettingsEnv   []settings.MalformedEnvIssue
	OverriddenNetworkEnv   []settings.EnvConflict
	PolicyNetworkEnv       []settings.EnvConflict
	CustomAuthHeaderEnv    []settings.EnvConflict
	ModelOverrideHits      []settings.EnvConflict
	ParsedFlagSettings     *settings.ParsedFlagSettings
	RewrittenChildArgs     []string
	Egress                 model.EgressConfig
	ModelEnv               map[string]string
	ModelConfigSources     []string
	ModelAlias             modelalias.Config
	UpstreamHeaders        upstreamheaders.Config
	Notes                  []string
	// Active provider-profile identity (the switcher and posture
	// timeline read these; empty => inherit-env).
	// Non-secret: name + group label only, never a credential.
	ActiveProfileName     string
	ActiveProfileProvider string
	// MissingAuthEnv names the env var that an active profile's `auth.key_env`
	// pointed at when no value was found in the process env (the profile-overlay
	// path's "key_env named but env missing" branch). It is empty in the
	// broader "no auth source at all" case (third-party-hidden + resolved
	// mode==passthrough), because there is no specific env to name. UI/error
	// surfaces branch on emptiness:
	//   non-empty → "profile X needs $Y"   (concrete recovery target)
	//   empty     → "profile X has no auth source configured"
	// Always paired with AuthBootstrap == AuthBootstrapMissing — the boolean
	// signal; MissingAuthEnv is the detail.
	//
	// Fail-closed is enforced on the forward path (request-time) rather than in
	// preflight (launch-time). When AuthBootstrap==Missing the launch SUCCEEDS
	// so inspect/popover/switch tools are reachable; only an actual request to a
	// host requiring ccwrap-owned auth gets refused.
	MissingAuthEnv string
}

// ChildAuthBootstrap describes the non-secret auth value injected into the
// Claude child process so Claude can reach CCWRAP while CCWRAP owns the real upstream
// secret. The value must never be forwarded upstream.
type ChildAuthBootstrap struct {
	EnvKey string
	Value  string
}

func (b ChildAuthBootstrap) Enabled() bool {
	return strings.TrimSpace(b.EnvKey) != "" && strings.TrimSpace(b.Value) != ""
}

var scrubKeys = envpolicy.SpawnScrubKeys()

var unsupportedKeys = envpolicy.UnsupportedTransportAuthKeys()

func ScrubKeys() []string {
	out := make([]string, len(scrubKeys))
	copy(out, scrubKeys)
	return out
}

func Run(opts Options) (*Result, error) {
	inspect, err := settings.InspectLaunch(opts.WorkingDirectory, opts.ChildArgs)
	if err != nil {
		return nil, err
	}
	return RunWithInspection(opts, inspect)
}

func RunWithInspection(opts Options, inspect *settings.InspectionResult) (*Result, error) {
	return ResolveProfile(opts, inspect)
}

// ClassifyRoute reports whether a parsed upstream URL belongs to a
// first-party Anthropic host or a third-party gateway. Exported so
// the doctor command (cmd/ccwrap) can share the same predicate.
func ClassifyRoute(apiBaseURL *url.URL) model.RouteClass {
	if apiBaseURL == nil {
		return model.RouteClassFirstParty
	}
	host := strings.ToLower(strings.TrimRight(strings.TrimSpace(apiBaseURL.Hostname()), "."))
	if host == "api.anthropic.com" || host == "api-staging.anthropic.com" {
		return model.RouteClassFirstParty
	}
	return model.RouteClassThirdPartyHidden
}

func hiddenAuthContract(thirdPartyRoute, allowAuthPassthrough bool, mode model.AuthMode) (model.AuthPolicy, model.AuthBootstrap, model.AuthBootstrapKind, string) {
	if !thirdPartyRoute {
		if mode == model.AuthModePassthrough {
			return model.AuthPolicyFirstPartyPassthrough, model.AuthBootstrapNotNeeded, model.AuthBootstrapKindNone, ""
		}
		return model.AuthPolicyCCWRAPOverride, model.AuthBootstrapNotNeeded, model.AuthBootstrapKindNone, ""
	}
	if mode == model.AuthModePassthrough {
		if allowAuthPassthrough {
			return model.AuthPolicyUnsafePassthrough, model.AuthBootstrapNotNeeded, model.AuthBootstrapKindNone, ""
		}
		return model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapMissing, model.AuthBootstrapKindNone, ""
	}
	kind, envKey := placeholderKindForAuthMode(mode)
	return model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, kind, envKey
}

func placeholderKindForAuthMode(mode model.AuthMode) (model.AuthBootstrapKind, string) {
	switch mode {
	case model.AuthModeOverrideXAPIKey:
		return model.AuthBootstrapKindXAPIKey, "ANTHROPIC_API_KEY"
	case model.AuthModeOverrideBearer:
		return model.AuthBootstrapKindBearer, "ANTHROPIC_AUTH_TOKEN"
	default:
		return model.AuthBootstrapKindNone, ""
	}
}

func inheritedCustomAuthHeaderConflicts(env map[string]string) []settings.EnvConflict {
	if env == nil {
		return nil
	}
	value := env["ANTHROPIC_CUSTOM_HEADERS"]
	if len(settings.AuthLikeCustomHeaderNames(value)) == 0 {
		return nil
	}
	return []settings.EnvConflict{{Source: "inherited_env", Path: "process env", Keys: []string{"ANTHROPIC_CUSTOM_HEADERS"}}}
}

func appendEnvConflicts(a, b []settings.EnvConflict) []settings.EnvConflict {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make([]settings.EnvConflict, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

func nilSafeFlagSettings(inspect *settings.InspectionResult) map[string]any {
	if inspect == nil || inspect.ParsedFlagSettings == nil {
		return nil
	}
	return inspect.ParsedFlagSettings.Settings
}

func claudeVisibleModelOverrides(inspect *settings.InspectionResult) []settings.EnvConflict {
	if inspect == nil {
		return nil
	}
	var out []settings.EnvConflict
	for _, hit := range inspect.ModelOverrideHits {
		if hit.Source == "flagSettings" {
			continue
		}
		out = append(out, hit)
	}
	return out
}

func validateHiddenModeModels(childArgs []string, modelEnv map[string]string) error {
	if hits := modelalias.ProviderSpecificModelArgs(childArgs); len(hits) > 0 {
		return fmt.Errorf("provider-specific model passed via --model in third-party hidden mode; use a Claude logical model and configure CCWRAP modelAliases")
	}
	for key, value := range modelEnv {
		if !envpolicy.IsModelPreferenceKey(key) || strings.TrimSpace(value) == "" {
			continue
		}
		if modelalias.LooksProviderSpecific(value) {
			return fmt.Errorf("provider-specific model in %s conflicts with third-party hidden mode; use a Claude logical model and configure CCWRAP modelAliases", key)
		}
	}
	return nil
}

func InjectedEnv(proxyURL, caCertPath, caBundlePath string) map[string]string {
	// SSL_CERT_DIR is deliberately omitted. Empty-string semantics on OpenSSL
	// builds are ambiguous (some treat "" as CWD). CCWRAP covers trust via
	// SSL_CERT_FILE plus per-runtime CA_BUNDLE vars; SSL_CERT_DIR is scrubbed
	// from the child environment via envpolicy and should remain unset.
	return map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"HTTP_PROXY":          proxyURL,
		"http_proxy":          proxyURL,
		"NO_PROXY":            "127.0.0.1,::1,localhost",
		"no_proxy":            "127.0.0.1,::1,localhost",
		"NODE_EXTRA_CA_CERTS": caCertPath,
		"SSL_CERT_FILE":       caBundlePath,
		"REQUESTS_CA_BUNDLE":  caBundlePath,
		"CURL_CA_BUNDLE":      caBundlePath,
		"GIT_SSL_CAINFO":      caBundlePath,
	}
}

// BuildChildEnv composes the Claude Code child environment. The trailing tz
// parameter, when non-empty, overrides any inherited TZ so the child stamps a
// chosen timezone into its request prompt (Claude Code computes "Today's date"
// from the LOCAL zone). An empty tz leaves the parent's TZ (if any) untouched —
// byte-identical to the pre-feature environment. Callers are expected to have
// already validated tz via time.LoadLocation; BuildChildEnv trusts it.
func BuildChildEnv(parentEnv []string, proxyURL, caCertPath, caBundlePath string, modelEnv map[string]string, authBootstrap ChildAuthBootstrap, tz string) []string {
	env := toMap(parentEnv)
	for key := range env {
		if envpolicy.IsSpawnScrubKey(key) {
			delete(env, key)
		}
	}
	for k, v := range modelEnv {
		env[k] = v
	}
	// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST declares "the host supplies the
	// credentials via env" — current Claude Code enforces it as a credential
	// lockdown, refusing every LOCAL credential source (stored claude.ai
	// OAuth, the /login keychain key, settings apiKeyHelper) and failing 401s
	// without offering re-login. Declare it only when the bootstrap actually
	// puts a CCWRAP-owned credential in the child env; in every other posture
	// (first-party passthrough, proxy-side override, fail-closed missing
	// auth) the child must keep reaching its own local credentials or it
	// launches with none at all. Env pollution is not lost by omitting the
	// flag: the spawn scrub above and the generated-settings strip already
	// close the routing/auth env paths on CCWRAP's side.
	if authBootstrap.Enabled() {
		env[authBootstrap.EnvKey] = authBootstrap.Value
		env["CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"] = "1"
	}
	for k, v := range InjectedEnv(proxyURL, caCertPath, caBundlePath) {
		env[k] = v
	}
	if tz != "" {
		env["TZ"] = tz
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func resolveAPIBase(explicit, inherited string) (*url.URL, model.RouteSource, error) {
	u, source, _, err := resolveAPIBaseWithSources(explicit, map[string]string{}, map[string]string{"ANTHROPIC_BASE_URL": inherited}, map[string]string{"ANTHROPIC_BASE_URL": "inherited_env"})
	return u, source, err
}

func resolveAPIBaseWithSources(explicit string, parentEnv map[string]string, env map[string]string, keySources map[string]string) (*url.URL, model.RouteSource, string, error) {
	if strings.TrimSpace(explicit) != "" {
		u, err := resolveURL(explicit)
		return u, model.RouteSourceExplicit, "explicit", err
	}
	if parentEnv == nil {
		parentEnv = map[string]string{}
	}
	if env == nil {
		env = map[string]string{}
	}
	if base := strings.TrimSpace(parentEnv["CCWRAP_UPSTREAM"]); base != "" {
		u, err := resolveURL(base)
		return u, model.RouteSourceInheritedEnv, "CCWRAP_UPSTREAM", err
	}
	base := strings.TrimSpace(env["ANTHROPIC_BASE_URL"])
	if base != "" {
		u, err := resolveURL(base)
		configSource := sourceForKey(keySources, "ANTHROPIC_BASE_URL", "inherited_env")
		return u, routeSourceForConfigSource(configSource), configSource, err
	}
	u, err := resolveURL("https://api.anthropic.com")
	return u, model.RouteSourceFallback, "fallback_default", err
}

func sourceForKey(sources map[string]string, key, fallback string) string {
	if sources != nil {
		if source := strings.TrimSpace(sources[key]); source != "" {
			return source
		}
	}
	return fallback
}

func routeSourceForConfigSource(source string) model.RouteSource {
	switch source {
	case "explicit":
		return model.RouteSourceExplicit
	case "inherited_env":
		return model.RouteSourceInheritedEnv
	case "flagSettings":
		return model.RouteSourceFlagSettings
	case "policySettings":
		return model.RouteSourcePolicySettings
	case "globalConfig", "userSettings":
		return model.RouteSourceClaudeSettings
	default:
		return model.RouteSourceFallback
	}
}

func resolveURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("url must be absolute with scheme and host")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u, nil
}

func resolveAuth(env map[string]string) (model.AuthMode, model.AuthSource, *model.AuthOverride, error) {
	mode, source, _, override, err := resolveAuthWithSources(map[string]string{}, env, nil)
	return mode, source, override, err
}

func resolveAuthWithSources(parentEnv map[string]string, env map[string]string, keySources map[string]string) (model.AuthMode, model.AuthSource, string, *model.AuthOverride, error) {
	if parentEnv == nil {
		parentEnv = map[string]string{}
	}
	if key := strings.TrimSpace(parentEnv["CCWRAP_UPSTREAM_API_KEY"]); key != "" {
		return model.AuthModeOverrideXAPIKey, model.AuthSourceCCWRAPUpstreamAPIKey, "CCWRAP_UPSTREAM_API_KEY", &model.AuthOverride{Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceCCWRAPUpstreamAPIKey, HeaderName: "X-API-Key", HeaderValue: key}, nil
	}
	if token := strings.TrimSpace(parentEnv["CCWRAP_UPSTREAM_AUTH_TOKEN"]); token != "" {
		return model.AuthModeOverrideBearer, model.AuthSourceCCWRAPUpstreamToken, "CCWRAP_UPSTREAM_AUTH_TOKEN", &model.AuthOverride{Mode: model.AuthModeOverrideBearer, Source: model.AuthSourceCCWRAPUpstreamToken, HeaderName: "Authorization", HeaderValue: "Bearer " + token}, nil
	}
	sources := []struct {
		Key               string
		Mode              model.AuthMode
		Source            model.AuthSource
		Header            string
		Prefix            string
		AdditionalHeaders map[string]string
	}{
		{Key: "ANTHROPIC_API_KEY", Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey, Header: "X-API-Key"},
		{Key: "ANTHROPIC_AUTH_TOKEN", Mode: model.AuthModeOverrideBearer, Source: model.AuthSourceAnthropicToken, Header: "Authorization", Prefix: "Bearer "},
		{Key: "CLAUDE_CODE_OAUTH_TOKEN", Mode: model.AuthModeOverrideBearer, Source: model.AuthSourceClaudeOAuthToken, Header: "Authorization", Prefix: "Bearer ", AdditionalHeaders: map[string]string{"anthropic-beta": "oauth-2025-04-20"}},
	}
	var found []struct {
		Key               string
		Mode              model.AuthMode
		Source            model.AuthSource
		Header            string
		Value             string
		AdditionalHeaders map[string]string
	}
	for _, src := range sources {
		if env == nil {
			continue
		}
		if val := strings.TrimSpace(env[src.Key]); val != "" {
			found = append(found, struct {
				Key               string
				Mode              model.AuthMode
				Source            model.AuthSource
				Header            string
				Value             string
				AdditionalHeaders map[string]string
			}{Key: src.Key, Mode: src.Mode, Source: src.Source, Header: src.Header, Value: src.Prefix + val, AdditionalHeaders: src.AdditionalHeaders})
		}
	}
	if len(found) > 1 {
		keys := make([]string, 0, len(found))
		for _, f := range found {
			keys = append(keys, string(f.Source))
		}
		sort.Strings(keys)
		return model.AuthModeUnsupported, model.AuthSourceNone, "conflict", nil, fmt.Errorf("conflicting auth sources: %s — keep exactly one (unset the others, or let an active profile own auth)", strings.Join(keys, ", "))
	}
	if len(found) == 0 {
		return model.AuthModePassthrough, model.AuthSourceNone, "none", nil, nil
	}
	f := found[0]
	configSource := sourceForKey(keySources, f.Key, "inherited_env")
	additional := map[string]string(nil)
	if len(f.AdditionalHeaders) > 0 {
		additional = make(map[string]string, len(f.AdditionalHeaders))
		for k, v := range f.AdditionalHeaders {
			additional[k] = v
		}
	}
	return f.Mode, f.Source, configSource, &model.AuthOverride{Mode: f.Mode, Source: f.Source, HeaderName: f.Header, HeaderValue: f.Value, AdditionalHeaders: additional}, nil
}

func detectUnsupported(env map[string]string) []string {
	var hits []string
	for _, key := range unsupportedKeys {
		if strings.TrimSpace(env[key]) != "" {
			hits = append(hits, key)
		}
	}
	return hits
}

func EffectiveEgressEnv(parentEnv []string, cwd string, childArgs []string) (map[string]string, []string, bool, error) {
	inspect, err := settings.InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, nil, false, err
	}
	return EffectiveEgressEnvFromInspection(parentEnv, inspect)
}

func EffectiveEgressEnvFromInspection(parentEnv []string, inspect *settings.InspectionResult) (map[string]string, []string, bool, error) {
	effective, err := settings.EffectiveProxyEnvFromInspection(parentEnv, inspect)
	if err != nil {
		return nil, nil, false, err
	}
	if effective == nil {
		return toMap(parentEnv), nil, false, nil
	}
	notes := make([]string, 0, 2)
	usedSettings := len(effective.ContributingSources) > 0
	if usedSettings {
		notes = append(notes, "Claude settings proxy sources: "+strings.Join(effective.ContributingSources, ", "))
	}
	if len(effective.IgnoredPolicyNetworkEnv) > 0 {
		notes = append(notes, "Ignored detectable local/cache policy-managed network/trust env for egress auto (unsupported with stock Claude Code; remote managed settings, MDM, and HKCU remain unsupported and cannot be fully verified pre-launch): "+formatSettingConflicts(effective.IgnoredPolicyNetworkEnv))
	}
	return effective.Env, notes, usedSettings, nil
}

func ResolveEgress(flagValue string, parentEnv []string, cwd string, childArgs []string) (model.EgressConfig, []string, error) {
	inspect, err := settings.InspectLaunch(cwd, childArgs)
	if err != nil {
		return model.EgressConfig{}, nil, err
	}
	return ResolveEgressFromInspection(flagValue, parentEnv, inspect)
}

func ResolveEgressFromInspection(flagValue string, parentEnv []string, inspect *settings.InspectionResult) (model.EgressConfig, []string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" || strings.EqualFold(flagValue, "auto") {
		env, notes, usedSettings, err := EffectiveEgressEnvFromInspection(parentEnv, inspect)
		if err != nil {
			return model.EgressConfig{}, nil, err
		}
		cfg, resolveNotes, err := egress.Resolve(flagValue, env)
		if err != nil {
			return model.EgressConfig{}, nil, err
		}
		if usedSettings && cfg.Mode != "direct" {
			cfg.Source = "claude_settings"
		}
		return cfg, append(resolveNotes, notes...), nil
	}
	return egress.Resolve(flagValue, toMap(parentEnv))
}

func ParentEnvMap(env []string) map[string]string {
	return toMap(env)
}

func ResolveAuth(env map[string]string) (model.AuthMode, model.AuthSource, *model.AuthOverride, error) {
	return resolveAuth(env)
}

func ResolveAuthFromInspection(parentEnv []string, inspect *settings.InspectionResult) (model.AuthMode, model.AuthSource, string, *model.AuthOverride, error) {
	provider, err := settings.EffectiveProviderEnvFromInspection(parentEnv, inspect)
	if err != nil {
		return model.AuthModeUnsupported, model.AuthSourceNone, "", nil, err
	}
	if provider == nil {
		provider = &settings.EffectiveProviderEnvResult{Env: toMap(parentEnv)}
	}
	return resolveAuthWithSources(toMap(parentEnv), provider.Env, provider.KeySources)
}

func ResolveAPIBase(explicit, inherited string) (*url.URL, model.RouteSource, error) {
	return resolveAPIBase(explicit, inherited)
}

func ResolveAPIBaseFromInspection(explicit string, parentEnv []string, inspect *settings.InspectionResult) (*url.URL, model.RouteSource, string, error) {
	provider, err := settings.EffectiveProviderEnvFromInspection(parentEnv, inspect)
	if err != nil {
		return nil, model.RouteSourceFallback, "", err
	}
	if provider == nil {
		provider = &settings.EffectiveProviderEnvResult{Env: toMap(parentEnv)}
	}
	return resolveAPIBaseWithSources(explicit, toMap(parentEnv), provider.Env, provider.KeySources)
}

func DetectUnsupportedEnv(env map[string]string) []string {
	return detectUnsupported(env)
}

func formatMalformedEnvIssues(issues []settings.MalformedEnvIssue) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", issue.Source, issue.Path, issue.Error))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func rewrittenChildArgs(parsed *settings.ParsedFlagSettings, original []string) []string {
	if parsed == nil {
		out := make([]string, len(original))
		copy(out, original)
		return out
	}
	out := make([]string, len(parsed.RemainingArgs))
	copy(out, parsed.RemainingArgs)
	return out
}

func policyNetworkEnvError(conflicts []settings.EnvConflict) error {
	return fmt.Errorf("detectable local/cache policy-managed network/trust env are unsupported with stock Claude Code when running under CCWRAP; CCWRAP only inspects local/cache policy sources pre-launch, and remote managed settings, MDM, and HKCU may still exist and remain unsupported; move proxy settings to --egress-proxy, launcher shell env, or non-policy user/global Claude settings, install enterprise CA trust in the host OS trust store, and remove these keys from managed policy settings: %s", formatSettingConflicts(conflicts))
}

func formatSettingConflicts(conflicts []settings.EnvConflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", conflict.Source, conflict.Path, strings.Join(conflict.Keys, ", ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func formatStringMapKeys(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func toMap(env []string) map[string]string {
	out := map[string]string{}
	for _, pair := range env {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}
