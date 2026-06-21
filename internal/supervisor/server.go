package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/certs"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

const (
	maxSessionRequests = 250
	maxSessionErrors   = 250
	maxSessionTrace    = 500
)

var errSingleSessionSupervisor = errors.New("single-session supervisor already has a session")

var procmetaMatches = procmeta.Matches

// LaunchContext is the secret-bearing in-process snapshot the supervisor retains
// from launch. It is never serialized, never crosses the socket, and never
// appears in any SwitchOutcome/CLI output. GC'd on supervisor exit.
//
// Inspection captures the on-disk settings snapshot from settings.InspectLaunch.
// Options carries the launch Options including the four file-content snapshot
// fields populated by the launcher (cmd/ccwrap composeLaunch) so the
// resolver can run on switch without reading disk for any file-backed input.
type LaunchContext struct {
	Options    preflight.Options          // includes the four file-content snapshot fields
	Inspection *settings.InspectionResult // ~/.claude/settings*.json snapshot
	Launch     *preflight.Result          // launch's *Result — for display projection + authBootstrap + identity
	// Launch-scoped toggles (the Live toggles). Not part of the Result — they
	// come from the launcher's --capture-* / --native-tls flags. Carried here so
	// createSession can publish them in-process without a SetRoute RPC.
	CaptureBodies    bool
	CaptureTelemetry bool
	NativeTLS        bool
	NativeTLSHello   []byte
}

// lookupEnv returns the value for key in a []string of "KEY=VALUE" entries
// (os.Environ() shape). Returns "" when key is absent.
func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// The per-session routing snapshot type (posture / resolved / live), its
// Result→resolved mapping (newResolved), the toggle/switch combinators
// (withResolved / withCaptureBodies / withCaptureTelemetry), and the
// Projection write (deriveInto) all live in posture.go. The launch + switch
// publish paths build a posture directly from *preflight.Result via
// newResolved; setRoute (the retained test fixture, below) builds one from a
// model.SessionRouteRequest via resolvedFromRequest.

type sessionState struct {
	mu       sync.RWMutex
	public   model.Session
	requests []model.RequestRecord
	errors   []model.ErrorRecord
	trace    []model.TraceRecord
	proxy    *sessionProxy
	// bodies spills captured request bodies to <RuntimeDir>/bodies
	// when capture is enabled; the 250-ring holds only
	// model.RequestBodyRef. Single-session supervisor ⇒ s.paths.RuntimeDir
	// is already the per-session runtime dir.
	bodies *bodyStore
	// active is the immutable-posture holder. Lock-free
	// readers on the hot path use active.Load(); the publish paths (launch +
	// SwitchProfile + the Set* toggles, plus the setRoute test fixture) do
	// Store under sess.mu.Lock as part of the single-critical-section publish
	// that also eagerly derives the Projection fields into sess.public via
	// deriveInto. createSession installs a safe-zero *posture so readers
	// between createSession and the first publish never observe nil.
	active atomic.Pointer[posture]
	// switchMu serializes Supervisor.SwitchProfile calls per-session.
	// Acquired immediately after session lookup; held across the full
	// validate → classify → publish 4-step procedure so concurrent switches
	// cannot race past each other into publishPosture. Independent of
	// sess.mu (which gates the single-critical-section publish itself).
	switchMu sync.Mutex
	// profileToken is the per-session CSRF nonce that the browser must echo
	// as X-CCWRAP-Profile-Token on POST /profile/switch. Generated
	// once at createSession from 16 cryptographically-random bytes; never
	// rotated; never logged. Lowercase hex.
	profileToken string
	// mirroredHelloRaw is CC's raw ClientHello bytes, captured once on the first
	// api.anthropic.com CONNECT and reused (re-parsed per dial) by the native-TLS
	// upstream dialer. A client property: stable per undici version, preserved
	// across profile switch; drainSupersededTransports MUST NOT touch it.
	mirroredHelloRaw atomic.Pointer[[]byte]
	// nativeTLSBlocked is the live native-TLS block flag, written by
	// recordNativeTLS under sess.mu: true while the most recent dial BLOCKED the
	// request (fail-closed — the fingerprint could not be mirrored, so no request
	// was sent rather than a de-anonymizing one), false once a mirror dial
	// succeeds again. Source of truth for the error-level Health contribution
	// recordNativeTLS sets and for recordRequest's heal-to-ok gate; guarded by sess.mu.
	nativeTLSBlocked bool
	// nativeTLSHelloLoaded is true when mirroredHelloRaw was PRE-SEEDED from a
	// loaded hello (CCWRAP_NATIVE_TLS_HELLO) rather than captured live; guarded
	// by sess.mu. Sticky — never reset by a later nil-hello profile-switch setRoute.
	nativeTLSHelloLoaded bool
}

type Supervisor struct {
	paths       app.Paths
	ca          *certs.Manager
	startedAt   time.Time
	idleTimeout time.Duration
	launchCtx   *LaunchContext

	// unmaskCredentials is the CCWRAP_UNMASK_CREDENTIALS=1 escape hatch.
	// When true, bodyredact's JSON-field redaction is
	// bypassed and OAuth refresh_token / access_token values appear in
	// plaintext in the inspect drawer AND the /tmp/.../bodies/<id>.json
	// spill files. Read once at New() from the process env (only "1" or
	// case-insensitive "true" enables); never togglable mid-session — the
	// user must restart with the env unset to mask again. Mirrored to
	// model.Session.CaptureBodiesUnmasked so the inspect-web ribbon shows
	// a persistent danger-color marker.
	unmaskCredentials bool

	transport *http.Transport
	logger    *log.Logger

	mu           sync.RWMutex
	session      *sessionState
	subscribers  map[int]chan model.Event
	nextSubID    int
	nextEventSeq uint64
	lastActive   time.Time

	// profileFileMu serializes load+mutate+OverwriteFile sequences across
	// the profile-CRUD handlers (add / edit / rm / set-default) WITHIN this
	// supervisor process. Without it, two concurrent in-process requests on
	// the same profiles.json (e.g. a browser retry, or two browser tabs hitting
	// this supervisor's control surface) interleave their load-check-write and
	// the second write silently clobbers the first. The mutex is on the
	// Supervisor (not on a session) because profiles.json is shared across all
	// sessions that share the StateDir.
	//
	// Scope: intra-process only. A separate `ccwrap profile ...` CLI invocation
	// runs in its own OS process and does NOT acquire this mutex, so a
	// CLI-vs-supervisor concurrent edit of the same profiles.json is NOT
	// serialized here. Closing that cross-process window would require a file
	// lock (flock) acquired by both the supervisor handlers and the CLI; not
	// done today.
	profileFileMu sync.Mutex

	httpServer *http.Server
	listener   net.Listener
	closed     chan struct{}
	stopOnce   sync.Once
}

