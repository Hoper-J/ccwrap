// Package termlaunch opens the user's own terminal app in a folder and runs
// a command there, so the app can start Claude Code without embedding a
// terminal.
//
// Every macOS terminal gets the same thing to run: a one-shot script that
// deletes itself, changes into the folder and execs the command, with every
// argument single-quoted. Windows Terminal gets the same steps as a
// PowerShell command, and a Linux terminal as a fixed sh script that takes
// the folder and the command as its arguments. Nothing user-supplied reaches
// a shell or AppleScript unquoted.
package termlaunch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/Hoper-J/ccwrap/internal/statefile"
)

// Terminal is a terminal app ccwrap can open.
type Terminal struct {
	ID   string `json:"id"`   // stable key: "terminal", "iterm", "ghostty"
	Name string `json:"name"` // display name
	// Verified is false for adapters not yet tried on a real install.
	Verified bool `json:"verified"`
	bundle   string
	bundleID string
	open     func(app, script string, run runner) error
	// exe is the program a Windows or Linux terminal is found by on PATH,
	// on goos only. command opens a Windows terminal on a request directly,
	// with no script file; argv is how a Linux terminal is started for one.
	exe     string
	goos    string
	command func(app string, req Request, run runner) error
	argv    func(req Request) []string
}

type runner func(name string, args ...string) error

var terminals = []Terminal{
	{ID: "terminal", Name: "Terminal", Verified: true, bundle: "Terminal.app", bundleID: "com.apple.Terminal", open: openWithApp("Terminal")},
	{ID: "iterm", Name: "iTerm", Verified: true, bundle: "iTerm.app", bundleID: "com.googlecode.iterm2", open: openITerm},
	{ID: "ghostty", Name: "Ghostty", Verified: true, bundle: "Ghostty.app", bundleID: "com.mitchellh.ghostty", open: openGhostty},
	{ID: "windows-terminal", Name: "Windows Terminal", Verified: true, exe: "wt.exe", goos: "windows", command: openWindowsTerminal},
	// Linux, in the order Claude Code prefers them for its own deep links,
	// after the Debian/Ubuntu default.
	linuxTerminal("x-terminal-emulator", "x-terminal-emulator (system default)", "-e"),
	linuxTerminal("ghostty", "Ghostty", "-e"),
	linuxTerminal("kitty", "kitty"),
	linuxTerminal("alacritty", "Alacritty", "-e"),
	linuxTerminal("wezterm", "WezTerm", "start", "--"),
	linuxTerminal("gnome-terminal", "GNOME Terminal", "--"),
	linuxTerminal("konsole", "Konsole", "-e"),
	linuxTerminal("xfce4-terminal", "Xfce Terminal", "-x"),
	linuxTerminal("mate-terminal", "MATE Terminal", "-x"),
	linuxTerminal("tilix", "Tilix", "-e"),
	linuxTerminal("foot", "foot"),
	verified(linuxTerminal("xterm", "XTerm", "-e")),
}

// verified marks t as tried on a real install.
func verified(t Terminal) Terminal {
	t.Verified = true
	return t
}

// linuxShell is what every Linux terminal runs: change into the folder, run
// the command with its arguments, and when it fails keep the window open
// until Enter, so its message can be read. The folder, the command and its
// arguments reach it as its own arguments, never as part of the script.
const linuxShell = `cd -- "$1" || exit 1; shift; "$@"; s=$?; [ "$s" -eq 0 ] || { printf '\n[exit status %s; press Enter to close]' "$s"; read -r _; }; exit "$s"`

// linuxTerminal is the Linux terminal program id, which runs the command
// that follows flags.
func linuxTerminal(id, name string, flags ...string) Terminal {
	return Terminal{ID: id, Name: name, exe: id, goos: "linux", argv: func(req Request) []string {
		args := append(append([]string(nil), flags...), "/bin/sh", "-c", linuxShell, "sh", req.Dir, req.Exe)
		return append(args, req.Args...)
	}}
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
// takes its command after -e and runs it with the user's shell
// environment. Arguments reach only a new instance, hence -n.
func openGhostty(appPath, script string, run runner) error {
	return run("open", "-na", appPath, "--args", "-e", script)
}

// Installed lists the supported terminals on this machine, in display
// order, each with the app path it would be opened with: from the usual
// app folders, else wherever Spotlight has seen it (Homebrew can install
// casks into any folder); on Windows and Linux, from PATH.
func Installed() []Installation { return find(DefaultAppDirs(), spotlightApp, pathExe) }

// InstalledIn is Installed looking in appDirs only.
func InstalledIn(appDirs []string) []Installation { return find(appDirs, nil, nil) }

// find looks for each terminal: an app bundle in appDirs, else where locate
// says, or a Windows or Linux terminal's program where lookExe says.
func find(appDirs []string, locate func(string) string, lookExe func(Terminal) string) []Installation {
	var out []Installation
	for _, t := range terminals {
		p := ""
		if t.exe != "" {
			if lookExe != nil {
				p = lookExe(t)
			}
		} else {
			for _, dir := range appDirs {
				if isApp(filepath.Join(dir, t.bundle)) {
					p = filepath.Join(dir, t.bundle)
					break
				}
			}
			if p == "" && locate != nil {
				p = locate(t.bundleID)
			}
		}
		if p != "" {
			out = append(out, Installation{Terminal: t, AppPath: p})
		}
	}
	return out
}

// pathExe is where t's program is on PATH, or "": "" also when t is for
// another system, or for Linux without a graphical session (over SSH, say).
func pathExe(t Terminal) string {
	if t.goos != runtime.GOOS || t.goos == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return ""
	}
	p, err := exec.LookPath(t.exe)
	if err != nil {
		return ""
	}
	return p
}

