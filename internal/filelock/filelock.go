// Package filelock takes exclusive locks on open files that other processes
// respect: flock on Unix, LockFileEx on Windows. Lock a dedicated sidecar
// file, not the data itself: on Windows the lock is mandatory, so other
// handles could not read a locked data file.
package filelock

import (
	"errors"
	"os"
)

// ErrLocked means TryLock found the lock held by someone else.
var ErrLocked = errors.New("file is locked by another holder")

// Lock blocks until f is locked exclusively. Two handles to the same file
// exclude each other, also within one process.
func Lock(f *os.File) error { return lock(f, true) }

// TryLock locks f exclusively, or returns ErrLocked without waiting.
func TryLock(f *os.File) error { return lock(f, false) }

// Unlock releases a lock taken through f.
func Unlock(f *os.File) error { return unlock(f) }
