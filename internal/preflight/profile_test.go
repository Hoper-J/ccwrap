package preflight

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestProfilesAuthToPreflight — round-trip schema AuthSpec into preflight
// AuthSpec. nil stays nil ("no auth ownership" propagates through).
func TestProfilesAuthToPreflight(t *testing.T) {
	cases := []struct {
		name string
		in   *profiles.AuthSpec
		want *AuthSpec
	}{
		{"nil-stays-nil", nil, nil},
		{
			"bearer-inline",
			&profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-x"},
			&AuthSpec{Mode: "ccwrap_bearer", Key: "sk-x"},
		},
		{
			"x-api-key-env",
			&profiles.AuthSpec{Mode: "ccwrap_x_api_key", KeyEnv: "X"},
			&AuthSpec{Mode: "ccwrap_x_api_key", KeyEnv: "X"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := profilesAuthToPreflight(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got=%+v want=%+v", got, c.want)
			}
		})
	}
}

// Resolver parity: ResolveProfile with a nil overlay must equal the
// Run() launch path for identical inputs (one resolver shared by both).
func TestResolveProfileNilOverlayEqualsRun(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	parent := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gateway-key"}

	runRes, err := Run(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        parent,
		WorkingDirectory: tmp,
		ChildArgs:        []string{"-p", "hello"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	directRes, err := ResolveProfile(Options{
		Upstream:         "https://gateway.example/v1",
		ParentEnv:        parent,
		WorkingDirectory: tmp,
		ChildArgs:        []string{"-p", "hello"},
		Profile:          nil,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if runRes.APIBaseURL.String() != directRes.APIBaseURL.String() ||
		runRes.RouteClass != directRes.RouteClass ||
		runRes.AuthMode != directRes.AuthMode ||
		runRes.AuthPolicy != directRes.AuthPolicy ||
		runRes.AuthBootstrap != directRes.AuthBootstrap ||
		runRes.Egress != directRes.Egress {
		t.Fatalf("resolver parity broken:\n run=%#v\n direct=%#v", runRes, directRes)
	}
}

// A profile overlay drives base_url + a ccwrap_bearer key from key_env.
func TestResolveProfileAppliesOverlay(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	// The profile auth key_env resolves from the opts.ParentEnv slice the
	// existing auth resolver reads (the copied env slice consumed by the
	// resolver; in production ParentEnv == os.Environ(), so a user-exported
	// token is in the slice) — NOT ambient os.Getenv (that would widen the
	// credential surface and break preflight's pure-input design). Put
	// ACME_TOKEN in ParentEnv; the absent-key_env fail-closed refusal has
	// its own dedicated test.

	res, err := ResolveProfile(Options{
		ParentEnv:        []string{"PATH=/usr/bin", "ACME_TOKEN=acme-secret"},
		WorkingDirectory: tmp,
		Profile: &ProfileInput{
			Name:     "gw-a",
			Provider: "AcmeGW",
			BaseURL:  "https://gw.acme.example/v1",
			Auth:     &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ACME_TOKEN"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.APIBaseURL.String() != "https://gw.acme.example/v1" {
		t.Fatalf("base_url overlay not applied: %q", res.APIBaseURL)
	}
	if res.RouteClass != model.RouteClassThirdPartyHidden {
		t.Fatalf("RouteClass = %q", res.RouteClass)
	}
	if res.AuthMode != model.AuthModeOverrideBearer || res.AuthPolicy != model.AuthPolicyCCWRAPOverrideFailClosed {
		t.Fatalf("auth from profile key_env not applied: mode=%s policy=%s", res.AuthMode, res.AuthPolicy)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderValue != "Bearer acme-secret" {
		t.Fatalf("override auth = %#v", res.OverrideAuth)
	}
}

// Resolver parity through RunWithInspection too: same inputs, same Result.
func TestRunWithInspectionStillEqualsResolveProfile(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	_ = os.MkdirAll(configDir, 0o700)
	opts := Options{ParentEnv: []string{"PATH=/usr/bin"}, WorkingDirectory: tmp}
	a, err := RunWithInspection(opts, nil)
	if err != nil {
		t.Fatalf("RunWithInspection: %v", err)
	}
	b, err := ResolveProfile(opts, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if a.APIBaseURL.String() != b.APIBaseURL.String() || a.AuthPolicy != b.AuthPolicy || a.RouteClass != b.RouteClass {
		t.Fatalf("RunWithInspection must delegate to ResolveProfile: a=%#v b=%#v", a, b)
	}
}

// TestResolveProfileKeyEnvAbsentReturnsMissingResult locks the contract: a
// profile that names a key_env the env doesn't have USED TO refuse launch
// ("absent key_env … refusing to launch"). The fail-closed invariant (no
// un-authed forward) moved to the request hot path in supervisor. Launch
// now SUCCEEDS so inspect/popover/switch tools stay reachable for recovery
// — the Result carries AuthBootstrap=Missing + MissingAuthEnv=<env name> so
// the supervisor can fail-close at forward time AND the ribbon Auth cell can
// render the danger state.
func TestResolveProfileKeyEnvAbsentReturnsMissingResult(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	// ACME_TOKEN deliberately NOT set.
	res, err := ResolveProfile(Options{
		ParentEnv:        []string{"PATH=/usr/bin"},
		WorkingDirectory: tmp,
		Profile: &ProfileInput{
			Name:     "gw-a",
			Provider: "AcmeGW",
			BaseURL:  "https://gw.acme.example/v1",
			Auth:     &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ACME_TOKEN"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("launch must succeed under C1 (request-time fail-closed); err=%v", err)
	}
	if res == nil {
		t.Fatal("Result must be non-nil even with missing auth — request-time gate needs it")
	}
	if res.AuthBootstrap != model.AuthBootstrapMissing {
		t.Errorf("AuthBootstrap = %q, want %q", res.AuthBootstrap, model.AuthBootstrapMissing)
	}
	// Case A: profile named a key_env → MissingAuthEnv carries the env name
	// for downstream UI ("profile X needs $ACME_TOKEN").
	if res.MissingAuthEnv != "ACME_TOKEN" {
		t.Errorf("MissingAuthEnv = %q, want ACME_TOKEN (Case A — profile named the env)", res.MissingAuthEnv)
	}
}

func TestResolveProfileInlineKeyResolves(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := ResolveProfile(Options{
		ParentEnv:        []string{"PATH=/usr/bin"},
		WorkingDirectory: tmp,
		Profile: &ProfileInput{
			Name:     "gw-a",
			Provider: "AcmeGW",
			BaseURL:  "https://gw.acme.example/v1",
			Auth:     &AuthSpec{Mode: "ccwrap_x_api_key", Key: "sk-inline-secret"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("inline auth.key must resolve (local trust), got %v", err)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderName != "X-API-Key" || res.OverrideAuth.HeaderValue != "sk-inline-secret" {
		t.Fatalf("inline key override = %#v", res.OverrideAuth)
	}
}

func contains2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestResolveProfileTPHPassthroughReturnsMissingResult locks the contract: a
// third-party-hidden route with a profile resolving to passthrough USED TO
// refuse launch (the "CCWRAP-owned upstream auth" hidden-auth error). The
// fail-closed invariant moved to the request hot path. Launch now succeeds;
// Result carries AuthBootstrap=Missing + MissingAuthEnv="" (Case B — profile
// didn't name a specific env). The no-partial-apply invariant on Profile +
// ParentEnv is preserved.
func TestResolveProfileTPHPassthroughReturnsMissingResult(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))

	parent := []string{"PATH=/usr/bin"}
	parentCopy := append([]string(nil), parent...)
	overlay := &ProfileInput{
		Name:     "gw-bad",
		Provider: "AcmeGW",
		BaseURL:  "https://gw.acme.example/v1",
		Auth:     &AuthSpec{Mode: "passthrough"}, // third-party + passthrough => Case B
	}
	overlayCopy := *overlay

	res, err := ResolveProfile(Options{
		ParentEnv:        parent,
		WorkingDirectory: tmp,
		Profile:          overlay,
	}, nil)

	if err != nil {
		t.Fatalf("launch must succeed under C1 (request-time fail-closed); err=%v", err)
	}
	if res == nil {
		t.Fatal("Result must be non-nil — supervisor needs it to start the session")
	}
	if res.AuthBootstrap != model.AuthBootstrapMissing {
		t.Errorf("AuthBootstrap = %q, want %q", res.AuthBootstrap, model.AuthBootstrapMissing)
	}
	// Case B: profile didn't name a key_env, so MissingAuthEnv is empty.
	// UI branches on emptiness to show "no auth source configured" vs
	// "needs $X".
	if res.MissingAuthEnv != "" {
		t.Errorf("MissingAuthEnv = %q, want empty (Case B — profile named no env)", res.MissingAuthEnv)
	}
	// No-partial-apply invariant: ParentEnv slice + Profile overlay must be
	// byte-identical to inputs. The launch-time-relax change doesn't touch
	// this contract — it just removes the early error.
	if len(parent) != len(parentCopy) {
		t.Fatalf("ParentEnv slice length mutated: %v vs %v", parent, parentCopy)
	}
	for i := range parent {
		if parent[i] != parentCopy[i] {
			t.Fatalf("ParentEnv element mutated at %d: %q vs %q", i, parent[i], parentCopy[i])
		}
	}
	if !reflect.DeepEqual(*overlay, overlayCopy) {
		t.Fatalf("Profile overlay mutated: %#v vs %#v", *overlay, overlayCopy)
	}
}

func TestResolveProfileInvalidBaseURLNoResult(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	res, err := ResolveProfile(Options{
		ParentEnv:        []string{"PATH=/usr/bin"},
		WorkingDirectory: tmp,
		Profile:          &ProfileInput{Name: "bad", BaseURL: "://not a url"},
	}, nil)
	if err == nil || res != nil {
		t.Fatalf("invalid base_url must error with nil Result; got res=%#v err=%v", res, err)
	}
}

func mkResult(policy model.AuthPolicy, bootstrap model.AuthBootstrap, routeClass model.RouteClass) *Result {
	return &Result{AuthPolicy: policy, AuthBootstrap: bootstrap, RouteClass: routeClass}
}

func TestClassifyTransitionMatrix(t *testing.T) {
	type tc struct {
		name    string
		current *Result
		cand    *Result
		want    model.RelaunchClass
	}
	cases := []tc{
		{"nil current (first launch) is live", nil, mkResult(model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, model.RouteClassThirdPartyHidden), model.RelaunchLive},
		{"gateway<->gateway (placeholder->placeholder) is live", mkResult(model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, model.RouteClassThirdPartyHidden), mkResult(model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, model.RouteClassThirdPartyHidden), model.RelaunchLive},
		{"hidden/CCWRAP-owned -> first-party passthrough is needs_relaunch", mkResult(model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, model.RouteClassThirdPartyHidden), mkResult(model.AuthPolicyFirstPartyPassthrough, model.AuthBootstrapNotNeeded, model.RouteClassFirstParty), model.RelaunchNeedsRelaunch},
		{"first-party -> gateway is live", mkResult(model.AuthPolicyFirstPartyPassthrough, model.AuthBootstrapNotNeeded, model.RouteClassFirstParty), mkResult(model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, model.RouteClassThirdPartyHidden), model.RelaunchLive},
		{"key/model/egress swap within CCWRAP-owned is live", mkResult(model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive, model.RouteClassThirdPartyHidden), mkResult(model.AuthPolicyCCWRAPOverride, model.AuthBootstrapNotNeeded, model.RouteClassThirdPartyHidden), model.RelaunchLive},
	}
	for _, c := range cases {
		if got := ClassifyTransition(c.current, c.cand); got != c.want {
			t.Fatalf("%s: ClassifyTransition = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResultProfileViewNonSecret(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
	t.Setenv("ACME_TOKEN", "acme-secret-value")
	res, err := ResolveProfile(Options{
		ParentEnv:        []string{"PATH=/usr/bin", "ACME_TOKEN=acme-secret-value"},
		WorkingDirectory: tmp,
		Profile: &ProfileInput{
			Name:         "gw-a",
			Provider:     "AcmeGW",
			BaseURL:      "https://gw.acme.example:8443/v1",
			Auth:         &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ACME_TOKEN"},
			ModelAliases: map[string]string{"claude-sonnet-4-5": "gpt-5.5"},
			EgressMode:   "http",
			EgressURL:    "http://127.0.0.1:10800",
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	v := res.ProfileView()
	if v.Name != "gw-a" || v.ProviderLabel != "gw-a (AcmeGW)" {
		t.Fatalf("ProfileView identity: %#v", v)
	}
	if v.BaseURLHost != "gw.acme.example:8443" {
		t.Fatalf("ProfileView.BaseURLHost = %q", v.BaseURLHost)
	}
	if v.ModelAliasCount != 1 {
		t.Fatalf("ProfileView.ModelAliasCount = %d", v.ModelAliasCount)
	}
	if v.EgressSummary == "" || v.AuthPolicy != model.AuthPolicyCCWRAPOverrideFailClosed {
		t.Fatalf("ProfileView fields: %#v", v)
	}
	if v.RelaunchClass != model.RelaunchLive {
		t.Fatalf("ProfileView.RelaunchClass (vs nil current) = %q, want live", v.RelaunchClass)
	}
	blob := v.Name + "|" + v.ProviderLabel + "|" + v.BaseURLHost + "|" + v.EgressSummary + "|" + string(v.AuthPolicy) + "|" + string(v.RelaunchClass)
	if contains2(blob, "acme-secret-value") || contains2(blob, "ACME_TOKEN") {
		t.Fatalf("ProfileView leaked a credential: %q", blob)
	}
}

// Env-only-launch + profile-switch: profile-tier MUST win on conflicts.
func TestResolveProfile_EnvOnlyLaunch_ProfileWinsOnSwitch(t *testing.T) {
	// Simulate launch: env had CCWRAP_MODEL_ALIASES_JSON='{"foo":"bar"}', no CLI explicit, no profile.
	// Use a third-party upstream so model aliases are valid (aliases require third-party route).
	launchOpts := Options{
		Upstream: "https://gateway.example/v1",
		ParentEnv: []string{
			"CCWRAP_MODEL_ALIASES_JSON={\"foo\":\"bar\"}",
			"ANTHROPIC_API_KEY=gw-key",
		},
		AllowProviderModelPassthrough: true,
	}
	launchPre, err := ResolveProfile(launchOpts, nil)
	if err != nil {
		t.Fatalf("launch ResolveProfile err: %v", err)
	}
	if got := launchPre.ModelAlias.Forward["foo"]; got != "bar" {
		t.Fatalf("launch Forward[foo] = %q, want bar", got)
	}

	// Switch: same Options + same Inspection (nil), Profile is now set to a profile with model_aliases:{"foo":"baz"}.
	switchOpts := launchOpts
	switchOpts.Profile = &ProfileInput{
		Name:         "switched",
		Provider:     "p",
		ModelAliases: map[string]string{"foo": "baz"},
	}
	switchPre, err := ResolveProfile(switchOpts, nil)
	if err != nil {
		t.Fatalf("switch ResolveProfile err: %v", err)
	}
	// Precedence: profile > env when no explicit CLI input — profile must win.
	if got := switchPre.ModelAlias.Forward["foo"]; got != "baz" {
		t.Errorf("TIER-COLLAPSE REGRESSION: switch Forward[foo] = %q, want baz (profile-tier should dominate env-tier)", got)
	}
}

// Explicit-CLI-launch + profile-switch — explicit must dominate the switched profile.
func TestResolveProfile_ExplicitCLILaunch_ExplicitDominatesProfileOnSwitch(t *testing.T) {
	dir := t.TempDir()
	cliFile := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(cliFile, []byte(`{"foo":"cliv"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	opts := Options{
		Upstream:                      "https://gateway.example/v1",
		ParentEnv:                     []string{"ANTHROPIC_API_KEY=gw-key"},
		ModelAliasFile:                cliFile,
		AllowProviderModelPassthrough: true,
	}
	launchPre, err := ResolveProfile(opts, nil)
	if err != nil {
		t.Fatalf("launch err: %v", err)
	}
	if got := launchPre.ModelAlias.Forward["foo"]; got != "cliv" {
		t.Fatalf("launch Forward[foo] = %q, want cliv", got)
	}

	switchOpts := opts
	switchOpts.Profile = &ProfileInput{
		Name:         "switched",
		Provider:     "p",
		ModelAliases: map[string]string{"foo": "pv"},
	}
	switchPre, err := ResolveProfile(switchOpts, nil)
	if err != nil {
		t.Fatalf("switch err: %v", err)
	}
	if got := switchPre.ModelAlias.Forward["foo"]; got != "cliv" {
		t.Errorf("explicit CLI must dominate switched profile; Forward[foo] = %q, want cliv", got)
	}
}

// Byte-faithful for CLI file content snapshot — file deleted mid-session does not change switch.
func TestResolveProfile_ExplicitFileContentSnapshot_ByteFaithfulAcrossDeletion(t *testing.T) {
	dir := t.TempDir()
	cliFile := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(cliFile, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	// Simulate launch: read file, snapshot content into Options.
	snapshot, err := os.ReadFile(cliFile)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	opts := Options{
		Upstream:                      "https://gateway.example/v1",
		ParentEnv:                     []string{"ANTHROPIC_API_KEY=gw-key"},
		ModelAliasFile:                cliFile,
		ModelAliasExplicitFileContent: snapshot,
		AllowProviderModelPassthrough: true,
	}
	// Delete file mid-session.
	if err := os.Remove(cliFile); err != nil {
		t.Fatalf("delete seed: %v", err)
	}
	// Switch resolution after deletion: MUST use snapshot.
	pre, err := ResolveProfile(opts, nil)
	if err != nil {
		t.Fatalf("post-deletion ResolveProfile err: %v", err)
	}
	if pre.ModelAlias.Forward["k"] != "v" {
		t.Errorf("Forward[k] = %q, want v (snapshot must survive file deletion)", pre.ModelAlias.Forward["k"])
	}
}

func TestZeroTouchInvariantResultByteIdentical(t *testing.T) {
	scenarios := []struct {
		name      string
		upstream  string
		parentEnv []string
		childArgs []string
	}{
		{"first-party fallback default", "", []string{"PATH=/usr/bin"}, nil},
		{"first-party explicit api key", "", []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=fp-key"}, []string{"-p", "hi"}},
		{"third-party hidden via env key", "https://gateway.example/v1", []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=gw-key"}, []string{"-p", "hi"}},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			tmp := t.TempDir()
			configDir := filepath.Join(tmp, "config")
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", filepath.Join(tmp, "managed"))
			opts := Options{
				Upstream:         s.upstream,
				ParentEnv:        s.parentEnv,
				WorkingDirectory: tmp,
				ChildArgs:        s.childArgs,
			}

			baseline, err := RunWithInspection(opts, nil)
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}
			zero, err := ResolveProfile(opts, nil)
			if err != nil {
				t.Fatalf("zero-touch: %v", err)
			}

			if zero.ActiveProfileName != "" || zero.ActiveProfileProvider != "" {
				t.Fatalf("zero-touch must not set active-profile identity: name=%q provider=%q", zero.ActiveProfileName, zero.ActiveProfileProvider)
			}
			if !reflect.DeepEqual(baseline, zero) {
				t.Fatalf("zero-touch Result must be byte-identical to pre-feature path\nbaseline=%#v\nzero=%#v", baseline, zero)
			}
		})
	}
}

// TestApplyProfileOverlay_AuthNil_NoInjection pins the contract that a
// profile with Auth=nil ("ccwrap does not own auth") yields no auth env
// injection from the overlay and signals no Case-A missing env: the outer
// "if p.Auth != nil" guard must skip the auth-injection branch cleanly.
func TestApplyProfileOverlay_AuthNil_NoInjection(t *testing.T) {
	opts := Options{
		Profile: &ProfileInput{
			Name:    "noauth",
			BaseURL: "http://x.test",
			Auth:    nil,
		},
		ParentEnv: []string{"PATH=/usr/bin"},
	}
	_, gotEnv, _, _, _, _, _, missingEnv, _, err := applyProfileOverlay(opts)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	for _, e := range gotEnv {
		if strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=") ||
			strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			t.Errorf("overlay must not inject auth env when Auth=nil; got %q", e)
		}
	}
	if missingEnv != "" {
		t.Errorf("missingAuthEnv = %q, want empty", missingEnv)
	}
}

// TestApplyProfileOverlay_BearerInline_Injects pins that
// Mode=ccwrap_bearer with an inline Key injects ANTHROPIC_AUTH_TOKEN into
// the resolved env when ParentEnv has no explicit auth keys (the env
// auth resolver downstream then consumes it through its normal path).
func TestApplyProfileOverlay_BearerInline_Injects(t *testing.T) {
	opts := Options{
		Profile: &ProfileInput{
			Name:    "p",
			BaseURL: "http://x.test",
			Auth:    &AuthSpec{Mode: "ccwrap_bearer", Key: "sk-inline"},
		},
		ParentEnv: []string{"PATH=/usr/bin"},
	}
	_, gotEnv, _, _, _, _, _, _, _, err := applyProfileOverlay(opts)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	found := false
	for _, e := range gotEnv {
		if e == "ANTHROPIC_AUTH_TOKEN=sk-inline" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ANTHROPIC_AUTH_TOKEN=sk-inline in env; got %v", gotEnv)
	}
}

// --- Profile-auth-is-authoritative (UI/UX review follow-up) -------------
// A profile that DECLARES auth (auth != nil) owns the upstream credential.
// Ambient first-party env auth (ANTHROPIC_*/CLAUDE_*) and CCWRAP_UPSTREAM_*
// must NOT preempt it — inherit-env (nil profile) and auth:null are the two
// explicit ways to say "use my environment". These pin the fix for the
// live-confirmed bug where ANTHROPIC_AUTH_TOKEN silently overrode a
// switched gateway profile's key (and could ship a first-party token to a
// third party).

func TestResolveProfile_ProfileAuthBeatsAmbientBearer(t *testing.T) {
	res, err := ResolveProfile(Options{
		ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_AUTH_TOKEN=ambient-token"},
		Profile: &ProfileInput{
			Name: "gw", Provider: "p", BaseURL: "https://gateway.example/v1",
			Auth: &AuthSpec{Mode: "ccwrap_bearer", Key: "profile-key"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderValue != "Bearer profile-key" {
		t.Fatalf("profile must own auth; override = %#v, want Bearer profile-key (NOT ambient-token)", res.OverrideAuth)
	}
}

func TestResolveProfile_ProfileAuthBeatsAmbientApiKey(t *testing.T) {
	res, err := ResolveProfile(Options{
		ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=ambient-key"},
		Profile: &ProfileInput{
			Name: "gw", Provider: "p", BaseURL: "https://gateway.example/v1",
			Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "profile-key"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderValue != "profile-key" || res.OverrideAuth.HeaderName != "X-API-Key" {
		t.Fatalf("profile x-api-key must win; override = %#v", res.OverrideAuth)
	}
}

func TestResolveProfile_ProfileAuthBeatsCCWRAPUpstream(t *testing.T) {
	res, err := ResolveProfile(Options{
		ParentEnv: []string{"PATH=/usr/bin", "CCWRAP_UPSTREAM_AUTH_TOKEN=cc-token"},
		Profile: &ProfileInput{
			Name: "gw", Provider: "p", BaseURL: "https://gateway.example/v1",
			Auth: &AuthSpec{Mode: "ccwrap_bearer", Key: "profile-key"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderValue != "Bearer profile-key" {
		t.Fatalf("profile must win over CCWRAP_UPSTREAM_* (uniform rule); override = %#v", res.OverrideAuth)
	}
}

func TestResolveProfile_ProfileAuthKeyEnv_BeatsAmbient(t *testing.T) {
	// key_env names a SPECIFIC var (the profile explicitly chose it) — it
	// must resolve from that var, not from an ambient ANTHROPIC_* one.
	res, err := ResolveProfile(Options{
		ParentEnv: []string{"GATEWAY_KEY=gw-secret", "ANTHROPIC_AUTH_TOKEN=ambient-token"},
		Profile: &ProfileInput{
			Name: "gw", Provider: "p", BaseURL: "https://gateway.example/v1",
			Auth: &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "GATEWAY_KEY"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderValue != "Bearer gw-secret" {
		t.Fatalf("key_env must resolve its named var, not ambient; override = %#v", res.OverrideAuth)
	}
}

func TestResolveProfile_ProfileAuthMissingKeyEnv_FailsClosedNoAmbientLeak(t *testing.T) {
	// Profile owns auth via key_env, but the var is unset; an ambient
	// ANTHROPIC_AUTH_TOKEN is present. Must fail closed (Missing) and must
	// NOT leak the ambient first-party token to the third-party gateway.
	res, err := ResolveProfile(Options{
		ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_AUTH_TOKEN=ambient-token"},
		Profile: &ProfileInput{
			Name: "gw", Provider: "p", BaseURL: "https://gateway.example/v1",
			Auth: &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "GATEWAY_KEY"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.AuthBootstrap != model.AuthBootstrapMissing || res.MissingAuthEnv != "GATEWAY_KEY" {
		t.Fatalf("must fail closed naming the env; bootstrap=%s missing=%q", res.AuthBootstrap, res.MissingAuthEnv)
	}
	if res.OverrideAuth != nil && res.OverrideAuth.HeaderValue == "Bearer ambient-token" {
		t.Fatalf("LEAK: ambient first-party token must not become the third-party credential; override=%#v", res.OverrideAuth)
	}
}

func TestResolveProfile_AuthNil_EnvStillSuppliesAuth(t *testing.T) {
	// auth:null = "ccwrap does not own auth for me" → env DOES supply it
	// (the explicit per-profile escape hatch). Must remain unchanged.
	res, err := ResolveProfile(Options{
		ParentEnv: []string{"PATH=/usr/bin", "ANTHROPIC_AUTH_TOKEN=env-token"},
		Profile: &ProfileInput{
			Name: "gw", Provider: "p", BaseURL: "https://gateway.example/v1",
			Auth: nil,
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if res.OverrideAuth == nil || res.OverrideAuth.HeaderValue != "Bearer env-token" {
		t.Fatalf("auth:null must let env supply auth; override = %#v", res.OverrideAuth)
	}
}
