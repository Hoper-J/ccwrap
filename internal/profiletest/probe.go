// Package profiletest implements a CLI-driven end-to-end probe of a
// ccwrap profile: POST /v1/messages with max_tokens=1 against the
// configured upstream, then classify the response. The probe is
// direct in-process — it does not depend on a running ccwrap session
// and does not appear in the inspect web /recent timeline.
package profiletest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// userinfoRE matches user[:pass]@ in URLs embedded in Go's net/url
// error strings (e.g. `Post "http://u:p@host:1": dial tcp ...`).
var userinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/\s"@]+@`)

// sanitizeNetErr strips any embedded URL userinfo from a Go net error
// string. The bare scheme survives; the credential vanishes.
func sanitizeNetErr(s string) string {
	return userinfoRE.ReplaceAllString(s, "$1")
}

// ProbeStatus is the classified outcome of a single probe.
type ProbeStatus int

const (
	StatusOK ProbeStatus = iota
	StatusSkipped
	StatusAuthFail
	StatusModel404
	StatusHTTP4xx
	StatusHTTP5xx
	StatusTimeout
	StatusNetFail
)

func (s ProbeStatus) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusSkipped:
		return "SKIPPED"
	case StatusAuthFail:
		return "AUTH_FAIL"
	case StatusModel404:
		return "MODEL_404"
	case StatusHTTP4xx:
		return "HTTP_4XX"
	case StatusHTTP5xx:
		return "HTTP_5XX"
	case StatusTimeout:
		return "TIMEOUT"
	case StatusNetFail:
		return "NET_FAIL"
	}
	return "UNKNOWN"
}

// IsFailure reports whether this status should contribute to a
// non-zero process exit code. OK and SKIPPED do not.
func (s ProbeStatus) IsFailure() bool {
	switch s {
	case StatusOK, StatusSkipped:
		return false
	}
	return true
}

// ProbeOptions controls a single probe.
type ProbeOptions struct {
	// Model, if non-empty, overrides the auto-resolved probe model.
	// When set, alias rewriting is bypassed (the literal value goes
	// into the request body's "model" field).
	Model string
	// Timeout caps the entire probe (DNS+connect+TLS+request+response).
	Timeout time.Duration
}

// ProbeResult is what one probe returns.
type ProbeResult struct {
	Profile              string
	Status               ProbeStatus
	Latency              time.Duration
	HTTPStatus           int    // 0 if no response
	BaseURLHost          string // host only, no userinfo
	ModelSent            string // the model field actually transmitted
	ModelSentRewroteFrom string // empty unless alias rewrite happened
	ModelEchoed          string // upstream response body's model field
	Err                  string // single-line summary
	SkippedReason        string // populated when Status == StatusSkipped
}

const defaultProbeModel = "claude-haiku-4-5-20251001"
const probeContent = "ping"

// probeModelFromAliases resolves which alias target the probe should send.
// Profiles may key a haiku alias by the bare "haiku" OR by the full Claude id
// the proxy actually rewrites (e.g. "claude-haiku-4-5-20251001"); honor both,
// else a profile that only carries the full-id key would send the raw Anthropic
// id to the gateway and false-report MODEL_404 for a profile that works in real
// traffic. Bare "haiku" wins; otherwise the first claude-haiku* key in sorted
// order (deterministic across multiple keys). Returns ("","") when the profile
// has no haiku alias.
func probeModelFromAliases(aliases map[string]string) (key, target string) {
	if v, ok := aliases["haiku"]; ok && v != "" {
		return "haiku", v
	}
	keys := make([]string, 0, len(aliases))
	for k := range aliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if aliases[k] != "" && strings.HasPrefix(strings.ToLower(k), "claude-haiku") {
			return k, aliases[k]
		}
	}
	return "", ""
}

