package control

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProfileCatalogResponseJSONShape locks the wire shape: empty
// Items must serialize as [] not null (UI relies on Array.isArray length).
func TestProfileCatalogResponseJSONShape(t *testing.T) {
	resp := ProfileCatalogResponse{
		HasProfilesFile: false,
		Items:           []SafeCatalogItem{},
		Source:          "/state/profiles.json",
	}
	blob, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(blob)
	for _, want := range []string{
		`"has_profiles_file":false`,
		`"items":[]`,
		`"source":"/state/profiles.json"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("response JSON missing %q\n  got: %s", want, got)
		}
	}
}

// TestProfileCatalogResponseJSONShape_EnvBaseURLHost pins the omitempty
// wire-shape for the env-base-url-host hint surfaced to the popover.
// Empty value MUST NOT appear in the wire JSON (saves bytes + keeps the
// wire shape compatible with viewers that haven't been updated).
func TestProfileCatalogResponseJSONShape_EnvBaseURLHost(t *testing.T) {
	t.Run("omitempty-when-blank", func(t *testing.T) {
		resp := ProfileCatalogResponse{Items: []SafeCatalogItem{}, EnvBaseURLHost: ""}
		blob, _ := json.Marshal(resp)
		if strings.Contains(string(blob), `"env_base_url_host"`) {
			t.Fatalf("env_base_url_host must be omitempty when blank: %s", blob)
		}
	})
	t.Run("emitted-when-set", func(t *testing.T) {
		resp := ProfileCatalogResponse{Items: []SafeCatalogItem{}, EnvBaseURLHost: "gw.example.com:8080"}
		blob, _ := json.Marshal(resp)
		if !strings.Contains(string(blob), `"env_base_url_host":"gw.example.com:8080"`) {
			t.Fatalf("env_base_url_host wire field missing: %s", blob)
		}
	})
}

func TestSafeCatalogItem_NoEnvNameField(t *testing.T) {
	// Invariant: env-var NAME never on the wire.
	// Marshal a populated item and confirm no "auth_key_env" key appears.
	item := SafeCatalogItem{
		Name: "alpha", Provider: "Acme",
		BaseURLHost: "gateway.example.com",
		Auth: &SafeAuthSpec{
			Mode:      "ccwrap_bearer",
			HasKeyEnv: true, // boolean flag only
		},
	}
	blob, _ := json.Marshal(item)
	if strings.Contains(string(blob), `"auth_key_env"`) {
		t.Fatalf("SafeCatalogItem must NOT carry auth_key_env field: %s", blob)
	}
	if !strings.Contains(string(blob), `"has_key_env":true`) {
		t.Fatalf("SafeCatalogItem must carry has_key_env boolean: %s", blob)
	}
}
