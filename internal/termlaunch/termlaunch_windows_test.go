package termlaunch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPowerShellHelperProcess stands in for ccwrap: it writes the folder it
// runs in and its arguments to $CCWRAP_PS_HELPER_OUT. As a test it does
// nothing.
func TestPowerShellHelperProcess(t *testing.T) {
	out := os.Getenv("CCWRAP_PS_HELPER_OUT")
	if out == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	wd, _ := os.Getwd()
	_ = os.WriteFile(out, []byte(strings.Join(append([]string{wd}, args[1:]...), "\n")), 0o600)
	os.Exit(0)
}

// The encoded command runs the program in the folder with every argument
// intact, whatever quotes or PowerShell and cmd.exe syntax they contain.
func TestPowerShellCommandRunsArgumentsVerbatim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's ‘q’ $x; & y")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"it's", "a b", "$HOME", "x;y", "&", "%PATH%", "‘smart’", "^caret", "(paren)", `back\slash`, "日本語 路径"}
	enc, err := PowerShellCommand(Request{Dir: dir, Exe: os.Args[0], Args: append([]string{"-test.run=^TestPowerShellHelperProcess$", "--"}, args...)})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", enc)
	cmd.Env = append(os.Environ(), "CCWRAP_PS_HELPER_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("powershell: %v\n%s", err, b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the program did not run: %v", err)
	}
	lines := strings.Split(string(got), "\n")
	// The same folder can read differently: a temp folder under a long user
	// name comes as 8.3 short names (RUNNER~1) from TEMP, PowerShell's in full.
	want, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := os.Stat(lines[0]); err != nil || !os.SameFile(ran, want) {
		t.Errorf("ran in %q, want %q", lines[0], dir)
	}
	if strings.Join(lines[1:], "\n") != strings.Join(args, "\n") {
		t.Errorf("got arguments\n%q\nwant\n%q", lines[1:], args)
	}
}
