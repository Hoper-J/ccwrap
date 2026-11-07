//go:build !windows

package supervisor

import (
	"os"
	"syscall"
)

// canStopSelf: /v1/shutdown can stop this process.
const canStopSelf = true

// stopSelf sends this process SIGTERM, which the launcher's signal relay
// hands on to Claude Code.
func stopSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
