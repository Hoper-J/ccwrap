// Package uiservice is `ccwrap ui`: one per-user service that shows every
// running session, the user's projects and profiles, and starts sessions in
// the user's terminal. It reaches sessions only through their control
// sockets (/api/v1), so session ports stay proxy-only and the service can
// stop or crash without touching a session.
//
// It runs on demand and exits when idle. A lock file keeps one instance per
// user; ui.json says where it listens.
package uiservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/statefile"
)

// Info describes a running service; it is written to ui.json.
type Info struct {
	PID        int       `json:"pid"`
	InstanceID string    `json:"instance_id"`
	Version    string    `json:"version"`
	Exe        string    `json:"exe"`
	HTTPAddr   string    `json:"http_addr"`
	Socket     string    `json:"socket"`
	StartedAt  time.Time `json:"started_at"`
	// PortNote explains a port other than the one asked for.
	PortNote string `json:"port_note,omitempty"`
}

// URL is the service's page address.
func (i Info) URL() string { return "http://" + i.HTTPAddr + "/" }

const (
	infoFile = "ui.json"
	lockFile = "ui.lock"
	sockFile = "ui.sock"
	logFile  = "ui.log"
)

// Dir holds the service's runtime files: <runtime dir>/ui, mode 0700.
func Dir(paths app.Paths) string { return filepath.Join(paths.RuntimeDir, "ui") }

// ErrRunning means another instance holds the lock.
var ErrRunning = errors.New("ccwrap ui is already running")

// lock takes the single-instance lock; the returned file keeps it held.
func lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, err
	}
	return f, nil
}

// Running returns the running service's info, checked by asking it over its
// socket; ok is false when none answers.
func Running(ctx context.Context, paths app.Paths) (Info, bool) {
	var info Info
	statefile.Read(Dir(paths), infoFile, &info)
	if info.Socket == "" {
		return Info{}, false
	}
	var meta struct {
		InstanceID string `json:"instance_id"`
	}
	if err := getJSON(ctx, info.Socket, "/api/v1/meta", &meta); err != nil || meta.InstanceID != info.InstanceID {
		return Info{}, false
	}
	return info, true
}

func getJSON(ctx context.Context, socket, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: control.UnixTransport(socket), Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Stop asks a running service to exit and waits up to wait for it.
func Stop(ctx context.Context, paths app.Paths, wait time.Duration) (bool, error) {
	info, ok := Running(ctx, paths)
	if !ok {
		return false, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/api/v1/ui/shutdown", nil)
	resp, err := (&http.Client{Transport: control.UnixTransport(info.Socket), Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return true, err
	}
	_ = resp.Body.Close()
	// Done once the old process has let go of the lock, so a replacement
	// started right after can take it.
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if f, err := lock(Dir(paths)); err == nil {
			_ = f.Close()
			return true, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true, errors.New("ccwrap ui did not stop in time")
}

// Ensure returns a running service of this version and executable, replacing
// an older one and starting one if needed, waiting up to wait for it.
func Ensure(ctx context.Context, paths app.Paths, exe, version string, wait time.Duration) (Info, error) {
	// The service records its resolved executable; compare like with like.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if info, ok := Running(ctx, paths); ok {
		if info.Version == version && info.Exe == exe {
			return info, nil
		}
		if _, err := Stop(ctx, paths, 2*time.Second); err != nil {
			return Info{}, fmt.Errorf("replace older ccwrap ui: %w", err)
		}
	}
	if err := start(paths, exe); err != nil {
		return Info{}, err
	}
	deadline := time.Now().Add(wait)
	for {
		if info, ok := Running(ctx, paths); ok {
			return info, nil
		}
		if time.Now().After(deadline) {
			return Info{}, fmt.Errorf("ccwrap ui did not start within %v; see %s", wait, filepath.Join(Dir(paths), logFile))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// maxUILogBytes is where start moves ui.log aside to ui.log.1.
const maxUILogBytes = 1 << 20

// start launches `exe ui serve` detached from the caller's terminal.
func start(paths app.Paths, exe string) error {
	dir := Dir(paths)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(dir, logFile)
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxUILogBytes {
		_ = os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "ui", "serve")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap it when it exits: a session launch that started it lives on for
	// hours, and an unreaped child stays a zombie until then. A starter that
	// exits first (ccwrap open) leaves it to init.
	go func() { _ = cmd.Wait() }()
	return nil
}
