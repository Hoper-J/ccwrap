package procmeta

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// StartToken returns a platform-specific token that uniquely identifies the
// current incarnation of a PID. On Linux it prefers /proc/<pid>/stat field 22
// (process start time in clock ticks after boot). On macOS and other POSIX
// targets it falls back to ps lstart output. The token is intended for local
// manifest validation, not user-facing display.
func StartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	if runtime.GOOS == "linux" {
		if tok, err := linuxStartToken(pid); err == nil {
			return tok, nil
		}
	}
	return psStartToken(pid)
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

func linuxStartToken(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(data))
	end := strings.LastIndex(raw, ")")
	if end <= 0 || end+2 > len(raw) {
		return "", fmt.Errorf("unexpected /proc stat format")
	}
	fields := strings.Fields(raw[end+2:])
	// Fields after the closing ')' begin at stat field 3. We need overall
	// field 22 (starttime), which maps to index 19 in this sliced tail.
	if len(fields) <= 19 {
		return "", fmt.Errorf("unexpected /proc stat field count")
	}
	return "linux:" + fields[19], nil
}

func psStartToken(pid int) (string, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	token := normalizeSpaces(string(out))
	if token == "" {
		return "", fmt.Errorf("empty ps lstart token")
	}
	return "ps:" + token, nil
}

func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
