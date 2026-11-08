package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestStopTargets(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "pkg")
	other := filepath.Join(root, "other")
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	session := func(id, dir, addr string, reachable bool) model.DiscoveredSession {
		return model.DiscoveredSession{Reachable: reachable, Manifest: model.SessionManifest{SessionID: id, ProjectDir: dir, ProxyListenAddr: addr}}
	}
	a := session("aaaa1111aaaa1111", repo, "127.0.0.1:50001", true)
	b := session("bbbb2222bbbb2222", other, "127.0.0.1:50002", true)
	dead := session("dddd4444dddd4444", root, "127.0.0.1:50004", false)
	snapshot := func(ds model.DiscoveredSession) (*model.Session, error) {
		return &model.Session{ID: ds.Manifest.SessionID, ProjectDir: ds.Manifest.ProjectDir}, nil
	}
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	none := env(nil)
	sessions := []model.DiscoveredSession{a, b, dead}

	cases := []struct {
		name       string
		discovered []model.DiscoveredSession
		all        bool
		session    string
		args       []string
		env        func(string) string
		cwd        string
		want       string // stopped session IDs, or an error substring prefixed with "!"
		skipped    string
	}{
		{name: "cwd in a project", discovered: sessions, env: none, cwd: sub, want: "aaaa1111aaaa1111"},
		{name: "inside a session", discovered: sessions, env: env(map[string]string{sessionIDEnv: "bbbb2222bbbb2222"}), cwd: sub, want: "bbbb2222bbbb2222"},
		{name: "only one running", discovered: []model.DiscoveredSession{b, dead}, env: none, cwd: root, want: "bbbb2222bbbb2222"},
		{name: "several, no hint", discovered: sessions, env: none, cwd: root, want: "!several sessions are running; pick one with `ccwrap stop"},
		{name: "nothing running", discovered: []model.DiscoveredSession{dead}, env: none, cwd: root, want: "!no running ccwrap session"},
		{name: "id prefix", discovered: sessions, args: []string{"bbbb"}, env: none, cwd: sub, want: "bbbb2222bbbb2222"},
		{name: "project name", discovered: sessions, args: []string{"other"}, env: none, cwd: sub, want: "bbbb2222bbbb2222"},
		{name: "--session short id", discovered: sessions, session: "aaaa1111", env: none, cwd: root, want: "aaaa1111aaaa1111"},
		{name: "--session unreachable", discovered: sessions, session: "dddd", env: none, cwd: root, want: "!session dddd4444dddd4444 is not reachable"},
		{name: "--session unknown", discovered: sessions, session: "zz", env: none, cwd: root, want: "!session zz not found"},
		{name: "--all", discovered: sessions, all: true, env: none, cwd: root, want: "aaaa1111aaaa1111 bbbb2222bbbb2222", skipped: "dddd4444dddd4444"},
		{name: "--all, nothing running", discovered: []model.DiscoveredSession{dead}, all: true, env: none, cwd: root, want: "!no running ccwrap session", skipped: "dddd4444dddd4444"},
		{name: "--all and a session", discovered: sessions, all: true, args: []string{"bbbb"}, env: none, cwd: root, want: "!--all stops every session"},
		{name: "--session and an argument", discovered: sessions, session: "aaaa", args: []string{"bbbb"}, env: none, cwd: root, want: "!name the session once"},
		{name: "two arguments", discovered: sessions, args: []string{"bbbb", "--all"}, env: none, cwd: root, want: "!takes one session, got: bbbb --all"},
	}
	for _, tc := range cases {
		targets, skipped, err := stopTargets(tc.discovered, tc.all, tc.session, tc.args, snapshot, tc.env, tc.cwd)
		if got := strings.Join(skipped, " "); got != tc.skipped {
			t.Errorf("%s: skipped %q, want %q", tc.name, got, tc.skipped)
		}
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, want)
			}
			continue
		}
		var ids []string
		for _, c := range targets {
			ids = append(ids, c.ds.Manifest.SessionID)
		}
		slices.Sort(ids)
		if err != nil || strings.Join(ids, " ") != tc.want {
			t.Errorf("%s: stopped %v, %v; want %s", tc.name, ids, err, tc.want)
		}
	}
}
