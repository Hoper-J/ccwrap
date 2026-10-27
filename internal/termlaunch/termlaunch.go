// Package termlaunch opens the user's own terminal app in a folder and runs
// a command there, so the app can start Claude Code without embedding a
// terminal.
//
// Every terminal gets the same thing to run: a one-shot script that deletes
// itself, changes into the folder and execs the command, with every argument
// single-quoted. Nothing user-supplied reaches a shell or AppleScript unquoted.
package termlaunch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/Hoper-J/ccwrap/internal/statefile"
)

// Terminal is a terminal app ccwrap can open.
type Terminal struct {
	ID   string `json:"id"`   // stable key: "terminal", "iterm", "ghostty"
	Name string `json:"name"` // display name
	// Verified is false for adapters not yet tried on a real install.
	Verified bool `json:"verified"`
	bundle   string
	open     func(app, script string, run runner) error
}

type runner func(name string, args ...string) error

var terminals = []Terminal{
	{ID: "terminal", Name: "Terminal", Verified: true, bundle: "Terminal.app", open: openWithApp("Terminal")},
	{ID: "iterm", Name: "iTerm", Verified: true, bundle: "iTerm.app", open: openITerm},
	{ID: "ghostty", Name: "Ghostty", bundle: "Ghostty.app", open: openGhostty},
}

// openWithApp hands the script to a terminal that runs executable files it
// is asked to open (Terminal does, in a new window).
func openWithApp(app string) func(string, string, runner) error {
	return func(appPath, script string, run runner) error {
		return run("open", "-a", appPath, script)
	}
}

// openITerm asks iTerm, through AppleScript, for a new window and types the
// script's path into it, so the script runs in the user's own shell with
// their environment (PATH, proxies), the way Terminal runs a .command file.
// A window's own command would run without the shell's rc files, where
// claude is often put on PATH; iTerm opens files given to it without
// running them. The script path is ccwrap's own; one with spaces or quotes
// is refused rather than escaped. macOS may ask once to let ccwrap control
// iTerm.
func openITerm(appPath, script string, run runner) error {
	if strings.ContainsAny(script, " \t\"'\\") {
		return fmt.Errorf("launch script path %q must not contain spaces or quotes", script)
	}
	return run("osascript",
		"-e", `tell application "iTerm"`,
		"-e", "activate",
		"-e", "set w to (create window with default profile)",
		"-e", `tell current session of w to write text "`+script+`"`,
		"-e", "end tell")
}

// openGhostty starts a new Ghostty instance running the script; Ghostty
// takes its command after -e.
func openGhostty(appPath, script string, run runner) error {
	return run("open", "-na", appPath, "--args", "-e", script)
}

// Installed lists the supported terminals found on this machine, in
// display order, each with the app path it would be opened with.
func Installed(appDirs []string) []Installation {
	var out []Installation
	for _, t := range terminals {
		for _, dir := range appDirs {
			p := filepath.Join(dir, t.bundle)
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				out = append(out, Installation{Terminal: t, AppPath: p})
				break
			}
		}
	}
	return out
}

// Installation is a terminal found on this machine.
type Installation struct {
	Terminal
	AppPath string `json:"app_path"`
}

// DefaultAppDirs are where macOS apps live.
func DefaultAppDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{"/Applications", filepath.Join(home, "Applications"), "/System/Applications/Utilities", "/Applications/Utilities"}
}

const prefFile = "terminal.json"

// Choose picks the terminal to open: ccwrap's saved choice, else the one
// Claude Code uses for its own deep links (its deepLinkTerminal setting,
// passed in as claudeChoice), else Terminal, else the first installed.
func Choose(installed []Installation, stateDir, claudeChoice string) (Installation, bool) {
	var pref struct {
		Terminal string `json:"terminal"`
	}
	statefile.Read(stateDir, prefFile, &pref)
	for _, want := range []string{pref.Terminal, idForClaudeName(claudeChoice), "terminal"} {
		for _, in := range installed {
			if want != "" && in.ID == want {
				return in, true
			}
		}
	}
	if len(installed) > 0 {
		return installed[0], true
	}
	return Installation{}, false
}

// SavePreference remembers the user's terminal choice.
func SavePreference(stateDir, id string) error {
	for _, t := range terminals {
		if t.ID == id {
			return statefile.Write(stateDir, prefFile, map[string]string{"terminal": id})
		}
	}
	return fmt.Errorf("unsupported terminal %q", id)
}

// idForClaudeName maps Claude Code's deepLinkTerminal value to an ID.
func idForClaudeName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "terminal", "terminal.app", "apple_terminal":
		return "terminal"
	case "iterm", "iterm2", "iterm.app":
		return "iterm"
	case "ghostty":
		return "ghostty"
	}
	return ""
}

// ClaudeChoiceID is the ID for Claude Code's deepLinkTerminal value, or "".
func ClaudeChoiceID(name string) string { return idForClaudeName(name) }

// Request is what to run: Exe with Args, in Dir.
type Request struct {
	Dir  string
	Exe  string
	Args []string
}

// ValidateDir accepts an existing directory given as an absolute path with
// no control or invisible bidirectional characters (the same rule Claude
// Code applies to deep-link folders).
func ValidateDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("folder must be an absolute path")
	}
	for _, r := range dir {
		if unicode.IsControl(r) || isBidiControl(r) {
			return errors.New("folder contains control or bidirectional characters")
		}
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("folder does not exist")
	}
	return nil
}

func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C:
		return true
	}
	return false
}

// Script is the one-shot script for req.
func Script(req Request) (string, error) {
	for _, a := range append([]string{req.Dir, req.Exe}, req.Args...) {
		if strings.ContainsRune(a, 0) || strings.ContainsAny(a, "\n\r") {
			return "", errors.New("arguments must not contain NUL or line breaks")
		}
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("rm -f -- \"$0\"\n")
	b.WriteString("cd -- " + shellQuote(req.Dir) + " || exit 1\n")
	b.WriteString("exec " + shellQuote(req.Exe))
	for _, a := range req.Args {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")
	return b.String(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Launch writes the script for req into scriptDir and opens it in the
// terminal. The script removes itself when it runs.
func Launch(in Installation, req Request, scriptDir string) error {
	return launch(in, req, scriptDir, func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	})
}

// staleScriptAge is how old a launch script must be before a later launch
// removes it as never run. Far longer than any terminal takes to start one,
// a first-open confirmation prompt included.
const staleScriptAge = time.Hour

// sweepStaleScripts removes launch scripts in dir older than staleScriptAge:
// ones a terminal accepted but never ran, which would otherwise stay.
func sweepStaleScripts(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "ccwrap-launch-") || !strings.HasSuffix(e.Name(), ".command") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > staleScriptAge {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func launch(in Installation, req Request, scriptDir string, run runner) error {
	if err := ValidateDir(req.Dir); err != nil {
		return err
	}
	text, err := Script(req)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(scriptDir, 0o700); err != nil {
		return err
	}
	sweepStaleScripts(scriptDir)
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	script := filepath.Join(scriptDir, "ccwrap-launch-"+hex.EncodeToString(rnd[:])+".command")
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		return err
	}
	if err := in.open(in.AppPath, script, run); err != nil {
		_ = os.Remove(script)
		return err
	}
	return nil
}
