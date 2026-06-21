package supervisor

import (
	"net/url"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

// posture is the per-session immutable routing snapshot the supervisor
// publishes via atomic.Pointer and every request captures once at entry.
// It is the product  resolved ⊕ live :
//   - a Switch replaces the resolved half wholesale (withResolved),
//   - SetCaptureBodies/SetCaptureTelemetry flip one live toggle.
//
// Because the two halves are disjoint sub-structs, replacing one never touches
// the other — toggle-preservation across a Switch is structural, with no
// preserve parameter and no field-by-field clone. Inner pointers/maps are
// read-only after construction; the hot path dereferences p.r.* (routing) and
// p.l.* (toggles) with no lock and no allocation. It is the in-package
// successor to activePosture.
type posture struct {
	r resolved
	l live
}

// resolved is the routing identity a Switch replaces wholesale. Immutable after
// newResolved. It carries the hot-path routing fields, the request-time
// identity fields, AND a precomputed userinfo-stripped display sub-struct so
// deriveInto is a pure copy (the strip/derive happens ONCE here, not per read).
type resolved struct {
	// routing — read on the hot path
	apiBaseURL      *url.URL
	overrideAuth    *model.AuthOverride
	egress          model.EgressConfig
	modelAlias      modelalias.Config
	upstreamHeaders upstreamheaders.Config

	// identity / classification — read on the hot path (record stamp, 502 body)
	routeClass      model.RouteClass
	authBootstrap   model.AuthBootstrap
	profileName     string
	profileProvider string
	missingAuthEnv  string

	// display — userinfo-stripped / pre-derived; read ONLY by deriveInto
	display postureDisplay
}

// live is the launch-scoped, profile-orthogonal toggle set that survives every
// Switch. captureBodies/captureTelemetry have runtime setters; nativeTLS is
// launch-only today (no setter) but lives here because it is equally
// profile-orthogonal and must survive a Switch.
type live struct {
	captureBodies    bool
	captureTelemetry bool
	nativeTLS        bool
}

// postureDisplay holds every Projection field that is a pure function of the
// posture (independent of dial state), computed once in newResolved. The
// dial-derived display fields (NativeTLS, NativeTLSLoaded, NativeTLSFallbacks)
// are NOT here — they depend on dialState and are merged by deriveInto.
type postureDisplay struct {
	apiBaseURL        string // stripUserinfoString(APIBaseURL.String())
	exactUpstreamHost string // APIBaseURL.Hostname() — userinfo-free
	exactUpstreamBase string // stripUserinfoString(APIBaseURL.String())
	egressSummary     string // stripUserinfoString(Egress.Summary)

	routeSource       model.RouteSource
	routeConfigSource string
	authMode          model.AuthMode
	authSource        model.AuthSource
	authConfigSource  string
	authPolicy        model.AuthPolicy
	authBootstrapKind model.AuthBootstrapKind
	egressMode        string
	egressSource      string

	modelAliasMode                     model.ModelAliasMode
	modelAliasSource                   string
	modelAliasStrict                   bool
	modelAliasProviderModelPassthrough bool
	modelAliasCount                    int
	modelAliasFingerprint              string
	modelAliasForward                  map[string]string // owned copy, read-only

	upstreamHeaderCount       int
	upstreamHeaderSource      string
	upstreamHeaderFingerprint string

	failPolicy model.FailPolicy
}

// dialState is the dial-path-written, sess.mu-guarded native-TLS display state.
// deriveInto takes it BY VALUE so it stays pure. recordNativeTLS remains the
// sole writer (it keeps writing sess.public.NativeTLS* as the store); snapshot
// reads those into a dialState and deriveInto re-applies them idempotently.
type dialState struct {
	nativeTLS          string // "", "active", "blocked: <reason>"
	nativeTLSFallbacks int
	nativeTLSLoaded    bool
}

// newResolved is the single *preflight.Result -> resolved mapping. It subsumes
// both the inline setRoute constructor and posturePublishFromResult. INFALLIBLE:
// the Result is already validated by preflight (URL parsed, modelalias/
// upstreamheaders Configs built), so this is pure field projection. A Switch
// that cannot produce a valid Result fails in ResolveProfile before calling
// this — no half-published state. All userinfo-stripping, ModelAliasMode
// derivation, FailPolicy pinning, and the defensive forward-map copy happen
// here, once.
func newResolved(pre *preflight.Result) resolved {
	apiBase := ""
	exactHost := ""
	exactBase := ""
	if pre.APIBaseURL != nil {
		raw := pre.APIBaseURL.String()
		apiBase = stripUserinfoString(raw)
		exactHost = pre.APIBaseURL.Hostname()
		exactBase = stripUserinfoString(raw)
	}
	aliasMode := model.ModelAliasDisabled
	if pre.ModelAlias.Enabled() {
		aliasMode = model.ModelAliasRewrite
	}
	return resolved{
		apiBaseURL:      pre.APIBaseURL,
		overrideAuth:    pre.OverrideAuth,
		egress:          pre.Egress,
		modelAlias:      pre.ModelAlias,
		upstreamHeaders: pre.UpstreamHeaders,
		routeClass:      pre.RouteClass,
		authBootstrap:   pre.AuthBootstrap,
		profileName:     pre.ActiveProfileName,
		profileProvider: pre.ActiveProfileProvider,
		missingAuthEnv:  pre.MissingAuthEnv,
		display: postureDisplay{
			apiBaseURL:                         apiBase,
			exactUpstreamHost:                  exactHost,
			exactUpstreamBase:                  exactBase,
			egressSummary:                      stripUserinfoString(pre.Egress.Summary),
			routeSource:                        pre.RouteSource,
			routeConfigSource:                  pre.RouteConfigSource,
			authMode:                           pre.AuthMode,
			authSource:                         pre.AuthSource,
			authConfigSource:                   pre.AuthConfigSource,
			authPolicy:                         pre.AuthPolicy,
			authBootstrapKind:                  pre.AuthBootstrapKind,
			egressMode:                         pre.Egress.Mode,
			egressSource:                       pre.Egress.Source,
			modelAliasMode:                     aliasMode,
			modelAliasSource:                   pre.ModelAlias.Source,
			modelAliasStrict:                   pre.ModelAlias.Strict,
			modelAliasProviderModelPassthrough: pre.ModelAlias.ProviderModelPassthrough,
			modelAliasCount:                    pre.ModelAlias.Count(),
			modelAliasFingerprint:              pre.ModelAlias.Fingerprint,
			modelAliasForward:                  copyStringMap(pre.ModelAlias.Forward),
			upstreamHeaderCount:                len(pre.UpstreamHeaders.Headers),
			upstreamHeaderSource:               pre.UpstreamHeaders.Source,
			upstreamHeaderFingerprint:          pre.UpstreamHeaders.Fingerprint,
			failPolicy:                         model.FailClosed,
		},
	}
}

// withResolved returns a new posture with new resolved and the SAME live —
// the Switch transition. It cannot touch a toggle (it does not take one);
// commutativity with the toggle setters is by construction.
func (p posture) withResolved(r resolved) posture { return posture{r: r, l: p.l} }

// withCaptureBodies returns a new posture with the SAME resolved and one toggle
// flipped. Replaces the SetCaptureBodies clone-all-fields block: nothing is
// enumerated, so no field (e.g. missingAuthEnv) can be forgotten.
func (p posture) withCaptureBodies(on bool) posture {
	p.l.captureBodies = on
	return p
}

func (p posture) withCaptureTelemetry(on bool) posture {
	p.l.captureTelemetry = on
	return p
}

// deriveInto writes the posture-derived Projection fields onto dst (a Session
// the caller already copied under sess.mu). PURE: no locks, no I/O, no clock.
// Value-identity: it emits exactly the fields the deleted publicProjection
// mirror wrote, plus the three dial-derived NativeTLS fields the old seed/
// sticky/preserve special-cases handled.
func (p *posture) deriveInto(dst *model.Session, d dialState) {
	dst.APIBaseURL = p.r.display.apiBaseURL
	dst.RouteClass = p.r.routeClass
	dst.RouteSource = p.r.display.routeSource
	dst.RouteConfigSource = p.r.display.routeConfigSource
	dst.AuthMode = p.r.display.authMode
	dst.AuthSource = p.r.display.authSource
	dst.AuthConfigSource = p.r.display.authConfigSource
	dst.AuthPolicy = p.r.display.authPolicy
	dst.AuthBootstrap = p.r.authBootstrap
	dst.AuthBootstrapKind = p.r.display.authBootstrapKind
	dst.ExactUpstreamHost = p.r.display.exactUpstreamHost
	dst.ExactUpstreamBase = p.r.display.exactUpstreamBase
	dst.EgressMode = p.r.display.egressMode
	dst.EgressSource = p.r.display.egressSource
	dst.EgressSummary = p.r.display.egressSummary
	dst.ModelAliasMode = p.r.display.modelAliasMode
	dst.ModelAliasSource = p.r.display.modelAliasSource
	dst.ModelAliasStrict = p.r.display.modelAliasStrict
	dst.ModelAliasProviderModelPassthrough = p.r.display.modelAliasProviderModelPassthrough
	dst.ModelAliasCount = p.r.display.modelAliasCount
	dst.ModelAliasFingerprint = p.r.display.modelAliasFingerprint
	dst.ModelAliasForward = p.r.display.modelAliasForward
	dst.UpstreamHeaderCount = p.r.display.upstreamHeaderCount
	dst.UpstreamHeaderSource = p.r.display.upstreamHeaderSource
	dst.UpstreamHeaderFingerprint = p.r.display.upstreamHeaderFingerprint
	dst.FailPolicy = p.r.display.failPolicy
	dst.ActiveProfileName = p.r.profileName
	dst.ActiveProfileProvider = p.r.profileProvider
	dst.MissingAuthEnv = p.r.missingAuthEnv
	dst.CaptureBodies = p.l.captureBodies
	dst.CaptureTelemetry = p.l.captureTelemetry
	// NativeTLS: dial-written status wins; else seed from the launch toggle.
	// This single expression replaces publishPosture's `if NativeTLS == ""`
	// seed guard plus the sticky/preserve special-cases.
	if d.nativeTLS != "" {
		dst.NativeTLS = d.nativeTLS
	} else {
		dst.NativeTLS = initialNativeTLSPosture(p.l.nativeTLS)
	}
	dst.NativeTLSLoaded = d.nativeTLSLoaded
	dst.NativeTLSFallbacks = d.nativeTLSFallbacks
}
