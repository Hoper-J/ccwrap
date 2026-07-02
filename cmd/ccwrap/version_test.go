package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

// TestVersionBase_IsSemverShape locks the base string at a parseable
// semver. If a future maintainer changes versionBase, they should keep
// it in MAJOR.MINOR.PATCH form so any downstream tooling that splits on
// "+" gets a clean prefix.
func TestVersionBase_IsSemverShape(t *testing.T) {
	parts := strings.Split(versionBase, ".")
	if len(parts) != 3 {
		t.Fatalf("versionBase must be MAJOR.MINOR.PATCH (got %q)", versionBase)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("versionBase has empty component: %q", versionBase)
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				t.Fatalf("versionBase component %q has non-digit %q", p, c)
			}
		}
	}
}

// TestVersionString_NeverEmpty — the formatter must always return a
// non-empty string without a leading "v" (the module-version prefix is
// stripped), whichever derivation path the test binary happened to take.
func TestVersionString_NeverEmpty(t *testing.T) {
	v := versionString()
	if v == "" {
		t.Fatal("versionString returned empty string")
	}
	if strings.HasPrefix(v, "v") {
		t.Errorf("versionString %q must not keep the module-version 'v' prefix", v)
	}
}

// TestVersionDispatchEarly — the dispatch must short-circuit before
// DefaultPaths so `HOME= ccwrap version` (and other degraded envs
// where state-dir resolution fails) still prints the version. The
// helper signature is structurally what enforces "before DefaultPaths"
// — it takes no paths and writes directly to its own io.Writer.
func TestVersionDispatchEarly(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		want       bool
		wantPrefix string
	}{
		{"no args", []string{"ccwrap"}, false, ""},
		{"version subcommand", []string{"ccwrap", "version"}, true, "ccwrap "},
		{"long flag", []string{"ccwrap", "--version"}, true, "ccwrap "},
		{"unrelated subcommand", []string{"ccwrap", "status"}, false, ""},
		{"version with trailing args ignored", []string{"ccwrap", "version", "extra"}, true, "ccwrap "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := versionDispatchEarly(tc.args, &buf)
			if got != tc.want {
				t.Errorf("dispatch returned %v, want %v", got, tc.want)
			}
			if tc.want && !strings.HasPrefix(buf.String(), tc.wantPrefix) {
				t.Errorf("output %q must start with %q", buf.String(), tc.wantPrefix)
			}
			if !tc.want && buf.Len() != 0 {
				t.Errorf("non-version arg wrote output: %q", buf.String())
			}
		})
	}
}

// TestHelpDispatchEarly — `ccwrap help` / `-h` / `--help` must work
// in degraded environments (same reasoning as version dispatch).
func TestHelpDispatchEarly(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var buf bytes.Buffer
			if !helpDispatchEarly([]string{"ccwrap", arg}, &buf) {
				t.Fatalf("help arg %q should dispatch", arg)
			}
			if !strings.Contains(buf.String(), "ccwrap") {
				t.Errorf("usage text missing program name; got %q", buf.String())
			}
		})
	}
	var buf bytes.Buffer
	if helpDispatchEarly([]string{"ccwrap", "status"}, &buf) {
		t.Error("non-help arg should not dispatch")
	}
}

// TestVersionString_StartsWithSemverDigits — whichever derivation path the
// test binary took (stamped module version, linker injection, or the
// sentinel fallback), the string must start with a MAJOR.MINOR.PATCH core so
// downstream tooling that splits on "+"/"-" gets a parseable prefix.
func TestVersionString_StartsWithSemverDigits(t *testing.T) {
	v := versionString()
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		t.Fatalf("version %q core %q is not MAJOR.MINOR.PATCH", v, core)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("version %q has empty core component", v)
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				t.Fatalf("version %q core component %q has non-digit %q", v, p, c)
			}
		}
	}
}

// TestFormatVersion_GoInstallTagReportsTag — `go install module@vX.Y.Z` has no
// VCS settings but carries Main.Version; report the tag, not the baseline.
func TestFormatVersion_GoInstallTagReportsTag(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}}
	if got := formatVersion(info, true); got != "0.2.0" {
		t.Fatalf("go install @v0.2.0 should report 0.2.0, got %q", got)
	}
}

// TestFormatVersion_DevelFallsBackToBase — a plain `go build` reports
// Main.Version "(devel)"; that must fall back to versionBase so the GoReleaser
// ldflag path keeps reporting its injected tag.
func TestFormatVersion_DevelFallsBackToBase(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	if got := formatVersion(info, true); got != versionBase {
		t.Fatalf("(devel) should fall back to versionBase %q, got %q", versionBase, got)
	}
}

// TestFormatVersion_StampedVersionWinsOverVCSDecoration — Go ≥1.24 stamps
// Main.Version for in-tree builds (exact tag, or a pseudo-version past it,
// with +dirty when modified). The stamp already encodes commit and dirtiness,
// so it is reported verbatim (minus the "v") instead of decorating the
// baseline with a second SHA.
func TestFormatVersion_StampedVersionWinsOverVCSDecoration(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.1.1-0.20260702104853-94175b8ba737+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "94175b8ba737abcdef"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	if got := formatVersion(info, true); got != "0.1.1-0.20260702104853-94175b8ba737+dirty" {
		t.Fatalf("stamped module version should be reported verbatim, got %q", got)
	}
}

// TestFormatVersion_LinkerInjectionWins — the GoReleaser pipeline injects the
// release tag via -X main.versionBase; that explicit intent beats the VCS
// stamp (snapshots inject e.g. "0.1.2-snapshot-abc" while the stamp would be
// an unrelated pseudo-version) and keeps the released "TAG+SHA" shape.
func TestFormatVersion_LinkerInjectionWins(t *testing.T) {
	saved := versionBase
	versionBase = "0.5.0"
	defer func() { versionBase = saved }()
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef0123456"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	if got := formatVersion(info, true); got != "0.5.0+abcdef0.dirty" {
		t.Fatalf("linker-injected version should win with SHA metadata, got %q", got)
	}
}

// TestFormatVersion_NoStampFallsBackToSentinelPlusSHA — old toolchains or
// -buildvcs=false leave Main.Version at "(devel)"; the sentinel baseline is
// decorated with the revision so the build is still identifiable.
func TestFormatVersion_NoStampFallsBackToSentinelPlusSHA(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef0123456"},
		},
	}
	if got := formatVersion(info, true); got != versionBase+"+abcdef0" {
		t.Fatalf("(devel) with VCS revision should decorate the sentinel, got %q", got)
	}
}
