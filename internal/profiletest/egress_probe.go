package profiletest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// defaultEgressTestTarget is the HTTPS endpoint probed by ProbeEgress
// when CCWRAP_EGRESS_TEST_URL is unset. ipinfo.io's /json returns
// IP + geo + ASN in a single ~300-byte response with no auth required.
const defaultEgressTestTarget = "https://ipinfo.io/json"

// envEgressTestTarget overrides the probe URL. Operators in
// privacy-strict environments can point at a self-hosted endpoint that
// returns the same shape: {ip, country, region, city, org}.
const envEgressTestTarget = "CCWRAP_EGRESS_TEST_URL"

// egressProbeUserAgent identifies probe traffic in third-party logs
// (e.g. ipinfo.io). Versionless by design — keeps the string stable
// and avoids leaking ccwrap build metadata to third parties.
const egressProbeUserAgent = "ccwrap-egress-probe"

// EgressProbeOptions controls one egress probe. Zero value uses
// defaults (5s timeout; target resolved from env or hardcoded default).
type EgressProbeOptions struct {
	// Timeout caps the entire probe (DNS+TLS+request+response).
	Timeout time.Duration

	// Target overrides both env var and hardcoded default. Used by
	// tests and by the CLI --target flag. Leave empty in normal paths.
	Target string

	// NoProxy overlays bypass rules onto the resolved transport. The
	// supervisor probe path passes posture.egress.NoProxy here so a
	// mode=inherit profile probes through the same bypass list the
	// forward path honors — egress.Resolve's default branch drops
	// NoProxy (only resolveFromEnv populates it), so without this
	// overlay a probe through a flag-shaped URL silently ignores
	// NO_PROXY hosts that the running session would bypass. Same
	// comma-separated shape as the standard NO_PROXY env var.
	NoProxy string
}

// EgressProbeResult is what one egress probe returns. Status reuses
// ProbeStatus from probe.go so consumer switch statements stay
// uniform; only OK / TIMEOUT / NET_FAIL / HTTP_4XX / HTTP_5XX are
// actually emitted (SKIPPED / AUTH_FAIL / MODEL_404 are upstream-probe
// concepts and never appear here).
type EgressProbeResult struct {
	Profile string        `json:"profile"`
	Status  ProbeStatus   `json:"status"`
	Latency time.Duration `json:"-"` // serialized via LatencyMs below
	// LatencyMs is always serialized — 0 is a real value (sub-ms probe
	// or early-failure latency before network), not "missing". Dropping
	// the field with omitempty would conflate a successful fast probe
	// with a probe that never started; both surfaces (CLI + popover)
	// would render "—" for an OK result, which users read as broken.
	LatencyMs  int64  `json:"latency_ms"`
	Target     string `json:"target"`     // URL actually probed
	EgressVia  string `json:"egress_via"` // sanitized (no userinfo)
	HTTPStatus int    `json:"http_status,omitempty"`

	// From ipinfo response body (best-effort parse on OK)
	PublicIP string `json:"public_ip,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	City     string `json:"city,omitempty"`
	Org      string `json:"org,omitempty"`

	Err string `json:"err,omitempty"`
}

// MarshalJSON renders Status as its string form (e.g. "OK", "NET_FAIL")
// so the wire schema exposes a stable string rather than the underlying
// int Go would serialize by default. Other fields use their tagged
// defaults. The Latency time.Duration is omitted from JSON via its
// `json:"-"` tag; LatencyMs is the wire-visible variant.
func (r EgressProbeResult) MarshalJSON() ([]byte, error) {
	type alias EgressProbeResult
	return json.Marshal(&struct {
		Status string `json:"status"`
		*alias
	}{
		Status: r.Status.String(),
		alias:  (*alias)(&r),
	})
}

// sanitizeProbeTarget strips userinfo (user:password@) from a probe
// target URL so credentials never reach result.Target's wire JSON,
// stdout table, or stored logs. Used for both the env-supplied URL
// (CCWRAP_EGRESS_TEST_URL) and any --target flag value.
//
// Behavior:
//   - Empty or whitespace-only input → returned unchanged (no leak).
//   - Parseable URL with userinfo → userinfo removed, rest preserved.
//   - Unparseable input → returned with any "://userinfo@" segment
//     replaced by "://", as a best-effort scrub when url.Parse fails.
//     Worst case the result is uglier than ideal but never contains
//     a credential pair.
func sanitizeProbeTarget(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}
	// Only treat the input as a URL when it parses AND has a scheme —
	// url.Parse is permissive ("not a url" becomes &URL{Path:"not a url"})
	// and calling .String() back on the result would percent-encode
	// spaces, surprising users with a "scrubbed" non-URL input. Without
	// a scheme there's no userinfo to strip.
	if u, err := url.Parse(s); err == nil && u != nil && u.Scheme != "" {
		u.User = nil
		return u.String()
	}
	// Fallback for shape-but-not-parseable inputs: any "scheme://user@"
	// or "://user@" segment gets byte-scrubbed so a leak doesn't survive.
	if idx := strings.Index(s, "://"); idx >= 0 {
		rest := s[idx+3:]
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			// Drop everything between "://" and the first "@".
			s = s[:idx+3] + rest[at+1:]
		}
	}
	return s
}

// sanitizedEgressDescriptor returns a human-readable string of the
// egress config with any URL userinfo stripped.
//
//	direct                 → "direct"
//	inherit / empty        → "inherit"
//	http://u:p@host:port   → "http://host:port"
//	socks5h://host:port    → "socks5h://host:port"
//	<malformed>            → bare mode string
func sanitizedEgressDescriptor(spec profiles.EgressSpec) string {
	mode := strings.ToLower(strings.TrimSpace(spec.Mode))
	switch mode {
	case "", "inherit":
		return "inherit"
	case "direct":
		return "direct"
	}
	u, err := url.Parse(strings.TrimSpace(spec.URL))
	if err != nil || u == nil || u.Scheme == "" {
		return mode
	}
	u.User = nil
	return u.String()
}

// parseIPInfoJSON unmarshals the ipinfo-shape body into res. Returns
// true when at least the ip field was populated. Caller surfaces a
// non-fatal note when this returns false (status stays OK; shape
// mismatch is informational, not an error).
func parseIPInfoJSON(body []byte, res *EgressProbeResult) bool {
	var parsed struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		Region  string `json:"region"`
		City    string `json:"city"`
		Org     string `json:"org"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	res.PublicIP = parsed.IP
	res.Country = parsed.Country
	res.Region = parsed.Region
	res.City = parsed.City
	res.Org = parsed.Org
	return parsed.IP != ""
}

