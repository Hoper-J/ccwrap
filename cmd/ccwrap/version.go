package main

import (
	"runtime/debug"
	"strings"
)

// versionBase is the semver baseline. For local/dev builds it is the
// source-declared default below and debug.ReadBuildInfo appends the git
// short-SHA and a +dirty suffix from the VCS info Go embeds during
// `go build`. For tagged releases the GoReleaser pipeline overrides it via
// -ldflags "-X main.versionBase={{.Version}}" so `ccwrap version` reports the
// release tag (e.g. 0.2.0) instead of this baseline. It is a var (not const)
// precisely so the linker can set it; do not make it const.
var versionBase = "0.1.0"

// versionString returns a semver-2.0 compliant version with build
// metadata appended:
//
//	0.1.0                       — clean build, no VCS info available
//	0.1.0+9944cdc               — built from a clean working tree
//	0.1.0+9944cdc.dirty         — built with uncommitted changes
//
// The "+" separator marks build metadata per semver 2.0 §10 (build
// metadata is ignored when determining version precedence — fine, the
// short-SHA is for telemetry / bug reports, not ordering).
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	return formatVersion(info, ok)
}

// formatVersion derives the version string from build info. Split out from
// versionString so the otherwise un-mockable debug.ReadBuildInfo branches can
// be unit-tested.
func formatVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return versionBase
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = ".dirty"
			}
		}
	}
	if rev == "" {
		// `go install module@vX.Y.Z` embeds the module version but no VCS
		// settings — report the tag (e.g. 0.2.0) instead of the source
		// baseline. An in-tree `go build` sets rev (handled below); a
		// GoReleaser build sets versionBase via ldflags and Main.Version is
		// "(devel)", so it falls through to versionBase.
		if v := strings.TrimPrefix(info.Main.Version, "v"); v != "" && info.Main.Version != "(devel)" {
			return v
		}
		return versionBase
	}
	short := rev
	if len(short) > 7 {
		short = short[:7]
	}
	var b strings.Builder
	b.WriteString(versionBase)
	b.WriteByte('+')
	b.WriteString(short)
	b.WriteString(dirty)
	return b.String()
}
