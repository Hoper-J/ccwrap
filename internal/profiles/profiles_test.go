package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPathUsesStateDir(t *testing.T) {
	got := DefaultPath("/var/lib/ccwrap-state")
	want := filepath.Join("/var/lib/ccwrap-state", "profiles.json")
	if got != want {
		t.Fatalf("DefaultPath = %q, want %q", got, want)
	}
}

func TestProfileStructFieldsExist(t *testing.T) {
	p := Profile{
		Name:            "gw-a",
		Provider:        "AcmeGW",
		BaseURL:         "https://gw.acme.example",
		Auth:            &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ACME_TOKEN"},
		ModelAliases:    map[string]string{"claude-sonnet-4-5": "gpt-5.5"},
		UpstreamHeaders: map[string]string{"x-acme": "1"},
		Egress:          EgressSpec{Mode: "http", URL: "http://127.0.0.1:10800"},
	}
	if p.Name != "gw-a" || p.Auth.KeyEnv != "ACME_TOKEN" || p.Egress.URL != "http://127.0.0.1:10800" {
		t.Fatalf("profile fields not wired: %#v", p)
	}
	f := File{Default: InheritEnv, Profiles: map[string]Profile{"gw-a": p}}
	if f.Default != "inherit-env" || len(f.Profiles) != 1 {
		t.Fatalf("file fields not wired: %#v", f)
	}
}

