package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/envpolicy"
)

type ParsedFlagSettings struct {
	OriginalArgValue string
	Path             string
	Inline           bool
	Settings         map[string]any
	RemainingArgs    []string
	SettingSources   *string
}

type EnvConflict struct {
	Source string   `json:"source"`
	Path   string   `json:"path"`
	Keys   []string `json:"keys"`
}

type MalformedEnvIssue struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Error  string `json:"error"`
}

type InspectionResult struct {
	ParsedFlagSettings     *ParsedFlagSettings `json:"parsed_flag_settings,omitempty"`
	ActiveSources          []string            `json:"active_sources,omitempty"`
	OverriddenNetworkEnv   []EnvConflict       `json:"overridden_network_env,omitempty"`
	PolicyNetworkEnv       []EnvConflict       `json:"policy_network_env,omitempty"`
	UnsupportedEnv         []EnvConflict       `json:"unsupported_env,omitempty"`
	MalformedEnv           []MalformedEnvIssue `json:"malformed_env,omitempty"`
	APIKeyHelperHits       []string            `json:"api_key_helper_hits,omitempty"`
	DangerousShellSettings []EnvConflict       `json:"dangerous_shell_settings,omitempty"`
	CustomAuthHeaderEnv    []EnvConflict       `json:"custom_auth_header_env,omitempty"`
	ModelOverrideHits      []EnvConflict       `json:"model_override_hits,omitempty"`
	CCWRAPInternalEnvHits  []EnvConflict       `json:"ccwrap_internal_env_hits,omitempty"`
	docs                   []settingsDoc
}

type EffectiveProxyEnvResult struct {
	Env                     map[string]string `json:"env,omitempty"`
	ContributingSources     []string          `json:"contributing_sources,omitempty"`
	IgnoredPolicyNetworkEnv []EnvConflict     `json:"ignored_policy_network_env,omitempty"`
}

// EffectiveProviderEnvResult captures the provider-routing/auth environment
// that CCWRAP should own when Claude Code is launched in host-managed mode.
// Trusted provider/auth settings follow Claude Code's pre-trust order:
// globalConfig, userSettings, flagSettings, then policySettings. Project and
// local settings are intentionally ignored for provider/auth keys because
// Claude Code strips them before trust has been established.
type EffectiveProviderEnvResult struct {
	Env                             map[string]string `json:"env,omitempty"`
	KeySources                      map[string]string `json:"key_sources,omitempty"`
	ContributingSources             []string          `json:"contributing_sources,omitempty"`
	IgnoredProjectScopedProviderEnv []EnvConflict     `json:"ignored_project_scoped_provider_env,omitempty"`
}

// EffectiveModelEnvResult captures model preference env that CCWRAP preserves as
// user intent while still running Claude Code in host-managed provider mode.
// These keys affect model/catalog defaults, not the provider/auth/network
// ownership path. Trusted settings follow the same pre-trust order as
// provider/auth; project/local model env is ignored before trust.
type EffectiveModelEnvResult struct {
	Env                          map[string]string `json:"env,omitempty"`
	KeySources                   map[string]string `json:"key_sources,omitempty"`
	ContributingSources          []string          `json:"contributing_sources,omitempty"`
	IgnoredProjectScopedModelEnv []EnvConflict     `json:"ignored_project_scoped_model_env,omitempty"`
}

type settingsDoc struct {
	Source   string
	Path     string
	Settings map[string]any
	Env      map[string]string
	EnvErr   error
}

// dangerousShellSettingsOtherThanAPIKeyHelper enumerates Claude settings
// fields that, when present, mean a shell command will execute at request
// time on a credential or header-injection path. apiKeyHelper is tracked
// separately. statusLine is deliberately excluded: it's a UI decoration in
// 99% of real configs (git branch, npm version, clock), the false-positive
// rate of blocking a launch on its presence was high, and there's no clean
// recovery path (users can't easily drop it from ~/.claude/settings.json
// without losing their normal claude-code UX). Users who explicitly
// construct a credential-leaking statusLine remain free to do so on their
// own responsibility.
var dangerousShellSettingsOtherThanAPIKeyHelper = []string{
	"awsAuthRefresh",
	"awsCredentialExport",
	"gcpAuthRefresh",
	"otelHeadersHelper",
}

func ClaudeConfigHomeDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func GlobalConfigPath() string {
	legacy := filepath.Join(ClaudeConfigHomeDir(), ".config.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

func CandidateSettingsPaths(cwd string) []string {
	allowed := defaultAllowedSources()
	docs, _, _ := activeSettingsDocs(cwd, nil, allowed)
	return docsPaths(docs)
}

func ActiveSettingsSources(cwd string, childArgs []string) ([]string, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return inspect.ActiveSources, nil
}

func DetectAPIKeyHelper(cwd string, childArgs []string) ([]string, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return inspect.APIKeyHelperHits, nil
}

func DetectOverriddenNetworkEnv(cwd string, childArgs []string) ([]EnvConflict, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return inspect.OverriddenNetworkEnv, nil
}

func DetectPolicyNetworkEnv(cwd string, childArgs []string) ([]EnvConflict, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return inspect.PolicyNetworkEnv, nil
}

func DetectProxyAndCAConflicts(cwd string, childArgs []string) ([]EnvConflict, error) {
	return DetectPolicyNetworkEnv(cwd, childArgs)
}

func DetectUnsupportedEnv(cwd string, childArgs []string) ([]EnvConflict, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return inspect.UnsupportedEnv, nil
}

func EffectiveProxyEnv(cwd string, childArgs []string, parentEnv []string) (*EffectiveProxyEnvResult, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return EffectiveProxyEnvFromInspection(parentEnv, inspect)
}

func EffectiveProxyEnvFromInspection(parentEnv []string, inspect *InspectionResult) (*EffectiveProxyEnvResult, error) {
	env := envSliceToMap(parentEnv)
	if inspect == nil {
		return &EffectiveProxyEnvResult{Env: env}, nil
	}
	var sources []string
	for _, doc := range inspect.docs {
		if doc.Source == "policySettings" {
			continue
		}
		if doc.EnvErr != nil {
			return nil, fmt.Errorf("%s env: %w", doc.Source, doc.EnvErr)
		}
		proxyEnv := filterEnv(doc.Env, envpolicy.IsProxyKey)
		if len(proxyEnv) == 0 {
			continue
		}
		for k, v := range proxyEnv {
			env[k] = v
		}
		sources = append(sources, doc.Source)
	}
	return &EffectiveProxyEnvResult{
		Env:                     env,
		ContributingSources:     dedupe(sources),
		IgnoredPolicyNetworkEnv: inspect.PolicyNetworkEnv,
	}, nil
}

func EffectiveProviderEnv(cwd string, childArgs []string, parentEnv []string) (*EffectiveProviderEnvResult, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return EffectiveProviderEnvFromInspection(parentEnv, inspect)
}

func EffectiveProviderEnvFromInspection(parentEnv []string, inspect *InspectionResult) (*EffectiveProviderEnvResult, error) {
	env := envSliceToMap(parentEnv)
	keySources := map[string]string{}
	for _, key := range envpolicy.ProviderRoutingAuthKeys() {
		if strings.TrimSpace(env[key]) != "" {
			keySources[key] = "inherited_env"
		}
	}
	if inspect == nil {
		return &EffectiveProviderEnvResult{Env: env, KeySources: keySources}, nil
	}

	trustedOrder := []string{"globalConfig", "userSettings", "flagSettings", "policySettings"}
	var sources []string
	for _, source := range trustedOrder {
		for _, doc := range inspect.docs {
			if doc.Source != source {
				continue
			}
			if doc.EnvErr != nil {
				return nil, fmt.Errorf("%s env: %w", doc.Source, doc.EnvErr)
			}
			providerEnv := filterEnv(doc.Env, envpolicy.IsProviderRoutingAuthKey)
			if len(providerEnv) == 0 {
				continue
			}
			for k, v := range providerEnv {
				env[k] = v
				keySources[k] = doc.Source
			}
			sources = append(sources, doc.Source)
		}
	}

	var ignored []EnvConflict
	for _, doc := range inspect.docs {
		if doc.Source != "projectSettings" && doc.Source != "localSettings" {
			continue
		}
		if doc.EnvErr != nil {
			return nil, fmt.Errorf("%s env: %w", doc.Source, doc.EnvErr)
		}
		keys := matchingEnvKeys(doc.Env, envpolicy.IsProviderRoutingAuthKey)
		if len(keys) > 0 {
			ignored = append(ignored, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: keys})
		}
	}

	return &EffectiveProviderEnvResult{
		Env:                             env,
		KeySources:                      keySources,
		ContributingSources:             dedupe(sources),
		IgnoredProjectScopedProviderEnv: ignored,
	}, nil
}

func EffectiveModelEnv(cwd string, childArgs []string, parentEnv []string) (*EffectiveModelEnvResult, error) {
	inspect, err := InspectLaunch(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	return EffectiveModelEnvFromInspection(parentEnv, inspect)
}

func EffectiveModelEnvFromInspection(parentEnv []string, inspect *InspectionResult) (*EffectiveModelEnvResult, error) {
	parent := envSliceToMap(parentEnv)
	env := filterEnv(parent, envpolicy.IsModelPreferenceKey)
	keySources := map[string]string{}
	for key := range env {
		keySources[key] = "inherited_env"
	}
	if inspect == nil {
		return &EffectiveModelEnvResult{Env: env, KeySources: keySources}, nil
	}

	trustedOrder := []string{"globalConfig", "userSettings", "flagSettings", "policySettings"}
	var sources []string
	for _, source := range trustedOrder {
		for _, doc := range inspect.docs {
			if doc.Source != source {
				continue
			}
			if doc.EnvErr != nil {
				return nil, fmt.Errorf("%s env: %w", doc.Source, doc.EnvErr)
			}
			modelEnv := filterEnv(doc.Env, envpolicy.IsModelPreferenceKey)
			if len(modelEnv) == 0 {
				continue
			}
			for k, v := range modelEnv {
				env[k] = v
				keySources[k] = doc.Source
			}
			sources = append(sources, doc.Source)
		}
	}

	var ignored []EnvConflict
	for _, doc := range inspect.docs {
		if doc.Source != "projectSettings" && doc.Source != "localSettings" {
			continue
		}
		if doc.EnvErr != nil {
			return nil, fmt.Errorf("%s env: %w", doc.Source, doc.EnvErr)
		}
		keys := matchingEnvKeys(doc.Env, envpolicy.IsModelPreferenceKey)
		if len(keys) > 0 {
			ignored = append(ignored, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: keys})
		}
	}

	return &EffectiveModelEnvResult{
		Env:                          env,
		KeySources:                   keySources,
		ContributingSources:          dedupe(sources),
		IgnoredProjectScopedModelEnv: ignored,
	}, nil
}

func InspectLaunch(cwd string, childArgs []string) (*InspectionResult, error) {
	parsed, err := ParseClaudeSettingsFlags(cwd, childArgs)
	if err != nil {
		return nil, err
	}
	allowed, _, err := parseSettingSourcesFlag(parsed.RemainingArgs)
	if err != nil {
		return nil, err
	}
	docs, activeSources, err := activeSettingsDocs(cwd, parsed, allowed)
	if err != nil {
		return nil, err
	}
	result := &InspectionResult{ParsedFlagSettings: parsed, ActiveSources: activeSources, docs: docs}
	for _, doc := range docs {
		if doc.EnvErr != nil {
			result.MalformedEnv = append(result.MalformedEnv, MalformedEnvIssue{Source: doc.Source, Path: doc.Path, Error: doc.EnvErr.Error()})
			continue
		}
		if keys := matchingEnvKeys(doc.Env, envpolicy.IsManagedNetworkTrustKey); len(keys) > 0 {
			conflict := EnvConflict{Source: doc.Source, Path: doc.Path, Keys: keys}
			if doc.Source == "policySettings" {
				result.PolicyNetworkEnv = append(result.PolicyNetworkEnv, conflict)
			} else {
				result.OverriddenNetworkEnv = append(result.OverriddenNetworkEnv, conflict)
			}
		}
		if keys := matchingEnvKeys(doc.Env, envpolicy.IsUnsupportedTransportAuthKey); len(keys) > 0 {
			result.UnsupportedEnv = append(result.UnsupportedEnv, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: keys})
		}
		if keys := matchingEnvKeys(doc.Env, envpolicy.IsCCWRAPInternalKey); len(keys) > 0 {
			result.CCWRAPInternalEnvHits = append(result.CCWRAPInternalEnvHits, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: keys})
		}
		if containsKey(doc.Settings, "apiKeyHelper") {
			result.APIKeyHelperHits = append(result.APIKeyHelperHits, doc.Path)
		}
		for _, settingName := range dangerousShellSettingsOtherThanAPIKeyHelper {
			if containsKey(doc.Settings, settingName) {
				result.DangerousShellSettings = append(result.DangerousShellSettings, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: []string{settingName}})
			}
		}
		if hasAuthLikeCustomHeaders(doc.Env["ANTHROPIC_CUSTOM_HEADERS"]) {
			result.CustomAuthHeaderEnv = append(result.CustomAuthHeaderEnv, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: []string{"ANTHROPIC_CUSTOM_HEADERS"}})
		}
		if containsKey(doc.Settings, "modelOverrides") {
			result.ModelOverrideHits = append(result.ModelOverrideHits, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: []string{"modelOverrides"}})
		}
		if hasCCWRAPModelAliases(doc.Settings) {
			result.ModelOverrideHits = append(result.ModelOverrideHits, EnvConflict{Source: doc.Source, Path: doc.Path, Keys: []string{"ccwrap.modelAliases"}})
		}
	}
	sort.Strings(result.APIKeyHelperHits)
	result.APIKeyHelperHits = dedupe(result.APIKeyHelperHits)
	sortEnvConflicts(result.DangerousShellSettings)
	sortEnvConflicts(result.CCWRAPInternalEnvHits)
	return result, nil
}

