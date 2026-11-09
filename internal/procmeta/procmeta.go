package procmeta

import (
	"fmt"
	"os"
	"strings"
)

// StartToken returns a platform-specific token that uniquely identifies the
// current incarnation of a PID. On Linux it prefers /proc/<pid>/stat field 22
// (process start time in clock ticks after boot). On macOS and other POSIX
// targets it falls back to ps lstart output. On Windows it is the process
// creation time. The token is intended for local manifest validation, not
// user-facing display.
func StartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	return platformStartToken(pid)
}

func CurrentStartToken() (string, error) {
	return StartToken(os.Getpid())
}

func Matches(pid int, want string) (exists bool, match bool, err error) {
	tok, err := StartToken(pid)
	if err != nil {
		return false, false, err
	}
	if strings.TrimSpace(want) == "" {
		return true, false, nil
	}
	return true, tok == want, nil
}