// runtimeEgressFlag returns the launcher --egress-proxy flag value as
// a raw string (the same shape preflight.Options.EgressProxy holds).
// Empty when the launcher did not set --egress-proxy. Used by
// handle_egress_probe.go::synthesizeActiveSessionProfile to recover
// what the active session is actually exiting through when a profile
// is mode=inherit.
func (s *Supervisor) runtimeEgressFlag() string {
	if s.launchCtx == nil {
		return ""
	}
	return s.launchCtx.Options.EgressProxy
}

func New(paths app.Paths, idleTimeout time.Duration, launchCtx *LaunchContext) (*Supervisor, error) {
	if err := app.EnsurePaths(paths); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(paths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	logger := log.New(logFile, "ccwrap-supervisor ", log.LstdFlags|log.Lmicroseconds)
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	unmask := readUnmaskCredentialsEnv()
	if unmask {
		// One-time loud stderr warning. The inspect ribbon also surfaces a
		// persistent in-session marker (CaptureBodiesUnmasked) so a forgotten
		// env flag remains visible after the terminal scrolls past this line.
		fmt.Fprintln(os.Stderr, "ccwrap: WARNING — CCWRAP_UNMASK_CREDENTIALS=1 — captured request and response bodies are NOT redacted. OAuth refresh_token / access_token values (including in token-endpoint responses) will appear in plaintext in the inspect drawer and the /tmp/.../bodies/<id>.json spill files. Unset the env to restore default masking.")
	}
	return &Supervisor{
		paths:             paths,
		ca:                certs.NewManager(paths),
		startedAt:         time.Now(),
		idleTimeout:       idleTimeout,
		launchCtx:         launchCtx,
		unmaskCredentials: unmask,
		transport:         transport,
		logger:            logger,
		subscribers:       map[int]chan model.Event{},
		lastActive:        time.Now(),
		closed:            make(chan struct{}),
	}, nil
}

// readUnmaskCredentialsEnv reads CCWRAP_UNMASK_CREDENTIALS from the supervisor
// process env. Accepts ONLY "1" or case-insensitive "true" — same accept-set
// as cmd/ccwrap's truthyEnv helper. Anything else (incl. "yes",
// "on", "TRUE", whitespace, empty) is treated as false. Indirected through
// a package-level var so tests can stub the env source without setenv races.
var readUnmaskCredentialsEnv = func() bool {
	v := strings.TrimSpace(os.Getenv("CCWRAP_UNMASK_CREDENTIALS"))
	return v == "1" || strings.EqualFold(v, "true")
}

// launchContext returns the in-process secret-bearing snapshot retained from
// launch. Package-private — never serialized, never crosses the socket.
func (s *Supervisor) launchContext() *LaunchContext { return s.launchCtx }

func (s *Supervisor) StartedAt() time.Time {
	return s.startedAt
}

func (s *Supervisor) Run(ctx context.Context) error {
	if err := app.EnsurePaths(s.paths); err != nil {
		return err
	}
	if err := s.ca.EnsureCA(); err != nil {
		return err
	}
	// macOS caps unix-socket paths at 104 bytes (sun_path); a deep $TMPDIR can
	// push ccwrap's session socket past it, which net.Listen would surface only
	// as a bare "bind: invalid argument". Fail with an actionable message first.
	if runtime.GOOS == "darwin" && len(s.paths.SocketPath) >= 104 {
		return fmt.Errorf("control socket path is too long for macOS (%d ≥ 104 bytes): %q\n"+
			"  ccwrap puts the socket under $TMPDIR — set a shorter TMPDIR (e.g. TMPDIR=/tmp) and relaunch",
			len(s.paths.SocketPath), s.paths.SocketPath)
	}
	if err := os.RemoveAll(s.paths.SocketPath); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", s.paths.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.paths.SocketPath, err)
	}
	if err := os.Chmod(s.paths.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.listener = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/shutdown", s.handleShutdown)
	mux.HandleFunc("/v1/sessions", s.handleSessions)
	mux.HandleFunc("/v1/sessions/", s.handleSessionSubresources)
	mux.HandleFunc("/v1/requests", s.handleRequests)
	mux.HandleFunc("/v1/errors", s.handleErrors)
	mux.HandleFunc("/v1/trace", s.handleTrace)
	mux.HandleFunc("/v1/events", s.handleEvents)

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go s.monitorSessions(ctx)
	go s.reapIdle(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()

	err = s.httpServer.Serve(ln)
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Supervisor) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		close(s.closed)
		s.mu.Lock()
		sess := s.session
		s.mu.Unlock()
		if sess != nil && sess.proxy != nil {
			_ = sess.proxy.Close()
		}
		if s.httpServer != nil {
			err = s.httpServer.Shutdown(ctx)
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.paths.SocketPath)
	})
	return err
}

func (s *Supervisor) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := s.statusSnapshot()
	writeJSON(w, status)
}

func (s *Supervisor) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]string{"schema_version": model.ControlAPIVersion, "status": "terminating"})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
}

func (s *Supervisor) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, model.SessionsResponse{SchemaVersion: model.ControlAPIVersion, Sessions: s.listSessions()})
	case http.MethodPost:
		var req model.SessionCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sess, err := s.createSession(req)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errSingleSessionSupervisor) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, model.SessionCreateResponse{SchemaVersion: model.ControlAPIVersion, Session: sess})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Supervisor) handleSessionSubresources(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sess := s.getSessionPublic(id)
		if sess == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, sess)
		return
	}
	action := parts[1]
	switch action {
	case "route":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req model.SessionRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.setRoute(id, req); err != nil {
			// Typed-409 surface: a routeSetupError carries the
			// {reason_code, message} pair the client decodes into a
			// *control.RouteError via the typed-409 branch in client.go.
			var rse *routeSetupError
			if errors.As(err, &rse) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"reason_code": rse.Code,
					"message":     rse.Message,
				})
				return
			}
			status := http.StatusBadRequest
			if !strings.Contains(err.Error(), "not found") {
				status = http.StatusInternalServerError
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]string{"schema_version": model.ControlAPIVersion, "status": "ok"})
	case "attach":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req model.SessionAttachRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.attachSession(id, req); err != nil {
			status := http.StatusBadRequest
			if !strings.Contains(err.Error(), "not found") {
				status = http.StatusInternalServerError
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]string{"schema_version": model.ControlAPIVersion, "status": "ok"})
	case "close":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req model.SessionCloseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := s.closeSession(id, req.Reason); err != nil {
			status := http.StatusBadRequest
			if !strings.Contains(err.Error(), "not found") {
				status = http.StatusInternalServerError
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]string{"schema_version": model.ControlAPIVersion, "status": "ok"})
	case "profile":
		// SwitchProfile control op. Body is
		// {"name": "<profile>"}; the response is the structured SwitchOutcome
		// at HTTP 200. Errors live INSIDE the outcome (never returned or logged
		// raw across any boundary), so a NoSuchSession / RejectedInvalid /
		// RefusedNeedsRelaunch is still a 200, with the outcome's Result
		// discriminating. Only transport-level errors (malformed body, method
		// not allowed) emit non-2xx.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := s.SwitchProfile(id, body.Name)
		writeJSON(w, out)
	default:
		http.NotFound(w, r)
	}
}

