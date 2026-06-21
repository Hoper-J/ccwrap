package supervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/control"
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

// writeMutationValidationError formats a *ParseErrors into the wire
// envelope without leaking the on-disk path through err.Error().
// Matches the sanitizeProfileCatalogError pattern in server.go: iterate
// items, format each, write to b.WriteString.
//
// CALLER CONTRACT: perr.Source MUST be an operation label like
// "profile add foo" — produced by Validate(f, opLabel) or
// OverwriteFile(path, f, opLabel). NEVER forward a *ParseErrors
// returned by profiles.Load(path) directly — its Source IS the
// on-disk path, which would leak via the wire envelope's Message field.
// As defense-in-depth, this function clamps Source to its base name
// (filepath.Base) if it contains a path separator.
func writeMutationValidationError(w http.ResponseWriter, perr *profiles.ParseErrors) {
	var b strings.Builder
	src := perr.Source
	if strings.ContainsAny(src, "/\\") {
		src = filepath.Base(src)
	}
	fmt.Fprintf(&b, "%s invalid: %d errors", src, len(perr.Items))
	paths := make([]string, 0, len(perr.Items))
	for _, it := range perr.Items {
		fmt.Fprintf(&b, "\n  - %s", it.Error())
		paths = append(paths, it.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(mutationResponse{
		OK:         false,
		Kind:       "validation_error",
		Message:    b.String(),
		ErrorPaths: paths,
	})
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
