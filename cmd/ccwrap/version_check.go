// cmd/ccwrap/version_check.go
package main

import (
	"fmt"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/update"
)

// versionCommand handles `ccwrap version` with flags. The bare form
// never reaches here (versionDispatchEarly answers it before state-dir
// resolution, preserving the broken-environment fast path).
func versionCommand(paths app.Paths, args []string) error {
	check := false
	egressFlag := "auto"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--check":
			check = true
		case arg == "--egress-proxy":
			if i+1 >= len(args) {
				return fmt.Errorf("--egress-proxy requires a value")
			}
			i++
			egressFlag = args[i]
		case strings.HasPrefix(arg, "--egress-proxy="):
			egressFlag = strings.TrimPrefix(arg, "--egress-proxy=")
		default:
			return fmt.Errorf("version: unknown flag %s (supported: --check, --egress-proxy)", arg)
		}
	}
	current := versionString()
	fmt.Println("ccwrap " + current)
	if !check {
		return nil
	}
	latest, err := syncUpdateCheck(paths.StateDir, egressFlag)
	if err != nil {
		return fmt.Errorf("version check failed: %w\n(network issue? try --egress-proxy auto|direct|URL)", err)
	}
	if update.Newer(current, latest) {
		fmt.Printf("update available: %s → %s — run `ccwrap upgrade`\n", current, latest)
	} else {
		fmt.Printf("up to date (latest is %s)\n", latest)
	}
	return nil
}