func (s *Supervisor) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, model.RequestsResponse{SchemaVersion: model.ControlAPIVersion, Requests: s.listRequests(r.URL.Query().Get("session_id"))})
}

func (s *Supervisor) handleErrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, model.ErrorsResponse{SchemaVersion: model.ControlAPIVersion, Errors: s.listErrors(r.URL.Query().Get("session_id"))})
}

func (s *Supervisor) handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, model.TraceResponse{SchemaVersion: model.ControlAPIVersion, Trace: s.listTrace(r.URL.Query().Get("session_id"))})
}

func (s *Supervisor) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.serveEventStream(w, r, r.URL.Query().Get("session_id"))
}

// eventForWire decorates a request event's payload with its Go-derived
// class so the live JS never re-derives. Only the
// "request" event is rewrapped; all other events pass through unchanged.
// model.RequestRecord and the broadcast/recordRequest path are untouched.
func eventForWire(ev model.Event) model.Event {
	if ev.Type == "request" {
		if rec, ok := ev.Data.(model.RequestRecord); ok {
			ev.Data = classifiedRecord{Class: recordClass(rec), RequestRecord: rec}
		}
	}
	return ev
}

func (s *Supervisor) serveEventStream(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsubscribe := s.subscribe()
	defer unsubscribe()
	_, _ = io.WriteString(w, "retry: 1500\n")
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if sessionID != "" && ev.SessionID != sessionID {
				continue
			}
			data, _ := json.Marshal(eventForWire(ev))
			_, _ = fmt.Fprintf(w, "id: %s\n", ev.ID)
			_, _ = fmt.Fprintf(w, "event: %s\n", ev.Type)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepAlive.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		}
	}
}

func (s *Supervisor) createSession(req model.SessionCreateRequest) (model.Session, error) {
	// Double-check pattern: cheap RLock fast-path, allocate the proxy
	// (which calls net.Listen) outside the lock, then reacquire the
	// write lock to install. A racer that wins between RUnlock and Lock
	// causes us to throw away the just-allocated proxy — wasteful but
	// not racy. Keeping listen out of the critical section prevents
	// concurrent createSession requests from serializing on TCP bind.
	s.mu.RLock()
	hasSession := s.session != nil
	s.mu.RUnlock()
	if hasSession {
		return model.Session{}, errSingleSessionSupervisor
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newID()
	}
	sess := &sessionState{}
	// Install a safe-zero posture before anything else
	// publishes, so any request hitting the hot path between createSession
	// and the first publish observes a zero-valued (non-nil) snapshot —
	// reproducing the pre-publish zero-routing behavior.
	sess.active.Store(&posture{})
	// CSRF nonce: 16 cryptographically-random bytes (128 bits
	// of entropy), hex-encoded. Set once, never rotated.
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return model.Session{}, fmt.Errorf("ccwrap: failed to seed profile token: %w", err)
	}
	sess.profileToken = hex.EncodeToString(tok[:])
	// Per-session body spill store (256 MB budget). The
	// supervisor is single-session, so s.paths.RuntimeDir IS the
	// per-session runtime dir; bodyStore writes to <RuntimeDir>/bodies.
	sess.bodies = newBodyStore(s.paths.RuntimeDir, 256*1024*1024)
	sess.public = model.Session{
		ID:                id,
		Name:              req.Name,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		State:             model.StateCreated,
		LauncherPID:       req.LauncherPID,
		SupervisorPID:     os.Getpid(),
		RouteSource:       model.RouteSourceFallback,
		RouteClass:        model.RouteClassFirstParty,
		RouteConfigSource: "fallback_default",
		AuthMode:          model.AuthModePassthrough,
		AuthConfigSource:  "none",
		AuthSource:        model.AuthSourceNone,
		AuthPolicy:        model.AuthPolicyFirstPartyPassthrough,
		AuthBootstrap:     model.AuthBootstrapNotNeeded,
		AuthBootstrapKind: model.AuthBootstrapKindNone,
		FailPolicy:        model.FailClosed,
		ModelAliasMode:    model.ModelAliasDisabled,
		ModelAliasStrict:  true,
		// CCWRAP_UNMASK_CREDENTIALS is process-wide + set at supervisor New();
		// mirror onto every session it creates. Immutable for the lifetime
		// of the session — the user must restart with the env unset to
		// restore masking. The publish paths (deriveInto) / SetCaptureBodies do
		// NOT touch this field (it is outside the routing posture cycle).
		CaptureBodiesUnmasked: s.unmaskCredentials,
	}
	proxy, err := newSessionProxy(s, sess)
	if err != nil {
		return model.Session{}, err
	}
	sess.proxy = proxy
	sess.public.ProxyListenAddr = proxy.ListenAddr()
	s.mu.Lock()
	if s.session != nil {
		s.mu.Unlock()
		_ = proxy.Close()
		return model.Session{}, errSingleSessionSupervisor
	}
	s.session = sess
	s.lastActive = time.Now()
	s.mu.Unlock()
	s.broadcast("session_created", id, sess.snapshot())
	// Launch publish, in-process and Result-direct: the launcher handed us the
	// launch *preflight.Result via LaunchContext, so build the posture from it
	// via newResolved (the single Result→resolved mapping) and publish here —
	// the session is created WITH its posture, no SetRoute RPC and no
	// SessionRouteRequest round-trip. The launch toggles ride the live half.
	// One sess.mu critical section: atomic Store + the state transition +
	// hello pre-seed + trace + the EAGER deriveInto that keeps sess.public the
	// live display source of truth (snapshot() stays a plain public copy).
	if lc := s.launchCtx; lc != nil && lc.Launch != nil && lc.Launch.APIBaseURL != nil {
		p := posture{r: newResolved(lc.Launch), l: live{captureBodies: lc.CaptureBodies, captureTelemetry: lc.CaptureTelemetry, nativeTLS: lc.NativeTLS}}
		now := time.Now()
		sess.mu.Lock()
		sess.active.Store(&p)
		if sess.public.State == model.StateCreated {
			sess.public.State = model.StateLaunching
		}
		sess.public.UpdatedAt = now
		// Pre-seed the mirrored hello from a LOADED ClientHello
		// (CCWRAP_NATIVE_TLS_HELLO) before deriveInto reads nativeTLSHelloLoaded
		// into the projection. Guarded on len>0 so the sticky flag is only ever
		// set true here.
		if len(lc.NativeTLSHello) > 0 {
			cp := append([]byte(nil), lc.NativeTLSHello...)
			sess.mirroredHelloRaw.Store(&cp)
			sess.nativeTLSHelloLoaded = true
		}
		// trace.Detail is userinfo-stripped at construction (the display field
		// is built userinfo-free in newResolved), matching the public-mirror
		// strip — the launch path is the only entry that may carry an embedded
		// credential in APIBaseURL.
		sess.appendTraceLocked(model.TraceRecord{Timestamp: now, SessionID: id, Category: "route", Summary: "session route configured", Detail: p.r.display.apiBaseURL})
		p.deriveInto(&sess.public, sess.currentDialStateLocked())
		sess.mu.Unlock()
		s.touch()
		s.broadcast("session_updated", id, sess.snapshot())
	}
	return sess.snapshot(), nil
}

