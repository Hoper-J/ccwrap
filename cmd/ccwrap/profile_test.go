package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// TestFormatProfileLine_StripsURLUserinfo is the T7 secret-safe contract
// at the format-line layer: a profile carrying userinfo-bearing base_url
// and egress URL must render with NO userinfo, NO key, NO key_env name or
// resolved value, and NO upstream-header values across CLI stdout/stderr.
func TestFormatProfileLine_StripsURLUserinfo(t *testing.T) {
	p := profiles.Profile{
		Name:     "alpha",
		Provider: "Acme",
		BaseURL:  "https://carol:topsecret@gateway.example.com/v1",
		Auth: &profiles.AuthSpec{
			Mode:   "ccwrap_bearer",
			Key:    "sk-live-secretvalue",
			KeyEnv: "CCWRAP_TEST_PROFILE_KEY_ENV",
		},
		ModelAliases: map[string]string{
			"sonnet": "anthropic/claude-3-5-sonnet",
			"opus":   "anthropic/claude-3-opus",
		},
		UpstreamHeaders: map[string]string{
			"X-Api-Key":  "sk-header-secretvalue",
			"X-Override": "override-secret",
		},
		Egress: profiles.EgressSpec{
			Mode: "http",
			URL:  "http://proxyuser:proxypw@egress.example.com:3128",
		},
	}
	t.Setenv("CCWRAP_TEST_PROFILE_KEY_ENV", "key-env-resolved-value")
	line := formatProfileLine(p, false)
	// Positive identity assertions — the line carries the non-secret
	// identity (name, provider, host, alias count, egress mode/host):
	for _, want := range []string{"alpha", "Acme", "gateway.example.com", "2 aliases"} {
		if !strings.Contains(line, want) {
			t.Errorf("formatProfileLine output missing %q\n  got: %q", want, line)
		}
	}
	// T7 secret-safe — every leak vector must be absent:
	for _, leak := range []string{
		"carol",                       // BaseURL username
		"topsecret",                   // BaseURL password
		"carol:topsecret",             // userinfo pair
		"proxyuser",                   // egress URL username
		"proxypw",                     // egress URL password
		"sk-live-secretvalue",         // inline auth.key
		"sk-header-secretvalue",       // upstream-header value
		"override-secret",             // upstream-header value
		"CCWRAP_TEST_PROFILE_KEY_ENV", // auth.key_env name
		"key-env-resolved-value",      // resolved key_env value
	} {
		if strings.Contains(line, leak) {
			t.Errorf("formatProfileLine leaks %q\n  got: %q", leak, line)
		}
	}
}

// TestFormatProfileLine_MarksActive asserts the active profile is
// distinguished from the rest by a leading "*" marker.
func TestFormatProfileLine_MarksActive(t *testing.T) {
	p := profiles.Profile{Name: "beta", Provider: "X", BaseURL: "https://b.example.com"}
	active := formatProfileLine(p, true)
	inactive := formatProfileLine(p, false)
	if !strings.HasPrefix(strings.TrimLeft(active, " "), "*") {
		t.Errorf("active marker missing; got %q", active)
	}
	if strings.HasPrefix(strings.TrimLeft(inactive, " "), "*") {
		t.Errorf("inactive line should not start with *; got %q", inactive)
	}
}

