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
