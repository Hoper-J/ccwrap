package termlaunch

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// The script runs the command in the folder with every argument intact,
// whatever quotes, spaces or shell syntax they contain, and removes itself.
func TestScriptRunsArgumentsVerbatim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launch scripts are POSIX shell, for macOS terminals")
	}
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

// A terminal outside the usual folders (Homebrew can put casks anywhere)
// is found where Spotlight saw it; one in the usual folders needs no lookup.
func TestInstalledFallsBackToSpotlight(t *testing.T) {
	apps := t.TempDir()
	_ = os.MkdirAll(filepath.Join(apps, "Terminal.app"), 0o755)
	elsewhere := filepath.Join(t.TempDir(), "_", "Ghostty.app")
	_ = os.MkdirAll(elsewhere, 0o755)
	var asked []string
	got := find([]string{apps}, func(id string) string {
		asked = append(asked, id)
		if id == "com.mitchellh.ghostty" {
			return elsewhere
		}
		return ""
	}, nil)
	if len(got) != 2 || got[0].ID != "terminal" || got[1].ID != "ghostty" || got[1].AppPath != elsewhere {
		t.Fatalf("installed = %+v", got)
	}
	if strings.Join(asked, ",") != "com.googlecode.iterm2,com.mitchellh.ghostty" {
		t.Fatalf("asked Spotlight for %v", asked)
	}
}

// Spotlight's answer is used only for a real app bundle that is not in the
// Trash or nested inside another app.
func TestFirstApp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Spotlight paths are macOS paths")
	}
	root := t.TempDir()
	mk := func(p string) string { _ = os.MkdirAll(filepath.Join(root, p), 0o755); return filepath.Join(root, p) }
	trashed := mk(".Trash/Ghostty.app")
	nested := mk("Other.app/Contents/Ghostty.app")
	real := mk("Apps/_/Ghostty.app")
	if got := firstApp(strings.Join([]string{trashed, nested, filepath.Join(root, "Gone.app"), real}, "\n")); got != real {
		t.Fatalf("firstApp = %q, want %q", got, real)
	}
	if got := firstApp(""); got != "" {
		t.Fatalf("no answer = %q", got)
	}
}

