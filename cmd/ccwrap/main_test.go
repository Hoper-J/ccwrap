package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/testutil"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

func TestVerifyStopTargetRejectsUnreachableSession(t *testing.T) {
	err := verifyStopTarget(model.DiscoveredSession{Manifest: model.SessionManifest{SessionID: "sess-unreachable"}})
	if err == nil {
		t.Fatal("expected unreachable session verification to fail")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyStopTargetAcceptsLiveSession(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	srv, err := supervisor.New(paths, 0, nil)
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
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "verify-stop"})
	if err != nil {
		t.Fatal(err)
	}
	startToken, err := procmeta.CurrentStartToken()
	if err != nil {
		t.Fatal(err)
	}
	ds := model.DiscoveredSession{
		Manifest: model.SessionManifest{
			SessionID:            sess.ID,
			ControlSocket:        paths.SocketPath,
			SupervisorPID:        sess.SupervisorPID,
			SupervisorStartToken: startToken,
			ProxyListenAddr:      sess.ProxyListenAddr,
		},
		Reachable: true,
	}
	if err := verifyStopTarget(ds); err != nil {
		t.Fatalf("expected live session verification to pass: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyStopTargetRejectsSupervisorPIDMismatch(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	srv, err := supervisor.New(paths, 0, nil)
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
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "verify-stop-mismatch"})
	if err != nil {
		t.Fatal(err)
	}
	ds := model.DiscoveredSession{
		Manifest: model.SessionManifest{
			SessionID:       sess.ID,
			ControlSocket:   paths.SocketPath,
			SupervisorPID:   sess.SupervisorPID + 1,
			ProxyListenAddr: sess.ProxyListenAddr,
		},
		Reachable: true,
	}
	err = verifyStopTarget(ds)
	if err == nil {
		t.Fatal("expected supervisor PID mismatch to fail")
	}
	if !strings.Contains(err.Error(), "PID mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyStopTargetRejectsSupervisorStartTokenMismatch(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "c.sock")
	srv, err := supervisor.New(paths, 0, nil)
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
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "verify-stop-token-mismatch"})
	if err != nil {
		t.Fatal(err)
	}
	ds := model.DiscoveredSession{
		Manifest: model.SessionManifest{
			SessionID:            sess.ID,
			ControlSocket:        paths.SocketPath,
			SupervisorPID:        sess.SupervisorPID,
			SupervisorStartToken: "definitely-not-the-current-process",
			ProxyListenAddr:      sess.ProxyListenAddr,
		},
		Reachable: true,
	}
	err = verifyStopTarget(ds)
	if err == nil {
		t.Fatal("expected supervisor start token mismatch to fail")
	}
	if !strings.Contains(err.Error(), "identity no longer matches") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParseLaunchArgsPassesClaudeShortFlagWithoutDoubleDash(t *testing.T) {
	got, err := parseLaunchArgs([]string{"-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "hello"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
	if got.EgressProxy != "auto" || got.ClaudeBin == "" {
		t.Fatalf("unexpected defaults: %#v", got)
	}
}

func TestParseLaunchArgsPassesClaudeSettingsWithoutDoubleDash(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--settings", "./settings.json", "-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--settings", "./settings.json", "-p", "hello"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsConsumesKnownCCWRAPFlagsBeforeClaudeArgs(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--upstream", "https://api.example.com", "--session-name=demo", "--egress-proxy", "direct", "-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Upstream != "https://api.example.com" || got.SessionName != "demo" || got.EgressProxy != "direct" {
		t.Fatalf("unexpected CCWRAP args: %#v", got)
	}
	want := []string{"-p", "hello"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsStopsAtFirstUnknownFlag(t *testing.T) {
	got, err := parseLaunchArgs([]string{"-p", "hello", "--session-name", "belongs-to-claude"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "hello", "--session-name", "belongs-to-claude"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
	if got.SessionName != "" {
		t.Fatalf("expected CCWRAP not to consume --session-name after Claude args start, got %#v", got)
	}
}

func TestParseLaunchArgsDoubleDashForcesRemainingArgsToClaude(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--session-name", "demo", "--", "--upstream", "belongs-to-claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionName != "demo" {
		t.Fatalf("SessionName = %q, want demo", got.SessionName)
	}
	want := []string{"--upstream", "belongs-to-claude"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsMissingKnownFlagValueFails(t *testing.T) {
	for _, args := range [][]string{{"--upstream"}, {"--upstream", "--model"}, {"--upstream", "--"}} {
		if _, err := parseLaunchArgs(args); err == nil {
			t.Fatalf("expected missing --upstream value to fail for %#v", args)
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseLaunchArgsModelAliasFlagsBeforeClaudeArgs(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--model-alias-file", "aliases.json", "--model-alias", "claude-sonnet-4-6=gateway/sonnet", "-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelAliasFile != "aliases.json" {
		t.Fatalf("ModelAliasFile = %q", got.ModelAliasFile)
	}
	if len(got.ModelAliases) != 1 || got.ModelAliases[0] != "claude-sonnet-4-6=gateway/sonnet" {
		t.Fatalf("ModelAliases = %#v", got.ModelAliases)
	}
	want := []string{"-p", "hello"}
	if strings.Join(got.ClaudeArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsAllowProviderModelPassthrough(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--allow-provider-model-passthrough", "--model", "gateway/sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowProviderModelPassthrough {
		t.Fatalf("expected AllowProviderModelPassthrough=true")
	}
	want := []string{"--model", "gateway/sonnet"}
	if strings.Join(got.ClaudeArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsAllowAuthPassthroughToThirdParty(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--allow-auth-passthrough-to-third-party", "-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowAuthPassthroughToThirdParty {
		t.Fatalf("expected AllowAuthPassthroughToThirdParty=true")
	}
	want := []string{"-p", "hello"}
	if strings.Join(got.ClaudeArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsQuiet(t *testing.T) {
	t.Run("default off when no flag and no env", func(t *testing.T) {
		t.Setenv("CCWRAP_QUIET", "")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Quiet {
			t.Fatalf("expected Quiet=false by default")
		}
	})
	t.Run("--quiet sets it; claude args pass through", func(t *testing.T) {
		t.Setenv("CCWRAP_QUIET", "")
		got, err := parseLaunchArgs([]string{"--quiet", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Quiet {
			t.Fatalf("expected Quiet=true with --quiet")
		}
		want := []string{"-p", "hello"}
		if strings.Join(got.ClaudeArgs, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
		}
	})
	t.Run("CCWRAP_QUIET=1 enables when no flag", func(t *testing.T) {
		t.Setenv("CCWRAP_QUIET", "1")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Quiet {
			t.Fatalf("expected Quiet=true when CCWRAP_QUIET=1")
		}
	})
	t.Run("--quiet=false overrides env", func(t *testing.T) {
		t.Setenv("CCWRAP_QUIET", "1")
		got, err := parseLaunchArgs([]string{"--quiet=false", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Quiet {
			t.Fatalf("expected Quiet=false: --quiet=false must override CCWRAP_QUIET=1")
		}
	})
}

func TestQuietSummaryLine(t *testing.T) {
	pal := ui.New(false) // no color so the plain string is assertable
	got := quietSummaryLine(pal, "api.anthropic.com", "official", false, "http://127.0.0.1:65029")
	want := "ccwrap → api.anthropic.com · official · inspect http://127.0.0.1:65029"
	if got != want {
		t.Errorf("quietSummaryLine = %q, want %q", got, want)
	}
	// no profile + degraded marker
	got2 := quietSummaryLine(pal, "gateway.example", "", true, "http://127.0.0.1:5000")
	want2 := "ccwrap → gateway.example · degraded · inspect http://127.0.0.1:5000"
	if got2 != want2 {
		t.Errorf("quietSummaryLine(degraded) = %q, want %q", got2, want2)
	}
}

func TestParseLaunchArgsCaptureRequestBodies(t *testing.T) {
	t.Run("default off when no flag and no env", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_BODIES", "")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if got.CaptureBodies {
			t.Fatalf("expected CaptureBodies=false by default")
		}
	})

	t.Run("env truthy enables when no flag", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_BODIES", "1")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.CaptureBodies {
			t.Fatalf("expected CaptureBodies=true when CCWRAP_CAPTURE_BODIES=1")
		}
	})

	t.Run("flag wins over env=0", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_BODIES", "0")
		got, err := parseLaunchArgs([]string{"--capture-request-bodies", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.CaptureBodies {
			t.Fatalf("expected CaptureBodies=true: flag must win over CCWRAP_CAPTURE_BODIES=0")
		}
		want := []string{"-p", "hello"}
		if strings.Join(got.ClaudeArgs, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
		}
	})

	t.Run("env truthy values accepted, empty is off", func(t *testing.T) {
		for _, on := range []string{"1", "true", "TRUE", "True", " true "} {
			t.Setenv("CCWRAP_CAPTURE_BODIES", on)
			got, err := parseLaunchArgs([]string{"-p", "hello"})
			if err != nil {
				t.Fatalf("env=%q: %v", on, err)
			}
			if !got.CaptureBodies {
				t.Fatalf("env=%q: expected CaptureBodies=true", on)
			}
		}
		for _, off := range []string{"", "0", "false", "no", "off"} {
			t.Setenv("CCWRAP_CAPTURE_BODIES", off)
			got, err := parseLaunchArgs([]string{"-p", "hello"})
			if err != nil {
				t.Fatalf("env=%q: %v", off, err)
			}
			if got.CaptureBodies {
				t.Fatalf("env=%q: expected CaptureBodies=false", off)
			}
		}
	})
}

// --capture-bodies is the canonical flag (captures request + response);
// --capture-request-bodies stays as a back-compat alias. Both drive the same
// CaptureBodies launch field.
func TestParseLaunchArgsCaptureBodiesCanonicalAndAlias(t *testing.T) {
	t.Run("--capture-bodies canonical sets it", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_BODIES", "")
		got, err := parseLaunchArgs([]string{"--capture-bodies", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.CaptureBodies {
			t.Fatalf("expected CaptureBodies=true with --capture-bodies")
		}
		want := []string{"-p", "hello"}
		if strings.Join(got.ClaudeArgs, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("ClaudeArgs = %#v, want %#v (flag must not leak to Claude)", got.ClaudeArgs, want)
		}
	})

	t.Run("--capture-bodies=false disables despite env=1", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_BODIES", "1")
		got, err := parseLaunchArgs([]string{"--capture-bodies=false", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if got.CaptureBodies {
			t.Fatalf("expected CaptureBodies=false: --capture-bodies=false must win over env=1")
		}
	})

	t.Run("--capture-request-bodies alias still works", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_BODIES", "")
		got, err := parseLaunchArgs([]string{"--capture-request-bodies", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.CaptureBodies {
			t.Fatalf("expected CaptureBodies=true with the --capture-request-bodies alias")
		}
	})
}

func TestParseLaunchArgsCaptureTelemetry(t *testing.T) {
	t.Run("default off when no flag and no env", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_TELEMETRY", "")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if got.CaptureTelemetry {
			t.Fatalf("expected CaptureTelemetry=false by default")
		}
	})

	t.Run("--capture-telemetry sets it", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_TELEMETRY", "")
		got, err := parseLaunchArgs([]string{"--capture-telemetry", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.CaptureTelemetry {
			t.Fatalf("expected CaptureTelemetry=true with --capture-telemetry")
		}
	})

	t.Run("CCWRAP_CAPTURE_TELEMETRY=1 enables when no flag", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_TELEMETRY", "1")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.CaptureTelemetry {
			t.Fatalf("expected CaptureTelemetry=true when CCWRAP_CAPTURE_TELEMETRY=1")
		}
	})

	t.Run("--capture-telemetry=false overrides env", func(t *testing.T) {
		t.Setenv("CCWRAP_CAPTURE_TELEMETRY", "1")
		got, err := parseLaunchArgs([]string{"--capture-telemetry=false", "-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if got.CaptureTelemetry {
			t.Fatalf("expected CaptureTelemetry=false: --capture-telemetry=false must override CCWRAP_CAPTURE_TELEMETRY=1")
		}
	})
}

func TestParseLaunchArgsNativeTLS(t *testing.T) {
	// native-tls fingerprint mirroring is ON by default; there is no CLI flag.
	// The only control is a hidden env kill-switch CCWRAP_NATIVE_TLS=0 (or
	// false/no/off) for the rare case utls misbehaves on a Go/undici combo.
	t.Run("default ON when no env", func(t *testing.T) {
		t.Setenv("CCWRAP_NATIVE_TLS", "")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.NativeTLS {
			t.Fatalf("expected NativeTLS=true by default (native-tls is on unless killed)")
		}
	})

	for _, off := range []string{"0", "false", "no", "off"} {
		t.Run("kill-switch CCWRAP_NATIVE_TLS="+off+" disables", func(t *testing.T) {
			t.Setenv("CCWRAP_NATIVE_TLS", off)
			got, err := parseLaunchArgs([]string{"-p", "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if got.NativeTLS {
				t.Fatalf("expected NativeTLS=false when CCWRAP_NATIVE_TLS=%q", off)
			}
		})
	}

	t.Run("CCWRAP_NATIVE_TLS=1 keeps it on", func(t *testing.T) {
		t.Setenv("CCWRAP_NATIVE_TLS", "1")
		got, err := parseLaunchArgs([]string{"-p", "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.NativeTLS {
			t.Fatalf("expected NativeTLS=true when CCWRAP_NATIVE_TLS=1")
		}
	})
}

func TestLoadNativeTLSHello(t *testing.T) {
	fixture := "../../internal/supervisor/testdata/undici_clienthello.bin"
	if b, err := loadNativeTLSHello("", true); b != nil || err != nil {
		t.Errorf("unset must be (nil,nil), got (%v,%v)", b, err)
	}
	if b, err := loadNativeTLSHello(fixture, true); err != nil || len(b) == 0 {
		t.Errorf("valid undici .bin must load, got err=%v len=%d", err, len(b))
	}
	if _, err := loadNativeTLSHello(fixture, false); err == nil {
		t.Error("CCWRAP_NATIVE_TLS_HELLO with native-tls disabled must error (mutually exclusive)")
	}
	if _, err := loadNativeTLSHello("/no/such/file.bin", true); err == nil {
		t.Error("missing file must error")
	}
}

// TestNativeTLSDisabledWarning locks the danger notice: an explicit off value
// yields a non-empty warning that names the risk (DISABLED); on/default yields "".
func TestNativeTLSDisabledWarning(t *testing.T) {
	for _, on := range []string{"", "1", "true", "yes", "on"} {
		if w := nativeTLSDisabledWarning(on); w != "" {
			t.Errorf("CCWRAP_NATIVE_TLS=%q is enabled — want no warning, got %q", on, w)
		}
	}
	for _, off := range []string{"0", "false", "no", "off"} {
		w := nativeTLSDisabledWarning(off)
		if w == "" {
			t.Errorf("CCWRAP_NATIVE_TLS=%q disables mirroring — want a danger warning, got \"\"", off)
		}
		if !strings.Contains(w, "DISABLED") || !strings.Contains(w, "non-native") {
			t.Errorf("warning for %q must name the risk (DISABLED/non-native), got %q", off, w)
		}
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and
// returns everything written. Mirrors doctor_test.go's stdout pattern.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data)
}

// captureStdout is the stdout twin (printStatus / doctorCommand write stdout).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data)
}

func mustContainAll(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("output missing %q\n---\n%s", sub, s)
		}
	}
}

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Fatalf("output should not contain %q\n---\n%s", sub, s)
	}
}

func longestVisibleLine(s string) int {
	max := 0
	for _, ln := range strings.Split(s, "\n") {
		if n := len([]rune(ln)); n > max {
			max = n
		}
	}
	return max
}

func TestPrintLaunchSummaryStructure(t *testing.T) {
	out := captureStderr(t, func() {
		printLaunchSummary(
			"74c5fcaf812ff6d5",
			"http://127.0.0.1:50094",
			"http://50.18.84.244:3000",
			model.RouteSourceInheritedEnv,
			model.RouteClassThirdPartyHidden,
			model.AuthModeOverrideBearer,
			model.AuthSourceAnthropicToken,
			model.AuthPolicyCCWRAPOverrideFailClosed,
			model.AuthBootstrapPlaceholderActive,
			model.AuthBootstrapKindBearer,
			"", "", "",
			modelalias.Config{},
			upstreamheaders.Config{},
			7374,
			"", "", "",
		)
	})
	mustContainAll(t, out,
		"ccwrap", "ready",
		"CCWRAP holds your gateway",       // posture sentence (wrap-tolerant — "credentials" may land on next line)
		"session", "74c5fcaf", "pid 7374", // 8-char id + merged pid
		"upstream", "Gateway", // HumanRouteClass
		"auth", "CCWRAP-owned · fail-closed", "placeholder injected",
		"inspect", "ccwrap dashboard",
	)
	mustNotContain(t, out, "74c5fcaf812ff6d5") // full id must not appear
	mustNotContain(t, out, "trust")            // trust line removed
	mustNotContain(t, out, "third_party_hidden")
	if n := longestVisibleLine(out); n > 80 {
		t.Fatalf("hero must wrap: longest line %d runes (>80)\n%s", n, out)
	}

	// Multi-alias detail: keys sorted → "a" first.
	out2 := captureStderr(t, func() {
		printLaunchSummary(
			"abc123def456", "http://127.0.0.1:1", "http://h:1",
			model.RouteSourceInheritedEnv, model.RouteClassThirdPartyHidden,
			model.AuthModeOverrideBearer, model.AuthSourceAnthropicToken,
			model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive,
			model.AuthBootstrapKindBearer, "", "", "",
			modelalias.Config{Forward: map[string]string{"a": "x", "b": "y"}},
			upstreamheaders.Config{}, 1,
			"", "", "",
		)
	})
	mustContainAll(t, out2, "2 aliases · a → x, +1 more")

	// --- launch-banner refinements: a single `inspect` row (no
	// duplicate `proxy` URL row), auth de-dup + none-suppression + no
	// scheme prefix, and Title-case provenance. The "models none" form
	// is intentionally not used — `0 aliases` is the consistent
	// zero-of-the-family form parallel to `1 alias`/`N aliases`; the row
	// counts configured alias RULES.
	//
	// Table-driven over the THREE real preflight-resolved combos that
	// produced the three banners observed in the field (the gateway
	// launcher passes l.pre.{RouteClass,AuthMode,AuthPolicy,
	// AuthBootstrap,…} — these are those exact combos, not fictional).
	// captureStderr runs the real printLaunchSummary (no stubs); each
	// case asserts the EXACT rendered `auth`/`models` rows (bounded by
	// \n so a value like "Passthrough · Passthrough" or a trailing
	// " · none" fails) — not loose token presence. ---
	cases := []struct {
		name                       string
		routeSrc                   model.RouteSource
		routeClass                 model.RouteClass
		authMode                   model.AuthMode
		authSrc                    model.AuthSource
		authPol                    model.AuthPolicy
		boot                       model.AuthBootstrap
		bootKind                   model.AuthBootstrapKind
		url                        string
		wantAuthRow, wantModelsRow string
	}{
		{
			name: "gateway-hidden", routeSrc: model.RouteSourceInheritedEnv, routeClass: model.RouteClassThirdPartyHidden,
			authMode: model.AuthModeOverrideBearer, authSrc: model.AuthSourceAnthropicToken,
			authPol: model.AuthPolicyCCWRAPOverrideFailClosed, boot: model.AuthBootstrapPlaceholderActive, bootKind: model.AuthBootstrapKindBearer,
			url:         "http://127.0.0.1:58001",
			wantAuthRow: "  auth      CCWRAP-owned · fail-closed · placeholder injected", wantModelsRow: "  models    0 aliases",
		},
		{
			name: "first-party-passthrough", routeSrc: model.RouteSourceFallback, routeClass: model.RouteClassFirstParty,
			authMode: model.AuthModePassthrough, authSrc: model.AuthSourceClaudeOAuthToken,
			authPol: model.AuthPolicyFirstPartyPassthrough, boot: model.AuthBootstrapNotNeeded, bootKind: model.AuthBootstrapKindNone,
			url:         "http://127.0.0.1:58002",
			wantAuthRow: "  auth      Passthrough", wantModelsRow: "  models    0 aliases",
		},
		{
			name: "first-party-ccwrap-owned", routeSrc: model.RouteSourceFallback, routeClass: model.RouteClassFirstParty,
			authMode: model.AuthModeOverrideBearer, authSrc: model.AuthSourceAnthropicToken,
			authPol: model.AuthPolicyCCWRAPOverride, boot: model.AuthBootstrapNotNeeded, bootKind: model.AuthBootstrapKindNone,
			url:         "http://127.0.0.1:58003",
			wantAuthRow: "  auth      CCWRAP-owned", wantModelsRow: "  models    0 aliases",
		},
	}
	for _, c := range cases {
		got := captureStderr(t, func() {
			printLaunchSummary(
				"af04affe1234ffff", c.url, "http://50.18.84.244:3000",
				c.routeSrc, c.routeClass, c.authMode, c.authSrc, c.authPol, c.boot, c.bootKind,
				"", "", "", modelalias.Config{}, upstreamheaders.Config{}, 31407,
				"", "", "",
			)
		})
		// EXACT rows (\n-bounded → pins the full value, catching a
		// "Passthrough · Passthrough" doubling, a trailing " · none",
		// or a "Bearer token · " scheme prefix). models is `0 aliases`
		// — asserted exactly here, not negated.
		if !strings.Contains(got, "\n"+c.wantAuthRow+"\n") {
			t.Fatalf("[%s] auth row must be exactly %q\n---\n%s", c.name, c.wantAuthRow, got)
		}
		if !strings.Contains(got, "\n"+c.wantModelsRow+"\n") {
			t.Fatalf("[%s] models row must be exactly %q\n---\n%s", c.name, c.wantModelsRow, got)
		}
		// The URL appears exactly once (single `inspect` row), and
		// there is no `proxy` label row.
		if n := strings.Count(got, c.url); n != 1 {
			t.Fatalf("[%s] URL must appear exactly once — one `inspect` row, no `proxy` row; got %d\n%s", c.name, n, got)
		}
		mustNotContain(t, got, "\n  proxy")
	}

	// An env-sourced route renders the humanizer's native
	// Title-case "From environment" (consistent with the egress
	// source), never the previously-lowercased "from environment".
	mustContainAll(t, out, "· From environment")
	mustNotContain(t, out, "from environment")
}

func TestPrintStatusStructure(t *testing.T) {
	sess := &model.Session{
		ID: "74c5fcaf812ff6d5", State: model.StateActive, ClaudePID: 7374,
		RouteClass: model.RouteClassThirdPartyHidden, ExactUpstreamHost: "50.18.84.244:3000",
		ExactUpstreamBase: "http://50.18.84.244:3000",
		AuthMode:          model.AuthModeOverrideBearer, AuthSource: model.AuthSourceAnthropicToken,
		AuthPolicy:    model.AuthPolicyCCWRAPOverrideFailClosed,
		AuthBootstrap: model.AuthBootstrapPlaceholderActive, AuthBootstrapKind: model.AuthBootstrapKindBearer,
		ModelAliasCount: 1, ModelAliasMode: model.ModelAliasRewrite,
		RecentRequestCount: 9, RecentErrorCount: 0,
	}
	out := captureStdout(t, func() {
		printStatus(
			[]model.DiscoveredSession{{Reachable: true}},
			[]*model.Session{sess},
		)
	})
	mustContainAll(t, out,
		"CCWRAP Status", "1 active", "0 stale",
		"74c5fcaf", "active",
		"CCWRAP holds your gateway credentials", // posture
		"proxy", "route", "Gateway", "auth", "CCWRAP-owned · fail-closed",
		"models", "1 alias", // count-only for status
		"traffic", "9 req", "0 err",
	)
	mustNotContain(t, out, "74c5fcaf812ff6d5")
	mustNotContain(t, out, "ccwrap_override_fail_closed")
}

func TestStopGCMessagesUseShortID(t *testing.T) {
	if got := stopMessage("74c5fcaf812ff6d5"); !strings.Contains(got, "74c5fcaf") || strings.Contains(got, "74c5fcaf812ff6d5") {
		t.Fatalf("stop message must use 8-char id, got %q", got)
	}
	got := gcRemovedLine([]string{"74c5fcaf812ff6d5", "8a92e018aa11bb22"})
	if strings.Contains(got, "812ff6d5") || strings.Contains(got, "aa11bb22") {
		t.Fatalf("gc line must use 8-char ids, got %q", got)
	}
	// Behavior unchanged: still one "removed <id>" per id.
	if strings.Count(got, "removed ") != 2 {
		t.Fatalf("gc must keep one line per removed id, got %q", got)
	}
}

// TestSweepOrphanSessionsRemovesDeadBodiesDir covers the
// crash-safe startup sweep: sweepOrphanSessions (invoked at the top of
// runClaude) reaps the runtime dir — including the per-session bodies/
// subdir — of a session whose supervisor is no longer live, while a
// live session's bodies/ is left intact. It exercises the real startup
// entry helper and the real liveness signal (empty start-token + a live
// SupervisorPID ⇒ not stale; invalid pid ⇒ dead).
func TestSweepOrphanSessionsRemovesDeadBodiesDir(t *testing.T) {
	tmp := t.TempDir()
	paths := app.Paths{RuntimeDir: filepath.Join(tmp, "run"), StateDir: filepath.Join(tmp, "state")}

	deadID := "sess-dead"
	deadDir := paths.SessionDir(deadID)
	deadBodies := filepath.Join(deadDir, "bodies")
	if err := os.MkdirAll(deadBodies, 0o700); err != nil {
		t.Fatalf("create dead bodies dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deadBodies, "x.json"), []byte(`{"b":1}`), 0o600); err != nil {
		t.Fatalf("write dead body file: %v", err)
	}
	if err := manifest.Write(manifest.Path(deadDir), model.SessionManifest{
		SessionID:     deadID,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		State:         model.StateActive,
		SupervisorPID: 0, // invalid pid ⇒ session no longer live
		ControlSocket: filepath.Join(deadDir, "control.sock"),
	}); err != nil {
		t.Fatalf("write dead manifest: %v", err)
	}

	liveID := "sess-live"
	liveDir := paths.SessionDir(liveID)
	liveBodies := filepath.Join(liveDir, "bodies")
	if err := os.MkdirAll(liveBodies, 0o700); err != nil {
		t.Fatalf("create live bodies dir: %v", err)
	}
	liveBodyFile := filepath.Join(liveBodies, "y.json")
	if err := os.WriteFile(liveBodyFile, []byte(`{"b":2}`), 0o600); err != nil {
		t.Fatalf("write live body file: %v", err)
	}
	if err := manifest.Write(manifest.Path(liveDir), model.SessionManifest{
		SessionID:     liveID,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		State:         model.StateActive,
		SupervisorPID: os.Getpid(), // empty token + live pid ⇒ still live
		ControlSocket: filepath.Join(liveDir, "control.sock"),
	}); err != nil {
		t.Fatalf("write live manifest: %v", err)
	}

	sweepOrphanSessions(paths)

	if _, err := os.Stat(deadBodies); !os.IsNotExist(err) {
		t.Fatalf("dead session bodies/ must be swept on startup, stat err = %v", err)
	}
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("dead session dir must be removed on startup, stat err = %v", err)
	}
	if _, err := os.Stat(liveBodyFile); err != nil {
		t.Fatalf("live session body file must be untouched: %v", err)
	}
}

func TestParseLaunchArgsProfileFlag(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--profile", "gw-a", "-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "gw-a" {
		t.Fatalf("Profile = %q, want gw-a", got.Profile)
	}
	want := []string{"-p", "hello"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

func TestParseLaunchArgsProfileInlineValue(t *testing.T) {
	got, err := parseLaunchArgs([]string{"--profile=AcmeGW"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "AcmeGW" {
		t.Fatalf("Profile = %q, want AcmeGW", got.Profile)
	}
}

func TestParseLaunchArgsProfileMissingValueFails(t *testing.T) {
	if _, err := parseLaunchArgs([]string{"--profile"}); err == nil {
		t.Fatal("expected --profile with no value to fail")
	}
}

func TestParseLaunchArgsProfileStopsBeforeClaudeArgs(t *testing.T) {
	got, err := parseLaunchArgs([]string{"-p", "hello", "--profile", "belongs-to-claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "" {
		t.Fatalf("CCWRAP must not consume --profile after Claude args start, got %q", got.Profile)
	}
	want := []string{"-p", "hello", "--profile", "belongs-to-claude"}
	if !sameStrings(got.ClaudeArgs, want) {
		t.Fatalf("ClaudeArgs = %#v, want %#v", got.ClaudeArgs, want)
	}
}

// T-parseLaunchArgs-no-init-bare — bare --no-init sets NoInit=true.
func TestParseLaunchArgs_NoInitBare(t *testing.T) {
	out, err := parseLaunchArgs([]string{"--no-init"})
	if err != nil {
		t.Fatalf("parseLaunchArgs: %v", err)
	}
	if !out.NoInit {
		t.Fatalf("NoInit = false; want true")
	}
}

// T-parseLaunchArgs-no-init-value — --no-init=true|false uses parseBoolLaunchFlag.
func TestParseLaunchArgs_NoInitValue(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"--no-init=true", true},
		{"--no-init=false", false},
		{"--no-init=1", true},
		{"--no-init=0", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			out, err := parseLaunchArgs([]string{c.in})
			if err != nil {
				t.Fatalf("parseLaunchArgs(%q): %v", c.in, err)
			}
			if out.NoInit != c.want {
				t.Errorf("NoInit = %v; want %v", out.NoInit, c.want)
			}
		})
	}
}

func TestResolveLaunchProfileSelectsAndBuildsInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	// "anthropic" omits auth → ccwrap does not own auth (replaces old mode=passthrough).
	if err := os.WriteFile(path, []byte(`{
	  "default": "anthropic",
	  "profiles": {
	    "anthropic": {"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}},
	    "gw-a": {"provider":"AcmeGW","base_url":"https://gw.acme.example","auth":{"mode":"ccwrap_bearer","key_env":"ACME_TOKEN"},"model_aliases":{"claude-sonnet-4-5":"gpt-5.5"},"egress":{"mode":"http","url":"http://127.0.0.1:10800"}}
	  }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	in, err := resolveLaunchProfile(path, "gw-a")
	if err != nil {
		t.Fatalf("resolveLaunchProfile(gw-a): %v", err)
	}
	if in == nil || in.Name != "gw-a" || in.BaseURL != "https://gw.acme.example" || in.Auth == nil || in.Auth.KeyEnv != "ACME_TOKEN" {
		t.Fatalf("ProfileInput = %#v", in)
	}
	if in.EgressMode != "http" || in.EgressURL != "http://127.0.0.1:10800" {
		t.Fatalf("egress overlay = %#v", in)
	}

	in2, err := resolveLaunchProfile(path, "")
	if err != nil {
		t.Fatalf("resolveLaunchProfile(\"\"): %v", err)
	}
	if in2 == nil || in2.Name != "anthropic" {
		t.Fatalf("persisted default ProfileInput = %#v", in2)
	}

	in3, err := resolveLaunchProfile(filepath.Join(dir, "absent.json"), "")
	if err != nil {
		t.Fatalf("missing file must be inherit-env, got err %v", err)
	}
	if in3 != nil {
		t.Fatalf("missing file + no --profile must be nil overlay, got %#v", in3)
	}

	if _, err := resolveLaunchProfile(filepath.Join(dir, "absent.json"), "gw-a"); err == nil {
		t.Fatal("explicit --profile with no profiles.json must error")
	}

	_ = profiles.InheritEnv
}

func TestResolveLaunchProfileHonorsCCWRAPProfileEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(`{
	  "default": "anthropic",
	  "profiles": {
	    "anthropic": {"provider":"Anthropic","base_url":"https://api.anthropic.com","egress":{"mode":"inherit"}},
	    "gw-a": {"provider":"AcmeGW","base_url":"https://gw.acme.example","auth":{"mode":"ccwrap_bearer","key_env":"ACME_TOKEN"},"egress":{"mode":"inherit"}}
	  }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Tier 2: CCWRAP_PROFILE selects the profile when no --profile flag is passed.
	t.Setenv("CCWRAP_PROFILE", "gw-a")
	in, err := resolveLaunchProfile(path, "")
	if err != nil {
		t.Fatalf("resolveLaunchProfile(\"\") with CCWRAP_PROFILE=gw-a: %v", err)
	}
	if in == nil || in.Name != "gw-a" {
		t.Fatalf("CCWRAP_PROFILE env must select gw-a, got %#v", in)
	}

	// Tier 1: an explicit --profile (non-empty requested) overrides the env.
	in2, err := resolveLaunchProfile(path, "anthropic")
	if err != nil {
		t.Fatalf("resolveLaunchProfile(anthropic) with CCWRAP_PROFILE=gw-a: %v", err)
	}
	if in2 == nil || in2.Name != "anthropic" {
		t.Fatalf("explicit --profile must override CCWRAP_PROFILE, got %#v", in2)
	}

	// Tier 3: empty CCWRAP_PROFILE falls back to the persisted file.default.
	t.Setenv("CCWRAP_PROFILE", "")
	in3, err := resolveLaunchProfile(path, "")
	if err != nil {
		t.Fatalf("resolveLaunchProfile(\"\") with CCWRAP_PROFILE unset: %v", err)
	}
	if in3 == nil || in3.Name != "anthropic" {
		t.Fatalf("empty CCWRAP_PROFILE must fall back to file.default anthropic, got %#v", in3)
	}
}

// TestPrintLaunchSummaryMissingAuth_CaseA — banner shows the missing-env
// marker on the auth row + the recovery hint at the bottom. Launch now
// SUCCEEDS even when ResolveProfile yields AuthBootstrap=Missing, so this
// banner is what users see when their profile's key_env was rotated
// out from under them.
func TestPrintLaunchSummaryMissingAuth_CaseA(t *testing.T) {
	out := captureStderr(t, func() {
		printLaunchSummary(
			"abc123def456", "http://127.0.0.1:1", "http://gw.acme.example:1",
			model.RouteSourceInheritedEnv, model.RouteClassThirdPartyHidden,
			model.AuthModePassthrough, model.AuthSourceNone,
			model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapMissing,
			model.AuthBootstrapKindNone, "", "", "",
			modelalias.Config{}, upstreamheaders.Config{}, 1,
			"local", "Anthropic", "ANTHROPIC_AUTH_TOKEN",
		)
	})
	mustContainAll(t, out,
		"⚠ MISSING",
		`"local"`,
		"$ANTHROPIC_AUTH_TOKEN",
		"Requests will fail until you",
		"--profile inherit-env",
	)
}

// TestPrintLaunchSummaryMissingAuth_CaseB — TPH route + no env named.
// Banner uses the generic "no auth source configured" phrasing on the
// auth row, and the recovery hint suggests editing profiles.json.
func TestPrintLaunchSummaryMissingAuth_CaseB(t *testing.T) {
	out := captureStderr(t, func() {
		printLaunchSummary(
			"abc123def456", "http://127.0.0.1:1", "http://gw.acme.example:1",
			model.RouteSourceInheritedEnv, model.RouteClassThirdPartyHidden,
			model.AuthModePassthrough, model.AuthSourceNone,
			model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapMissing,
			model.AuthBootstrapKindNone, "", "", "",
			modelalias.Config{}, upstreamheaders.Config{}, 1,
			"foo", "AcmeGW", "",
		)
	})
	mustContainAll(t, out,
		"⚠ MISSING",
		`"foo"`,
		"no auth source configured",
		"edit profiles.json",
		"Requests will fail until you",
	)
	// Case B specifically must NOT mention a $env token — there is none to name.
	if strings.Contains(out, "needs $") {
		t.Errorf("Case B banner must not say 'needs $...'; got %s", out)
	}
}

func TestPrintLaunchSummaryProfileCue(t *testing.T) {
	out := captureStderr(t, func() {
		printLaunchSummary(
			"abc123def456", "http://127.0.0.1:1", "http://gw.acme.example:1",
			model.RouteSourceInheritedEnv, model.RouteClassThirdPartyHidden,
			model.AuthModeOverrideBearer, model.AuthSourceAnthropicToken,
			model.AuthPolicyCCWRAPOverrideFailClosed, model.AuthBootstrapPlaceholderActive,
			model.AuthBootstrapKindBearer, "", "", "",
			modelalias.Config{}, upstreamheaders.Config{}, 1,
			"gw-a", "AcmeGW", "",
		)
	})
	mustContainAll(t, out, "profile", "gw-a", "AcmeGW")
	mustNotContain(t, out, "sk-")
	mustNotContain(t, out, "ACME_TOKEN")

	out2 := captureStderr(t, func() {
		printLaunchSummary(
			"abc123def456", "http://127.0.0.1:1", "http://api.anthropic.com:1",
			model.RouteSourceFallback, model.RouteClassFirstParty,
			model.AuthModePassthrough, model.AuthSourceNone,
			model.AuthPolicyFirstPartyPassthrough, model.AuthBootstrapNotNeeded,
			model.AuthBootstrapKindNone, "", "", "",
			modelalias.Config{}, upstreamheaders.Config{}, 1,
			"", "", "",
		)
	})
	for _, ln := range strings.Split(out2, "\n") {
		trimmed := strings.TrimSpace(stripANSI(ln))
		if strings.HasPrefix(trimmed, "profile ") || trimmed == "profile" {
			t.Fatalf("inherit-env must NOT print a profile row; got line %q\n---\n%s", ln, out2)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Helper: read snapshot content from the returned opts and assert it parses to expected map.
func mustForwardFromAliasContent(t *testing.T, data []byte, label string) map[string]string {
	t.Helper()
	if len(data) == 0 {
		t.Fatalf("%s: content snapshot is empty", label)
	}
	m, err := modelalias.ParseJSON(data, label)
	if err != nil {
		t.Fatalf("%s: ParseJSON failed: %v", label, err)
	}
	return m
}

func mustHeadersFromContent(t *testing.T, data []byte, label string) map[string]string {
	t.Helper()
	if len(data) == 0 {
		t.Fatalf("%s: content snapshot is empty", label)
	}
	m, err := upstreamheaders.ParseJSON(data, label)
	if err != nil {
		t.Fatalf("%s: ParseJSON failed: %v", label, err)
	}
	return m
}

func TestComposeLaunch_SnapshotsCLIModelAliasFile(t *testing.T) {
	dir := t.TempDir()
	aliasFile := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(aliasFile, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	opts := preflight.Options{
		Upstream:                      "https://gw.example/v1",
		ModelAliasFile:                aliasFile,
		ParentEnv:                     append(os.Environ(), "ANTHROPIC_API_KEY=gw-key"),
		AllowProviderModelPassthrough: true,
	}
	inspect, pre, outOpts, err := composeLaunch(opts)
	if err != nil {
		t.Fatalf("composeLaunch err: %v", err)
	}
	if inspect == nil {
		t.Fatal("inspect is nil")
	}
	if pre == nil {
		t.Fatal("pre is nil")
	}
	got := mustForwardFromAliasContent(t, outOpts.ModelAliasExplicitFileContent, "ModelAliasExplicitFileContent")
	if got["k"] != "v" {
		t.Errorf("snapshot Forward[k] = %q, want v", got["k"])
	}
	// Delete the seed file post-launch.
	if err := os.Remove(aliasFile); err != nil {
		t.Fatalf("delete seed: %v", err)
	}
	// Verify the resolver uses the snapshot (mid-session "switch" semantic):
	postSwitchPre, err := preflight.ResolveProfile(outOpts, inspect)
	if err != nil {
		t.Fatalf("post-deletion ResolveProfile err: %v", err)
	}
	if postSwitchPre.ModelAlias.Forward["k"] != "v" {
		t.Errorf("Forward[k] = %q, want v (snapshot must survive deletion)",
			postSwitchPre.ModelAlias.Forward["k"])
	}
}

func TestComposeLaunch_SnapshotsEnvModelAliasFile(t *testing.T) {
	dir := t.TempDir()
	aliasFile := filepath.Join(dir, "env-aliases.json")
	if err := os.WriteFile(aliasFile, []byte(`{"k2":"v2"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	opts := preflight.Options{
		Upstream: "https://gw.example/v1",
		ParentEnv: append(os.Environ(),
			"ANTHROPIC_API_KEY=gw-key",
			"CCWRAP_MODEL_ALIASES_FILE="+aliasFile,
		),
		AllowProviderModelPassthrough: true,
	}
	inspect, _, outOpts, err := composeLaunch(opts)
	if err != nil {
		t.Fatalf("composeLaunch err: %v", err)
	}
	got := mustForwardFromAliasContent(t, outOpts.ModelAliasEnvFileContent, "ModelAliasEnvFileContent")
	if got["k2"] != "v2" {
		t.Errorf("env snapshot Forward[k2] = %q, want v2", got["k2"])
	}
	if err := os.Remove(aliasFile); err != nil {
		t.Fatalf("delete seed: %v", err)
	}
	postSwitchPre, err := preflight.ResolveProfile(outOpts, inspect)
	if err != nil {
		t.Fatalf("post-deletion ResolveProfile err: %v", err)
	}
	if postSwitchPre.ModelAlias.Forward["k2"] != "v2" {
		t.Errorf("Forward[k2] = %q, want v2 (env-snapshot must survive deletion)",
			postSwitchPre.ModelAlias.Forward["k2"])
	}
}

func TestComposeLaunch_SnapshotsCLIUpstreamHeaderFile(t *testing.T) {
	dir := t.TempDir()
	hdrFile := filepath.Join(dir, "headers.json")
	// Use a canonical-form header name to avoid http.CanonicalHeaderKey rewrite surprises.
	if err := os.WriteFile(hdrFile, []byte(`{"X-Gateway-Tenant":"acme"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	opts := preflight.Options{
		Upstream:           "https://gw.example/v1",
		UpstreamHeaderFile: hdrFile,
		ParentEnv:          append(os.Environ(), "ANTHROPIC_API_KEY=gw-key"),
	}
	inspect, _, outOpts, err := composeLaunch(opts)
	if err != nil {
		t.Fatalf("composeLaunch err: %v", err)
	}
	got := mustHeadersFromContent(t, outOpts.UpstreamHeaderExplicitFileContent, "UpstreamHeaderExplicitFileContent")
	if got["X-Gateway-Tenant"] != "acme" {
		t.Errorf("snapshot Headers[X-Gateway-Tenant] = %q, want acme", got["X-Gateway-Tenant"])
	}
	if err := os.Remove(hdrFile); err != nil {
		t.Fatalf("delete seed: %v", err)
	}
	postSwitchPre, err := preflight.ResolveProfile(outOpts, inspect)
	if err != nil {
		t.Fatalf("post-deletion ResolveProfile err: %v", err)
	}
	if postSwitchPre.UpstreamHeaders.Headers["X-Gateway-Tenant"] != "acme" {
		t.Errorf("Headers[X-Gateway-Tenant] = %q, want acme (snapshot must survive deletion)",
			postSwitchPre.UpstreamHeaders.Headers["X-Gateway-Tenant"])
	}
}

func TestComposeLaunch_SnapshotsEnvUpstreamHeaderFile(t *testing.T) {
	dir := t.TempDir()
	hdrFile := filepath.Join(dir, "env-headers.json")
	if err := os.WriteFile(hdrFile, []byte(`{"X-Gateway-Key":"k1"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	opts := preflight.Options{
		Upstream: "https://gw.example/v1",
		ParentEnv: append(os.Environ(),
			"ANTHROPIC_API_KEY=gw-key",
			"CCWRAP_UPSTREAM_HEADERS_FILE="+hdrFile,
		),
	}
	inspect, _, outOpts, err := composeLaunch(opts)
	if err != nil {
		t.Fatalf("composeLaunch err: %v", err)
	}
	got := mustHeadersFromContent(t, outOpts.UpstreamHeaderEnvFileContent, "UpstreamHeaderEnvFileContent")
	if got["X-Gateway-Key"] != "k1" {
		t.Errorf("env snapshot Headers[X-Gateway-Key] = %q, want k1", got["X-Gateway-Key"])
	}
	if err := os.Remove(hdrFile); err != nil {
		t.Fatalf("delete seed: %v", err)
	}
	postSwitchPre, err := preflight.ResolveProfile(outOpts, inspect)
	if err != nil {
		t.Fatalf("post-deletion ResolveProfile err: %v", err)
	}
	if postSwitchPre.UpstreamHeaders.Headers["X-Gateway-Key"] != "k1" {
		t.Errorf("Headers[X-Gateway-Key] = %q, want k1 (env-snapshot must survive deletion)",
			postSwitchPre.UpstreamHeaders.Headers["X-Gateway-Key"])
	}
}

// Zero-touch: when no file-backed input is set, composeLaunch leaves all 4 content
// fields empty (today's disk-read fallback path). This is the back-compat guard.
func TestComposeLaunch_NoFileInputs_LeavesContentEmpty(t *testing.T) {
	opts := preflight.Options{
		ParentEnv: os.Environ(),
	}
	_, _, outOpts, err := composeLaunch(opts)
	if err != nil {
		t.Fatalf("composeLaunch err: %v", err)
	}
	if len(outOpts.ModelAliasExplicitFileContent) != 0 {
		t.Error("ModelAliasExplicitFileContent should be empty when no CLI file set")
	}
	if len(outOpts.ModelAliasEnvFileContent) != 0 {
		t.Error("ModelAliasEnvFileContent should be empty when no env file set")
	}
	if len(outOpts.UpstreamHeaderExplicitFileContent) != 0 {
		t.Error("UpstreamHeaderExplicitFileContent should be empty when no CLI file set")
	}
	if len(outOpts.UpstreamHeaderEnvFileContent) != 0 {
		t.Error("UpstreamHeaderEnvFileContent should be empty when no env file set")
	}
}

// composeLaunch outputs + LaunchContext construction yields a
// LaunchContext that carries the file-content snapshots into the supervisor.
// The launcher's actual supervisor.New call is exercised by existing integration
// tests; this unit test guards the LaunchContext-construction code path.
func TestComposeLaunch_LaunchContextCarriesSnapshots(t *testing.T) {
	dir := t.TempDir()
	aliasFile := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(aliasFile, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	opts := preflight.Options{
		Upstream:                      "https://gw.example/v1",
		ModelAliasFile:                aliasFile,
		ParentEnv:                     append(os.Environ(), "ANTHROPIC_API_KEY=gw-key"),
		AllowProviderModelPassthrough: true,
	}
	inspect, pre, outOpts, err := composeLaunch(opts)
	if err != nil {
		t.Fatalf("composeLaunch err: %v", err)
	}
	// Construct LaunchContext from composeLaunch outputs (the wiring shape).
	lc := &supervisor.LaunchContext{
		Options:    outOpts,
		Inspection: inspect,
		Launch:     pre,
	}
	if lc.Inspection == nil {
		t.Fatal("lc.Inspection is nil")
	}
	if lc.Launch == nil {
		t.Fatal("lc.Launch is nil")
	}
	if len(lc.Options.ModelAliasExplicitFileContent) == 0 {
		t.Errorf("lc.Options.ModelAliasExplicitFileContent should be populated")
	}
}

// T-trigger-off-flag — when noInit=true (CLI flag), migration is disabled
// regardless of env.
func TestMigrationDisabled_Flag(t *testing.T) {
	if !migrationDisabled([]string{"CCWRAP_NO_INIT=0"}, true) {
		t.Errorf("migrationDisabled(env=0, flag=true) = false; want true")
	}
}

// T-trigger-off-env — CCWRAP_NO_INIT accept-set per truthyEnv in main.go:
//
//	"1" / "true" / "TRUE" / "True" → blocks
//	"yes" / "0" / "" / "no" / "false" → does NOT block
func TestMigrationDisabled_EnvAcceptSet(t *testing.T) {
	cases := []struct {
		env  string
		want bool // want disabled?
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"0", false},
		{"yes", false},
		{"no", false},
		{"false", false},
		{"", false},
	}
	for _, c := range cases {
		c := c
		t.Run("env="+c.env, func(t *testing.T) {
			parent := []string{"CCWRAP_NO_INIT=" + c.env}
			got := migrationDisabled(parent, false)
			if got != c.want {
				t.Errorf("migrationDisabled(env=%q, flag=false) = %v; want %v", c.env, got, c.want)
			}
		})
	}
}

// T-trigger-off-env-unset — CCWRAP_NO_INIT not in env at all → not disabled.
func TestMigrationDisabled_EnvUnset(t *testing.T) {
	if migrationDisabled([]string{"PATH=/usr/bin"}, false) {
		t.Errorf("migrationDisabled(no CCWRAP_NO_INIT, flag=false) = true; want false")
	}
}

// T-tty-prompt-typed — TTY (stub) returns user-typed name verbatim.
func TestResolveSeedName_TTYTyped(t *testing.T) {
	restore := stubIsTerminal(true)
	defer restore()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	go func() {
		defer w.Close()
		w.Write([]byte("alpha\n"))
	}()
	got := resolveSeedName(r)
	if got != "alpha" {
		t.Errorf("resolveSeedName = %q; want alpha", got)
	}
}

// T-tty-prompt-default — TTY (stub) with empty Enter returns "local".
func TestResolveSeedName_TTYDefault(t *testing.T) {
	restore := stubIsTerminal(true)
	defer restore()
	r, w, _ := os.Pipe()
	defer r.Close()
	go func() {
		defer w.Close()
		w.Write([]byte("\n"))
	}()
	got := resolveSeedName(r)
	if got != "local" {
		t.Errorf("resolveSeedName = %q; want local (default)", got)
	}
}

// T-tty-prompt-eof — TTY (stub) with EOF (Ctrl-D) returns "" — caller skips.
func TestResolveSeedName_TTYEOF(t *testing.T) {
	restore := stubIsTerminal(true)
	defer restore()
	r, w, _ := os.Pipe()
	defer r.Close()
	w.Close() // immediate EOF
	got := resolveSeedName(r)
	if got != "" {
		t.Errorf("resolveSeedName(EOF) = %q; want empty", got)
	}
}

// T-non-tty — non-TTY stub returns "local" directly without reading.
func TestResolveSeedName_NonTTY(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	r, w, _ := os.Pipe()
	defer r.Close()
	// Intentionally do NOT close w — confirms resolveSeedName does not block on read.
	defer w.Close()
	got := resolveSeedName(r)
	if got != "local" {
		t.Errorf("resolveSeedName(non-tty) = %q; want local", got)
	}
}

// stubIsTerminal swaps the package-level seam for the duration of a test.
// Returns the restore function so tests can defer cleanup.
func stubIsTerminal(isatty bool) func() {
	prev := isTerminalFn
	isTerminalFn = func(*os.File) bool { return isatty }
	return func() { isTerminalFn = prev }
}

// captureStderrFn redirects os.Stderr to a pipe for the duration of fn,
// returning the captured bytes. Mirrors the pattern used in other CCWRAP
// tests for stderr-only logging assertions.
func captureStderrFn(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 1024)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				break
			}
		}
		done <- string(buf)
	}()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

// T-trigger-on — happy path: profiles.json missing + non-default
// BASE_URL + ANTHROPIC_API_KEY present + non-TTY (so name defaults to
// "local") → profiles.json appears with the right shape.
func TestMaybeMigrateFromEnv_TriggerOn(t *testing.T) {
	restore := stubIsTerminal(false) // non-TTY → name = "local"
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=https://gw.example.com/",
		"ANTHROPIC_API_KEY=sk-fake-12345",
		"PATH=/usr/bin",
	}
	out := captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	if !strings.Contains(out, "seeded initial profile") {
		t.Errorf("stderr missing 'seeded initial profile' line; got: %s", out)
	}
	// profiles.json must now exist with the seeded shape.
	path := profiles.DefaultPath(dir)
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected profiles.json at %s: %v", path, err)
	}
	if !strings.Contains(string(blob), `"default": "local"`) {
		t.Errorf("profiles.json missing default=local; got: %s", string(blob))
	}
	// The seeded profile now carries the credential VALUE inline at
	// auth.key (and NOT auth.key_env — Key wins; key_env beside it would
	// be a dead field). profiles.json is owner-only (0600) and treated as
	// a secret-bearing file.
	if !strings.Contains(string(blob), `"key": "sk-fake-12345"`) {
		t.Errorf("profiles.json missing inline auth.key value; got: %s", string(blob))
	}
	if strings.Contains(string(blob), `"key_env"`) {
		t.Errorf("profiles.json should not carry a key_env field when auth.key is inline; got: %s", string(blob))
	}
}

// T-trigger-off-canonical — BASE_URL is canonical Anthropic → no migration.
func TestMaybeMigrateFromEnv_OffCanonical(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=https://api.anthropic.com/",
		"ANTHROPIC_API_KEY=sk-fake",
	}
	_ = captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	if _, err := os.Stat(profiles.DefaultPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected NotExist; got err=%v", err)
	}
}

// T-trigger-off-empty-base — BASE_URL unset → no migration.
func TestMaybeMigrateFromEnv_OffEmptyBase(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{"ANTHROPIC_API_KEY=sk-fake"}
	_ = captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	if _, err := os.Stat(profiles.DefaultPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected NotExist; got err=%v", err)
	}
}

// T-trigger-off-file-exists — profiles.json already exists → no migration.
func TestMaybeMigrateFromEnv_OffFileExists(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	path := profiles.DefaultPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdirp: %v", err)
	}
	seed := []byte(`{"default":"keep","profiles":{}}`)
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	parent := []string{
		"ANTHROPIC_BASE_URL=https://gw.example.com/",
		"ANTHROPIC_API_KEY=sk-fake",
	}
	_ = captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	blob, _ := os.ReadFile(path)
	if string(blob) != string(seed) {
		t.Errorf("profiles.json was modified; want unchanged. Got: %s", string(blob))
	}
}

// T-trigger-off-oauth-only — only CLAUDE_CODE_OAUTH_TOKEN set; neither
// ANTHROPIC_API_KEY nor ANTHROPIC_AUTH_TOKEN → no migration (poison-pill
// guard).
func TestMaybeMigrateFromEnv_OffOAuthOnly(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=https://gw.example.com/",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-fake",
	}
	_ = captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	if _, err := os.Stat(profiles.DefaultPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected NotExist (poison-pill guard); got err=%v", err)
	}
}

// T-malformed-baseurl-skip — BASE_URL is unparseable → stderr warn + skip.
func TestMaybeMigrateFromEnv_MalformedBaseURLSkip(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=://broken",
		"ANTHROPIC_API_KEY=sk-fake",
	}
	out := captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	if _, err := os.Stat(profiles.DefaultPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected NotExist; got err=%v", err)
	}
	if !strings.Contains(out, "malformed") {
		t.Errorf("stderr missing 'malformed' marker; got: %s", out)
	}
}

// T-write-fail-fallback — write fails (read-only parent dir) → stderr warn
// + skip; subsequent code path proceeds without panic.
//
// Adaptation note: profiles.DefaultPath returns filepath.Join(stateDir,
// "profiles.json") directly (no subdir), so the plan's "blocker file at
// dir/ccwrap" approach cannot block. Instead we chmod the stateDir to 0o555
// so WriteFile's OpenFile(O_CREATE) returns EACCES. We restore 0o755
// after capture so t.TempDir cleanup can rm the dir.
func TestMaybeMigrateFromEnv_WriteFailFallback(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	// Make stateDir read-only so WriteFile's OpenFile(O_CREATE) fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	defer os.Chmod(dir, 0o755) // restore so TempDir cleanup can remove it
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=https://gw.example.com/",
		"ANTHROPIC_API_KEY=sk-fake",
	}
	out := captureStderrFn(t, func() {
		// Must not panic.
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	// No usable profiles.json (write failed; file either absent or empty).
	if blob, err := os.ReadFile(profiles.DefaultPath(dir)); err == nil && len(blob) > 0 {
		t.Errorf("profiles.json unexpectedly readable with content: %s", string(blob))
	}
	// Per plan, exact stderr content is secondary; absence-of-panic plus
	// no usable profiles.json is the load-bearing assertion. Keep an
	// assertion-free read so the variable is referenced.
	_ = out
}

// T-postmigration-default-flows — end-to-end happy path: after
// maybeMigrateFromEnv writes a profile, the immediately-following
// resolveLaunchProfile loads it and the overlay reflects the seeded
// profile name. Verifies ordering invariant.
//
// Mimics runClaude's linear call sequence (maybeMigrateFromEnv →
// resolveLaunchProfile) with the same stateDir, exercising the
// load-bearing ordering directly.
func TestMaybeMigrateFromEnv_PostmigrationDefaultFlows(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=https://gw.example.com/",
		"ANTHROPIC_API_KEY=sk-fake-xyz",
	}
	_ = captureStderrFn(t, func() {
		maybeMigrateFromEnv(dir, parent, cwd, nil, false)
	})
	// Now mimic the runClaude flow: resolveLaunchProfile picks up the
	// just-written file.
	overlay, err := resolveLaunchProfile(profiles.DefaultPath(dir), "")
	if err != nil {
		t.Fatalf("resolveLaunchProfile: %v", err)
	}
	if overlay == nil {
		t.Fatalf("overlay nil; expected the seeded profile")
	}
	if overlay.Name != "local" {
		t.Errorf("overlay.Name = %q; want local", overlay.Name)
	}
	if overlay.BaseURL != "https://gw.example.com/" {
		t.Errorf("overlay.BaseURL = %q; want passthrough", overlay.BaseURL)
	}
}

// T-readme-stat-error-skip — os.Stat returns a NON-ErrNotExist error
// (here: syscall.EINVAL via a NUL-byte in the path). maybeMigrateFromEnv
// must emit a stderr "skip init: stat" line and return cleanly without
// panic. The blocker-file approach is unworkable because
// profiles.DefaultPath returns stateDir/profiles.json flat.
func TestMaybeMigrateFromEnv_StatErrorSkip(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	cwd, _ := os.Getwd()
	// NUL byte in the path causes os.Stat to return syscall.EINVAL
	// (which is NOT errors.Is(err, fs.ErrNotExist)). This exercises the
	// "skip init: stat ..." branch in maybeMigrateFromEnv.
	badStateDir := "\x00bad"
	out := captureStderrFn(t, func() {
		maybeMigrateFromEnv(badStateDir, []string{
			"ANTHROPIC_BASE_URL=https://gw.example.com/",
			"ANTHROPIC_API_KEY=sk-fake",
		}, cwd, nil, false)
	})
	if !strings.Contains(out, "skip init") {
		t.Errorf("stderr missing 'skip init' marker; got: %s", out)
	}
}

func TestPrintUsage_ListsAllProfileSubcommands(t *testing.T) {
	r, w, _ := os.Pipe()
	// printUsage may write to os.Stdout or os.Stderr; capture both.
	origStdout := os.Stdout
	origStderr := os.Stderr
	os.Stdout = w
	os.Stderr = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	})

	printUsage()
	w.Close()
	captured, _ := io.ReadAll(r)
	os.Stdout = origStdout
	os.Stderr = origStderr

	got := string(captured)
	for _, verb := range []string{"ls", "status", "switch", "test", "add", "edit", "rm", "set-default"} {
		if !strings.Contains(got, verb) {
			t.Fatalf("printUsage missing verb %q; got %q", verb, got)
		}
	}
}

// TestEnsureOfficialProfile_LaunchOrder — verify EnsureOfficialProfile
// runs before maybeMigrateFromEnv. After both helpers, the file has
// official (from ensure) AND the env-migrated profile, and file.Default
// stays at "official" — env migration does not steal default.
func TestEnsureOfficialProfile_LaunchOrder(t *testing.T) {
	restore := stubIsTerminal(false) // non-TTY → name = "local"
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=http://3rd.example.com/",
		"ANTHROPIC_AUTH_TOKEN=sk-test",
		"PATH=/usr/bin",
	}

	// Simulate runClaude's helper sequence.
	if err := profiles.EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	maybeMigrateFromEnv(dir, parent, cwd, nil, false)

	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil || f == nil {
		t.Fatalf("load after both helpers: %v, %v", err, f)
	}
	if _, ok := f.Profiles[profiles.OfficialProfileName]; !ok {
		t.Error("official must be present after ensure")
	}
	if f.Default != profiles.OfficialProfileName {
		t.Errorf("Default = %q, want %q (env migration must not steal default)",
			f.Default, profiles.OfficialProfileName)
	}
}

// TestSP4a_SkipsWhenEnvMigratedProfileExists — if file already has a
// profile whose BaseURL matches env's ANTHROPIC_BASE_URL, env migration
// no-ops (no duplicate prompts, no second profile created).
func TestSP4a_SkipsWhenEnvMigratedProfileExists(t *testing.T) {
	restore := stubIsTerminal(false)
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=http://3rd.example.com",
		"ANTHROPIC_AUTH_TOKEN=sk-test",
		"PATH=/usr/bin",
	}

	// Seed file with both official AND an env-migrated entry.
	initial := &profiles.File{
		Default: profiles.OfficialProfileName,
		Profiles: map[string]profiles.Profile{
			profiles.OfficialProfileName: profiles.OfficialProfile(),
			"existing": {
				Name:     "existing",
				Provider: "3rd",
				BaseURL:  "http://3rd.example.com",
				Auth:     &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-test"},
				Egress:   profiles.EgressSpec{Mode: "inherit"},
			},
		},
	}
	if err := profiles.OverwriteFile(profiles.DefaultPath(dir), initial, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	maybeMigrateFromEnv(dir, parent, cwd, nil, false)

	f, _ := profiles.Load(profiles.DefaultPath(dir))
	if len(f.Profiles) != 2 {
		t.Errorf("expected 2 profiles (official + existing), got %d", len(f.Profiles))
	}
}

// TestSP4a_AppendsToExistingFile — file has official only; env has BASE_URL
// + token; env migration appends the env-migrated profile WITHOUT stealing
// default.
func TestSP4a_AppendsToExistingFile(t *testing.T) {
	restore := stubIsTerminal(false) // non-TTY → name = "local"
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=http://3rd.example.com/",
		"ANTHROPIC_AUTH_TOKEN=sk-test",
		"PATH=/usr/bin",
	}

	if err := profiles.EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	maybeMigrateFromEnv(dir, parent, cwd, nil, false)

	f, _ := profiles.Load(profiles.DefaultPath(dir))
	if len(f.Profiles) != 2 {
		t.Errorf("expected official + local (2), got %d profiles", len(f.Profiles))
	}
	if _, ok := f.Profiles["local"]; !ok {
		t.Error("env-migrated profile 'local' must be appended")
	}
	if f.Default != profiles.OfficialProfileName {
		t.Errorf("Default = %q, want %q (env migration must not steal default)",
			f.Default, profiles.OfficialProfileName)
	}
}
