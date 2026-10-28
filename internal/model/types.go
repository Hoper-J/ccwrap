package model

import (
	"net/http"
	"strings"
	"time"
)

type SessionState string

type RouteSource string

type RouteClass string

type AuthMode string

type AuthSource string

type AuthPolicy string

type AuthBootstrap string

type AuthBootstrapKind string

type FailPolicy string

type ModelAliasMode string

type StreamState string

const ControlAPIVersion = "v1"

const (
	StateCreated   SessionState = "created"
	StateLaunching SessionState = "launching"
	StateAttached  SessionState = "attached"
	StateActive    SessionState = "active"
	StateEnded     SessionState = "ended"
)

// SessionHealth is the orthogonal "is the session healthy" dimension,
// independent of lifecycle State: the severity of the worst open Issue, or ok
// when none is open. Empty string is treated as ok at render.
type SessionHealth string

const (
	HealthOK    SessionHealth = "ok"
	HealthWarn  SessionHealth = "warn"
	HealthError SessionHealth = "error"
)

// Issue is a problem the session ran into, worded for the user, with the
// fixes a UI can offer. A session keeps one per ID: it opens when the
// problem shows up and turns resolved when the problem goes away, staying in
// the list until the session ends; it opens again if the problem comes back.
type Issue struct {
	ID       string        `json:"id"`
	State    IssueState    `json:"state"`
	Severity SessionHealth `json:"severity"`
	Title    string        `json:"title"`
	// TitleCode is the leading part of Title that names a model or host,
	// for showing as code; empty when there is none.
	TitleCode string `json:"title_code,omitempty"`
	Detail    string `json:"detail"`
	// Evidence is what ccwrap saw since the issue opened: the latest error
	// and how many requests it hit (Count). LastSeenAt is when it last
	// happened.
	Evidence   string     `json:"evidence,omitempty"`
	Count      int        `json:"count,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	// Fixes and Note are offered while the issue is open.
	Fixes []IssueFix `json:"fixes,omitempty"`
	Note  string     `json:"note,omitempty"`
	// Resolution says how a resolved issue went away.
	Resolution string `json:"resolution,omitempty"`
}

type IssueState string

const (
	IssueOpen     IssueState = "open"
	IssueResolved IssueState = "resolved"
)

// Issue IDs.
const (
	IssueUnaliasedModel      = "unaliased_model"
	IssueCredentialsRejected = "credentials_rejected"
	// IssueBaseURL is a base URL Claude Code can't use: one ending in /v1,
	// or a provider's OpenAI-compatible endpoint.
	IssueBaseURL             = "base_url"
	IssueRateLimited         = "rate_limited"
	IssueAuthMissing         = "auth_missing"
	IssueNativeTLSBlocked    = "native_tls_blocked"
	IssueUpstreamUnreachable = "upstream_unreachable"
	// IssueRequestsFailing collects the failures no other issue covers.
	IssueRequestsFailing = "requests_failing"
)

// IssueFix is one button on an issue.
type IssueFix struct {
	Label  string `json:"label"`
	Action string `json:"action"`
	// Value is the action's argument: the model_default target, the
	// open_profile profile name, or the show_activity filter.
	Value string `json:"value,omitempty"`
	// Field is the profile field open_profile points at: "key" or
	// "base_url".
	Field string `json:"field,omitempty"`
	// At is the timestamp of the activity row show_activity reveals.
	At      *time.Time `json:"at,omitempty"`
	Primary bool       `json:"primary,omitempty"`
}

// IssueFix actions.
const (
	// FixModelDefault sets the profile's "*" alias to Value and applies it
	// (POST model-default).
	FixModelDefault = "model_default"
	// FixOpenProfile opens profile Value for editing, at Field.
	FixOpenProfile = "open_profile"
	// FixShowActivity shows the Activity filter Value, at row At.
	FixShowActivity = "show_activity"
)

const (
	RouteSourceExplicit       RouteSource = "explicit"
	RouteSourceInheritedEnv   RouteSource = "inherited_env"
	RouteSourceClaudeSettings RouteSource = "claude_settings"
	RouteSourceFlagSettings   RouteSource = "flag_settings"
	RouteSourcePolicySettings RouteSource = "policy_settings"
	RouteSourceFallback       RouteSource = "fallback_default"
)

const (
	RouteClassFirstParty           RouteClass = "first_party"
	RouteClassThirdPartyHidden     RouteClass = "third_party_hidden"
	RouteClassThirdPartyCompatible RouteClass = "third_party_compatible"
)

const (
	AuthModePassthrough            AuthMode   = "passthrough"
	AuthModeOverrideXAPIKey        AuthMode   = "override-x-api-key"
	AuthModeOverrideBearer         AuthMode   = "override-authorization-bearer"
	AuthModeUnsupported            AuthMode   = "unsupported"
	AuthSourceNone                 AuthSource = "none"
	AuthSourceAnthropicAPIKey      AuthSource = "ANTHROPIC_API_KEY"
	AuthSourceAnthropicToken       AuthSource = "ANTHROPIC_AUTH_TOKEN"
	AuthSourceClaudeOAuthToken     AuthSource = "CLAUDE_CODE_OAUTH_TOKEN"
	AuthSourceCCWRAPUpstreamAPIKey AuthSource = "CCWRAP_UPSTREAM_API_KEY"
	AuthSourceCCWRAPUpstreamToken  AuthSource = "CCWRAP_UPSTREAM_AUTH_TOKEN"
)

const (
	AuthPolicyFirstPartyPassthrough    AuthPolicy = "first_party_passthrough"
	AuthPolicyCCWRAPOverride           AuthPolicy = "ccwrap_override"
	AuthPolicyCCWRAPOverrideFailClosed AuthPolicy = "ccwrap_override_fail_closed"
	AuthPolicyUnsafePassthrough        AuthPolicy = "unsafe_passthrough"
)

const (
	AuthBootstrapNotNeeded         AuthBootstrap = "not_needed"
	AuthBootstrapPlaceholderActive AuthBootstrap = "placeholder_active"
	AuthBootstrapMissing           AuthBootstrap = "missing"
)

const (
	AuthBootstrapKindNone    AuthBootstrapKind = "none"
	AuthBootstrapKindXAPIKey AuthBootstrapKind = "x_api_key"
	AuthBootstrapKindBearer  AuthBootstrapKind = "bearer"
)

const (
	FailClosed FailPolicy = "fail-closed"
)

// RelaunchClass classifies a posture transition (current → candidate
// profile) for the Provider Profiles feature. It is computed when a
// profile is selected and consumed by the live-swap and switcher logic.
// "live" = pure ccwrap-side hot rebind, no Claude restart.
// "needs_relaunch" = the transition changes what Claude's own process
// must hold (the narrow exception: hidden/CCWRAP-owned → first-party
// passthrough), so a fresh session is required.
type RelaunchClass string

const (
	RelaunchLive          RelaunchClass = "live"
	RelaunchNeedsRelaunch RelaunchClass = "needs_relaunch"
)

const (
	ModelAliasDisabled ModelAliasMode = "disabled"
	ModelAliasRewrite  ModelAliasMode = "rewrite"
)

const (
	StreamStateHTTP      StreamState = "http"
	StreamStateSSE       StreamState = "sse"
	StreamStateWebSocket StreamState = "websocket"
	StreamStateMultipart StreamState = "multipart"
	StreamStateUnknown   StreamState = "unknown"
)

type EgressConfig struct {
	Mode       string `json:"mode,omitempty"`
	HTTPProxy  string `json:"http_proxy,omitempty"`
	HTTPSProxy string `json:"https_proxy,omitempty"`
	NoProxy    string `json:"no_proxy,omitempty"`
	Source     string `json:"source,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

type ModelAliasConfig struct {
	Mode                     ModelAliasMode    `json:"mode,omitempty"`
	Source                   string            `json:"source,omitempty"`
	Strict                   bool              `json:"strict,omitempty"`
	ProviderModelPassthrough bool              `json:"provider_model_passthrough,omitempty"`
	AliasCount               int               `json:"alias_count,omitempty"`
	Fingerprint              string            `json:"fingerprint,omitempty"`
	Forward                  map[string]string `json:"forward,omitempty"`
}

type StatusResponse struct {
	SchemaVersion    string    `json:"schema_version,omitempty"`
	Health           string    `json:"health"`
	StartedAt        time.Time `json:"started_at"`
	Timestamp        time.Time `json:"timestamp"`
	RuntimeDir       string    `json:"runtime_dir"`
	StateDir         string    `json:"state_dir"`
	SocketPath       string    `json:"socket_path"`
	SessionID        string    `json:"session_id,omitempty"`
	ProxyListenAddr  string    `json:"proxy_listen_addr,omitempty"`
	RecentErrorCount int       `json:"recent_error_count"`
}

type Session struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ProjectDir is the working directory Claude Code was launched in.
	ProjectDir string     `json:"project_dir,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	// LastActivityAt is the newest request, error or trace entry's time.
	LastActivityAt                     *time.Time        `json:"last_activity_at,omitempty"`
	State                              SessionState      `json:"state"`
	Health                             SessionHealth     `json:"session_health"`
	SupervisorPID                      int               `json:"supervisor_pid,omitempty"`
	LauncherPID                        int               `json:"launcher_pid,omitempty"`
	ClaudePID                          int               `json:"claude_pid,omitempty"`
	ClaudeStartToken                   string            `json:"claude_start_token,omitempty"`
	ProxyListenAddr                    string            `json:"proxy_listen_addr"`
	APIBaseURL                         string            `json:"api_base_url,omitempty"`
	RouteClass                         RouteClass        `json:"route_class,omitempty"`
	RouteSource                        RouteSource       `json:"route_source"`
	RouteConfigSource                  string            `json:"route_config_source,omitempty"`
	AuthMode                           AuthMode          `json:"auth_mode"`
	AuthSource                         AuthSource        `json:"auth_source"`
	AuthConfigSource                   string            `json:"auth_config_source,omitempty"`
	AuthPolicy                         AuthPolicy        `json:"auth_policy,omitempty"`
	AuthBootstrap                      AuthBootstrap     `json:"auth_bootstrap,omitempty"`
	AuthBootstrapKind                  AuthBootstrapKind `json:"auth_bootstrap_kind,omitempty"`
	ExactUpstreamHost                  string            `json:"exact_upstream_host,omitempty"`
	ExactUpstreamBase                  string            `json:"exact_upstream_base,omitempty"`
	EgressMode                         string            `json:"egress_mode,omitempty"`
	EgressSource                       string            `json:"egress_source,omitempty"`
	EgressSummary                      string            `json:"egress_summary,omitempty"`
	ModelAliasMode                     ModelAliasMode    `json:"model_alias_mode,omitempty"`
	ModelAliasCount                    int               `json:"model_alias_count,omitempty"`
	ModelAliasSource                   string            `json:"model_alias_source,omitempty"`
	ModelAliasStrict                   bool              `json:"model_alias_strict,omitempty"`
	ModelAliasProviderModelPassthrough bool              `json:"model_alias_provider_model_passthrough,omitempty"`
	ModelAliasFingerprint              string            `json:"model_alias_fingerprint,omitempty"`
	ModelAliasForward                  map[string]string `json:"model_alias_forward,omitempty"`
	UpstreamHeaderCount                int               `json:"upstream_header_count,omitempty"`
	UpstreamHeaderSource               string            `json:"upstream_header_source,omitempty"`
	UpstreamHeaderFingerprint          string            `json:"upstream_header_fingerprint,omitempty"`
	FailPolicy                         FailPolicy        `json:"fail_policy"`
	RecentErrorCount                   int               `json:"recent_error_count"`
	RecentRequestCount                 int               `json:"recent_request_count"`
	// RecentFailedCount counts the model requests (POST /v1/messages) among
	// the recent ones that ended with an error status.
	RecentFailedCount int `json:"recent_failed_count"`
	// UnmappedModels are the Claude models this session sent with no alias
	// on its current route, first seen first. A route change keeps only the
	// ones the new route still leaves unaliased.
	UnmappedModels        []string `json:"unmapped_models,omitempty"`
	ActiveProfileName     string   `json:"active_profile_name,omitempty"`
	ActiveProfileProvider string   `json:"active_profile_provider,omitempty"`
	// Issues are the problems the session ran into, in the order they first
	// opened; Health is the worst open one's severity. Never modified in
	// place: the supervisor replaces the slice.
	Issues []Issue `json:"issues,omitempty"`
	// CaptureBodies mirrors the per-session captureBodies routing flag so the
	// Bodies cell can show on/off and the runtime capture toggle
	// (POST /api/v1/capture) has a state to report. Launch-time initial value
	// comes from SessionRouteRequest.CaptureRequestBodies (set by the CCWRAP
	// launcher from --capture-request-bodies / CCWRAP_CAPTURE_BODIES); runtime
	// changes via Supervisor.SetCaptureBodies emit session_updated events
	// with this field flipped.
	CaptureBodies bool `json:"capture_bodies,omitempty"`
	// CaptureTelemetry mirrors the per-session captureTelemetry routing flag
	// (the opt-in transparent telemetry MITM) into the public projection so the
	// inspect ribbon + status reflect it; flips via Supervisor.SetCaptureTelemetry.
	CaptureTelemetry bool `json:"capture_telemetry,omitempty"`
	// NativeTLS surfaces the native-fingerprint TLS state the user opted into:
	// "" / "off" (feature inactive), "active" (CC's captured ClientHello is being
	// mirrored upstream — parity is being delivered), or "blocked: <reason>" (the
	// mirror could not be produced, so ccwrap BLOCKED the request fail-closed
	// rather than emit a de-anonymizing stdlib fingerprint — no request was sent).
	// A blocked state drives session Health to error so the loss of parity (and
	// the failing requests) is visible. This is a client property, preserved
	// across a profile switch by publishPosture.
	NativeTLS string `json:"native_tls,omitempty"`
	// NativeTLSLoaded is true when the mirrored hello was LOADED from
	// CCWRAP_NATIVE_TLS_HELLO, not captured from the live client. A client
	// property, preserved across a profile switch by publishPosture (sticky,
	// like NativeTLS).
	NativeTLSLoaded bool `json:"native_tls_loaded,omitempty"`
	// NativeTLSFallbacks counts native-TLS block EPISODES (transitions into a
	// blocked state) over the session's lifetime — NOT blocked dials, so a
	// persistent outage is one episode, not one-per-dial. Monotonic; surfaced
	// alongside NativeTLS so a single transient block is distinguishable from a
	// recurring one.
	NativeTLSFallbacks int `json:"native_tls_fallbacks,omitempty"`
	// CaptureBodiesUnmasked is set when the supervisor was launched with
	// CCWRAP_UNMASK_CREDENTIALS=1. The flag is
	// process-wide and immutable for the session; it bypasses the bodyredact
	// JSON-field redaction so OAuth refresh_token / access_token values
	// appear in plaintext in inspect drawer and spill files. The inspect
	// ribbon's Bodies cell renders a persistent danger-color "UNMASKED"
	// marker when this is true so the user does not forget the env flag.
	CaptureBodiesUnmasked bool `json:"capture_bodies_unmasked,omitempty"`
	// MissingAuthEnv names the env var the active profile asked ccwrap to
	// bootstrap auth from when no value was found. Empty in two cases:
	// (a) auth is fine (the common case) — distinguish via AuthBootstrap
	// != AuthBootstrapMissing; (b) AuthBootstrap == AuthBootstrapMissing
	// but the profile didn't name a specific env var (Case B — no auth
	// source configured at all). The UI branches on emptiness:
	//   AuthBootstrap=Missing, MissingAuthEnv="X"  → "needs $X"
	//   AuthBootstrap=Missing, MissingAuthEnv=""   → "no auth source configured"
	//
	// Fail-closed is enforced at forward-time rather than launch-time, so
	// AuthBootstrap==Missing is observable in a running session (launch
	// succeeds; requests fail).
	MissingAuthEnv string `json:"missing_auth_env,omitempty"`
}

type SessionsResponse struct {
	SchemaVersion string    `json:"schema_version,omitempty"`
	Sessions      []Session `json:"sessions"`
}

type RequestsResponse struct {
	SchemaVersion string          `json:"schema_version,omitempty"`
	Requests      []RequestRecord `json:"requests"`
}

type ErrorsResponse struct {
	SchemaVersion string        `json:"schema_version,omitempty"`
	Errors        []ErrorRecord `json:"errors"`
}

type TraceResponse struct {
	SchemaVersion string        `json:"schema_version,omitempty"`
	Trace         []TraceRecord `json:"trace"`
}

type SessionCreateRequest struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	ProjectDir  string `json:"project_dir,omitempty"`
	LauncherPID int    `json:"launcher_pid"`
}

type SessionCreateResponse struct {
	SchemaVersion string  `json:"schema_version,omitempty"`
	Session       Session `json:"session"`
}

type AuthOverride struct {
	Mode              AuthMode          `json:"mode"`
	Source            AuthSource        `json:"source"`
	HeaderName        string            `json:"header_name"`
	HeaderValue       string            `json:"header_value"`
	AdditionalHeaders map[string]string `json:"additional_headers,omitempty"`
}

// SessionRouteRequest is the /route control-op payload, RETAINED AS TEST-FIXTURE
// INFRA: production launch + SwitchProfile publish a routing posture directly
// from a *preflight.Result (supervisor.newResolved), so no production caller
// constructs this type. The supervisor's setRoute handler + control.Client.SetRoute
// remain so the test suite can configure a session's posture from a literal
// request (supervisor.resolvedFromRequest mirrors newResolved field-for-field).
type SessionRouteRequest struct {
	APIBaseURL                string            `json:"api_base_url"`
	RouteClass                RouteClass        `json:"route_class,omitempty"`
	RouteSource               RouteSource       `json:"route_source"`
	RouteConfigSource         string            `json:"route_config_source,omitempty"`
	AuthMode                  AuthMode          `json:"auth_mode"`
	AuthSource                AuthSource        `json:"auth_source"`
	AuthConfigSource          string            `json:"auth_config_source,omitempty"`
	AuthPolicy                AuthPolicy        `json:"auth_policy,omitempty"`
	AuthBootstrap             AuthBootstrap     `json:"auth_bootstrap,omitempty"`
	AuthBootstrapKind         AuthBootstrapKind `json:"auth_bootstrap_kind,omitempty"`
	ExactUpstreamHost         string            `json:"exact_upstream_host,omitempty"`
	ExactUpstreamBase         string            `json:"exact_upstream_base,omitempty"`
	FailPolicy                FailPolicy        `json:"fail_policy"`
	OverrideAuth              *AuthOverride     `json:"override_auth,omitempty"`
	Egress                    EgressConfig      `json:"egress,omitempty"`
	ModelAlias                ModelAliasConfig  `json:"model_alias,omitempty"`
	UpstreamHeaders           map[string]string `json:"upstream_headers,omitempty"`
	UpstreamHeaderSource      string            `json:"upstream_header_source,omitempty"`
	UpstreamHeaderFingerprint string            `json:"upstream_header_fingerprint,omitempty"`
	CaptureRequestBodies      bool              `json:"capture_request_bodies,omitempty"`
	CaptureTelemetry          bool              `json:"capture_telemetry,omitempty"`
	NativeTLS                 bool              `json:"native_tls,omitempty"`
	// NativeTLSHello carries loaded ClientHello bytes that override live
	// capture; nil = capture the live client.
	NativeTLSHello        []byte `json:"native_tls_hello,omitempty"`
	ActiveProfileName     string `json:"active_profile_name,omitempty"`
	ActiveProfileProvider string `json:"active_profile_provider,omitempty"`
	// MissingAuthEnv mirrors preflight.Result.MissingAuthEnv across the
	// control socket so the supervisor session can carry the detail for UI
	// rendering. Empty in healthy / Case B; non-empty for Case A.
	MissingAuthEnv string `json:"missing_auth_env,omitempty"`
}

type SessionAttachRequest struct {
	ClaudePID        int    `json:"claude_pid"`
	ClaudeStartToken string `json:"claude_start_token,omitempty"`
}

type SessionCloseRequest struct {
	Reason string `json:"reason,omitempty"`
}

type RequestRecord struct {
	Timestamp          time.Time       `json:"timestamp"`
	SessionID          string          `json:"session_id"`
	Method             string          `json:"method"`
	Synthetic          bool            `json:"synthetic,omitempty"`
	LogicalTargetHost  string          `json:"logical_target_host"`
	ActualUpstreamHost string          `json:"actual_upstream_host"`
	Path               string          `json:"path"`
	StatusCode         int             `json:"status_code"`
	LatencyMS          int64           `json:"latency_ms"`
	StreamState        StreamState     `json:"stream_state"`
	RequestHeaders     http.Header     `json:"request_headers,omitempty"`
	BodyRef            *RequestBodyRef `json:"body_ref,omitempty"`
	// UpstreamHeaders is what ccwrap did to RequestHeaders on the way
	// upstream; nil when the request did not go through ccwrap's rewrite
	// (tunnels, synthetic answers, telemetry).
	UpstreamHeaders *HeaderDiff `json:"upstream_headers,omitempty"`
	// UpstreamBodyRef is the body as it lands on the wire to the upstream
	// — captured AFTER all in-supervisor request mutators (modelalias
	// rewrite, system-block stripping). Set ONLY when capture is on AND
	// the rewrite pipeline actually changed the body; nil means "same as
	// BodyRef" (no ccwrap-side mutation occurred). The inspector drawer
	// surfaces a tab between client view (BodyRef) and upstream view.
	UpstreamBodyRef *RequestBodyRef `json:"upstream_body_ref,omitempty"`
	// ResponseBodyRef is the captured upstream RESPONSE body. It is spilled on
	// the Anthropic API MITM path when body capture is on (the streaming tee),
	// and on the transparent telemetry-MITM path when telemetry capture is on.
	// Nil when neither capture is active for this request.
	ResponseBodyRef *RequestBodyRef `json:"response_body_ref,omitempty"`
	// Identity captured at request time from the immutable *activePosture
	// so post-switch records carry the posture they ran under — never the
	// latest posture (timeline correctness).
	ActiveProfileName     string `json:"active_profile_name,omitempty"`
	ActiveProfileProvider string `json:"active_profile_provider,omitempty"`
	// ClientHelloKey names the ClientHello shape (GET /native-tls "stacks")
	// this request's upstream connection mirrored. Empty when native TLS did
	// not apply or the session-default hello was used.
	ClientHelloKey string `json:"client_hello_key,omitempty"`
	// Model is the model Claude Code asked for and UpstreamModel the one the
	// upstream received; they differ only under a model alias. Empty when
	// the request names no model.
	Model         string `json:"model,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	// ModelUnmapped marks a Claude model sent as-is to a third-party route
	// whose aliases map other models but not this one.
	ModelUnmapped bool `json:"model_unmapped,omitempty"`
	// Failure explains a request that ended with an error status. Recorded
	// whether or not body capture is on.
	Failure *RequestFailure `json:"failure,omitempty"`
}

