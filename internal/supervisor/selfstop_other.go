//go:build !windows

package supervisor

import (
	"os"
	"syscall"
)

// stopSelf sends this process SIGTERM, which the launcher's signal relay
// hands on to Claude Code.
func (s *Supervisor) stopSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
