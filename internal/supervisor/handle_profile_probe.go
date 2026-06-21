package supervisor

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// handleProfileTest is the browser-facing POST endpoint that probes
// one profile end-to-end via internal/profiletest.Probe and returns
// a JSON ProbeResult. CSRF token check runs FIRST — before any body
// decode or probe — so a missing/wrong token consumes no resources
// and has no side effects (defense-in-depth, same as
// /profile/switch and /capture/bodies).
//
// Wire contract:
//
//	POST /profile/test
//	  X-CCWRAP-Profile-Token: <token>
//	  Content-Type: application/json
//	  body: {"name": "<profile-name>"}
//	→ 200 + ProbeResult JSON (even on probe failure)
//	→ 4xx (text body) on pre-probe failures (CSRF, parse, lookup)
//
// Name resolution reads profiles.json under sp.supervisor.paths.StateDir,
// with inherit-env support for probing the current shell environment.
// Probe outcomes (OK / AUTH_FAIL / NET_FAIL / SKIPPED / etc.) all return
// HTTP 200 with the ProbeResult JSON; only pre-probe failures (CSRF,
// parse, lookup) return 4xx.
func (sp *sessionProxy) handleProfileTest(w http.ResponseWriter, r *http.Request) {
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden: csrf token missing or invalid", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	profile, err := sp.resolveProfileForTest(name)
	if err != nil {
		writeProfileTestError(w, err)
		return
	}

	result := profiletest.Probe(profile, profiletest.ProbeOptions{
		Timeout: 15 * time.Second,
	})
	data, err := profiletest.MarshalResult(result)
	if err != nil {
		http.Error(w, "marshal result", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// resolveProfileForTest finds the named profile to probe by reading
// profiles.json from the supervisor's StateDir. Returns
// errProfileNotFound when the file is absent / empty / lacks the named
// entry (404), and errProfileConfig for I/O or parse failures (500).
// The synthetic name "inherit-env" dispatches to resolveInheritEnv,
// which builds an ephemeral profile from current ANTHROPIC_* env.
func (sp *sessionProxy) resolveProfileForTest(name string) (profiles.Profile, error) {
	if name == "inherit-env" {
		return resolveInheritEnv()
	}
	stateDir := sp.supervisor.paths.StateDir
	file, err := profiles.Load(profiles.DefaultPath(stateDir))
	if err != nil {
		return profiles.Profile{}, errProfileConfig
	}
	if file == nil || len(file.Profiles) == 0 {
		return profiles.Profile{}, errProfileNotFound
	}
	p, ok := file.Profiles[name]
	if !ok {
		return profiles.Profile{}, errProfileNotFound
	}
	// profiles.Parse already injects Name from the map key, but set it
	// here defensively so callers can rely on Profile.Name without
	// re-reading the map key.
	p.Name = name
	return p, nil
}

// resolveInheritEnv builds an ephemeral profile from current shell
// ANTHROPIC_* env. Precedence matches seedAuth and
// selectInheritEnvTarget in cmd/ccwrap/profile_test_cmd.go:
// ANTHROPIC_API_KEY wins over ANTHROPIC_AUTH_TOKEN; base_url falls
// back to canonical Anthropic if env is empty.
func resolveInheritEnv() (profiles.Profile, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	authTok := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if apiKey == "" && authTok == "" {
		return profiles.Profile{}, errInheritEnvMissing
	}
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	auth := &profiles.AuthSpec{}
	if apiKey != "" {
		auth.Mode = "ccwrap_x_api_key"
		auth.Key = apiKey
	} else {
		auth.Mode = "ccwrap_bearer"
		auth.Key = authTok
	}
	return profiles.Profile{
		Name:    "inherit-env",
		BaseURL: baseURL,
		Auth:    auth,
	}, nil
}

// Error sentinels returned by resolveProfileForTest and the inherit-env
// resolver. writeProfileTestError maps them to HTTP codes.
// errInheritEnvMissing is declared here so the mapping in
// writeProfileTestError stays complete alongside the resolver that
// emits it.
var (
	errProfileNotFound   = errors.New("no such profile")
	errProfileConfig     = errors.New("profile config error")
	errInheritEnvMissing = errors.New("inherit-env credentials missing")
)

// writeProfileTestError maps a resolution error to the spec'd HTTP
// response. Unknown errors collapse to 500 with a sanitized body so
// internal details do not leak into the response.
func writeProfileTestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errProfileNotFound):
		http.Error(w, "no such profile", http.StatusNotFound)
	case errors.Is(err, errInheritEnvMissing):
		http.Error(w, "inherit-env credentials missing", http.StatusNotFound)
	case errors.Is(err, errProfileConfig):
		http.Error(w, "profile config error", http.StatusInternalServerError)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
