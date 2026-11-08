package update

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRunningProgramHelper stands in for a running ccwrap; as a test it
// does nothing.
func TestRunningProgramHelper(t *testing.T) {
	if os.Getenv("CCWRAP_TEST_RUNNING") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// Upgrading replaces ccwrap.exe while a copy of it runs, as a session or
// ccwrap ui does during `ccwrap upgrade`; the old file goes once it is free.
func TestApplyReplacesARunningProgram(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap.exe")
	if err := os.WriteFile(exe, program, 0o755); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(exe, "-test.run=^TestRunningProgramHelper$")
	running.Env = append(os.Environ(), "CCWRAP_TEST_RUNNING=1")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Process.Kill(); _, _ = running.Process.Wait() }()

	newBin := []byte("NEW BINARY BYTES")
	archive, sum := makeArchive(t, newBin)
	srv := releaseServer(t, "0.3.1", archive, fmt.Sprintf("%s  %s\n", sum, releaseArchive("0.3.1", runtime.GOOS, runtime.GOARCH)))
	defer srv.Close()
	if err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard); err != nil {
		t.Fatalf("Apply over a running program: %v", err)
	}
	if got, err := os.ReadFile(exe); err != nil || !bytes.Equal(got, newBin) {
		t.Fatalf("ccwrap.exe not replaced: %v", err)
	}
	olds, _ := filepath.Glob(exe + ".old-*")
	if len(olds) != 1 {
		t.Fatalf("moved-aside files = %v, want the running one", olds)
	}
	RemoveReplaced(exe)
	if _, err := os.Stat(olds[0]); err != nil {
		t.Fatalf("removed the file of a running program: %v", err)
	}
	_ = running.Process.Kill()
	_, _ = running.Process.Wait()
	for i := 0; i < 50; i++ { // the system lets go of the file shortly after the exit
		RemoveReplaced(exe)
		if _, err := os.Stat(olds[0]); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("moved-aside file still there once nothing ran it")
}
