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
	symlink(t, own, link)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	makeWritableByOthers(t, open)
	readable := filepath.Join(root, "readable")
	if err := os.Mkdir(readable, 0o700); err != nil {
		t.Fatal(err)
	}
	makeReadableByOthers(t, readable)

	for _, tc := range []struct {
		name, dir, want string
	}{
		{"own private dir", own, ""},
		{"missing dir", filepath.Join(root, "missing"), ""},
		{"readable by others", readable, ""},
		{"symlink", link, "it is a symlink"},
		{"not a directory", file, "it is not a directory"},
		{"writable by others", open, wantWritableByOthers},
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
	pretendOwnedBySomeoneElse(t, dir)

	err := CheckPrivateDir(dir)
	if err == nil || !strings.Contains(err.Error(), wantOwnedBy) {
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
	makeWritableByOthers(t, p.RuntimeDir)
	if err := EnsurePaths(p); err != nil {
		t.Fatalf("EnsurePaths = %v", err)
	}
	assertOwnerOnly(t, p.RuntimeDir)
}

// A runtime dir someone else made is refused before makePrivate runs: it
// would fail on it anyway, and through a symlink it would change the target.
func TestEnsurePathsRefusesSomeoneElsesRuntimeDirUntouched(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeReadableByOthers(t, p.RuntimeDir)
	pretendOwnedBySomeoneElse(t, p.RuntimeDir)
	before := permSnapshot(t, p.RuntimeDir)

	err := EnsurePaths(p)
	if err == nil || !strings.Contains(err.Error(), wantOwnedBy) {
		t.Fatalf("EnsurePaths = %v, want an owned-by error", err)
	}
	if got := permSnapshot(t, p.RuntimeDir); got != before {
		t.Fatalf("runtime dir permissions = %s after refusal, want them untouched (%s)", got, before)
	}
}

func TestEnsurePathsRefusesSymlinkedRuntimeDirUntouched(t *testing.T) {
	root := t.TempDir()
	p := testPaths(root)
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	makeReadableByOthers(t, target)
	before := permSnapshot(t, target)
	symlink(t, target, p.RuntimeDir)

	err := EnsurePaths(p)
	if err == nil || !strings.Contains(err.Error(), "it is a symlink") {
		t.Fatalf("EnsurePaths = %v, want a symlink error", err)
	}
	if got := permSnapshot(t, target); got != before {
		t.Fatalf("symlink target permissions = %s after refusal, want them untouched (%s)", got, before)
	}
}
