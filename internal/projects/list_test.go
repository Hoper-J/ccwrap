package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t          *testing.T
	claudeHome string
	config     string
	state      string
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	f := &fixture{t: t, claudeHome: filepath.Join(root, ".claude"), config: filepath.Join(root, ".claude.json"), state: filepath.Join(root, "state")}
	for _, d := range []string{f.claudeHome, f.state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) project(name string) string {
	dir := filepath.Join(filepath.Dir(f.claudeHome), "work", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *fixture) config_(projects map[string]bool) {
	cfg := map[string]any{"projects": map[string]any{}}
	for path, trusted := range projects {
		cfg["projects"].(map[string]any)[path] = map[string]any{"hasTrustDialogAccepted": trusted}
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(f.config, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// conversation writes a conversation for the folder dir, its lines given as
// JSON objects.
func (f *fixture) conversation(dir, file string, lines ...map[string]any) string {
	conv := filepath.Join(f.claudeHome, "projects", encodeDir(dir))
	if err := os.MkdirAll(conv, 0o755); err != nil {
		f.t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		data, _ := json.Marshal(l)
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(conv, file)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) sources(running map[string][]string) Sources {
	return Sources{ClaudeConfig: f.config, ClaudeHome: f.claudeHome, StateDir: f.state, Running: running}
}

func TestListMergesClaudeCodeHistoryAndCCWrapState(t *testing.T) {
	f := newFixture(t)
	alpha := f.project("alpha")
	beta := f.project("beta.v2")
	gone := filepath.Join(filepath.Dir(alpha), "gone")
	f.config_(map[string]bool{alpha: true, beta: false, gone: true})
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// beta's conversation started in beta, then moved into a subfolder; the
	// starting folder is found at the head of the file.
	f.conversation(beta, "b.jsonl",
		map[string]any{"type": "user", "cwd": beta, "gitBranch": "main", "timestamp": t0},
		map[string]any{"type": "ai-title", "aiTitle": "Port the parser"},
		map[string]any{"type": "user", "cwd": filepath.Join(beta, "sub"), "gitBranch": "feat/x", "timestamp": t0.Add(time.Hour)},
	)
	// alpha has an older and a newer conversation; the newer one counts.
	old := f.conversation(alpha, "old.jsonl", map[string]any{"type": "ai-title", "aiTitle": "Old title"}, map[string]any{"type": "user", "cwd": alpha, "timestamp": t0.Add(-48 * time.Hour)})
	oldTime := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, oldTime, oldTime)
	f.conversation(alpha, "new.jsonl", map[string]any{"type": "user", "cwd": alpha, "gitBranch": "HEAD", "timestamp": t0.Add(2 * time.Hour)}, map[string]any{"type": "ai-title", "aiTitle": "New title"})
	// gamma exists only as a conversation, not in the config.
	gamma := f.project("gamma")
	f.conversation(gamma, "g.jsonl", map[string]any{"type": "user", "cwd": gamma, "timestamp": t0.Add(-time.Hour)})
	// ccwrap launched in alpha with deepseek, in delta with no profile.
	delta := f.project("delta")
	if err := RememberLaunch(f.state, alpha, "deepseek", t0); err != nil {
		t.Fatal(err)
	}
	if err := RememberLaunch(f.state, delta, "", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	var l Lister
	list := l.List(f.sources(map[string][]string{beta: {"s2", "s1"}}))
	byName := map[string]Project{}
	var order []string
	for _, p := range list {
		byName[p.Name] = p
		order = append(order, p.Name)
	}
	if _, ok := byName["gone"]; ok {
		t.Fatal("a folder that no longer exists is hidden")
	}
	if strings.Join(order, ",") != "delta,alpha,beta.v2,gamma" {
		t.Fatalf("order = %v", order)
	}
	a := byName["alpha"]
	if !a.Trusted || a.LastTitle != "New title" || a.GitBranch != "" || a.LastProfile == nil || *a.LastProfile != "deepseek" {
		t.Fatalf("alpha = %+v", a)
	}
	b := byName["beta.v2"]
	if b.Trusted || b.LastTitle != "Port the parser" || b.GitBranch != "feat/x" || b.LastProfile != nil || strings.Join(b.RunningSessions, ",") != "s1,s2" {
		t.Fatalf("beta = %+v", b)
	}
	if d := byName["delta"]; d.LastProfile == nil || *d.LastProfile != "" {
		t.Fatalf("delta = %+v", d)
	}
	if g := byName["gamma"]; g.Path != Canonical(gamma) || g.Trusted {
		t.Fatalf("gamma = %+v", g)
	}
}

// A folder reached through a symlink is one project: what ccwrap recorded
// under the link (its memory, a running session) meets Claude Code's record
// of the resolved folder.
func TestListMergesSymlinkedFolders(t *testing.T) {
	f := newFixture(t)
	real := Canonical(f.project("real"))
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f.config_(map[string]bool{real: true})
	// Launched through the link with ark, before that in the folder itself
	// with deepseek: the latest launch tells the profile.
	if err := RememberLaunch(f.state, real, "deepseek", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := RememberLaunch(f.state, link, "ark", time.Now()); err != nil {
		t.Fatal(err)
	}
	var l Lister
	list := l.List(f.sources(map[string][]string{link: {"s1"}}))
	if len(list) != 1 || list[0].Path != real || !list[0].Trusted || list[0].LastProfile == nil || *list[0].LastProfile != "ark" ||
		strings.Join(list[0].RunningSessions, ",") != "s1" {
		t.Fatalf("list = %+v", list)
	}
}

// A changed conversation file is read again; an unchanged one comes from
// the cache.
func TestListCachesConversationSummaries(t *testing.T) {
	f := newFixture(t)
	alpha := f.project("alpha")
	f.config_(map[string]bool{alpha: true})
	file := f.conversation(alpha, "a.jsonl", map[string]any{"type": "ai-title", "aiTitle": "First"}, map[string]any{"type": "user", "cwd": alpha})
	var l Lister
	if got := l.List(f.sources(nil))[0].LastTitle; got != "First" {
		t.Fatalf("title = %q", got)
	}
	if len(l.cache) != 1 {
		t.Fatalf("cache = %d entries", len(l.cache))
	}
	data, _ := os.ReadFile(file)
	data = append(data, []byte(`{"type":"ai-title","aiTitle":"Second title"}`+"\n")...)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := l.List(f.sources(nil))[0].LastTitle; got != "Second title" {
		t.Fatalf("title after change = %q", got)
	}
}

func TestEncodeDir(t *testing.T) {
	if got := encodeDir("/Users/home/my.repo_2"); got != "-Users-home-my-repo-2" {
		t.Fatalf("encodeDir = %q", got)
	}
}

// A conversation can be resumed only by its ID, and only among those
// Claude Code keeps for the folder.
func TestHasConversation(t *testing.T) {
	home := t.TempDir()
	dir := Canonical(t.TempDir())
	id := "0b6c2a4e-5f1d-4c3b-9a8e-7d6f5e4c3b2a"
	conv := ConversationDir(home, dir)
	if err := os.MkdirAll(conv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsConversationID(id) || !HasConversation(home, dir, id) {
		t.Fatal("a kept conversation was not found")
	}
	for _, bad := range []string{"", "0b6c2a4e-5f1d-4c3b-9a8e-000000000000", "../" + id, id + ".jsonl", "0B6C2A4E-5F1D-4C3B-9A8E-7D6F5E4C3B2A"} {
		if HasConversation(home, dir, bad) {
			t.Errorf("HasConversation(%q) = true", bad)
		}
	}
	if HasConversation(home, Canonical(t.TempDir()), id) {
		t.Error("found a conversation under another folder")
	}
}

// The Resume list holds the conversations `claude --resume` in the folder
// can open, newest first: files named like Claude Code's session IDs, under
// the folder's own conversation directory, that hold more than bookkeeping.
func TestConversations(t *testing.T) {
	f := newFixture(t)
	dir, other := f.project("dir"), f.project("other")
	at := func(min int) time.Time { return time.Date(2026, 10, 30, 9, min, 0, 0, time.UTC) }
	write := func(dir, file string, mtime time.Time, lines ...map[string]any) string {
		path := f.conversation(dir, file, lines...)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return path
	}
	older, newer, untitled, big := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", "66666666-6666-4666-8666-666666666666"
	write(dir, older+".jsonl", at(1),
		map[string]any{"type": "user", "cwd": dir, "gitBranch": "main", "timestamp": at(1)},
		map[string]any{"type": "ai-title", "aiTitle": "Older work"})
	write(dir, newer+".jsonl", at(6),
		map[string]any{"type": "user", "cwd": dir, "gitBranch": "feature/x", "timestamp": at(5)},
		map[string]any{"type": "assistant", "cwd": dir, "timestamp": at(6)},
		map[string]any{"type": "ai-title", "aiTitle": "Newer work"})
	write(dir, untitled+".jsonl", at(3),
		map[string]any{"type": "user", "cwd": dir, "timestamp": at(3)},
		map[string]any{"type": "last-prompt", "lastPrompt": "why\n does it fail"})
	// Too long to read whole, its tail one unparsable line: still a
	// conversation, untitled, as old as the file.
	path := write(dir, big+".jsonl", at(0), map[string]any{"type": "user", "cwd": dir, "timestamp": at(0)})
	long, _ := os.ReadFile(path)
	long = append(long, []byte(`{"type":"assistant","message":"`+strings.Repeat("x", tailBytes)+`"}`+"\n")...)
	if err := os.WriteFile(path, long, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at(0), at(0)); err != nil {
		t.Fatal(err)
	}
	// Not ones to resume: bookkeeping only, a sidechain, a stray file, a
	// directory, and another folder's conversation.
	write(dir, "44444444-4444-4444-8444-444444444444.jsonl", at(9), map[string]any{"type": "file-history-snapshot", "messageId": "m"})
	write(dir, "agent-1234.jsonl", at(9), map[string]any{"type": "user", "cwd": dir, "timestamp": at(9)})
	write(dir, "notes.txt", at(9), map[string]any{"type": "user"})
	if err := os.MkdirAll(filepath.Join(ConversationDir(f.claudeHome, dir), "55555555-5555-4555-8555-555555555555.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(other, "77777777-7777-4777-8777-777777777777.jsonl", at(9), map[string]any{"type": "user", "cwd": other, "timestamp": at(9)})

	var l Lister
	got := l.Conversations(f.claudeHome, dir, 10)
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, " ") != strings.Join([]string{newer, untitled, older, big}, " ") {
		t.Fatalf("ids = %v", ids)
	}
	if c := got[0]; c.Title != "Newer work" || c.GitBranch != "feature/x" || !c.LastActiveAt.Equal(at(6)) {
		t.Errorf("newer = %+v", c)
	}
	if c := got[1]; c.Title != "" || c.LastPrompt != "why does it fail" {
		t.Errorf("untitled = %+v", c)
	}
	if c := got[3]; c.Title != "" || c.LastPrompt != "" || !c.LastActiveAt.Equal(at(0)) {
		t.Errorf("long = %+v", c)
	}
	if got := l.Conversations(f.claudeHome, dir, 2); len(got) != 2 || got[0].ID != newer || got[1].ID != untitled {
		t.Errorf("limit 2 = %+v", got)
	}
	if got := l.Conversations(f.claudeHome, f.project("empty"), 10); len(got) != 0 {
		t.Errorf("a folder without conversations = %+v", got)
	}
}

// A conversation counts even without an AI title, which Claude Code skips
// when title generation is off or fails: the folder can be continued, and
// its last prompt names the conversation.
func TestListConversationWithoutTitle(t *testing.T) {
	f := newFixture(t)
	untitled, idle, long := f.project("untitled"), f.project("idle"), f.project("long")
	f.config_(map[string]bool{untitled: true, idle: true, long: true})
	f.conversation(untitled, "11111111-2222-4333-8444-555555555555.jsonl",
		map[string]any{"type": "user", "cwd": untitled, "timestamp": "2026-10-29T10:24:00Z"},
		map[string]any{"type": "last-prompt", "lastPrompt": "hello\n  there"})
	f.conversation(long, "11111111-2222-4333-8444-666666666666.jsonl",
		map[string]any{"type": "user", "cwd": long, "timestamp": "2026-10-29T10:25:00Z"},
		map[string]any{"type": "last-prompt", "lastPrompt": strings.Repeat("a", 300)})
	got := map[string]Project{}
	for _, p := range (&Lister{}).List(f.sources(nil)) {
		got[p.Name] = p
	}
	if p := got["untitled"]; !p.HasConversation || p.LastTitle != "" || p.LastPrompt != "hello there" {
		t.Errorf("untitled: %+v", p)
	}
	if p := got["long"]; !p.HasConversation || len([]rune(p.LastPrompt)) != maxPromptRunes+1 || !strings.HasSuffix(p.LastPrompt, "…") {
		t.Errorf("long prompt: %q", p.LastPrompt)
	}
	if p := got["idle"]; p.HasConversation || p.LastPrompt != "" {
		t.Errorf("idle: %+v", p)
	}
}
