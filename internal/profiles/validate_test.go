package profiles

import (
	"errors"
	"strings"
	"testing"
)

func TestValidationError_Error_WithGot(t *testing.T) {
	e := ValidationError{Path: "profiles.glm.auth.mode", Want: "one of: ccwrap_bearer, ccwrap_x_api_key", Got: "ccwrapKey"}
	want := `profiles.glm.auth.mode: want one of: ccwrap_bearer, ccwrap_x_api_key, got "ccwrapKey"`
	if got := e.Error(); got != want {
		t.Errorf("Error()\n got: %q\nwant: %q", got, want)
	}
}

func TestValidationError_Error_NoGot(t *testing.T) {
	e := ValidationError{Path: "profiles.kimi.auth", Want: "key or key_env required when mode=ccwrap_bearer"}
	want := "profiles.kimi.auth: key or key_env required when mode=ccwrap_bearer"
	if got := e.Error(); got != want {
		t.Errorf("Error()\n got: %q\nwant: %q", got, want)
	}
}

func TestParseErrors_Error_MultiLine(t *testing.T) {
	perr := &ParseErrors{
		Source: "test.json",
		Items: []ValidationError{
			{Path: "default", Want: `one of: glm, "inherit-env"`, Got: "nope"},
			{Path: "profiles.glm.base_url", Want: "URL with http or https scheme and host"},
		},
	}
	got := perr.Error()
	if !strings.HasPrefix(got, "test.json invalid: 2 errors\n") {
		t.Errorf("missing header line; got:\n%s", got)
	}
	if !strings.Contains(got, "  - default: want") {
		t.Errorf("missing item 1 indent; got:\n%s", got)
	}
	if !strings.Contains(got, "  - profiles.glm.base_url: URL") {
		t.Errorf("missing item 2 indent; got:\n%s", got)
	}
}

func TestParseErrors_Is_WithItems_MatchesSentinel(t *testing.T) {
	perr := &ParseErrors{Source: "x", Items: []ValidationError{{Path: "p"}}}
	if !errors.Is(perr, ErrValidationFailed) {
		t.Errorf("errors.Is should match ErrValidationFailed when Items non-empty")
	}
}

func TestParseErrors_Is_ZeroItems_DoesNotMatchSentinel(t *testing.T) {
	perr := &ParseErrors{Source: "x"}
	if errors.Is(perr, ErrValidationFailed) {
		t.Errorf("errors.Is must NOT match when Items empty (defensive D5)")
	}
}

func TestParseErrors_Is_OtherSentinel_DoesNotMatch(t *testing.T) {
	perr := &ParseErrors{Source: "x", Items: []ValidationError{{Path: "p"}}}
	other := errors.New("unrelated")
	if errors.Is(perr, other) {
		t.Errorf("errors.Is must NOT match unrelated sentinels")
	}
}

func TestScrubURL_ValidWithUserinfo(t *testing.T) {
	got := scrubURL("https://user:secret@host.example/path")
	if strings.Contains(got, "secret") || strings.Contains(got, "user:") {
		t.Errorf("scrub failed; got %q", got)
	}
	if !strings.Contains(got, "host.example") {
		t.Errorf("scrub dropped host; got %q", got)
	}
}

func TestScrubURL_NoUserinfo_Unchanged(t *testing.T) {
	in := "https://host.example/path"
	if got := scrubURL(in); got != in {
		t.Errorf("scrub mangled non-userinfo URL; got %q want %q", got, in)
	}
}

func TestScrubURL_Empty(t *testing.T) {
	if got := scrubURL(""); got != "" {
		t.Errorf("empty input → empty output; got %q", got)
	}
}

func TestScrubURL_Malformed(t *testing.T) {
	// url.Parse returns an error for inputs like ":::" → helper must NOT
	// nil-deref AND must NOT leak the raw value (which could embed secrets).
	got := scrubURL("::: invalid")
	if got == "::: invalid" {
		t.Errorf("malformed URL must NOT pass through raw (Codex iter-3 (a)); got %q", got)
	}
	if got != "<malformed>" {
		t.Errorf("malformed sentinel; got %q want %q", got, "<malformed>")
	}
}