// HeaderDiff lists the request headers ccwrap changed on the way upstream,
// by canonical name; every other header went as Claude Code sent it.
type HeaderDiff struct {
	Changes []HeaderChange `json:"changes"`
}

// HeaderChange is one header ccwrap rewrote, injected or stripped. It
// carries no value: the ones ccwrap sets are keys and configured upstream
// headers, which never leave the proxy, and Claude Code's own value is in
// RequestHeaders.
type HeaderChange struct {
	Name   string       `json:"name"`
	Action HeaderAction `json:"action"`
}

type HeaderAction string

const (
	HeaderRewritten HeaderAction = "rewritten"
	HeaderInjected  HeaderAction = "injected"
	HeaderStripped  HeaderAction = "stripped"
)

// RequestFailure is what went wrong with one request, as far as ccwrap can
// tell: the error the upstream returned, or ccwrap's own reason when no
// upstream response arrived.
type RequestFailure struct {
	// Source is "upstream" (it answered with an error status) or "ccwrap"
	// (no upstream response, e.g. unreachable).
	Source string `json:"source"`
	// Type and Message come from the error body when it has a known shape.
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
	// Body is the raw error body, credential-masked and capped; Truncated
	// marks a cut-off body.
	Body      string `json:"body,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Hint is ccwrap's suggestion for the user.
	Hint string `json:"hint,omitempty"`
	// Issue is the session issue this failure counts toward. Set in the
	// supervisor and read there only, so it is not on the wire.
	Issue string `json:"-"`
}

// RequestBodyRef is the in-ring reference to a request body spilled to a
// per-session file. The 250-ring carries only this, never the bytes.
// Truncated is set true on the telemetry-capture path when a captured body
// exceeds telemetryBodyCapBytes and is stored truncated; the (uncapped)
// request-body path leaves it false.
type RequestBodyRef struct {
	ID         string    `json:"id"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	CapturedAt time.Time `json:"captured_at"`
	Truncated  bool      `json:"truncated"`
}

