package profiles

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T-seed-default-name — verifies File.Default == seeded Profile.Name so the
// default-selection logic picks up the new profile automatically on the next launch.
func TestSeedFromEnv_DefaultNameMatchesProfileName(t *testing.T) {
	spec := SeedSpec{
		Name:    "myprofile",
		BaseURL: "https://gw.example.com/",
		APIKey:  "any-api-key-value",
	}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if file.Default != "myprofile" {
		t.Fatalf("File.Default = %q; want %q", file.Default, "myprofile")
	}
	if _, ok := file.Profiles["myprofile"]; !ok {
		t.Fatalf("File.Profiles missing key %q; got keys %v", "myprofile", keysOf(file.Profiles))
	}
	if got := file.Profiles["myprofile"].Name; got != "myprofile" {
		t.Fatalf("Profile.Name = %q; want %q", got, "myprofile")
	}
}

func keysOf(m map[string]Profile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// T-seed-shape-api-key — verifies the ANTHROPIC_API_KEY → ccwrap_x_api_key
// pairing with the credential value carried inline at Auth.Key. KeyEnv is
// empty because Key always wins in applyProfileOverlay's precedence; a
// key_env alongside an inline key would be a dead field.
func TestSeedFromEnv_ShapeAPIKey(t *testing.T) {
	const tokenValue = "sk-ant-api-test-value-001"
	spec := SeedSpec{
		Name:    "alpha",
		BaseURL: "https://gw.example.com/v1/",
		APIKey:  tokenValue,
	}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	p, ok := file.Profiles["alpha"]
	if !ok {
		t.Fatalf("file.Profiles missing key alpha; got %v", keysOf(file.Profiles))
	}
	if p.Auth == nil {
		t.Fatalf("Auth = nil; want non-nil")
	}
	if p.Auth.Mode != "ccwrap_x_api_key" {
		t.Errorf("Auth.Mode = %q; want ccwrap_x_api_key", p.Auth.Mode)
	}
	if p.Auth.Key != tokenValue {
		t.Errorf("Auth.Key = %q; want inline value %q", p.Auth.Key, tokenValue)
	}
	if p.Auth.KeyEnv != "" {
		t.Errorf("Auth.KeyEnv = %q; want empty (Key wins; key_env would be a dead field)", p.Auth.KeyEnv)
	}
	if p.BaseURL != "https://gw.example.com/v1/" {
		t.Errorf("BaseURL = %q; want passthrough of input", p.BaseURL)
	}
	if p.Egress.Mode != "inherit" {
		t.Errorf("Egress.Mode = %q; want inherit", p.Egress.Mode)
	}
	if p.Egress.URL != "" {
		t.Errorf("Egress.URL = %q; want empty", p.Egress.URL)
	}
}

// T-seed-shape-auth-tok — verifies the ANTHROPIC_AUTH_TOKEN → ccwrap_bearer
// pairing when API_KEY is not set. Inline value at Auth.Key; KeyEnv empty.
func TestSeedFromEnv_ShapeAuthTok(t *testing.T) {
	const tokenValue = "sk-ant-oat01-test-value-002"
	spec := SeedSpec{
		Name:      "beta",
		BaseURL:   "https://gw.example.com/",
		AuthToken: tokenValue,
	}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	p := file.Profiles["beta"]
	if p.Auth == nil {
		t.Fatalf("Auth = nil; want non-nil")
	}
	if p.Auth.Mode != "ccwrap_bearer" {
		t.Errorf("Auth.Mode = %q; want ccwrap_bearer", p.Auth.Mode)
	}
	if p.Auth.Key != tokenValue {
		t.Errorf("Auth.Key = %q; want inline value %q", p.Auth.Key, tokenValue)
	}
	if p.Auth.KeyEnv != "" {
		t.Errorf("Auth.KeyEnv = %q; want empty", p.Auth.KeyEnv)
	}
}

// T-seed-shape-no-auth — verifies neither env set → Auth pointer is nil
// (ccwrap does not own auth for this profile). The seedAuth function used to
// emit AuthSpec{Mode: "passthrough"} as a sentinel; the auth block is now
// whole-or-nothing, so the absence of credentials is expressed as the
// absence of the auth block.
func TestSeedFromEnv_ShapeNoAuth(t *testing.T) {
	spec := SeedSpec{
		Name:    "gamma",
		BaseURL: "https://gw.example.com/",
	}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	p := file.Profiles["gamma"]
	if p.Auth != nil {
		t.Errorf("Auth = %+v; want nil (ccwrap does not own auth)", p.Auth)
	}
}

// T-seed-mode-key-pairing — table-driven Mode↔inline-Key invariant across
// all four (APIKey, AuthToken) presence combinations. APIKey beats
// AuthToken when both are set, matching applyProfileOverlay's
// explicitAuth precedence. Both empty → Auth == nil (the auth block is
// whole-or-nothing; absence expresses "ccwrap does not own auth").
func TestSeedFromEnv_ModeKeyPairing(t *testing.T) {
	const apiKeyVal = "sk-ant-api-test"
	const authTokVal = "sk-ant-oat01-test"
	cases := []struct {
		name     string
		apiKey   string
		authTok  string
		wantNil  bool   // expect Auth == nil
		wantMode string // ignored when wantNil
		wantKey  string // ignored when wantNil
	}{
		{name: "both-API_KEY-wins", apiKey: apiKeyVal, authTok: authTokVal, wantMode: "ccwrap_x_api_key", wantKey: apiKeyVal},
		{name: "api-key-only", apiKey: apiKeyVal, wantMode: "ccwrap_x_api_key", wantKey: apiKeyVal},
		{name: "auth-tok-only", authTok: authTokVal, wantMode: "ccwrap_bearer", wantKey: authTokVal},
		{name: "neither", wantNil: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			spec := SeedSpec{Name: "x", BaseURL: "https://gw/", APIKey: c.apiKey, AuthToken: c.authTok}
			file, err := SeedFromEnv(spec)
			if err != nil {
				t.Fatalf("SeedFromEnv: %v", err)
			}
			p := file.Profiles["x"]
			if c.wantNil {
				if p.Auth != nil {
					t.Errorf("Auth = %+v; want nil", p.Auth)
				}
				return
			}
			if p.Auth == nil {
				t.Fatalf("Auth = nil; want non-nil with Mode=%q Key=%q", c.wantMode, c.wantKey)
			}
			if p.Auth.Mode != c.wantMode {
				t.Errorf("Mode = %q; want %q", p.Auth.Mode, c.wantMode)
			}
			if p.Auth.Key != c.wantKey {
				t.Errorf("Key = %q; want %q", p.Auth.Key, c.wantKey)
			}
			// Inline-key seeds never carry a key_env (dead field — Key wins).
			if p.Auth.KeyEnv != "" {
				t.Errorf("KeyEnv = %q; want empty (Key wins; key_env would be a dead field)", p.Auth.KeyEnv)
			}
		})
	}
}

