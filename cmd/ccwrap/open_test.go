package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func openCand(id, dir, addr, profile string, age time.Duration) openCandidate {
	return openCandidate{
		ds:   model.DiscoveredSession{Reachable: true, Manifest: model.SessionManifest{SessionID: id, ProjectDir: dir, ProxyListenAddr: addr, CreatedAt: time.Now().Add(-age)}},
		sess: &model.Session{ID: id, ProjectDir: dir, ActiveProfileName: profile},
	}
}

func TestPickOpenSession(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "pkg", "x")
	other := filepath.Join(root, "other")
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := openCand("aaaa1111", repo, "127.0.0.1:50001", "deepseek", time.Hour)
	b := openCand("bbbb2222", other, "127.0.0.1:50002", "official", time.Minute)
	nested := openCand("cccc3333", filepath.Join(repo, "pkg"), "127.0.0.1:50003", "", time.Minute)
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	none := env(nil)

	cases := []struct {
		name string
		live []openCandidate
		arg  string
		env  func(string) string
		cwd  string
		want string // session ID, or an error substring prefixed with "!"
	}{
		{"nothing running", nil, "", none, root, "!no running ccwrap session"},
		{"id prefix", []openCandidate{a, b}, "bbbb", none, root, "bbbb2222"},
		{"project name", []openCandidate{a, b}, "other", none, root, "bbbb2222"},
		{"no match", []openCandidate{a, b}, "zzz", none, root, "!no running session matches"},
		{"inside a session", []openCandidate{a, b}, "", env(map[string]string{sessionIDEnv: "bbbb2222"}), repo, "bbbb2222"},
		{"proxy port", []openCandidate{a, b}, "", env(map[string]string{"HTTPS_PROXY": "http://127.0.0.1:50002"}), repo, "bbbb2222"},
		{"cwd in project", []openCandidate{a, b}, "", none, sub, "aaaa1111"},
		{"deepest project wins", []openCandidate{a, b, nested}, "", none, sub, "cccc3333"},
		{"only one", []openCandidate{b}, "", none, root, "bbbb2222"},
		{"several, no hint", []openCandidate{a, b}, "", none, root, "!several sessions are running"},
	}
	for _, tc := range cases {
		got, err := pickOpenSession(tc.live, tc.arg, tc.env, tc.cwd)
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

	// The list names each session by folder and profile, oldest first.
	_, err := pickOpenSession([]openCandidate{b, a}, "", none, root)
	if err == nil || !strings.Contains(err.Error(), "aaaa1111") || strings.Index(err.Error(), "aaaa1111") > strings.Index(err.Error(), "bbbb2222") || !strings.Contains(err.Error(), "deepseek") {
		t.Fatalf("list: %v", err)
	}
}

func TestOpenInBrowserChoosesOpener(t *testing.T) {
	var ran []string
	run := func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	link := "http://127.0.0.1:5/"
	cases := []struct {
		goos   string
		env    map[string]string
		opened bool
		ran    string
	}{
		{"darwin", nil, true, "open " + link},
		{"darwin", map[string]string{"SSH_CONNECTION": "1 2 3 4"}, false, ""},
		{"linux", nil, false, ""},
		{"linux", map[string]string{"DISPLAY": ":0"}, true, "xdg-open " + link},
	}
	for _, tc := range cases {
		ran = nil
		opened, err := openInBrowser(link, env(tc.env), tc.goos, run)
		if err != nil || opened != tc.opened || strings.Join(ran, "") != tc.ran {
			t.Errorf("%s %v: opened=%v ran=%q err=%v", tc.goos, tc.env, opened, ran, err)
		}
	}
}