func ParseClaudeSettingsFlags(cwd string, childArgs []string) (*ParsedFlagSettings, error) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	out := &ParsedFlagSettings{RemainingArgs: make([]string, 0, len(childArgs))}
	var settingsValues []string
	var settingSources string
	var settingSourcesSet bool
	for i := 0; i < len(childArgs); i++ {
		arg := childArgs[i]
		if arg == "--" {
			out.RemainingArgs = append(out.RemainingArgs, childArgs[i:]...)
			break
		}
		switch {
		case strings.HasPrefix(arg, "--settings="):
			settingsValues = append(settingsValues, strings.TrimSpace(arg[len("--settings="):]))
			continue
		case arg == "--settings":
			if i+1 >= len(childArgs) {
				return nil, fmt.Errorf("--settings requires a value")
			}
			settingsValues = append(settingsValues, strings.TrimSpace(childArgs[i+1]))
			i++
			continue
		case strings.HasPrefix(arg, "--setting-sources="):
			if !settingSourcesSet {
				settingSources = strings.TrimSpace(arg[len("--setting-sources="):])
				settingSourcesSet = true
			}
			out.RemainingArgs = append(out.RemainingArgs, arg)
			continue
		case arg == "--setting-sources":
			if i+1 >= len(childArgs) {
				return nil, fmt.Errorf("--setting-sources requires a value")
			}
			value := childArgs[i+1]
			if !settingSourcesSet {
				settingSources = strings.TrimSpace(value)
				settingSourcesSet = true
			}
			out.RemainingArgs = append(out.RemainingArgs, arg, value)
			i++
			continue
		default:
			out.RemainingArgs = append(out.RemainingArgs, arg)
		}
	}
	if settingSourcesSet {
		v := settingSources
		out.SettingSources = &v
	}
	if len(settingsValues) == 0 {
		return out, nil
	}
	if len(settingsValues) > 1 {
		return nil, fmt.Errorf("multiple --settings flags are not supported; pass exactly one")
	}
	value := settingsValues[0]
	out.OriginalArgValue = value
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("--settings value is empty")
	}
	if strings.HasPrefix(strings.TrimSpace(value), "{") {
		parsed, err := parseSettingsBytes([]byte(value), "<inline --settings>")
		if err != nil {
			return nil, err
		}
		out.Inline = true
		out.Path = "<inline --settings>"
		out.Settings = parsed
		return out, nil
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read settings file %s: %w", path, err)
	}
	parsed, err := parseSettingsBytes(data, path)
	if err != nil {
		return nil, err
	}
	out.Path = path
	out.Settings = parsed
	return out, nil
}