func TestScrubURL_MalformedWithUserinfo(t *testing.T) {
	// http://user:secret@%zz parses with error in stdlib → helper must
	// return "<malformed>" so the raw secret-bearing string never leaks.
	got := scrubURL("http://user:secret@%zz")
	if strings.Contains(got, "secret") {
		t.Errorf("malformed-with-userinfo leaked secret; got %q", got)
	}
}

func TestValidate_Stub_CleanFile_ReturnsNil(t *testing.T) {
	// validate() with a nil/empty file should return nil (no rules fire
	// yet — stub baseline).
	if err := Validate(&File{Profiles: map[string]Profile{}}, "test"); err != nil {
		t.Errorf("stub validate on empty file should return nil; got %v", err)
	}
}

func TestR1_DefaultKnown_NoError(t *testing.T) {
	f := &File{
		Default: "glm",
		Profiles: map[string]Profile{
			"glm": {Name: "glm", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
		},
	}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if perr != nil {
			for _, item := range perr.Items {
				if strings.HasPrefix(item.Path, "default") {
					t.Errorf("R1 should pass for known default; got: %v", item)
				}
			}
		}
	}
}

func TestR1_DefaultInheritEnv_NoError(t *testing.T) {
	// Default=inherit-env is accepted by Validate — the migration to
	// "official" is enforced by EnsureOfficialProfile at launch, not at
	// the schema level, to keep migration paths working.
	f := &File{
		Default: "inherit-env",
		Profiles: map[string]Profile{
			"glm": {Name: "glm", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		return // clean is fine
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	for _, item := range perr.Items {
		if item.Path == "default" {
			t.Errorf("R1 should accept \"inherit-env\"; got: %v", item)
		}
	}
}

func TestR1_DefaultUnknown_Fails(t *testing.T) {
	f := &File{
		Default: "nope",
		Profiles: map[string]Profile{
			"glm": {Name: "glm", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("R1 should reject unknown default")
	}
	var perr *ParseErrors
	if !errors.As(err, &perr) {
		t.Fatalf("err not *ParseErrors: %v", err)
	}
	found := false
	for _, item := range perr.Items {
		if item.Path == "default" && item.Got == "nope" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("R1 default error missing; items=%+v", perr.Items)
	}
}

func TestR2_NameSentinelConflict_Fails(t *testing.T) {
	f := &File{
		Default: "inherit-env",
		Profiles: map[string]Profile{
			"inherit-env": {Name: "inherit-env", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("R2 should reject name == inherit-env")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	found := false
	for _, item := range perr.Items {
		if item.Path == "profiles.inherit-env" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("R2 name conflict error missing; items=%+v", perr.Items)
	}
}

// TestR2_NameSentinelConflict_RejectsAngleBracketSynthetics — the
// egress-probe surface reserves "<active-session>" and "<draft>" as
// synthetic names handled before the disk lookup runs. A profile saved
// under either name would be invisible to /profile/test-egress (the
// synthetic handler always wins), silently shadowing the user's entry.
// validateProfileName must reject both so the conflict surfaces at
// save time, not at debug time.
func TestR2_NameSentinelConflict_RejectsAngleBracketSynthetics(t *testing.T) {
	for _, sentinel := range []string{"<active-session>", "<draft>"} {
		t.Run(sentinel, func(t *testing.T) {
			f := &File{
				Default: sentinel,
				Profiles: map[string]Profile{
					sentinel: {Name: sentinel, BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
				},
			}
			err := Validate(f, "test")
			if err == nil {
				t.Fatalf("name == %q must be rejected; would shadow synthetic handler", sentinel)
			}
			var perr *ParseErrors
			errors.As(err, &perr)
			found := false
			for _, item := range perr.Items {
				if item.Path == "profiles."+sentinel {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected validation error at profiles.%s; items=%+v", sentinel, perr.Items)
			}
		})
	}
}

// TestR3_BaseURL_Empty_OK — empty BaseURL is valid. Means "profile
// does not override base_url; preflight falls back to env or
// api.anthropic.com." The official profile uses this shape.
func TestR3_BaseURL_Empty_OK(t *testing.T) {
	f := &File{Default: "glm", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	if err := Validate(f, "test"); err != nil {
		t.Errorf("empty base_url should validate post-Slice #2: %v", err)
	}
}

func TestR3_BaseURL_Malformed_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: ":::", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("malformed base_url should fail")
	}
}

func TestR3_BaseURL_MissingScheme_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "api.anthropic.com/v1", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("missing scheme should fail R3")
	}
}

func TestR3_BaseURL_MissingHost_Fails(t *testing.T) {
	// url.Parse("https://") succeeds with empty Host — must reject.
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "https://", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("missing host should fail R3 (v4)")
	}
}

func TestR3_BaseURL_BadScheme_UserinfoScrubbed(t *testing.T) {
	// Pick a rule-violating URL that ALSO contains userinfo. ftp scheme
	// fails R3, exercising the Got-scrub path.
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "ftp://user:secret@host", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("ftp scheme should fail R3")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	for _, item := range perr.Items {
		if item.Path == "profiles.glm.base_url" {
			if strings.Contains(item.Got, "secret") {
				t.Errorf("Got leaked userinfo secret; Got=%q", item.Got)
			}
		}
	}
}

func TestR3_BaseURL_MalformedUserinfo_NoLeak(t *testing.T) {
	// url.Parse fails on this → scrubURL returns "<malformed>". Got
	// must NOT contain the raw secret.
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "http://user:secret@%zz", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("malformed URL should fail R3")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	for _, item := range perr.Items {
		if item.Path == "profiles.glm.base_url" {
			if strings.Contains(item.Got, "secret") {
				t.Errorf("Got leaked secret on malformed URL; Got=%q", item.Got)
			}
		}
	}
}

func TestR3_BaseURL_HTTPS_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "https://api.example.com", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.glm.base_url") {
			t.Errorf("valid HTTPS base_url incorrectly rejected; items=%+v", perr.Items)
		}
	}
}

// pathHasErr — test helper used by R3+ tests.
func pathHasErr(perr *ParseErrors, path string) bool {
	if perr == nil {
		return false
	}
	for _, item := range perr.Items {
		if item.Path == path {
			return true
		}
	}
	return false
}

func TestR4_AuthMode_Bad_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"glm": {Name: "glm", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrapKey", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("bad auth.mode should fail")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	if !pathHasErr(perr, "profiles.glm.auth.mode") {
		t.Errorf("missing R4 error; items=%+v", perr.Items)
	}
}

func TestR4_AuthMode_Uppercase_OK(t *testing.T) {
	// Case-insensitive — "CCWRAP_BEARER" passes validation (runtime
	// applyProfileOverlay normalizes the same way).
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "CCWRAP_BEARER", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.auth.mode") {
			t.Errorf("D10: uppercase mode should pass R4; items=%+v", perr.Items)
		}
	}
}