// MethodLabel returns the display label for the request's HTTP method.
// Synthetic responses surface as "SYNTHETIC" so UI consumers don't lose
// the visual cue, while the underlying Method field stores the real
// HTTP verb so programmatic filters by method behave correctly.
func (r RequestRecord) MethodLabel() string {
	if r.Synthetic {
		return "SYNTHETIC"
	}
	if r.Method == "" {
		return "REQUEST"
	}
	return r.Method
}

// ModelRequest reports a request Claude Code sent for a model turn: POST
// /v1/messages, not one ccwrap answered itself.
func (r RequestRecord) ModelRequest() bool {
	if r.Synthetic || !strings.EqualFold(r.Method, http.MethodPost) {
		return false
	}
	path, _, _ := strings.Cut(r.Path, "?")
	return strings.TrimRight(path, "/") == "/v1/messages"
}

// FailedModelRequest reports a model request that ended with an error
// status — what Session.RecentFailedCount counts. Other API calls can fail
// harmlessly (a gateway without count_tokens, say), so they show their
// status but are not counted.
func (r RequestRecord) FailedModelRequest() bool {
	return r.StatusCode >= 400 && r.ModelRequest()
}

type ErrorRecord struct {
	Timestamp       time.Time `json:"timestamp"`
	SessionID       string    `json:"session_id"`
	Severity        string    `json:"severity"`
	ErrorClass      string    `json:"error_class"`
	Summary         string    `json:"summary"`
	UpstreamHost    string    `json:"upstream_host,omitempty"`
	RetryState      string    `json:"retry_state,omitempty"`
	SuggestedAction string    `json:"suggested_action,omitempty"`
	// Count is how many times this same error repeated in a row (Claude
	// Code retries failed requests); 0 or 1 means once.
	Count int `json:"count,omitempty"`
}

