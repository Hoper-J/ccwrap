package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// helperCommand runs this test binary as a stand-in child process, where a
// shell one-liner would need sh: "exit N" exits with status N, "sleep D"
// sleeps for the duration D. See TestHelperProcess.
func helperCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "CCWRAP_TEST_HELPER_PROCESS=1")
	return cmd
}

// TestHelperProcess is the child that helperCommand starts; run as a test it
// does nothing.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CCWRAP_TEST_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 3 {
		switch args[1] {
		case "exit":
			if n, err := strconv.Atoi(args[2]); err == nil {
				os.Exit(n)
			}
		case "sleep":
			if d, err := time.ParseDuration(args[2]); err == nil {
				time.Sleep(d)
				os.Exit(0)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "helper process: bad arguments %q\n", args)
	os.Exit(2)
}
