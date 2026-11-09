//go:build !windows

package main

// directClaudeBin returns bin: only Windows runs scripts through a second
// command-line parser (see claudebin_windows.go).
func directClaudeBin(bin string) (string, error) { return bin, nil }