// T-trigger-off-reserved-name — the reserved sentinel "inherit-env" is rejected
// at the SeedFromEnv layer (defensive against TTY prompt malice/typo).
func TestSeedFromEnv_ReservedName(t *testing.T) {
	spec := SeedSpec{Name: InheritEnv, BaseURL: "https://gw/", APIKey: "x"}
	_, err := SeedFromEnv(spec)
	if err == nil {
		t.Fatalf("SeedFromEnv(InheritEnv): want error, got nil")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error message = %q; want substring 'reserved'", err.Error())
	}
}

// T-seed-empty-name-err — defensive guard against empty name reaching the
// seed layer (resolveSeedName should never emit it, but layered defense).
func TestSeedFromEnv_EmptyName(t *testing.T) {
	spec := SeedSpec{Name: "  ", BaseURL: "https://gw/", APIKey: "x"}
	_, err := SeedFromEnv(spec)
	if err == nil {
		t.Fatalf("SeedFromEnv(empty): want error, got nil")
	}
}

// T-seed-malformed-url-err — defensive guard against a non-URL BaseURL.
func TestSeedFromEnv_MalformedURL(t *testing.T) {
	spec := SeedSpec{Name: "x", BaseURL: "://broken", APIKey: "x"}
	_, err := SeedFromEnv(spec)
	if err == nil {
		t.Fatalf("SeedFromEnv(malformed): want error, got nil")
	}
}

// T-seed-shape-provider — table-driven heuristic check for the
// host-to-provider-label mapping. Locks the 10-row mapping against regression.
func TestProviderFromHost_Table(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"gateway.example.com", "gateway"},
		{"api.openai.com", "openai"},
		{"api.anthropic.com", "anthropic"},
		{"proxy.acme.io", "proxy"},
		{"10.0.0.5", "10.0.0.5"},
		{"localhost", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"claude.tools.example.com", "claude"},
		{"::1", "::1"},
		{"2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.host, func(t *testing.T) {
			got := providerFromHost(c.host)
			if got != c.want {
				t.Errorf("providerFromHost(%q) = %q; want %q", c.host, got, c.want)
			}
		})
	}
}

