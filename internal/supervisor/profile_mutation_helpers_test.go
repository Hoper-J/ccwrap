package supervisor

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// newSessionForProfileMutation stands up a Supervisor + a single Session
// suitable for browser-facing /profile/{add,edit,rm,set-default} tests.
// It returns:
//
//   - sess: the model.Session whose ProxyListenAddr the test targets via
//     direct HTTP (no http.ProxyURL wrapper — production fetch() from the
//     web UI hits handleInfoRequest directly with a relative URI).
//   - state: the underlying *sessionState so tests can read profileToken
//     for the X-CCWRAP-Profile-Token header.
//   - dir: the supervisor's StateDir, so tests can seed profiles.json
//     under profiles.DefaultPath(dir).
//
// Mirrors headerInspectorSessionWithSupervisor (proxy_test.go:3194) but
// without the upstream httptest.Server / CA-pool / ProxyURL machinery
// that the probe path needs. Single source of truth for Slice B/C/D/E
// mutation-endpoint tests.
func newSessionForProfileMutation(t *testing.T) (*model.Session, *sessionState, string) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, "pmut.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "pmut"})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.getSession(sess.ID)
	if state == nil {
		t.Fatal("getSession returned nil after CreateSession")
	}
	return sess, state, paths.StateDir
}

// seedTwoProfiles writes a profiles.json under dir containing "alpha"
// (declared as default) and "beta", both minimal-valid (no auth block
// — ccwrap does not own auth — + inherit egress). Cleanup removes the
// file at test end (the StateDir itself is owned by
// testutil.ShortAppPaths).
func seedTwoProfiles(t *testing.T, dir string) {
	t.Helper()
	const body = `{
  "default": "alpha",
  "profiles": {
    "alpha": {
      "provider": "Anthropic",
      "base_url": "https://api.anthropic.com",
      "egress": {"mode": "inherit"}
    },
    "beta": {
      "provider": "Anthropic",
      "base_url": "https://api.anthropic.com",
      "egress": {"mode": "inherit"}
    }
  }
}`
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	path := profiles.DefaultPath(dir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

// readAllString drains r into a string. Returned error mirrors io.ReadAll.
// Wrapped here so tests can write `body, _ := readAllString(resp.Body)`
// without repeating the io.ReadAll + string(...) boilerplate.
func readAllString(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}
