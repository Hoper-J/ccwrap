package supervisor

import (
	"encoding/json"
	"net/http"
)

// maxProfileMutationBytes caps the request body for the four profile
// mutation endpoints at 5 MiB. The constant is declared in the
// set-default handler; add/edit/rm just reference it.
const maxProfileMutationBytes = 5 * 1024 * 1024

// setDefaultRequest is the JSON body for POST /profile/set-default.
// A bare `name` field; no payload, no metadata. Validation: non-empty
// and no leading/trailing whitespace.
type setDefaultRequest struct {
	Name string `json:"name"`
}

// handleProfileSetDefault flips file.Default in profiles.json to either
// a named profile or the synthetic InheritEnv sentinel.
//
// Wire contract:
//
//	POST /profile/set-default
//	  X-CCWRAP-Profile-Token: <token>
//	  Content-Type: application/json
//	  body: {"name": "<profile-name>" | "inherit-env"}
//	→ 200 + mutationResponse{ok:true, has_profiles_file:true, default:<name>}
//	→ 403 (CSRF), 405 (non-POST), 400 (bad body / empty / whitespace name),
//	   413 (body > 5 MiB), 404 (no profiles.json / no such profile),
//	   422 (Validate rejects file post-mutation), 500 (I/O / marshal).
//
// Method-check FIRST (matches the dispatch contract for /profile/switch:
// dispatch returns 405 for non-POST before calling the handler). The
// CSRF guard runs SECOND so a wrong-method GET still surfaces 405
// rather than 403.
func (sp *sessionProxy) handleProfileSetDefault(w http.ResponseWriter, r *http.Request) {
	var req setDefaultRequest
	if !sp.decodeProfileMutation(w, r, &req) {
		return
	}
	def, e := sp.supervisor.profileStore.SetDefault(req.Name)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:              true,
		HasProfilesFile: true,
		Default:         def,
	})
}