// T-isAllDigits — guard rails for the helper used by providerFromHost.
func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", true},
		{"123", true},
		{"1a", false},
		{"a1", false},
		{"-1", false},
		{"01234567890", true},
	}
	for _, c := range cases {
		if got := isAllDigits(c.in); got != c.want {
			t.Errorf("isAllDigits(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

// T-write-atomic — WriteFile uses O_EXCL; writing where a file already
// exists must error with errors.Is(err, fs.ErrExist) == true (post-wrap).
func TestWriteFile_AtomicEXCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed setup: %v", err)
	}
	spec := SeedSpec{Name: "x", BaseURL: "https://gw/", APIKey: "x"}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	err = WriteFile(path, file)
	if err == nil {
		t.Fatalf("WriteFile over existing path: want error, got nil")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("errors.Is(err, fs.ErrExist) = false; err = %v", err)
	}
}

// T-write-mkdirp — WriteFile must create the parent directory if missing.
func TestWriteFile_MkdirP(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c", "profiles.json")
	spec := SeedSpec{Name: "x", BaseURL: "https://gw/", APIKey: "x"}
	file, _ := SeedFromEnv(spec)
	if err := WriteFile(deep, file); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(deep); err != nil {
		t.Fatalf("stat written file: %v", err)
	}
}

// T-secret-written-inline — the seed-then-write path produces a file
// whose JSON contains the credential VALUE at the Auth.Key field and
// does NOT carry a dead key_env reference. The earlier "never write the
// secret value" invariant was deliberately relaxed: the inline form
// makes the profile self-contained and immune to env rotation; the
// 0600-mode file is the trust boundary, matching the storage posture
// of any other credential file on the user's machine.
func TestWriteFile_SecretWrittenInline(t *testing.T) {
	const apiKeyValue = "sk-ant-api-test-on-disk-VVV"
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	spec := SeedSpec{Name: "x", BaseURL: "https://gw/", APIKey: apiKeyValue}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if err := WriteFile(path, file); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	s := string(blob)
	// The credential value IS on disk now — explicit assertion.
	if !strings.Contains(s, apiKeyValue) {
		t.Errorf("written file missing inline key value; got: %s", s)
	}
	// And no dead key_env reference (Key wins; a side-by-side key_env
	// would be confusing noise).
	if strings.Contains(s, `"key_env"`) {
		t.Errorf("written file should not carry key_env when Key is inline; got: %s", s)
	}
	// 0600 permissions — credential file posture.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o; want 0600 (owner-only credential file)", perm)
	}
	var got File
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Errorf("written JSON does not parse: %v", err)
	}
	if got.Default != "x" {
		t.Errorf("Default = %q; want x", got.Default)
	}
	gp := got.Profiles["x"]
	if gp.Auth == nil {
		t.Fatalf("Auth = nil after roundtrip; want non-nil")
	}
	if gp.Auth.Key != apiKeyValue {
		t.Errorf("Auth.Key roundtrip = %q; want %q", gp.Auth.Key, apiKeyValue)
	}
}

// T-write-nil-file — WriteFile must reject nil File.
func TestWriteFile_NilFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := WriteFile(path, nil); err == nil {
		t.Fatalf("WriteFile(nil): want error, got nil")
	}
}

// TestOfficialProfile_Shape — OfficialProfile returns the canonical
// reserved-profile shape: name=official, provider=anthropic, no
// base_url override, Auth=nil, egress=inherit.
func TestOfficialProfile_Shape(t *testing.T) {
	p := OfficialProfile()
	if p.Name != "official" {
		t.Errorf("Name = %q, want official", p.Name)
	}
	if p.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", p.Provider)
	}
	if p.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty (fallback to api.anthropic.com)", p.BaseURL)
	}
	if p.Auth != nil {
		t.Errorf("Auth = %+v, want nil (claude-code OAuth)", p.Auth)
	}
	if p.Egress.Mode != "inherit" {
		t.Errorf("Egress.Mode = %q, want inherit", p.Egress.Mode)
	}
}

// TestOfficialProfileName_Constant — the exported constant matches
// the function's Name field so callers can reference the name
// without hardcoding the string.
func TestOfficialProfileName_Constant(t *testing.T) {
	if OfficialProfileName != "official" {
		t.Errorf("OfficialProfileName = %q, want official", OfficialProfileName)
	}
	if OfficialProfile().Name != OfficialProfileName {
		t.Error("OfficialProfile().Name must equal OfficialProfileName")
	}
}

// TestEnsureOfficialProfile_FreshFile — file doesn't exist → create
// it with {default: official, profiles: {official}}.
func TestEnsureOfficialProfile_FreshFile(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	f, err := Load(DefaultPath(dir))
	if err != nil || f == nil {
		t.Fatalf("load after ensure: %v, %v", err, f)
	}
	if f.Default != OfficialProfileName {
		t.Errorf("Default = %q, want official", f.Default)
	}
	if _, ok := f.Profiles[OfficialProfileName]; !ok {
		t.Error("official entry missing in fresh file")
	}
}

