// Package projects lists the folders Claude Code has been used in, for the
// app's Projects page, and remembers per folder the profile ccwrap last
// launched with there.
package projects

import (
	"time"

	"github.com/Hoper-J/ccwrap/internal/statefile"
)

const (
	memoryFile = "projects.json"
	// maxMemories bounds the folders remembered; the least recent go first.
	maxMemories = 500
)

// Memory is what ccwrap remembers about one folder.
type Memory struct {
	// LastProfile is the profile the last session there ran with; "" means
	// no profile overlay (inherit-env).
	LastProfile    string    `json:"last_profile"`
	LastLaunchedAt time.Time `json:"last_launched_at"`
}

type memoryStore struct {
	Folders map[string]Memory `json:"folders"`
}

// RememberLaunch records that a session started in dir with profile. Best
// effort, like the other state files: concurrent launches can lose one
// record.
func RememberLaunch(stateDir, dir, profile string, at time.Time) error {
	if stateDir == "" || dir == "" {
		return nil
	}
	var s memoryStore
	statefile.Read(stateDir, memoryFile, &s)
	if s.Folders == nil {
		s.Folders = map[string]Memory{}
	}
	s.Folders[dir] = Memory{LastProfile: profile, LastLaunchedAt: at.UTC()}
	for len(s.Folders) > maxMemories {
		oldest := ""
		for d, m := range s.Folders {
			if oldest == "" || m.LastLaunchedAt.Before(s.Folders[oldest].LastLaunchedAt) {
				oldest = d
			}
		}
		delete(s.Folders, oldest)
	}
	return statefile.Write(stateDir, memoryFile, s)
}

// Memories returns what ccwrap remembers, by folder.
func Memories(stateDir string) map[string]Memory {
	var s memoryStore
	if stateDir != "" {
		statefile.Read(stateDir, memoryFile, &s)
	}
	if s.Folders == nil {
		s.Folders = map[string]Memory{}
	}
	return s.Folders
}
