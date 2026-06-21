package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// addRequest is the JSON body for POST /profile/add. It mirrors a subset
// of profiles.Profile shape — Name + Provider + BaseURL + Auth + Egress
// — plus the imperative SetDefault flag that, when true, flips
// file.Default to the new profile in the same write.
type addRequest struct {
	Name         string            `json:"name"`
	Provider     string            `json:"provider"`
	BaseURL      string            `json:"base_url"`
	Auth         *addAuthBlock     `json:"auth"`
	Egress       addEgressBlock    `json:"egress"`
	ModelAliases map[string]string `json:"model_aliases,omitempty"`
	SetDefault   bool              `json:"set_default"`
}

type addAuthBlock struct {
	Mode   string `json:"mode"`
	Key    string `json:"key,omitempty"`
	KeyEnv string `json:"key_env,omitempty"`
}

type addEgressBlock struct {
	Mode string `json:"mode,omitempty"`
	URL  string `json:"url,omitempty"`
}

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
	var req addRequest
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
	if strings.TrimSpace(req.BaseURL) == "" {
		writeMutationError(w, http.StatusBadRequest, "usage", "base_url is required")
		return
	}

	// Auth nil = "ccwrap does not own auth" (toggle off in popover).
	// When Auth is present, Validate V1/V2/V3 enforce mode + key rules
	// uniformly through the validation envelope — no handler-side
	// prechecks (those produce divergent error messages for the same
	// condition).

	// Egress non-http + URL mutex.
	egressMode := strings.ToLower(strings.TrimSpace(req.Egress.Mode))
	if egressMode != "" && egressMode != "http" && strings.TrimSpace(req.Egress.URL) != "" {
		writeMutationError(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("egress.mode %s is incompatible with egress.url", egressMode))
		return
	}

	// Serialize the load-check-write sequence so two concurrent adds
	// with the same name can't both pass the exists-check and clobber
	// each other. The mutex is shared with edit / rm / set-default in
	// the same Supervisor.
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
		// No file exists yet. Seed with official + the new profile
		// (official auto-restored by EnsureOfficialProfile on every launch,
		// but the add endpoint may be called in tests / before-launch flows
		// where ensure hasn't run).
		f = &profiles.File{
			Default: profiles.OfficialProfileName,
			Profiles: map[string]profiles.Profile{
				profiles.OfficialProfileName: profiles.OfficialProfile(),
			},
		}
	}
	if _, exists := f.Profiles[req.Name]; exists {
		writeMutationError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("profile %q already exists", req.Name))
		return
	}

	p := profiles.Profile{
		Name:     req.Name,
		Provider: req.Provider,
		BaseURL:  req.BaseURL,
		Egress: profiles.EgressSpec{
			Mode: req.Egress.Mode,
			URL:  req.Egress.URL,
		},
	}
	if req.Auth != nil {
		p.Auth = &profiles.AuthSpec{
			Mode:   req.Auth.Mode,
			Key:    req.Auth.Key,
			KeyEnv: req.Auth.KeyEnv,
		}
	}
	if len(req.ModelAliases) > 0 {
		p.ModelAliases = make(map[string]string, len(req.ModelAliases))
		for k, v := range req.ModelAliases {
			p.ModelAliases[k] = v
		}
	}
	f.Profiles[req.Name] = p
	if req.SetDefault {
		f.Default = req.Name
	}

	if err := profiles.OverwriteFile(path, f, "profile add "+req.Name); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			writeMutationValidationError(w, perr)
			return
		}
		writeMutationError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// p.Name set above; SafeView consumes it. The struct conversion
	// from profiles.SafeCatalogItem to control.SafeCatalogItem is the
	// wire boundary — neither shape carries the inline key.
	item := toControlSafeCatalogItem(p.SafeView())
	writeMutationSuccess(w, &item, true, f.Default)
}