// TestEnsureOfficialProfile_ExistingNoOfficial — file exists with a
// third-party profile only → official added, file.Default UNCHANGED.
func TestEnsureOfficialProfile_ExistingNoOfficial(t *testing.T) {
	dir := t.TempDir()
	path := DefaultPath(dir)
	_ = os.MkdirAll(dir, 0o700)
	initial := &File{
		Default: "third-party",
		Profiles: map[string]Profile{
			"third-party": {
				Name:    "third-party",
				BaseURL: "http://gateway.example.com",
				Auth:    &AuthSpec{Mode: "ccwrap_bearer", Key: "sk-x"},
				Egress:  EgressSpec{Mode: "inherit"},
			},
		},
	}
	if err := OverwriteFile(path, initial, "test-seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	f, _ := Load(path)
	if f.Default != "third-party" {
		t.Errorf("Default = %q, want third-party (user choice preserved)", f.Default)
	}
	if _, ok := f.Profiles[OfficialProfileName]; !ok {
		t.Error("official entry must be added")
	}
	if _, ok := f.Profiles["third-party"]; !ok {
		t.Error("existing third-party entry must be preserved")
	}
}

// TestEnsureOfficialProfile_ExistingWithOfficial — file exists with a
// user-customized official entry → no-op (don't overwrite content).
func TestEnsureOfficialProfile_ExistingWithOfficial(t *testing.T) {
	dir := t.TempDir()
	path := DefaultPath(dir)
	_ = os.MkdirAll(dir, 0o700)
	customized := &File{
		Default: OfficialProfileName,
		Profiles: map[string]Profile{
			OfficialProfileName: {
				Name:     OfficialProfileName,
				Provider: "anthropic",
				BaseURL:  "https://api-staging.anthropic.com", // user customization
				Auth:     nil,
				Egress:   EgressSpec{Mode: "inherit"},
				ModelAliases: map[string]string{
					"sonnet": "claude-sonnet-4-6",
				},
			},
		},
	}
	if err := OverwriteFile(path, customized, "test-seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	f, _ := Load(path)
	p := f.Profiles[OfficialProfileName]
	if p.BaseURL != "https://api-staging.anthropic.com" {
		t.Errorf("BaseURL = %q, want preserved staging URL (no overwrite)", p.BaseURL)
	}
	if _, ok := p.ModelAliases["sonnet"]; !ok {
		t.Error("ModelAliases must be preserved (no overwrite)")
	}
}

// TestEnsureOfficialProfile_MigratesInheritEnvDefault — file has
// default: "inherit-env" → migrated to default: "official" + official
// entry added.
func TestEnsureOfficialProfile_MigratesInheritEnvDefault(t *testing.T) {
	dir := t.TempDir()
	path := DefaultPath(dir)
	_ = os.MkdirAll(dir, 0o700)
	// Direct file write — Default=inherit-env would fail Validate,
	// so we bypass OverwriteFile's validation for the seed step.
	raw := `{"default":"inherit-env","profiles":{}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("raw seed: %v", err)
	}
	if err := EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	f, _ := Load(path)
	if f.Default != OfficialProfileName {
		t.Errorf("Default = %q, want %q (migrated from inherit-env)", f.Default, OfficialProfileName)
	}
	if _, ok := f.Profiles[OfficialProfileName]; !ok {
		t.Error("official entry must be added during migration")
	}
}

// TestEnsureOfficialProfile_Idempotent — calling ensure twice produces
// the same on-disk content (no spurious rewrites).
func TestEnsureOfficialProfile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	stat1, _ := os.Stat(DefaultPath(dir))
	if err := EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	stat2, _ := os.Stat(DefaultPath(dir))
	if !stat1.ModTime().Equal(stat2.ModTime()) {
		t.Errorf("file rewritten on no-op ensure: t1=%v t2=%v", stat1.ModTime(), stat2.ModTime())
	}
}

// TestWriteFile_AtomicNoTempLeak verifies the temp+fsync+link path leaves a
// clean, loadable 0600 file and no orphaned temp file behind on success.
func TestWriteFile_AtomicNoTempLeak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	spec := SeedSpec{Name: "x", BaseURL: "https://gw/", APIKey: "x"}
	file, err := SeedFromEnv(spec)
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if err := WriteFile(path, file); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", info.Mode().Perm())
	}
	if _, err := Load(path); err != nil {
		t.Errorf("written file must Load cleanly: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".profiles.json.") {
			t.Errorf("temp file leaked after successful write: %s", e.Name())
		}
	}
}
