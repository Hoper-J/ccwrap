package projects

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// conversationID matches a Claude Code session ID, which names the
// conversation's file.
var conversationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsConversationID reports whether id has the form of a Claude Code session ID.
func IsConversationID(id string) bool { return conversationID.MatchString(id) }

// ConversationDir is where Claude Code keeps the conversations started in
// dir, under claudeHome (settings.ClaudeConfigHomeDir).
func ConversationDir(claudeHome, dir string) string {
	return filepath.Join(claudeHome, "projects", encodeDir(dir))
}

// HasConversation reports whether Claude Code keeps conversation id among
// those started in dir.
func HasConversation(claudeHome, dir, id string) bool {
	if !IsConversationID(id) {
		return false
	}
	info, err := os.Stat(filepath.Join(ConversationDir(claudeHome, dir), id+".jsonl"))
	return err == nil && info.Mode().IsRegular()
}

// Conversation is one conversation Claude Code keeps for a folder.
type Conversation struct {
	ID           string    `json:"id"`
	Title        string    `json:"title,omitempty"`
	LastPrompt   string    `json:"last_prompt,omitempty"`
	GitBranch    string    `json:"git_branch,omitempty"`
	LastActiveAt time.Time `json:"last_active_at"`
}

// Conversations lists the conversations `claude --resume` in dir can open,
// most recently active first, at most limit. It reads the newest files
// first and keeps their summaries cached like List.
func (l *Lister) Conversations(claudeHome, dir string, limit int) []Conversation {
	convDir := ConversationDir(claudeHome, dir)
	entries, err := os.ReadDir(convDir)
	if err != nil {
		return nil
	}
	type file struct {
		id   string
		info os.FileInfo
	}
	var files []file
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !IsConversationID(id) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, file{id, info})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	var out []Conversation
	for _, f := range files {
		if len(out) == limit {
			break
		}
		s := l.summary(filepath.Join(convDir, f.info.Name()), f.info)
		if s.bookkeeping {
			continue
		}
		out = append(out, Conversation{ID: f.id, Title: s.title, LastPrompt: s.prompt, GitBranch: s.branch, LastActiveAt: s.at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastActiveAt.After(out[j].LastActiveAt) })
	return out
}