// Probe runs a single probe against one profile. The result is
// always non-nil and never panics. Callers may run Probe concurrently
// for different profiles.
//
// A profile with no auth block (profile.Auth == nil) means "ccwrap does
// not own auth": there is no credential the probe can inject, so the
// probe short-circuits to SKIPPED, the same disposition as a
// passthrough profile.
func Probe(profile profiles.Profile, opts ProbeOptions) ProbeResult {
	res := ProbeResult{Profile: profile.Name, BaseURLHost: hostOnly(profile.BaseURL)}

	if profile.Auth == nil || strings.EqualFold(strings.TrimSpace(profile.Auth.Mode), "passthrough") {
		res.Status = StatusSkipped
		res.SkippedReason = "passthrough: CCWRAP does not own credential"
		return res
	}

	model := opts.Model
	rewroteFrom := ""
	if model == "" {
		if k, v := probeModelFromAliases(profile.ModelAliases); v != "" {
			model = v
			rewroteFrom = k
		} else {
			model = defaultProbeModel
		}
	}

	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": probeContent}},
	})
	if err != nil {
		res.Status = StatusNetFail
		res.Err = "marshal body: " + err.Error()
		return res
	}
	res.ModelSent = model
	res.ModelSentRewroteFrom = rewroteFrom

	endpoint := strings.TrimRight(profile.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		res.Status = StatusNetFail
		res.Err = "build request: " + err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	stampProfileAuth(req, profile)
	stampProfileUpstreamHeaders(req, profile)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// Upstream profile probe carries no posture context; NoProxy stays
	// at the env-default (which Resolve's explicit-flag branch drops,
	// so this caller doesn't honor NO_PROXY for mode=http profiles).
	// The egress-self-test path (ProbeEgress) is where the supervisor
	// passes its posture's NoProxy explicitly via opts.
	client := buildEgressAwareHTTPClient(profile.Egress, "", timeout)

	start := time.Now()
	resp, err := client.Do(req)
	res.Latency = time.Since(start)

	var respBody []byte
	if resp != nil {
		defer resp.Body.Close()
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		res.HTTPStatus = resp.StatusCode
	}

	res.Status, res.Err = classifyProbeResult(resp, respBody, err)
	res.Err = sanitizeNetErr(res.Err)
	if res.Status == StatusOK {
		var parsed struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(respBody, &parsed)
		res.ModelEchoed = parsed.Model
	}
	return res
}

