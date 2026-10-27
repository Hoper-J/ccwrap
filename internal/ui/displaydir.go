package ui

import (
	"os"
	"path/filepath"
	"strings"
)

// DisplayDir shortens a folder under the user's home to "~/…" for display.
func DisplayDir(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || dir == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rel
	}
	return dir
}
