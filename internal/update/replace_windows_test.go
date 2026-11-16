package update

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRunningProgramHelper stands in for a running ccwrap; as a test it
// does nothing.
func TestRunningProgramHelper(t *testing.T) {
	if os.Getenv("CCWRAP_TEST_RUNNING") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// Upgrading replaces ccwrap.exe while a copy of it runs, as a session or
// ccwrap ui does during `ccwrap upgrade`; the old file goes once it is free.
func TestApplyReplacesARunningProgram(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccwrap.exe")
	if err := os.WriteFile(exe, program, 0o755); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(exe, "-test.run=^TestRunningProgramHelper$")
	running.Env = append(os.Environ(), "CCWRAP_TEST_RUNNING=1")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Process.Kill(); _, _ = running.Process.Wait() }()

	newBin := []byte("NEW BINARY BYTES")
	archive, sum := makeArchive(t, newBin)
	srv := releaseServer(t, "0.3.1", archive, fmt.Sprintf("%s  %s\n", sum, releaseArchive("0.3.1", runtime.GOOS, runtime.GOARCH)))
	defer srv.Close()
	if err := Apply(context.Background(), srv.Client(), srv.URL, "0.3.1", runtime.GOOS, runtime.GOARCH, exe, io.Discard); err != nil {
		t.Fatalf("Apply over a running program: %v", err)
	}
	if got, err := os.ReadFile(exe); err != nil || !bytes.Equal(got, newBin) {
		t.Fatalf("ccwrap.exe not replaced: %v", err)
	}
	olds, _ := filepath.Glob(exe + ".old-*")
	if len(olds) != 1 {
		t.Fatalf("moved-aside files = %v, want the running one", olds)
	}
	RemoveReplaced(exe)
	if _, err := os.Stat(olds[0]); err != nil {
		t.Fatalf("removed the file of a running program: %v", err)
	}
	_ = running.Process.Kill()
	_, _ = running.Process.Wait()
	for i := 0; i < 50; i++ { // the system lets go of the file shortly after the exit
		RemoveReplaced(exe)
		if _, err := os.Stat(olds[0]); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("moved-aside file still there once nothing ran it")
}

// runCopyAt copies this test binary to path and runs it there, as a ccwrap
// that is still running; stop ends it.
func runCopyAt(t *testing.T, path string) (stop func()) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, program, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-test.run=^TestRunningProgramHelper$")
	cmd.Env = append(os.Environ(), "CCWRAP_TEST_RUNNING=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
	t.Cleanup(stop)
	return stop
}

// npmExe is where npm puts ccwrap.exe under the global node_modules root.
func npmExe(root string) string {
	return filepath.Join(root, "ccwrap-cli", "node_modules", "@hoper-j", "ccwrap-cli-windows-amd64", "bin", "ccwrap.exe")
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// npm upgrades a package by moving its folder aside and deleting it at the
// end. With ccwrap.exe running from the package the delete fails; after
// FreeNPMPackage both steps work while it still runs, the package keeps a
// ccwrap.exe, and the moved file goes once the program has exited.
func TestFreeNPMPackageLetsNpmReplaceARunningProgramsPackage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prefix", "node_modules")
	exe := npmExe(root)
	stop := runCopyAt(t, exe)
	want, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}

	if err := FreeNPMPackage(exe); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(exe); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the package's ccwrap.exe is not the same program any more: %v", err)
	}
	if left := dirNames(t, filepath.Dir(exe)); len(left) != 1 {
		t.Fatalf("bin holds %v, want ccwrap.exe alone", left)
	}
	retired := filepath.Join(root, ".ccwrap-cli-AbCd1234")
	if err := os.Rename(filepath.Join(root, "ccwrap-cli"), retired); err != nil {
		t.Fatalf("npm's move of the package: %v", err)
	}
	if err := os.RemoveAll(retired); err != nil {
		t.Fatalf("npm's delete of the old package, with the program still running: %v", err)
	}

	aside := filepath.Join(root, asideDir)
	if moved := dirNames(t, aside); len(moved) != 1 {
		t.Fatalf("moved aside: %v, want the running program", moved)
	}
	RemoveReplaced(exe)
	if moved := dirNames(t, aside); len(moved) != 1 {
		t.Fatalf("removed the file of a running program: %v left", moved)
	}
	stop()
	for i := 0; i < 50; i++ { // the system lets go of the file shortly after the exit
		RemoveReplaced(exe)
		if _, err := os.Stat(aside); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("still there once nothing ran it: %v", dirNames(t, aside))
}

// A folder an earlier upgrade could not delete, with a ccwrap.exe that
// still runs in it, would make npm's next move into it fail: it is cleared,
// the running program moved aside with the rest.
func TestFreeNPMPackageClearsWhatAnEarlierUpgradeLeft(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prefix", "node_modules")
	exe := npmExe(root)
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("CURRENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(root, ".ccwrap-cli-AbCd1234")
	runCopyAt(t, filepath.Join(retired, "node_modules", "@hoper-j", "ccwrap-cli-windows-amd64", "bin", "ccwrap.exe"))
	if err := os.WriteFile(filepath.Join(retired, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := FreeNPMPackage(exe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(retired); !os.IsNotExist(err) {
		t.Fatalf("the folder the earlier upgrade left is still there: %v", err)
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "CURRENT" {
		t.Fatalf("the package's ccwrap.exe: %q, %v", got, err)
	}
	if moved := dirNames(t, filepath.Join(root, asideDir)); len(moved) != 2 {
		t.Fatalf("moved aside: %v, want the old running program and the current one", moved)
	}
}

// Only a ccwrap.exe inside npm's ccwrap-cli package is touched.
func TestFreeNPMPackageLeavesOtherInstallsAlone(t *testing.T) {
	for path, want := range map[string]string{
		`C:\Users\u\AppData\Roaming\npm\node_modules\ccwrap-cli\node_modules\@hoper-j\ccwrap-cli-windows-amd64\bin\ccwrap.exe`: `C:\Users\u\AppData\Roaming\npm\node_modules`,
		`D:\node_global\node_modules\ccwrap-cli\bin\ccwrap.exe`:                                                                `D:\node_global\node_modules`,
		`C:\Users\u\.local\bin\ccwrap.exe`:                                                                                     "",
		`C:\src\node_modules\other\bin\ccwrap.exe`:                                                                             "",
	} {
		if got := npmGlobalRoot(path); got != want {
			t.Errorf("npmGlobalRoot(%s) = %q, want %q", path, got, want)
		}
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "ccwrap.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("PROGRAM"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := FreeNPMPackage(exe); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, filepath.Dir(exe)); len(got) != 1 {
		t.Fatalf("bin holds %v", got)
	}
	if got := dirNames(t, dir); len(got) != 1 {
		t.Fatalf("created next to the install: %v", got)
	}
}
