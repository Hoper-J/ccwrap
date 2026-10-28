package uiservice

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// SessionSummary is one session in the sidebar. The page shows the times as
// they pass: how long it has run, how long ago it ended.
type SessionSummary struct {
	ID             string     `json:"id"`
	ProjectDir     string     `json:"project_dir,omitempty"`
	Profile        string     `json:"profile"`
	State          string     `json:"state"`
	Health         string     `json:"health"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	Display        struct {
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

// summaryResult is how asking a session for its summary went.
type summaryResult int

const (
	summaryOK summaryResult = iota
	// summaryLegacy: a session from a ccwrap without /api/v1. Not listed.
	summaryLegacy
	// summaryFailed: it did not answer this time.
	summaryFailed
)

// summarize asks the session for its state.
func (s *Server) summarize(ctx context.Context, ls liveSession) (SessionSummary, summaryResult) {
	m := ls.manifest
	var sess struct {
		model.Session
		Display struct {
			Badge string `json:"badge"`
		} `json:"display"`
	}
	if err := getJSON(ctx, m.ControlSocket, "/api/v1/session", &sess); errors.Is(err, errNotFound) {
		return SessionSummary{}, summaryLegacy
	} else if err != nil {
		return SessionSummary{}, summaryFailed
	}
	sum := SessionSummary{
		ID: m.SessionID, ProjectDir: sess.ProjectDir, Profile: sess.ActiveProfileName,
		State: string(sess.State), Health: string(sess.Health), StartedAt: sess.CreatedAt,
		EndedAt: sess.EndedAt, LastActivityAt: sess.LastActivityAt, socket: m.ControlSocket,
	}
	if sum.Health == "" {
		sum.Health = string(model.HealthOK)
	}
	switch {
	case sum.ProjectDir != "":
		sum.Display.Title = filepath.Base(sum.ProjectDir)
	case m.Name != "":
		sum.Display.Title = m.Name
	default:
		sum.Display.Title = ui.ShortID(m.SessionID)
	}
	sum.Display.Subtitle = sum.Profile
	if sum.Display.Subtitle == "" {
		sum.Display.Subtitle = "no profile"
	}
	sum.Display.Badge = sess.Display.Badge
	return sum, summaryOK
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"sessions": s.hub.refresh(r.Context())})
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

// hub keeps the session list and follows every session's event stream,
// fanning the events out to the pages, with sessions.changed when the list
// changes.
type hub struct {
	s *Server
	// ctx is the hub's run context, which follows run under; nil before run.
	ctx context.Context
	// refreshMu makes refreshes take turns, so an older one never overwrites
	// a newer list.
	refreshMu sync.Mutex
	mu        sync.Mutex
	clients   map[chan hubEvent]string // channel → session filter ("" = all)
	follows   map[string]*follower     // event streams being relayed
	sessions  []SessionSummary         // the list pages were last told
}

type follower struct{ cancel context.CancelFunc }

type hubEvent struct {
	typ       string
	sessionID string
	data      json.RawMessage
}

func newHub(s *Server) *hub {
	return &hub{s: s, clients: map[chan hubEvent]string{}, follows: map[string]*follower{}}
}

// sessionCount is how many listed sessions are still running.
func (h *hub) sessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, sum := range h.sessions {
		if sum.State != string(model.StateEnded) {
			n++
		}
	}
	return n
}

func (h *hub) run(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
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

// refresh rediscovers sessions and returns the new list: the sessions found,
// by start, and for EndedRetention after they end, the ones that ended or
// went away. A session that does not answer keeps its last summary. It
// follows the sessions it reached that are not followed yet (new ones, and
// ones whose stream dropped), and tells pages when the list changed.
func (h *hub) refresh(ctx context.Context) []SessionSummary {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	live := h.s.liveSessions()
	sums := make([]SessionSummary, len(live))
	results := make([]summaryResult, len(live))
	var wg sync.WaitGroup
	for i, ls := range live {
		wg.Add(1)
		go func(i int, ls liveSession) {
			defer wg.Done()
			sums[i], results[i] = h.s.summarize(ctx, ls)
		}(i, ls)
	}
	wg.Wait()
	now := time.Now()

	h.mu.Lock()
	prev := map[string]SessionSummary{}
	for _, sum := range h.sessions {
		prev[sum.ID] = sum
	}
	next := map[string]SessionSummary{}
	found := map[string]bool{}
	for i, ls := range live {
		id := ls.manifest.SessionID
		found[id] = true
		switch results[i] {
		case summaryOK:
			next[id] = sums[i]
			if h.follows[id] == nil && h.ctx != nil {
				fctx, cancel := context.WithCancel(h.ctx)
				f := &follower{cancel: cancel}
				h.follows[id] = f
				go h.follow(fctx, sums[i], f)
			}
		case summaryFailed:
			if p, ok := prev[id]; ok {
				next[id] = p
			}
		}
	}
	for id, p := range prev {
		if _, ok := next[id]; ok {
			continue
		}
		if p.State != string(model.StateEnded) {
			p.State = string(model.StateEnded)
			p.Display.Badge = ""
		}
		if p.EndedAt == nil {
			t := now
			p.EndedAt = &t
		}
		next[id] = p
	}
	list := make([]SessionSummary, 0, len(next))
	for _, sum := range next {
		if sum.State == string(model.StateEnded) && sum.EndedAt != nil && now.Sub(*sum.EndedAt) >= h.s.opts.EndedRetention {
			continue
		}
		list = append(list, sum)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].StartedAt.Equal(list[j].StartedAt) {
			return list[i].StartedAt.Before(list[j].StartedAt)
		}
		return list[i].ID < list[j].ID
	})
	for id, f := range h.follows {
		if !found[id] {
			f.cancel()
			delete(h.follows, id)
		}
	}
	changed := !sameSessionStates(h.sessions, list)
	h.sessions = list
	h.mu.Unlock()
	if changed {
		data, _ := json.Marshal(map[string]any{"sessions": list})
		h.publish(hubEvent{typ: "sessions.changed", data: data})
	}
	return list
}

// sameSessionStates reports whether two lists hold the same sessions in the
// same states: what a page's list needs sessions.changed for. Health and
// badges change with session.updated.
func sameSessionStates(a, b []SessionSummary) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].State != b[i].State || a[i].Profile != b[i].Profile {
			return false
		}
	}
	return true
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
