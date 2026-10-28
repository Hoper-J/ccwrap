package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/profileapi"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// mutationResponse is the JSON envelope for all four profile mutation
// endpoints (/profile/{add,edit,rm,set-default}). Field usage varies
// per endpoint.
type mutationResponse struct {
	OK              bool                     `json:"ok"`
	Item            *control.SafeCatalogItem `json:"item,omitempty"`
	HasProfilesFile bool                     `json:"has_profiles_file"`
	Default         string                   `json:"default,omitempty"`
	RemovedName     string                   `json:"removed_name,omitempty"`
	LenItems        *int                     `json:"len_items,omitempty"`
	ActiveSessions  []string                 `json:"active_sessions,omitempty"`
	Kind            string                   `json:"kind,omitempty"`
	Message         string                   `json:"message,omitempty"`
	ErrorPaths      []string                 `json:"error_paths,omitempty"`
}

// writeMutationError emits a JSON failure envelope with the given status.
func writeMutationError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:      false,
		Kind:    kind,
		Message: message,
	})
}

// writeMutationSuccess emits the 200 envelope used by add/edit. rm and
// set-default use their own builders (more fields).
func writeMutationSuccess(w http.ResponseWriter, item *control.SafeCatalogItem, hasProfilesFile bool, defaultName string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:              true,
		Item:            item,
		HasProfilesFile: hasProfilesFile,
		Default:         defaultName,
	})
}

// writeStoreError writes a refused profile edit in the mutation envelope.
func writeStoreError(w http.ResponseWriter, e *profileapi.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(mutationResponse{OK: false, Kind: e.Kind, Message: e.Message, ErrorPaths: e.Paths})
}

// decodeProfileMutation runs the checks every profile mutation endpoint
// shares and decodes the body into v: POST only (405, checked first so a
// wrong method never reads as a CSRF failure), the profile token (403), at
// most maxProfileMutationBytes (413), valid JSON (400).
func (sp *sessionProxy) decodeProfileMutation(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	body := http.MaxBytesReader(w, r.Body, maxProfileMutationBytes)
	defer body.Close()
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeMutationError(w, http.StatusRequestEntityTooLarge, "usage",
				fmt.Sprintf("request body exceeds %d bytes", maxProfileMutationBytes))
			return false
		}
		writeMutationError(w, http.StatusBadRequest, "usage", err.Error())
		return false
	}
	return true
}

// toControlSafeCatalogItem converts a profiles.SafeCatalogItem into the
// wire mirror in control. The nested Auth pointer requires field-by-field
// copy across package boundaries (Go does not allow struct conversion
// when a field's pointer target differs across packages, even if shape
// matches). nil-in nil-out propagates.
func toControlSafeCatalogItem(in profiles.SafeCatalogItem) control.SafeCatalogItem {
	var auth *control.SafeAuthSpec
	if in.Auth != nil {
		auth = &control.SafeAuthSpec{
			Mode:         in.Auth.Mode,
			HasInlineKey: in.Auth.HasInlineKey,
			HasKeyEnv:    in.Auth.HasKeyEnv,
		}
	}
	var aliases map[string]string
	if len(in.ModelAliases) > 0 {
		aliases = make(map[string]string, len(in.ModelAliases))
		for k, v := range in.ModelAliases {
			aliases[k] = v
		}
	}
	return control.SafeCatalogItem{
		Name:                in.Name,
		Provider:            in.Provider,
		BaseURLHost:         in.BaseURLHost,
		BaseURL:             in.BaseURL,
		Auth:                auth,
		ModelAliasCount:     in.ModelAliasCount,
		ModelAliases:        aliases,
		UpstreamHeaderCount: in.UpstreamHeaderCount,
		EgressMode:          in.EgressMode,
		EgressHost:          in.EgressHost,
		EgressURL:           in.EgressURL,
	}
}

// listSessionsUsingProfile returns the IDs of currently-reachable
// sessions whose ActiveProfileName matches name. This is a supervisor
// in-process scan via sp.supervisor.listSessions(), NOT loopback via
// discovery.Scan + control.GetSession over a Unix socket.
// Empty/nil slice when no match.
//
// The CLI uses a different mechanism (defaultSessionLooker in
// cmd/ccwrap/profile_crud.go) because the CLI is out-of-process; the
// supervisor reads its own in-memory state.
func (sp *sessionProxy) listSessionsUsingProfile(name string) []string {
	var ids []string
	for _, sess := range sp.supervisor.listSessions() {
		if strings.TrimSpace(sess.ActiveProfileName) == name {
			ids = append(ids, sess.ID)
		}
	}
	return ids
}