// routeSetupError is the typed sentinel returned by setRoute when /route is
// called after the session transitions out of the launch-only states
// {StateCreated, StateLaunching}. The public wire format is the
// pair {reason_code, message}; this internal type carries the same two fields
// so the HTTP handler can serialize them directly without touching
// internal/control (preserving the supervisor → control one-way dependency).
// The matching client-side decoder lives at internal/control/errors.go's
// RouteError.
type routeSetupError struct {
	Code    string
	Message string
}

func (e *routeSetupError) Error() string {
	if e == nil {
		return "route setup error: <nil>"
	}
	if e.Message == "" {
		return fmt.Sprintf("route setup error: %s", e.Code)
	}
	return fmt.Sprintf("route setup error [%s]: %s", e.Code, e.Message)
}

// setRoute is RETAINED AS A TEST FIXTURE. Production launch + SwitchProfile
// publish a posture directly from *preflight.Result via newResolved (see
// createSession and SwitchProfile); no production caller reaches setRoute or
// model.SessionRouteRequest. The supervisor's control surface still exposes the
// /route handler + control.Client.SetRoute so the test suite can configure a
// session's routing posture with a literal SessionRouteRequest (~90 call sites)
// without migrating each to a *preflight.Result. resolvedFromRequest mirrors
// newResolved field-for-field but reads from the request, so a fixture-built
// posture is value-identical to a production one. Keep this and
// SessionRouteRequest in lockstep with newResolved when the routing shape
// changes.
func (s *Supervisor) setRoute(id string, req model.SessionRouteRequest) error {
	sess := s.getSession(id)
	if sess == nil {
		return fmt.Errorf("session %s not found", id)
	}
	// /route is the startup-only single validated publish path.
	// Accept only at StateCreated (the launcher's initial SetRoute) or
	// StateLaunching (setRoute itself does Created→Launching, so a second
	// pre-attach /route still lands in this allow-list). Any later state —
	// StateAttached / StateActive / StateEnded — surfaces a
	// typed RouteSetupAfterAttach refusal; the HTTP handler maps this to a
	// 409 Conflict with the structured body. The pinned non-secret message
	// contains the offending state so callers see WHY without leaking
	// userinfo or credentials.
	sess.mu.RLock()
	state := sess.public.State
	sess.mu.RUnlock()
	if state != model.StateCreated && state != model.StateLaunching {
		return &routeSetupError{
			Code:    "RouteSetupAfterAttach",
			Message: fmt.Sprintf("/route refused: session is in state %q (post-attach); SwitchProfile is the runtime swap path", state),
		}
	}
	r, err := resolvedFromRequest(req)
	if err != nil {
		return err
	}
	p := posture{r: r, l: live{captureBodies: req.CaptureRequestBodies, captureTelemetry: req.CaptureTelemetry, nativeTLS: req.NativeTLS}}
	now := time.Now()
	// Pre-seed the mirrored hello from a LOADED ClientHello (override live
	// capture) when one is carried. Done inside the same critical section as the
	// publish so deriveInto reads the flag into sess.public.NativeTLSLoaded.
	// Guarded on len>0 so a later nil-hello fixture call never resets the flag or
	// clobbers the pre-seeded mirroredHelloRaw.
	//
	// Single-critical-section publish: atomic Store(*posture) + EAGER deriveInto
	// mirror of the Projection fields into sess.public. Initial route setup is
	// authoritative for the launch-scoped toggles (no prior live values to
	// protect), so the live half is taken straight from the request. The three
	// URL-string display fields are userinfo-stripped inside newResolved/
	// resolvedFromRequest; the trace Detail reuses that stripped apiBaseURL.
	sess.mu.Lock()
	if len(req.NativeTLSHello) > 0 {
		cp := append([]byte(nil), req.NativeTLSHello...)
		sess.mirroredHelloRaw.Store(&cp)
		sess.nativeTLSHelloLoaded = true
	}
	sess.active.Store(&p)
	if sess.public.State == model.StateCreated {
		sess.public.State = model.StateLaunching
	}
	sess.public.UpdatedAt = now
	sess.appendTraceLocked(model.TraceRecord{Timestamp: now, SessionID: id, Category: "route", Summary: "session route configured", Detail: p.r.display.apiBaseURL})
	p.deriveInto(&sess.public, sess.currentDialStateLocked())
	sess.mu.Unlock()
	s.touch()
	s.broadcast("session_updated", id, sess.snapshot())
	return nil
}