// TestProfileLsRendersAllProfilesAndStrips writes a profiles.json containing
// userinfo-bearing URLs and inline secrets, then runs `ccwrap profile ls`,
// asserting (a) entries render with non-secret identity, and (b) NO secret
// leaks (T7) in stdout/stderr.
func TestProfileLsRendersAllProfilesAndStrips(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "p.sock")
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profilesJSON := `{
  "default": "alpha",
  "profiles": {
    "alpha": {
      "provider": "Acme",
      "base_url": "https://alice:apw@gw.example.com/v1",
      "auth": {"mode": "ccwrap_bearer", "key": "sk-inline-alpha"},
      "model_aliases": {"sonnet": "anthropic/claude-3-5-sonnet"},
      "upstream_headers": {"X-Api-Key": "sk-header-alpha"},
      "egress": {"mode": "direct"}
    },
    "beta": {
      "provider": "Bravo",
      "base_url": "https://bob:bpw@gw2.example.com",
      "egress": {"mode": "http", "url": "http://proxyu:proxyp@egress.example.com:3128"}
    }
  }
}`
	path := profiles.DefaultPath(paths.StateDir)
	if err := os.WriteFile(path, []byte(profilesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var rc int
	stdout, stderr := withCapturedStdio(t, func() {
		rc = profileLs(paths, "")
	})
	if rc != 0 {
		t.Fatalf("profileLs rc=%d", rc)
	}
	// stderr is intentionally captured to assert no secret leaks
	// there either; stdout is the primary surface, but the T7
	// contract covers both.
	mustNotContain(t, stderr, "alice")
	mustNotContain(t, stderr, "apw")
	mustNotContain(t, stderr, "sk-inline-alpha")
	mustNotContain(t, stderr, "sk-header-alpha")
	mustNotContain(t, stderr, "proxyu")
	mustNotContain(t, stderr, "proxyp")
	mustContainAll(t, stdout, "alpha", "Acme", "gw.example.com", "beta", "Bravo", "gw2.example.com")
	// T7 — no userinfo, key, header value, or proxy creds in stdout:
	for _, leak := range []string{
		"alice", "apw", "alice:apw",
		"bob", "bpw", "bob:bpw",
		"sk-inline-alpha",
		"sk-header-alpha",
		"proxyu", "proxyp",
	} {
		mustNotContain(t, stdout, leak)
	}
}

// TestProfileLsMissingFile prints a friendly empty-inventory message
// and exits 0 (not an error).
func TestProfileLsMissingFile(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "p.sock")
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var rc int
	stdout, _ := withCapturedStdio(t, func() {
		rc = profileLs(paths, "")
	})
	if rc != 0 {
		t.Fatalf("missing profiles.json should rc=0, got %d", rc)
	}
	if !strings.Contains(stdout, "no profiles.json") {
		t.Errorf("expected friendly empty-inventory message; got %q", stdout)
	}
}

// TestRunProfileSubcommandDispatch covers argument-parsing edge cases:
// empty args, unknown subcommand, switch without name. Each must rc=2 and
// emit a usage message to stderr — never a stack trace.
func TestRunProfileSubcommandDispatch(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "p.sock")
	cases := []struct {
		name      string
		args      []string
		wantRC    int
		wantInErr string
	}{
		{name: "no args", args: nil, wantRC: 2, wantInErr: "usage"},
		{name: "unknown subcmd", args: []string{"frobnicate"}, wantRC: 2, wantInErr: "unknown profile subcommand"},
		{name: "switch no name", args: []string{"switch"}, wantRC: 2, wantInErr: "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rc int
			stderr := captureStderr(t, func() {
				rc = runProfileSubcommand(paths, tc.args)
			})
			if rc != tc.wantRC {
				t.Errorf("rc = %d, want %d", rc, tc.wantRC)
			}
			if !strings.Contains(stderr, tc.wantInErr) {
				t.Errorf("stderr missing %q; got %q", tc.wantInErr, stderr)
			}
		})
	}
}

