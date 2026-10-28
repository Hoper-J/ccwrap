package supervisor

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profileapi"
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

	profile, err := sp.supervisor.profileStore.ProfileForTest(name, os.Getenv)
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

// writeProfileTestError maps a profile lookup error to its HTTP response.
func writeProfileTestError(w http.ResponseWriter, err error) {
	status, msg := profileapi.TestErrorStatus(err)
	http.Error(w, msg, status)
}
