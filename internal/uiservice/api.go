package uiservice

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/projects"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/termlaunch"
)

func (s *Server) apiMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/meta", s.handleMeta)
	mux.HandleFunc("GET /api/v1/sessions", s.handleSessions)
	mux.HandleFunc("/api/v1/sessions/{id}/{rest...}", s.handleSessionAPI)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/projects", s.handleProjects)
	mux.HandleFunc("POST /api/v1/projects/launch", s.handleLaunch)
	mux.HandleFunc("GET /api/v1/settings/terminal", s.handleGetTerminal)
	mux.HandleFunc("PUT /api/v1/settings/terminal", s.handlePutTerminal)
	mux.HandleFunc("GET /api/v1/profiles", s.handleProfiles)
	// The catch-all also wins over a method mismatch, so it tells the two
	// apart itself.
	known := map[string]string{
		"/api/v1/meta": "GET", "/api/v1/sessions": "GET", "/api/v1/events": "GET", "/api/v1/projects": "GET",
		"/api/v1/projects/launch": "POST", "/api/v1/settings/terminal": "GET, PUT", "/api/v1/profiles": "GET",
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if allow, ok := known[r.URL.Path]; ok {
			w.Header().Set("Allow", allow)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.URL.Path+" takes "+allow)
			return
		}
		writeAPIError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return mux
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"api":          "v1",
		"version":      s.info.Version,
		"instance_id":  s.info.InstanceID,
		"capabilities": []string{"sessions", "events", "projects", "launch", "terminal-settings", "profiles-read"},
	})
}

// projectSources wires the project list to Claude Code's files, ccwrap's
// state and the running sessions.
func (s *Server) projectSources() projects.Sources {
	running := map[string][]string{}
	for _, ls := range s.liveSessions() {
		if dir := ls.manifest.ProjectDir; dir != "" {
			running[dir] = append(running[dir], ls.manifest.SessionID)
		}
	}
	return projects.Sources{
		ClaudeConfig: settings.GlobalConfigPath(),
		ClaudeHome:   settings.ClaudeConfigHomeDir(),
		StateDir:     s.opts.Paths.StateDir,
		Running:      running,
	}
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"projects": s.lister.List(s.projectSources())})
}

// claudeDeepLinkTerminal is the terminal Claude Code opens for its own deep
// links, from its global config.
func claudeDeepLinkTerminal() string {
	var cfg struct {
		DeepLinkTerminal string `json:"deepLinkTerminal"`
	}
	if data, err := os.ReadFile(settings.GlobalConfigPath()); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	return cfg.DeepLinkTerminal
}

// appDirs is where terminals are looked for; a seam for tests.
var appDirs = termlaunch.DefaultAppDirs

// launchTerminal opens the script; a seam for tests.
var launchTerminal = termlaunch.Launch

type launchRequest struct {
	Path    string `json:"path"`
	Profile string `json:"profile"`
	Mode    string `json:"mode"`
}

// handleLaunch opens the user's terminal in a known project folder running
// this ccwrap with a known profile. Nothing else can be run: the command is
// fixed and its only arguments are --profile and --continue.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req launchRequest
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "usage", "invalid request body: "+err.Error())
		return
	}
	if req.Mode != "new" && req.Mode != "continue" {
		writeAPIError(w, http.StatusBadRequest, "usage", `mode must be "new" or "continue"`)
		return
	}
	known := false
	for _, p := range s.lister.List(s.projectSources()) {
		if p.Path == req.Path {
			known = true
			break
		}
	}
	if !known {
		writeAPIError(w, http.StatusNotFound, "unknown_project", "not a known project folder: "+req.Path)
		return
	}
	if err := termlaunch.ValidateDir(req.Path); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_folder", err.Error())
		return
	}
	if req.Profile != "" {
		f, err := profiles.Load(profiles.DefaultPath(s.opts.Paths.StateDir))
		if err != nil || f == nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "unknown_profile", "no profiles are configured")
			return
		}
		if _, ok := f.Profiles[req.Profile]; !ok {
			writeAPIError(w, http.StatusUnprocessableEntity, "unknown_profile", "profile "+req.Profile+" no longer exists")
			return
		}
	}
	term, ok := termlaunch.Choose(termlaunch.Installed(appDirs()), s.opts.Paths.StateDir, claudeDeepLinkTerminal())
	if !ok {
		writeAPIError(w, http.StatusConflict, "no_terminal", "no supported terminal app found (Terminal, iTerm, Ghostty on macOS)")
		return
	}
	s.launchMu.Lock()
	if time.Since(s.lastLaunch) < time.Second {
		s.launchMu.Unlock()
		writeAPIError(w, http.StatusTooManyRequests, "too_fast", "one launch per second")
		return
	}
	s.lastLaunch = time.Now()
	s.launchMu.Unlock()

	var args []string
	if req.Profile != "" {
		args = append(args, "--profile", req.Profile)
	}
	if req.Mode == "continue" {
		args = append(args, "--", "--continue")
	}
	if err := launchTerminal(term, termlaunch.Request{Dir: req.Path, Exe: s.opts.Exe, Args: args}, filepath.Join(s.dir, "launch")); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "launch_failed", err.Error())
		return
	}
	writeJSON(w, map[string]string{"terminal": term.Name, "path": req.Path, "profile": req.Profile, "mode": req.Mode})
}

func (s *Server) handleGetTerminal(w http.ResponseWriter, r *http.Request) {
	installed := termlaunch.Installed(appDirs())
	claude := claudeDeepLinkTerminal()
	current, ok := termlaunch.Choose(installed, s.opts.Paths.StateDir, claude)
	out := map[string]any{"installed": installed, "claude_choice": termlaunch.ClaudeChoiceID(claude), "current": nil}
	if ok {
		out["current"] = current.ID
	}
	writeJSON(w, out)
}

func (s *Server) handlePutTerminal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Terminal string `json:"terminal"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "usage", "invalid request body: "+err.Error())
		return
	}
	for _, in := range termlaunch.Installed(appDirs()) {
		if in.ID == req.Terminal {
			if err := termlaunch.SavePreference(s.opts.Paths.StateDir, req.Terminal); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			s.handleGetTerminal(w, r)
			return
		}
	}
	writeAPIError(w, http.StatusUnprocessableEntity, "unknown_terminal", "not an installed terminal: "+req.Terminal)
}

// handleProfiles lists profiles in their safe view (no secrets), for the
// Projects page's profile picker.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	f, err := profiles.Load(profiles.DefaultPath(s.opts.Paths.StateDir))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := map[string]any{"default": "", "profiles": []profiles.SafeCatalogItem{}}
	if f != nil {
		items := make([]profiles.SafeCatalogItem, 0, len(f.Profiles))
		for name, p := range f.Profiles {
			p.Name = name
			items = append(items, p.SafeView())
		}
		sortSafeItems(items)
		out["default"], out["profiles"] = f.Default, items
	}
	writeJSON(w, out)
}

func sortSafeItems(items []profiles.SafeCatalogItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
}
