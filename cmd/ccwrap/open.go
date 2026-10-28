package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/projects"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// openCommand opens a session in ccwrap ui, starting the app if needed —
// even under CCWRAP_NO_UI=1, which only keeps launches from starting it.
// Without an argument it picks the session it runs in, else the one whose
// project folder holds the current directory, else the only running one;
// with no session running it opens the app's Projects page.
func openCommand(paths app.Paths, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	printOnly := fs.Bool("print", false, "print the address instead of opening a browser")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ccwrap open [--print] [session-id-prefix | project-name]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return err
	}
	var live []openCandidate
	for _, ds := range discovered {
		if ds.Stale || !ds.Reachable {
			continue
		}
		c := openCandidate{ds: ds}
		if sess, err := fetchSnapshot(ds); err == nil {
			c.sess = sess
		}
		live = append(live, c)
	}
	cwd, _ := os.Getwd()
	picked, pickErr := pickOpenSession(live, fs.Arg(0), os.Getenv, cwd)
	// No session at all is not an error when the app is there: it opens on
	// Projects, where sessions start.
	noSession := pickErr != nil && len(live) == 0 && fs.Arg(0) == ""
	if pickErr != nil && !noSession {
		return pickErr
	}
	info, err := ensureUI(paths, 3*time.Second)
	if err != nil {
		return fmt.Errorf("can't open ccwrap ui: %w", err)
	}
	link, what := info.URL()+"#/projects", "ccwrap ui"
	if !noSession {
		link, what = appSessionURL(info, picked.ds.Manifest.SessionID), picked.label()
	}
	if *printOnly {
		fmt.Println(link)
		return nil
	}
	fmt.Printf("Opening %s — %s\n", what, link)
	if opened, err := openInBrowser(link, os.Getenv, runtime.GOOS, startCommand); !opened || err != nil {
		printNoBrowserHint(os.Stdout, info.HTTPAddr)
	}
	return nil
}

type openCandidate struct {
	ds   model.DiscoveredSession
	sess *model.Session // live snapshot; nil when the control socket did not answer
}

func (c openCandidate) projectDir() string {
	if c.sess != nil && c.sess.ProjectDir != "" {
		return c.sess.ProjectDir
	}
	return c.ds.Manifest.ProjectDir
}

// label is how a session reads in ccwrap open's output: folder · profile.
func (c openCandidate) label() string {
	parts := []string{ui.DisplayDir(c.projectDir())}
	if parts[0] == "" {
		parts[0] = ui.ShortID(c.ds.Manifest.SessionID)
	}
	if c.sess != nil && c.sess.ActiveProfileName != "" {
		parts = append(parts, c.sess.ActiveProfileName)
	}
	return strings.Join(parts, " · ")
}

// pickOpenSession chooses the session to open:
//  1. the argument: a session ID prefix or a project folder name;
//  2. the session this process runs in (CCWRAP_SESSION_ID, else the port in
//     HTTPS_PROXY);
//  3. the session whose project folder holds cwd (the deepest one);
//  4. the only running session.
//
// With several left it returns an error listing them.
func pickOpenSession(live []openCandidate, arg string, getenv func(string) string, cwd string) (openCandidate, error) {
	if len(live) == 0 {
		return openCandidate{}, errors.New("no running ccwrap session; start one with `ccwrap`")
	}
	if arg != "" {
		var hits []openCandidate
		for _, c := range live {
			if strings.HasPrefix(c.ds.Manifest.SessionID, arg) || (c.projectDir() != "" && filepath.Base(c.projectDir()) == arg) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			return openCandidate{}, fmt.Errorf("no running session matches %q\n%s", arg, listOpenCandidates(live))
		case 1:
			return hits[0], nil
		default:
			return openCandidate{}, fmt.Errorf("%q matches several sessions; use a longer session ID\n%s", arg, listOpenCandidates(hits))
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
	return openCandidate{}, fmt.Errorf("several sessions are running; pick one with `ccwrap open <session-id or project>`\n%s", listOpenCandidates(live))
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
func deepestProjectFor(live []openCandidate, cwd string) (openCandidate, bool) {
	if cwd == "" {
		return openCandidate{}, false
	}
	here := projects.Canonical(cwd)
	var best openCandidate
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

func listOpenCandidates(cs []openCandidate) string {
	sorted := append([]openCandidate(nil), cs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ds.Manifest.CreatedAt.Before(sorted[j].ds.Manifest.CreatedAt) })
	var b strings.Builder
	for _, c := range sorted {
		age := ""
		if up := ui.Uptime(c.ds.Manifest.CreatedAt); up != "" {
			age = " · up " + up
		}
		fmt.Fprintf(&b, "  %s  %s%s\n", ui.ShortID(c.ds.Manifest.SessionID), c.label(), age)
	}
	return strings.TrimRight(b.String(), "\n")
}

// startCommand runs a browser opener; a seam for tests.
var startCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// openInBrowser opens link with the platform's opener. It does nothing over
// SSH or without a graphical session, where a browser would open on the
// wrong screen or not at all; the caller then prints the address.
func openInBrowser(link string, getenv func(string) string, goos string, run func(string, ...string) error) (bool, error) {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return false, nil
	}
	switch goos {
	case "darwin":
		return true, run("open", link)
	case "linux":
		if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
			return false, nil
		}
		return true, run("xdg-open", link)
	}
	return false, nil
}

func printNoBrowserHint(w io.Writer, addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "No browser here. From your own machine, forward the port and open it there:\n  ssh -L %s:127.0.0.1:%s <this host>\n  then open http://127.0.0.1:%s/\n", port, port, port)
}
