package supervisor

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// synthesizeActiveSessionProfile returns a profile whose Egress reflects
// what the active session is ACTUALLY exiting through. The resolved
// posture is the source of truth — it folds together all egress
// sources (profile / launcher --egress-proxy / Claude settings / env)
// into a single model.EgressConfig that the supervisor uses for
// forwarding. Probing anything else risks reporting a different
// egress than the session actually uses.
//
// Resolution order:
//  1. sp.session.active.Load().r.egress — the resolved posture
//     egress (covers profile + launcher flag + claude_settings + env).
//  2. parseLauncherEgressFlag(supervisor.runtimeEgressFlag()) — last-
//     resort fallback in the early-bootstrap window before posture is
//     published.
//  3. Empty spec → ProbeEgress falls back to env-resolution from the
//     supervisor's own process environment (HTTPS_PROXY etc.), which
//     matches what the session would inherit at the same moment.
//
// Name is fixed sentinel "<active-session>" so the UI can distinguish
// it from any real profile.
func (sp *sessionProxy) synthesizeActiveSessionProfile() profiles.Profile {
	// Local var named ap (not "posture") to avoid shadowing the posture TYPE.
	var ap *posture
	if sp.session != nil {
		ap = sp.session.active.Load()
	}
	return synthesizeActiveSessionProfileFrom(ap, sp.supervisor.runtimeEgressFlag())
}

// synthesizeActiveSessionProfileFrom builds the <active-session> profile from
// an already-captured posture snapshot, so a caller that also reads other
// posture sub-fields (e.g. NoProxy) uses ONE consistent snapshot rather than
// racing a second sess.active.Load(). launcherFlag is the fallback when the
// posture carries no egress spec.
func synthesizeActiveSessionProfileFrom(ap *posture, launcherFlag string) profiles.Profile {
	spec := profiles.EgressSpec{}
	if ap != nil {
		spec = postureEgressToSpec(ap.r.egress)
	}
	if spec.Mode == "" {
		spec = parseLauncherEgressFlag(launcherFlag)
	}
	return profiles.Profile{Name: "<active-session>", Egress: spec}
}

// egressProbeNoProxyOverlay decides the NoProxy bypass list to overlay onto an
// egress probe. The posture's NoProxy is overlaid ONLY when the spec under
// test came FROM the posture: the <active-session> synthetic, or a
// named/draft profile whose mode=inherit/empty was substituted from the
// posture (substituted==true). An explicit-proxy profile (mode=http/socks5
// with its own URL) is probed THROUGH that URL with no overlay — otherwise the
// posture NoProxy could match the probe target and silently dial DIRECT,
// reporting OK for a proxy that was never exercised, so a broken/down proxy
// reads as working.
//
// inherit-env is exempt unconditionally: it is documented to read the
// supervisor's PROCESS env, so its NoProxy comes from env, not posture.
func egressProbeNoProxyOverlay(profileName string, substituted bool, ap *posture) string {
	if profileName == "inherit-env" {
		return ""
	}
	if !substituted && profileName != "<active-session>" {
		return ""
	}
	if ap == nil {
		return ""
	}
	return ap.r.egress.NoProxy
}

// postureEgressToSpec converts the resolved session egress
// (model.EgressConfig — what the supervisor actually forwards through)
// into a profiles.EgressSpec form ProbeEgress can consume. URL
// userinfo is preserved so probe traffic can authenticate to an
// upstream proxy that requires credentials. HTTPSProxy is preferred
// over HTTPProxy (egress.Resolve populates both equivalently when
// resolving from a single source, so this matters only in
// scheme-asymmetric configurations).
func postureEgressToSpec(eg model.EgressConfig) profiles.EgressSpec {
	switch strings.ToLower(strings.TrimSpace(eg.Mode)) {
	case "direct", "none":
		return profiles.EgressSpec{Mode: "direct"}
	}
	rawURL := strings.TrimSpace(eg.HTTPSProxy)
	if rawURL == "" {
		rawURL = strings.TrimSpace(eg.HTTPProxy)
	}
	if rawURL == "" {
		return profiles.EgressSpec{}
	}
	return parseLauncherEgressFlag(rawURL)
}

// parseLauncherEgressFlag is the inverse of preflight/profile.go's
// EgressSpec→flag-string conversion. Accepts:
//
//	""                     → mode=inherit
//	"direct"               → mode=direct
//	"http(s)://host:port"  → mode=http   url=...
//	"socks5://host:port"   → mode=socks5 url=...
//	"socks5h://host:port"  → mode=socks5h url=...
//	anything else          → mode=inherit
func parseLauncherEgressFlag(flag string) profiles.EgressSpec {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return profiles.EgressSpec{Mode: "inherit"}
	}
	if flag == "direct" {
		return profiles.EgressSpec{Mode: "direct"}
	}
	if u, err := url.Parse(flag); err == nil {
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			return profiles.EgressSpec{Mode: "http", URL: flag}
		case "socks5":
			return profiles.EgressSpec{Mode: "socks5", URL: flag}
		case "socks5h":
			return profiles.EgressSpec{Mode: "socks5h", URL: flag}
		}
	}
	return profiles.EgressSpec{Mode: "inherit"}
}

// writeActiveEgressProbe reports where the session's traffic exits now:
// the posture's egress, probed the way the session would dial. The v1 API's
// egress-test.
func (sp *sessionProxy) writeActiveEgressProbe(w http.ResponseWriter) {
	profile := sp.synthesizeActiveSessionProfile()
	var ap *posture
	if sp.session != nil {
		ap = sp.session.active.Load()
	}
	writeJSON(w, profiletest.ProbeEgress(profile, profiletest.EgressProbeOptions{
		Timeout: 5 * time.Second,
		NoProxy: egressProbeNoProxyOverlay(profile.Name, false, ap),
	}))
}
