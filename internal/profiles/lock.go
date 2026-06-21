package profiles

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock acquires an exclusive, cross-process advisory lock guarding the
// load → mutate → OverwriteFile sequence on profiles.json. Callers MUST
// hold it across the whole sequence and release the returned unlock func
// (defer it) once the write completes.
//
// Why this is needed: profiles.json is a single file shared by every
// ccwrap process — concurrent `ccwrap` supervisors and the `ccwrap
// profile` CLI all read-modify-write it. The supervisor's in-process
// profileFileMu (internal/supervisor/server.go) only serializes goroutines
// within one process; it cannot see another process. Without this lock two
// processes each Load the same snapshot, mutate independently, and the
// second OverwriteFile clobbers the first — a silently lost update.
//
// The lock is held on a sidecar file under <stateDir>/locks (the same
// convention as the CA lock; see internal/certs/ca.go's acquireLock), NOT
// on profiles.json itself: OverwriteFile replaces profiles.json via an
// atomic rename, which swaps the inode out from under any lock held on the
// original file's fd. flock is associated with the open file description,
// so two acquisitions from the same process (each opening its own fd) also
// serialize — making this the cross-process complement to profileFileMu.
func Lock(stateDir string) (unlock func(), err error) {
	lockPath := filepath.Join(stateDir, "locks", "profiles.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir profiles lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open profiles lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock profiles lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
