//go:build !linux && !windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/procmeta"
)

// guardPipe is the write end of the pipe the guard process reads. It stays
// open for as long as ccwrap runs, on purpose: the guard reads EOF once
// ccwrap is gone, however it ends.
var guardPipe *os.File

// startGuard starts a guard process that ends p if this process ends while
// p still runs: macOS and the BSDs have no Pdeathsig. The guard runs in a
// session of its own, out of reach of the terminal's signals.
func startGuard(p *os.Process) error {
	token, err := procmeta.StartToken(p.Pid)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	cmd := exec.Command(exe, guardCommand, strconv.Itoa(p.Pid), token)
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return err
	}
	go func() { _ = cmd.Wait() }()
	guardPipe = w
	return nil
}

// runGuard is `ccwrap __guard PID START-TOKEN`, the process startGuard
// starts.
func runGuard(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: ccwrap %s PID START-TOKEN", guardCommand)
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return err
	}
	guardClaude(os.Stdin, pid, args[1], stopGrace)
	return nil
}

// guardClaude waits until ccwrap is gone, which EOF on ccwrap tells, and
// then ends Claude Code if it still runs: SIGTERM, as Pdeathsig sends on
// Linux, and SIGKILL if it still runs grace later. The start token keeps a
// pid that now names another process safe.
func guardClaude(ccwrap io.Reader, pid int, token string, grace time.Duration) {
	_, _ = io.Copy(io.Discard, ccwrap)
	running := func() bool {
		_, match, err := procmeta.Matches(pid, token)
		return err == nil && match
	}
	if !running() {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !procmeta.Exists(pid) {
			return
		}
	}
	if running() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
