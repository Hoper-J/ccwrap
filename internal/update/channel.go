// internal/update/channel.go
package update

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"golang.org/x/mod/semver"
)

// Channel identifies how this binary was installed, which decides how
// `ccwrap upgrade` acts.
type Channel int

const (
	// ChannelSource is a from-source build (pseudo-version, dirty, or no
	// stamp): upgrade refuses and points at git pull && go build.
	ChannelSource Channel = iota
	ChannelNPM
	ChannelPnpm
	ChannelBun
	ChannelYarn
	// ChannelBinary is a goreleaser release binary outside node_modules
	// (install.sh or manual tarball): upgrade self-replaces in place.
	ChannelBinary
	ChannelGoInstall
)

// DetectChannel classifies the running binary. Order matters: the npm
// platform package ships the SAME goreleaser binary as the release
// tarball (release.yml publishes dist/ verbatim to npm), so ldflags
// cannot tell those two apart — the node_modules path test must win
// before the versionBase test.
func DetectChannel(exePath, versionBase string, bi *debug.BuildInfo) Channel {
	p := filepath.ToSlash(exePath)
	if strings.Contains(p, "/node_modules/") {
		switch {
		case strings.Contains(p, "/pnpm/"):
			return ChannelPnpm
		case strings.Contains(p, "/.bun/"):
			return ChannelBun
		case strings.Contains(p, "/yarn/"):
			return ChannelYarn
		}
		return ChannelNPM
	}
	if versionBase != "0.0.0" {
		return ChannelBinary
	}
	// `go install module@vX.Y.Z` stamps a clean tag into Main.Version;
	// anything else (pseudo-version, "(devel)", missing info) is a
	// source build we must not try to manage.
	if bi != nil {
		v := bi.Main.Version
		if v != "" && v != "(devel)" && semver.IsValid(v) && semver.Prerelease(v) == "" {
			return ChannelGoInstall
		}
	}
	return ChannelSource
}

// UpgradeArgv returns the package-manager command that performs the
// upgrade for exec-style channels, nil for binary (self-replace) and
// source (refused).
func UpgradeArgv(ch Channel) []string {
	switch ch {
	case ChannelNPM:
		return []string{"npm", "install", "-g", "ccwrap-cli@latest"}
	case ChannelPnpm:
		return []string{"pnpm", "add", "-g", "ccwrap-cli@latest"}
	case ChannelBun:
		return []string{"bun", "add", "-g", "ccwrap-cli@latest"}
	case ChannelYarn:
		return []string{"yarn", "global", "add", "ccwrap-cli@latest"}
	case ChannelGoInstall:
		return []string{"go", "install", "github.com/Hoper-J/ccwrap/cmd/ccwrap@latest"}
	}
	return nil
}

// ManualHint is the copy-pasteable fallback command shown whenever the
// automated path fails or is unavailable — upgrade failures must always
// leave the user with a working next step.
func ManualHint(ch Channel) string {
	if argv := UpgradeArgv(ch); argv != nil {
		return strings.Join(argv, " ")
	}
	if ch == ChannelBinary {
		return "curl -fsSL https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.sh | sh"
	}
	return "git pull && go build -o ccwrap ./cmd/ccwrap"
}

// ResolveExecutable returns the symlink-resolved path of the running
// binary. Self-replace targets the REAL file so user-made symlinks keep
// pointing at the refreshed install.
func ResolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
