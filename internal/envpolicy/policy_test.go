package envpolicy

import "testing"

func TestIsHostManagedProviderKeyIncludesReferenceKeysAndVertexPrefix(t *testing.T) {
	for _, key := range []string{
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"VERTEX_REGION_CLAUDE_3_5_HAIKU",
	} {
		if !IsHostManagedProviderKey(key) {
			t.Fatalf("expected %s to be host-managed provider env", key)
		}
	}
	if IsHostManagedProviderKey("ANTHROPIC_CUSTOM_HEADERS") {
		t.Fatal("ANTHROPIC_CUSTOM_HEADERS should remain non-provider env")
	}
}

func TestIsSpawnScrubKeyIncludesProviderControlButNotModelPreferences(t *testing.T) {
	if !IsSpawnScrubKey("VERTEX_REGION_CLAUDE_4_OPUS") {
		t.Fatal("expected dynamic VERTEX_REGION_CLAUDE_* key to be scrubbed from child env")
	}
	if !IsSpawnScrubKey("HTTPS_PROXY") {
		t.Fatal("expected proxy env to be scrubbed from child env")
	}
	if !IsSpawnScrubKey("ANTHROPIC_BASE_URL") {
		t.Fatal("expected provider routing env to be scrubbed from child env")
	}
	if IsSpawnScrubKey("CLAUDE_CODE_SUBAGENT_MODEL") || IsSpawnScrubKey("ANTHROPIC_DEFAULT_SONNET_MODEL") {
		t.Fatal("model preference env should be preserved through CCWRAP-mediated child env")
	}
	if IsSpawnScrubKey("PATH") {
		t.Fatal("PATH should not be scrubbed")
	}
}

