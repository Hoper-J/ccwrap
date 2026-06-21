package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestProfileRm_DoesNotHoldLockDuringSessionScan locks the fix that moved the
// (possibly multi-second, networked) live-session scan OUT from under the
// cross-process file lock. While `rm` is in the scan, the lock must be FREE — a
// concurrent acquirer must not block for the scan duration. If `rm` held the
// lock across the scan (the bug), the concurrent Lock blocks until rm finishes.
func TestProfileRm_DoesNotHoldLockDuringSessionScan(t *testing.T) {
	dir := t.TempDir()
	paths := app.Paths{StateDir: dir, RuntimeDir: dir}
	initial := &profiles.File{
		Default: profiles.OfficialProfileName,
		Profiles: map[string]profiles.Profile{
			profiles.OfficialProfileName: profiles.OfficialProfile(),
			"victim": {
				Name:     "victim",
				Provider: "x",
				BaseURL:  "https://x.example.com",
				Auth:     &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-x"},
				Egress:   profiles.EgressSpec{Mode: "inherit"},
			},
		},
	}
	if err := profiles.OverwriteFile(profiles.DefaultPath(dir), initial, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const scanDur = 800 * time.Millisecond
	scanning := make(chan struct{})
	slowLooker := func(ctx context.Context, _ app.Paths, _ string) []string {
		close(scanning)
		select {
		case <-time.After(scanDur):
		case <-ctx.Done():
		}
		return nil
	}

	rmDone := make(chan int, 1)
	go func() { rmDone <- profileRmWithLooker(paths, []string{"victim"}, slowLooker) }()

	<-scanning // rm is now inside the slow scan
	start := time.Now()
	unlock, err := profiles.Lock(dir)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	unlock()
	if elapsed > scanDur/2 {
		t.Fatalf("Lock blocked %v during the rm session scan — rm holds the cross-process lock across the scan", elapsed)
	}
	if code := <-rmDone; code != 0 {
		t.Fatalf("rm exit code = %d, want 0", code)
	}
}

// TestSP4a_ReloadRecheck_DoesNotClobberSameNameProfile reaches the migration's
// reload-under-lock recheck: a profile already exists under the name the prompt
// resolves to ("local" on non-TTY) but with a DIFFERENT BaseURL — as if a peer
// added it during the prompt. The migration must NOT overwrite it.
func TestSP4a_ReloadRecheck_DoesNotClobberSameNameProfile(t *testing.T) {
	restore := stubIsTerminal(false) // resolveSeedName → "local"
	defer restore()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	parent := []string{
		"ANTHROPIC_BASE_URL=http://3rd.example.com",
		"ANTHROPIC_AUTH_TOKEN=sk-test",
		"PATH=/usr/bin",
	}

	const preservedURL = "http://old.example.com"
	initial := &profiles.File{
		Default: profiles.OfficialProfileName,
		Profiles: map[string]profiles.Profile{
			profiles.OfficialProfileName: profiles.OfficialProfile(),
			// Same NAME as the prompt result, DIFFERENT BaseURL — so the
			// pre-prompt BaseURL skip does NOT fire and we reach the reload
			// recheck.
			"local": {
				Name:     "local",
				Provider: "old",
				BaseURL:  preservedURL,
				Auth:     &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-old"},
				Egress:   profiles.EgressSpec{Mode: "inherit"},
			},
		},
	}
	if err := profiles.OverwriteFile(profiles.DefaultPath(dir), initial, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	maybeMigrateFromEnv(dir, parent, cwd, nil, false)

	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := f.Profiles["local"].BaseURL; got != preservedURL {
		t.Fatalf("name-collision clobber: profile \"local\" BaseURL = %q, want preserved %q (migration overwrote a same-name peer profile)", got, preservedURL)
	}
}
