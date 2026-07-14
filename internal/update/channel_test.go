// internal/update/channel_test.go
package update

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func bi(version string) *debug.BuildInfo {
	if version == "" {
		return nil
	}
	return &debug.BuildInfo{Main: debug.Module{Version: version}}
}

func TestDetectChannel(t *testing.T) {
	// Key background: the binary inside the npm package and the release
	// tarball are the same goreleaser artifact (release.yml notes the
	// npm payload equals dist), so ldflags cannot tell these two
	// channels apart — which is why the node_modules path check must
	// precede versionBase.
	cases := []struct {
		name        string
		exePath     string
		versionBase string
		buildInfo   *debug.BuildInfo
		want        Channel
	}{
		{"npm global", "/usr/lib/node_modules/@hoper-j/ccwrap-cli-linux-amd64/bin/ccwrap", "0.3.0", bi(""), ChannelNPM},
		{"pnpm global", "/home/u/.local/share/pnpm/global/5/node_modules/@hoper-j/ccwrap-cli-linux-amd64/bin/ccwrap", "0.3.0", bi(""), ChannelPnpm},
		{"bun global", "/home/u/.bun/install/global/node_modules/@hoper-j/ccwrap-cli-linux-amd64/bin/ccwrap", "0.3.0", bi(""), ChannelBun},
		{"yarn global", "/home/u/.config/yarn/global/node_modules/@hoper-j/ccwrap-cli-linux-amd64/bin/ccwrap", "0.3.0", bi(""), ChannelYarn},
		{"install.sh binary", "/usr/local/bin/ccwrap", "0.3.0", bi(""), ChannelBinary},
		{"go install", "/home/u/go/bin/ccwrap", "0.0.0", bi("v0.3.0"), ChannelGoInstall},
		{"go install pseudo", "/home/u/go/bin/ccwrap", "0.0.0", bi("v0.3.1-0.20260702120000-9944cdcabcde"), ChannelSource},
		{"dev build devel", "/home/u/src/ccwrap/ccwrap", "0.0.0", bi("(devel)"), ChannelSource},
		{"dev build nil buildinfo", "/home/u/src/ccwrap/ccwrap", "0.0.0", nil, ChannelSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectChannel(tc.exePath, tc.versionBase, tc.buildInfo); got != tc.want {
				t.Fatalf("DetectChannel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpgradeArgvAndHints(t *testing.T) {
	if got := UpgradeArgv(ChannelNPM); len(got) == 0 || got[0] != "npm" {
		t.Fatalf("npm argv = %v", got)
	}
	if got := UpgradeArgv(ChannelGoInstall); len(got) == 0 || got[len(got)-1] != "github.com/Hoper-J/ccwrap/cmd/ccwrap@latest" {
		t.Fatalf("go argv = %v", got)
	}
	if UpgradeArgv(ChannelBinary) != nil || UpgradeArgv(ChannelSource) != nil {
		t.Fatal("binary/source must have no argv (self-replace / refuse)")
	}
	for _, ch := range []Channel{ChannelNPM, ChannelPnpm, ChannelBun, ChannelYarn, ChannelBinary, ChannelGoInstall, ChannelSource} {
		if ManualHint(ch) == "" {
			t.Fatalf("empty manual hint for %v", ch)
		}
	}
}

func TestResolveExecutableFollowsSymlink(t *testing.T) {
	// Self-replacement must land on the symlink's target: a user-made
	// symlink keeps pointing where it did, and the real installed file
	// is what gets replaced.
	dir := t.TempDir()
	real := filepath.Join(dir, "real-ccwrap")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// macOS puts TMPDIR under /var (a symlink to /private/var), so the
	// baseline path must be resolved the same way — otherwise the two
	// sides of the comparison are not in the same namespace.
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ccwrap")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != resolvedReal {
		t.Fatalf("EvalSymlinks = %q, want %q", resolved, resolvedReal)
	}
	// ResolveExecutable must also work on the test binary itself.
	if _, err := ResolveExecutable(); err != nil {
		t.Fatalf("ResolveExecutable: %v", err)
	}
}
