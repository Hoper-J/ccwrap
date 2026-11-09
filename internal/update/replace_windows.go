package update

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// replaceExe puts tmp in place of exePath. Windows does not let the file of
// a running program be replaced (this ccwrap's, and that of any session or
// ccwrap ui running it) but does let it be renamed: the old file moves aside
// first, to a name RemoveReplaced deletes once nothing runs it.
func replaceExe(tmp, exePath string) error {
	old := fmt.Sprintf("%s.old-%d", exePath, time.Now().UnixNano())
	if err := os.Rename(exePath, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exePath); err != nil {
		_ = os.Rename(old, exePath)
		return err
	}
	_ = os.Remove(old) // fails while something still runs it
	return nil
}

// RemoveReplaced deletes the files replaceExe moved aside next to exePath
// that nothing runs any more.
func RemoveReplaced(exePath string) {
	olds, _ := filepath.Glob(exePath + ".old-*")
	for _, p := range olds {
		_ = os.Remove(p)
	}
}
