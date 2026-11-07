//go:build !windows

package app

import (
	"fmt"
	"os"
	"testing"
)

const (
	wantWritableByOthers = "others can write to it (mode 0777)"
	wantOwnedBy          = "it is owned by uid"
)

func chmod(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
}

// makeWritableByOthers is chmod 0777.
func makeWritableByOthers(t *testing.T, dir string) { chmod(t, dir, 0o777) }

// makeReadableByOthers is chmod 0755.
func makeReadableByOthers(t *testing.T, dir string) { chmod(t, dir, 0o755) }

// pretendOwnedBySomeoneElse has ownership checks see another uid as the
// current one.
func pretendOwnedBySomeoneElse(t *testing.T, _ string) {
	prev := getuid
	getuid = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { getuid = prev })
}

// permSnapshot is dir's mode.
func permSnapshot(t *testing.T, dir string) string {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%04o", fi.Mode().Perm())
}

func assertOwnerOnly(t *testing.T, dir string) {
	t.Helper()
	if got := permSnapshot(t, dir); got != "0700" {
		t.Fatalf("runtime dir mode = %s, want 0700", got)
	}
}

func symlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatal(err)
	}
}