// ProbeEgress runs one HTTPS GET against the resolved probe target,
// routed through profile.Egress. Result is always non-nil; never panics.
// Safe for concurrent use across different profiles.
//
// Target resolution precedence:
//  1. opts.Target (test / CLI --target)
//  2. os.Getenv(envEgressTestTarget)
//  3. defaultEgressTestTarget
//
// Profile.Auth is intentionally ignored — egress probe sends NO
// credentials. profile.BaseURL is ignored too; the target is always
// the egress-test URL.
//
// Treatment of profile.Egress.Mode:
//   - "direct"             → no proxy (explicit bypass of env)
//   - "http"               → HTTP CONNECT via profile.Egress.URL
//   - "socks5", "socks5h"  → SOCKS via profile.Egress.URL
//   - "inherit" or unset   → resolves through the calling process's
//     environment via egress.Resolve (HTTPS_PROXY, HTTP_PROXY,
//     ALL_PROXY, NO_PROXY). With no proxy env set, this is direct.
//     With env set, the probe traverses that proxy — the same
//     traffic-shape the caller would inherit at runtime. Callers
//     wanting an explicit direct bypass must use mode="direct".
//   - anything else        → same env-resolution path as inherit
//     (forward-compat; unknown modes do not crash but they DO follow
//     env, so they are not equivalent to "direct")
//
// Latency and LatencyMs are both populated at every result-return
// point; callers should not need to derive one from the other.
func ProbeEgress(profile profiles.Profile, opts EgressProbeOptions) EgressProbeResult {
	target := opts.Target
	if target == "" {
		target = strings.TrimSpace(os.Getenv(envEgressTestTarget))
	}
	if target == "" {
		target = defaultEgressTestTarget
	}

	res := EgressProbeResult{
		Profile: profile.Name,
		// res.Target is wire-visible (JSON / CLI table / popover). Strip
		// userinfo before storing — the actual request below still uses
		// the credentialed URL for authentication, but the result never
		// leaks credentials into logs, stdout, or the dashboard.
		Target:    sanitizeProbeTarget(target),
		EgressVia: sanitizedEgressDescriptor(profile.Egress),
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := buildEgressAwareHTTPClient(profile.Egress, opts.NoProxy, timeout)

	// context.WithTimeout drives early cancellation in connect/TLS phase;
	// client.Timeout is the outer belt-and-braces bound.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Latency = time.Since(start)
		res.LatencyMs = res.Latency.Milliseconds()
		res.Status = StatusNetFail
		res.Err = "build request: " + err.Error()
		return res
	}
	req.Header.Set("User-Agent", egressProbeUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, doErr := client.Do(req)
	res.Latency = time.Since(start)
	res.LatencyMs = res.Latency.Milliseconds()

	var body []byte
	if resp != nil {
		defer resp.Body.Close()
		var readErr error
		body, readErr = io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		res.HTTPStatus = resp.StatusCode
		// A successful headers-phase + failed body read (TLS error
		// mid-stream, RST after headers, truncated transfer) is a
		// network failure, not a successful response. Promote the
		// read error so classifyProbeResult routes through the
		// err-bearing branch instead of treating the response as OK.
		if doErr == nil && readErr != nil {
			doErr = readErr
		}
	}
	// egressMode=true: the probe sends no credentials, so 401/403/404 are
	// plain HTTP_4XX, never AUTH_FAIL/MODEL_404.
	res.Status, res.Err = classifyProbeResultMode(resp, body, doErr, true)
	res.Err = sanitizeNetErr(res.Err)

	if res.Status == StatusOK {
		if !parseIPInfoJSON(body, &res) {
			res.Err = "egress reachable; response body not in ipinfo schema (ip/country/region/city/org)"
		}
	}
	return res
}