// resolvedFromRequest mirrors newResolved but reads from a
// model.SessionRouteRequest (the test fixture's input). It TRANSCRIBES the
// field mapping of the deleted setRoute inline constructor + publicProjection
// mirror so a fixture-built posture is value-identical to a production one:
//   - apiBaseURL parsed via url.Parse (fallible — bad URL is a fixture bug);
//   - modelalias.Config rebuilt via modelalias.New(Forward, Source, Strict &&
//     !ProviderModelPassthrough) then ProviderModelPassthrough set (and Strict
//     forced false when passthrough), matching the old inline build;
//   - upstreamheaders.Config rebuilt via upstreamheaders.New(Headers, Source)
//     with Fingerprint overlaid when the request carries one;
//   - the three URL/summary display strings userinfo-stripped;
//   - ModelAliasMode derived from Enabled(); FailPolicy taken from the request
//     (the launch fixture always sends FailClosed).
func resolvedFromRequest(req model.SessionRouteRequest) (resolved, error) {
	apiBase, err := url.Parse(req.APIBaseURL)
	if err != nil {
		return resolved{}, fmt.Errorf("parse api_base_url: %w", err)
	}
	aliasCfg, err := modelalias.New(req.ModelAlias.Forward, req.ModelAlias.Source, req.ModelAlias.Strict && !req.ModelAlias.ProviderModelPassthrough)
	if err != nil {
		return resolved{}, fmt.Errorf("model_alias: %w", err)
	}
	aliasCfg.ProviderModelPassthrough = req.ModelAlias.ProviderModelPassthrough
	if aliasCfg.ProviderModelPassthrough {
		aliasCfg.Strict = false
	}
	aliasMode := model.ModelAliasDisabled
	if aliasCfg.Enabled() {
		aliasMode = model.ModelAliasRewrite
	}
	upstreamHeaderCfg, err := upstreamheaders.New(req.UpstreamHeaders, req.UpstreamHeaderSource)
	if err != nil {
		return resolved{}, fmt.Errorf("upstream_headers: %w", err)
	}
	if req.UpstreamHeaderFingerprint != "" {
		upstreamHeaderCfg.Fingerprint = req.UpstreamHeaderFingerprint
	}
	return resolved{
		apiBaseURL:      apiBase,
		overrideAuth:    req.OverrideAuth,
		egress:          req.Egress,
		modelAlias:      aliasCfg,
		upstreamHeaders: upstreamHeaderCfg,
		routeClass:      req.RouteClass,
		authBootstrap:   req.AuthBootstrap,
		profileName:     req.ActiveProfileName,
		profileProvider: req.ActiveProfileProvider,
		missingAuthEnv:  req.MissingAuthEnv,
		display: postureDisplay{
			apiBaseURL:                         stripUserinfoString(req.APIBaseURL),
			exactUpstreamHost:                  req.ExactUpstreamHost,
			exactUpstreamBase:                  stripUserinfoString(req.ExactUpstreamBase),
			egressSummary:                      stripUserinfoString(req.Egress.Summary),
			routeSource:                        req.RouteSource,
			routeConfigSource:                  req.RouteConfigSource,
			authMode:                           req.AuthMode,
			authSource:                         req.AuthSource,
			authConfigSource:                   req.AuthConfigSource,
			authPolicy:                         req.AuthPolicy,
			authBootstrapKind:                  req.AuthBootstrapKind,
			egressMode:                         req.Egress.Mode,
			egressSource:                       req.Egress.Source,
			modelAliasMode:                     aliasMode,
			modelAliasSource:                   aliasCfg.Source,
			modelAliasStrict:                   aliasCfg.Strict,
			modelAliasProviderModelPassthrough: aliasCfg.ProviderModelPassthrough,
			modelAliasCount:                    aliasCfg.Count(),
			modelAliasFingerprint:              aliasCfg.Fingerprint,
			modelAliasForward:                  copyStringMap(aliasCfg.Forward),
			upstreamHeaderCount:                len(upstreamHeaderCfg.Headers),
			upstreamHeaderSource:               upstreamHeaderCfg.Source,
			upstreamHeaderFingerprint:          upstreamHeaderCfg.Fingerprint,
			failPolicy:                         req.FailPolicy,
		},
	}, nil
}

