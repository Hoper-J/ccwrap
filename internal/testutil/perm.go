package testutil

import (
	"runtime"
	"testing"
)

// HasUnixPermissions reports whether files carry Unix permission bits.
// Windows has none: there the private state directory's access list keeps
// ccwrap's files to their owner (see app.EnsurePaths).
func HasUnixPermissions() bool { return runtime.GOOS != "windows" }

// SkipWithoutUnixPermissions skips a test that is about permission bits.
func SkipWithoutUnixPermissions(t testing.TB) {
	t.Helper()
	if !HasUnixPermissions() {
		t.Skip("no Unix permission bits on " + runtime.GOOS)
	}
}
