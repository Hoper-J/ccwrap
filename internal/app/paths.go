package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type Paths struct {
	RuntimeDir   string
	StateDir     string
	SocketPath   string
	LogPath      string
	CACertPath   string
	CAKeyPath    string
	CABundlePath string
	CALockPath   string
}

func DefaultPaths() (Paths, error) {
	if v := os.Getenv("CCWRAP_RUNTIME_DIR"); v != "" {
		return pathsFrom(os.Getenv("CCWRAP_RUNTIME_DIR"), defaultStateDir())
	}
	return pathsFrom(defaultRuntimeDir(), defaultStateDir())
}

func pathsFrom(runtimeDir, stateDir string) (Paths, error) {
	if v := os.Getenv("CCWRAP_STATE_DIR"); v != "" {
		stateDir = v
	}
	if runtimeDir == "" {
		return Paths{}, fmt.Errorf("runtime dir is empty")
	}
	if stateDir == "" {
		return Paths{}, fmt.Errorf("state dir is empty")
	}
	caDir := filepath.Join(stateDir, "certs")
	locksDir := filepath.Join(stateDir, "locks")
	return Paths{
		RuntimeDir:   runtimeDir,
		StateDir:     stateDir,
		CACertPath:   filepath.Join(caDir, "ca-cert.pem"),
		CAKeyPath:    filepath.Join(caDir, "ca-key.pem"),
		CABundlePath: filepath.Join(caDir, "ca-bundle.pem"),
		CALockPath:   filepath.Join(locksDir, "ca.lock"),
	}, nil
}

func EnsurePaths(p Paths) error {
	for _, dir := range []string{p.RuntimeDir, p.StateDir, filepath.Dir(p.CACertPath), filepath.Dir(p.CALockPath)} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// Checked after MkdirAll, so a directory someone else created first is
	// seen, and before chmod, which follows a symlink to its target.
	if err := checkDir(p.RuntimeDir, false); err != nil {
		return err
	}
	for _, dir := range []string{p.RuntimeDir, p.StateDir, filepath.Dir(p.CACertPath), filepath.Dir(p.CALockPath)} {
		if dir != "" {
			_ = os.Chmod(dir, 0o700)
		}
	}
	return CheckPrivateDir(p.RuntimeDir)
}

// getuid is os.Getuid; a seam for tests.
var getuid = os.Getuid

// CheckPrivateDir refuses a runtime directory that another user created or
// can write to. ccwrap trusts the manifests, sockets and ui.json it finds
// there, and the default location on Linux without XDG_RUNTIME_DIR is under
// the shared /tmp. The directory must be a real directory (not a symlink),
// owned by the current user, with no group or other write permission. A
// directory that does not exist passes: there is nothing in it to trust.
func CheckPrivateDir(dir string) error {
	return checkDir(dir, true)
}

func checkDir(dir string, perm bool) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var problem string
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		problem = "it is a symlink"
	case !fi.IsDir():
		problem = "it is not a directory"
	default:
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
			problem = fmt.Sprintf("it is owned by uid %d", st.Uid)
		} else if perm && fi.Mode().Perm()&0o022 != 0 {
			problem = fmt.Sprintf("others can write to it (mode %04o)", fi.Mode().Perm())
		}
	}
	if problem == "" {
		return nil
	}
	return fmt.Errorf("refusing runtime directory %s: %s; set CCWRAP_RUNTIME_DIR to a directory only you can write to", dir, problem)
}

func (p Paths) SessionsDir() string {
	return filepath.Join(p.RuntimeDir, "sessions")
}

func (p Paths) SessionDir(sessionID string) string {
	return filepath.Join(p.SessionsDir(), sessionID)
}

func (p Paths) SessionPaths(sessionID string) Paths {
	sessionDir := p.SessionDir(sessionID)
	return Paths{
		RuntimeDir:   sessionDir,
		StateDir:     p.StateDir,
		SocketPath:   filepath.Join(sessionDir, "control.sock"),
		LogPath:      filepath.Join(sessionDir, "supervisor.log"),
		CACertPath:   p.CACertPath,
		CAKeyPath:    p.CAKeyPath,
		CABundlePath: p.CABundlePath,
		CALockPath:   p.CALockPath,
	}
}

func defaultRuntimeDir() string {
	if v := os.Getenv("CCWRAP_RUNTIME_DIR"); v != "" {
		return v
	}
	u := currentUID()
	switch runtime.GOOS {
	case "darwin":
		tmp := os.Getenv("TMPDIR")
		tmp = strings.TrimRight(tmp, string(os.PathSeparator))
		if tmp == "" {
			tmp = os.TempDir()
		}
		return filepath.Join(tmp, fmt.Sprintf("ccwrap-%s", u))
	case "linux":
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			return filepath.Join(xdg, "ccwrap")
		}
		return filepath.Join(os.TempDir(), fmt.Sprintf("ccwrap-%s", u))
	default:
		return filepath.Join(os.TempDir(), fmt.Sprintf("ccwrap-%s", u))
	}
}

func defaultStateDir() string {
	if v := os.Getenv("CCWRAP_STATE_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "ccwrap")
	case "linux":
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, "ccwrap")
		}
		return filepath.Join(home, ".local", "state", "ccwrap")
	default:
		return filepath.Join(home, ".ccwrap")
	}
}

func currentUID() string {
	u, err := user.Current()
	if err == nil && u.Uid != "" {
		return u.Uid
	}
	return fmt.Sprintf("%d", os.Getuid())
}
