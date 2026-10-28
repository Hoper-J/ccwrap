package uiservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profileapi"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// The Profiles page's API. Edits go through profileapi, the same code a
// session's own page uses; an edit to a profile sessions are running on is
// applied to them right away.

// maxProfileBody bounds a profile request body.
const maxProfileBody = 1 << 20

// profileView is a profile as the Profiles page shows it: its safe view (no
// key, no key_env name), whether it is the default, the running sessions on
// it, and its last test in this service's lifetime.
type profileView struct {
	profiles.SafeCatalogItem
	IsDefault bool      `json:"is_default"`
	Sessions  []string  `json:"sessions"`
	LastTest  *lastTest `json:"last_test,omitempty"`
}

// lastTest is a profile's latest connection test. Kept in memory only: a
// restarted service shows profiles as not tested.
type lastTest struct {
	At time.Time `json:"at"`
	// Status is the probe's status (OK, AUTH_FAIL, TIMEOUT, ...) and Text
	// the line to show: "OK · 230 ms".
	Status string          `json:"status"`
	Text   string          `json:"text"`
	Result json.RawMessage `json:"result"`
}

// testRecord holds the last tests by profile name.
type testRecord struct {
	mu    sync.Mutex
	tests map[string]lastTest
}

func (r *testRecord) get(name string) *lastTest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tests[name]; ok {
		return &t
	}
	return nil
}

func (r *testRecord) set(name string, t lastTest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tests == nil {
		r.tests = map[string]lastTest{}
	}
	r.tests[name] = t
}

// forget drops a profile's test: after an edit it describes a profile that
// is gone.
func (r *testRecord) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tests, name)
}

// testText is the line a probe result shows as: its status, the HTTP status
// when it failed with one, and the time it took.
func testText(res profiletest.ProbeResult) string {
	parts := []string{res.Status.String()}
	if res.Status.IsFailure() && res.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprint(res.HTTPStatus))
	}
	if res.Latency > 0 && res.Status != profiletest.StatusSkipped {
		parts = append(parts, fmt.Sprintf("%d ms", res.Latency.Milliseconds()))
	}
	return strings.Join(parts, " · ")
}

// sessionsOn is the running sessions on each profile, from the session list.
func (s *Server) sessionsOn() map[string][]string {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	out := map[string][]string{}
	for _, sum := range s.hub.sessions {
		if sum.State != string(model.StateEnded) && sum.Profile != "" {
			out[sum.Profile] = append(out[sum.Profile], sum.ID)
		}
	}
	return out
}

func (s *Server) view(p profiles.Profile, def string, on map[string][]string) profileView {
	sessions := on[p.Name]
	if sessions == nil {
		sessions = []string{}
	}
	return profileView{SafeCatalogItem: p.SafeView(), IsDefault: p.Name == def, Sessions: sessions, LastTest: s.tests.get(p.Name)}
}

func writeStoreError(w http.ResponseWriter, e *profileapi.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	body := map[string]any{"code": e.Kind, "message": e.Message}
	if len(e.Paths) > 0 {
		body["paths"] = e.Paths
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

// decodeBody reads a JSON body of at most maxProfileBody into v; an empty
// body leaves v as it is when optional.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProfileBody))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "usage", fmt.Sprintf("request body exceeds %d bytes", maxProfileBody))
		return false
	}
	if optional && len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	if err := json.Unmarshal(data, v); err != nil {
		writeAPIError(w, http.StatusBadRequest, "usage", "invalid request body: "+err.Error())
		return false
	}
	return true
}

// handleProfiles lists the profiles: safe views (no secrets), the default,
// and where the file is.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	f, err := s.profiles.Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", sanitizeLoadError(err))
		return
	}
	out := map[string]any{"default": "", "path": s.profiles.Path(), "profiles": []profileView{}}
	if f != nil {
		on := s.sessionsOn()
		items := make([]profileView, 0, len(f.Profiles))
		for name, p := range f.Profiles {
			p.Name = name
			items = append(items, s.view(p, f.Default, on))
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		out["default"], out["profiles"] = f.Default, items
	}
	writeJSON(w, out)
}

// sanitizeLoadError words a profiles.json load failure without its path.
func sanitizeLoadError(err error) string {
	var perr *profiles.ParseErrors
	if errors.As(err, &perr) {
		return fmt.Sprintf("profiles.json invalid: %d errors", len(perr.Items))
	}
	if strings.HasPrefix(err.Error(), "parse profiles ") {
		return "profiles.json malformed"
	}
	return "profiles.json unreadable"
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	p, e := s.profiles.Get(r.PathValue("name"))
	if e != nil {
		writeStoreError(w, e)
		return
	}
	f, _ := s.profiles.Load()
	def := ""
	if f != nil {
		def = f.Default
	}
	writeJSON(w, s.view(p, def, s.sessionsOn()))
}

