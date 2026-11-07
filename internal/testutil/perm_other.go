//go:build !windows

package testutil

import (
	"os"
	"testing"
)

// MakeWritableByOthers lets every user change dir: chmod 0777.
func MakeWritableByOthers(t testing.TB, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
}

// Symlink makes newname a symlink to oldname.
func Symlink(t testing.TB, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatal(err)
	}
}
