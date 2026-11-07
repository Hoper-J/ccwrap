package uiservice

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedProcAttr starts the service without a console and in its own
// process group, so closing the caller's window or pressing Ctrl-C there
// does not reach it.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
