package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/discovery"
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
	cwd, _ := os.Getwd()
	picked, pickErr := pickSession(liveCandidates(discovered, fetchSnapshot), fs.Arg(0), os.Getenv, cwd, "open")
	// No session at all is not an error when the app is there: it opens on
	// Projects, where sessions start.
	noSession := errors.Is(pickErr, errNoSession)
	if noSession && fs.Arg(0) != "" {
		return fmt.Errorf("%w; start one with `ccwrap`", pickErr)
	}
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
	case "windows":
		// The URL protocol handler starts the default browser without a
		// shell parsing the link, as `cmd /c start` would.
		return true, run("rundll32", "url.dll,FileProtocolHandler", link)
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
