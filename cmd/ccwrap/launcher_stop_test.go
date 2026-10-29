package main

import (
	"bufio"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// startChild starts sh -c script; the test reaps it. A script that writes
// "$READY" once its traps are set is waited for, so no signal arrives first.
func startChild(t *testing.T, script string, args ...string) *exec.Cmd {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("sh", append([]string{"-c", script}, args...)...)
	cmd.Env = append(os.Environ(), "READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if strings.Contains(script, "$READY") {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("child did not get ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return cmd
}

// waitChild reaps cmd, failing the test if it is still running after d.
func waitChild(t *testing.T, cmd *exec.Cmd, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("child still running after %v", d)
		return nil
	}
}

func killedBy(err error, sig syscall.Signal) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == sig
}

// A stop signal reaches Claude Code, which finishes its own exit handling.
func TestRelaySignalsLetsClaudeExitOnItsOwn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned-up")
	cmd := startChild(t, `trap 'sleep 0.3; : > "$0"; exit 0' TERM; : > "$READY"; while :; do sleep 0.05; done`, marker)
	sigCh := make(chan os.Signal, 1)
	noted := make(chan os.Signal, 1)
	stop := relaySignals(sigCh, cmd.Process, func(s os.Signal) { noted <- s }, stopGrace, stopRepeatGap)
	defer stop()

	sigCh <- syscall.SIGTERM
	if err := waitChild(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("child exit = %v, want a clean exit after its TERM handler ran", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the child's TERM handler did not finish: %v", err)
	}
	if s := <-noted; s != syscall.SIGTERM {
		t.Fatalf("onStop got %v, want SIGTERM", s)
	}
}

// Claude Code that ignores the stop signal is killed once the grace ends.
func TestRelaySignalsKillsClaudeAfterGrace(t *testing.T) {
	cmd := startChild(t, `trap '' TERM; : > "$READY"; while :; do sleep 0.05; done`)
	sigCh := make(chan os.Signal, 1)
	stop := relaySignals(sigCh, cmd.Process, nil, 200*time.Millisecond, stopRepeatGap)
	defer stop()

	sigCh <- syscall.SIGTERM
	if err := waitChild(t, cmd, 5*time.Second); !killedBy(err, syscall.SIGKILL) {
		t.Fatalf("child exit = %v, want SIGKILL after the grace period", err)
	}
}

// A second stop signal kills Claude Code without waiting out the grace.
func TestRelaySignalsSecondSignalKillsClaude(t *testing.T) {
	cmd := startChild(t, `trap '' TERM HUP; : > "$READY"; while :; do sleep 0.05; done`)
	sigCh := make(chan os.Signal, 2)
	stop := relaySignals(sigCh, cmd.Process, nil, time.Minute, 100*time.Millisecond)
	defer stop()

	sigCh <- syscall.SIGHUP
	time.Sleep(200 * time.Millisecond)
	sigCh <- syscall.SIGTERM
	if err := waitChild(t, cmd, 5*time.Second); !killedBy(err, syscall.SIGKILL) {
		t.Fatalf("child exit = %v, want SIGKILL on the second signal", err)
	}
}

// Closing a terminal delivers SIGHUP more than once within milliseconds (the
// shell passes it on to its jobs, the kernel sends it to the foreground
// group). Such a burst is one stop, not a second one asking to kill.
func TestRelaySignalsBurstIsOneStop(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned-up")
	cmd := startChild(t, `trap 'sleep 0.5; : > "$0"; exit 0' HUP; : > "$READY"; while :; do sleep 0.05; done`, marker)
	sigCh := make(chan os.Signal, 3)
	stop := relaySignals(sigCh, cmd.Process, nil, time.Minute, time.Second)
	defer stop()

	sigCh <- syscall.SIGHUP
	sigCh <- syscall.SIGHUP
	sigCh <- syscall.SIGHUP
	if err := waitChild(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("child exit = %v, want a clean exit after its HUP handler ran", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the child's HUP handler did not finish: %v", err)
	}
}

// Wait reports a signal-ended Claude Code the way a shell does, 128+n, and
// a stop signal names the session's end reason.
func TestWait_SignalExitStatusAndReason(t *testing.T) {
	cmd := startChild(t, `kill -TERM $$`)
	l := &sessionLauncher{cmd: cmd, supervisorErr: make(chan error, 1)}
	l.noteStopSignal(syscall.SIGTERM)
	code, err := l.Wait()
	if err != nil || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("Wait = %d, %v; want %d, nil", code, err, 128+int(syscall.SIGTERM))
	}
	if l.closeReason != "stopped by SIGTERM" {
		t.Fatalf("closeReason = %q, want stopped by SIGTERM", l.closeReason)
	}
}

// ccwrap stop and a closed terminal end the session the way an exit does:
// the supervisor keeps serving while Claude Code exits, Wait returns Claude
// Code's status instead of a supervisor error, and teardown announces the
// end on the session's event stream.
func TestStopSignalAnnouncesTheEnd(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "st.sock")
	l := newSessionLauncher(paths, launchArgs{}, &preflight.Result{}, &settings.InspectionResult{}, preflight.Options{}, t.TempDir(), "st00000000000001")
	for _, step := range []func() error{l.PreparePaths, l.StartSupervisor, l.AwaitControl, l.CreateSession} {
		if err := step(); err != nil {
			l.teardown()
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "http://unix/api/v1/events", nil)
	resp, err := (&http.Client{Transport: control.UnixTransport(l.sessionPaths.SocketPath)}).Do(req)
	if err != nil {
		l.teardown()
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event: ") {
				events <- strings.TrimPrefix(line, "event: ")
			}
		}
		close(events)
	}()
	if ev := <-events; ev != "hello" {
		t.Fatalf("first event %q", ev)
	}

	l.cmd = startChild(t, `trap 'sleep 0.3; exit 0' TERM; : > "$READY"; while :; do sleep 0.05; done`)
	sigCh := make(chan os.Signal, 1)
	l.rollback.push(relaySignals(sigCh, l.cmd.Process, l.noteStopSignal, stopGrace, stopRepeatGap))
	sigCh <- syscall.SIGTERM
	code, err := l.Wait()
	if err != nil || code != 0 {
		l.teardown()
		t.Fatalf("Wait = %d, %v; want Claude Code's own exit (0, nil)", code, err)
	}
	if l.closeReason != "stopped by SIGTERM" {
		t.Fatalf("closeReason = %q", l.closeReason)
	}
	l.teardown()
	var got []string
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) == 0 || got[len(got)-1] != "session.ended" {
		t.Fatalf("events after teardown = %v", got)
	}
}

// A non-zero Claude Code exit reaches main as exitCodeError, so runClaude's
// deferred teardown runs before the process exits with that code.
func TestExitFor(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		msg  string
	}{
		{nil, 0, ""},
		{exitCodeError{code: 3}, 3, ""},
		{errors.New("boom"), 1, "boom"},
	} {
		code, msg := exitFor(tc.err)
		if code != tc.code || msg != tc.msg {
			t.Errorf("exitFor(%v) = %d, %q; want %d, %q", tc.err, code, msg, tc.code, tc.msg)
		}
	}
}
