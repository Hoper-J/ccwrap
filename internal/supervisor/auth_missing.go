package supervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// authMissingErrorBody is the JSON shape of the 502 response body for
// requests refused at the request-time fail-closed gate.
// Modeled on Anthropic's own error shape ({type:"error", error:{type,
// message}}) so Claude Code's SDK + downstream JSON consumers see a
// familiar structure. error.type = "ccwrap_auth_missing" is the unique
// discriminator. error.env_var (omitempty) and error.profile let
// machine consumers branch without parsing the message string.
type authMissingErrorBody struct {
	Type  string                 `json:"type"`
	Error authMissingErrorDetail `json:"error"`
}

type authMissingErrorDetail struct {
	Type    string `json:"type"`
	Profile string `json:"profile"`
	EnvVar  string `json:"env_var,omitempty"`
	Message string `json:"message"`
}

// maybeRefuseAuthMissing is the invariant guard at the layer where
// the invariant actually applies — the request boundary. Returns true if
// the caller should stop (the request was refused with a 502); false if
// the caller should proceed with normal forwarding.
//
// Fires strictly when ALL three conditions hold:
//   - applyAuth: this request would normally have ccwrap-injected auth (i.e.,
//     it targets an Anthropic API host). For inherit-env / first-party
//     SDK-auth / non-anthropic-host paths this is false → no refusal.
//   - ap.r.authBootstrap == AuthBootstrapMissing: preflight detected no
//     usable auth source. set in two cases: (A) profile names a key_env
//     that env doesn't have, (B) third-party-hidden route resolved to
//     passthrough because no source was found.
//   - ap.r.overrideAuth == nil: belt-and-suspenders — the resolver
//     would never produce overrideAuth in the Missing case, but the
//     defensive check catches future regressions.
//
// Records the refusal as a synthetic request row (so it appears in the
// Activity list at the moment the user acts) + an ErrorRecord with
// SuggestedAction telling the user how to recover. Returns a structured
// JSON 502 body.
func (sp *sessionProxy) maybeRefuseAuthMissing(w http.ResponseWriter, r *http.Request, ap *posture, applyAuth bool, logicalHost string, start time.Time) bool {
	if !applyAuth {
		return false
	}
	if ap == nil || ap.r.authBootstrap != model.AuthBootstrapMissing {
		return false
	}
	if ap.r.overrideAuth != nil {
		return false
	}

	msg, envVar := authMissingMessage(ap.r.profileName, ap.r.missingAuthEnv)
	body, _ := json.Marshal(authMissingErrorBody{
		Type: "error",
		Error: authMissingErrorDetail{
			Type:    "ccwrap_auth_missing",
			Profile: ap.r.profileName,
			EnvVar:  envVar,
			Message: msg,
		},
	})

	sessionID := sp.session.public.ID
	sp.supervisor.recordRequest(sessionID, model.RequestRecord{
		Timestamp:             start,
		SessionID:             sessionID,
		Method:                r.Method,
		LogicalTargetHost:     logicalHost,
		Path:                  pathForRecord(r.URL, true),
		StatusCode:            http.StatusBadGateway,
		LatencyMS:             time.Since(start).Milliseconds(),
		Synthetic:             true, // ccwrap-generated, not from upstream
		ActiveProfileName:     ap.r.profileName,
		ActiveProfileProvider: ap.r.profileProvider,
		RequestHeaders:        r.Header.Clone(),
	})
	sp.supervisor.recordError(sessionID, model.ErrorRecord{
		Timestamp:       time.Now(),
		SessionID:       sessionID,
		Severity:        "warn",
		ErrorClass:      "ccwrap_auth_missing",
		Summary:         msg,
		UpstreamHost:    logicalHost,
		SuggestedAction: authMissingSuggestion(envVar),
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(body)
	return true
}

// authMissingMessage formats the human-readable single-line message used in
// the 502 body, the recorded ErrorRecord.Summary, and (with the same logic)
// the launch banner + ribbon Auth-cell detail. Branches on missingAuthEnv
// emptiness: non-empty = Case A (concrete env), empty = Case B (no source).
// Returned envVar mirrors the input — convenience for the struct builder.
func authMissingMessage(profileName, missingAuthEnv string) (msg, envVar string) {
	profile := profileName
	if profile == "" {
		profile = "(no profile)"
	}
	envVar = missingAuthEnv
	if envVar != "" {
		msg = fmt.Sprintf("profile %q requires CCWRAP-owned auth but env $%s is not set. Restore the env, switch profile via inspect, or restart with `ccwrap --profile inherit-env`.", profile, envVar)
		return msg, envVar
	}
	msg = fmt.Sprintf("profile %q has no auth source configured (no auth.key, no auth.key_env, and no env candidate). Edit the profile to add auth.key_env, switch profile via inspect, or restart with `ccwrap --profile inherit-env`.", profile)
	return msg, envVar
}

// authMissingSuggestion is the short ErrorRecord.SuggestedAction string —
// shorter than the message, optimized for the Activity row's "right" column.
func authMissingSuggestion(envVar string) string {
	if envVar != "" {
		return fmt.Sprintf("set $%s, switch profile via inspect, or use --profile inherit-env", envVar)
	}
	return "edit the profile to add auth.key_env, switch profile via inspect, or use --profile inherit-env"
}