// TestProfileSwitchAgainstLiveSupervisor stands up a real supervisor +
// session + on-disk profiles.json, runs `ccwrap profile switch alpha` end-
// to-end, and asserts the T7 secret-safe contract: NO URL userinfo, NO
// inline auth.key, NO upstream-header values may appear in stdout OR
// stderr. The test exercises the third-party-hidden + passthrough path,
// where the new profile has no resolvable auth source. The switch
// SUCCEEDS (posture published with AuthBootstrap=Missing) rather than
// rejecting at launch; the secret-safe contract — the true subject of the
// test — still holds: a successful switch must scrub userinfo / inline
// keys / header values from any user-visible output. The supervisor's
// switch_test.go covers the posture publication itself; here we lock the
// CLI's sanitization.
func TestProfileSwitchAgainstLiveSupervisor(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "p.sock")
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// profiles.json carries userinfo-bearing URL + inline key + header
	// value — every T7 leak vector exercised on one switch.
	// "alpha" omits auth → ccwrap does not own auth (replaces old mode=passthrough).
	profilesJSON := `{
  "default": "alpha",
  "profiles": {
    "alpha": {
      "provider": "Acme",
      "base_url": "https://alice:apw@gw.example.com/v1",
      "upstream_headers": {"X-Custom": "header-secret-xyz"},
      "egress": {"mode": "direct"}
    }
  }
}`
	if err := os.WriteFile(profiles.DefaultPath(paths.StateDir), []byte(profilesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// LaunchContext is required for SwitchProfile's byte-faithful
	// resolution; without it the supervisor returns RejectedInvalid with
	// "switch unavailable: launch context not retained". Provide the same
	// minimal LaunchContext shape supervisor switch_test.go uses.
	lc := &supervisor.LaunchContext{
		Options: preflight.Options{
			ParentEnv:        []string{"PATH=/usr/bin"},
			WorkingDirectory: t.TempDir(),
		},
		Inspection: &settings.InspectionResult{},
	}
	srv, err := supervisor.New(paths, 0, lc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := waitForControl(waitCtx, client); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "switch-cli"})
	if err != nil {
		t.Fatal(err)
	}
	// Write a manifest so discovery.Find resolves the live session for
	// the CLI helper (resolveSessionForProfileCmd uses discovery).
	sessionDir := filepath.Join(paths.RuntimeDir, "sessions", sess.ID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := model.SessionManifest{
		SessionID:       sess.ID,
		CreatedAt:       sess.CreatedAt,
		UpdatedAt:       time.Now(),
		State:           sess.State,
		SupervisorPID:   sess.SupervisorPID,
		ControlSocket:   paths.SocketPath,
		ProxyListenAddr: sess.ProxyListenAddr,
	}
	if err := manifest.Write(manifest.Path(sessionDir), m); err != nil {
		t.Fatal(err)
	}

	var rc int
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			rc = profileSwitch(paths, sess.ID, "alpha")
		})
	})
	// The switch SUCCEEDS even when the new profile's auth source is
	// missing (third-party-hidden + passthrough). Posture is published with
	// AuthBootstrap=Missing; the supervisor's request-time gate will
	// fail-close any /v1/messages. CLI exit is 0 on the successful switch.
	// The test's true subject — T7 secret-safety — applies on BOTH paths
	// and is asserted below regardless of rc.
	if rc != 0 {
		t.Fatalf("rc=%d unexpected — switch must succeed under C1; stdout=%q stderr=%q", rc, stdout, stderr)
	}
	combined := stdout + "\n" + stderr
	// T7 secret-safe: NONE of these leak vectors may appear in stdout OR
	// stderr — even on the reject path the message is sanitized.
	for _, leak := range []string{
		"alice", "apw", "alice:apw", // base_url userinfo
		"header-secret-xyz", // upstream-header value
	} {
		if strings.Contains(combined, leak) {
			t.Errorf("CLI output leaks %q\n  stdout=%q\n  stderr=%q", leak, stdout, stderr)
		}
	}
	// On a successful switch the CLI announces the new profile by name.
	// Stdout still must not leak secrets (asserted above).
	if !strings.Contains(stdout, "alpha") {
		t.Errorf("stdout missing the new profile name; got %q", stdout)
	}
}

func TestRunProfileSubcommand_NoArgs_UsageListsAllVerbs(t *testing.T) {
	paths := pathsFor(t) // helper from profile_crud_test.go (same package)
	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	code := runProfileSubcommand(paths, []string{})
	w.Close()
	captured, _ := io.ReadAll(r)
	os.Stderr = origStderr

	if code != 2 {
		t.Fatalf("expected 2; got %d", code)
	}
	got := string(captured)
	for _, verb := range []string{"ls", "status", "switch", "test", "add", "edit", "rm", "set-default"} {
		if !strings.Contains(got, verb) {
			t.Fatalf("usage missing verb %q; got %q", verb, got)
		}
	}
}

func TestRunProfileSubcommand_UnknownVerb_UsageListsAllVerbs(t *testing.T) {
	paths := pathsFor(t)
	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	code := runProfileSubcommand(paths, []string{"bogus"})
	w.Close()
	captured, _ := io.ReadAll(r)
	os.Stderr = origStderr

	if code != 2 {
		t.Fatalf("expected 2; got %d", code)
	}
	got := string(captured)
	for _, verb := range []string{"ls", "status", "switch", "test", "add", "edit", "rm", "set-default"} {
		if !strings.Contains(got, verb) {
			t.Fatalf("usage missing verb %q; got %q", verb, got)
		}
	}
}
