package main

import (
	"runtime/debug"
	"strings"
)

// versionBase is the linker injection point for the release pipeline:
// GoReleaser sets -ldflags "-X main.versionBase={{.Version}}" so released
// binaries report the tag (plus the +SHA metadata appended below). The source
// default is a sentinel, NOT a maintained baseline — every other build shape
// derives its version automatically from debug.ReadBuildInfo, so nothing here
// needs bumping at release time. It is a var (not const) precisely so the
// linker can set it; do not make it const.
var versionBase = "0.0.0"

// versionString returns a semver-2.0 compliant version. Derivation order:
//
//	0.1.1+9944cdc[.dirty]                        — release build (linker-injected tag + VCS SHA)
//	0.1.1                                        — `go install module@v0.1.1`
//	0.1.1 / 0.1.2-0.20260702…-9944cdc[+dirty]    — in-tree `go build`, Go ≥1.24 VCS stamp
//	                                               (exact tag, or pseudo-version past it)
//	0.0.0+9944cdc[.dirty]                        — in-tree build without a stamp
//	                                               (older toolchain or -buildvcs=false)
//	0.0.0                                        — no build info at all
//
// The "+" separator marks build metadata per semver 2.0 §10 (build metadata
// is ignored when determining version precedence — fine, the short-SHA is for
// telemetry / bug reports, not ordering).
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
	if versionBase == "0.0.0" {
		// No linker injection. Go stamps Main.Version for `go install
		// module@vX.Y.Z` and, since Go 1.24, for in-tree `go build` too. The
		// stamp already encodes the commit (pseudo-version) and dirtiness
		// ("+dirty"), so report it verbatim instead of decorating it with a
		// second SHA.
		if v := strings.TrimPrefix(info.Main.Version, "v"); v != "" && info.Main.Version != "(devel)" {
			return v
		}
	}
	if rev == "" {
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
