// cmd/ccwrap/profile_crud_test.go
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

func TestReadStdinAuthKey_StripsTrailingLF(t *testing.T) {
	r := bytes.NewReader([]byte("abc\n"))
	got, err := readStdinAuthKey(r)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != "abc" {
		t.Fatalf("expected %q; got %q", "abc", got)
	}
}

func TestReadStdinAuthKey_StripsTrailingCRLF(t *testing.T) {
	r := bytes.NewReader([]byte("abc\r\n"))
	got, err := readStdinAuthKey(r)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != "abc" {
		t.Fatalf("expected %q; got %q", "abc", got)
	}
}

func TestReadStdinAuthKey_Empty_ReturnsError(t *testing.T) {
	r := bytes.NewReader([]byte(""))
	_, err := readStdinAuthKey(r)
	if err == nil {
		t.Fatalf("expected error on empty stdin")
	}
	if !strings.Contains(err.Error(), "stdin was empty") {
		t.Fatalf("expected 'stdin was empty' in error; got %v", err)
	}
}

func TestReadStdinAuthKey_OnlyNewline_ReturnsError(t *testing.T) {
	r := bytes.NewReader([]byte("\n"))
	_, err := readStdinAuthKey(r)
	if err == nil {
		t.Fatalf("expected error on newline-only stdin")
	}
}

func TestReadStdinAuthKey_OverflowDetected(t *testing.T) {
	// cap is 4 MiB; feed 4 MiB + 1 byte.
	big := bytes.Repeat([]byte("x"), 4*1024*1024+1)
	r := bytes.NewReader(big)
	_, err := readStdinAuthKey(r)
	if err == nil {
		t.Fatalf("expected overflow error")
	}
	if !strings.Contains(err.Error(), "exceeds 4 MiB cap") {
		t.Fatalf("expected 'exceeds 4 MiB cap' in error; got %v", err)
	}
}

func TestReadStdinAuthKey_AtCapBoundary_OK(t *testing.T) {
	// Exactly 4 MiB — should be accepted.
	big := bytes.Repeat([]byte("x"), 4*1024*1024)
	r := bytes.NewReader(big)
	got, err := readStdinAuthKey(r)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 4*1024*1024 {
		t.Fatalf("expected len 4 MiB; got %d", len(got))
	}
}

