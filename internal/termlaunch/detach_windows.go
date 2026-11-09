package termlaunch

import "syscall"

// detachedAttr adds nothing: Windows terminals are opened through wt.exe,
// which returns at once.
func detachedAttr() *syscall.SysProcAttr { return nil }
