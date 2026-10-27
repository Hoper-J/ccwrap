package projects

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Project is one folder on the Projects page.
type Project struct {
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	Trusted      bool      `json:"trusted"`
	GitBranch    string    `json:"git_branch,omitempty"`
	LastActiveAt time.Time `json:"last_active_at"`
	// LastTitle is the title Claude Code gave the folder's latest conversation.
	LastTitle string `json:"last_title,omitempty"`
	// LastPrompt is that conversation's last prompt, shortened: it names the
	// conversation when Claude Code gave it no title.
	LastPrompt string `json:"last_prompt,omitempty"`
	// HasConversation says Claude Code keeps a conversation for the folder,
	// titled or not.
	HasConversation bool `json:"has_conversation,omitempty"`
	// LastProfile is the profile ccwrap last launched with here; nil when
	// ccwrap never launched here, "" for no profile (inherit-env).
	LastProfile     *string  `json:"last_profile"`
	RunningSessions []string `json:"running_sessions"`
}

// Sources says where List reads from.
type Sources struct {
	// ClaudeConfig is Claude Code's global config (settings.GlobalConfigPath).
	ClaudeConfig string
	// ClaudeHome is Claude Code's config directory (settings.ClaudeConfigHomeDir);
	// conversations live under its projects/.
	ClaudeHome string
	// StateDir is ccwrap's state directory, for the per-folder memory.
	StateDir string
	// Running maps a project folder to the IDs of ccwrap sessions running there.
	Running map[string][]string
}

// Lister builds the Projects list. It caches what it read from each
// conversation file until the file changes, so listing stays cheap.
type Lister struct {
	mu    sync.Mutex
	cache map[string]cachedSummary
}

type cachedSummary struct {
	modTime time.Time
	size    int64
	summary summary
}

// summary is what one conversation file says about its folder.
type summary struct {
	cwd    string // the conversation's starting folder, when found
	title  string
	prompt string
	branch string
	at     time.Time
}

// maxPromptRunes is how much of a last prompt Project.LastPrompt keeps.
const maxPromptRunes = 120

// shortPrompt is a prompt on one line, cut to maxPromptRunes.
func shortPrompt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxPromptRunes {
		s = string(r[:maxPromptRunes]) + "…"
	}
	return s
}

// tailBytes and headBytes bound how much of a conversation file is read.
const (
	tailBytes = 256 << 10
	headBytes = 64 << 10
)

