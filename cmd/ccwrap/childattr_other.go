//go:build !linux && !windows

package main

import (
	"os"
	"syscall"
)

// childSysProcAttr adds nothing here: macOS and the BSDs have no Pdeathsig,
// so bindChildLifetime starts a guard process instead.
func childSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// bindChildLifetime starts a guard that ends the child if ccwrap dies before
// it (see startGuard), as Pdeathsig does on Linux.
func bindChildLifetime(p *os.Process) error { return startGuard(p) }
