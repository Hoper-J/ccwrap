package procmeta

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// Windows reuses process ids soon, but never with the same creation time.
func platformStartToken(pid int) (string, error) {
	h, err := openProcess(pid)
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if !running(h) {
		return "", fmt.Errorf("process %d has exited", pid)
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", fmt.Errorf("read process %d times: %w", pid, err)
	}
	return fmt.Sprintf("windows:%d", uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)), nil
}

// Exists reports whether a process with pid is running, whoever it belongs
// to.
func Exists(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := openProcess(pid)
	if err != nil {
		// Another user's process can refuse to be opened; it still runs.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	return running(h)
}

func openProcess(pid int) (windows.Handle, error) {
	return windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
}

// running is false for a process that has exited but is still open, as it
// stays while anyone holds a handle to it.
func running(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
