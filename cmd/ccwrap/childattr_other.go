//go:build !linux && !windows

package main

import (
	"os"
	"syscall"
)

// childSysProcAttr is a no-op off Linux. macOS / BSD have no Pdeathsig
// equivalent, so the hard-process-death linkage Linux gets is unavailable;
// the supervisor-goroutine-returns case is still handled in Wait(), and
// leftover runtime dirs are reaped by the next launch's orphan sweep.
func childSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// bindChildLifetime has nothing to do here: there is no way to have the
// child end with this process.
func bindChildLifetime(*os.Process) error { return nil }
