package uiservice

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// SessionSummary is one session in the sidebar.
type SessionSummary struct {
	ID         string    `json:"id"`
	ProjectDir string    `json:"project_dir,omitempty"`
	Profile    string    `json:"profile"`
	State      string    `json:"state"`
	Health     string    `json:"health"`
	StartedAt  time.Time `json:"started_at"`
	// API is "v1", or "legacy" for a session from a ccwrap without /api/v1:
	// it shows only what its manifest says, with a link to its own page.
	API       string `json:"api"`
	LegacyURL string `json:"legacy_url,omitempty"`
	Display   struct {
		Title    string `json:"title"`
		Subtitle string `json:"subtitle"`
		Badge    string `json:"badge,omitempty"`
	} `json:"display"`
	socket string
}

// liveSession is a discovered session the service can reach.
type liveSession struct {
	manifest model.SessionManifest
}

func (s *Server) liveSessions() []liveSession {
	found, _ := discovery.Scan(s.opts.Paths)
	var out []liveSession
	for _, ds := range found {
		if ds.Reachable && !ds.Stale {
			out = append(out, liveSession{manifest: ds.Manifest})
		}
	}
	return out
}

// summarize asks the session for its state; a session without /api/v1 is
// described from its manifest.
func (s *Server) summarize(ctx context.Context, ls liveSession) SessionSummary {
	m := ls.manifest
	sum := SessionSummary{ID: m.SessionID, ProjectDir: m.ProjectDir, State: string(m.State), StartedAt: m.CreatedAt, socket: m.ControlSocket, LegacyURL: "http://" + m.ProxyListenAddr + "/"}
	var sess struct {
		model.Session
		Display struct {
			HeroSentence string `json:"hero_sentence"`
		} `json:"display"`
	}
	if err := getJSON(ctx, m.ControlSocket, "/api/v1/session", &sess); err == nil {
		sum.API = "v1"
		sum.ProjectDir = sess.ProjectDir
		sum.Profile = sess.ActiveProfileName
		sum.State = string(sess.State)
		sum.Health = string(sess.Health)
		sum.StartedAt = sess.CreatedAt
		switch {
		case sess.RecentFailedCount > 0:
			sum.Display.Badge = plural(sess.RecentFailedCount, "failed request", "failed requests")
		case sess.RecentErrorCount > 0:
			sum.Display.Badge = plural(sess.RecentErrorCount, "issue", "issues")
		}
	} else {
		sum.API = "legacy"
	}
	switch {
	case sum.ProjectDir != "":
		sum.Display.Title = filepath.Base(sum.ProjectDir)
	case m.Name != "":
		sum.Display.Title = m.Name
	default:
		sum.Display.Title = ui.ShortID(m.SessionID)
	}
	profile := sum.Profile
	if profile == "" {
		profile = "no profile"
	}
	sum.Display.Subtitle = profile
	if up := ui.Uptime(sum.StartedAt); up != "" {
		sum.Display.Subtitle += " · " + up
	}
	return sum
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (s *Server) sessionSummaries(ctx context.Context) []SessionSummary {
	live := s.liveSessions()
	out := make([]SessionSummary, len(live))
	var wg sync.WaitGroup
	for i, ls := range live {
		wg.Add(1)
		go func(i int, ls liveSession) {
			defer wg.Done()
			out[i] = s.summarize(ctx, ls)
		}(i, ls)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"sessions": s.sessionSummaries(r.Context())})
}

// handleSessionAPI forwards /api/v1/sessions/{id}/<rest> to that session's
// /api/v1/<rest>, for discovered sessions only.
func (s *Server) handleSessionAPI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var socket string
	for _, ls := range s.liveSessions() {
		if ls.manifest.SessionID == id {
			socket = ls.manifest.ControlSocket
		}
	}
	if socket == "" {
		writeAPIError(w, http.StatusNotFound, "no_session", "no running session "+id)
		return
	}
	rest := r.PathValue("rest")
	proxy := &httputil.ReverseProxy{
		Transport:     control.UnixTransport(socket),
		FlushInterval: -1,
		ErrorLog:      s.opts.Logger,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: "unix", Path: "/api/v1/" + rest, RawQuery: r.URL.RawQuery}
			pr.Out.Host = "unix"
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-CCWRAP-Token")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeAPIError(w, http.StatusBadGateway, "session_unreachable", err.Error())
		},
	}
	if strings.HasSuffix(rest, "events") {
		s.clients.Add(1)
		defer s.clients.Add(-1)
	}
	proxy.ServeHTTP(w, r)
}