// stripUserinfoString returns s with any URL userinfo (user[:password]@)
// removed. On parse failure or when u.User == nil, returns s unchanged.
// Duplicated locally (verbatim contract with
// preflight.stripUserinfo in safeview.go) to avoid leaking a
// sanitization API from the preflight package; the two implementations MUST
// stay byte-identical. Edge cases (empty string, non-URL strings like
// "direct", scheme-less URLs, URLs with @ in path/query/fragment, malformed
// URLs) are covered by preflight/safeview_test.go.
func stripUserinfoString(s string) string {
	if s == "" {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	return u.String()
}

// SetCaptureBodies atomically flips the per-session request-body capture flag.
// Replaces the current posture with one whose live half has captureBodies
// flipped (withCaptureBodies — same resolved, nothing enumerated, so no
// identity field like missingAuthEnv can be forgotten) and atomic-Stores it
// under sess.mu, matching the single-critical-section invariant the publish
// paths follow so no observer ever sees a split (active=B, public.CaptureBodies
// =A) pair.
//
// Past bodies remain inspectable: existing RequestRecord.BodyRef entries in the
// 250-ring still resolve from the bodyStore (which stays alive for the session),
// and the 256 MB LRU budget continues to govern eviction. Toggling off only
// gates NEW captures; toggling back on starts capturing the next request.
//
// Idempotent on no-op (same value): no Store, no public mutation, no event;
// returns the unchanged snapshot. Otherwise emits session_updated so the
// inspect ribbon's Bodies cell flips without a page reload. Reused by the
// browser POST /capture/bodies endpoint (CSRF-guarded by the profile token).
func (s *Supervisor) SetCaptureBodies(id string, enable bool) (model.Session, error) {
	sess := s.getSession(id)
	if sess == nil {
		return model.Session{}, fmt.Errorf("session %s not found", id)
	}
	changed := false
	sess.mu.Lock()
	cur := sess.active.Load()
	if cur.l.captureBodies != enable {
		np := cur.withCaptureBodies(enable)
		sess.active.Store(&np)
		sess.public.CaptureBodies = enable
		sess.public.UpdatedAt = time.Now()
		changed = true
	}
	sess.mu.Unlock()
	if changed {
		s.touch()
		s.broadcast("session_updated", id, sess.snapshot())
	}
	return sess.snapshot(), nil
}

// SetCaptureTelemetry atomically flips the per-session telemetry-capture flag
// (the opt-in transparent telemetry MITM). Mirrors SetCaptureBodies via
// withCaptureTelemetry: replace the posture with one whose live half has the
// telemetry toggle flipped, atomic-Store under sess.mu (one critical section,
// so no observer sees a split active/public pair). Idempotent on no-op. Reused
// by the browser POST /capture/telemetry endpoint (CSRF-guarded by the profile
// token).
func (s *Supervisor) SetCaptureTelemetry(id string, enable bool) (model.Session, error) {
	sess := s.getSession(id)
	if sess == nil {
		return model.Session{}, fmt.Errorf("session %s not found", id)
	}
	changed := false
	sess.mu.Lock()
	cur := sess.active.Load()
	if cur.l.captureTelemetry != enable {
		np := cur.withCaptureTelemetry(enable)
		sess.active.Store(&np)
		sess.public.CaptureTelemetry = enable
		sess.public.UpdatedAt = time.Now()
		changed = true
	}
	sess.mu.Unlock()
	if changed {
		s.touch()
		s.broadcast("session_updated", id, sess.snapshot())
	}
	return sess.snapshot(), nil
}

func (s *Supervisor) attachSession(id string, req model.SessionAttachRequest) error {
	sess := s.getSession(id)
	if sess == nil {
		return fmt.Errorf("session %s not found", id)
	}
	now := time.Now()
	sess.mu.Lock()
	sess.public.ClaudePID = req.ClaudePID
	sess.public.ClaudeStartToken = req.ClaudeStartToken
	sess.public.State = model.StateAttached
	sess.public.UpdatedAt = now
	sess.mu.Unlock()
	s.touch()
	s.broadcast("session_attached", id, sess.snapshot())
	return nil
}

func (s *Supervisor) closeSession(id, reason string) error {
	sess := s.getSession(id)
	if sess == nil {
		return fmt.Errorf("session %s not found", id)
	}
	sess.mu.Lock()
	firstClose := sess.public.State != model.StateEnded
	if firstClose {
		now := time.Now()
		sess.public.State = model.StateEnded
		sess.public.EndedAt = &now
		sess.public.UpdatedAt = now
		sess.appendTraceLocked(model.TraceRecord{Timestamp: now, SessionID: id, Category: "session", Summary: "session closed", Detail: reason})
	}
	proxy := sess.proxy
	sess.mu.Unlock()
	if proxy != nil {
		_ = proxy.Close()
	}
	if firstClose && sess.bodies != nil {
		// Session-end: drop the whole per-session bodies/ dir.
		// removeAll waits for in-flight async writers then RemoveAll;
		// done after proxy teardown and outside sess.mu (consistent with
		// the proxy.Close() cleanup above). Gated on firstClose to avoid
		// a redundant wg.Wait()+RemoveAll on idempotent re-close;
		// removeAll is itself idempotent (os.RemoveAll is nil on missing)
		// so this is defense-in-depth, not a correctness requirement.
		sess.bodies.removeAll()
	}
	s.touch()
	if firstClose {
		// Idempotent: a duplicate close (e.g. supervisor monitor +
		// launcher Wait both racing on a freshly-exited Claude) does
		// not re-broadcast session_closed. Subscribers that already
		// observed the terminal event don't see a phantom second one.
		s.broadcast("session_closed", id, sess.snapshot())
	}
	return nil
}

func (s *Supervisor) markSessionActive(id string) {
	sess := s.getSession(id)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	if sess.public.State == model.StateAttached ||
		sess.public.State == model.StateLaunching {
		sess.public.State = model.StateActive
		sess.public.UpdatedAt = time.Now()
	}
	sess.mu.Unlock()
	s.touch()
}

func (s *Supervisor) recordRequest(id string, record model.RequestRecord) {
	sess := s.getSession(id)
	if sess == nil {
		return
	}
	// Mask credential request headers BEFORE the record enters the ring — the
	// single store-side convergence point for all eight construction sites. The
	// raw value therefore never reaches /recent, the page bootstrap, the SSE
	// stream, a HAR export, or any future emit site: masking is
	// correct-by-construction here, not a property each consumer must remember.
	// Render-time redaction (ui.RenderHeaderGroupsWithRedaction + the web.go JS
	// twin) is now defense in depth on top of this. Bypassed only under the
	// explicit CCWRAP_UNMASK_CREDENTIALS=1 launch opt-in — the same flag that
	// already gates body redaction — so the wire stays consistent across header
	// and body, and the ribbon's persistent UNMASKED marker covers both.
	if !s.unmaskCredentials && len(record.RequestHeaders) > 0 {
		record.RequestHeaders = ui.MaskCredentialHeaders(record.RequestHeaders)
	}
	sess.mu.Lock()
	// Ring-eviction hook: appendCapped is a generic trimmer that
	// silently drops the oldest element(s) without telling us which —
	// detect the about-to-be-evicted record BEFORE the append (when at
	// cap, the next append drops sess.requests[0], the oldest). Capture a
	// copy under the lock; do the (async, own-locked) body-file delete
	// AFTER unlocking so we never hold sess.mu across bodyStore.delete.
	var evicted model.RequestRecord
	hasEvicted := len(sess.requests) >= maxSessionRequests
	if hasEvicted {
		evicted = sess.requests[0]
	}
	store := sess.bodies
	sess.requests = appendCapped(sess.requests, record, maxSessionRequests)
	sess.public.RecentRequestCount = len(sess.requests)
	// A successful request heals Health to ok — EXCEPT while native-TLS is
	// blocked. While blocked, new dials fail-closed; a request that completes over
	// a still-pooled good conn must NOT heal away the error before a mirror dial
	// resumes, or the user never sees that new connections are being blocked.
	// nativeTLSBlocked (set under this same sess.mu by recordNativeTLS) holds the
	// error until a mirror dial resumes.
	if sess.public.State != model.StateEnded && !sess.nativeTLSBlocked {
		sess.public.Health = model.HealthOK
	}
	sess.public.UpdatedAt = time.Now()
	sess.mu.Unlock()
	if hasEvicted {
		evictBodyFiles(store, evicted)
	}
	s.touch()
	s.broadcast("request", id, record)
}

// evictBodyFiles reclaims the spilled body file(s) backing a ring-evicted
// record. A captured record can spill THREE files: BodyRef (the client-view
// body), UpstreamBodyRef (the post-modelalias-rewrite body, present only when
// the rewrite mutated the body), and ResponseBodyRef (the captured upstream
// RESPONSE body, present only on the telemetry-MITM path). All must be deleted
// on eviction or the spilled file orphans on disk for the rest of the session.
// bodyStore.delete is async and own-locked; the caller must NOT
// hold sess.mu.
func evictBodyFiles(store *bodyStore, evicted model.RequestRecord) {
	if store == nil {
		return
	}
	if evicted.BodyRef != nil {
		store.delete(evicted.BodyRef.ID)
	}
	if evicted.UpstreamBodyRef != nil {
		store.delete(evicted.UpstreamBodyRef.ID)
	}
	if evicted.ResponseBodyRef != nil {
		store.delete(evicted.ResponseBodyRef.ID)
	}
}

func (s *Supervisor) recordError(id string, record model.ErrorRecord) {
	sess := s.getSession(id)
	if sess == nil {
		return
	}
	sev, health := severityForClass(record.ErrorClass)
	if record.Severity == "" {
		record.Severity = sev
	}
	sess.mu.Lock()
	sess.errors = appendCapped(sess.errors, record, maxSessionErrors)
	sess.public.RecentErrorCount = len(sess.errors)
	// Health reflects the most-recent activity result; lifecycle State is
	// untouched (the two dimensions are orthogonal). Ended is terminal.
	if sess.public.State != model.StateEnded {
		sess.public.Health = health
	}
	sess.public.UpdatedAt = time.Now()
	sess.mu.Unlock()
	s.touch()
	s.broadcast("proxy_error", id, record)
}

func (s *Supervisor) recordTrace(id string, record model.TraceRecord) {
	sess := s.getSession(id)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	sess.appendTraceLocked(record)
	sess.public.UpdatedAt = time.Now()
	sess.mu.Unlock()
	s.touch()
	s.broadcast("trace", id, record)
}

// sanitizeProfileCatalogError reduces a profiles.Load error to a fixed,
// non-leaking message (mirrors sanitizeSwitchError's principle: no path,
// no parse-state details, no environment crumbs). The catalog endpoint
// returns the result as ProfileCatalogResponse.LoadError; the UI renders
// it in a danger block.
func sanitizeProfileCatalogError(err error) string {
	if err == nil {
		return ""
	}
	// Multi-error validation report — manually build multi-line
	// output from items so the source path (which may be a full
	// filesystem path) never reaches the wire.
	var perr *profiles.ParseErrors
	if errors.As(err, &perr) && len(perr.Items) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "profiles.json invalid: %d errors", len(perr.Items))
		for _, it := range perr.Items {
			fmt.Fprintf(&b, "\n  - %s", it.Error())
		}
		return b.String()
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "parse profiles "):
		return "profiles.json malformed"
	case strings.HasPrefix(msg, "read profiles file "):
		return "profiles.json unreadable"
	default:
		return "profiles.json error"
	}
}