// List returns the folders Claude Code has been used in that still exist,
// most recently active first. It only reads local files.
func (l *Lister) List(src Sources) []Project {
	byPath := map[string]*Project{}
	add := func(path string) *Project {
		if p := byPath[path]; p != nil {
			return p
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return nil
		}
		p := &Project{Path: path, Name: filepath.Base(path), RunningSessions: []string{}}
		byPath[path] = p
		return p
	}

	// Claude Code's config lists the folders it was used in, with trust.
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if data, err := os.ReadFile(src.ClaudeConfig); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	for path, meta := range cfg.Projects {
		if p := add(path); p != nil {
			p.Trusted = meta.HasTrustDialogAccepted
		}
	}

	// Each conversation folder is named after its starting folder with every
	// character but letters and digits replaced by "-".
	convRoot := filepath.Join(src.ClaudeHome, "projects")
	dirs, _ := os.ReadDir(convRoot)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		s, ok := l.latest(filepath.Join(convRoot, d.Name()))
		if !ok {
			continue
		}
		path := ""
		for known := range byPath {
			if encodeDir(known) == d.Name() {
				path = known
				break
			}
		}
		if path == "" && s.cwd != "" && encodeDir(s.cwd) == d.Name() {
			path = s.cwd
		}
		p := add(path)
		if p == nil {
			continue
		}
		p.HasConversation = true
		if s.at.After(p.LastActiveAt) {
			p.LastActiveAt, p.LastTitle, p.LastPrompt, p.GitBranch = s.at, s.title, s.prompt, s.branch
		}
	}

	for dir, m := range Memories(src.StateDir) {
		if p := add(dir); p != nil {
			profile := m.LastProfile
			p.LastProfile = &profile
			if m.LastLaunchedAt.After(p.LastActiveAt) {
				p.LastActiveAt = m.LastLaunchedAt
			}
		}
	}
	for dir, ids := range src.Running {
		if p := add(dir); p != nil {
			p.RunningSessions = append([]string(nil), ids...)
			sort.Strings(p.RunningSessions)
		}
	}

	out := make([]Project, 0, len(byPath))
	for _, p := range byPath {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActiveAt.Equal(out[j].LastActiveAt) {
			return out[i].LastActiveAt.After(out[j].LastActiveAt)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func encodeDir(path string) string {
	b := []byte(path)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// latest summarizes the newest conversation file in dir.
func (l *Lister) latest(dir string) (summary, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return summary{}, false
	}
	var newest string
	var newestInfo os.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newestInfo == nil || info.ModTime().After(newestInfo.ModTime()) {
			newest, newestInfo = filepath.Join(dir, e.Name()), info
		}
	}
	if newestInfo == nil {
		return summary{}, false
	}
	l.mu.Lock()
	if c, ok := l.cache[newest]; ok && c.modTime.Equal(newestInfo.ModTime()) && c.size == newestInfo.Size() {
		l.mu.Unlock()
		return c.summary, true
	}
	l.mu.Unlock()
	s := summarize(newest, filepath.Base(dir))
	if s.at.IsZero() {
		s.at = newestInfo.ModTime()
	}
	l.mu.Lock()
	if l.cache == nil {
		l.cache = map[string]cachedSummary{}
	}
	l.cache[newest] = cachedSummary{modTime: newestInfo.ModTime(), size: newestInfo.Size(), summary: s}
	l.mu.Unlock()
	return s, true
}

type entry struct {
	Type       string    `json:"type"`
	AITitle    string    `json:"aiTitle"`
	LastPrompt string    `json:"lastPrompt"`
	Cwd        string    `json:"cwd"`
	GitBranch  string    `json:"gitBranch"`
	Timestamp  time.Time `json:"timestamp"`
}

// summarize reads the end of a conversation file for its latest title,
// branch and time, and looks for the starting folder (the cwd matching
// dirName) at the end, then at the start.
func summarize(file, dirName string) summary {
	f, err := os.Open(file)
	if err != nil {
		return summary{}
	}
	defer f.Close()
	var s summary
	if info, err := f.Stat(); err == nil {
		start := info.Size() - tailBytes
		if start < 0 {
			start = 0
		}
		tail := make([]byte, info.Size()-start)
		if _, err := f.ReadAt(tail, start); err == nil || err == io.EOF {
			lines := bytes.Split(tail, []byte("\n"))
			for i := len(lines) - 1; i >= 0; i-- {
				var e entry
				if json.Unmarshal(lines[i], &e) != nil {
					continue
				}
				if s.title == "" && e.Type == "ai-title" && e.AITitle != "" {
					s.title = e.AITitle
				}
				if s.prompt == "" && e.Type == "last-prompt" && e.LastPrompt != "" {
					s.prompt = shortPrompt(e.LastPrompt)
				}
				if s.at.IsZero() && !e.Timestamp.IsZero() {
					s.at = e.Timestamp
				}
				if s.branch == "" && e.GitBranch != "" && e.GitBranch != "HEAD" {
					s.branch = e.GitBranch
				}
				if s.cwd == "" && e.Cwd != "" && encodeDir(e.Cwd) == dirName {
					s.cwd = e.Cwd
				}
			}
		}
	}
	if s.cwd == "" {
		sc := bufio.NewScanner(io.LimitReader(io.NewSectionReader(f, 0, headBytes), headBytes))
		sc.Buffer(make([]byte, 64<<10), headBytes)
		for sc.Scan() {
			var e entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Cwd != "" && encodeDir(e.Cwd) == dirName {
				s.cwd = e.Cwd
				break
			}
		}
	}
	return s
}
