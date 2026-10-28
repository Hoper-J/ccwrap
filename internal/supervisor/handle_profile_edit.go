package supervisor

import (
	"net/http"

	"github.com/Hoper-J/ccwrap/internal/profileapi"
)

// handleProfileEdit is the browser-facing POST endpoint that performs a
// partial update against an existing profiles.json entry.
//
// Wire contract:
//
//	POST /profile/edit
//	  X-CCWRAP-Profile-Token: <profile token>
//	  Content-Type: application/json
//	  body: editRequest — only the fields the caller wants to change.
//	        Pointer fields (Provider, BaseURL, Auth, Egress, SetDefault)
//	        decode left nil for absent → no-op for that field. Name is
//	        REQUIRED (locates the entry; never renamed by /profile/edit).
//	→ 200 + mutationResponse{ok:true, item:<SafeCatalogItem>, ...}
//	→ 403 (CSRF), 405 (non-POST), 400 (bad body / empty name),
//	   404 (no profiles.json or unknown name), 413 (body > 5 MiB),
//	   422 (Validate rejects file post-mutation; passthrough+key mutex;
//	       non-http egress + url mutex), 500 (I/O / marshal).
//
// Method-check FIRST then CSRF, matching the add/set-default handlers
// so a wrong-method PUT surfaces 405 rather than 403. The partial-update
// semantics live in profileapi.ApplyEdit; this handler is the HTTP
// boundary.
func (sp *sessionProxy) handleProfileEdit(w http.ResponseWriter, r *http.Request) {
	var req profileapi.EditRequest
	if !sp.decodeProfileMutation(w, r, &req) {
		return
	}
	saved, e := sp.supervisor.profileStore.Edit(req)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	item := toControlSafeCatalogItem(saved.Profile.SafeView())
	writeMutationSuccess(w, &item, true, saved.Default)
}
