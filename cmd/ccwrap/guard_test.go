//go:build !linux && !windows

package main

import (
	"bufio"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/procmeta"
)

// startClaudeStandIn starts a helper process in mode for a minute, standing
// in for Claude Code; exited tells how it ended.
func startClaudeStandIn(t *testing.T, mode string) (pid int, token string, exited <-chan *os.ProcessState) {
	t.Helper()
	cmd := helperCommand(mode, "1m")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if mode == "deaf-sleep" {
		_, _ = bufio.NewReader(out).ReadString('\n') // SIGTERM is ignored from here on
	}
	token, err = procmeta.StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *os.ProcessState, 1)
	go func() {
		st, _ := cmd.Process.Wait()
		ch <- st
	}()
	return cmd.Process.Pid, token, ch
}

// guardWith runs guardClaude with ccwrap as a pipe; closing the returned
// file stands for ccwrap exiting.
func guardWith(t *testing.T, pid int, token string, grace time.Duration) (ccwrap *os.File, done <-chan struct{}) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{})
	go func() {
		defer r.Close()
		guardClaude(r, pid, token, grace)
		close(ch)
	}()
	return w, ch
}

func endedBy(t *testing.T, exited <-chan *os.ProcessState, within time.Duration) syscall.Signal {
	t.Helper()
	select {
	case st := <-exited:
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal()
		}
		return 0
	case <-time.After(within):
		t.Fatalf("Claude Code still running %s on", within)
		return 0
	}
}

// While ccwrap runs the guard leaves Claude Code alone; once ccwrap is gone
// it sends SIGTERM.
func TestGuardEndsClaudeCodeOnceCcwrapIsGone(t *testing.T) {
	pid, token, exited := startClaudeStandIn(t, "sleep")
	ccwrap, done := guardWith(t, pid, token, 5*time.Second)
	select {
	case <-exited:
		t.Fatal("Claude Code ended while ccwrap ran")
	case <-time.After(300 * time.Millisecond):
	}
	ccwrap.Close()
	if sig := endedBy(t, exited, 5*time.Second); sig != syscall.SIGTERM {
		t.Fatalf("Claude Code ended by %v, want SIGTERM", sig)
	}
	<-done
}

// Claude Code still running grace after the SIGTERM gets SIGKILL.
func TestGuardKillsClaudeCodeThatOutlastsTheGrace(t *testing.T) {
	pid, token, exited := startClaudeStandIn(t, "deaf-sleep")
	ccwrap, done := guardWith(t, pid, token, 300*time.Millisecond)
	ccwrap.Close()
	if sig := endedBy(t, exited, 5*time.Second); sig != syscall.SIGKILL {
		t.Fatalf("Claude Code ended by %v, want SIGKILL", sig)
	}
	<-done
}

// A pid whose start token differs is another process now: no signal.
func TestGuardLeavesAProcessThatReusedThePid(t *testing.T) {
	pid, _, exited := startClaudeStandIn(t, "sleep")
	ccwrap, done := guardWith(t, pid, "ps:Thu Jan 1 00:00:00 1970", time.Second)
	ccwrap.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guard still waiting")
	}
	select {
	case <-exited:
		t.Fatal("the guard signaled a process that reused the pid")
	case <-time.After(300 * time.Millisecond):
	}
}