func TestGeneratedSessionSettingsStripKeyIncludesModelPreferences(t *testing.T) {
	for _, key := range []string{
		"ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_USE_VERTEX",
		"VERTEX_REGION_CLAUDE_4_OPUS",
		"HTTPS_PROXY",
		"ANTHROPIC_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
	} {
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}

func TestCCWRAPInternalKeysAreScrubbedFromChildAndGeneratedSettings(t *testing.T) {
	for _, key := range []string{"CCWRAP_MODEL_ALIASES_FILE", "CCWRAP_MODEL_ALIASES_JSON", "ccwrap_internal_demo"} {
		if !IsCCWRAPInternalKey(key) {
			t.Fatalf("expected %s to be CCWRAP-internal", key)
		}
		if !IsSpawnScrubKey(key) {
			t.Fatalf("expected %s to be spawn-scrubbed", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated session settings", key)
		}
	}
}

// The Fable model tier and the custom-model-option catalog keys are model
// preferences like the existing tiers and must flow the same way — preserved
// into the child env (NOT spawn-scrubbed) but stripped from generated
// session settings.
func TestFableAndCustomModelOptionKeysAreModelPreferences(t *testing.T) {
	for _, key := range []string{
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
		"ANTHROPIC_DEFAULT_FABLE_MODEL_DESCRIPTION",
		"ANTHROPIC_DEFAULT_FABLE_MODEL_NAME",
		"ANTHROPIC_DEFAULT_FABLE_MODEL_SUPPORTED_CAPABILITIES",
		"ANTHROPIC_CUSTOM_MODEL_OPTION",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES",
	} {
		if !IsModelPreferenceKey(key) {
			t.Fatalf("expected %s to be a model preference key", key)
		}
		if IsSpawnScrubKey(key) {
			t.Fatalf("%s is user model intent and must survive into the child env", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}

// CLAUDE_CODE_CERT_STORE selects the child's certificate store — a
// trust-surface control that must not bypass the CCWRAP-pinned CA;
// ANTHROPIC_BEDROCK_SERVICE_TIER is a provider tuning control that belongs
// with the Bedrock switches.
func TestCertStoreIsTrustKeyAndBedrockServiceTierIsProviderControl(t *testing.T) {
	if !IsTrustKey("CLAUDE_CODE_CERT_STORE") {
		t.Fatal("expected CLAUDE_CODE_CERT_STORE to be a trust key")
	}
	if !IsProviderControlKey("ANTHROPIC_BEDROCK_SERVICE_TIER") {
		t.Fatal("expected ANTHROPIC_BEDROCK_SERVICE_TIER to be a provider control key")
	}
	for _, key := range []string{"CLAUDE_CODE_CERT_STORE", "ANTHROPIC_BEDROCK_SERVICE_TIER"} {
		if !IsSpawnScrubKey(key) {
			t.Fatalf("expected %s to be spawn-scrubbed", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}

// The anthropic_aws / mantle / gateway provider families, the two extra
// base-URL overrides, and the host-auth channel keys are all provider
// selection/routing/auth controls in the Claude Code reference set. Ambient
// copies must never reach the child: the provider switches reroute the whole
// session past CCWRAP's launch contract, ARTIFACTS_API_BASE_URL redirects a
// side traffic flow independently of ANTHROPIC_BASE_URL, and the host-auth
// keys open credential channels inside the child (HOST_CREDS_FILE is armed
// by the very MANAGED_BY_HOST flag CCWRAP sets in placeholder sessions;
// HOST_AUTH_ENV_VAR flips Claude's host-managed semantics all by itself).
func TestProviderFamiliesAndHostAuthChannelAreProviderControls(t *testing.T) {
	for _, key := range []string{
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"ANTHROPIC_AWS_BASE_URL",
		"ANTHROPIC_AWS_API_KEY",
		"ANTHROPIC_AWS_WORKSPACE_ID",
		"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
		"CLAUDE_CODE_USE_MANTLE",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_API_KEY",
		"CLAUDE_CODE_SKIP_MANTLE_AUTH",
		"CLAUDE_CODE_USE_GATEWAY",
		"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
		"CLAUDE_CODE_ARTIFACTS_API_BASE_URL",
		"CLAUDE_CODE_HOST_AUTH_ENV_VAR",
		"CLAUDE_CODE_HOST_CREDS_FILE",
		"CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH",
		"CLAUDE_CODE_HOST_AUTH_REFRESH_TIMEOUT_MS",
	} {
		if !IsProviderControlKey(key) {
			t.Fatalf("expected %s to be a provider control key", key)
		}
		if !IsSpawnScrubKey(key) {
			t.Fatalf("expected %s to be spawn-scrubbed", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}

// The anthropicGoogleCloud provider family reroutes the whole session to its
// own endpoint (claude.googleapis.com by default) exactly like the other
// provider switches; the artifact/memory base-URL and token keys redirect and
// authenticate side traffic independently of ANTHROPIC_BASE_URL (same class as
// ARTIFACTS_API_BASE_URL); FOUNDRY_AUTH_TOKEN and BEDROCK_REGION_PREFIX belong
// with their families. All sit in Claude Code's host-managed provider set, so
// ambient copies must never reach the child.
func TestGoogleCloudFamilyAndSideAPIOverridesAreProviderControls(t *testing.T) {
	for _, key := range []string{
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
		"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
		"ANTHROPIC_GOOGLE_CLOUD_PROJECT",
		"ANTHROPIC_GOOGLE_CLOUD_LOCATION",
		"ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID",
		"CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_BEDROCK_REGION_PREFIX",
		"CLAUDE_CODE_ARTIFACTS_API_TOKEN",
		"CLAUDE_CODE_ARTIFACT_ASSET_BASE_URL",
		"CLAUDE_CODE_ARTIFACT_LIVE_BASE_URL",
		"CLAUDE_CODE_ARTIFACT_SYNC_BASE_URL",
		"CLAUDE_CODE_ARTIFACT_VIEWER_BASE_URL",
		"CLAUDE_CODE_MEMORY_API_BASE_URL",
		"CLAUDE_CODE_MEMORY_API_TOKEN",
	} {
		if !IsProviderControlKey(key) {
			t.Fatalf("expected %s to be a provider control key", key)
		}
		if !IsSpawnScrubKey(key) {
			t.Fatalf("expected %s to be spawn-scrubbed", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}

// Claude Code's model-selection key set also covers the default-model,
// workflow/forced subagent, auto-mode/background classifier models and the
// third-party default-seeding markers. Like the existing tiers they are user
// model intent: preserved into the child env (Claude Code ignores
// settings-sourced copies under CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST) but
// stripped from generated settings.
func TestExtendedModelSelectionKeysAreModelPreferences(t *testing.T) {
	for _, key := range []string{
		"ANTHROPIC_DEFAULT_MODEL",
		"CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE",
		"CLAUDE_CODE_AUTO_MODE_MODEL",
		"CLAUDE_CODE_BG_CLASSIFIER_MODEL",
		"CLAUDE_CODE_3P_PROBE_WROTE_SONNET_DEFAULT",
		"CLAUDE_CODE_3P_PROBE_WROTE_OPUS_DEFAULT",
		"CLAUDE_CODE_3P_PROBE_WROTE_HAIKU_DEFAULT",
		"CLAUDE_CODE_3P_SEEDED_SONNET_DEFAULT",
		"CLAUDE_CODE_3P_SEEDED_OPUS_DEFAULT",
	} {
		if !IsModelPreferenceKey(key) {
			t.Fatalf("expected %s to be a model preference key", key)
		}
		if IsSpawnScrubKey(key) {
			t.Fatalf("%s is user model intent and must survive into the child env", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}

// Doctor's provider-selection transparency check derives its key list from
// the scrub set, so a newly scrubbed provider switch is surfaced too.
func TestProviderSelectionKeysCoverEveryScrubbedProviderSwitch(t *testing.T) {
	got := map[string]bool{}
	for _, k := range ProviderSelectionKeys() {
		got[k] = true
	}
	for _, key := range []string{
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_GATEWAY",
	} {
		if !got[key] {
			t.Fatalf("expected %s in ProviderSelectionKeys(), got %v", key, ProviderSelectionKeys())
		}
	}
	for k := range got {
		if !IsSpawnScrubKey(k) {
			t.Fatalf("ProviderSelectionKeys() returned %s, which is not spawn-scrubbed", k)
		}
	}
}

func TestOTELEndpointsAreManagedNetworkTrustKeys(t *testing.T) {
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		if !IsManagedNetworkTrustKey(key) {
			t.Fatalf("expected %s to be managed network/trust env", key)
		}
		if !IsSpawnScrubKey(key) {
			t.Fatalf("expected %s to be spawn-scrubbed", key)
		}
		if !IsGeneratedSessionSettingsStripKey(key) {
			t.Fatalf("expected %s to be stripped from generated settings", key)
		}
	}
}