// hub follows every session's event stream and fans the events out to the
// pages, with sessions.changed when the set of sessions changes.
type hub struct {
	s        *Server
	mu       sync.Mutex
	clients  map[chan hubEvent]string // channel → session filter ("" = all)
	known    map[string]bool          // sessions announced to pages
	follows  map[string]*follower     // event streams being relayed
	sessions []SessionSummary
}

type follower struct{ cancel context.CancelFunc }

type hubEvent struct {
	typ       string
	sessionID string
	data      json.RawMessage
}

func newHub(s *Server) *hub {
	return &hub{s: s, clients: map[chan hubEvent]string{}, known: map[string]bool{}, follows: map[string]*follower{}}
}

func (h *hub) sessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.known)
}

func (h *hub) run(ctx context.Context) {
	t := time.NewTicker(h.s.opts.PollInterval)
	defer t.Stop()
	for {
		h.refresh(ctx)
		select {
		case <-ctx.Done():
			h.mu.Lock()
			for _, f := range h.follows {
				f.cancel()
			}
			h.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// refresh rediscovers sessions, follows those not followed (new ones, and
// ones whose stream dropped), and announces changes to the set.
func (h *hub) refresh(ctx context.Context) {
	sums := h.s.sessionSummaries(ctx)
	live := map[string]bool{}
	h.mu.Lock()
	changed := false
	for _, sum := range sums {
		live[sum.ID] = true
		if !h.known[sum.ID] {
			h.known[sum.ID] = true
			changed = true
		}
		if sum.API != "v1" || h.follows[sum.ID] != nil {
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		f := &follower{cancel: cancel}
		h.follows[sum.ID] = f
		go h.follow(fctx, sum, f)
	}
	for id := range h.known {
		if !live[id] {
			delete(h.known, id)
			if f := h.follows[id]; f != nil {
				f.cancel()
				delete(h.follows, id)
			}
			changed = true
		}
	}
	h.sessions = sums
	h.mu.Unlock()
	if changed {
		data, _ := json.Marshal(map[string]any{"sessions": sums})
		h.publish(hubEvent{typ: "sessions.changed", data: data})
	}
}

// follow relays one session's /api/v1/events until it ends, then lets the
// next refresh follow it again if the session is still running.
func (h *hub) follow(ctx context.Context, sum SessionSummary, f *follower) {
	defer func() {
		h.mu.Lock()
		if h.follows[sum.ID] == f {
			delete(h.follows, sum.ID)
		}
		h.mu.Unlock()
		// A stream that ends on its own leaves its context registered under
		// the hub's until cancelled.
		f.cancel()
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/api/v1/events", nil)
	resp, err := (&http.Client{Transport: control.UnixTransport(sum.socket)}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	typ := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && typ != "" && typ != "hello":
			h.publish(hubEvent{typ: typ, sessionID: sum.ID, data: json.RawMessage(strings.TrimPrefix(line, "data: "))})
		case line == "":
			typ = ""
		}
	}
}

func (h *hub) publish(ev hubEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, filter := range h.clients {
		if filter != "" && ev.sessionID != "" && ev.sessionID != filter {
			continue
		}
		select {
		case ch <- ev:
		default: // a slow page drops events; it resyncs from the API
		}
	}
}

// handleEvents streams merged events. The first, hello, names this service
// instance so a page can tell when it reconnected to a new one.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		writeAPIError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	s.clients.Add(1)
	defer s.clients.Add(-1)
	ch := make(chan hubEvent, 256)
	s.hub.mu.Lock()
	s.hub.clients[ch] = r.URL.Query().Get("session")
	sums := s.hub.sessions
	s.hub.mu.Unlock()
	defer func() {
		s.hub.mu.Lock()
		delete(s.hub.clients, ch)
		s.hub.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sw := newSSEWriter(w)
	defer sw.done()
	hello, _ := json.Marshal(map[string]any{"instance_id": s.info.InstanceID, "version": s.info.Version, "sessions": sums})
	if !sw.send(func(w io.Writer) {
		_, _ = io.WriteString(w, "retry: 1500\n\n")
		writeEvent(w, "hello", hello)
	}) {
		return
	}
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case ev := <-ch:
			if !sw.send(func(w io.Writer) { writeEvent(w, ev.typ, ev.data) }) {
				return
			}
		case <-keepAlive.C:
			if !sw.send(func(w io.Writer) { _, _ = io.WriteString(w, ": ping\n\n") }) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		}
	}
}

func writeEvent(w io.Writer, typ string, data []byte) {
	_, _ = io.WriteString(w, "event: "+typ+"\ndata: ")
	_, _ = w.Write(data)
	_, _ = io.WriteString(w, "\n\n")
}
