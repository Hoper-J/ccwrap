package supervisor

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/model"
)

// Per-request capture model:
//
// All forwarding handlers (forward proxy, MITM, blind tunnel) capture the
// immutable *posture at entry and route every downstream read through
// it. There are no live accessors that read session routing state under
// sess.mu.RLock; those were the torn-read hazard surface and have no callers
// in the proxy hot path. The two routing helpers in this file —
// upstreamTransport(cfg) and resolveUpstream(host, ap) — take caller-supplied
// inputs, so they never re-acquire sess.mu for routing reads.
// resolveUpstream keeps its own sess.mu.RLock for the independent
// session-ended gate on sess.public.State, which is a separate fail-safe
// boundary.

// copyUpstreamHeaders returns a defensive copy of the upstream-header map for
// the per-request rewrite closure. The captured ap.r.upstreamHeaders is
// itself immutable after publish; the copy preserves the nil-when-empty
// semantics so upstreamheaders.Apply behaves identically.
func copyUpstreamHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// upstreamTransport returns a *http.Transport keyed by the supplied
// EgressConfig, lazily creating and caching it. The egress source is a
// caller-supplied EgressConfig (the handler passes ap.r.egress) rather
// than a live read of session state.
func (sp *sessionProxy) upstreamTransport(cfg model.EgressConfig) *http.Transport {
	key := egressTransportKey(cfg)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if tr := sp.transports[key]; tr != nil {
		return tr
	}
	tr := sp.supervisor.transport.Clone()
	tr.Proxy = egress.ProxyFunc(cfg)
	if egress.IsSOCKSEgress(cfg) {
		tr.DialContext = egress.DialContextFunc(cfg)
	}
	sp.transports[key] = tr
	return tr
}

// nativeUpstreamTransport returns a SEPARATE *http.Transport for the
// native-TLS Anthropic route, cached in sp.nativeTransports keyed by the same
// egressTransportKey as the plain cache. It is NEVER the shared transport
// upstreamTransport returns: the egress moves into the dial (Proxy=nil,
// DialContext=nil) and DialTLSContext does the egress-aware utls/stdlib
// handshake, so this path must not be reused by telemetry/forward (which keep
// their Proxy/DialContext egress on the plain transport). ForceAttemptHTTP2 is
// cleared because the native handshake negotiates http/1.1. Locking mirrors
// upstreamTransport exactly (the same sp.mu gates both caches).
func (sp *sessionProxy) nativeUpstreamTransport(cfg model.EgressConfig) *http.Transport {
	key := egressTransportKey(cfg)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if tr := sp.nativeTransports[key]; tr != nil {
		return tr
	}
	tr := sp.supervisor.transport.Clone()
	tr.ForceAttemptHTTP2 = false
	tr.Proxy = nil
	tr.DialContext = nil
	tr.DialTLSContext = sp.nativeTLSDial(cfg)
	sp.nativeTransports[key] = tr
	return tr
}

// upstreamTransportFor selects the upstream transport for a logical target host.
// When native-TLS is on AND the target is ANY Anthropic host (isAnthropicHost —
// not just the API host), it returns the native fingerprint-mirroring, fail-
// closed transport, so NO *.anthropic.com dial ever carries a Go TLS fingerprint.
// Everything else (non-Anthropic hosts, native-TLS off) uses the plain transport.
// Centralizing the gate here is the single guard that keeps a new dial site from
// silently leaking a Go fingerprint to Anthropic. (Telemetry hosts are never
// Anthropic, so the telemetry MITM keeps the plain transport via this same rule.)
func (sp *sessionProxy) upstreamTransportFor(logicalHost string, cfg model.EgressConfig, nativeTLS bool) *http.Transport {
	if nativeTLS && isAnthropicHost(logicalHost) {
		return sp.nativeUpstreamTransport(cfg)
	}
	return sp.upstreamTransport(cfg)
}

func egressTransportKey(cfg model.EgressConfig) string {
	return strings.Join([]string{cfg.Mode, cfg.HTTPProxy, cfg.HTTPSProxy, cfg.NoProxy, cfg.Source}, "\x00")
}

// drainSupersededTransports drops every cached transport whose key != newKey
// (under sp.mu) and CloseIdleConnections() on each (after releasing sp.mu).
// Only the active-key transport is preserved. This is a best-effort transport
// drain; called by SwitchProfile only on egress-changing live switches.
//
// Concurrency: deletes serialize through sp.mu (same lock that gates
// upstreamTransport's get-or-create); any subsequent upstreamTransport(oldCfg)
// call from a still-in-flight captured-ap request lazy-creates a FRESH
// transport for the old key, naturally bounded and GC'd at the next drain.
// CloseIdleConnections only closes idle conns — in-flight conns on the
// original drained transport keep going to completion on their existing TCP
// connections.
func (sp *sessionProxy) drainSupersededTransports(newKey string) {
	sp.mu.Lock()
	var drained []*http.Transport
	for k, tr := range sp.transports {
		if k == newKey {
			continue
		}
		drained = append(drained, tr)
		delete(sp.transports, k)
	}
	// Drain the SEPARATE native-TLS cache the same way, so a profile switch
	// that changes egress does not leak native idle conns bound to the old
	// egress. sess.mirroredHelloRaw is untouched — the captured ClientHello
	// survives the switch (server.go documents this invariant).
	for k, tr := range sp.nativeTransports {
		if k == newKey {
			continue
		}
		drained = append(drained, tr)
		delete(sp.nativeTransports, k)
	}
	sp.mu.Unlock()
	for _, tr := range drained {
		tr.CloseIdleConnections()
	}
}

