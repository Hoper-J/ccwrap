package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// allBytes locks the whole file, whatever its size, the way cmd/go's
// lockedfile does.
const allBytes = ^uint32(0)

func lock(f *os.File, wait bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, allBytes, allBytes, new(windows.Overlapped))
	if !wait && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}
