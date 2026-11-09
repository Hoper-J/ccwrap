//go:build !windows

package termlaunch

import "syscall"

// detachedAttr starts a process in a session of its own, away from ccwrap
// ui's signals.
func detachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
