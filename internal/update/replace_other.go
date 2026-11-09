//go:build !windows

package update

import "os"

// replaceExe renames tmp over exePath; a running program's file can be
// replaced that way here.
func replaceExe(tmp, exePath string) error { return os.Rename(tmp, exePath) }

// RemoveReplaced has nothing to remove here: replaceExe leaves nothing.
func RemoveReplaced(string) {}