type TraceRecord struct {
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"session_id"`
	Category  string    `json:"category"`
	Summary   string    `json:"summary"`
	Detail    string    `json:"detail,omitempty"`
}

type Event struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Time      time.Time   `json:"time"`
	SessionID string      `json:"session_id,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

type DoctorReport struct {
	Overall string        `json:"overall"`
	Checks  []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Detail  string         `json:"detail,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type SessionManifest struct {
	SessionID                          string            `json:"session_id"`
	CreatedAt                          time.Time         `json:"created_at"`
	UpdatedAt                          time.Time         `json:"updated_at"`
	State                              SessionState      `json:"state"`
	SupervisorPID                      int               `json:"supervisor_pid"`
	SupervisorStartToken               string            `json:"supervisor_start_token,omitempty"`
	ClaudePID                          int               `json:"claude_pid,omitempty"`
	Name                               string            `json:"name,omitempty"`
	ProjectDir                         string            `json:"project_dir,omitempty"`
	ControlSocket                      string            `json:"control_socket"`
	ProxyListenAddr                    string            `json:"proxy_listen_addr,omitempty"`
	RouteClass                         RouteClass        `json:"route_class,omitempty"`
	RouteSource                        RouteSource       `json:"route_source,omitempty"`
	RouteConfigSource                  string            `json:"route_config_source,omitempty"`
	ExactUpstreamBase                  string            `json:"exact_upstream_base,omitempty"`
	AuthMode                           AuthMode          `json:"auth_mode,omitempty"`
	AuthSource                         AuthSource        `json:"auth_source,omitempty"`
	AuthConfigSource                   string            `json:"auth_config_source,omitempty"`
	AuthPolicy                         AuthPolicy        `json:"auth_policy,omitempty"`
	AuthBootstrap                      AuthBootstrap     `json:"auth_bootstrap,omitempty"`
	AuthBootstrapKind                  AuthBootstrapKind `json:"auth_bootstrap_kind,omitempty"`
	EgressMode                         string            `json:"egress_mode,omitempty"`
	EgressSource                       string            `json:"egress_source,omitempty"`
	EgressSummary                      string            `json:"egress_summary,omitempty"`
	ModelAliasMode                     ModelAliasMode    `json:"model_alias_mode,omitempty"`
	ModelAliasCount                    int               `json:"model_alias_count,omitempty"`
	ModelAliasSource                   string            `json:"model_alias_source,omitempty"`
	ModelAliasProviderModelPassthrough bool              `json:"model_alias_provider_model_passthrough,omitempty"`
	ModelAliasFingerprint              string            `json:"model_alias_fingerprint,omitempty"`
}

type DiscoveredSession struct {
	Manifest  SessionManifest `json:"manifest"`
	Reachable bool            `json:"reachable"`
	Stale     bool            `json:"stale"`
	Error     string          `json:"error,omitempty"`
}
