package supervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// handleEgressProbe is the browser-facing POST endpoint for
// /profile/test-egress. CSRF first, method second — same shape as
// handle_profile_probe.go::handleProfileTest. Probe failures return
// HTTP 200 with the result JSON; only pre-probe failures (CSRF,
// method, parse, lookup, override-validation) return 4xx.
//
// Wire contract:
//
//	POST /profile/test-egress
//	  X-CCWRAP-Profile-Token: <token>
//	  Content-Type: application/json
//	  body: {
//	    "name": "<profile-name>" | "inherit-env" | "<active-session>",
//	    "egress_override": { "mode": "...", "url": "..." }   // optional
//	  }
//	→ 200 + EgressProbeResult JSON
//	→ 403 (CSRF), 405 (non-POST),
//	  400 (parse / both name and egress_override missing),
//	  404 (no profiles.json or unknown name),
//	  422 (egress_override invalid),
//	  500 (I/O / marshal).
//
// Name-less draft probe: when name is empty but egress_override is
// supplied, the probe runs against the override (no profile lookup).
// Result.Profile carries the sentinel "<draft>". Used by the popover's
// add-mode test button to validate a draft EgressSpec before [save]
// — the profile name input may still be empty / in flight, and the
// user wants to validate the proxy URL anyway.
//
// Inherit carve-out: if the resolved draft Egress has mode=inherit or
// mode="" (after any override is applied), the final-stage block
// substitutes the posture-derived spec — same behavior as a named
// profile with mode=inherit. So a name-less draft override of
// {mode:"inherit"} still walks the session's actual egress, not
// supervisor process env. This is intentional symmetry with the
// named-profile inherit path.
//
// NO target_override field is accepted — probe URL is server-controlled
// (env or hardcoded) to prevent SSRF.
func (sp *sessionProxy) handleEgressProbe(w http.ResponseWriter, r *http.Request) {
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden: csrf token missing or invalid", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxProfileMutationBytes)
	defer body.Close()

	var req struct {
		Name           string `json:"name"`
		EgressOverride *struct {
			Mode string `json:"mode"`
			URL  string `json:"url"`
		} `json:"egress_override,omitempty"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w,
			fmt.Sprintf("invalid request body (or exceeds %d bytes)", maxProfileMutationBytes),
			http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" && req.EgressOverride == nil {
		http.Error(w, "name or egress_override required", http.StatusBadRequest)
		return
	}
	// Synthetic names probe a SPECIFIC live state — overriding their
	// egress defeats the contract. <active-session> reflects the session's
	// resolved posture; inherit-env reflects the supervisor's env. Both
	// are read-only diagnostics. A caller that wants to test an arbitrary
	// EgressSpec must use the name-less draft path (omit name, supply
	// egress_override) — surface a 422 with the alternative inline.
	if req.EgressOverride != nil && (name == "<active-session>" || name == "inherit-env") {
		http.Error(w,
			fmt.Sprintf("egress_override is incompatible with synthetic name %q — omit the name to run a name-less draft probe", name),
			http.StatusUnprocessableEntity)
		return
	}

	var profile profiles.Profile
	if name == "" {
		// Name-less draft probe: caller supplies only egress_override
		// (popover add-mode). No profile lookup; the override is the
		// entire spec being probed. Sentinel name distinguishes the
		// result from any real profile.
		profile = profiles.Profile{Name: "<draft>"}
	} else {
		var err error
		profile, err = sp.resolveProfileForEgressProbe(name)
		if err != nil {
			writeProfileTestError(w, err) // reused from handle_profile_probe.go
			return
		}
	}

	// Draft override: popover may test unsaved edits. Validate via the
	// single source of truth (profiles.ValidateEgressSpec) before
	// overlaying onto the resolved profile. For the name-less path this
	// override IS the spec; for the named path it shadows the saved one.
	if req.EgressOverride != nil {
		overrideMode := strings.TrimSpace(req.EgressOverride.Mode)
		overrideURL := strings.TrimSpace(req.EgressOverride.URL)
		if overrideMode == "" && overrideURL == "" {
			// Reject the empty struct rather than silently swapping in an
			// empty spec. An honest "override = nothing" should omit the
			// field entirely; sending {} suggests the caller meant to send
			// something but lost it on the wire — surface explicitly.
			http.Error(w,
				"egress_override requires non-empty mode — omit the field to probe the profile's saved egress",
				http.StatusUnprocessableEntity)
			return
		}
		draft := profiles.EgressSpec{
			Mode: req.EgressOverride.Mode,
			URL:  req.EgressOverride.URL,
		}
		if verr := profiles.ValidateEgressSpec(draft); verr != nil {
			http.Error(w, verr.Error(), http.StatusUnprocessableEntity)
			return
		}
		profile.Egress = draft
	}

	// Single posture snapshot for the whole probe. Both the inherit
	// substitution below and the NoProxy overlay read from THIS pointer,
	// so a mid-probe SwitchProfile can never pair posture A's egress spec
	// with posture B's NoProxy (the two reads used to be independent
	// sess.active.Load() calls on a volatile pointer).
	// Local var named ap (not "posture") to avoid shadowing the posture TYPE.
	var ap *posture
	if sp.session != nil {
		ap = sp.session.active.Load()
	}

	// Final-stage inherit-substitution: any non-synthetic profile (named
	// from disk, or "<draft>" from the name-less path) whose effective
	// Egress is mode=inherit or empty resolves through the session's
	// posture — matching what the user sees on the dashboard Egress
	// ribbon cell. Without this, probes silently fall through to the
	// supervisor's process env via egress.Resolve(""), missing any proxy
	// that Claude settings injected into the child but not the parent.
	// Synthetic names (<active-session>, inherit-env) bypass: the former
	// already returned a posture-derived spec; the latter is documented
	// to read process env (CLI-style). The override path lands here too,
	// so a popover "test this draft" with mode=inherit gets the same
	// posture treatment instead of leaking through env.
	//
	// `substituted` records whether the spec under test actually came
	// FROM the posture — it gates the NoProxy overlay below.
	substituted := false
	if profile.Name != "inherit-env" && profile.Name != "<active-session>" {
		mode := strings.ToLower(strings.TrimSpace(profile.Egress.Mode))
		if mode == "" || mode == "inherit" {
			profile.Egress = synthesizeActiveSessionProfileFrom(ap, sp.supervisor.runtimeEgressFlag()).Egress
			substituted = true
		}
	}

	// Propagate posture's NoProxy into the probe — but ONLY for specs that
	// came from the posture. See egressProbeNoProxyOverlay.
	noProxyForProbe := egressProbeNoProxyOverlay(profile.Name, substituted, ap)

	result := profiletest.ProbeEgress(profile, profiletest.EgressProbeOptions{
		Timeout: 5 * time.Second,
		NoProxy: noProxyForProbe,
	})
	data, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "marshal result", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// resolveProfileForEgressProbe resolves a name into a profiles.Profile
// suitable for egress probing. Unlike resolveProfileForTest, it:
//   - does NOT require auth (egress probe never touches credentials)
//   - handles the synthetic "<active-session>" name by reading the
//     resolved posture so the probe reflects the session's actual
//     egress (covers profile + launcher flag + Claude settings + env).
//
// Inherit-mode substitution for named profiles happens in the caller
// (handleEgressProbe's final-stage block) so it applies uniformly to
// the override path too. Putting it here would let a popover-supplied
// override re-introduce mode=inherit and silently break out into env.
func (sp *sessionProxy) resolveProfileForEgressProbe(name string) (profiles.Profile, error) {
	if name == "<active-session>" {
		return sp.synthesizeActiveSessionProfile(), nil
	}
	if name == "inherit-env" {
		// inherit-env names a credential source, which egress probe
		// ignores. Empty Egress means ProbeEgress resolves the supervisor
		// process's own env (HTTPS_PROXY / HTTP_PROXY / ALL_PROXY) — so
		// the probe reports the same exit the supervisor would inherit
		// at runtime. With no proxy env set, this is direct.
		return profiles.Profile{Name: "inherit-env"}, nil
	}
	stateDir := sp.supervisor.paths.StateDir
	file, err := profiles.Load(profiles.DefaultPath(stateDir))
	if err != nil {
		return profiles.Profile{}, errProfileConfig
	}
	if file == nil || len(file.Profiles) == 0 {
		return profiles.Profile{}, errProfileNotFound
	}
	p, ok := file.Profiles[name]
	if !ok {
		return profiles.Profile{}, errProfileNotFound
	}
	p.Name = name
	return p, nil
}

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
