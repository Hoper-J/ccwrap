package supervisor

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/modelalias"
)

// applyModelDefault is the one-click fix for Claude models sent with no
// alias (the v1 API's model-default): it sets the active profile's "*"
// alias to target (a model, or "*" to keep Claude names) and applies the
// profile to the live session. Only "*" is touched; the profile's other
// aliases stay as they are.
func (sp *sessionProxy) applyModelDefault(target string) (SwitchOutcome, *apiError) {
	target = strings.TrimSpace(target)
	if target == "" {
		return SwitchOutcome{}, &apiError{http.StatusBadRequest, "usage", "target must not be empty"}
	}
	ap := sp.session.active.Load()
	if ap == nil || ap.r.profileName == "" {
		return SwitchOutcome{}, &apiError{http.StatusConflict, "no_profile",
			fmt.Sprintf("this session runs without a profile; relaunch with --model-alias '%s=%s'", modelalias.DefaultKey, target)}
	}
	name := ap.r.profileName
	if e := sp.supervisor.profileStore.SetModelDefault(name, target); e != nil {
		return SwitchOutcome{}, &apiError{e.Status, e.Kind, e.Message}
	}
	// The write is done and its locks released; re-apply the profile so
	// the session uses it now (SwitchProfile takes its own locks).
	return sp.supervisor.SwitchProfile(sp.session.public.ID, name), nil
}
