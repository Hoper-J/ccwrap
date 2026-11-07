//go:build !windows

package uiservice

import "syscall"

// detachedProcAttr starts the service in its own session, away from the
// caller's terminal and its hangup.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
