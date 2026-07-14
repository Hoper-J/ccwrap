package main

import (
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/app"
)

func TestVersionDispatchEarlyOnlyBareForm(t *testing.T) {
	var sink noopWriter
	if !versionDispatchEarly([]string{"ccwrap", "version"}, sink) {
		t.Fatal("bare `version` must early-dispatch")
	}
	if !versionDispatchEarly([]string{"ccwrap", "--version"}, sink) {
		t.Fatal("bare `--version` must early-dispatch")
	}
	if versionDispatchEarly([]string{"ccwrap", "version", "--check"}, sink) {
		t.Fatal("`version --check` must fall through to versionCommand")
	}
	if versionDispatchEarly([]string{"ccwrap", "--version", "--check"}, sink) {
		t.Fatal("`--version --check` must fall through to versionCommand")
	}
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestVersionCommandCheck(t *testing.T) {
	fakeCheckServer(t, "99.0.0") // reuses upgrade_test.go's helper
	paths := app.Paths{StateDir: t.TempDir()}
	if err := versionCommand(paths, []string{"--check"}); err != nil {
		t.Fatalf("versionCommand --check: %v", err)
	}
	if err := versionCommand(paths, []string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

func TestVersionCommandCheckNetworkFailure(t *testing.T) {
	t.Setenv("CCWRAP_UPDATE_CHECK_URL", "http://127.0.0.1:1/unreachable")
	paths := app.Paths{StateDir: t.TempDir()}
	err := versionCommand(paths, []string{"--check"})
	if err == nil {
		t.Fatal("network failure must be loud (explicit action)")
	}
	// Failure-posture hard rule: a network failure must offer an
	// actionable next step (same treatment as upgrade).
	if !strings.Contains(err.Error(), "--egress-proxy") {
		t.Fatalf("failure must point at --egress-proxy, got: %v", err)
	}
}
