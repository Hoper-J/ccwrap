//go:build !windows

package ui

import (
	"os"
	"strings"
)

// terminalReady: TERM is set and not "dumb".
func terminalReady(*os.File) bool {
	t := strings.ToLower(strings.TrimSpace(os.Getenv("TERM")))
	return t != "" && t != "dumb"
}

// EnableVT does nothing here: terminals take escape sequences as they are.
func EnableVT(...*os.File) (restore func()) { return func() {} }