func TestR4_AuthMode_Whitespace_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: " ccwrap_x_api_key ", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.auth.mode") {
			t.Errorf("D10: whitespace-padded mode should pass R4; items=%+v", perr.Items)
		}
	}
}

// Passthrough is no longer a valid mode regardless of key state. See
// TestValidate_RejectsPassthrough for rejection coverage; "absent auth
// block" (the replacement for old-passthrough) is covered by
// TestValidate_AcceptsAbsentAuth.

func TestR6_CCWRAPMode_NoKey_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("ccwrap_x_api_key without key/key_env should fail R6")
	}
}

func TestR6_CCWRAPMode_KeyOnly_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_bearer", Key: "tok"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.auth") {
			t.Errorf("ccwrap_bearer + key rejected; items=%+v", perr.Items)
		}
	}
}

func TestR6_CCWRAPMode_KeyEnvOnly_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", KeyEnv: "X"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.auth") {
			t.Errorf("ccwrap_x_api_key + KeyEnv rejected; items=%+v", perr.Items)
		}
	}
}

// Key + KeyEnv are mutually exclusive, so the "both set is accepted"
// assertion no longer holds. See TestValidate_V3_RejectsBothKeySources
// for the contract.

// TestValidate_RejectsPassthrough — old mode=passthrough data is
// hard-rejected with a message naming the fix.
func TestValidate_RejectsPassthrough(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth:    &AuthSpec{Mode: "passthrough"},
				Egress:  EgressSpec{Mode: "inherit"},
			},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("Validate must reject mode=passthrough")
	}
	if !strings.Contains(err.Error(), "passthrough") {
		t.Errorf("error must mention 'passthrough': %v", err)
	}
	if !strings.Contains(err.Error(), "remove the auth block") {
		t.Errorf("error must name the fix 'remove the auth block': %v", err)
	}
}

