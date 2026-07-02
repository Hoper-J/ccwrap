package envpolicy

import (
	"sort"
	"strings"
)

var proxyKeys = map[string]struct{}{
	"HTTPS_PROXY": {},
	"https_proxy": {},
	"HTTP_PROXY":  {},
	"http_proxy":  {},
	"NO_PROXY":    {},
	"no_proxy":    {},
	"ALL_PROXY":   {},
	"all_proxy":   {},
}

var trustKeys = map[string]struct{}{
	"NODE_EXTRA_CA_CERTS":                 {},
	"SSL_CERT_FILE":                       {},
	"SSL_CERT_DIR":                        {},
	"REQUESTS_CA_BUNDLE":                  {},
	"CURL_CA_BUNDLE":                      {},
	"GIT_SSL_CAINFO":                      {},
	"NODE_TLS_REJECT_UNAUTHORIZED":        {},
	"CLAUDE_CODE_CERT_STORE":              {},
	"OTEL_EXPORTER_OTLP_ENDPOINT":         {},
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":    {},
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": {},
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  {},
}

var providerRoutingAuthKeys = map[string]struct{}{
	"ANTHROPIC_BASE_URL":      {},
	"ANTHROPIC_API_KEY":       {},
	"ANTHROPIC_AUTH_TOKEN":    {},
	"CLAUDE_CODE_OAUTH_TOKEN": {},
}

// providerControlKeys are provider selection/routing/auth controls that CCWRAP
// must not allow through as a second Claude-side control path when CCWRAP owns the
// launch contract. They exclude model preference keys; CCWRAP may safely carry
// model defaults as user intent without allowing Claude settings to reroute the
// network or provider.
var providerControlKeys = map[string]struct{}{
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": {},
	"CLAUDE_CODE_USE_BEDROCK":              {},
	"CLAUDE_CODE_USE_VERTEX":               {},
	"CLAUDE_CODE_USE_FOUNDRY":              {},
	"ANTHROPIC_BASE_URL":                   {},
	"ANTHROPIC_BEDROCK_BASE_URL":           {},
	"ANTHROPIC_VERTEX_BASE_URL":            {},
	"ANTHROPIC_FOUNDRY_BASE_URL":           {},
	"ANTHROPIC_FOUNDRY_RESOURCE":           {},
	"ANTHROPIC_VERTEX_PROJECT_ID":          {},
	"CLOUD_ML_REGION":                      {},
	"ANTHROPIC_API_KEY":                    {},
	"ANTHROPIC_AUTH_TOKEN":                 {},
	"CLAUDE_CODE_OAUTH_TOKEN":              {},
	"AWS_BEARER_TOKEN_BEDROCK":             {},
	"ANTHROPIC_FOUNDRY_API_KEY":            {},
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH":        {},
	"CLAUDE_CODE_SKIP_VERTEX_AUTH":         {},
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH":        {},
	"ANTHROPIC_BEDROCK_SERVICE_TIER":       {},
}

var providerControlPrefixes = []string{
	"VERTEX_REGION_CLAUDE_",
}

