package main

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestProfileAdd_ConcurrentCrossProcess_NoLostUpdate proves that two
// separate OS processes each running `profile add` against the same
// profiles.json do not lose each other's writes.
//
// This is the cross-process window the in-process profileFileMu (see
// internal/supervisor/server.go) explicitly cannot close: two `ccwrap`
// processes, or a `ccwrap profile` CLI racing the supervisor's
// browser-driven CRUD, each run their own OS process and share only the
// profiles.json file. Each add is a load → mutate → OverwriteFile
// sequence; OverwriteFile finishes with an atomic rename. Without a
// cross-process file lock spanning load→write, the two sequences
// interleave and the second rename clobbers the first writer's freshly
// added profile, so the final file holds fewer than addsPerWorker*2
// profiles — a silently lost update.
//
// The two worker processes are re-execs of this same test binary (the
// CCWRAP_RACE_ROLE branch below), released from a shared wall-clock
// barrier so their add loops overlap maximally.
func TestProfileAdd_ConcurrentCrossProcess_NoLostUpdate(t *testing.T) {
	const addsPerWorker = 80

	// Worker mode: re-exec entry. Each child adds addsPerWorker distinctly
	// named profiles to the shared profiles.json, then exits. Distinct
	// names mean a lost update never surfaces as a name-conflict error —
	// it surfaces only as a missing entry in the final count.
	if role := os.Getenv("CCWRAP_RACE_ROLE"); role != "" {
		waitForBarrier(os.Getenv("CCWRAP_RACE_START_UNIXNANO"))
		stateDir := os.Getenv("CCWRAP_STATE_DIR")
		paths := app.Paths{StateDir: stateDir, RuntimeDir: stateDir}
		for i := range addsPerWorker {
			code := profileAddIO(paths, nil, []string{
				role + "-" + strconv.Itoa(i),
				"--base-url", "https://x.example.com",
				"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-x",
			})
			if code != 0 {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}

	stateDir := t.TempDir()
	startAt := time.Now().Add(400 * time.Millisecond)

	var wg sync.WaitGroup
	for _, role := range []string{"a", "b"} {
		wg.Add(1)
		go func(role string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$")
			cmd.Env = append(os.Environ(),
				"CCWRAP_RACE_ROLE="+role,
				"CCWRAP_STATE_DIR="+stateDir,
				"CCWRAP_RACE_START_UNIXNANO="+strconv.FormatInt(startAt.UnixNano(), 10),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("worker %q failed: %v\n%s", role, err, out)
			}
		}(role)
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	path := profiles.DefaultPath(stateDir)
	f, err := profiles.Load(path)
	if err != nil {
		t.Fatalf("load final profiles.json: %v", err)
	}
	if f == nil {
		t.Fatalf("expected profiles.json to exist after concurrent adds")
	}
	want := addsPerWorker * 2
	if len(f.Profiles) != want {
		t.Fatalf("lost updates: final profiles.json has %d profiles, want %d "+
			"(cross-process load→mutate→write clobbered concurrent writes)",
			len(f.Profiles), want)
	}
}

// waitForBarrier sleeps until the wall-clock instant encoded as unix-nanos
// in raw, so re-exec'd workers begin their add loops simultaneously. An
// empty or unparseable value means "start now".
func waitForBarrier(raw string) {
	if raw == "" {
		return
	}
	ns, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return
	}
	if d := time.Until(time.Unix(0, ns)); d > 0 {
		time.Sleep(d)
	}
}
