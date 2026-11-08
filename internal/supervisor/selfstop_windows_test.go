package supervisor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// TestStopSelfHelperProcess stands in for Claude Code; as a test it does
// nothing.
func TestStopSelfHelperProcess(t *testing.T) {
	if os.Getenv("CCWRAP_TEST_STOP_HELPER") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// startStopSession is a supervisor whose session's Claude Code is a helper
// process.
func startStopSession(t *testing.T) (*Supervisor, *exec.Cmd) {
	t.Helper()
	sv, err := New(testutil.ShortAppPaths(t, "st.sock"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sv.createSession(model.SessionCreateRequest{ID: "stop-1", LauncherPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	claude := exec.Command(os.Args[0], "-test.run=^TestStopSelfHelperProcess$")
	claude.Env = append(os.Environ(), "CCWRAP_TEST_STOP_HELPER=1")
	if err := claude.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claude.Process.Kill() })
	token, err := procmeta.StartToken(claude.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := sv.attachSession("stop-1", model.SessionAttachRequest{ClaudePID: claude.Process.Pid, ClaudeStartToken: token}); err != nil {
		t.Fatal(err)
	}
	return sv, claude
}

// stubInterrupt replaces the group Ctrl+C: this test does not lead a group,
// and a console-wide one would reach it and what runs it.
func stubInterrupt(t *testing.T, f func() error) {
	t.Helper()
	old := interruptGroup
	interruptGroup = f
	t.Cleanup(func() { interruptGroup = old })
}

// exitsWithin reports whether p exits within d.
func exitsWithin(p *os.Process, d time.Duration) bool {
	done := make(chan struct{})
	go func() { _, _ = p.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// The session asks Claude Code to exit with a Ctrl+C to its group and leaves
// it to exit by itself.
func TestStopSelfSendsCtrlCToTheGroup(t *testing.T) {
	sv, claude := startStopSession(t)
	sent := 0
	stubInterrupt(t, func() error { sent++; return nil })
	if err := sv.stopSelf(); err != nil {
		t.Fatalf("stopSelf: %v", err)
	}
	if sent != 1 {
		t.Fatalf("Ctrl+C sent %d times, want 1", sent)
	}
	if exitsWithin(claude.Process, 300*time.Millisecond) {
		t.Fatal("stopSelf ended Claude Code instead of leaving it to exit")
	}
}

// Without a group to send the Ctrl+C to, Claude Code is terminated.
func TestStopSelfTerminatesClaudeCodeOtherwise(t *testing.T) {
	sv, claude := startStopSession(t)
	stubInterrupt(t, func() error { return errors.New("not leading a process group") })
	if err := sv.stopSelf(); err != nil {
		t.Fatalf("stopSelf: %v", err)
	}
	if !exitsWithin(claude.Process, 10*time.Second) {
		t.Fatal("Claude Code still running 10s after stopSelf")
	}
}

// Before Claude Code is attached, the supervisor stops itself.
func TestStopSelfWithoutClaudeShutsTheSupervisorDown(t *testing.T) {
	sv, err := New(testutil.ShortAppPaths(t, "sd.sock"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sv.stopSelf(); err != nil {
		t.Fatalf("stopSelf: %v", err)
	}
	select {
	case <-sv.closed:
	default:
		t.Fatal("supervisor not shut down")
	}
}

// The supervisor remembers that ccwrap stop asked it to end the session.
func TestShutdownRequestIsRemembered(t *testing.T) {
	sv, err := New(testutil.ShortAppPaths(t, "sr.sock"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	sv.handleShutdown(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/shutdown", nil))
	if !sv.StopRequested() {
		t.Fatal("StopRequested = false after /v1/shutdown")
	}
	select {
	case <-sv.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor not shut down")
	}
}
