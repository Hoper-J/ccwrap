package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/app"
)

// ShortAppPaths returns deliberately short unix-socket paths so macOS tests do
// not trip over sockaddr_un.sun_path limits.
func ShortAppPaths(t testing.TB, socketName string) app.Paths {
	t.Helper()
	root := shortTempDir(t, "ccwrap-t-")
	paths := app.Paths{
		RuntimeDir:   filepath.Join(root, "r"),
		StateDir:     filepath.Join(root, "s"),
		SocketPath:   filepath.Join(root, socketName),
		LogPath:      filepath.Join(root, "s", "supervisor.log"),
		CACertPath:   filepath.Join(root, "s", "c", "ca-cert.pem"),
		CAKeyPath:    filepath.Join(root, "s", "c", "ca-key.pem"),
		CABundlePath: filepath.Join(root, "s", "c", "ca-bundle.pem"),
		CALockPath:   filepath.Join(root, "s", "l", "ca.lock"),
	}
	if runtime.GOOS == "darwin" && len(paths.SocketPath) >= 104 {
		t.Fatalf("short test socket path still too long for macOS: %q (%d)", paths.SocketPath, len(paths.SocketPath))
	}
	return paths
}

func shortTempDir(t testing.TB, prefix string) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		dir, err = os.MkdirTemp("", prefix)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
