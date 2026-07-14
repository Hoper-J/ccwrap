//go:build unix

package update

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// TestApplyUmaskDoesNotStripExec: the replaced binary must be 0755.
// os.WriteFile's 0o755 only applies at creation and is subject to the
// umask — umask 077, common in sudo scenarios, strips the group/other
// exec bits, so Apply must chmod explicitly as a backstop. (Releases
// ship darwin/linux only; syscall.Umask is always available under the
// unix tag.)
func TestApplyUmaskDoesNotStripExec(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

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
	if err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("perm = %o, want 0755 (umask must not strip exec bits)", info.Mode().Perm())
	}
}