func (s *Server) handleAddProfile(w http.ResponseWriter, r *http.Request) {
	var req profileapi.AddRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	saved, e := s.profiles.Add(req)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"profile": s.view(saved.Profile, saved.Default, nil), "default": saved.Default})
}

// applied is how re-applying an edited profile went for one session.
type applied struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	Message   string `json:"message,omitempty"`
}

// handleEditProfile changes a profile and applies it to the sessions running
// on it.
func (s *Server) handleEditProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req profileapi.EditRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.Name != "" && req.Name != name {
		writeAPIError(w, http.StatusBadRequest, "usage", "a profile cannot be renamed")
		return
	}
	req.Name = name
	saved, e := s.profiles.Edit(req)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	s.tests.forget(name)
	on := s.sessionsOn()
	writeJSON(w, map[string]any{
		"profile": s.view(saved.Profile, saved.Default, on),
		"default": saved.Default,
		"applied": s.applyProfile(r.Context(), name, on[name]),
	})
}

// applyProfile switches each session in ids to profile name again, so it
// picks up the edit.
func (s *Server) applyProfile(ctx context.Context, name string, ids []string) []applied {
	sockets := map[string]string{}
	for _, ls := range s.liveSessions() {
		sockets[ls.manifest.SessionID] = ls.manifest.ControlSocket
	}
	out := []applied{}
	for _, id := range ids {
		a := applied{SessionID: id}
		var outcome struct {
			Result  string `json:"result"`
			Message string `json:"message"`
		}
		if socket := sockets[id]; socket == "" {
			a.Result, a.Message = "unreachable", "the session is no longer running"
		} else if err := postJSON(ctx, socket, "/api/v1/profile", map[string]string{"name": name}, &outcome); err != nil {
			a.Result, a.Message = "unreachable", err.Error()
		} else {
			a.Result, a.Message = outcome.Result, outcome.Message
		}
		out = append(out, a)
	}
	return out
}

func postJSON(ctx context.Context, socket, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: control.UnixTransport(socket), Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// handleRemoveProfile deletes a profile. The body must repeat its name. The
// sessions running on it keep it until they end.
func (s *Server) handleRemoveProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		ConfirmName string `json:"confirm_name"`
	}
	if !decodeBody(w, r, &req, false) {
		return
	}
	sessions := s.sessionsOn()[name]
	if sessions == nil {
		sessions = []string{}
	}
	removed, e := s.profiles.Remove(name, req.ConfirmName)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	s.tests.forget(name)
	writeJSON(w, map[string]any{"removed": name, "default": removed.Default, "remaining": removed.Remaining, "sessions": sessions})
}

func (s *Server) handleSetDefaultProfile(w http.ResponseWriter, r *http.Request) {
	def, e := s.profiles.SetDefault(r.PathValue("name"))
	if e != nil {
		writeStoreError(w, e)
		return
	}
	writeJSON(w, map[string]string{"default": def})
}

// handleTestProfile sends the profile one small model request and records
// the result as its last test.
func (s *Server) handleTestProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, err := s.profiles.ProfileForTest(name, s.opts.Getenv)
	if err != nil {
		status, msg := profileapi.TestErrorStatus(err)
		code := "internal"
		if status == http.StatusNotFound {
			code = "not_found"
		}
		writeAPIError(w, status, code, msg)
		return
	}
	res := probeProfile(p, profiletest.ProbeOptions{Timeout: 15 * time.Second})
	data, err := profiletest.MarshalResult(res)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "marshal result")
		return
	}
	t := lastTest{At: time.Now().UTC(), Status: res.Status.String(), Text: testText(res), Result: data}
	s.tests.set(name, t)
	writeJSON(w, t)
}

// probeProfile and probeEgress run the probes; seams for tests.
var (
	probeProfile = profiletest.Probe
	probeEgress  = profiletest.ProbeEgress
)

// handleTestEgress reports where the profile's traffic exits. The body may
// carry an egress to test instead of the saved one (a form not saved yet).
func (s *Server) handleTestEgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Egress *profiles.EgressSpec `json:"egress"`
	}
	if !decodeBody(w, r, &req, true) {
		return
	}
	p := profiles.Profile{Name: name}
	if name != profiles.InheritEnv {
		saved, e := s.profiles.Get(name)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		p = saved
	}
	if req.Egress != nil {
		if err := profiles.ValidateEgressSpec(*req.Egress); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
			return
		}
		p.Egress = *req.Egress
	}
	writeJSON(w, probeEgress(p, profiletest.EgressProbeOptions{Timeout: 5 * time.Second}))
}
