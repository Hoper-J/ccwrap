package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWait_ChildExitsFirst is the normal path: Claude exits, Wait returns
// its code and never consults supervisorErr (left buffered for the
// rollback drain).
func TestWait_ChildExitsFirst(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	l := &sessionLauncher{cmd: cmd, supervisorErr: make(chan error, 1)}
	code, err := l.Wait()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if l.closeReason != "claude process exited" {
		t.Fatalf("closeReason = %q, want claude process exited", l.closeReason)
	}
	// supervisorErr must be untouched (still drainable).
	select {
	case <-l.supervisorErr:
		t.Fatalf("Wait must not consume supervisorErr on the normal path")
	default:
	}
}

// TestWait_SupervisorDiesFirst is the silent-degradation fix: the
// supervisor goroutine returns while Claude is still running. Wait must
// stop blocking, KILL the now-useless child (its proxy is gone), reap it,
// and return the supervisor cause — instead of hanging on cmd.Wait()
// forever while Claude fails every request against a dead proxy.
func TestWait_SupervisorDiesFirst(t *testing.T) {
	// A long-lived child that would otherwise outlive the test.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	l := &sessionLauncher{cmd: cmd, supervisorErr: make(chan error, 1)}

	sentinel := errors.New("listener closed unexpectedly")
	l.supervisorErr <- sentinel

	done := make(chan struct{})
	var code int
	var err error
	go func() { code, err = l.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("Wait blocked despite supervisor death — the silent-degradation bug is back")
	}

	if code != 0 {
		t.Fatalf("code = %d, want 0 on supervisor-death path", code)
	}
	if err == nil || !strings.Contains(err.Error(), "supervisor exited before Claude") {
		t.Fatalf("err = %v, want a 'supervisor exited before Claude' cause", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err must wrap the supervisor cause, got %v", err)
	}
	if l.closeReason != "supervisor exited before claude" {
		t.Fatalf("closeReason = %q, want supervisor exited before claude", l.closeReason)
	}
	// The child must actually be dead (killed + reaped) — no orphan.
	// ProcessState is non-nil once cmd.Wait returned; a signal-killed
	// child is NOT .Exited() (that is normal-exit only), so assert reaped
	// + not-success rather than Exited().
	if cmd.ProcessState == nil {
		t.Fatalf("child must be reaped after supervisor-death path (ProcessState nil = leaked)")
	}
	if cmd.ProcessState.Success() {
		t.Fatalf("a killed child must not report success; ProcessState=%v", cmd.ProcessState)
	}
}

// TestWait_SupervisorDiesNilError covers a clean (nil) supervisor return
// that still arrives before Claude: the proxy is gone regardless of error,
// so the child is killed and a non-nil cause is surfaced.
func TestWait_SupervisorDiesNilError(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	l := &sessionLauncher{cmd: cmd, supervisorErr: make(chan error, 1)}
	l.supervisorErr <- nil

	done := make(chan struct{})
	var err error
	go func() { _, err = l.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("Wait blocked on nil supervisor return")
	}
	if err == nil || !strings.Contains(err.Error(), "proxy no longer available") {
		t.Fatalf("nil supervisor return must still surface a proxy-gone error, got %v", err)
	}
}
