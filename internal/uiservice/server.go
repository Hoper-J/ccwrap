package uiservice

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/localpeer"
	"github.com/Hoper-J/ccwrap/internal/profileapi"
	"github.com/Hoper-J/ccwrap/internal/projects"
	"github.com/Hoper-J/ccwrap/internal/statefile"
	"github.com/Hoper-J/ccwrap/internal/webguard"
)

// Options configure a Server.
type Options struct {
	Paths   app.Paths
	Version string
	// Exe is the ccwrap executable sessions are launched with.
	Exe    string
	Getenv func(string) string
	Logger *log.Logger
	// IdleTimeout is how long the service lives with no page connected and no
	// session running; 0 means 10 minutes.
	IdleTimeout time.Duration
	// PollInterval is how often sessions are rediscovered; 0 means 2s.
	PollInterval time.Duration
	// EndedRetention is how long an ended session stays listed; 0 means
	// 60s.
	EndedRetention time.Duration
}

// PortEnv fixes the HTTP port (for remote use through a forwarded port).
const PortEnv = "CCWRAP_UI_PORT"

const portFile = "ui-port.json"

// Server is one running ccwrap ui.
type Server struct {
	opts       Options
	dir        string
	info       Info
	token      string
	allowed    map[string]struct{}
	lister     projects.Lister
	hub        *hub
	profiles   *profileapi.Store
	tests      testRecord
	launchMu   sync.Mutex
	lastLaunch time.Time

	clients      atomic.Int64 // open event streams
	lastActivity atomic.Int64 // unix nanos of the last request
	stop         chan struct{}
	stopOnce     sync.Once
}

// New prepares a server; Serve runs it.
func New(opts Options) *Server {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "ccwrap ui: ", log.LstdFlags)
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 10 * time.Minute
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.EndedRetention == 0 {
		opts.EndedRetention = 60 * time.Second
	}
	s := &Server{opts: opts, dir: Dir(opts.Paths), allowed: webguard.AllowedHostsFromEnv(), profiles: profileapi.NewStore(opts.Paths.StateDir), stop: make(chan struct{})}
	s.hub = newHub(s)
	return s
}

// sseWriteTimeout ends an event stream whose client stopped reading: each
// write must finish within it. Keep-alive pings every 15s keep a reading
// client's writes well inside it. A variable for tests.
var sseWriteTimeout = 30 * time.Second

// connIdleTimeout closes a client's keep-alive connection to the ui's
// servers after it has sat idle this long; event streams are not cut, the
// timeout only runs between requests. A variable for tests.
var connIdleTimeout = 60 * time.Second

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Serve runs until ctx ends, Shutdown is called, or the service idles out.
func (s *Server) Serve(ctx context.Context) error {
	lockF, err := lock(s.dir)
	if err != nil {
		return err
	}
	defer lockF.Close()
	// The port and terminal choices are kept in the state directory.
	if err := os.MkdirAll(s.opts.Paths.StateDir, 0o700); err != nil {
		return err
	}

	sock := filepath.Join(s.dir, sockFile)
	_ = os.Remove(sock)
	unixLn, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = unixLn.Close()
		return err
	}
	httpLn, note, err := s.listenHTTP()
	if err != nil {
		_ = unixLn.Close()
		return err
	}
	httpLn = localpeer.GuardStrict(httpLn, s.opts.Getenv, s.opts.Logger.Printf, "ui port")

	s.token = randomHex(16)
	s.info = Info{
		PID: os.Getpid(), InstanceID: randomHex(8), Version: s.opts.Version, Exe: s.opts.Exe,
		HTTPAddr: httpLn.Addr().String(), Socket: sock, StartedAt: time.Now().UTC(), PortNote: note,
	}
	s.touch()

	api := s.apiMux()
	unixMux := http.NewServeMux()
	unixMux.Handle("/", api)
	unixMux.HandleFunc("POST /api/v1/ui/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go s.Shutdown()
	})
	unixSrv := &http.Server{Handler: s.track(unixMux), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: connIdleTimeout}
	httpSrv := &http.Server{Handler: s.track(s.guard(s.pageMux(api))), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: connIdleTimeout}

	errs := make(chan error, 2)
	go func() { errs <- unixSrv.Serve(unixLn) }()
	go func() { errs <- httpSrv.Serve(httpLn) }()
	if err := statefile.Write(s.dir, infoFile, s.info); err != nil {
		s.opts.Logger.Printf("write %s: %v", infoFile, err)
	}
	s.opts.Logger.Printf("serving %s (version %s)", s.info.URL(), s.info.Version)

	hubCtx, cancelHub := context.WithCancel(ctx)
	go s.hub.run(hubCtx)
	go s.watchIdle(hubCtx)

	select {
	case <-ctx.Done():
	case <-s.stop:
	case err = <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	cancelHub()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = unixSrv.Shutdown(shutdownCtx)
	_ = os.Remove(filepath.Join(s.dir, infoFile))
	_ = os.Remove(sock)
	s.opts.Logger.Printf("stopped")
	return err
}

