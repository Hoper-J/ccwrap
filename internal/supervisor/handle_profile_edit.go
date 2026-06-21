package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

type editRequest struct {
	Name         string             `json:"name"`
	Provider     *string            `json:"provider,omitempty"`
	BaseURL      *string            `json:"base_url,omitempty"`
	Auth         json.RawMessage    `json:"auth,omitempty"`          // 3-state: nil=absent, "null"=remove, object=replace/patch
	ModelAliases *map[string]string `json:"model_aliases,omitempty"` // nil=no change, &{}=clear, &{...}=replace
	Egress       *editEgress        `json:"egress,omitempty"`
	SetDefault   *bool              `json:"set_default,omitempty"`
}

// editAuthDelta is the inner shape carried by editRequest.Auth when the
// field is a JSON object. It carries the same partial-update fields the
// old editAuth value used, decoded lazily from RawMessage so the handler
// can distinguish absent ("no change") from null ("remove") at the byte
// level — JSON unmarshal alone cannot tell those apart.
type editAuthDelta struct {
	Mode      *string `json:"mode,omitempty"`
	KeySource string  `json:"key_source,omitempty"` // "inline" | "env_var" | "unchanged" | ""
	Key       string  `json:"key,omitempty"`
	KeyEnv    string  `json:"key_env,omitempty"`
}

type editEgress struct {
	Mode *string `json:"mode,omitempty"`
	URL  *string `json:"url,omitempty"`
}

// applyEditRequest mutates p in place based on which fields the request
// provided. Mirrors cmd/ccwrap/profile_crud.go::applyEditOpts.
//
// Auth 3-state:
//   - field absent (req.Auth==nil) → no change to stored Auth
//   - field is JSON null          → set p.Auth = nil (remove block)
//   - field is JSON object        → patch existing Auth or allocate one
func applyEditRequest(p *profiles.Profile, req editRequest) error {
	if req.Provider != nil {
		p.Provider = *req.Provider
	}
	if req.BaseURL != nil {
		p.BaseURL = *req.BaseURL
	}

	if len(req.Auth) > 0 {
		if bytes.Equal(bytes.TrimSpace(req.Auth), []byte("null")) {
			// Explicit null: remove the auth block. Toggle-off path
			// from popover; CLI equivalent of --remove-auth.
			p.Auth = nil
		} else {
			var delta editAuthDelta
			if err := json.Unmarshal(req.Auth, &delta); err != nil {
				return fmt.Errorf("auth: %v", err)
			}
			if p.Auth == nil {
				p.Auth = &profiles.AuthSpec{}
			}
			if delta.Mode != nil {
				p.Auth.Mode = *delta.Mode
			}
			switch delta.KeySource {
			case "", "unchanged":
				// no-op — keep current Key / KeyEnv on disk
			case "inline":
				p.Auth.Key = delta.Key
				p.Auth.KeyEnv = ""
			case "env_var":
				p.Auth.Key = ""
				p.Auth.KeyEnv = delta.KeyEnv
			default:
				return fmt.Errorf("auth.key_source: unknown value %q", delta.KeySource)
			}
		}
	}

	// ModelAliases 3-state via pointer: nil=no change, &{}=clear, &{...}=replace.
	if req.ModelAliases != nil {
		if len(*req.ModelAliases) == 0 {
			p.ModelAliases = nil
		} else {
			p.ModelAliases = make(map[string]string, len(*req.ModelAliases))
			for k, v := range *req.ModelAliases {
				p.ModelAliases[k] = v
			}
		}
	}

	if req.Egress != nil {
		if req.Egress.Mode != nil {
			p.Egress.Mode = *req.Egress.Mode
			newMode := strings.ToLower(strings.TrimSpace(*req.Egress.Mode))
			if newMode != "http" && newMode != "socks5" && newMode != "socks5h" {
				p.Egress.URL = ""
			}
		}
		if req.Egress.URL != nil {
			modeForCheck := p.Egress.Mode
			if req.Egress.Mode != nil {
				modeForCheck = *req.Egress.Mode
			}
			lowered := strings.ToLower(strings.TrimSpace(modeForCheck))
			urlBearing := lowered == "http" || lowered == "socks5" || lowered == "socks5h"
			if lowered != "" && !urlBearing && strings.TrimSpace(*req.Egress.URL) != "" {
				return fmt.Errorf("egress.mode %s is incompatible with egress.url", lowered)
			}
			p.Egress.URL = *req.Egress.URL
		}
	}
	return nil
}

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
// semantics live in applyEditRequest — this handler only handles
// the HTTP boundary: decode, locate, dispatch, then OverwriteFile.
func (sp *sessionProxy) handleProfileEdit(w http.ResponseWriter, r *http.Request) {
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
	var req editRequest
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
	// handlers so concurrent edits / adds / rm on the same file can't
	// race.
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
	p, ok := f.Profiles[req.Name]
	if !ok {
		writeMutationError(w, http.StatusNotFound, "not_found", "no such profile")
		return
	}
	p.Name = req.Name

	if err := applyEditRequest(&p, req); err != nil {
		writeMutationError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	f.Profiles[req.Name] = p
	if req.SetDefault != nil && *req.SetDefault {
		f.Default = req.Name
	}

	if err := profiles.OverwriteFile(path, f, "profile edit "+req.Name); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			writeMutationValidationError(w, perr)
			return
		}
		writeMutationError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	item := toControlSafeCatalogItem(p.SafeView())
	writeMutationSuccess(w, &item, true, f.Default)
}