func TestParseAddArgs_HappyPath(t *testing.T) {
	opts, err := parseAddArgs([]string{
		"foo",
		"--base-url", "https://api.example.com",
		"--auth-mode", "ccwrap_bearer",
		"--auth-key", "sk-test",
		"--provider", "acme",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if opts.Name != "foo" {
		t.Fatalf("Name: want foo; got %q", opts.Name)
	}
	if opts.BaseURL != "https://api.example.com" {
		t.Fatalf("BaseURL: got %q", opts.BaseURL)
	}
	if opts.AuthMode != "ccwrap_bearer" {
		t.Fatalf("AuthMode: got %q", opts.AuthMode)
	}
	if opts.Provider != "acme" {
		t.Fatalf("Provider: got %q", opts.Provider)
	}
	if opts.AuthKeySource != authKeyInline {
		t.Fatalf("AuthKeySource: want inline; got %v", opts.AuthKeySource)
	}
}

func TestParseAddArgs_ModelAliasesAndHeaders(t *testing.T) {
	opts, err := parseAddArgs([]string{
		"foo",
		"--base-url", "https://api.example.com",
		"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
		"--model-alias", "claude-opus-4-8=gpt-5.5",
		"--model-alias", "claude-sonnet-4-6=gpt-5-mini",
		"--upstream-header", "X-Gateway-Tenant=team-a",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(opts.ModelAliases) != 2 ||
		opts.ModelAliases["claude-opus-4-8"] != "gpt-5.5" ||
		opts.ModelAliases["claude-sonnet-4-6"] != "gpt-5-mini" {
		t.Fatalf("ModelAliases: got %v", opts.ModelAliases)
	}
	if len(opts.UpstreamHeaders) != 1 {
		t.Fatalf("UpstreamHeaders: got %v", opts.UpstreamHeaders)
	}
	var hv string
	for _, v := range opts.UpstreamHeaders {
		hv = v
	}
	if hv != "team-a" {
		t.Fatalf("UpstreamHeaders value: got %q", hv)
	}
	if !opts.SetFields["model-alias"] || !opts.SetFields["upstream-header"] {
		t.Fatalf("SetFields not recorded: %v", opts.SetFields)
	}
}

func TestParseAddArgs_BadModelAlias_Error(t *testing.T) {
	_, err := parseAddArgs([]string{
		"foo", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "sk",
		"--model-alias", "no-equals-sign",
	})
	if err == nil || !strings.Contains(err.Error(), "logical=provider") {
		t.Fatalf("expected logical=provider error, got %v", err)
	}
}

// TestApplyEditOpts_ModelAliasMerge pins the upsert semantic: editing in a new
// alias keeps the profile's existing aliases for other models.
func TestApplyEditOpts_ModelAliasMerge(t *testing.T) {
	p := profiles.Profile{
		Name:         "foo",
		BaseURL:      "https://x",
		ModelAliases: map[string]string{"claude-opus-4-8": "gpt-5.5"},
	}
	opts, err := parseEditArgs([]string{"foo", "--model-alias", "claude-sonnet-4-6=gpt-5-mini"})
	if err != nil {
		t.Fatalf("parseEditArgs: %v", err)
	}
	if err := applyEditOpts(&p, opts, nil); err != nil {
		t.Fatalf("applyEditOpts: %v", err)
	}
	if p.ModelAliases["claude-opus-4-8"] != "gpt-5.5" {
		t.Fatalf("merge wiped the existing alias: %v", p.ModelAliases)
	}
	if p.ModelAliases["claude-sonnet-4-6"] != "gpt-5-mini" {
		t.Fatalf("merge did not add the new alias: %v", p.ModelAliases)
	}
}

func TestParseAddArgs_MissingName_Error(t *testing.T) {
	_, err := parseAddArgs([]string{"--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "k"})
	if err == nil {
		t.Fatalf("expected error on missing name")
	}
}

func TestParseAddArgs_MissingBaseURL_Error(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--auth-mode", "ccwrap_bearer", "--auth-key", "k"})
	if err == nil {
		t.Fatalf("expected error on missing --base-url")
	}
}

func TestParseAddArgs_MissingAuthMode_Error(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--base-url", "https://x"})
	if err == nil {
		t.Fatalf("expected error on missing --auth-mode")
	}
}

func TestParseAddArgs_AuthKeyMutex_FlagAndStdin(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "x", "--auth-key-stdin"})
	if err == nil {
		t.Fatalf("expected mutex error")
	}
}

func TestParseAddArgs_AuthKeyMutex_FlagAndEnv(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "x", "--auth-key-env", "VAR"})
	if err == nil {
		t.Fatalf("expected mutex error")
	}
}

func TestParseAddArgs_AuthKeyMutex_StdinAndEnv(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key-stdin", "--auth-key-env", "VAR"})
	if err == nil {
		t.Fatalf("expected mutex error")
	}
}

// TestParseAddArgs_Passthrough_RejectedWithGuidance — V1 removed
// "passthrough" as a mode value (a no-auth profile is an ABSENT auth block).
// add must fail fast at parse time, not defer to a guaranteed write-time
// validation reject, and the error must name the profiles.json workaround.
// With or without a key flag: the mode itself is the error.
func TestParseAddArgs_Passthrough_RejectedWithGuidance(t *testing.T) {
	for _, extra := range [][]string{nil, {"--auth-key", "x"}} {
		args := append([]string{"foo", "--base-url", "https://x", "--auth-mode", "passthrough"}, extra...)
		_, err := parseAddArgs(args)
		if err == nil {
			t.Fatalf("expected error for --auth-mode passthrough (extra=%v)", extra)
		}
		if !strings.Contains(err.Error(), "NO auth block") {
			t.Fatalf("error must carry the no-auth-block guidance; got %q", err)
		}
	}
}

func TestParseAddArgs_BearerWithoutAuthKey_Error(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer"})
	if err == nil {
		t.Fatalf("expected error: ccwrap_bearer requires --auth-key*")
	}
}

func TestParseAddArgs_EgressHttp_RequiresURL(t *testing.T) {
	_, err := parseAddArgs([]string{"foo", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "k", "--egress-mode", "http"})
	if err == nil {
		t.Fatalf("expected error: egress http needs --egress-url")
	}
}

func TestParseAddArgs_EmptyName_Error(t *testing.T) {
	_, err := parseAddArgs([]string{"", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "k"})
	if err == nil {
		t.Fatalf("expected error: empty name")
	}
}

func TestParseAddArgs_WhitespaceName_Error(t *testing.T) {
	_, err := parseAddArgs([]string{" foo ", "--base-url", "https://x", "--auth-mode", "ccwrap_bearer", "--auth-key", "k"})
	if err == nil {
		t.Fatalf("expected error: whitespace-bounded name")
	}
}

// TestBuildProfileFromOpts_ModeWithoutKeySource pins builder mechanics in
// isolation: the mode maps onto Auth.Mode and absent key sources leave
// Key/KeyEnv empty. (Mode<->key pairing is parse-time policy, not the
// builder's job.)
func TestBuildProfileFromOpts_ModeWithoutKeySource(t *testing.T) {
	opts := crudOpts{
		Name:     "foo",
		Provider: "acme",
		BaseURL:  "https://api.example.com",
		AuthMode: "ccwrap_bearer",
	}
	p, err := buildProfileFromOpts(opts, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Name != "foo" {
		t.Fatalf("Name: got %q", p.Name)
	}
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "" || p.Auth.KeyEnv != "" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
	if p.Egress.Mode != "" {
		t.Fatalf("Egress.Mode should be empty default; got %q", p.Egress.Mode)
	}
}

func TestBuildProfileFromOpts_BearerInline(t *testing.T) {
	opts := crudOpts{
		Name:          "bar",
		BaseURL:       "https://api.example.com",
		AuthMode:      "ccwrap_bearer",
		AuthKeySource: authKeyInline,
		AuthKeyInline: "sk-abc",
	}
	p, err := buildProfileFromOpts(opts, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "sk-abc" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
}

func TestBuildProfileFromOpts_BearerEnv(t *testing.T) {
	opts := crudOpts{
		Name:          "baz",
		BaseURL:       "https://api.example.com",
		AuthMode:      "ccwrap_bearer",
		AuthKeySource: authKeyEnv,
		AuthKeyEnv:    "MY_TOKEN",
	}
	p, err := buildProfileFromOpts(opts, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.KeyEnv != "MY_TOKEN" || p.Auth.Key != "" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
}

func TestBuildProfileFromOpts_BearerStdin(t *testing.T) {
	opts := crudOpts{
		Name:          "qux",
		BaseURL:       "https://api.example.com",
		AuthMode:      "ccwrap_bearer",
		AuthKeySource: authKeyStdin,
	}
	stdin := strings.NewReader("sk-stdin-key\n")
	p, err := buildProfileFromOpts(opts, stdin)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "sk-stdin-key" || p.Auth.KeyEnv != "" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
}

func TestBuildProfileFromOpts_BearerStdinEmpty_Error(t *testing.T) {
	opts := crudOpts{
		Name:          "qux",
		BaseURL:       "https://api.example.com",
		AuthMode:      "ccwrap_bearer",
		AuthKeySource: authKeyStdin,
	}
	stdin := strings.NewReader("")
	_, err := buildProfileFromOpts(opts, stdin)
	if err == nil {
		t.Fatalf("expected stdin-empty error")
	}
}

func TestBuildProfileFromOpts_EgressHttp(t *testing.T) {
	opts := crudOpts{
		Name:       "h",
		BaseURL:    "https://api.example.com",
		AuthMode:   "ccwrap_bearer",
		EgressMode: "http",
		EgressURL:  "http://proxy:8080",
	}
	p, err := buildProfileFromOpts(opts, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Egress.Mode != "http" || p.Egress.URL != "http://proxy:8080" {
		t.Fatalf("Egress: got %+v", p.Egress)
	}
}

func TestBuildProfileFromOpts_SetsName(t *testing.T) {
	opts := crudOpts{Name: "named", BaseURL: "https://api.example.com", AuthMode: "ccwrap_bearer"}
	p, err := buildProfileFromOpts(opts, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Name != "named" {
		t.Fatalf("expected Name=named; got %q", p.Name)
	}
}

var _ = profiles.Profile{} // pin import

// withCapturedStdio replaces stdOut/stdErr for the duration of fn and
// returns the captured contents.
func withCapturedStdio(t *testing.T, fn func()) (string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	origOut := stdOut
	origErr := stdErr
	stdOut = func() io.Writer { return &out }
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() {
		stdOut = origOut
		stdErr = origErr
	})
	fn()
	return out.String(), errBuf.String()
}

// pathsFor returns an app.Paths rooted at a temp dir for CRUD tests.
func pathsFor(t *testing.T) app.Paths {
	t.Helper()
	dir := t.TempDir()
	return app.Paths{StateDir: dir, RuntimeDir: dir}
}

func TestProfileAdd_CreatesFileWhenMissing(t *testing.T) {
	paths := pathsFor(t)
	args := []string{
		"foo",
		"--base-url", "https://api.example.com",
		"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
	}
	var code int
	out, errBuf := withCapturedStdio(t, func() {
		code = profileAdd(paths, args)
	})
	if code != 0 {
		t.Fatalf("expected exit 0; got %d. stderr=%q", code, errBuf)
	}
	if !strings.Contains(out, "foo") {
		t.Fatalf("expected 'foo' in stdout; got %q", out)
	}
	path := profiles.DefaultPath(paths.StateDir)
	file, err := profiles.Load(path)
	if err != nil || file == nil {
		t.Fatalf("file not loaded; err=%v file=%v", err, file)
	}
	if _, ok := file.Profiles["foo"]; !ok {
		t.Fatalf("profile 'foo' not in file; got %+v", file.Profiles)
	}
}

func TestProfileAdd_AppendsToExisting(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	// Pre-seed with one profile via OverwriteFile.
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileAdd(paths, []string{
		"beta",
		"--base-url", "https://b.example.com",
		"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
	})
	if code != 0 {
		t.Fatalf("expected exit 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	if file == nil || len(file.Profiles) != 2 {
		t.Fatalf("expected 2 profiles; got %+v", file)
	}
	if file.Default != "alpha" {
		t.Fatalf("Default should be unchanged; got %q", file.Default)
	}
}

var _ = os.DevNull         // pin os import
var _ = filepath.Separator // pin filepath import

func TestProfileAdd_NameConflict_Exit1(t *testing.T) {
	paths := pathsFor(t)
	// First add succeeds.
	code := profileAdd(paths, []string{
		"foo", "--base-url", "https://a.example.com", "--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
	})
	if code != 0 {
		t.Fatalf("first add: expected 0; got %d", code)
	}
	// Second add of same name fails.
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code = profileAdd(paths, []string{
		"foo", "--base-url", "https://b.example.com", "--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
	})
	if code != 1 {
		t.Fatalf("expected exit 1; got %d", code)
	}
	if !strings.Contains(errBuf.String(), `already exists`) {
		t.Fatalf("expected 'already exists' in stderr; got %q", errBuf.String())
	}
}

func TestProfileAdd_ReservedName_Exit1ViaValidation(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileAdd(paths, []string{
		"inherit-env",
		"--base-url", "https://x.example.com",
		"--auth-mode", "ccwrap_bearer",
		"--auth-key", "k",
	})
	if code != 1 {
		t.Fatalf("expected exit 1; got %d", code)
	}
	// R2 message includes "must not equal sentinel".
	if !strings.Contains(errBuf.String(), `must not equal sentinel`) {
		t.Fatalf("expected R2 sentinel error; got %q", errBuf.String())
	}
}

func TestProfileAdd_EmptyName_Exit2(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileAdd(paths, []string{
		"", "--base-url", "https://x.example.com", "--auth-mode", "ccwrap_bearer", "--auth-key", "k",
	})
	if code != 2 {
		t.Fatalf("expected exit 2; got %d", code)
	}
}

func TestProfileAdd_WhitespaceName_Exit2(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileAdd(paths, []string{
		" foo ", "--base-url", "https://x.example.com", "--auth-mode", "ccwrap_bearer", "--auth-key", "k",
	})
	if code != 2 {
		t.Fatalf("expected exit 2; got %d", code)
	}
}

func TestProfileAdd_BadBaseURL_Exit1_NoWrite(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileAdd(paths, []string{
		"foo", "--base-url", "ftp://wrong-scheme", "--auth-mode", "ccwrap_bearer", "--auth-key", "k",
	})
	if code != 1 {
		t.Fatalf("expected exit 1; got %d. stderr=%q", code, errBuf.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not have been created; stat err=%v", err)
	}
	// Validation error should NOT have the "ccwrap profile add:" prefix.
	if strings.HasPrefix(errBuf.String(), "ccwrap profile add:") {
		t.Fatalf("unexpected CLI prefix on ParseErrors output; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "profile add foo invalid") {
		t.Fatalf("expected operation-labeled error; got %q", errBuf.String())
	}
}

func TestProfileAdd_SetDefault_UpdatesFileDefault(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	// Pre-seed two profiles, default=alpha.
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileAdd(paths, []string{
		"beta",
		"--base-url", "https://b.example.com",
		"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
		"--set-default",
	})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	if file.Default != "beta" {
		t.Fatalf("expected default=beta; got %q", file.Default)
	}
}

func TestProfileAdd_StdoutMatchesLsRow(t *testing.T) {
	paths := pathsFor(t)
	out, _ := withCapturedStdio(t, func() {
		_ = profileAdd(paths, []string{
			"foo",
			"--base-url", "https://api.example.com",
			"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-test",
			"--provider", "acme",
			"--set-default",
		})
	})
	out = strings.TrimRight(out, "\n")
	// Active marker "*" because --set-default was passed.
	if !strings.HasPrefix(out, "* foo") {
		t.Fatalf("expected '* foo' prefix; got %q", out)
	}
	// Provider + host appear.
	if !strings.Contains(out, "acme") {
		t.Fatalf("expected 'acme' in row; got %q", out)
	}
	if !strings.Contains(out, "api.example.com") {
		t.Fatalf("expected host in row; got %q", out)
	}
}

func TestProfileAdd_AuthKeyStdin_StoresInlineKey(t *testing.T) {
	paths := pathsFor(t)
	stdin := strings.NewReader("sk-stdin-secret\n")
	code := profileAddIO(paths, stdin, []string{
		"foo",
		"--base-url", "https://api.example.com",
		"--auth-mode", "ccwrap_bearer",
		"--auth-key-stdin",
	})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(profiles.DefaultPath(paths.StateDir))
	p := file.Profiles["foo"]
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "sk-stdin-secret" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
}

func TestProfileEdit_NoSuchProfile_Exit1(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{
		"ghost", "--provider", "x",
	})
	if code != 1 {
		t.Fatalf("expected 1; got %d", code)
	}
}

func TestProfileEdit_NoFlags_Exit2(t *testing.T) {
	paths := pathsFor(t)
	// Pre-seed.
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {Name: "foo", BaseURL: "https://x.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{"foo"})
	if code != 2 {
		t.Fatalf("expected 2; got %d", code)
	}
}

func TestProfileEdit_ProviderOnly_PreservesOtherFields(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name:         "foo",
			BaseURL:      "https://x.example.com",
			Auth:         &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-original"},
			ModelAliases: map[string]string{"alias": "real"},
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--provider", "acme"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	p := file.Profiles["foo"]
	if p.Provider != "acme" {
		t.Fatalf("Provider: got %q", p.Provider)
	}
	if p.BaseURL != "https://x.example.com" {
		t.Fatalf("BaseURL changed: got %q", p.BaseURL)
	}
	if p.Auth.Key != "sk-original" {
		t.Fatalf("Auth.Key changed: got %q", p.Auth.Key)
	}
	if p.ModelAliases["alias"] != "real" {
		t.Fatalf("ModelAliases lost: got %+v", p.ModelAliases)
	}
}

func TestProfileEdit_ExplicitEmptyProvider_Clears(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", Provider: "acme",
			BaseURL: "https://x.example.com",
			Auth:    nil,
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--provider", ""})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	if file.Profiles["foo"].Provider != "" {
		t.Fatalf("Provider should be empty; got %q", file.Profiles["foo"].Provider)
	}
}

func TestProfileEdit_AbsentFlag_LeavesProviderAlone(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", Provider: "acme",
			BaseURL: "https://x.example.com",
			Auth:    nil,
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--base-url", "https://new.example.com"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	if file.Profiles["foo"].Provider != "acme" {
		t.Fatalf("Provider should be unchanged; got %q", file.Profiles["foo"].Provider)
	}
	if file.Profiles["foo"].BaseURL != "https://new.example.com" {
		t.Fatalf("BaseURL: got %q", file.Profiles["foo"].BaseURL)
	}
}

func TestProfileEdit_PassthroughWithKey_Exit2(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {Name: "foo", BaseURL: "https://x.example.com", Auth: &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "old"}},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{
		"foo",
		"--auth-mode", "passthrough",
		"--auth-key", "new",
	})
	if code != 2 {
		t.Fatalf("expected 2 (E5 mutex); got %d. stderr=%q", code, errBuf.String())
	}
}

// TestProfileEdit_PassthroughNoKeyFlag_AutoClearsAuth — removed. The
// behavior it tested (CLI's "switch to mode=passthrough auto-clears key")
// is obsolete under V1; passthrough is no longer a valid mode value.
// The CLI's surface for expressing "remove auth" will be added in a
// later slice. New V1 coverage lives in
// TestValidate_RejectsPassthrough (internal/profiles/validate_test.go).

func TestProfileEdit_ToBearer_RequiresKey(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {Name: "foo", BaseURL: "https://x.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{
		"foo",
		"--auth-mode", "ccwrap_bearer",
	})
	// R6 fires at OverwriteFile -> exit 1 (operational/validation).
	if code != 1 {
		t.Fatalf("expected 1; got %d. stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "key or key_env required") {
		t.Fatalf("expected R6 message; got %q", errBuf.String())
	}
}

func TestProfileEdit_EgressInherit_ClearsURL(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", BaseURL: "https://x.example.com",
			Auth:   nil,
			Egress: profiles.EgressSpec{Mode: "http", URL: "http://user:pw@proxy:8080"},
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--egress-mode", "inherit"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	p := file.Profiles["foo"]
	if p.Egress.Mode != "inherit" || p.Egress.URL != "" {
		t.Fatalf("Egress: got %+v", p.Egress)
	}
}

func TestProfileEdit_EgressDirect_ClearsURL(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", BaseURL: "https://x.example.com",
			Auth:   nil,
			Egress: profiles.EgressSpec{Mode: "http", URL: "http://user:pw@proxy:8080"},
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--egress-mode", "direct"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	p := file.Profiles["foo"]
	if p.Egress.Mode != "direct" || p.Egress.URL != "" {
		t.Fatalf("Egress: got %+v", p.Egress)
	}
}

func TestProfileEdit_EgressHttp_PreservesURL(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", BaseURL: "https://x.example.com",
			Auth:   nil,
			Egress: profiles.EgressSpec{Mode: "http", URL: "http://proxy:8080"},
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--egress-url", "http://new-proxy:9090"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	p := file.Profiles["foo"]
	if p.Egress.Mode != "http" || p.Egress.URL != "http://new-proxy:9090" {
		t.Fatalf("Egress: got %+v", p.Egress)
	}
}

func TestProfileEdit_PreservesModelAliases_UpstreamHeaders(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", BaseURL: "https://x.example.com",
			Auth:            nil,
			ModelAliases:    map[string]string{"shortcut": "claude-x"},
			UpstreamHeaders: map[string]string{"X-Custom": "v"},
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileEdit(paths, []string{"foo", "--provider", "acme"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	p := file.Profiles["foo"]
	if p.ModelAliases["shortcut"] != "claude-x" {
		t.Fatalf("ModelAliases lost: %+v", p.ModelAliases)
	}
	if p.UpstreamHeaders["X-Custom"] != "v" {
		t.Fatalf("UpstreamHeaders lost: %+v", p.UpstreamHeaders)
	}
}

func TestProfileEdit_ValidationFailure_NoDuplicatePrefix(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {Name: "foo", BaseURL: "https://x.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{"foo", "--base-url", "ftp://wrong"})
	if code != 1 {
		t.Fatalf("expected 1; got %d", code)
	}
	if strings.HasPrefix(errBuf.String(), "ccwrap profile edit:") {
		t.Fatalf("unexpected CLI prefix on ParseErrors; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "profile edit foo invalid") {
		t.Fatalf("expected operation-labeled error; got %q", errBuf.String())
	}
}

func TestProfileEdit_EmptyName_Exit2(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{"", "--provider", "x"})
	if code != 2 {
		t.Fatalf("expected 2; got %d", code)
	}
}

func TestParseAddArgs_EgressInherit_WithURL_Error(t *testing.T) {
	_, err := parseAddArgs([]string{
		"foo",
		"--base-url", "https://x.example.com",
		"--auth-mode", "ccwrap_bearer",
		"--auth-key", "k",
		"--egress-mode", "inherit",
		"--egress-url", "http://proxy:8080",
	})
	if err == nil {
		t.Fatalf("expected error: inherit + --egress-url")
	}
}

func TestProfileEdit_EgressInherit_WithURL_Exit2(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "foo", Profiles: map[string]profiles.Profile{
		"foo": {
			Name: "foo", BaseURL: "https://x.example.com",
			Auth: nil,
		},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileEdit(paths, []string{
		"foo",
		"--egress-mode", "direct",
		"--egress-url", "http://user:pw@proxy:8080",
	})
	if code != 2 {
		t.Fatalf("expected 2 (mutex); got %d. stderr=%q", code, errBuf.String())
	}
}

func TestProfileRm_NoSuchProfile_Exit1(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileRm(paths, []string{"ghost"})
	if code != 1 {
		t.Fatalf("expected 1; got %d", code)
	}
}

func TestProfileRm_NotDefault_RemovesQuietly(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
		"beta":  {Name: "beta", BaseURL: "https://b.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, _ := withCapturedStdio(t, func() {
		code := profileRm(paths, []string{"beta"})
		if code != 0 {
			t.Fatalf("expected 0; got %d", code)
		}
	})
	if !strings.Contains(out, `removed profile "beta"`) {
		t.Fatalf("expected confirmation; got %q", out)
	}
	file, _ := profiles.Load(path)
	if _, ok := file.Profiles["beta"]; ok {
		t.Fatalf("beta should be removed; got %+v", file.Profiles)
	}
	if file.Default != "alpha" {
		t.Fatalf("Default should be unchanged; got %q", file.Default)
	}
}

func TestProfileRm_EmptyName_Exit2(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileRm(paths, []string{""})
	if code != 2 {
		t.Fatalf("expected 2; got %d", code)
	}
}

func TestProfileRm_DefaultWithoutForce_Exit1(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileRm(paths, []string{"alpha"})
	if code != 1 {
		t.Fatalf("expected 1; got %d", code)
	}
	msg := errBuf.String()
	if !strings.Contains(msg, "refusing to remove the default profile") {
		t.Fatalf("expected refusal phrase; got %q", msg)
	}
	if !strings.Contains(msg, "next ccwrap launch") {
		t.Fatalf("expected next-launch warning; got %q", msg)
	}
	if !strings.Contains(msg, "inherit-env") {
		t.Fatalf("expected 'inherit-env' in warning; got %q", msg)
	}
	file, _ := profiles.Load(path)
	if _, ok := file.Profiles["alpha"]; !ok {
		t.Fatalf("alpha should still exist; got %+v", file.Profiles)
	}
}

func TestProfileRm_DefaultWithForce_ResetsDefault(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
		"beta":  {Name: "beta", BaseURL: "https://b.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code := profileRm(paths, []string{"alpha", "--force"})
	if code != 0 {
		t.Fatalf("expected 0; got %d", code)
	}
	file, _ := profiles.Load(path)
	if _, ok := file.Profiles["alpha"]; ok {
		t.Fatalf("alpha should be gone; got %+v", file.Profiles)
	}
	if file.Default != profiles.InheritEnv {
		t.Fatalf("expected Default=inherit-env; got %q", file.Default)
	}
}

func TestProfileRm_LastProfile_LeavesEmptyFile(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, _ := withCapturedStdio(t, func() {
		code := profileRm(paths, []string{"alpha", "--force"})
		if code != 0 {
			t.Fatalf("expected 0; got %d", code)
		}
	})
	if !strings.Contains(out, `removed profile "alpha"`) {
		t.Fatalf("expected removed line; got %q", out)
	}
	if !strings.Contains(out, "now empty") {
		t.Fatalf("expected empty notification; got %q", out)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file should exist; stat err=%v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected 0o600; got %o", got)
	}
	file, _ := profiles.Load(path)
	if file == nil || file.Default != profiles.InheritEnv || len(file.Profiles) != 0 {
		t.Fatalf("expected zero-state file; got %+v", file)
	}
}

func TestProfileRm_LiveSessionLookerWarns(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
		"beta":  {Name: "beta", BaseURL: "https://b.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	called := 0
	looker := func(ctx context.Context, p app.Paths, profileName string) []string {
		called++
		if profileName == "beta" {
			return []string{"ABC"}
		}
		return nil
	}

	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })

	code := profileRmWithLooker(paths, []string{"beta"}, looker)
	if code != 0 {
		t.Fatalf("expected 0 (warn-but-proceed); got %d", code)
	}
	if called == 0 {
		t.Fatalf("expected looker to be called")
	}
	if !strings.Contains(errBuf.String(), "warning: session ABC") {
		t.Fatalf("expected warning text; got %q", errBuf.String())
	}
	file, _ := profiles.Load(path)
	if _, ok := file.Profiles["beta"]; ok {
		t.Fatalf("beta should be removed despite warning")
	}
}

func TestProfileSetDefault_KnownProfile_Updates(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
		"beta":  {Name: "beta", BaseURL: "https://b.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, _ := withCapturedStdio(t, func() {
		code := profileSetDefault(paths, []string{"beta"})
		if code != 0 {
			t.Fatalf("expected 0; got %d", code)
		}
	})
	if !strings.Contains(out, "default = beta") {
		t.Fatalf("expected confirmation; got %q", out)
	}
	file, _ := profiles.Load(path)
	if file.Default != "beta" {
		t.Fatalf("Default: got %q", file.Default)
	}
}

func TestProfileSetDefault_InheritEnv_Sentinel(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, _ := withCapturedStdio(t, func() {
		code := profileSetDefault(paths, []string{"inherit-env"})
		if code != 0 {
			t.Fatalf("expected 0; got %d", code)
		}
	})
	if !strings.Contains(out, "default = inherit-env") {
		t.Fatalf("expected sentinel confirmation; got %q", out)
	}
	file, _ := profiles.Load(path)
	if file.Default != profiles.InheritEnv {
		t.Fatalf("Default: got %q", file.Default)
	}
}

func TestProfileSetDefault_NoArg_Exit2(t *testing.T) {
	paths := pathsFor(t)
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileSetDefault(paths, []string{})
	if code != 2 {
		t.Fatalf("expected 2; got %d", code)
	}
}

func TestProfileSetDefault_Unknown_Exit1(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileSetDefault(paths, []string{"ghost"})
	if code != 1 {
		t.Fatalf("expected 1; got %d", code)
	}
}

func TestProfileSetDefault_MixedCaseInheritEnv_Exit1(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com", Auth: nil},
	}}
	if err := profiles.OverwriteFile(path, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var errBuf bytes.Buffer
	origErr := stdErr
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() { stdErr = origErr })
	code := profileSetDefault(paths, []string{"Inherit-Env"})
	if code != 1 {
		t.Fatalf("expected 1 (case-sensitive); got %d", code)
	}
	file, _ := profiles.Load(path)
	if file.Default != "alpha" {
		t.Fatalf("Default should be unchanged; got %q", file.Default)
	}
}