// TestValidate_RejectsUnknownMode — invalid mode values list the
// allowed set.
func TestValidate_RejectsUnknownMode(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth:    &AuthSpec{Mode: "garbage", Key: "k"},
				Egress:  EgressSpec{Mode: "inherit"},
			},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("Validate must reject mode=garbage")
	}
	if !strings.Contains(err.Error(), "ccwrap_bearer") || !strings.Contains(err.Error(), "ccwrap_x_api_key") {
		t.Errorf("error must list allowed modes: %v", err)
	}
}

// TestValidate_AcceptsValidModes — both valid modes pass.
func TestValidate_AcceptsValidModes(t *testing.T) {
	for _, mode := range []string{"ccwrap_bearer", "ccwrap_x_api_key"} {
		f := &File{
			Default: "p",
			Profiles: map[string]Profile{
				"p": {
					Name:    "p",
					BaseURL: "https://api.anthropic.com",
					Auth:    &AuthSpec{Mode: mode, Key: "k"},
					Egress:  EgressSpec{Mode: "inherit"},
				},
			},
		}
		if err := Validate(f, "test"); err != nil {
			t.Errorf("mode=%q: Validate failed: %v", mode, err)
		}
	}
}

// TestValidate_AcceptsAbsentAuth — Auth nil is valid (covers
// passthrough's replacement: no auth block).
func TestValidate_AcceptsAbsentAuth(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {Name: "p", BaseURL: "https://api.anthropic.com", Egress: EgressSpec{Mode: "inherit"}},
		},
	}
	if err := Validate(f, "test"); err != nil {
		t.Errorf("Auth=nil must validate: %v", err)
	}
}

// TestValidate_V2_RequiresKeyOrKeyEnv — auth block with no key source is rejected.
func TestValidate_V2_RequiresKeyOrKeyEnv(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth:    &AuthSpec{Mode: "ccwrap_bearer"}, // no Key, no KeyEnv
				Egress:  EgressSpec{Mode: "inherit"},
			},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("Validate must reject auth block with no key source")
	}
	msg := err.Error()
	if !strings.Contains(msg, "key") && !strings.Contains(msg, "key_env") {
		t.Errorf("error must mention key/key_env: %v", err)
	}
}

// TestValidate_V3_RejectsBothKeySources — auth.key AND auth.key_env are mutex.
func TestValidate_V3_RejectsBothKeySources(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth: &AuthSpec{
					Mode:   "ccwrap_bearer",
					Key:    "sk-inline",
					KeyEnv: "ANTHROPIC_AUTH_TOKEN",
				},
				Egress: EgressSpec{Mode: "inherit"},
			},
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("Validate must reject auth.key + auth.key_env together")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error must say 'mutually exclusive': %v", err)
	}
}

