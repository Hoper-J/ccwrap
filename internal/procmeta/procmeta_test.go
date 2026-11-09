package procmeta

import (
	"os"
	"os/exec"
	"testing"
)

func TestCurrentStartToken(t *testing.T) {
	tok, err := CurrentStartToken()
	if err != nil {
		t.Fatalf("CurrentStartToken() error = %v", err)
	}
	if tok == "" {
		t.Fatal("CurrentStartToken() returned empty token")
	}
}

func TestMatchesCurrentProcess(t *testing.T) {
	tok, err := CurrentStartToken()
	if err != nil {
		t.Fatalf("CurrentStartToken() error = %v", err)
	}
	exists, match, err := Matches(os.Getpid(), tok)
	if err != nil {
		t.Fatalf("Matches() error = %v", err)
	}
	if !exists || !match {
		t.Fatalf("Matches(current pid) = exists=%v match=%v, want true/true", exists, match)
	}
}

func TestStartTokenRejectsInvalidPID(t *testing.T) {
	if _, err := StartToken(-1); err == nil {
		t.Fatal("expected invalid pid error")
	}
}

func TestExists(t *testing.T) {
	if !Exists(os.Getpid()) {
		t.Fatal("Exists(own pid) = false")
	}
	if Exists(-1) || Exists(0) {
		t.Fatal("Exists(invalid pid) = true")
	}
	// The test binary, asked to run no tests, exits at once.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if Exists(cmd.Process.Pid) {
		t.Fatalf("Exists(%d) = true after the process exited", cmd.Process.Pid)
	}
	if _, err := StartToken(cmd.Process.Pid); err == nil {
		t.Fatalf("StartToken(%d) succeeded after the process exited", cmd.Process.Pid)
	}
}
