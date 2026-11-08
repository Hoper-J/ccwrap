//go:build !windows

package uiservice

import (
	"os/exec"
	"syscall"
)

// startDetached starts newCmd's command in its own session, away from the
// caller's terminal and its hangup.
func startDetached(newCmd func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cmd.Start()
}
