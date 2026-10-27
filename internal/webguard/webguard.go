// Package webguard holds the checks ccwrap's browser-facing listeners apply
// on top of binding loopback and accepting only the user's own connections.
//
// Host: loopback binding alone does not stop a DNS-rebinding page. A hostile
// site can point its own hostname at 127.0.0.1 and then read GET endpoints
// from JavaScript; the same-origin policy does not help because the
// document's origin is the attacker's hostname. The Host header is filled in
// faithfully from the address bar, so only loopback-shaped Hosts are
// answered by default. Tunnels (e.g. a Cloudflare Tunnel hostname in front
// of the loopback port) are admitted by listing their hostnames in
// CCWRAP_WEB_ALLOWED_HOSTS (comma-separated, case-insensitive; a :port
// suffix is ignored).
//
// Same-site writes: a state-changing request must come from a page served
// by the listener itself, judged by Sec-Fetch-Site and Origin, on top of
// the token the page carries.
package webguard

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// AllowedHostsEnv names extra Host values the listeners answer.
const AllowedHostsEnv = "CCWRAP_WEB_ALLOWED_HOSTS"

// AllowedHostsFromEnv reads CCWRAP_WEB_ALLOWED_HOSTS; callers snapshot it
// when their listener starts.
func AllowedHostsFromEnv() map[string]struct{} {
	return ParseAllowedHosts(os.Getenv(AllowedHostsEnv))
}

// ParseAllowedHosts parses a comma-separated host list into lowercase
// hostnames.
func ParseAllowedHosts(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		if host := hostOf(strings.ToLower(strings.TrimSpace(part))); host != "" {
			out[host] = struct{}{}
		}
	}
	return out
}

// HostAllowed reports whether a Host header may be answered: loopback IP
// literals, localhost and *.localhost (names browsers resolve to loopback
// themselves), the extra hosts, and an empty Host (local tools speaking
// HTTP/1.0; browsers always send Host).
func HostAllowed(hostHeader string, extra map[string]struct{}) bool {
	host := strings.ToLower(strings.Trim(hostOf(strings.TrimSpace(hostHeader)), "[]"))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	_, ok := extra[host]
	return ok
}

// SameSite reports whether a state-changing request came from a page on
// this listener. Browsers mark cross-site requests with Sec-Fetch-Site and
// Origin; tools that send neither are allowed, since they are not a page an
// attacker controls.
func SameSite(r *http.Request, extra map[string]struct{}) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return HostAllowed(u.Host, extra) && strings.EqualFold(u.Host, r.Host)
}

// hostOf strips a :port suffix, tolerating a bare host and IPv6 literals.
func hostOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	if strings.Count(hostport, ":") == 1 {
		return strings.SplitN(hostport, ":", 2)[0]
	}
	return hostport
}
