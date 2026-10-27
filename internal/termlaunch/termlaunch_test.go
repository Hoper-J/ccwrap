package termlaunch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The script runs the command in the folder with every argument intact,
// whatever quotes, spaces or shell syntax they contain, and removes itself.
func TestScriptRunsArgumentsVerbatim(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, `dir with space 'q' "dq" $(touch PWNED) ; echo`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "marker")
	exe := filepath.Join(root, "show.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\" > \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"--profile", `it's "x" $HOME`, "--", "--continue"}
	text, err := Script(Request{Dir: dir, Exe: exe, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "run.command")
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(script).CombinedOutput(); err != nil {
		t.Fatalf("run: %v %s", err, out)
	}
	got, _ := os.ReadFile(marker)
	want := strings.Join(append([]string{dir}, args...), "\n") + "\n"
	if string(got) != want {
		t.Fatalf("ran with:\n%q\nwant\n%q", got, want)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Fatal("the script must remove itself")
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "*", "PWNED")); len(matches) > 0 {
		t.Fatal("shell syntax in the folder name ran")
	}
	if _, err := Script(Request{Dir: dir, Exe: exe, Args: []string{"a\nb"}}); err == nil {
		t.Fatal("line breaks are refused")
	}
}

func TestValidateDir(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateDir(dir); err != nil {
		t.Fatalf("plain folder: %v", err)
	}
	for _, bad := range []string{"relative/x", filepath.Join(dir, "missing"), dir + "\x1b[31m", dir + "\u202e"} {
		if ValidateDir(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	file := filepath.Join(dir, "f")
	_ = os.WriteFile(file, nil, 0o600)
	if ValidateDir(file) == nil {
		t.Error("accepted a file")
	}
}

func TestChoose(t *testing.T) {
	apps := t.TempDir()
	for _, b := range []string{"Terminal.app", "iTerm.app"} {
		_ = os.MkdirAll(filepath.Join(apps, b), 0o755)
	}
	installed := Installed([]string{apps})
	if len(installed) != 2 || installed[0].ID != "terminal" || installed[1].ID != "iterm" {
		t.Fatalf("installed = %+v", installed)
	}
	state := t.TempDir()
	if in, _ := Choose(installed, state, ""); in.ID != "terminal" {
		t.Fatalf("default = %s", in.ID)
	}
	if in, _ := Choose(installed, state, "iTerm"); in.ID != "iterm" {
		t.Fatalf("Claude Code's choice = %s", in.ID)
	}
	if in, _ := Choose(installed, state, "Ghostty"); in.ID != "terminal" {
		t.Fatalf("an uninstalled Claude Code choice falls back: %s", in.ID)
	}
	if err := SavePreference(state, "terminal"); err != nil {
		t.Fatal(err)
	}
	if in, _ := Choose(installed, state, "iTerm"); in.ID != "terminal" {
		t.Fatalf("ccwrap's own choice wins: %s", in.ID)
	}
	if SavePreference(state, "nope") == nil {
		t.Fatal("unknown terminal saved")
	}
	if _, ok := Choose(nil, state, ""); ok {
		t.Fatal("nothing installed")
	}
}

// Each terminal is opened the way it runs the script: Terminal through
// open -a, iTerm by typing it into a new window's shell (AppleScript),
// Ghostty with -e.
func TestLaunchCommands(t *testing.T) {
	dir := t.TempDir()
	scripts := t.TempDir()
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"terminal", "open -a /Apps/Terminal.app SCRIPT"},
		{"iterm", `osascript -e tell application "iTerm" -e activate -e set w to (create window with default profile) -e tell current session of w to write text "SCRIPT" -e end tell`},
		{"ghostty", "open -na /Apps/Ghostty.app --args -e SCRIPT"},
	} {
		var term Terminal
		for _, x := range terminals {
			if x.ID == tc.id {
				term = x
			}
		}
		var got string
		err := launch(Installation{Terminal: term, AppPath: "/Apps/" + term.bundle}, Request{Dir: dir, Exe: "/bin/true"}, scripts, func(name string, args ...string) error {
			got = name + " " + strings.Join(args, " ")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		matches, _ := filepath.Glob(filepath.Join(scripts, "ccwrap-launch-*.command"))
		if len(matches) == 0 {
			t.Fatal("no script written")
		}
		script := matches[len(matches)-1]
		if info, _ := os.Stat(script); info.Mode().Perm() != 0o700 {
			t.Fatalf("script mode %v", info.Mode())
		}
		if got != strings.ReplaceAll(tc.want, "SCRIPT", script) {
			t.Errorf("%s:\n got %s\nwant %s", tc.id, got, strings.ReplaceAll(tc.want, "SCRIPT", script))
		}
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	if err := launch(Installation{Terminal: terminals[0]}, Request{Dir: "relative", Exe: "/bin/true"}, scripts, func(string, ...string) error { return nil }); err == nil {
		t.Fatal("a bad folder is refused before anything runs")
	}
}

// A launch script the terminal never ran (the window closed before its shell
// started, say) would stay forever: each launch first removes scripts older
// than staleScriptAge, and leaves newer ones, still being opened, alone.
func TestLaunchSweepsStaleScripts(t *testing.T) {
	scripts := t.TempDir()
	old := filepath.Join(scripts, "ccwrap-launch-0000000000000000.command")
	fresh := filepath.Join(scripts, "ccwrap-launch-1111111111111111.command")
	other := filepath.Join(scripts, "notes.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-staleScriptAge - time.Minute)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := launch(Installation{Terminal: terminals[0], AppPath: "/Apps/Terminal.app"}, Request{Dir: t.TempDir(), Exe: "/bin/true"}, scripts, func(string, ...string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale script still there: %v", err)
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}