func MergeUserSettingsIntoCCWRAPSessionSettings(user map[string]any, ccwrapEnv map[string]string) (map[string]any, error) {
	out := map[string]any{}
	if user != nil {
		cloned, err := cloneMap(user)
		if err != nil {
			return nil, err
		}
		out = cloned
	}
	delete(out, "ccwrap")
	delete(out, "modelOverrides")
	envMap := map[string]string{}
	if raw, ok := out["env"]; ok {
		existing, err := coerceEnvMap(raw)
		if err != nil {
			return nil, fmt.Errorf("settings env: %w", err)
		}
		for k, v := range existing {
			if envpolicy.IsGeneratedSessionSettingsStripKey(k) {
				continue
			}
			envMap[k] = v
		}
	}
	for k, v := range ccwrapEnv {
		envMap[k] = v
	}
	if len(envMap) > 0 {
		out["env"] = envMap
	} else {
		delete(out, "env")
	}
	return out, nil
}

func activeSettingsDocs(cwd string, parsedFlag *ParsedFlagSettings, allowed map[string]bool) ([]settingsDoc, []string, error) {
	var docs []settingsDoc
	var activeSources []string
	add := func(source, path string) error {
		settings, ok, err := readSettingsFile(path)
		if err != nil {
			return err
		}
		if ok {
			env, envErr := docEnv(settings)
			activeSources = append(activeSources, source)
			docs = append(docs, settingsDoc{Source: source, Path: path, Settings: settings, Env: env, EnvErr: envErr})
		}
		return nil
	}
	if path := GlobalConfigPath(); strings.TrimSpace(path) != "" {
		if err := add("globalConfig", path); err != nil {
			return nil, nil, err
		}
	}
	if allowed["userSettings"] {
		if err := add("userSettings", filepath.Join(ClaudeConfigHomeDir(), userSettingsFilename())); err != nil {
			return nil, nil, err
		}
	}
	if allowed["projectSettings"] && cwd != "" {
		if err := add("projectSettings", filepath.Join(cwd, ".claude", "settings.json")); err != nil {
			return nil, nil, err
		}
	}
	if allowed["localSettings"] && cwd != "" {
		if err := add("localSettings", filepath.Join(cwd, ".claude", "settings.local.json")); err != nil {
			return nil, nil, err
		}
	}
	if err := add("policySettings", filepath.Join(ClaudeConfigHomeDir(), "remote-settings.json")); err != nil {
		return nil, nil, err
	}
	managedFiles, err := managedSettingsPaths()
	if err != nil {
		return nil, nil, err
	}
	for _, path := range managedFiles {
		if err := add("policySettings", path); err != nil {
			return nil, nil, err
		}
	}
	if parsedFlag != nil && parsedFlag.Settings != nil {
		env, envErr := docEnv(parsedFlag.Settings)
		activeSources = append(activeSources, "flagSettings")
		docs = append(docs, settingsDoc{Source: "flagSettings", Path: fallbackPath(parsedFlag.Path, "<inline --settings>"), Settings: parsedFlag.Settings, Env: env, EnvErr: envErr})
	}
	return docs, dedupe(activeSources), nil
}

