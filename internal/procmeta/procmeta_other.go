//go:build !windows

package procmeta

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func platformStartToken(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		if tok, err := linuxStartToken(pid); err == nil {
			return tok, nil
		}
	}
	return psStartToken(pid)
}

// Exists reports whether a process with pid is running, whoever it belongs
// to.
func Exists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