// modelPreferenceKeys tune model selection/catalog defaults. They do not create
// a second network/provider/auth control path, so CCWRAP preserves trusted user
// intent by injecting them into the child process env after sanitising settings.
var modelPreferenceKeys = map[string]struct{}{
	"ANTHROPIC_MODEL":                                       {},
	"ANTHROPIC_CUSTOM_MODEL_OPTION":                         {},
	"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION":             {},
	"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":                    {},
	"ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES":  {},
	"ANTHROPIC_DEFAULT_FABLE_MODEL":                         {},
	"ANTHROPIC_DEFAULT_FABLE_MODEL_DESCRIPTION":             {},
	"ANTHROPIC_DEFAULT_FABLE_MODEL_NAME":                    {},
	"ANTHROPIC_DEFAULT_FABLE_MODEL_SUPPORTED_CAPABILITIES":  {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":                         {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION":             {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME":                    {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES":  {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL":                          {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION":              {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME":                     {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES":   {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL":                        {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION":            {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME":                   {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES": {},
	"ANTHROPIC_SMALL_FAST_MODEL":                            {},
	"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION":                 {},
	"CLAUDE_CODE_SUBAGENT_MODEL":                            {},
}

var unsupportedTransportAuthKeys = map[string]struct{}{
	"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":     {},
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR": {},
	"ANTHROPIC_UNIX_SOCKET":                   {},
	"CLAUDE_CODE_CUSTOM_OAUTH_URL":            {},
}

var spawnScrubKeys = func() []string {
	m := map[string]struct{}{}
	for k := range providerControlKeys {
		m[k] = struct{}{}
	}
	for k := range unsupportedTransportAuthKeys {
		m[k] = struct{}{}
	}
	for k := range proxyKeys {
		m[k] = struct{}{}
	}
	for k := range trustKeys {
		m[k] = struct{}{}
	}
	return sortedKeys(m)
}()

func IsProxyKey(key string) bool               { _, ok := proxyKeys[key]; return ok }
func IsTrustKey(key string) bool               { _, ok := trustKeys[key]; return ok }
func IsManagedNetworkTrustKey(key string) bool { return IsProxyKey(key) || IsTrustKey(key) }
func IsProviderRoutingAuthKey(key string) bool {
	_, ok := providerRoutingAuthKeys[strings.ToUpper(key)]
	return ok
}
func IsProviderControlKey(key string) bool {
	upper := strings.ToUpper(key)
	if _, ok := providerControlKeys[upper]; ok {
		return true
	}
	for _, prefix := range providerControlPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
func IsModelPreferenceKey(key string) bool {
	_, ok := modelPreferenceKeys[strings.ToUpper(key)]
	return ok
}

func IsCCWRAPInternalKey(key string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(key)), "CCWRAP_")
}

// IsHostManagedProviderKey mirrors the checked-in Claude Code reference for
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST. CCWRAP does not use this full set for
// generated settings stripping because model preferences are preserved via
// CCWRAP-mediated child env injection.
func IsHostManagedProviderKey(key string) bool {
	return IsProviderControlKey(key) || IsModelPreferenceKey(key)
}
func IsUnsupportedTransportAuthKey(key string) bool {
	_, ok := unsupportedTransportAuthKeys[strings.ToUpper(key)]
	return ok
}
func IsGeneratedSessionSettingsStripKey(key string) bool {
	return IsManagedNetworkTrustKey(key) || IsHostManagedProviderKey(key) || IsUnsupportedTransportAuthKey(key) || IsCCWRAPInternalKey(key)
}
func IsSpawnScrubKey(key string) bool {
	return IsProviderControlKey(key) || IsUnsupportedTransportAuthKey(key) || IsManagedNetworkTrustKey(key) || IsCCWRAPInternalKey(key)
}
func UnsupportedTransportAuthKeys() []string { return sortedKeys(unsupportedTransportAuthKeys) }
func ProviderRoutingAuthKeys() []string      { return sortedKeys(providerRoutingAuthKeys) }
func ProviderControlKeys() []string          { return sortedKeys(providerControlKeys) }
func ModelPreferenceKeys() []string          { return sortedKeys(modelPreferenceKeys) }
func HostManagedProviderKeys() []string {
	m := map[string]struct{}{}
	for k := range providerControlKeys {
		m[k] = struct{}{}
	}
	for k := range modelPreferenceKeys {
		m[k] = struct{}{}
	}
	return sortedKeys(m)
}
func SpawnScrubKeys() []string {
	out := make([]string, len(spawnScrubKeys))
	copy(out, spawnScrubKeys)
	return out
}
func ManagedNetworkTrustKeys() []string {
	m := map[string]struct{}{}
	for k := range proxyKeys {
		m[k] = struct{}{}
	}
	for k := range trustKeys {
		m[k] = struct{}{}
	}
	return sortedKeys(m)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
