//go:build !windows

package procgroup

import (
	"errors"
	"os"
)

// Lead reports false: only Windows launches lead a group.
func Lead() bool { return false }

// Leader reports false.
func Leader() bool { return false }

// Interrupt is not supported here.
func Interrupt() error { return errors.ErrUnsupported }

// Respawn leaves the launch to the caller.
func Respawn([]string, func(*os.Process)) (code int, ok bool) { return 0, false }
