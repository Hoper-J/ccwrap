package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPrivateDir(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, "own")
	if err := os.Mkdir(own, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(root, "readable")
	if err := os.Mkdir(readable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readable, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, dir, want string
	}{
		{"own private dir", own, ""},
		{"missing dir", filepath.Join(root, "missing"), ""},
		{"readable by others", readable, ""},
		{"symlink", link, "it is a symlink"},
		{"not a directory", file, "it is not a directory"},
		{"writable by others", open, "others can write to it (mode 0777)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPrivateDir(tc.dir)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CheckPrivateDir(%s) = %v, want nil", tc.dir, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.dir) || !strings.Contains(err.Error(), "CCWRAP_RUNTIME_DIR") {
				t.Fatalf("CheckPrivateDir(%s) = %v, want an error naming the dir, %q and CCWRAP_RUNTIME_DIR", tc.dir, err, tc.want)
			}
		})
	}
}

func TestCheckPrivateDirRefusesSomeoneElsesDir(t *testing.T) {
	dir := t.TempDir()
	prev := getuid
	getuid = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { getuid = prev })

	err := CheckPrivateDir(dir)
	if err == nil || !strings.Contains(err.Error(), "it is owned by uid") {
		t.Fatalf("CheckPrivateDir = %v, want an owned-by error", err)
	}
}

func testPaths(root string) Paths {
	return Paths{
		RuntimeDir:   filepath.Join(root, "run"),
		StateDir:     filepath.Join(root, "state"),
		CACertPath:   filepath.Join(root, "state", "certs", "ca-cert.pem"),
		CALockPath:   filepath.Join(root, "state", "locks", "ca.lock"),
		CAKeyPath:    filepath.Join(root, "state", "certs", "ca-key.pem"),
		CABundlePath: filepath.Join(root, "state", "certs", "ca-bundle.pem"),
	}
}

func TestEnsurePathsTightensItsOwnRuntimeDir(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.RuntimeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePaths(p); err != nil {
		t.Fatalf("EnsurePaths = %v", err)
	}
	fi, err := os.Stat(p.RuntimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("runtime dir mode = %04o, want 0700", got)
	}
}

// A runtime dir someone else made is refused before chmod runs: chmod would
// fail on it anyway, and through a symlink it would change the target.
func TestEnsurePathsRefusesSomeoneElsesRuntimeDirUntouched(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.RuntimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := getuid
	getuid = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { getuid = prev })

	err := EnsurePaths(p)
	if err == nil || !strings.Contains(err.Error(), "it is owned by uid") {
		t.Fatalf("EnsurePaths = %v, want an owned-by error", err)
	}
	fi, statErr := os.Stat(p.RuntimeDir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("runtime dir mode = %04o after refusal, want it untouched (0755)", got)
	}
}

func TestEnsurePathsRefusesSymlinkedRuntimeDirUntouched(t *testing.T) {
	root := t.TempDir()
	p := testPaths(root)
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p.RuntimeDir); err != nil {
		t.Fatal(err)
	}

	err := EnsurePaths(p)
	if err == nil || !strings.Contains(err.Error(), "it is a symlink") {
		t.Fatalf("EnsurePaths = %v, want a symlink error", err)
	}
	fi, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlink target mode = %04o after refusal, want it untouched (0755)", got)
	}
}
