package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestScanMarksPIDReuseAsStale(t *testing.T) {
	tmp := t.TempDir()
	paths := app.Paths{RuntimeDir: filepath.Join(tmp, "run"), StateDir: filepath.Join(tmp, "state")}
	sessionID := "sess-reused"
	sessionDir := paths.SessionDir(sessionID)
	m := model.SessionManifest{
		SessionID:            sessionID,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
		State:                model.StateActive,
		SupervisorPID:        os.Getpid(),
		SupervisorStartToken: "definitely-not-the-current-process",
		ControlSocket:        filepath.Join(sessionDir, "control.sock"),
	}
	if err := manifest.Write(manifest.Path(sessionDir), m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	sessions, err := Scan(paths)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Scan() sessions = %d, want 1", len(sessions))
	}
	if !sessions[0].Stale {
		t.Fatalf("session should be stale when supervisor start token mismatches")
	}
	if !strings.Contains(sessions[0].Error, "pid reused") {
		t.Fatalf("stale error = %q, want pid reused", sessions[0].Error)
	}
}

func TestCleanupWithEmptyManifestSessionIDDoesNotRemoveSessionsRoot(t *testing.T) {
	tmp := t.TempDir()
	paths := app.Paths{RuntimeDir: filepath.Join(tmp, "run"), StateDir: filepath.Join(tmp, "state")}
	badID := "sess-empty"
	badDir := paths.SessionDir(badID)
	keepDir := paths.SessionDir("sess-keep")
	if err := os.MkdirAll(keepDir, 0o700); err != nil {
		t.Fatalf("create keep dir: %v", err)
	}
	m := model.SessionManifest{
		SessionID:     "",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		State:         model.StateActive,
		SupervisorPID: 0,
		ControlSocket: filepath.Join(badDir, "control.sock"),
	}
	if err := manifest.Write(manifest.Path(badDir), m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	removed, err := Cleanup(paths)
	if err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != badID {
		t.Fatalf("Cleanup() removed = %#v, want [%s]", removed, badID)
	}
	if _, err := os.Stat(paths.SessionsDir()); err != nil {
		t.Fatalf("sessions root should still exist: %v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Fatalf("unrelated session dir should still exist: %v", err)
	}
}

// TestCleanupSweepsBodiesDirOfDeadSession is the regression
// guard: `ccwrap gc` (discovery.Cleanup) RemoveAll's the whole
// <RuntimeDir>/sessions/<id> dir for a dead session, so the per-session
// bodies/ subdir (<sessionRuntimeDir>/bodies/) is swept for free, while
// a live session's bodies/ is untouched. Liveness is the real staleState
// signal: empty start-token + a live SupervisorPID ⇒ not stale.
func TestCleanupSweepsBodiesDirOfDeadSession(t *testing.T) {
	tmp := t.TempDir()
	paths := app.Paths{RuntimeDir: filepath.Join(tmp, "run"), StateDir: filepath.Join(tmp, "state")}

	deadID := "sess-dead"
	deadDir := paths.SessionDir(deadID)
	deadBodies := filepath.Join(deadDir, "bodies")
	if err := os.MkdirAll(deadBodies, 0o700); err != nil {
		t.Fatalf("create dead bodies dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deadBodies, "x.json"), []byte(`{"body":1}`), 0o600); err != nil {
		t.Fatalf("write dead body file: %v", err)
	}
	deadManifest := model.SessionManifest{
		SessionID:     deadID,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		State:         model.StateActive,
		SupervisorPID: 0, // invalid pid ⇒ staleState() reports dead
		ControlSocket: filepath.Join(deadDir, "control.sock"),
	}
	if err := manifest.Write(manifest.Path(deadDir), deadManifest); err != nil {
		t.Fatalf("write dead manifest: %v", err)
	}

	liveID := "sess-live"
	liveDir := paths.SessionDir(liveID)
	liveBodies := filepath.Join(liveDir, "bodies")
	if err := os.MkdirAll(liveBodies, 0o700); err != nil {
		t.Fatalf("create live bodies dir: %v", err)
	}
	liveBodyFile := filepath.Join(liveBodies, "y.json")
	if err := os.WriteFile(liveBodyFile, []byte(`{"body":2}`), 0o600); err != nil {
		t.Fatalf("write live body file: %v", err)
	}
	liveManifest := model.SessionManifest{
		SessionID:     liveID,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		State:         model.StateActive,
		SupervisorPID: os.Getpid(), // empty token + live pid ⇒ staleState() not stale
		ControlSocket: filepath.Join(liveDir, "control.sock"),
	}
	if err := manifest.Write(manifest.Path(liveDir), liveManifest); err != nil {
		t.Fatalf("write live manifest: %v", err)
	}

	removed, err := Cleanup(paths)
	if err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != deadID {
		t.Fatalf("Cleanup() removed = %#v, want [%s]", removed, deadID)
	}
	if _, err := os.Stat(deadBodies); !os.IsNotExist(err) {
		t.Fatalf("dead session bodies/ must be swept, stat err = %v", err)
	}
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("dead session dir must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(liveBodyFile); err != nil {
		t.Fatalf("live session body file must be untouched: %v", err)
	}
}

func TestScanMarksManifestSessionIDMismatchAsStale(t *testing.T) {
	tmp := t.TempDir()
	paths := app.Paths{RuntimeDir: filepath.Join(tmp, "run"), StateDir: filepath.Join(tmp, "state")}
	dirID := "sess-dir"
	sessionDir := paths.SessionDir(dirID)
	m := model.SessionManifest{
		SessionID:     "sess-other",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		State:         model.StateActive,
		SupervisorPID: os.Getpid(),
		ControlSocket: filepath.Join(sessionDir, "control.sock"),
	}
	if err := manifest.Write(manifest.Path(sessionDir), m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sessions, err := Scan(paths)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Scan() sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Manifest.SessionID != dirID {
		t.Fatalf("discovered session id = %q, want directory id %q", sessions[0].Manifest.SessionID, dirID)
	}
	if !sessions[0].Stale {
		t.Fatal("mismatched manifest should be stale")
	}
	if !strings.Contains(sessions[0].Error, "does not match directory") {
		t.Fatalf("stale error = %q, want directory mismatch", sessions[0].Error)
	}
}
