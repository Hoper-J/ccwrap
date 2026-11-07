package certs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// testCAPaths lays out a CA in dir whose system roots are the file it
// returns, holding systemPEM.
func testCAPaths(t *testing.T, dir, systemPEM string) (app.Paths, string) {
	t.Helper()
	system := filepath.Join(dir, "system-roots.pem")
	if err := os.WriteFile(system, []byte(systemPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCWRAP_SYSTEM_CA_BUNDLE", system)
	return app.Paths{
		RuntimeDir:   filepath.Join(dir, "runtime"),
		StateDir:     filepath.Join(dir, "state"),
		CACertPath:   filepath.Join(dir, "state", "certs", "ca-cert.pem"),
		CAKeyPath:    filepath.Join(dir, "state", "certs", "ca-key.pem"),
		CABundlePath: filepath.Join(dir, "state", "certs", "ca-bundle.pem"),
		CALockPath:   filepath.Join(dir, "state", "locks", "ca.lock"),
	}, system
}

// An unchanged bundle is left alone: replacing it fails on Windows while a
// program has it open, and every launch would try.
func TestEnsureCALeavesAnUnchangedBundleAlone(t *testing.T) {
	paths, _ := testCAPaths(t, t.TempDir(), "-----BEGIN CERTIFICATE-----\nSYSTEMROOT\n-----END CERTIFICATE-----\n")
	if err := NewManager(paths).EnsureCA(); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(paths.CABundlePath, past, past); err != nil {
		t.Fatal(err)
	}
	if err := NewManager(paths).EnsureCA(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(paths.CABundlePath); err != nil || info.ModTime().After(past.Add(time.Minute)) {
		t.Fatalf("an unchanged bundle was rewritten: %v", err)
	}
}

// A bundle that cannot be replaced stays when it already trusts ccwrap's
// root; without that root the failure stands.
func TestWriteBundleKeepsAUsableBundleItCannotReplace(t *testing.T) {
	testutil.SkipWithoutUnixPermissions(t) // a read-only folder still takes new files on Windows
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ca-bundle.pem")
	root := []byte("-----BEGIN CERTIFICATE-----\nCCWRAPROOT\n-----END CERTIFICATE-----\n")
	old := append([]byte("OLD SYSTEM ROOTS\n"), root...)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	stale, err := writeBundle(path, append([]byte("NEW SYSTEM ROOTS\n"), root...), root)
	if err != nil || !strings.Contains(stale, "kept the previous one") {
		t.Fatalf("writeBundle = %q, %v; want the old bundle kept", stale, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, old) {
		t.Fatal("the kept bundle changed")
	}
	if _, err := writeBundle(path, []byte("NEW"), []byte("ANOTHER ROOT")); err == nil {
		t.Fatal("kept a bundle that does not trust ccwrap's root")
	}
}

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
