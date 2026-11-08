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
	"github.com/Hoper-J/ccwrap/internal/testutil"
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

// Another user who can write to the runtime dir could plant manifests that
// point ccwrap at their own sockets; Scan refuses the dir instead.
func TestScanRefusesRuntimeDirOthersCanWrite(t *testing.T) {
	tmp := t.TempDir()
	paths := app.Paths{RuntimeDir: filepath.Join(tmp, "run"), StateDir: filepath.Join(tmp, "state")}
	if err := os.MkdirAll(paths.SessionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	testutil.MakeWritableByOthers(t, paths.RuntimeDir)
	if _, err := Scan(paths); err == nil || !strings.Contains(err.Error(), "refusing runtime directory") {
		t.Fatalf("Scan() error = %v, want a refusal", err)
	}
}

// Match takes a session's full ID or a prefix of exactly one session's ID,
// so the 8-character IDs ccwrap prints can name a session.
func TestMatch(t *testing.T) {
	var sessions []model.DiscoveredSession
	for _, id := range []string{"46613c6d8343c8ec", "4661aaaa00000000", "c24c23b73cf888f9", "beef", "beef0001"} {
		sessions = append(sessions, model.DiscoveredSession{Manifest: model.SessionManifest{SessionID: id}})
	}
	for _, tc := range []struct{ id, want string }{
		{"46613c6d8343c8ec", "46613c6d8343c8ec"},
		{"46613c6d", "46613c6d8343c8ec"},
		{"c", "c24c23b73cf888f9"},
		{"beef", "beef"}, // a full ID wins over the IDs it begins
		{"4661", `!"4661" matches several sessions (46613c6d8343c8ec, 4661aaaa00000000)`},
		{"zz", "!session zz not found"},
		{"", "!no session ID given"},
	} {
		got, err := Match(sessions, tc.id)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Match(%q) = %v, %v; want error %q", tc.id, got, err, want)
			}
			continue
		}
		if err != nil || got.Manifest.SessionID != tc.want {
			t.Errorf("Match(%q) = %v, %v; want %s", tc.id, got, err, tc.want)
		}
	}
}
