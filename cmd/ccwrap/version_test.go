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
// non-empty string. The fallback (no VCS info) returns versionBase
// alone, which is enough for `ccwrap version` to print something.
func TestVersionString_NeverEmpty(t *testing.T) {
	v := versionString()
	if v == "" {
		t.Fatal("versionString returned empty string")
	}
	if !strings.HasPrefix(v, versionBase) {
		t.Errorf("versionString %q must start with versionBase %q", v, versionBase)
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

// TestVersionString_BuildMetadataFormat — when VCS info is present
// (typical for go-built binaries from a git checkout), the string
// looks like "0.1.0+SHA" or "0.1.0+SHA.dirty". Either matches the
// "MAJOR.MINOR.PATCH" prefix followed by an optional "+" build
// metadata segment.
func TestVersionString_BuildMetadataFormat(t *testing.T) {
	v := versionString()
	if v == versionBase {
		return // no VCS info — fallback path, nothing more to check
	}
	plus := strings.Index(v, "+")
	if plus < 0 {
		t.Fatalf("expected '+' separator when VCS info present, got %q", v)
	}
	if v[:plus] != versionBase {
		t.Errorf("prefix %q != versionBase %q", v[:plus], versionBase)
	}
	meta := v[plus+1:]
	if meta == "" {
		t.Errorf("empty build metadata after '+': %q", v)
	}
	// SHA portion is hex; .dirty suffix is optional.
	sha := strings.TrimSuffix(meta, ".dirty")
	for _, c := range sha {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			t.Errorf("non-hex char %q in SHA portion of %q", c, v)
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

// TestFormatVersion_VCSRevisionWins — an in-tree VCS revision takes precedence
// over Main.Version (the dev-build path) and yields versionBase+SHA[.dirty].
func TestFormatVersion_VCSRevisionWins(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef0123456"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	if got := formatVersion(info, true); got != versionBase+"+abcdef0.dirty" {
		t.Fatalf("VCS revision should win, got %q", got)
	}
}
