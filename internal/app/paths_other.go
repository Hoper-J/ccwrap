//go:build !windows

package app

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// getuid is os.Getuid; a seam for tests.
var getuid = os.Getuid

// ForeignOwner names the owner of fi at path ("uid 501") when that is not
// the current user.
func ForeignOwner(path string, fi fs.FileInfo) (string, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		return fmt.Sprintf("uid %d", st.Uid), true
	}
	return "", false
}

// othersCanWrite says how users other than the owner can write to dir, or
// returns "" when they cannot.
func othersCanWrite(dir string, fi fs.FileInfo) string {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Sprintf("others can write to it (mode %04o)", fi.Mode().Perm())
	}
	return ""
}

// makePrivate limits dir to its owner.
func makePrivate(dir string) error {
	return os.Chmod(dir, 0o700)
}
