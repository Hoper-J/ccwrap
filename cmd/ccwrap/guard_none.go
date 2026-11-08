//go:build linux || windows

package main

import "errors"

// runGuard has no guard to run: Linux ends Claude Code with ccwrap through
// Pdeathsig, Windows through a job object.
func runGuard([]string) error { return errors.New("no guard process on this platform") }