// TestValidate_V2V3_AcceptsKeyOnly — only Key set: valid.
func TestValidate_V2V3_AcceptsKeyOnly(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth:    &AuthSpec{Mode: "ccwrap_bearer", Key: "sk-inline"},
				Egress:  EgressSpec{Mode: "inherit"},
			},
		},
	}
	if err := Validate(f, "test"); err != nil {
		t.Errorf("Key-only must validate: %v", err)
	}
}

// TestValidate_V2V3_AcceptsKeyEnvOnly — only KeyEnv set: valid.
func TestValidate_V2V3_AcceptsKeyEnvOnly(t *testing.T) {
	f := &File{
		Default: "p",
		Profiles: map[string]Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth:    &AuthSpec{Mode: "ccwrap_bearer", KeyEnv: "ANTHROPIC_AUTH_TOKEN"},
				Egress:  EgressSpec{Mode: "inherit"},
			},
		},
	}
	if err := Validate(f, "test"); err != nil {
		t.Errorf("KeyEnv-only must validate: %v", err)
	}
}

func TestR7_EgressMode_Bad_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "weird_mode"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("egress.mode=weird_mode should fail R7")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	if !pathHasErr(perr, "profiles.x.egress.mode") {
		t.Errorf("missing R7 error; items=%+v", perr.Items)
	}
}

func TestR8_EgressSOCKS5_WithURL_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "socks5", URL: "socks5://proxy:1080"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		t.Errorf("valid socks5 egress rejected; items=%+v", perr.Items)
	}
}

func TestR8_EgressSOCKS5H_WithURL_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "socks5h", URL: "socks5h://proxy:1080"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		t.Errorf("valid socks5h egress rejected; items=%+v", perr.Items)
	}
}

func TestR8_EgressSOCKS5_NoURL_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "socks5"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("egress.mode=socks5 with empty url should fail")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	if !pathHasErr(perr, "profiles.x.egress.url") {
		t.Errorf("missing socks5 no-url error; items=%+v", perr.Items)
	}
}

func TestR8_EgressSchemeMismatch_Fails(t *testing.T) {
	// mode=socks5 + url=http://... should be rejected
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "socks5", URL: "http://proxy:8080"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("mode=socks5 + url scheme=http should fail scheme-match")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	if !pathHasErr(perr, "profiles.x.egress.url") {
		t.Errorf("missing scheme-mismatch error; items=%+v", perr.Items)
	}
}

func TestR8_EgressHTTPS_OK_under_http_mode(t *testing.T) {
	// http mode accepts http:// AND https://
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "http", URL: "https://proxy:8443"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		t.Errorf("https url under http mode should pass; items=%+v", perr.Items)
	}
}

func TestR7_EgressMode_Uppercase_OK(t *testing.T) {
	// Case-insensitive
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "HTTP", URL: "http://proxy:8080"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.egress.mode") {
			t.Errorf("D10: HTTP uppercase should pass R7; items=%+v", perr.Items)
		}
	}
}

func TestR7_EgressMode_Empty_OK(t *testing.T) {
	// Empty egress.mode treated semantically as inherit
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.egress.mode") {
			t.Errorf("empty egress.mode rejected; items=%+v", perr.Items)
		}
	}
}

func TestR8_EgressHTTP_NoURL_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "http"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("egress.mode=http with empty url should fail R8")
	}
	var perr *ParseErrors
	errors.As(err, &perr)
	if !pathHasErr(perr, "profiles.x.egress.url") {
		t.Errorf("missing R8 error; items=%+v", perr.Items)
	}
}

func TestR8_EgressHTTP_WithURL_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "http", URL: "http://proxy.example:8080"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.egress.url") {
			t.Errorf("valid http egress rejected; items=%+v", perr.Items)
		}
	}
}

func TestR8_EgressHTTP_BadURL_Fails(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "http", URL: ":::"}},
	}}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("egress.url=::: should fail R8")
	}
}

