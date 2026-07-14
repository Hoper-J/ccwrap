package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/update"
)

// fakeCheckServer serves a fixed latest version as a fake registry and
// injects its URL into the env.
func fakeCheckServer(t *testing.T, latest string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q}`, latest)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CCWRAP_UPDATE_CHECK_URL", srv.URL)
}

func TestUpgradeAlreadyLatest(t *testing.T) {
	// The `go test` binary's versionString() looks like 0.0.0 (valid
	// semver); with latest at the same 0.0.0, Newer is false — which is
	// exactly the already-latest branch this test wants to cover.
	fakeCheckServer(t, "0.0.0")
	paths := app.Paths{StateDir: t.TempDir()}
	calls := 0
	restore := upgradeRun
	upgradeRun = func(name string, args ...string) error { calls++; return nil }
	t.Cleanup(func() { upgradeRun = restore })
	if err := upgradeCommand(paths, nil); err != nil {
		t.Fatalf("upgradeCommand: %v", err)
	}
	if calls != 0 {
		t.Fatal("no package manager must run when already latest")
	}
	// An explicit check must also write the cache so the next launch's
	// banner can use it directly.
	if c, ok := update.LoadCache(paths.StateDir); !ok || c.Latest != "0.0.0" {
		t.Fatalf("cache not written: %+v ok=%v", c, ok)
	}
}

func TestUpgradeExecChannel(t *testing.T) {
	fakeCheckServer(t, "99.0.0")
	paths := app.Paths{StateDir: t.TempDir()}
	restoreDetect := upgradeDetect
	upgradeDetect = func() (string, update.Channel, error) {
		return "/usr/lib/node_modules/@hoper-j/x/bin/ccwrap", update.ChannelNPM, nil
	}
	t.Cleanup(func() { upgradeDetect = restoreDetect })
	var got []string
	restoreRun := upgradeRun
	upgradeRun = func(name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}
	t.Cleanup(func() { upgradeRun = restoreRun })
	// LookPath goes through the seam: the test must not require a real
	// npm in PATH.
	restoreLook := upgradeLookPath
	upgradeLookPath = func(string) (string, error) { return "/usr/bin/npm", nil }
	t.Cleanup(func() { upgradeLookPath = restoreLook })
	// The `go test` binary's versionString() looks like 0.0.0 or
	// 0.0.0+sha — valid semver, and 99.0.0 is strictly greater, so Newer
	// is true and the upgrade branch runs. (Eligible only gates passive
	// notices; upgrade is an explicit action and exempt from it.)
	if err := upgradeCommand(paths, nil); err != nil {
		t.Fatalf("upgradeCommand: %v", err)
	}
	want := "npm install -g ccwrap-cli@latest"
	if strings.Join(got, " ") != want {
		t.Fatalf("ran %v, want %q", got, want)
	}
}

// TestUpgradeEgressProxyFlagForms: both forms of --egress-proxy must
// be accepted (latest is the test binary's own 0.0.0, so the
// already-latest early return fires and the upgrade branch is never
// touched); a missing value must error.
func TestUpgradeEgressProxyFlagForms(t *testing.T) {
	fakeCheckServer(t, "0.0.0")
	paths := app.Paths{StateDir: t.TempDir()}
	if err := upgradeCommand(paths, []string{"--egress-proxy", "direct"}); err != nil {
		t.Fatalf("space form: %v", err)
	}
	if err := upgradeCommand(paths, []string{"--egress-proxy=direct"}); err != nil {
		t.Fatalf("equals form: %v", err)
	}
	if err := upgradeCommand(paths, []string{"--egress-proxy"}); err == nil {
		t.Fatal("missing value must error")
	}
}

func TestUpgradeSourceRefused(t *testing.T) {
	fakeCheckServer(t, "99.0.0")
	paths := app.Paths{StateDir: t.TempDir()}
	restoreDetect := upgradeDetect
	upgradeDetect = func() (string, update.Channel, error) {
		return "/home/u/src/ccwrap/ccwrap", update.ChannelSource, nil
	}
	t.Cleanup(func() { upgradeDetect = restoreDetect })
	err := upgradeCommand(paths, nil)
	if err == nil {
		t.Fatal("source build must be refused")
	}
	// Failure-posture hard rule: even a refusal must carry the manual
	// command for this channel.
	if !strings.Contains(err.Error(), update.ManualHint(update.ChannelSource)) {
		t.Fatalf("refusal must carry the source-channel manual hint, got: %v", err)
	}
}

func TestUpgradeUnknownFlag(t *testing.T) {
	paths := app.Paths{StateDir: t.TempDir()}
	if err := upgradeCommand(paths, []string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

// TestUpgradePMExitPropagates covers a non-zero package-manager exit:
// before the exit code is propagated, stderr must first state one line
// of ccwrap's own conclusion (the package manager's output does not
// necessarily make the failure clear). os.Exit cannot be asserted
// in-process, so this uses the standard subprocess pattern.
func TestUpgradePMExitPropagates(t *testing.T) {
	if os.Getenv("CCWRAP_TEST_PM_EXIT") == "1" {
		upgradeDetect = func() (string, update.Channel, error) {
			return "/usr/lib/node_modules/ccwrap-cli/bin/ccwrap", update.ChannelNPM, nil
		}
		upgradeLookPath = func(string) (string, error) { return "/usr/bin/npm", nil }
		upgradeRun = func(name string, args ...string) error {
			return exec.Command("sh", "-c", "exit 7").Run() // a real *exec.ExitError
		}
		err := upgradeCommand(app.Paths{StateDir: os.Getenv("CCWRAP_TEST_STATE_DIR")}, nil)
		// Must not reach this point: a non-zero exit has to propagate
		// via os.Exit.
		fmt.Fprintf(os.Stderr, "upgradeCommand returned instead of exiting: %v\n", err)
		os.Exit(0)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"99.0.0"}`)
	}))
	defer srv.Close()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestUpgradePMExitPropagates$")
	cmd.Env = append(os.Environ(),
		"CCWRAP_TEST_PM_EXIT=1",
		"CCWRAP_TEST_STATE_DIR="+t.TempDir(),
		"CCWRAP_UPDATE_CHECK_URL="+srv.URL,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("want child exit code 7, got err=%v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "npm exited with 7 — upgrade did not complete") {
		t.Fatalf("stderr must state the conclusion before exiting, got:\n%s", out)
	}
}

