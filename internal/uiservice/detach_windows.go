package uiservice

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached starts newCmd's command without a console and in its own
// process group, so closing the caller's window or pressing Ctrl-C there
// does not reach it. It also leaves the caller's job where the job allows
// that: a ui that `ccwrap open` starts inside a session would otherwise end
// with the session's job.
func startDetached(newCmd func() *exec.Cmd) (*exec.Cmd, error) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB}
	err := cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The job does not let processes leave it.
		cmd = newCmd()
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		err = cmd.Start()
	}
	return cmd, err
}
