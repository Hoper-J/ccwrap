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

// ProfileCatalogResponse is the wire shape for GET /profile/catalog. Items
// is empty array (not null) when no profiles file or
// when profiles map is empty; HasProfilesFile distinguishes the two.
// LoadError carries a sanitized message when profiles.json could not be
// loaded (malformed / IO error); the endpoint still returns HTTP 200 so
// the UI can distinguish "load failed (here's why)" from "network down".
type ProfileCatalogResponse struct {
	HasProfilesFile bool              `json:"has_profiles_file"`
	Default         string            `json:"default,omitempty"`
	ActiveProfile   string            `json:"active_profile,omitempty"`
	Items           []SafeCatalogItem `json:"items"`
	Source          string            `json:"source,omitempty"`
	LoadError       string            `json:"load_error,omitempty"`
	// EnvBaseURLHost is the host[:port] of ANTHROPIC_BASE_URL as
	// captured in the supervisor's snapshotted parent env at launch
	// (lookupEnv over LaunchContext.Options.ParentEnv). Empty when the
	// env var is unset or its value is malformed. The popover renders
	// it next to the inherit-env row so the user can see what inherit-
	// env mode would actually route to — and detect drift between the
	// frozen active profile's BaseURL and the live env.
	EnvBaseURLHost string `json:"env_base_url_host,omitempty"`
	// EnvHasCredentials reports whether the launch-time parent env has
	// either ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN set (non-empty
	// after trim). The popover uses this to decide whether the
	// inherit-env row's [test] button is meaningful: in OAuth-mode
	// claude-code sessions, both env vars are empty (OAuth tokens
	// live in the keychain, not env), so a probe would always fail
	// with "credentials missing" and the button is misleading.
	EnvHasCredentials bool `json:"env_has_credentials"`
}

// SafeCatalogItem is the wire mirror of profiles.SafeCatalogItem. Same
// JSON tag set; the supervisor converts via a direct struct conversion.
// Keeping the type here preserves the layering (control depends on no
// internal package).
type SafeCatalogItem struct {
	Name                string            `json:"name"`
	Provider            string            `json:"provider"`
	BaseURLHost         string            `json:"base_url_host"`
	BaseURL             string            `json:"base_url,omitempty"`
	Auth                *SafeAuthSpec     `json:"auth,omitempty"`
	ModelAliasCount     int               `json:"model_alias_count,omitempty"`
	ModelAliases        map[string]string `json:"model_aliases,omitempty"`
	UpstreamHeaderCount int               `json:"upstream_header_count,omitempty"`
	EgressMode          string            `json:"egress_mode"`
	EgressHost          string            `json:"egress_host,omitempty"`
	EgressURL           string            `json:"egress_url,omitempty"`
}

// SafeAuthSpec mirrors profiles.SafeAuthSpec. nil means "ccwrap does not
// own auth"; non-nil carries Mode + boolean presence flags only (no key
// bytes, no env NAME).
type SafeAuthSpec struct {
	Mode         string `json:"mode"`
	HasInlineKey bool   `json:"has_inline_key,omitempty"`
	HasKeyEnv    bool   `json:"has_key_env,omitempty"`
}