func managedSettingsPaths() ([]string, error) {
	base := managedSettingsRoot()
	paths := []string{filepath.Join(base, "managed-settings.json")}
	dropInDir := filepath.Join(base, "managed-settings.d")
	entries, err := os.ReadDir(dropInDir)
	if err != nil {
		if os.IsNotExist(err) {
			return paths, nil
		}
		return nil, fmt.Errorf("read %s: %w", dropInDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			paths = append(paths, filepath.Join(dropInDir, entry.Name()))
		}
	}
	if len(paths) > 1 {
		sort.Strings(paths[1:])
	}
	return paths, nil
}

func managedSettingsRoot() string {
	if v := strings.TrimSpace(os.Getenv("CCWRAP_MANAGED_SETTINGS_DIR")); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join("/Library", "Application Support", "ClaudeCode")
	default:
		return filepath.Join("/etc", "claude-code")
	}
}

func parseSettingSourcesFlag(childArgs []string) (map[string]bool, *string, error) {
	allowed := defaultAllowedSources()
	if childArgs == nil {
		return allowed, nil, nil
	}
	var raw *string
	for i := 0; i < len(childArgs); i++ {
		arg := childArgs[i]
		if arg == "--" {
			break
		}
		var value string
		matched := false
		switch {
		case strings.HasPrefix(arg, "--setting-sources="):
			value = strings.TrimSpace(arg[len("--setting-sources="):])
			matched = true
		case arg == "--setting-sources":
			if i+1 >= len(childArgs) {
				return nil, nil, fmt.Errorf("--setting-sources requires a value")
			}
			value = strings.TrimSpace(childArgs[i+1])
			matched = true
			i++
		}
		if !matched || raw != nil {
			continue
		}
		copyValue := value
		raw = &copyValue
		allowed = map[string]bool{"userSettings": false, "projectSettings": false, "localSettings": false}
		if value == "" {
			continue
		}
		for _, token := range strings.Split(value, ",") {
			switch strings.TrimSpace(token) {
			case "user":
				allowed["userSettings"] = true
			case "project":
				allowed["projectSettings"] = true
			case "local":
				allowed["localSettings"] = true
			case "":
				// allow empty tokens so --setting-sources= works.
			default:
				return nil, nil, fmt.Errorf("invalid --setting-sources value %q", strings.TrimSpace(token))
			}
		}
	}
	return allowed, raw, nil
}

