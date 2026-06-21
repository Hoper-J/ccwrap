package certs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/app"
)

func TestEnsureCAWritesCompositeBundleAndSelfHeals(t *testing.T) {
	tmp := t.TempDir()
	systemBundlePath := filepath.Join(tmp, "system-roots.pem")
	systemPEM := []byte("-----BEGIN CERTIFICATE-----\nSYSTEMROOT\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(systemBundlePath, systemPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCWRAP_SYSTEM_CA_BUNDLE", systemBundlePath)

	paths := app.Paths{
		RuntimeDir:   filepath.Join(tmp, "runtime"),
		StateDir:     filepath.Join(tmp, "state"),
		CACertPath:   filepath.Join(tmp, "state", "certs", "ca-cert.pem"),
		CAKeyPath:    filepath.Join(tmp, "state", "certs", "ca-key.pem"),
		CABundlePath: filepath.Join(tmp, "state", "certs", "ca-bundle.pem"),
		CALockPath:   filepath.Join(tmp, "state", "locks", "ca.lock"),
	}
	mgr := NewManager(paths)
	if err := mgr.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(paths.CABundlePath)
	if err != nil {
		t.Fatal(err)
	}
	ccwrapCert, err := os.ReadFile(paths.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bundle), "SYSTEMROOT") {
		t.Fatalf("expected bundle to include overridden system roots, got %q", string(bundle))
	}
	if !strings.Contains(string(bundle), string(strings.TrimSpace(string(ccwrapCert)))) {
		t.Fatal("expected bundle to include CCWRAP root certificate")
	}
	status := mgr.BundleStatus()
	if !status.HasSystemCA || !status.HasCCWRAPRoot || status.Mode != "composite" {
		t.Fatalf("unexpected bundle status: %#v", status)
	}
	if err := os.Remove(paths.CABundlePath); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.CABundlePath); err != nil {
		t.Fatalf("expected bundle to be recreated, got %v", err)
	}
}

func TestEnsureCAFallsBackToCCWRAPOnlyWhenSystemBundleMissing(t *testing.T) {
	tmp := t.TempDir()
	missingPath := filepath.Join(tmp, "does-not-exist.pem")
	t.Setenv("CCWRAP_SYSTEM_CA_BUNDLE", missingPath)

	paths := app.Paths{
		RuntimeDir:   filepath.Join(tmp, "runtime"),
		StateDir:     filepath.Join(tmp, "state"),
		CACertPath:   filepath.Join(tmp, "state", "certs", "ca-cert.pem"),
		CAKeyPath:    filepath.Join(tmp, "state", "certs", "ca-key.pem"),
		CABundlePath: filepath.Join(tmp, "state", "certs", "ca-bundle.pem"),
		CALockPath:   filepath.Join(tmp, "state", "locks", "ca.lock"),
	}
	mgr := NewManager(paths)
	if err := mgr.EnsureCA(); err != nil {
		t.Fatalf("EnsureCA should succeed with CCWRAP-only fallback, got %v", err)
	}
	status := mgr.BundleStatus()
	if status.Mode != "ccwrap_only" {
		t.Fatalf("expected Mode=ccwrap_only, got %q", status.Mode)
	}
	if status.HasSystemCA {
		t.Fatal("expected HasSystemCA=false on fallback")
	}
	if !status.HasCCWRAPRoot {
		t.Fatal("expected HasCCWRAPRoot=true on fallback")
	}
	if status.SystemCAWarning == "" {
		t.Fatal("expected SystemCAWarning set on fallback")
	}
	bundle, err := os.ReadFile(paths.CABundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bundle), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("expected CCWRAP root certificate in bundle, got %q", string(bundle))
	}
}

func TestEnsureCARejectsOverrideWithoutPEMMarker(t *testing.T) {
	tmp := t.TempDir()
	garbagePath := filepath.Join(tmp, "garbage.html")
	if err := os.WriteFile(garbagePath, []byte("<html><body>proxy error</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCWRAP_SYSTEM_CA_BUNDLE", garbagePath)

	paths := app.Paths{
		RuntimeDir:   filepath.Join(tmp, "runtime"),
		StateDir:     filepath.Join(tmp, "state"),
		CACertPath:   filepath.Join(tmp, "state", "certs", "ca-cert.pem"),
		CAKeyPath:    filepath.Join(tmp, "state", "certs", "ca-key.pem"),
		CABundlePath: filepath.Join(tmp, "state", "certs", "ca-bundle.pem"),
		CALockPath:   filepath.Join(tmp, "state", "locks", "ca.lock"),
	}
	mgr := NewManager(paths)
	if err := mgr.EnsureCA(); err != nil {
		t.Fatalf("EnsureCA should still succeed via fallback, got %v", err)
	}
	status := mgr.BundleStatus()
	if status.Mode != "ccwrap_only" {
		t.Fatalf("expected Mode=ccwrap_only when override is invalid, got %q", status.Mode)
	}
	if !strings.Contains(status.SystemCAWarning, "does not contain a PEM CERTIFICATE block") {
		t.Fatalf("expected validation-reason warning, got %q", status.SystemCAWarning)
	}
}

func TestParseDarwinKeychainListIncludesQuotedSearchList(t *testing.T) {
	out := []byte("    \"/Users/test/Library/Keychains/login.keychain-db\"\n    \"/Library/Keychains/System.keychain\"\n    \"/Users/test/Library/Keychains/login.keychain-db\"\n")
	got := parseDarwinKeychainList(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 keychains after dedupe, got %#v", got)
	}
	if got[0] != "/Users/test/Library/Keychains/login.keychain-db" {
		t.Fatalf("unexpected first keychain: %#v", got)
	}
	if got[1] != "/Library/Keychains/System.keychain" {
		t.Fatalf("unexpected second keychain: %#v", got)
	}
}