func TestChoose(t *testing.T) {
	apps := t.TempDir()
	for _, b := range []string{"Terminal.app", "iTerm.app"} {
		_ = os.MkdirAll(filepath.Join(apps, b), 0o755)
	}
	installed := InstalledIn([]string{apps})
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
	if runtime.GOOS == "windows" {
		t.Skip("the terminals are macOS apps")
	}
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
			if x.ID == tc.id && x.bundle != "" {
				term = x
			}
		}
		var got string
		err := launch(Installation{Terminal: term, AppPath: "/Apps/" + term.bundle}, Request{Dir: dir, Exe: "/bin/true"}, scripts, func(name string, args ...string) error {
			got = name + " " + strings.Join(args, " ")
			return nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		matches, _ := filepath.Glob(filepath.Join(scripts, "ccwrap-launch-*.command"))
		if len(matches) == 0 {
			t.Fatal("no script written")
		}
		script := matches[len(matches)-1]
		if info, _ := os.Stat(script); testutil.HasUnixPermissions() && info.Mode().Perm() != 0o700 {
			t.Fatalf("script mode %v", info.Mode())
		}
		if got != strings.ReplaceAll(tc.want, "SCRIPT", script) {
			t.Errorf("%s:\n got %s\nwant %s", tc.id, got, strings.ReplaceAll(tc.want, "SCRIPT", script))
		}
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	if err := launch(Installation{Terminal: terminals[0]}, Request{Dir: "relative", Exe: "/bin/true"}, scripts, func(string, ...string) error { return nil }, nil); err == nil {
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
	if err := launch(Installation{Terminal: terminals[0], AppPath: "/Apps/Terminal.app"}, Request{Dir: t.TempDir(), Exe: "/bin/true"}, scripts, func(string, ...string) error { return nil }, nil); err != nil {
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

func decodePowerShell(t *testing.T, enc string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("not base64 UTF-16LE: %v", err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	return string(utf16.Decode(units))
}

// Every value is single-quoted, with the quotes PowerShell ends a string at
// doubled.
func TestPowerShellCommandQuotesEveryValue(t *testing.T) {
	enc, err := PowerShellCommand(Request{
		Dir:  `C:\it's ‘q’ $x; & y`,
		Exe:  `C:\Program Files\ccwrap\ccwrap.exe`,
		Args: []string{"--profile", "it's", "--", "--resume", "abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Set-Location -LiteralPath 'C:\\it''s ‘‘q’’ $x; & y'\n" +
		"& 'C:\\Program Files\\ccwrap\\ccwrap.exe' '--profile' 'it''s' '--' '--resume' 'abc'\n"
	if got := decodePowerShell(t, enc); got != want {
		t.Fatalf("script:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range [][]string{{`say "hi"`}, {""}, {"line\nbreak"}, {`ends in \`}} {
		if _, err := PowerShellCommand(Request{Dir: `C:\x`, Exe: `C:\x\y.exe`, Args: bad}); err == nil {
			t.Errorf("PowerShellCommand(%q) accepted", bad)
		}
	}
}

func TestWindowsTerminalCommandLine(t *testing.T) {
	var term Terminal
	for _, x := range terminals {
		if x.ID == "windows-terminal" {
			term = x
		}
	}
	dir := t.TempDir()
	var got []string
	err := launch(Installation{Terminal: term, AppPath: `C:\wt.exe`}, Request{Dir: dir, Exe: `C:\ccwrap.exe`, Args: []string{"--continue"}}, t.TempDir(), func(name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 || strings.Join(got[:6], " ") != `C:\wt.exe -w new powershell.exe -NoLogo -EncodedCommand` {
		t.Fatalf("ran %q", got)
	}
	if script := decodePowerShell(t, got[6]); !strings.Contains(script, "& 'C:\\ccwrap.exe' '--continue'") {
		t.Fatalf("script %q", script)
	}
}

// linuxEntry is the Linux terminal with id.
func linuxEntry(t *testing.T, id string) Terminal {
	t.Helper()
	for _, x := range terminals {
		if x.ID == id && x.goos == "linux" {
			return x
		}
	}
	t.Fatalf("no Linux terminal %q", id)
	return Terminal{}
}

// A Linux terminal is started, not waited for, on the fixed script with the
// folder, the program and its arguments after it, each its own argument.
func TestLinuxTerminalArguments(t *testing.T) {
	dir := t.TempDir()
	req := Request{Dir: dir, Exe: "/usr/bin/ccwrap", Args: []string{"--profile", "it's mine"}}
	for _, tc := range []struct{ id, flags string }{
		{"x-terminal-emulator", "-e"},
		{"ghostty", "-e"},
		{"kitty", ""},
		{"alacritty", "-e"},
		{"wezterm", "start --"},
		{"gnome-terminal", "--"},
		{"konsole", "-e"},
		{"xfce4-terminal", "-x"},
		{"mate-terminal", "-x"},
		{"tilix", "-e"},
		{"foot", ""},
		{"xterm", "-e"},
	} {
		var got []string
		ran := func(string, ...string) error { t.Errorf("%s: waited for the terminal", tc.id); return nil }
		start := func(name string, args ...string) error { got = append([]string{name}, args...); return nil }
		if err := launch(Installation{Terminal: linuxEntry(t, tc.id), AppPath: "/usr/bin/" + tc.id}, req, t.TempDir(), ran, start); err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		want := append([]string{"/usr/bin/" + tc.id}, strings.Fields(tc.flags)...)
		want = append(want, "/bin/sh", "-c", linuxShell, "sh", dir, "/usr/bin/ccwrap", "--profile", "it's mine")
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.id, got, want)
		}
	}
}

// The script runs the command in the folder with every argument intact, and
// on failure waits for Enter, keeping the exit status.
func TestLinuxShellRunsArgumentsVerbatim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	args := []string{"it's", "a b", "$HOME", "*", `"; rm -rf /"`}
	out, err := exec.Command("/bin/sh", append([]string{"-c", linuxShell, "sh", dir, "/bin/sh", "-c", `pwd -P; for a; do printf '[%s]\n' "$a"; done`, "x"}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	want := real + "\n"
	for _, a := range args {
		want += "[" + a + "]\n"
	}
	if string(out) != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}
	cmd := exec.Command("/bin/sh", "-c", linuxShell, "sh", dir, "/bin/sh", "-c", "exit 3")
	cmd.Stdin = strings.NewReader("\n")
	out, err = cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 || !strings.Contains(string(out), "[exit status 3; press Enter to close]") {
		t.Fatalf("failure: %v, %q", err, out)
	}
}

// Terminals are looked for on their own system only, and Linux ones only
// with a graphical session.
func TestPathExe(t *testing.T) {
	for _, x := range terminals {
		if x.goos != "" && x.goos != runtime.GOOS && pathExe(x) != "" {
			t.Errorf("%s (%s) found on %s", x.ID, x.goos, runtime.GOOS)
		}
	}
	if runtime.GOOS == "linux" {
		t.Setenv("DISPLAY", "")
		t.Setenv("WAYLAND_DISPLAY", "")
		if p := pathExe(linuxEntry(t, "xterm")); p != "" {
			t.Errorf("found %s without a graphical session", p)
		}
	}
	got := find(nil, nil, func(x Terminal) string {
		if x.goos == "linux" && (x.ID == "kitty" || x.ID == "xterm") {
			return "/usr/bin/" + x.ID
		}
		return ""
	})
	if len(got) != 2 || got[0].ID != "kitty" || got[1].ID != "xterm" || got[1].AppPath != "/usr/bin/xterm" {
		t.Fatalf("installed = %+v", got)
	}
}

// On Linux, $TERMINAL comes after the saved and Claude Code choices, then
// the system default, then the first installed.
func TestChooseOnLinux(t *testing.T) {
	installed := []Installation{
		{Terminal: linuxEntry(t, "x-terminal-emulator")},
		{Terminal: linuxEntry(t, "kitty")},
		{Terminal: linuxEntry(t, "konsole")},
	}
	state := t.TempDir()
	t.Setenv("TERMINAL", "")
	if in, _ := Choose(installed, state, ""); in.ID != "x-terminal-emulator" {
		t.Fatalf("default = %s", in.ID)
	}
	t.Setenv("TERMINAL", "/usr/bin/konsole")
	if in, _ := Choose(installed, state, ""); in.ID != "konsole" {
		t.Fatalf("$TERMINAL = %s", in.ID)
	}
	if in, _ := Choose(installed, state, "kitty"); in.ID != "kitty" {
		t.Fatalf("Claude Code's choice = %s", in.ID)
	}
	if in, _ := Choose(installed[1:], state, "alacritty"); in.ID != "konsole" {
		t.Fatalf("uninstalled choices fall through to $TERMINAL: %s", in.ID)
	}
}

// startDetached returns while the terminal still runs, and reports one that
// fails at once with what it printed.
func TestStartDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	began := time.Now()
	if err := startDetached("/bin/sh", "-c", "sleep 3"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > startCheck+time.Second {
		t.Fatalf("waited %s for a terminal that still runs", took)
	}
	err := startDetached("/bin/sh", "-c", "echo no display >&2; exit 2")
	if err == nil || !strings.Contains(err.Error(), "no display") {
		t.Fatalf("err = %v", err)
	}
}
