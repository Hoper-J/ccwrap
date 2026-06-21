package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/profiles"
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxProfileMutationBytes)
	defer body.Close()
	var req setDefaultRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeMutationError(w, http.StatusRequestEntityTooLarge, "usage",
				fmt.Sprintf("request body exceeds %d bytes", maxProfileMutationBytes))
			return
		}
		writeMutationError(w, http.StatusBadRequest, "usage", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) != req.Name || req.Name == "" {
		writeMutationError(w, http.StatusBadRequest, "usage",
			"name must not be empty or contain leading/trailing whitespace")
		return
	}

	// Serialize the load-check-write sequence with the other CRUD
	// handlers (see profileFileMu doc on Supervisor).
	sp.supervisor.profileFileMu.Lock()
	defer sp.supervisor.profileFileMu.Unlock()

	// profileFileMu only serializes goroutines in THIS process; take the
	// cross-process file lock too so a `ccwrap profile` CLI or another
	// supervisor cannot clobber this write. See profiles.Lock.
	unlock, err := profiles.Lock(sp.supervisor.paths.StateDir)
	if err != nil {
		writeMutationError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer unlock()

	path := profiles.DefaultPath(sp.supervisor.paths.StateDir)
	f, err := profiles.Load(path)
	if err != nil {
		writeMutationError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if f == nil {
		writeMutationError(w, http.StatusNotFound, "not_found", "no profiles.json present")
		return
	}
	if req.Name == profiles.InheritEnv {
		// inherit-env-as-Default is invalid. Treat the sentinel request
		// as "set default to official" — the modern equivalent
		// (Auth=nil → claude-code OAuth path).
		f.Default = profiles.OfficialProfileName
		// Make sure official actually exists (defense-in-depth; ensure
		// runs on every launch but the file might've been edited
		// between then and now).
		if _, ok := f.Profiles[profiles.OfficialProfileName]; !ok {
			f.Profiles[profiles.OfficialProfileName] = profiles.OfficialProfile()
		}
	} else if _, ok := f.Profiles[req.Name]; ok {
		f.Default = req.Name
	} else {
		writeMutationError(w, http.StatusNotFound, "not_found", "no such profile")
		return
	}

	if err := profiles.OverwriteFile(path, f, "profile set-default"); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			writeMutationValidationError(w, perr)
			return
		}
		writeMutationError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:              true,
		HasProfilesFile: true,
		Default:         f.Default,
	})
}
