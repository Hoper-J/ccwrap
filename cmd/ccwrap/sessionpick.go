package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/projects"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// errNoSession is what pickSession reports when no session is running.
var errNoSession = errors.New("no running ccwrap session")

// sessionCandidate is a running session ccwrap open or ccwrap stop can pick.
type sessionCandidate struct {
	ds   model.DiscoveredSession
	sess *model.Session // live snapshot; nil when the control socket did not answer
}

// liveCandidates are the running sessions in discovered, each with its
// snapshot when the session answers.
func liveCandidates(discovered []model.DiscoveredSession, snapshot func(model.DiscoveredSession) (*model.Session, error)) []sessionCandidate {
	var live []sessionCandidate
	for _, ds := range discovered {
		if ds.Stale || !ds.Reachable {
			continue
		}
		live = append(live, newCandidate(ds, snapshot))
	}
	return live
}

func newCandidate(ds model.DiscoveredSession, snapshot func(model.DiscoveredSession) (*model.Session, error)) sessionCandidate {
	c := sessionCandidate{ds: ds}
	if sess, err := snapshot(ds); err == nil {
		c.sess = sess
	}
	return c
}

func (c sessionCandidate) projectDir() string {
	if c.sess != nil && c.sess.ProjectDir != "" {
		return c.sess.ProjectDir
	}
	return c.ds.Manifest.ProjectDir
}

// label is how a session reads in ccwrap open's output: folder · profile.
func (c sessionCandidate) label() string {
	parts := []string{ui.DisplayDir(c.projectDir())}
	if parts[0] == "" {
		parts[0] = ui.ShortID(c.ds.Manifest.SessionID)
	}
	if c.sess != nil && c.sess.ActiveProfileName != "" {
		parts = append(parts, c.sess.ActiveProfileName)
	}
	return strings.Join(parts, " · ")
}

// row is a session's line in lists and in ccwrap stop's output: short ID,
// then folder and profile.
func (c sessionCandidate) row() string {
	return ui.ShortID(c.ds.Manifest.SessionID) + "  " + c.label()
}

// pickSession chooses the session `ccwrap <cmd>` acts on:
//  1. the argument: a session ID prefix or a project folder name;
//  2. the session this process runs in (CCWRAP_SESSION_ID, else the port in
//     HTTPS_PROXY);
//  3. the session whose project folder holds cwd (the deepest one);
//  4. the only running session.
//
// With several left it returns an error listing them.
func pickSession(live []sessionCandidate, arg string, getenv func(string) string, cwd, cmd string) (sessionCandidate, error) {
	if len(live) == 0 {
		return sessionCandidate{}, errNoSession
	}
	if arg != "" {
		var hits []sessionCandidate
		for _, c := range live {
			if strings.HasPrefix(c.ds.Manifest.SessionID, arg) || (c.projectDir() != "" && filepath.Base(c.projectDir()) == arg) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			return sessionCandidate{}, fmt.Errorf("no running session matches %q\n%s", arg, listCandidates(live))
		case 1:
			return hits[0], nil
		default:
			return sessionCandidate{}, fmt.Errorf("%q matches several sessions; use a longer session ID\n%s", arg, listCandidates(hits))
		}
	}
	if id := getenv(sessionIDEnv); id != "" {
		for _, c := range live {
			if c.ds.Manifest.SessionID == id {
				return c, nil
			}
		}
	}
	if port := proxyPort(getenv("HTTPS_PROXY")); port != "" {
		for _, c := range live {
			if _, p, err := net.SplitHostPort(c.ds.Manifest.ProxyListenAddr); err == nil && p == port {
				return c, nil
			}
		}
	}
	if best, ok := deepestProjectFor(live, cwd); ok {
		return best, nil
	}
	if len(live) == 1 {
		return live[0], nil
	}
	return sessionCandidate{}, fmt.Errorf("several sessions are running; pick one with `ccwrap %s <session-id or project>`\n%s", cmd, listCandidates(live))
}

func proxyPort(proxy string) string {
	u, err := url.Parse(strings.TrimSpace(proxy))
	if err != nil || u.Host == "" {
		return ""
	}
	if host, _, _ := net.SplitHostPort(u.Host); host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return ""
	}
	return u.Port()
}

// deepestProjectFor is the session whose project folder is cwd or its
// closest ancestor. Symlinks are resolved on both sides (macOS /tmp is
// /private/tmp).
func deepestProjectFor(live []sessionCandidate, cwd string) (sessionCandidate, bool) {
	if cwd == "" {
		return sessionCandidate{}, false
	}
	here := projects.Canonical(cwd)
	var best sessionCandidate
	bestLen := -1
	for _, c := range live {
		dir := c.projectDir()
		if dir == "" {
			continue
		}
		dir = projects.Canonical(dir)
		if here == dir || strings.HasPrefix(here, dir+string(filepath.Separator)) {
			if len(dir) > bestLen {
				best, bestLen = c, len(dir)
			}
		}
	}
	return best, bestLen >= 0
}

func listCandidates(cs []sessionCandidate) string {
	sorted := append([]sessionCandidate(nil), cs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ds.Manifest.CreatedAt.Before(sorted[j].ds.Manifest.CreatedAt) })
	var b strings.Builder
	for _, c := range sorted {
		age := ""
		if up := ui.Uptime(c.ds.Manifest.CreatedAt); up != "" {
			age = " · up " + up
		}
		fmt.Fprintf(&b, "  %s%s\n", c.row(), age)
	}
	return strings.TrimRight(b.String(), "\n")
}
