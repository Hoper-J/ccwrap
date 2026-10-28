package supervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/modelalias"
)

// handleProfileModelDefault is the dashboard's one-click fix for Claude
// models sent with no alias: it sets the active profile's "*" alias to
// target (a model, or "*" to keep Claude names) and applies the profile to
// the live session.
//
// Wire contract:
//
//	POST /profile/model-default
//	  X-CCWRAP-Profile-Token: <profile token>
//	  body: {"target": "<model>" | "*"}
//	→ 200 + SwitchOutcome of re-applying the profile (its result says
//	  whether the live session picked the change up)
//	→ 403 (CSRF), 405 (non-POST), 400 (bad body), 409 (session has no
//	  profile), 404 (profile gone), 422 (alias rejected), 500 (I/O).
//
// Only "*" is touched; the profile's other aliases stay as they are.
func (sp *sessionProxy) handleProfileModelDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Target string `json:"target"`
	}
	body := http.MaxBytesReader(w, r.Body, maxProfileMutationBytes)
	defer body.Close()
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeMutationError(w, http.StatusBadRequest, "usage", err.Error())
		return
	}
	outcome, apiErr := sp.applyModelDefault(req.Target)
	if apiErr != nil {
		writeMutationError(w, apiErr.status, apiErr.code, apiErr.message)
		return
	}
	writeJSON(w, outcome)
}

// applyModelDefault sets the active profile's "*" alias to target and applies
// the profile to the live session; shared by the page endpoint and the API.
func (sp *sessionProxy) applyModelDefault(target string) (SwitchOutcome, *apiError) {
	target = strings.TrimSpace(target)
	if target == "" {
		return SwitchOutcome{}, &apiError{http.StatusBadRequest, "usage", "target must not be empty"}
	}
	ap := sp.session.active.Load()
	if ap == nil || ap.r.profileName == "" {
		return SwitchOutcome{}, &apiError{http.StatusConflict, "no_profile",
			fmt.Sprintf("this session runs without a profile; relaunch with --model-alias '%s=%s'", modelalias.DefaultKey, target)}
	}
	name := ap.r.profileName
	if e := sp.supervisor.profileStore.SetModelDefault(name, target); e != nil {
		return SwitchOutcome{}, &apiError{e.Status, e.Kind, e.Message}
	}
	// The write is done and its locks released; re-apply the profile so
	// the session uses it now (SwitchProfile takes its own locks).
	return sp.supervisor.SwitchProfile(sp.session.public.ID, name), nil
}
