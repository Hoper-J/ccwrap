package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeArchive builds a tar.gz containing the single file "ccwrap" and
// returns (archive, sha256hex).
func makeArchive(t *testing.T, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "ccwrap", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), fmt.Sprintf("%x", sum)
}

// releaseServer mimics the GitHub releases download path layout
// (…/v<ver>/ccwrap_<ver>_<os>_<arch>.tar.gz and …/v<ver>/checksums.txt).
func releaseServer(t *testing.T, version string, archive []byte, checksums string) *httptest.Server {
	t.Helper()
	name := fmt.Sprintf("ccwrap_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/v"+version+"/"+name, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/v"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, checksums) })
	return httptest.NewServer(mux)
}

func TestApplyReplacesBinary(t *testing.T) {
	newBin := []byte("NEW BINARY BYTES")
	archive, sum := makeArchive(t, newBin)
	name := fmt.Sprintf("ccwrap_0.3.1_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := releaseServer(t, "0.3.1", archive, fmt.Sprintf("%s  %s\n", sum, name))
	defer srv.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, &out); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || !bytes.Equal(got, newBin) {
		t.Fatalf("binary not replaced: %q err=%v", got, err)
	}
	info, _ := os.Stat(exe)
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("replaced binary must be executable")
	}
	// Failure paths leave no temp files; the success path must not
	// either.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp residue left behind: %d entries", len(entries))
	}
	if !strings.Contains(out.String(), "downloading v0.3.1") {
		t.Fatalf("missing progress line: %q", out.String())
	}
}

func TestApplyChecksumMismatch(t *testing.T) {
	archive, _ := makeArchive(t, []byte("NEW"))
	name := fmt.Sprintf("ccwrap_0.3.1_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := releaseServer(t, "0.3.1", archive, fmt.Sprintf("%064d  %s\n", 0, name))
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Fatal("binary must be untouched on checksum mismatch")
	}
}

func TestApplyMissingChecksumEntry(t *testing.T) {
	// checksums.txt exists but has no entry for this platform's archive
	// → installation must be refused (same rule as install.sh: no
	// checksum, no install).
	archive, sum := makeArchive(t, []byte("NEW"))
	srv := releaseServer(t, "0.3.1", archive, fmt.Sprintf("%s  ccwrap_0.3.1_plan9_mips.tar.gz\n", sum))
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no checksum for") {
		t.Fatalf("want 'no checksum for' error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Fatal("binary must be untouched when the checksum entry is missing")
	}
}

func TestApplyMissingAsset(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestApplyUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	newBin := []byte("NEW")
	archive, sum := makeArchive(t, newBin)
	name := fmt.Sprintf("ccwrap_0.3.1_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := releaseServer(t, "0.3.1", archive, fmt.Sprintf("%s  %s\n", sum, name))
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("want os.ErrPermission (for the caller's sudo hint), got %v", err)
	}
}