func TestParseFullFileCoercesAliasSubmapViaModelalias(t *testing.T) {
	// Anthropic profile omits "auth" entirely — the new way to express
	// "ccwrap does not own auth for this profile" (replaces mode=passthrough).
	data := []byte(`{
	  "default": "anthropic",
	  "profiles": {
	    "anthropic": {
	      "provider": "Anthropic",
	      "base_url": "https://api.anthropic.com",
	      "model_aliases": {},
	      "upstream_headers": {},
	      "egress": {"mode": "inherit"}
	    },
	    "gw-a": {
	      "provider": "AcmeGW",
	      "base_url": "https://gw.acme.example",
	      "auth": {"mode": "ccwrap_bearer", "key_env": "ACME_TOKEN"},
	      "model_aliases": {"claude-sonnet-4-5": "gpt-5.5"},
	      "egress": {"mode": "http", "url": "http://127.0.0.1:10800"}
	    }
	  }
	}`)
	f, err := Parse(data, "profiles.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Default != "anthropic" || len(f.Profiles) != 2 {
		t.Fatalf("unexpected file: %#v", f)
	}
	gw := f.Profiles["gw-a"]
	if gw.Name != "gw-a" {
		t.Fatalf("Parse must inject Name from the map key, got %q", gw.Name)
	}
	if gw.Provider != "AcmeGW" || gw.BaseURL != "https://gw.acme.example" {
		t.Fatalf("gw-a fields: %#v", gw)
	}
	if gw.Auth.Mode != "ccwrap_bearer" || gw.Auth.KeyEnv != "ACME_TOKEN" {
		t.Fatalf("gw-a auth: %#v", gw.Auth)
	}
	if gw.ModelAliases["claude-sonnet-4-5"] != "gpt-5.5" {
		t.Fatalf("alias submap not coerced via modelalias.ParseJSON: %#v", gw.ModelAliases)
	}
	if gw.Egress.Mode != "http" || gw.Egress.URL != "http://127.0.0.1:10800" {
		t.Fatalf("gw-a egress: %#v", gw.Egress)
	}
	an := f.Profiles["anthropic"]
	if an.Auth != nil {
		t.Fatalf("anthropic with no auth block must yield Auth=nil; got %#v", an.Auth)
	}
	if an.Egress.Mode != "inherit" {
		t.Fatalf("anthropic egress: %#v", an.Egress)
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	_, err := Parse([]byte(`{"profiles": {`), "profiles.json")
	if err == nil {
		t.Fatal("expected malformed-JSON error")
	}
	if !contains(err.Error(), "profiles.json") {
		t.Fatalf("error should name the source: %v", err)
	}
}

func TestParseRejectsBadAliasSubmap(t *testing.T) {
	_, err := Parse([]byte(`{"profiles":{"x":{"model_aliases":{"claude-sonnet-4-5":7}}}}`), "profiles.json")
	if err == nil {
		t.Fatal("expected alias-submap coercion error")
	}
}

// TestParse_AuthOmitted — profile with no "auth" field at all → Auth == nil.
func TestParse_AuthOmitted(t *testing.T) {
	data := []byte(`{
        "default": "p",
        "profiles": {
            "p": { "base_url": "https://api.anthropic.com" }
        }
    }`)
	f, err := Parse(data, "profiles.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	prof, ok := f.Profiles["p"]
	if !ok {
		t.Fatal("profile p missing")
	}
	if prof.Auth != nil {
		t.Errorf("Auth = %+v, want nil", prof.Auth)
	}
}

// TestParse_AuthExplicitNull — "auth": null → Auth == nil.
func TestParse_AuthExplicitNull(t *testing.T) {
	data := []byte(`{
        "default": "p",
        "profiles": {
            "p": { "base_url": "https://api.anthropic.com", "auth": null }
        }
    }`)
	f, err := Parse(data, "profiles.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Profiles["p"].Auth != nil {
		t.Error("Auth must be nil for explicit null")
	}
}

// TestParse_AuthPresent — Auth pointer is allocated with subfields.
func TestParse_AuthPresent(t *testing.T) {
	data := []byte(`{
        "default": "p",
        "profiles": {
            "p": {
                "base_url": "http://example.test",
                "auth": { "mode": "ccwrap_bearer", "key": "sk-test" }
            }
        }
    }`)
	f, err := Parse(data, "profiles.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := f.Profiles["p"].Auth
	if a == nil {
		t.Fatal("Auth nil, want allocated")
	}
	if a.Mode != "ccwrap_bearer" {
		t.Errorf("Mode = %q, want ccwrap_bearer", a.Mode)
	}
	if a.Key != "sk-test" {
		t.Errorf("Key = %q, want sk-test", a.Key)
	}
}

func TestLoadMissingFileReturnsNilNil(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file must not error (zero-touch), got %v", err)
	}
	if f != nil {
		t.Fatalf("missing file must return nil *File, got %#v", f)
	}
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(`{"default":"inherit-env","profiles":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f == nil || f.Default != InheritEnv {
		t.Fatalf("unexpected: %#v", f)
	}
}

func TestProfileNonSecretAccessors(t *testing.T) {
	p := &Profile{
		Name:         "gw-a",
		Provider:     "AcmeGW",
		BaseURL:      "https://gw.acme.example:8443/v1",
		Auth:         &AuthSpec{Mode: "ccwrap_bearer", Key: "sk-SECRET-INLINE", KeyEnv: "ACME_TOKEN"},
		ModelAliases: map[string]string{"claude-sonnet-4-5": "gpt-5.5", "claude-haiku-4-5": "gpt-5-mini"},
		Egress:       EgressSpec{Mode: "http", URL: "http://127.0.0.1:10800"},
	}
	if got := p.Label(); got != "gw-a (AcmeGW)" {
		t.Fatalf("Label = %q, want %q", got, "gw-a (AcmeGW)")
	}
	if got := p.BaseURLHost(); got != "gw.acme.example:8443" {
		t.Fatalf("BaseURLHost = %q", got)
	}
	if got := p.ModelAliasCount(); got != 2 {
		t.Fatalf("ModelAliasCount = %d, want 2", got)
	}
	if got := p.EgressSummary(); got != "http 127.0.0.1:10800" {
		t.Fatalf("EgressSummary = %q", got)
	}
	for _, s := range []string{p.Label(), p.BaseURLHost(), p.EgressSummary()} {
		if contains(s, "sk-SECRET-INLINE") || contains(s, "ACME_TOKEN") {
			t.Fatalf("non-secret accessor leaked a credential: %q", s)
		}
	}
}

func TestProfileEgressSummaryVariants(t *testing.T) {
	if (&Profile{Egress: EgressSpec{Mode: "inherit"}}).EgressSummary() != "inherit" {
		t.Fatal("inherit summary")
	}
	if (&Profile{Egress: EgressSpec{Mode: "direct"}}).EgressSummary() != "direct" {
		t.Fatal("direct summary")
	}
	if (&Profile{Egress: EgressSpec{}}).EgressSummary() != "inherit" {
		t.Fatal("empty egress mode defaults to inherit summary")
	}
	if got := (&Profile{Provider: "", Name: "solo"}).Label(); got != "solo" {
		t.Fatalf("Label with empty provider = %q, want %q", got, "solo")
	}
}

func sampleFile() *File {
	// "anthropic" and "solo" omit Auth (== nil) — the new way to express
	// "ccwrap does not own auth for this profile" (replaces mode=passthrough).
	return &File{
		Default: "anthropic",
		Profiles: map[string]Profile{
			"anthropic":    {Name: "anthropic", Provider: "Anthropic", BaseURL: "https://api.anthropic.com", Egress: EgressSpec{Mode: "inherit"}},
			"gw-a":         {Name: "gw-a", Provider: "AcmeGW", BaseURL: "https://gw.acme.example", Auth: &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ACME_TOKEN"}, Egress: EgressSpec{Mode: "http", URL: "http://127.0.0.1:10800"}},
			"gw-a-staging": {Name: "gw-a-staging", Provider: "AcmeGW", BaseURL: "https://stg.acme.example", Auth: &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ACME_TOKEN"}, Egress: EgressSpec{Mode: "inherit"}},
			"solo":         {Name: "solo", Provider: "Solo", BaseURL: "https://solo.example", Egress: EgressSpec{Mode: "inherit"}},
		},
	}
}

func TestSelectByExactName(t *testing.T) {
	p, inherit, err := sampleFile().Select("gw-a")
	if err != nil || inherit {
		t.Fatalf("Select(gw-a): inherit=%v err=%v", inherit, err)
	}
	if p == nil || p.Name != "gw-a" {
		t.Fatalf("Select(gw-a) = %#v", p)
	}
}

func TestSelectEmptyUsesPersistedDefault(t *testing.T) {
	p, inherit, err := sampleFile().Select("")
	if err != nil || inherit {
		t.Fatalf("Select(\"\"): inherit=%v err=%v", inherit, err)
	}
	if p == nil || p.Name != "anthropic" {
		t.Fatalf("persisted default not chosen: %#v", p)
	}
}

func TestSelectPersistedDefaultInheritEnv(t *testing.T) {
	f := sampleFile()
	f.Default = InheritEnv
	p, inherit, err := f.Select("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !inherit || p != nil {
		t.Fatalf("expected inherit-env: p=%#v inherit=%v", p, inherit)
	}
}

func TestSelectExplicitInheritEnvRequest(t *testing.T) {
	p, inherit, err := sampleFile().Select("inherit-env")
	if err != nil || !inherit || p != nil {
		t.Fatalf("explicit inherit-env: p=%#v inherit=%v err=%v", p, inherit, err)
	}
}

func TestSelectGroupResolvesToGroupDefault(t *testing.T) {
	_, _, err := sampleFile().Select("AcmeGW")
	if err == nil {
		t.Fatal("expected ambiguous-group error for a >1 group with no in-group default")
	}
	if !contains(err.Error(), "gw-a") || !contains(err.Error(), "gw-a-staging") {
		t.Fatalf("error should list the group's profiles: %v", err)
	}
}

func TestSelectGroupWithSingleProfileIsThatProfile(t *testing.T) {
	p, inherit, err := sampleFile().Select("Solo")
	if err != nil || inherit {
		t.Fatalf("Select(Solo): inherit=%v err=%v", inherit, err)
	}
	if p == nil || p.Name != "solo" {
		t.Fatalf("single-profile group must resolve to that profile: %#v", p)
	}
}

func TestSelectGroupDefaultWhenPersistedDefaultInGroup(t *testing.T) {
	f := sampleFile()
	f.Default = "gw-a"
	p, inherit, err := f.Select("AcmeGW")
	if err != nil || inherit {
		t.Fatalf("Select(AcmeGW): inherit=%v err=%v", inherit, err)
	}
	if p == nil || p.Name != "gw-a" {
		t.Fatalf("group default should be the in-group persisted default: %#v", p)
	}
}

func TestSelectUnknownNameErrors(t *testing.T) {
	_, _, err := sampleFile().Select("nope")
	if err == nil || !contains(err.Error(), "nope") {
		t.Fatalf("expected unknown-profile error naming \"nope\", got %v", err)
	}
}

func TestSelectNilFileIsInheritEnv(t *testing.T) {
	var f *File
	p, inherit, err := f.Select("")
	if err != nil || !inherit || p != nil {
		t.Fatalf("nil file Select(\"\") must be inherit-env: p=%#v inherit=%v err=%v", p, inherit, err)
	}
}

func TestSelectNilFileWithExplicitNameErrors(t *testing.T) {
	var f *File
	_, _, err := f.Select("gw-a")
	if err == nil {
		t.Fatal("explicit --profile with no profiles.json must error (not silently inherit-env)")
	}
}

func TestParseMinimalProfileNoModelAliases(t *testing.T) {
	// A spec-valid minimal profile omits model_aliases (omitempty) and
	// the auth block entirely (Auth=nil → ccwrap does not own auth).
	// It must parse cleanly with zero aliases — NOT error, and NOT
	// turn its own provider/base_url fields into phantom alias rules.
	data := []byte(`{"profiles":{"min":{"provider":"Acme","base_url":"https://acme.example","egress":{"mode":"inherit"}}}}`)
	f, err := Parse(data, "profiles.json")
	if err != nil {
		t.Fatalf("minimal profile (no model_aliases) must parse cleanly, got: %v", err)
	}
	p, ok := f.Profiles["min"]
	if !ok {
		t.Fatalf("profile 'min' missing: %#v", f)
	}
	if p.ModelAliasCount() != 0 {
		t.Fatalf("minimal profile must have 0 aliases, got %d (%#v)", p.ModelAliasCount(), p.ModelAliases)
	}
	if _, bad := p.ModelAliases["provider"]; bad {
		t.Fatalf("phantom alias from profile field 'provider': %#v", p.ModelAliases)
	}
	if _, bad := p.ModelAliases["base_url"]; bad {
		t.Fatalf("phantom alias from profile field 'base_url': %#v", p.ModelAliases)
	}
	if p.Provider != "Acme" || p.BaseURL != "https://acme.example" || p.Auth != nil {
		t.Fatalf("minimal profile fields not preserved: %#v", p)
	}
}

func TestParseNullModelAliasesNoAliases(t *testing.T) {
	f, err := Parse([]byte(`{"profiles":{"n":{"provider":"X","base_url":"https://x.example","model_aliases":null}}}`), "profiles.json")
	if err != nil {
		t.Fatalf("model_aliases:null must parse cleanly, got %v", err)
	}
	pn := f.Profiles["n"]
	if pn.ModelAliasCount() != 0 {
		t.Fatalf("model_aliases:null => 0 aliases, got %#v", pn.ModelAliases)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringIndex(s, sub) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
