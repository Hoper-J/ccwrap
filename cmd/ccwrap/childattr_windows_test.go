package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Hoper-J/ccwrap/internal/procmeta"
)

// Closing the job ends the child: what happens to Claude Code when ccwrap
// exits without waiting for it.
func TestBindChildLifetimeEndsTheChildWithTheJob(t *testing.T) {
	cmd := helperCommand("sleep", "60s")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err := bindChildLifetime(cmd.Process); err != nil {
		t.Fatalf("bindChildLifetime: %v", err)
	}
	job := childJob
	childJob = 0
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("child exited before its job closed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := windows.CloseHandle(job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child still running 10s after its job closed")
	}
}

// What Claude Code starts is not in the job, so it outlives ccwrap the way
// Claude Code's own update run has to.
func TestBindChildLifetimeLeavesGrandchildrenRunning(t *testing.T) {
	cmd := helperCommand("spawn-sleep", "60s")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err := bindChildLifetime(cmd.Process); err != nil {
		t.Fatalf("bindChildLifetime: %v", err)
	}
	job := childJob
	childJob = 0
	var gpid int
	if _, err := fmt.Fscan(out, &gpid); err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(gpid); err == nil {
			_ = p.Kill()
		}
	})
	if err := windows.CloseHandle(job); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child still running 10s after its job closed")
	}
	if !procmeta.Exists(gpid) {
		t.Fatal("the child's own child ended with the job")
	}
}