// envBaseURLHostFor returns the host[:port] of ANTHROPIC_BASE_URL from
// the supervisor's snapshotted parent env (LaunchContext.Options.ParentEnv).
// Empty when launchCtx is nil, the env var is unset, the value does not
// parse as a URL, or the parsed URL has no Host. Surfaced via /profile/catalog
// so the popover's inherit-env row can show what env-live mode would route
// to and the user can detect drift between the active profile's frozen
// BaseURL and the live env. Never includes credentials (ANTHROPIC_BASE_URL
// is a URL, not a secret); empty-on-malformed is defensive — the env value
// is user-provided and may be any string.
func envBaseURLHostFor(lc *LaunchContext) string {
	if lc == nil {
		return ""
	}
	raw := strings.TrimSpace(lookupEnv(lc.Options.ParentEnv, "ANTHROPIC_BASE_URL"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// envHasCredentialsFor reports whether the launch-time parent env
// has either ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN set (non-empty
// after trim). Used to gate the popover's inherit-env [test] button:
// in OAuth-mode sessions both vars are empty (OAuth tokens live in
// the keychain), so a probe would always fail with "credentials
// missing".
func envHasCredentialsFor(lc *LaunchContext) bool {
	if lc == nil {
		return false
	}
	if strings.TrimSpace(lookupEnv(lc.Options.ParentEnv, "ANTHROPIC_API_KEY")) != "" {
		return true
	}
	if strings.TrimSpace(lookupEnv(lc.Options.ParentEnv, "ANTHROPIC_AUTH_TOKEN")) != "" {
		return true
	}
	return false
}

// profileCatalogFor assembles the browser-facing catalog response for one
// session. Disk-read on every call (mirrors `ccwrap profile ls` semantics —
// fresh fetch). Items[] sorted by (Provider, Name) for
// stable UI grouping. Items is empty slice (not nil) when no profiles or
// no file — preserves "Array.isArray length" semantics in browser JS.
func (s *Supervisor) profileCatalogFor(sess *sessionState) *control.ProfileCatalogResponse {
	path := profiles.DefaultPath(s.paths.StateDir)
	resp := &control.ProfileCatalogResponse{
		Source:            path,
		Items:             []control.SafeCatalogItem{},
		EnvBaseURLHost:    envBaseURLHostFor(s.launchCtx),
		EnvHasCredentials: envHasCredentialsFor(s.launchCtx),
	}
	if sess == nil {
		return resp
	}
	file, err := profiles.Load(path)
	if err != nil {
		resp.LoadError = sanitizeProfileCatalogError(err)
		// Active profile still surfaces so the UI can render the chip even
		// when catalog load failed.
		sess.mu.RLock()
		resp.ActiveProfile = sess.public.ActiveProfileName
		sess.mu.RUnlock()
		return resp
	}
	if file == nil {
		sess.mu.RLock()
		resp.ActiveProfile = sess.public.ActiveProfileName
		sess.mu.RUnlock()
		return resp
	}
	resp.HasProfilesFile = true
	resp.Default = file.Default
	items := make([]control.SafeCatalogItem, 0, len(file.Profiles))
	for name, p := range file.Profiles {
		p.Name = name
		items = append(items, toControlSafeCatalogItem(p.SafeView()))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].Name < items[j].Name
	})
	resp.Items = items
	sess.mu.RLock()
	resp.ActiveProfile = sess.public.ActiveProfileName
	sess.mu.RUnlock()
	return resp
}

func (s *Supervisor) statusSnapshot() model.StatusResponse {
	s.mu.RLock()
	sess := s.session
	s.mu.RUnlock()
	resp := model.StatusResponse{
		SchemaVersion: model.ControlAPIVersion,
		Health:        "ok",
		StartedAt:     s.startedAt,
		Timestamp:     time.Now(),
		RuntimeDir:    s.paths.RuntimeDir,
		StateDir:      s.paths.StateDir,
		SocketPath:    s.paths.SocketPath,
	}
	if sess != nil {
		sess.mu.RLock()
		resp.SessionID = sess.public.ID
		resp.ProxyListenAddr = sess.public.ProxyListenAddr
		resp.RecentErrorCount = sess.public.RecentErrorCount
		if sess.public.Health != "" {
			resp.Health = string(sess.public.Health)
		}
		sess.mu.RUnlock()
	}
	return resp
}

func (s *Supervisor) listSessions() []model.Session {
	s.mu.RLock()
	sess := s.session
	s.mu.RUnlock()
	if sess == nil {
		return []model.Session{}
	}
	return []model.Session{sess.snapshot()}
}

func (s *Supervisor) listRequests(sessionID string) []model.RequestRecord {
	sess := s.activeSessionForID(sessionID)
	if sess == nil {
		return []model.RequestRecord{}
	}
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	if len(sess.requests) == 0 {
		return []model.RequestRecord{}
	}
	return append([]model.RequestRecord(nil), sess.requests...)
}

func (s *Supervisor) listErrors(sessionID string) []model.ErrorRecord {
	sess := s.activeSessionForID(sessionID)
	if sess == nil {
		return []model.ErrorRecord{}
	}
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	if len(sess.errors) == 0 {
		return []model.ErrorRecord{}
	}
	return append([]model.ErrorRecord(nil), sess.errors...)
}

func (s *Supervisor) listTrace(sessionID string) []model.TraceRecord {
	sess := s.activeSessionForID(sessionID)
	if sess == nil {
		return []model.TraceRecord{}
	}
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	if len(sess.trace) == 0 {
		return []model.TraceRecord{}
	}
	return append([]model.TraceRecord(nil), sess.trace...)
}

// activeSessionForID returns s.session when the requested id is empty
// (caller wants "all sessions" — there's only ever one) or matches the
// installed session. Any other id resolves to nil so the HTTP handlers
// can emit empty-list responses for stale ids without lying about
// having data.
func (s *Supervisor) activeSessionForID(sessionID string) *sessionState {
	s.mu.RLock()
	sess := s.session
	s.mu.RUnlock()
	if sess == nil {
		return nil
	}
	if sessionID != "" && sess.public.ID != sessionID {
		return nil
	}
	return sess
}

func (s *Supervisor) getSession(id string) *sessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.session == nil || s.session.public.ID != id {
		return nil
	}
	return s.session
}