// resolveUpstream consults the request-captured ap for the apiBaseURL routing
// read while keeping its own sess.mu.RLock for the independent session-ended
// check on sess.public.State. The two reads are orthogonal: ap is immutable
// per-request, while the State gate is a separate fail-safe boundary.
func (sp *sessionProxy) resolveUpstream(logicalHost string, ap *posture) (*url.URL, string, string, error) {
	logicalHost = normalizeProxyHost(logicalHost)
	sp.session.mu.RLock()
	ended := sp.session.public.State == model.StateEnded
	sp.session.mu.RUnlock()
	if ended {
		return nil, logicalHost, "session_ended", fmt.Errorf("session already ended")
	}
	if isAnthropicAPIHost(logicalHost) && ap != nil && ap.r.apiBaseURL != nil {
		return cloneURL(ap.r.apiBaseURL), ap.r.apiBaseURL.Hostname(), "upstream_unreachable", nil
	}
	// Any other *.anthropic.com host routes to the same public host so ccwrap
	// stays transparent for Anthropic-owned traffic without per-host config.
	return publicAnthropicURL(logicalHost), logicalHost, "upstream_unreachable", nil
}

func publicAnthropicURL(host string) *url.URL {
	return &url.URL{Scheme: "https", Host: host}
}

func routeSuggestion(class string) string {
	switch class {
	case "session_ended":
		return "restart Claude via `ccwrap`"
	case "tls_mitm_failed":
		return "run `ccwrap doctor` and verify child CA env trust coverage"
	case "native_tls_blocked":
		return "native-TLS could not mirror Claude Code's fingerprint; check egress reachability and run `ccwrap doctor`. Last resort only: CCWRAP_NATIVE_TLS=0 bypasses the mirror, but exposes ccwrap's Go fingerprint and makes the session identifiable as non-native"
	default:
		return "check upstream reachability and route configuration"
	}
}

func applyAuthOverride(h http.Header, override *model.AuthOverride) {
	if override == nil {
		return
	}
	// MUST stay aligned with ui/headerclass.go::credentialDenyList:
	// every credential ccwrap strips from upstream must also be
	// redacted in inspect rendering, otherwise the same secret gets
	// removed from the forward but rendered raw in /recent. Both
	// directions are enforced by
	// routeresolve_test.go::TestApplyAuthOverride_StripListMatchesDenyList.
	//
	// http.Header.Del canonicalizes via textproto.CanonicalMIMEHeaderKey
	// before lookup, so "X-API-Key" and "X-Api-Key" collapse to the
	// same map key — only one entry per canonical form needs to appear.
	// "X-Apikey" (no hyphen) IS a distinct canonical form and stays.
	//
	// Cookie is stripped because applyAuthOverride only runs when
	// override != nil — the ccwrap-owned credential path. In that mode
	// the upstream is a third-party gateway (the override is the
	// gateway credential), not the Anthropic origin the client's
	// cookie was scoped for; forwarding it leaks a session cookie
	// across origins.
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Apikey", "Api-Key", "X-Gateway-Key", "X-LitellM-Key", "X-Provider-Key", "X-Provider-Token", "Cookie"} {
		h.Del(name)
	}
	h.Set(override.HeaderName, override.HeaderValue)
	additional := override.AdditionalHeaders
	if override.Source == model.AuthSourceClaudeOAuthToken && len(additional) == 0 {
		additional = map[string]string{"anthropic-beta": "oauth-2025-04-20"}
	}
	for name, value := range additional {
		if strings.EqualFold(name, "anthropic-beta") {
			mergeCommaHeaderValue(h, "anthropic-beta", value)
			continue
		}
		h.Set(name, value)
	}
}

func mergeCommaHeaderValue(h http.Header, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	existing := h.Get(name)
	if existing == "" {
		h.Set(name, value)
		return
	}
	for _, token := range strings.Split(existing, ",") {
		if strings.TrimSpace(token) == value {
			return
		}
	}
	h.Set(name, existing+", "+value)
}

func joinTargetURL(base *url.URL, req *url.URL) *url.URL {
	out := cloneURL(base)
	baseDecoded := strings.TrimRight(base.Path, "/")
	baseEscaped := strings.TrimRight(base.EscapedPath(), "/")
	reqDecoded := req.Path
	if reqDecoded == "" {
		reqDecoded = "/"
	}
	reqEscaped := req.EscapedPath()
	if reqEscaped == "" {
		reqEscaped = reqDecoded
	}
	switch {
	case baseDecoded == "":
		out.Path = reqDecoded
		out.RawPath = reqEscaped
	case reqDecoded == "/":
		out.Path = baseDecoded + "/"
		out.RawPath = baseEscaped + "/"
	default:
		out.Path = singleJoiningSlash(baseDecoded, reqDecoded)
		out.RawPath = singleJoiningSlash(baseEscaped, reqEscaped)
	}
	if out.RawPath == out.Path {
		out.RawPath = ""
	}
	out.RawQuery = req.RawQuery
	out.Fragment = ""
	return out
}

func cloneURL(in *url.URL) *url.URL {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	default:
		return a + b
	}
}

func pathAndQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}
