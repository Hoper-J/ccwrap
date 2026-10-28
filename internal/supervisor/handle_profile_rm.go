package supervisor

import (
	"encoding/json"
	"net/http"
)

// rmRequest is the JSON body for POST /profile/rm. The confirm_name
// field is a server-side defense-in-depth guard — even if the browser
// skips the type-to-confirm UI, the supervisor rejects the request
// unless confirm_name matches name. confirm_name is trimmed BEFORE
// comparison so trailing whitespace from a paste does not block a
// legitimate removal.
type rmRequest struct {
	Name        string `json:"name"`
	ConfirmName string `json:"confirm_name"`
}

// handleProfileRm removes a profile from profiles.json. When the removed
// profile was the file.Default another one becomes the default
// (profileapi.ChooseFallbackDefault); when it was the last one, the file is
// rewritten as a legal-empty {Default:"inherit-env", Profiles:{}}.
//
// Wire contract:
//
//	POST /profile/rm
//	  X-CCWRAP-Profile-Token: <profile token>
//	  Content-Type: application/json
//	  body: {"name":"<profile-name>","confirm_name":"<profile-name>"}
//	→ 200 + mutationResponse{ok:true, has_profiles_file:true,
//	         default:<file.Default>, removed_name:<name>,
//	         len_items:<remaining count>, active_sessions:[<ids>]}
//	→ 403 (CSRF), 405 (non-POST), 400 (bad body / empty name /
//	   confirm_name mismatch), 413 (body > 5 MiB),
//	   404 (no profiles.json / no such profile),
//	   422 (Validate rejects file post-mutation), 500 (I/O / marshal).
//
// Method-check FIRST then CSRF, matching the add/edit/set-default
// handlers so a wrong-method PUT surfaces 405 rather than 403.
//
// Active-session report: the in-process scan
// listSessionsUsingProfile(name) records IDs of sessions whose active
// profile matched the name at removal time — the browser surfaces them
// so the user knows which sessions will fall back to InheritEnv on next
// request. This is observational; the supervisor does NOT pre-empt
// in-flight requests or reset session active postures here.
func (sp *sessionProxy) handleProfileRm(w http.ResponseWriter, r *http.Request) {
	var req rmRequest
	if !sp.decodeProfileMutation(w, r, &req) {
		return
	}
	// Taken before the removal, so a session using the profile is still
	// counted although its entry is about to go.
	activeIDs := sp.listSessionsUsingProfile(req.Name)
	removed, e := sp.supervisor.profileStore.Remove(req.Name, req.ConfirmName)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	n := removed.Remaining
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:              true,
		HasProfilesFile: true,
		Default:         removed.Default,
		RemovedName:     req.Name,
		LenItems:        &n,
		ActiveSessions:  activeIDs,
	})
}
