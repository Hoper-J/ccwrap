package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// The v1 API serves one session's data and actions on its control socket.
// The socket's file permissions are the guard, so there is no token. The
// ccwrap ui service, the CLI and the terminal dashboard reach a session only
// through it.

const apiVersion = "v1"

// apiError is a v1 error: an HTTP status plus a stable code.
type apiError struct {
	status  int
	code    string
	message string
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": e.code, "message": e.message}})
}

func (s *Supervisor) apiV1() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/meta", s.apiMeta)
	mux.HandleFunc("GET /api/v1/session", s.withAPISession(s.apiSession))
	mux.HandleFunc("GET /api/v1/activity", s.withAPISession(s.apiActivity))
	mux.HandleFunc("GET /api/v1/bodies/{id}", s.withAPISession(s.apiBody))
	mux.HandleFunc("GET /api/v1/events", s.withAPISession(s.apiEvents))
	mux.HandleFunc("POST /api/v1/profile", s.withAPISession(s.apiSwitchProfile))
	mux.HandleFunc("POST /api/v1/capture", s.withAPISession(s.apiCapture))
	mux.HandleFunc("POST /api/v1/model-default", s.withAPISession(s.apiModelDefault))
	mux.HandleFunc("POST /api/v1/egress-test", s.withAPISession(func(w http.ResponseWriter, r *http.Request, sess *sessionState) {
		sess.proxy.writeActiveEgressProbe(w)
	}))
	mux.HandleFunc("GET /api/v1/native-tls", s.withAPISession(func(w http.ResponseWriter, r *http.Request, sess *sessionState) {
		sess.proxy.handleNativeTLSInfo(w, r)
	}))
	mux.HandleFunc("GET /api/v1/native-tls/clienthello.bin", s.withAPISession(func(w http.ResponseWriter, r *http.Request, sess *sessionState) {
		sess.proxy.handleNativeTLSClientHello(w, r)
	}))
	// The catch-all also wins over a method mismatch, so it tells the two
	// apart itself.
	known := map[string]string{
		"/api/v1/meta": "GET", "/api/v1/session": "GET", "/api/v1/activity": "GET", "/api/v1/events": "GET",
		"/api/v1/profile": "POST", "/api/v1/capture": "POST", "/api/v1/model-default": "POST", "/api/v1/egress-test": "POST",
		"/api/v1/native-tls": "GET", "/api/v1/native-tls/clienthello.bin": "GET",
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		method, ok := known[r.URL.Path]
		if !ok && strings.HasPrefix(r.URL.Path, "/api/v1/bodies/") {
			method, ok = "GET", true
		}
		if ok {
			w.Header().Set("Allow", method)
			writeAPIError(w, &apiError{http.StatusMethodNotAllowed, "method_not_allowed", r.URL.Path + " takes " + method})
			return
		}
		writeAPIError(w, &apiError{http.StatusNotFound, "not_found", "no such endpoint: " + r.Method + " " + r.URL.Path})
	})
	return mux
}

// apiSessionState is the session this supervisor serves: a launch runs one.
func (s *Supervisor) apiSessionState() *sessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.session
}

func (s *Supervisor) withAPISession(h func(http.ResponseWriter, *http.Request, *sessionState)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.apiSessionState()
		if sess == nil || sess.proxy == nil {
			writeAPIError(w, &apiError{http.StatusNotFound, "no_session", "this supervisor has no session yet"})
			return
		}
		h(w, r, sess)
	}
}

func (s *Supervisor) apiMeta(w http.ResponseWriter, r *http.Request) {
	meta := map[string]any{
		"api":          apiVersion,
		"capabilities": []string{"session", "activity", "bodies", "events", "profile", "capture", "model-default", "native-tls", "egress-test"},
	}
	if lc := s.launchContext(); lc != nil {
		meta["version"] = lc.Version
	}
	if sess := s.apiSessionState(); sess != nil {
		meta["session_id"] = sess.snapshot().ID
	}
	writeJSON(w, meta)
}

func (s *Supervisor) apiSession(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	writeJSON(w, sessionWithDisplay(sess.snapshot()))
}

// apiActivity returns the session's retained requests, errors and trace,
// newest last. kind narrows it to one of them; limit keeps the newest N.
func (s *Supervisor) apiActivity(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "" && kind != "requests" && kind != "errors" && kind != "trace" {
		writeAPIError(w, &apiError{http.StatusBadRequest, "usage", "kind must be requests, errors or trace"})
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeAPIError(w, &apiError{http.StatusBadRequest, "usage", "limit must be a positive integer"})
			return
		}
		limit = n
	}
	capped := func(max int) int {
		if limit > 0 && limit < max {
			return limit
		}
		return max
	}
	id := sess.snapshot().ID
	out := map[string]any{}
	if kind == "" || kind == "requests" {
		recs := tailRequests(s.listRequests(id), capped(maxSessionRequests))
		classified := make([]classifiedRecord, len(recs))
		for i, rec := range recs {
			classified[i] = classifiedRecord{Class: recordClass(rec), RequestRecord: rec}
		}
		out["requests"] = classified
	}
	if kind == "" || kind == "errors" {
		out["errors"] = tailErrors(s.listErrors(id), capped(maxSessionErrors))
	}
	if kind == "" || kind == "trace" {
		out["trace"] = tailTrace(s.listTrace(id), capped(maxSessionTrace))
	}
	writeJSON(w, out)
}