func isApp(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// spotlightApp is where Spotlight has seen the app with bundleID, or ""
// (not on macOS, Spotlight off, or not installed). Copies in the Trash or
// inside another app are skipped.
func spotlightApp(bundleID string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "mdfind", "kMDItemCFBundleIdentifier == '"+bundleID+"'").Output()
	if err != nil {
		return ""
	}
	return firstApp(string(out))
}

// firstApp is the first app bundle in Spotlight's output (a path a line)
// that is not in the Trash or inside another app.
func firstApp(out string) string {
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(p, ".app") && !strings.Contains(p, "/.Trash/") && !strings.Contains(strings.TrimSuffix(p, ".app"), ".app/") && isApp(p) {
			return p
		}
	}
	return ""
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
// passed in as claudeChoice), else $TERMINAL, else Terminal on macOS or the
// system default on Linux, else the first installed.
func Choose(installed []Installation, stateDir, claudeChoice string) (Installation, bool) {
	var pref struct {
		Terminal string `json:"terminal"`
	}
	statefile.Read(stateDir, prefFile, &pref)
	envChoice := idForClaudeName(filepath.Base(os.Getenv("TERMINAL")))
	for _, want := range []string{pref.Terminal, idForClaudeName(claudeChoice), envChoice, "terminal", "x-terminal-emulator"} {
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
	case "windows-terminal", "windows_terminal", "windowsterminal", "wt":
		return "windows-terminal"
	case "x-terminal-emulator", "kitty", "alacritty", "wezterm", "gnome-terminal", "konsole", "xfce4-terminal", "mate-terminal", "tilix", "foot", "xterm":
		return strings.ToLower(strings.TrimSpace(name))
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

// Launch opens req in the terminal: a macOS terminal gets a script written
// into scriptDir, which removes itself when it runs.
func Launch(in Installation, req Request, scriptDir string) error {
	return launch(in, req, scriptDir, func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}, startDetached)
}

// startCheck is how long startDetached watches a terminal for a failure.
const startCheck = time.Second

// startDetached starts a terminal in a session of its own and returns once
// it runs: most Linux terminals stay in the foreground until their window
// closes. A terminal that fails within startCheck reports what it printed.
// Its output goes to a file rather than a pipe, which would break, and with
// it a terminal that writes, once ccwrap ui exits.
func startDetached(name string, args ...string) error {
	out, err := os.CreateTemp("", "ccwrap-terminal-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			msg, _ := os.ReadFile(out.Name())
			return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(msg)))
		}
	case <-time.After(startCheck):
	}
	return nil
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

func launch(in Installation, req Request, scriptDir string, run, start runner) error {
	if err := ValidateDir(req.Dir); err != nil {
		return err
	}
	if in.command != nil {
		return in.command(in.AppPath, req, run)
	}
	if in.argv != nil {
		for _, a := range append([]string{req.Dir, req.Exe}, req.Args...) {
			if strings.ContainsRune(a, 0) {
				return errors.New("arguments must not contain NUL")
			}
		}
		return start(in.AppPath, in.argv(req)...)
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

// openWindowsTerminal opens a new Windows Terminal window running req as a
// PowerShell command. The command goes base64-encoded, so neither Windows
// Terminal, which splits its command line at semicolons, nor cmd.exe parses
// any part of it.
func openWindowsTerminal(app string, req Request, run runner) error {
	cmd, err := PowerShellCommand(req)
	if err != nil {
		return err
	}
	return run(app, "-w", "new", "powershell.exe", "-NoLogo", "-EncodedCommand", cmd)
}

// PowerShellCommand is req as a PowerShell -EncodedCommand value: change into
// the folder, then run the program, with every value single-quoted. Windows
// PowerShell drops empty arguments, garbles double quotes and lets a trailing
// backslash escape the quotes it adds in the command lines it builds for
// programs, so those are refused.
func PowerShellCommand(req Request) (string, error) {
	for i, a := range append([]string{req.Dir, req.Exe}, req.Args...) {
		if a == "" || strings.ContainsAny(a, "\x00\r\n\"") || (i >= 2 && strings.HasSuffix(a, `\`)) {
			return "", errors.New("arguments must not be empty, end in a backslash, or contain NUL, line breaks or double quotes")
		}
	}
	var b strings.Builder
	b.WriteString("Set-Location -LiteralPath " + psQuote(req.Dir) + "\n")
	b.WriteString("& " + psQuote(req.Exe))
	for _, a := range req.Args {
		b.WriteString(" " + psQuote(a))
	}
	b.WriteString("\n")
	units := utf16.Encode([]rune(b.String()))
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// psQuoter doubles every quote that ends a single-quoted PowerShell string:
// the apostrophe and the typographic ‘ ’ ‚ ‛.
var psQuoter = strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")

func psQuote(s string) string { return "'" + psQuoter.Replace(s) + "'" }
