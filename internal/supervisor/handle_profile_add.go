package supervisor

import (
	"net/http"

	"github.com/Hoper-J/ccwrap/internal/profileapi"
)

// handleProfileAdd creates a new profile entry in profiles.json and
// optionally promotes it to default in the same atomic write.
//
// Wire contract:
//
//	POST /profile/add
//	  X-CCWRAP-Profile-Token: <token>
//	  Content-Type: application/json
//	  body: {name, provider?, base_url, auth:{mode,key?,key_env?},
//	         egress?:{mode?,url?}, set_default?}
//	→ 200 + mutationResponse{ok:true, item:<SafeCatalogItem>,
//	         has_profiles_file:true, default:<file.Default>}
//	→ 403 (CSRF), 405 (non-POST), 400 (bad body / missing name/base_url),
//	   413 (body > 5 MiB), 409 (profile name already exists),
//	   422 (Validate rejects file post-mutation; or passthrough+key mutex;
//	       or non-http egress + url mutex), 500 (I/O / marshal).
//
// Method-check FIRST (matches set-default and the dispatch contract for
// /profile/switch), CSRF SECOND so a wrong-method PUT still surfaces 405
// rather than 403. The wire envelope's `item` field uses
// control.SafeCatalogItem (no AuthKey field) so the inline secret never
// escapes the supervisor.
func (sp *sessionProxy) handleProfileAdd(w http.ResponseWriter, r *http.Request) {
	var req profileapi.AddRequest
	if !sp.decodeProfileMutation(w, r, &req) {
		return
	}
	saved, e := sp.supervisor.profileStore.Add(req)
	if e != nil {
		writeStoreError(w, e)
		return
	}
	// The wire item is control.SafeCatalogItem: no inline key.
	item := toControlSafeCatalogItem(saved.Profile.SafeView())
	writeMutationSuccess(w, &item, true, saved.Default)
}
