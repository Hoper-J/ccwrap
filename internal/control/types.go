package control

import "encoding/json"

// SwitchOutcomeView is the wire-mirror of supervisor.SwitchOutcome. It lives
// in internal/control because the SwitchProfile client method
// returns it; internal/supervisor cannot be imported here (the dependency
// goes the OTHER way — supervisor is a server, control is a wire/client
// library that callers compose against). The supervisor serializes its own
// SwitchOutcome as JSON; this struct decodes that JSON.
//
// View is intentionally an opaque json.RawMessage so the CLI / UI can
// decode it further on demand without internal/control needing to import
// internal/preflight (which would create a cyclic dep via Result types). The
// JSON shape for View matches preflight.ProfileView's tagged fields verbatim.
//
// The string fields (Result, Class, ReasonCode) carry typed enum values from
// the supervisor (SwitchResult, model.RelaunchClass); they're kept as plain
// strings here so the CLI can compare with string literals or with the
// constants below without further marshalling.
type SwitchOutcomeView struct {
	Result     string          `json:"result"`
	Class      string          `json:"class,omitempty"`
	View       json.RawMessage `json:"view,omitempty"`
	ReasonCode string          `json:"reason_code,omitempty"`
	Message    string          `json:"message,omitempty"`
}