func TestUpgradeCheckFailureHasHint(t *testing.T) {
	t.Setenv("CCWRAP_UPDATE_CHECK_URL", "http://127.0.0.1:1/unreachable")
	paths := app.Paths{StateDir: t.TempDir()}
	restore := upgradeDetect
	upgradeDetect = func() (string, update.Channel, error) {
		return "/usr/lib/node_modules/@hoper-j/x/bin/ccwrap", update.ChannelNPM, nil
	}
	t.Cleanup(func() { upgradeDetect = restore })
	err := upgradeCommand(paths, nil)
	if err == nil {
		t.Fatal("check failure must error")
	}
	if !strings.Contains(err.Error(), "npm install -g ccwrap-cli@latest") {
		t.Fatalf("check-failure error must carry the channel's manual command, got: %v", err)
	}
}

// TestUpgradeCheckFailureFallbackHint: a check failure stacked on a
// channel-detection failure — when we don't even know which command to
// suggest, the fallback must be the all-channels hint string.
func TestUpgradeCheckFailureFallbackHint(t *testing.T) {
	t.Setenv("CCWRAP_UPDATE_CHECK_URL", "http://127.0.0.1:1/unreachable")
	paths := app.Paths{StateDir: t.TempDir()}
	restore := upgradeDetect
	upgradeDetect = func() (string, update.Channel, error) {
		return "", update.ChannelSource, fmt.Errorf("boom")
	}
	t.Cleanup(func() { upgradeDetect = restore })
	err := upgradeCommand(paths, nil)
	if err == nil {
		t.Fatal("check failure must error")
	}
	if !strings.Contains(err.Error(), allChannelsHint) {
		t.Fatalf("unknown channel must fall back to the all-channels hint, got: %v", err)
	}
}

func TestUpgradeDetectFailureHasHint(t *testing.T) {
	fakeCheckServer(t, "99.0.0")
	paths := app.Paths{StateDir: t.TempDir()}
	restore := upgradeDetect
	upgradeDetect = func() (string, update.Channel, error) {
		return "", update.ChannelSource, fmt.Errorf("boom")
	}
	t.Cleanup(func() { upgradeDetect = restore })
	err := upgradeCommand(paths, nil)
	if err == nil {
		t.Fatal("detect failure must error")
	}
	if !strings.Contains(err.Error(), "releases/tag/v99.0.0") {
		t.Fatalf("detect-failure error must carry the release link, got: %v", err)
	}
}