func (s *Supervisor) getSessionPublic(id string) *model.Session {
	sess := s.getSession(id)
	if sess == nil {
		return nil
	}
	snap := sess.snapshot()
	return &snap
}

func (s *Supervisor) subscribe() (<-chan model.Event, func()) {
	ch := make(chan model.Event, 128)
	s.mu.Lock()
	id := s.nextSubID
	s.nextSubID++
	s.subscribers[id] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if c, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(c)
		}
		s.mu.Unlock()
	}
}

func (s *Supervisor) broadcast(typ, sessionID string, data interface{}) {
	var stuck []int
	s.mu.Lock()
	s.nextEventSeq++
	ev := model.Event{ID: fmt.Sprintf("%d", s.nextEventSeq), Type: typ, Time: time.Now(), SessionID: sessionID, Data: data}
	for id, ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
			stuck = append(stuck, id)
		}
	}
	for _, id := range stuck {
		if c, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(c)
		}
	}
	s.mu.Unlock()
}

func (s *Supervisor) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// recoverGoroutine is the panic backstop for the supervisor's bare
// background goroutines. The supervisor runs IN the launcher process
// (StartSupervisor spawns it as a goroutine), so an unrecovered panic in
// any of these would terminate the whole runtime — taking down the proxy
// AND orphaning the Claude child, the most ironic failure for a diagnostic
// tool. A localized panic here is far better logged than fatal: the
// session degrades, the user sees a stack, ccwrap stays up. net/http
// already recovers per-request panics in the HTTP handlers, so this is for
// the goroutines OUTSIDE that protection (lifecycle monitor, idle reaper,
// hijacked-tunnel copy loops). Defer it at the top of each such goroutine.
func recoverGoroutine(where string) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: recovered panic in %s: %v\n%s\n", where, r, debug.Stack())
	}
}

func (s *Supervisor) monitorSessions(ctx context.Context) {
	defer recoverGoroutine("supervisor.monitorSessions")
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.updateSessionLifecycle()
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		}
	}
}

func (s *Supervisor) updateSessionLifecycle() {
	sessions := s.listSessions()
	for _, public := range sessions {
		if public.ClaudePID <= 0 || public.State == model.StateEnded {
			continue
		}
		gone := false
		if public.ClaudeStartToken != "" {
			exists, match, err := procmetaMatches(public.ClaudePID, public.ClaudeStartToken)
			switch {
			case err == nil:
				gone = !exists || !match
			case !processExists(public.ClaudePID):
				gone = true
			}
		} else if !processExists(public.ClaudePID) {
			gone = true
		}
		if gone {
			_ = s.closeSession(public.ID, "claude process exited")
		}
	}
}

func (s *Supervisor) reapIdle(ctx context.Context) {
	defer recoverGoroutine("supervisor.reapIdle")
	if s.idleTimeout <= 0 {
		return
	}
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.RLock()
			last := s.lastActive
			sess := s.session
			s.mu.RUnlock()
			active := 0
			if sess != nil {
				sess.mu.RLock()
				if sess.public.State != model.StateEnded {
					active = 1
				}
				sess.mu.RUnlock()
			}
			if active == 0 && time.Since(last) > s.idleTimeout {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.Shutdown(shutdownCtx)
				cancel()
				return
			}
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		}
	}
}

func (ss *sessionState) snapshot() model.Session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	copySession := ss.public
	return copySession
}

// currentDialStateLocked snapshots the dial-path-written native-TLS display
// fields off sess.public into a dialState for deriveInto. recordNativeTLS
// (nativetls.go) remains the sole writer of these fields on the dial hot path;
// a publish reads them here so deriveInto re-applies a dial-written status
// idempotently (dial-written wins over the launch-toggle seed). Caller MUST
// hold sess.mu (the publish critical section also covers the Store + derive).
func (ss *sessionState) currentDialStateLocked() dialState {
	return dialState{nativeTLS: ss.public.NativeTLS, nativeTLSFallbacks: ss.public.NativeTLSFallbacks, nativeTLSLoaded: ss.nativeTLSHelloLoaded}
}

func (ss *sessionState) appendTraceLocked(record model.TraceRecord) {
	ss.trace = appendCapped(ss.trace, record, maxSessionTrace)
}

// matchProfileToken returns true iff candidate equals ss.profileToken under
// constant-time compare (defense against timing-based token
// disclosure). Empty candidate is rejected (subtle.ConstantTimeCompare
// returns 0 for length mismatch, but the explicit guard makes intent
// obvious and survives a future refactor).
func (ss *sessionState) matchProfileToken(candidate string) bool {
	if candidate == "" || ss.profileToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(ss.profileToken)) == 1
}

func appendCapped[T any](in []T, item T, limit int) []T {
	in = append(in, item)
	if len(in) <= limit {
		return in
	}
	over := len(in) - limit
	copy(in, in[over:])
	return in[:limit]
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// newBodyID returns a unique id for a spilled request body file. It
// reuses newID()'s crypto-random scheme (collision-free under
// concurrency, same generator session ids use); the RequestRecord
// literals at the capture sites assign no record id, so the body id is
// generated fresh and stored on RequestBodyRef.ID.
func newBodyID() string { return newID() }

// copyStringMap returns nil for nil input, else a defensive shallow
// copy. Used when publishing maps onto the Session wire snapshot so
// supervisor-internal state can't be mutated by readers.
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
