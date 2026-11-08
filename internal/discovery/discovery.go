package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
)

// scanProbeConcurrency caps simultaneous control-socket probes during
// Scan. Each probe has a 700ms timeout, so an unbounded fanout against
// a directory full of stale manifests would burn goroutines.
const scanProbeConcurrency = 16

func Scan(paths app.Paths) ([]model.DiscoveredSession, error) {
	// Manifests name the control sockets ccwrap then talks to; none are
	// trusted from a runtime dir another user could have written them into.
	if err := app.CheckPrivateDir(paths.RuntimeDir); err != nil {
		return nil, err
	}
	root := paths.SessionsDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Phase 1 (serial, disk-bound): read every manifest, classify
	// stale/dead-ahead-of-probe cases, mark survivors as needing a
	// control-socket probe. This stays serial because manifest reads
	// are local file IO and cheap.
	type pending struct {
		ds         model.DiscoveredSession
		needsProbe bool
	}
	pendings := make([]pending, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirID := entry.Name()
		m, err := manifest.Read(filepath.Join(root, entry.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(m.SessionID) == "" {
			m.SessionID = dirID
			pendings = append(pendings, pending{ds: model.DiscoveredSession{
				Manifest: m,
				Stale:    true,
				Error:    "manifest session_id is empty",
			}})
			continue
		}
		if m.SessionID != dirID {
			originalID := m.SessionID
			m.SessionID = dirID
			pendings = append(pendings, pending{ds: model.DiscoveredSession{
				Manifest: m,
				Stale:    true,
				Error:    fmt.Sprintf("manifest session_id %q does not match directory %q", originalID, dirID),
			}})
			continue
		}
		ds := model.DiscoveredSession{Manifest: m}
		ds.Stale, ds.Error = staleState(m)
		if ds.Stale {
			pendings = append(pendings, pending{ds: ds})
			continue
		}
		if m.ControlSocket == "" {
			ds.Error = "manifest missing control socket"
			pendings = append(pendings, pending{ds: ds})
			continue
		}
		if _, err := os.Stat(m.ControlSocket); err != nil {
			ds.Error = err.Error()
			pendings = append(pendings, pending{ds: ds})
			continue
		}
		pendings = append(pendings, pending{ds: ds, needsProbe: true})
	}
	// Phase 2 (parallel, network-bound): probe control sockets with
	// bounded concurrency. Each goroutine writes back into its own
	// pending slot by index — slice header is fixed at this point,
	// element writes are non-overlapping, so no extra sync.
	sem := make(chan struct{}, scanProbeConcurrency)
	var wg sync.WaitGroup
	for i := range pendings {
		if !pendings[i].needsProbe {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			probeControlSocket(&pendings[idx].ds)
		}(i)
	}
	wg.Wait()
	// Phase 3: collect + sort by CreatedAt (stable ordering for UI).
	out := make([]model.DiscoveredSession, len(pendings))
	for i, p := range pendings {
		out[i] = p.ds
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Manifest.CreatedAt.After(out[j].Manifest.CreatedAt)
	})
	return out, nil
}

// probeControlSocket issues a 700ms control.Status request against a
// single manifest's control socket and writes the result back into
// ds.{Reachable,Stale,Error}.
func probeControlSocket(ds *model.DiscoveredSession) {
	m := ds.Manifest
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	status, err := control.NewClient(m.ControlSocket).Status(ctx)
	if err != nil {
		ds.Error = err.Error()
		return
	}
	if status.SessionID != "" && status.SessionID != m.SessionID {
		ds.Stale = true
		ds.Error = fmt.Sprintf("control socket is bound to %s", status.SessionID)
		return
	}
	if status.SocketPath != "" && status.SocketPath != m.ControlSocket {
		ds.Stale = true
		ds.Error = "control socket path mismatch"
		return
	}
	ds.Reachable = true
}

// Find scans the sessions and returns the one sessionID names (see Match).
func Find(paths app.Paths, sessionID string) (*model.DiscoveredSession, error) {
	sessions, err := Scan(paths)
	if err != nil {
		return nil, err
	}
	return Match(sessions, sessionID)
}

// Match returns the session id names: the one with that ID, else the only
// one whose ID starts with it, so the 8-character IDs ccwrap prints work.
func Match(sessions []model.DiscoveredSession, id string) (*model.DiscoveredSession, error) {
	if id == "" {
		return nil, errors.New("no session ID given")
	}
	var hits []*model.DiscoveredSession
	for i := range sessions {
		sid := sessions[i].Manifest.SessionID
		if sid == id {
			return &sessions[i], nil
		}
		if strings.HasPrefix(sid, id) {
			hits = append(hits, &sessions[i])
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("session %s not found", id)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, ds := range hits {
		ids[i] = ds.Manifest.SessionID
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("%q matches several sessions (%s); use a longer session ID", id, strings.Join(ids, ", "))
}

func Active(paths app.Paths) ([]model.DiscoveredSession, error) {
	sessions, err := Scan(paths)
	if err != nil {
		return nil, err
	}
	out := sessions[:0]
	for _, sess := range sessions {
		if !sess.Stale && sess.Reachable {
			out = append(out, sess)
		}
	}
	return out, nil
}

func Cleanup(paths app.Paths) ([]string, error) {
	sessions, err := Scan(paths)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, sess := range sessions {
		if !sess.Stale {
			continue
		}
		dir := paths.SessionDir(sess.Manifest.SessionID)
		if err := os.RemoveAll(dir); err != nil {
			return removed, err
		}
		removed = append(removed, sess.Manifest.SessionID)
	}
	return removed, nil
}

func staleState(m model.SessionManifest) (bool, string) {
	if m.SupervisorPID <= 0 {
		return true, "invalid supervisor pid"
	}
	if m.SupervisorStartToken != "" {
		exists, match, err := procmeta.Matches(m.SupervisorPID, m.SupervisorStartToken)
		if err != nil {
			return true, "supervisor not running"
		}
		if !exists {
			return true, "supervisor not running"
		}
		if !match {
			return true, "supervisor pid reused"
		}
		return false, ""
	}
	if !procmeta.Exists(m.SupervisorPID) {
		return true, "supervisor not running"
	}
	return false, ""
}
