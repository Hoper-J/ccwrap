package supervisor

import "github.com/Hoper-J/ccwrap/internal/webguard"

// The info listener's Host check lives in webguard, shared with the ccwrap
// ui service. Scope here: handleInfoRequest ONLY. CONNECT and absolute-URI
// forward-proxy requests carry the TARGET host in r.Host by protocol design
// and are never gated.

const webAllowedHostsEnv = webguard.AllowedHostsEnv

// webAllowedHostsFromEnv snapshots CCWRAP_WEB_ALLOWED_HOSTS at session-proxy
// construction (launch time); runtime changes require a relaunch.
func webAllowedHostsFromEnv() map[string]struct{} { return webguard.AllowedHostsFromEnv() }

func parseWebAllowedHosts(raw string) map[string]struct{} { return webguard.ParseAllowedHosts(raw) }

// infoHostAllowed reports whether r.Host may reach the info endpoints.
func (sp *sessionProxy) infoHostAllowed(hostHeader string) bool {
	return webguard.HostAllowed(hostHeader, sp.webAllowedHosts)
}