// stampProfileAuth sets the upstream auth header from a profile's
// AuthSpec. Key source precedence:
//  1. inline Auth.Key (the default)
//  2. os.Getenv(Auth.KeyEnv) (legacy env-ref)
//  3. empty — request goes out with no credential; upstream returns
//     401, which is the right diagnostic.
//
// p.Auth == nil never reaches here; caller short-circuits to SKIPPED.
func stampProfileAuth(req *http.Request, p profiles.Profile) {
	if p.Auth == nil {
		return
	}
	key := p.Auth.Key
	if key == "" && p.Auth.KeyEnv != "" {
		key = os.Getenv(p.Auth.KeyEnv)
	}
	switch strings.ToLower(strings.TrimSpace(p.Auth.Mode)) {
	case "ccwrap_x_api_key":
		req.Header.Set("X-Api-Key", key)
	case "ccwrap_bearer":
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// stampProfileUpstreamHeaders applies the profile's upstream_headers
// to the probe request. Defaults for Anthropic-Version and User-Agent
// are filled when the profile does not override them.
func stampProfileUpstreamHeaders(req *http.Request, p profiles.Profile) {
	for k, v := range p.UpstreamHeaders {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Anthropic-Version") == "" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "github.com/Hoper-J/ccwrap/profile-test")
	}
}

func hostOnly(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	if h := u.Hostname(); h != "" {
		return h
	}
	return u.Host
}

// buildEgressAwareHTTPClient constructs a one-shot http.Client whose
// transport honors the profile's EgressSpec (direct / http / socks5 /
// socks5h). Used by both Probe (upstream) and ProbeEgress (egress).
//
// DisableKeepAlives is set because probes are single-use; keeping idle
// SOCKS5/HTTP tunnel sockets alive serves no purpose and risks holding
// file descriptors for the lifetime of the http.Client GC.
//
// The SOCKS/HTTP dispatch below probes IsSOCKSEgress via the "https"
// proxy lookup. This is safe today because egress.Resolve populates
// both HTTPProxy and HTTPSProxy from the same flag-or-env value, so the
// two paths are equivalent. A future flag shape that supports
// scheme-asymmetric proxy URLs (HTTP-only SOCKS, etc.) will need this
// dispatch reworked to take the request URL into account.
//
// noProxy overlays a NO_PROXY rule list onto the resolved EgressConfig.
// Required because egress.Resolve's "default" branch (when a flag-shaped
// URL is supplied) builds an EgressConfig with NoProxy="" — it never
// reads env. resolveFromEnv DOES populate NoProxy, but that branch only
// runs for empty/"auto" flag values. So a probe constructed from a flag
// URL (the typical posture-substitution case) needs the caller to pass
// NoProxy explicitly, otherwise the probe routes through the proxy for
// hosts the forward path would bypass via shouldBypass.
func buildEgressAwareHTTPClient(spec profiles.EgressSpec, noProxy string, timeout time.Duration) *http.Client {
	flagVal := profileEgressToTransportFlag(spec)
	cfg, _, _ := egress.Resolve(flagVal, envMap())
	if strings.TrimSpace(noProxy) != "" {
		cfg.NoProxy = noProxy
	}

	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	if egress.IsSOCKSEgress(cfg) {
		transport.DialContext = egress.DialContextFunc(cfg)
	} else {
		transport.Proxy = egress.ProxyFunc(cfg)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// Do not follow redirects: the probe must measure the CONFIGURED
		// target's exit, not a redirected hop. A redirect to an internal
		// host would be dialed DIRECTLY by the bypass floor, letting a
		// malicious target steer the probe to arbitrary internal URLs and
		// report the redirect's IP. Return the redirect response as-is so
		// the classifier sees the original hop's status.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// profileEgressToTransportFlag is the single source of truth for
// converting a profile's EgressSpec to the flag-shaped string accepted
// by internal/egress.Resolve. Used by both Probe (upstream) and
// ProbeEgress (egress self-test).
//
// The "case http, socks5, socks5h" branch carries the spec URL through
// for all proxy modes; only those modes are forwarded. The default
// branch maps genuinely unknown modes to empty, falling back to the env.
func profileEgressToTransportFlag(spec profiles.EgressSpec) string {
	switch strings.TrimSpace(strings.ToLower(spec.Mode)) {
	case "direct":
		return "direct"
	case "http", "socks5", "socks5h":
		return strings.TrimSpace(spec.URL)
	default:
		return ""
	}
}

// envMap snapshots os.Environ() into a map for egress.Resolve.
func envMap() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	return out
}

// classifyProbeResult maps a (response, body, err) triple into a
// ProbeStatus and a single-line error summary. Pure function; no
// I/O; callers do the request and pass results in. This is the
// upstream-AUTH-probe classifier: it interprets 401/403 as AUTH_FAIL and a
// 404 mentioning "model" as MODEL_404 because the auth probe sends
// credentials and a model id.
func classifyProbeResult(resp *http.Response, body []byte, err error) (ProbeStatus, string) {
	return classifyProbeResultMode(resp, body, err, false)
}

// classifyProbeResultMode is classifyProbeResult with an egressMode switch.
// When egressMode is true the auth/model special cases (401/403 → AUTH_FAIL,
// 404-with-"model" → MODEL_404) are suppressed: the egress probe sends NO
// credentials and no model id, so those 4xx codes are plain HTTP_4XX. Without
// this, an egress-only test against a self-hosted endpoint that returns 403
// would wrongly report AUTH_FAIL — "your credentials failed" when none were
// ever sent.
func classifyProbeResultMode(resp *http.Response, body []byte, err error, egressMode bool) (ProbeStatus, string) {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return StatusTimeout, "timeout: " + err.Error()
		}
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return StatusNetFail, "dns: " + dns.Err
		}
		var op *net.OpError
		if errors.As(err, &op) {
			return StatusNetFail, op.Op + ": " + op.Err.Error()
		}
		return StatusNetFail, err.Error()
	}
	if resp == nil {
		return StatusNetFail, "nil response and nil error"
	}
	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return StatusOK, ""
	case !egressMode && (code == 401 || code == 403):
		return StatusAuthFail, fmt.Sprintf("%d %s", code, http.StatusText(code))
	case !egressMode && code == 404 && strings.Contains(strings.ToLower(string(body)), "model"):
		return StatusModel404, fmt.Sprintf("%d %s", code, "model not recognized by provider")
	case code >= 400 && code < 500:
		return StatusHTTP4xx, fmt.Sprintf("%d %s", code, http.StatusText(code))
	case code >= 500 && code < 600:
		return StatusHTTP5xx, fmt.Sprintf("%d %s", code, http.StatusText(code))
	}
	return StatusHTTP4xx, fmt.Sprintf("unexpected status %d", code)
}
