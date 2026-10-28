package uiservice

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/testutil"
)

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// The page is the app shell with the write token and instance in data
// attributes, served under a CSP that runs nothing inline.
func TestPageShell(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	resp, body := get(t, r.info.URL())
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": pageCSP,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"X-Frame-Options":         "DENY",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, want := range []string{
		`data-token="` + r.srv.token + `"`, `data-instance="` + r.info.InstanceID + `"`, `data-address="` + r.info.HTTPAddr + `"`,
		`<script type="module" src="/assets/js/main.js"></script>`, `<symbol id="i-panel-left"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Nothing inline: no script bodies, no style attributes or elements.
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<\s]`).MatchString(body) || strings.Contains(body, "style=") || strings.Contains(body, "<style") {
		t.Fatal("the page has inline script or style, which its CSP blocks")
	}
	if !strings.Contains(pageCSP, "script-src 'self';") || strings.Contains(pageCSP, "unsafe") {
		t.Fatalf("CSP = %q", pageCSP)
	}
}

// Assets come with the types ES modules and stylesheets need; nothing else
// under /assets/ is served.
func TestPageAssets(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	for path, typ := range map[string]string{"assets/js/main.js": "text/javascript", "assets/css/app.css": "text/css"} {
		resp, _ := get(t, r.info.URL()+path)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), typ) || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	for _, path := range []string{"assets/index.html", "assets/icons.svg", "assets/js/", "assets/js/nope.js", "assets/../page.go"} {
		if resp, _ := get(t, r.info.URL()+path); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

// The page's code builds DOM from nodes and text only, and sets no inline
// style (the CSP would block it).
func TestPageScriptsUseNoHTMLStrings(t *testing.T) {
	banned := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*['"]style|\bstyle:\s`)
	err := fs.WalkDir(webFS, "web/js", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := webFS.ReadFile(path)
		for i, line := range strings.Split(string(data), "\n") {
			if banned.MatchString(line) {
				t.Errorf("%s:%d: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runModule runs an ES module script with node against a copy of the
// page's modules (imported as $JS/<file>.js, in a package marked as ES
// modules) and returns its stdout.
func runModule(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	err := fs.WalkDir(webFS, "web/js", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, "js", strings.TrimPrefix(path, "web/js"))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := webFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "t.js")
	if err := os.WriteFile(f, []byte(strings.ReplaceAll(script, "$JS/", "./js/")), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", f)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s%s", err, out, stderr.String())
	}
	return string(out)
}

func TestPageTimes(t *testing.T) {
	out := runModule(t, `
import { span, ago, since } from '$JS/fmt.js';
const now = Date.parse('2026-10-28T10:00:00Z');
console.log(JSON.stringify([span(40), span(720), span(3*3600+5), span(2*86400), ago('2026-10-28T09:59:20Z', now), since('2026-10-28T09:48:00Z', now), ago('', now), since(undefined, now), ago('0001-01-01T00:00:00Z', now)]));
`)
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	want := []string{"40s", "12m", "3h", "2d", "40s ago", "12m", "", "", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPageRoutes(t *testing.T) {
	out := runModule(t, `
import { parseRoute, landingSession } from '$JS/router.js';
const routes = ['', '#/', '#/sessions/abc', '#/projects', '#/profiles', '#/profiles/new', '#/profiles/deep%20seek', '#/nope'].map(parseRoute);
const sessions = [
  { id: 'old', state: 'active', started_at: '2026-10-28T09:00:00Z' },
  { id: 'new', state: 'attached', started_at: '2026-10-28T09:30:00Z' },
  { id: 'ended', state: 'ended', started_at: '2026-10-28T09:45:00Z' },
];
console.log(JSON.stringify({ routes, landing: landingSession(sessions).id, none: landingSession([sessions[2]]) }));
`)
	var got struct {
		Routes  []map[string]any
		Landing string
		None    any
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	b, _ := json.Marshal(got.Routes)
	want := `[{"id":"","view":"session"},{"id":"","view":"session"},{"id":"abc","view":"session"},{"view":"projects"},{"view":"profiles"},{"isNew":true,"name":"","view":"profile"},{"isNew":false,"name":"deep seek","view":"profile"},{"id":"","view":"session"}]`
	if string(b) != want || got.Landing != "new" || got.None != nil {
		t.Fatalf("routes %s, landing %q, none %v", b, got.Landing, got.None)
	}
}

func TestPageConnectionStates(t *testing.T) {
	out := runModule(t, `
import { connState } from '$JS/api.js';
const run = (events) => events.reduce((s, e) => { const n = connState(s, e); return n; }, 'connecting');
console.log(JSON.stringify([
  run(['error']),                     // never connected, still trying
  run(['error', 'giveup']),           // gave up
  run(['hello']),
  run(['hello', 'error']),            // dropped
  run(['hello', 'error', 'hello']),
  run(['hello', 'error', 'giveup', 'retry']),
  run(['hello', 'error', 'giveup', 'error']),
]));
`)
	want := `["connecting","disconnected","connected","reconnecting","connected","reconnecting","disconnected"]`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

// The sidebar keeps the session being viewed after the server drops it,
// tells same-named sessions apart, and words the times.
func TestPageSidebarItems(t *testing.T) {
	out := runModule(t, `
import { sessionItems } from '$JS/shell/sidebar.js';
import { mergeSessions } from '$JS/store.js';
const now = Date.parse('2026-10-28T10:00:00Z');
const a = { id: '1d6dafe2', state: 'active', health: 'warn', started_at: '2026-10-28T09:48:00Z', display: { title: 'ccwrap', subtitle: 'deepseek', badge: '2 issues' } };
const b = { id: '9f2c3b7e', state: 'active', health: 'ok', started_at: '2026-10-28T09:02:00Z', display: { title: 'ccwrap', subtitle: 'official' } };
const c = { id: 'b3e8d1f0', state: 'ended', health: 'ok', started_at: '2026-10-28T09:30:00Z', ended_at: '2026-10-28T09:59:20Z', display: { title: 'dotfiles', subtitle: 'official', badge: '' } };
const items = sessionItems([b, c, a], 'b3e8d1f0', now);
const kept = mergeSessions([b], [b, c], 'b3e8d1f0').map((s) => s.id + ':' + s.state);
const dropped = mergeSessions([b], [b, c], '').map((s) => s.id);
console.log(JSON.stringify({ items, kept, dropped }));
`)
	var got struct {
		Items []struct {
			ID, Title, Dup, Sub, Badge, Health, Tip, Mono string
			Ended, Current                                bool
		}
		Kept, Dropped []string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	b, c, a := got.Items[0], got.Items[1], got.Items[2]
	if b.Dup != " · 9f2c" || a.Dup != " · 1d6d" || c.Dup != "" {
		t.Errorf("dup suffixes: %q %q %q", b.Dup, a.Dup, c.Dup)
	}
	if a.Sub != "deepseek · 12m" || a.Badge != "2 issues" || a.Health != "warn" || a.Tip != "ccwrap — deepseek · 12m · 2 issues" || a.Mono != "cc" {
		t.Errorf("live item: %+v", a)
	}
	if c.Sub != "ended 40s ago" || c.Health != "ended" || !c.Ended || !c.Current || c.Badge != "" {
		t.Errorf("ended item: %+v", c)
	}
	if strings.Join(got.Kept, ",") != "9f2c3b7e:active,b3e8d1f0:ended" || strings.Join(got.Dropped, ",") != "9f2c3b7e" {
		t.Errorf("kept %v, dropped %v", got.Kept, got.Dropped)
	}
}

func TestPageEmptyStateChoices(t *testing.T) {
	out := runModule(t, `
import { recentProjects, launchProfile } from '$JS/views/empty.js';
const ps = [
  { path: '/a', last_active_at: '2026-10-27T10:00:00Z', last_title: 'x', has_conversation: true },
  { path: '/b', last_active_at: '2026-10-28T10:00:00Z', last_title: 'y', has_conversation: true },
  { path: '/c', last_active_at: '2026-10-26T10:00:00Z', last_title: 'z', has_conversation: true },
  { path: '/d', last_active_at: '2026-10-28T11:00:00Z', has_conversation: true },
  { path: '/e', last_active_at: '0001-01-01T00:00:00Z' },
  { path: '/f', last_active_at: '2026-10-29T10:00:00Z', last_profile: 'deepseek' },
];
const names = ['deepseek', 'official'];
console.log(JSON.stringify({
  recent: recentProjects(ps).map((p) => p.path),
  profiles: [launchProfile({ last_profile: 'deepseek' }, names, 'official'), launchProfile({ last_profile: 'gone' }, names, 'official'),
    launchProfile({ last_profile: null }, names, 'official'), launchProfile({ last_profile: '' }, names, 'official'), launchProfile({ last_profile: null }, names, 'missing')],
}));
`)
	want := `{"recent":["/d","/b","/a"],"profiles":["deepseek","official","official","",""]}`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}
