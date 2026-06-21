package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/profiles"
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
// profile was the file.Default the default is reset to the InheritEnv
// sentinel; when the removed profile is the last one, the file is
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
	var req rmRequest
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
	// confirm_name is trimmed before comparison so a paste with trailing
	// whitespace doesn't block a legitimate removal. The
	// `name` field is required not to have any whitespace, so trimming
	// here only loosens the comparison on the confirmation side.
	if strings.TrimSpace(req.ConfirmName) != req.Name {
		writeMutationError(w, http.StatusBadRequest, "usage", "confirm_name must match name")
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
	if _, ok := f.Profiles[req.Name]; !ok {
		writeMutationError(w, http.StatusNotFound, "not_found", "no such profile")
		return
	}

	// In-process active-session scan, NOT loopback. The scan happens
	// BEFORE the delete so a session whose ActiveProfileName
	// matched at request time is still recorded even though the entry
	// is about to be gone from disk.
	activeIDs := sp.listSessionsUsingProfile(req.Name)

	delete(f.Profiles, req.Name)
	if req.Name == f.Default {
		// Pick a replacement Default. Order: prefer OfficialProfileName
		// when still present, else the alphabetically-first remaining
		// profile (deterministic), else the InheritEnv sentinel when
		// the file would have zero profiles left.
		//
		// validate.go::validateDefault EXPLICITLY accepts InheritEnv.
		// EnsureOfficialProfile auto-migrates Default=inherit-env back to
		// "official" on next launch, so the sentinel is transient in
		// practice but a legitimate persisted state per the validate
		// contract.
		f.Default = chooseFallbackDefault(f, req.Name)
	}

	if err := profiles.OverwriteFile(path, f, "profile rm "+req.Name); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			writeMutationValidationError(w, perr)
			return
		}
		writeMutationError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	n := len(f.Profiles)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:              true,
		HasProfilesFile: true,
		Default:         f.Default,
		RemovedName:     req.Name,
		LenItems:        &n,
		ActiveSessions:  activeIDs,
	})
}

// chooseFallbackDefault picks a replacement Default after rm removes the
// existing default profile. Prefers OfficialProfileName when present
// (and not the one being removed); else the alphabetically-first
// remaining profile (deterministic); else returns empty string when
// the file would have zero profiles — caller is expected to delete
// the file or let EnsureOfficialProfile re-seed on next launch.
func chooseFallbackDefault(f *profiles.File, removingName string) string {
	if _, ok := f.Profiles[profiles.OfficialProfileName]; ok && profiles.OfficialProfileName != removingName {
		return profiles.OfficialProfileName
	}
	names := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		if n == removingName {
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		// No remaining profiles — fall back to the sentinel. Validate
		// accepts it; EnsureOfficialProfile re-seeds on next launch.
		return profiles.InheritEnv
	}
	// Alphabetical first for determinism.
	sort.Strings(names)
	return names[0]
}
