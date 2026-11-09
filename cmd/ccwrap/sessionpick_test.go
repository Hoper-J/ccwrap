package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func cand(id, dir, addr, profile string, age time.Duration) sessionCandidate {
	return sessionCandidate{
		ds:   model.DiscoveredSession{Reachable: true, Manifest: model.SessionManifest{SessionID: id, ProjectDir: dir, ProxyListenAddr: addr, CreatedAt: time.Now().Add(-age)}},
		sess: &model.Session{ID: id, ProjectDir: dir, ActiveProfileName: profile},
	}
}

func TestPickSession(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "pkg", "x")
	other := filepath.Join(root, "other")
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := cand("aaaa1111", repo, "127.0.0.1:50001", "deepseek", time.Hour)
	b := cand("bbbb2222", other, "127.0.0.1:50002", "official", time.Minute)
	nested := cand("cccc3333", filepath.Join(repo, "pkg"), "127.0.0.1:50003", "", time.Minute)
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	none := env(nil)

	cases := []struct {
		name string
		live []sessionCandidate
		arg  string
		env  func(string) string
		cwd  string
		want string // session ID, or an error substring prefixed with "!"
	}{
		{"nothing running", nil, "", none, root, "!no running ccwrap session"},
		{"id prefix", []sessionCandidate{a, b}, "bbbb", none, root, "bbbb2222"},
		{"project name", []sessionCandidate{a, b}, "other", none, root, "bbbb2222"},
		{"no match", []sessionCandidate{a, b}, "zzz", none, root, "!no running session matches"},
		{"inside a session", []sessionCandidate{a, b}, "", env(map[string]string{sessionIDEnv: "bbbb2222"}), repo, "bbbb2222"},
		{"proxy port", []sessionCandidate{a, b}, "", env(map[string]string{"HTTPS_PROXY": "http://127.0.0.1:50002"}), repo, "bbbb2222"},
		{"cwd in project", []sessionCandidate{a, b}, "", none, sub, "aaaa1111"},
		{"deepest project wins", []sessionCandidate{a, b, nested}, "", none, sub, "cccc3333"},
		{"only one", []sessionCandidate{b}, "", none, root, "bbbb2222"},
		{"several, no hint", []sessionCandidate{a, b}, "", none, root, "!several sessions are running"},
	}
	for _, tc := range cases {
		got, err := pickSession(tc.live, tc.arg, tc.env, tc.cwd, "open")
		if strings.HasPrefix(tc.want, "!") {
			if err == nil || !strings.Contains(err.Error(), tc.want[1:]) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want[1:])
			}
			continue
		}
		if err != nil || got.ds.Manifest.SessionID != tc.want {
			t.Errorf("%s: got %q, %v; want %s", tc.name, got.ds.Manifest.SessionID, err, tc.want)
		}
	}

	// The list names each session by folder and profile, oldest first, and
	// the hint names the command that asked.
	_, err := pickSession([]sessionCandidate{b, a}, "", none, root, "stop")
	if err == nil || !strings.Contains(err.Error(), "aaaa1111") || strings.Index(err.Error(), "aaaa1111") > strings.Index(err.Error(), "bbbb2222") || !strings.Contains(err.Error(), "deepseek") || !strings.Contains(err.Error(), "`ccwrap stop <session-id or project>`") {
		t.Fatalf("list: %v", err)
	}
}