func (s *Supervisor) apiBody(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	sess.proxy.writeBody(w, r.PathValue("id"))
}

// apiEvents streams the session's events as SSE. The first event, hello,
// names the session, so a client that reconnects to a reused address can
// tell it reached a different one; the stream ends after session.ended.
func (s *Supervisor) apiEvents(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	if _, ok := w.(http.Flusher); !ok {
		writeAPIError(w, &apiError{http.StatusInternalServerError, "internal", "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.subscribe()
	defer unsubscribe()
	id := sess.snapshot().ID
	sw := newSSEWriter(w)
	defer sw.done()
	if !sw.send(func(w io.Writer) {
		_, _ = io.WriteString(w, "retry: 1500\n\n")
		writeSSE(w, "hello", model.Event{Time: time.Now(), SessionID: id, Data: map[string]string{"session_id": id, "api": apiVersion}})
	}) {
		return
	}
	// send writes one event and reports whether the stream is over: the
	// event ended the session, or the client stopped reading.
	send := func(ev model.Event) bool {
		if ev.SessionID != id {
			return false
		}
		typ, ev, ok := v1Event(ev)
		if ok && !sw.send(func(w io.Writer) { writeSSE(w, typ, ev) }) {
			return true
		}
		return typ == "session.ended"
	}
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok || send(ev) {
				return
			}
		case <-keepAlive.C:
			if !sw.send(func(w io.Writer) { _, _ = io.WriteString(w, ": ping\n\n") }) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.closed:
			// The supervisor stops right after the session closes: send
			// what is queued, session.ended among it, before the stream ends.
			for {
				select {
				case ev, ok := <-ch:
					if !ok || send(ev) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// v1Event maps a supervisor event to its v1 name and wire payload.
func v1Event(ev model.Event) (string, model.Event, bool) {
	var typ string
	switch ev.Type {
	case "session_created", "session_attached", "session_updated":
		typ = "session.updated"
	case "session_closed":
		typ = "session.ended"
	case "request":
		typ = "request.added"
	case "proxy_error":
		typ = "error.added"
	case "trace":
		typ = "trace.added"
	default:
		return "", ev, false
	}
	return typ, eventForWire(ev), true
}

func writeSSE(w io.Writer, typ string, ev model.Event) {
	data, _ := json.Marshal(map[string]any{"id": ev.ID, "time": ev.Time, "session_id": ev.SessionID, "data": ev.Data})
	if ev.ID != "" {
		_, _ = fmt.Fprintf(w, "id: %s\n", ev.ID)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
}

// maxAPIBodyBytes caps a v1 request body.
const maxAPIBodyBytes = 5 * 1024 * 1024

func decodeAPIBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
	defer body.Close()
	if err := json.NewDecoder(body).Decode(v); err != nil {
		writeAPIError(w, &apiError{http.StatusBadRequest, "usage", "invalid request body: " + err.Error()})
		return false
	}
	return true
}

func (s *Supervisor) apiSwitchProfile(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeAPIBody(w, r, &req) {
		return
	}
	writeJSON(w, s.SwitchProfile(sess.snapshot().ID, req.Name))
}

// apiCapture turns body capture and telemetry capture on or off; a field
// left out stays as it is.
func (s *Supervisor) apiCapture(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	var req struct {
		Bodies    *bool `json:"bodies"`
		Telemetry *bool `json:"telemetry"`
	}
	if !decodeAPIBody(w, r, &req) {
		return
	}
	if req.Bodies == nil && req.Telemetry == nil {
		writeAPIError(w, &apiError{http.StatusBadRequest, "usage", "set bodies and/or telemetry"})
		return
	}
	id := sess.snapshot().ID
	if req.Bodies != nil {
		if _, err := s.SetCaptureBodies(id, *req.Bodies); err != nil {
			writeAPIError(w, &apiError{http.StatusInternalServerError, "internal", err.Error()})
			return
		}
	}
	if req.Telemetry != nil {
		if _, err := s.SetCaptureTelemetry(id, *req.Telemetry); err != nil {
			writeAPIError(w, &apiError{http.StatusInternalServerError, "internal", err.Error()})
			return
		}
	}
	writeJSON(w, sessionWithDisplay(sess.snapshot()))
}

func (s *Supervisor) apiModelDefault(w http.ResponseWriter, r *http.Request, sess *sessionState) {
	var req struct {
		Target string `json:"target"`
	}
	if !decodeAPIBody(w, r, &req) {
		return
	}
	outcome, e := sess.proxy.applyModelDefault(req.Target)
	if e != nil {
		writeAPIError(w, e)
		return
	}
	writeJSON(w, outcome)
}