// Shutdown makes Serve return.
func (s *Server) Shutdown() { s.stopOnce.Do(func() { close(s.stop) }) }

// Info is the running server's info (valid once Serve is listening).
func (s *Server) Info() Info { return s.info }

// listenHTTP binds loopback: the fixed port if set, else the last port used
// (so bookmarks keep working), else any free port.
func (s *Server) listenHTTP() (net.Listener, string, error) {
	var saved struct {
		Port int `json:"port"`
	}
	statefile.Read(s.opts.Paths.StateDir, portFile, &saved)
	fixed := 0
	if v := s.opts.Getenv(PortEnv); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			fixed = p
		}
	}
	try := saved.Port
	if fixed != 0 {
		try = fixed
	}
	note := ""
	if try != 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(try)))
		if err == nil {
			return ln, "", nil
		}
		if fixed != 0 {
			note = fmt.Sprintf("%s=%d is in use; using another port", PortEnv, fixed)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	if fixed == 0 {
		port := ln.Addr().(*net.TCPAddr).Port
		_ = statefile.Write(s.opts.Paths.StateDir, portFile, map[string]int{"port": port})
	}
	return ln, note, nil
}

func (s *Server) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

// track counts activity for the idle timer.
func (s *Server) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.touch()
		h.ServeHTTP(w, r)
	})
}

// watchIdle exits after IdleTimeout with no page connected and no session.
func (s *Server) watchIdle(ctx context.Context) {
	t := time.NewTicker(s.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.clients.Load() > 0 || s.hub.sessionCount() > 0 {
				s.touch()
				continue
			}
			if time.Since(time.Unix(0, s.lastActivity.Load())) > s.opts.IdleTimeout {
				s.opts.Logger.Printf("idle for %v with no page and no session; exiting", s.opts.IdleTimeout)
				s.Shutdown()
				return
			}
		}
	}
}

// guard applies the browser-facing checks: answered Hosts only, and for
// writes a same-site request carrying the page's token.
func (s *Server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !webguard.HostAllowed(r.Host, s.allowed) {
			http.Error(w, "ccwrap ui: refusing a request for an unrecognized Host header (DNS-rebinding guard); to serve it through a tunnel, start ccwrap ui with "+webguard.AllowedHostsEnv+"=<hostname>", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !webguard.SameSite(r, s.allowed) {
				writeAPIError(w, http.StatusForbidden, "cross_site", "cross-site request refused")
				return
			}
			if got := r.Header.Get("X-CCWRAP-Token"); subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeAPIError(w, http.StatusForbidden, "token", "missing or wrong X-CCWRAP-Token")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// pageMux serves the page, its assets and the API on the HTTP port.
func (s *Server) pageMux(api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.Handle("GET /assets/", assets())
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
