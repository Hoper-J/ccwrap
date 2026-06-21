package supervisor

import (
	"net"
	"os"
	"strings"
)

// Host-header validation for the browser-facing info listener.
//
// The session proxy binds 127.0.0.1, but loopback binding alone does not stop
// a DNS-rebinding page: a hostile site the user's browser visits can point the
// attacker's own hostname at 127.0.0.1 and then read the GET info endpoints
// (/, /recent, /events — and with them the page-embedded profile token) from
// JavaScript. The same-origin policy never helps because the document's origin
// IS the attacker's hostname. The Host header is the one request attribute the
// browser fills in faithfully from the address bar, so the info endpoints
// accept only loopback-shaped Hosts by default and refuse everything else
// with 421.
//
// Tunnel deployments are the sanctioned exception (the dashboard's
// plain-HTTP-tunnel doctrine — e.g. a Cloudflare Tunnel hostname in front of
// the loopback port): launch ccwrap with CCWRAP_WEB_ALLOWED_HOSTS set to a
// comma-separated list of additional hostnames (case-insensitive; a :port
// suffix on an entry is tolerated and ignored — matching is by hostname).
//
// Scope: handleInfoRequest ONLY. CONNECT and absolute-URI forward-proxy
// requests carry the TARGET host in r.Host by protocol design and are never
// gated here.

// webAllowedHostsEnv names extra Host values admitted to the info endpoints.
const webAllowedHostsEnv = "CCWRAP_WEB_ALLOWED_HOSTS"

// webAllowedHostsFromEnv snapshots CCWRAP_WEB_ALLOWED_HOSTS at session-proxy
// construction (launch time). Runtime changes to the env require a relaunch —
// same lifecycle as every other launch-scoped knob.
func webAllowedHostsFromEnv() map[string]struct{} {
	return parseWebAllowedHosts(os.Getenv(webAllowedHostsEnv))
}

func parseWebAllowedHosts(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.ToLower(strings.TrimSpace(part))
		if entry == "" {
			continue
		}
		host, _ := splitHostPort(entry)
		if host == "" {
			continue
		}
		out[host] = struct{}{}
	}
	return out
}

// infoHostAllowed reports whether r.Host may reach the info endpoints.
// Allowed: loopback IP literals (127.0.0.0/8, ::1), localhost and
// *.localhost (RFC 6761 names browsers resolve to loopback themselves —
// a remote attacker cannot serve a document from them), the launch-time
// CCWRAP_WEB_ALLOWED_HOSTS entries, and an EMPTY Host (HTTP/1.0-style local
// tooling; browsers — the rebinding vector — always send Host).
func (sp *sessionProxy) infoHostAllowed(hostHeader string) bool {
	host, _ := splitHostPort(strings.TrimSpace(hostHeader))
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	_, ok := sp.webAllowedHosts[host]
	return ok
}
