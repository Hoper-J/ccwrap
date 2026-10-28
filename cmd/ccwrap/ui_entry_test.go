package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/testutil"
	"github.com/Hoper-J/ccwrap/internal/uiservice"
)

func stubEnsureUI(t *testing.T, fn func(app.Paths, time.Duration) (uiservice.Info, error)) {
	t.Helper()
	old := ensureUI
	ensureUI = fn
	t.Cleanup(func() { ensureUI = old })
}

// A launch starts ccwrap ui only when someone will see the link and the
// user has not opted out; the banner waits for it only briefly, and without
// it says to run ccwrap open.
func TestLaunchStartsTheApp(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "e.sock")
	calls := 0
	stubEnsureUI(t, func(app.Paths, time.Duration) (uiservice.Info, error) {
		calls++
		return uiservice.Info{HTTPAddr: "127.0.0.1:47321"}, nil
	})
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s := startUIInBackground(paths, env(map[string]string{noUIEnv: "1"}), true); s != nil {
		t.Fatal("CCWRAP_NO_UI=1 still starts the app")
	}
	if s := startUIInBackground(paths, env(nil), false); s != nil {
		t.Fatal("a launch nobody watches starts the app")
	}
	var none *uiStart
	if link := none.sessionLink("abc", time.Second); link != "" {
		t.Fatalf("no app: %q", link)
	}
	s := startUIInBackground(paths, env(nil), true)
	link := s.sessionLink("abc", time.Second)
	if link != "http://127.0.0.1:47321/#/sessions/abc" || calls != 1 {
		t.Fatalf("app: %q (%d calls)", link, calls)
	}
	if inspectText(link) != link+"  ·  ccwrap ui" || inspectText("") != "ccwrap open  ·  opens this session in ccwrap ui" {
		t.Fatal("inspect row")
	}

	// A slow or failed start leaves the banner without a link.
	release := make(chan struct{})
	stubEnsureUI(t, func(app.Paths, time.Duration) (uiservice.Info, error) {
		<-release
		return uiservice.Info{HTTPAddr: "127.0.0.1:1"}, nil
	})
	slow := startUIInBackground(paths, env(nil), true)
	if link := slow.sessionLink("abc", 20*time.Millisecond); link != "" {
		t.Fatalf("slow app: %q", link)
	}
	close(release)
	<-slow.done // it read ensureUI; let it finish before swapping the stub
	stubEnsureUI(t, func(app.Paths, time.Duration) (uiservice.Info, error) { return uiservice.Info{}, errors.New("boom") })
	if link := startUIInBackground(paths, env(nil), true).sessionLink("abc", time.Second); link != "" {
		t.Fatalf("failed app: %q", link)
	}
}

// ccwrap open with no session opens the app on Projects, also under
// CCWRAP_NO_UI=1 (it only keeps launches from starting the app); when the
// app won't start it says why.
func TestOpenWithoutSessions(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "o.sock")
	stubEnsureUI(t, func(app.Paths, time.Duration) (uiservice.Info, error) {
		return uiservice.Info{HTTPAddr: "127.0.0.1:47321"}, nil
	})
	t.Setenv(noUIEnv, "")
	out := captureStdout(t, func() {
		if err := openCommand(paths, []string{"--print"}); err != nil {
			t.Errorf("open: %v", err)
		}
	})
	if strings.TrimSpace(out) != "http://127.0.0.1:47321/#/projects" {
		t.Fatalf("printed %q", out)
	}
	t.Setenv(noUIEnv, "1")
	out = captureStdout(t, func() {
		if err := openCommand(paths, []string{"--print"}); err != nil {
			t.Errorf("open under CCWRAP_NO_UI=1: %v", err)
		}
	})
	if strings.TrimSpace(out) != "http://127.0.0.1:47321/#/projects" {
		t.Fatalf("CCWRAP_NO_UI=1 printed %q", out)
	}
	if err := openCommand(paths, []string{"--print", "nope"}); err == nil || !strings.Contains(err.Error(), "no running ccwrap session") {
		t.Fatalf("named session: %v", err)
	}
	stubEnsureUI(t, func(app.Paths, time.Duration) (uiservice.Info, error) { return uiservice.Info{}, errors.New("boom") })
	if err := openCommand(paths, []string{"--print"}); err == nil || err.Error() != "can't open ccwrap ui: boom" {
		t.Fatalf("app down: %v", err)
	}
}
