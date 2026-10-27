// Package unmapped remembers, per profile, the Claude models Claude Code sent
// with no alias on a route that maps model names, so the next launch can say
// so before any request goes out. ccwrap cannot see Claude Code's built-in
// default model ahead of time; what it actually sent is the reliable signal.
//
// The record is best effort: two ccwrap processes writing at once can drop
// one observation, which the next session that sends the model records again.
package unmapped

import (
	"sort"
	"time"

	"github.com/Hoper-J/ccwrap/internal/statefile"
)

const (
	fileName = "unmapped-models.json"
	// maxPerProfile bounds the models kept per profile; the oldest go first.
	maxPerProfile = 16
)

type store struct {
	// Profiles maps profile name → model → when it was last sent unmapped.
	Profiles map[string]map[string]time.Time `json:"profiles"`
}

func load(stateDir string) store {
	var s store
	statefile.Read(stateDir, fileName, &s)
	if s.Profiles == nil {
		s.Profiles = map[string]map[string]time.Time{}
	}
	return s
}

// Record notes that a session on profile sent model without an alias at t.
func Record(stateDir, profile, model string, t time.Time) error {
	if stateDir == "" || profile == "" || model == "" {
		return nil
	}
	s := load(stateDir)
	seen := s.Profiles[profile]
	if seen == nil {
		seen = map[string]time.Time{}
		s.Profiles[profile] = seen
	}
	seen[model] = t.UTC()
	for len(seen) > maxPerProfile {
		oldest := ""
		for m, at := range seen {
			if oldest == "" || at.Before(seen[oldest]) {
				oldest = m
			}
		}
		delete(seen, oldest)
	}
	return statefile.Write(stateDir, fileName, s)
}

// Models returns the models recorded for profile, sorted.
func Models(stateDir, profile string) []string {
	if stateDir == "" || profile == "" {
		return nil
	}
	seen := load(stateDir).Profiles[profile]
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