func TestR8_EgressInherit_NoURL_OK(t *testing.T) {
	f := &File{Default: "inherit-env", Profiles: map[string]Profile{
		"x": {Name: "x", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}, Egress: EgressSpec{Mode: "inherit"}},
	}}
	if err := Validate(f, "test"); err != nil {
		var perr *ParseErrors
		errors.As(err, &perr)
		if pathHasErr(perr, "profiles.x.egress.url") || pathHasErr(perr, "profiles.x.egress.mode") {
			t.Errorf("clean inherit rejected; items=%+v", perr.Items)
		}
	}
}

func TestValidate_MultiErrorCollected(t *testing.T) {
	// profiles.json with 4 distinct issues
	f := &File{
		Default: "nope", // R1
		Profiles: map[string]Profile{
			"glm":  {Name: "glm", BaseURL: "", Auth: &AuthSpec{Mode: "ccwrapKey"}, Egress: EgressSpec{Mode: "inherit"}},               // R3 + R4
			"kimi": {Name: "kimi", BaseURL: "https://x", Auth: &AuthSpec{Mode: "ccwrap_x_api_key"}, Egress: EgressSpec{Mode: "http"}}, // R6 + R8
		},
	}
	err := Validate(f, "test")
	if err == nil {
		t.Fatal("expected multi-error report")
	}
	if !errors.Is(err, ErrValidationFailed) {
		t.Errorf("errors.Is(err, ErrValidationFailed) should be true")
	}
	var perr *ParseErrors
	if !errors.As(err, &perr) {
		t.Fatalf("err not *ParseErrors")
	}
	if len(perr.Items) < 4 {
		t.Errorf("expected >= 4 items; got %d: %+v", len(perr.Items), perr.Items)
	}
	// Multi-line output sanity
	rendered := err.Error()
	if !strings.Contains(rendered, "test invalid: ") {
		t.Errorf("missing header; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "  - ") {
		t.Errorf("missing item indent; got:\n%s", rendered)
	}
}

func TestValidate_SortDeterminism(t *testing.T) {
	// Run validate 10 times — Items must be in same order every run
	// (map iteration randomness defeated by sort).
	f := &File{
		Default: "nope",
		Profiles: map[string]Profile{
			"z-profile": {Name: "z-profile", BaseURL: "", Auth: &AuthSpec{Mode: "ccwrapKey"}, Egress: EgressSpec{Mode: "inherit"}},
			"a-profile": {Name: "a-profile", BaseURL: "", Auth: &AuthSpec{Mode: "ccwrapKey"}, Egress: EgressSpec{Mode: "inherit"}},
		},
	}
	var first string
	for i := 0; i < 10; i++ {
		err := Validate(f, "test")
		if err == nil {
			t.Fatal("expected error")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if got := err.Error(); got != first {
			t.Errorf("iteration %d differs from iteration 0\nfirst:\n%s\ngot:\n%s", i, first, got)
			break
		}
	}
}

func TestParse_ValidProfilesJSON_NoError(t *testing.T) {
	// Regression: an existing-pattern valid profiles.json still parses
	// clean after wiring validate() into Parse.
	data := []byte(`{
		"default": "glm",
		"profiles": {
			"glm": {
				"provider": "GLM",
				"base_url": "https://open.bigmodel.cn/api/anthropic/",
				"auth": {"mode": "ccwrap_x_api_key", "key": "sk-fake"},
				"egress": {"mode": "inherit"}
			}
		}
	}`)
	f, err := Parse(data, "test.json")
	if err != nil {
		t.Fatalf("clean parse failed: %v", err)
	}
	if f.Default != "glm" {
		t.Errorf("default: got %q want glm", f.Default)
	}
}

func TestParse_InvalidProfilesJSON_ReturnsParseErrors(t *testing.T) {
	data := []byte(`{
		"default": "nope",
		"profiles": {
			"glm": {"base_url": "", "auth": {"mode": "ccwrapKey"}}
		}
	}`)
	_, err := Parse(data, "test.json")
	if err == nil {
		t.Fatal("Parse should reject invalid profile")
	}
	if !errors.Is(err, ErrValidationFailed) {
		t.Errorf("errors.Is should match ErrValidationFailed; err=%v", err)
	}
	var perr *ParseErrors
	if !errors.As(err, &perr) {
		t.Fatalf("not *ParseErrors: %v", err)
	}
	if perr.Source != "test.json" {
		t.Errorf("source: got %q want test.json", perr.Source)
	}
}

func TestParse_JSONSyntaxError_NotParseErrors(t *testing.T) {
	// JSON syntax error bails BEFORE validate runs — should NOT return
	// *ParseErrors (existing wrap is preserved).
	data := []byte(`{"default": invalid`)
	_, err := Parse(data, "test.json")
	if err == nil {
		t.Fatal("syntax error should fail")
	}
	if errors.Is(err, ErrValidationFailed) {
		t.Errorf("JSON syntax error should NOT match ErrValidationFailed")
	}
}

// TestValidateEgressSpec validates the single-spec wrapper used by
// HTTP handlers that need to check a draft EgressSpec before applying
// it (e.g. /profile/test-egress with egress_override).
func TestValidateEgressSpec_Table(t *testing.T) {
	cases := []struct {
		name    string
		spec    EgressSpec
		wantErr bool
	}{
		{"empty spec ok", EgressSpec{}, false},
		{"inherit ok", EgressSpec{Mode: "inherit"}, false},
		{"direct ok", EgressSpec{Mode: "direct"}, false},
		{"http with url ok", EgressSpec{Mode: "http", URL: "http://proxy:8080"}, false},
		{"https url for http mode ok", EgressSpec{Mode: "http", URL: "https://proxy:8443"}, false},
		{"socks5 with url ok", EgressSpec{Mode: "socks5", URL: "socks5://proxy:1080"}, false},
		{"socks5h with url ok", EgressSpec{Mode: "socks5h", URL: "socks5h://proxy:1080"}, false},
		{"http missing url", EgressSpec{Mode: "http"}, true},
		{"socks5 missing url", EgressSpec{Mode: "socks5"}, true},
		{"socks5h missing url", EgressSpec{Mode: "socks5h"}, true},
		{"socks5 with http url", EgressSpec{Mode: "socks5", URL: "http://proxy:1080"}, true},
		{"socks5h with socks5 url", EgressSpec{Mode: "socks5h", URL: "socks5://proxy:1080"}, true},
		{"unknown mode", EgressSpec{Mode: "ssh"}, true},
		{"malformed url for http mode", EgressSpec{Mode: "http", URL: "::not a url"}, true},
		{"inherit with url rejected", EgressSpec{Mode: "inherit", URL: "http://proxy:8080"}, true},
		{"direct with url rejected", EgressSpec{Mode: "direct", URL: "http://proxy:8080"}, true},
		{"empty mode with url rejected", EgressSpec{URL: "http://proxy:8080"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEgressSpec(tc.spec)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

// TestValidateEgressSpec_PathPrefix checks the returned ParseErrors
// items have user-meaningful paths (rooted at "egress.*", not the
// internal "profiles..egress.*" form the helper builds).
func TestValidateEgressSpec_PathPrefix(t *testing.T) {
	err := ValidateEgressSpec(EgressSpec{Mode: "socks5"})
	if err == nil {
		t.Fatal("want error for socks5 without URL")
	}
	var perr *ParseErrors
	if !errors.As(err, &perr) {
		t.Fatalf("want *ParseErrors, got %T", err)
	}
	if len(perr.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(perr.Items))
	}
	if !strings.HasPrefix(perr.Items[0].Path, "egress.") {
		t.Fatalf("path: want prefix \"egress.\", got %q", perr.Items[0].Path)
	}
}