func defaultAllowedSources() map[string]bool {
	return map[string]bool{"userSettings": true, "projectSettings": true, "localSettings": true}
}

func userSettingsFilename() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_CODE_USE_COWORK_PLUGINS")))
	if v == "1" || v == "true" {
		return "cowork_settings.json"
	}
	return "settings.json"
}

func parseSettingsBytes(data []byte, source string) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", source, err)
	}
	if parsed == nil {
		return map[string]any{}, nil
	}
	return parsed, nil
}

func readSettingsFile(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	parsed, err := parseSettingsBytes(data, path)
	if err != nil {
		return nil, false, err
	}
	return parsed, true, nil
}

func docEnv(settings map[string]any) (map[string]string, error) {
	if settings == nil {
		return nil, nil
	}
	raw, ok := settings["env"]
	if !ok {
		return nil, nil
	}
	return coerceEnvMap(raw)
}

func filterEnv(env map[string]string, match func(string) bool) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range env {
		if match(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func matchingEnvKeys(env map[string]string, match func(string) bool) []string {
	if len(env) == 0 {
		return nil
	}
	var keys []string
	for k := range env {
		if match(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func coerceEnvMap(raw any) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}
	switch v := raw.(type) {
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, val := range v {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("key %q must have a string value", k)
			}
			out[k] = s
		}
		return out, nil
	case map[string]string:
		out := make(map[string]string, len(v))
		for k, val := range v {
			out[k] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected object")
	}
}

func hasAuthLikeCustomHeaders(value string) bool {
	return len(AuthLikeCustomHeaderNames(value)) > 0
}

// AuthLikeCustomHeaderNames parses ANTHROPIC_CUSTOM_HEADERS using the same
// newline-delimited "Name: Value" shape Claude Code accepts and returns header
// names that should not be Claude-visible in third-party hidden mode. Values are
// deliberately ignored so diagnostics never expose secrets.
func AuthLikeCustomHeaderNames(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var hits []string
	seen := map[string]struct{}{}
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "" || !isAuthLikeHeaderName(name) {
			continue
		}
		canonical := strings.ToLower(name)
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		hits = append(hits, name)
	}
	sort.Strings(hits)
	return hits
}

func isAuthLikeHeaderName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	compact := strings.ReplaceAll(normalized, "-", "")
	switch normalized {
	case "authorization", "proxy-authorization", "x-api-key", "x-apikey", "api-key", "x-gateway-key", "x-litellm-key", "x-provider-key", "x-provider-token":
		return true
	}
	switch compact {
	case "authorization", "proxyauthorization", "xapikey", "apikey", "xgatewaykey", "xlitellmkey", "xproviderkey", "xprovidertoken":
		return true
	}
	return strings.Contains(compact, "token") || strings.Contains(compact, "secret") || strings.Contains(compact, "credential")
}

func hasCCWRAPModelAliases(settings map[string]any) bool {
	raw, ok := settings["ccwrap"]
	if !ok {
		return false
	}
	ccwrap, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	return containsKey(ccwrap, "modelAliases") || containsKey(ccwrap, "model_aliases")
}

func containsKey(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == key {
				return true
			}
			if containsKey(val, key) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if containsKey(val, key) {
				return true
			}
		}
	}
	return false
}

func cloneMap(in map[string]any) (map[string]any, error) {
	if in == nil {
		return map[string]any{}, nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("clone settings: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("clone settings: %w", err)
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

func docsPaths(docs []settingsDoc) []string {
	out := make([]string, 0, len(docs))
	for _, doc := range docs {
		out = append(out, doc.Path)
	}
	return dedupe(out)
}

func fallbackPath(path, fallback string) string {
	if strings.TrimSpace(path) == "" {
		return fallback
	}
	return path
}

func envSliceToMap(env []string) map[string]string {
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

func sortEnvConflicts(conflicts []EnvConflict) {
	sort.Slice(conflicts, func(i, j int) bool {
		li := conflicts[i].Source + "\x00" + conflicts[i].Path + "\x00" + strings.Join(conflicts[i].Keys, ",")
		lj := conflicts[j].Source + "\x00" + conflicts[j].Path + "\x00" + strings.Join(conflicts[j].Keys, ",")
		return li < lj
	})
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
