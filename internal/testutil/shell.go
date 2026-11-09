package testutil

import (
	"runtime"
	"strings"
)

// ShellQuotes is s, a command hint written with single quotes, the way ccwrap
// prints it here: Windows hints use double quotes (see modelalias.ShellArg).
func ShellQuotes(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(s, "'", `"`)
	}
	return s
}
