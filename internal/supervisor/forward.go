package supervisor

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

// forwardTarget is where an entry point (handleForwardProxyRequest for
// plain-HTTP proxy requests, mitmHandler for MITM'd Anthropic connections)
// routed one request. forwardToUpstream does the rest.
type forwardTarget struct {
	// logicalHost is the host Claude Code addressed; upstreamHost is the
	// host the request actually goes to.
	logicalHost  string
	upstreamHost string
	// applyAuth is set for Anthropic API hosts: ccwrap rewrites model
	// aliases and applies its upstream headers and auth override.
	applyAuth bool
	// url builds the upstream URL from the inbound request URL.
	url func(in *url.URL) *url.URL
	// hello is the MITM connection's ClientHello shape, mirrored upstream on
	// the native-TLS path. nil on the forward path and with native TLS off.
	hello *helloStack
	// wireOrder is the client's request-head order, re-laid on native
	// upstream connections. nil on the forward path.
	wireOrder *wireOrder
}

// refuseUnroutable answers 502 for a request whose upstream route could not
// be resolved, and records why.
func (sp *sessionProxy) refuseUnroutable(w http.ResponseWriter, logicalHost, errClass string, err error) {
	sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
		Timestamp:       time.Now(),
		SessionID:       sp.session.public.ID,
		Severity:        "error",
		ErrorClass:      errClass,
		Summary:         err.Error(),
		UpstreamHost:    logicalHost,
		SuggestedAction: routeSuggestion(errClass),
	})
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "route",
		Summary:   "route resolution failed",
		Detail:    err.Error(),
	})
	http.Error(w, err.Error(), http.StatusBadGateway)
}

// teeRequestBody reads r.Body, restores it for the forward, and spills a
// copy to the session body store.
//
// Best-effort, faithfulness-first: it returns a ref only when the full body
// was read. On a read error the (partial) body is still forwarded unchanged,
// but it is NOT spilled — the inspector must never show a silently-truncated
// body as complete. Disk-write failure is handled separately (endpoint 404).
func (sp *sessionProxy) teeRequestBody(r *http.Request, logicalHost string) *model.RequestBodyRef {
	b, rerr := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	st := sp.session.bodies
	if st == nil || rerr != nil || len(b) == 0 {
		return nil
	}
	// OAuth-host bodies: redact JSON-field credentials (refresh_token,
	// access_token, ...) before spilling. The original `b` already went to
	// r.Body above, so the upstream still receives the unmodified body —
	// redaction affects only the ccwrap-internal observability surface
	// (drawer + spill file).
	spill := b
	if shouldRedactBody(logicalHost, r.URL.Path) {
		spill = redactJSONBody(b, sp.supervisor.unmaskCredentials)
	}
	return st.put(newBodyID(), spill)
}

// forwardToUpstream sends one routed request upstream and records it: body
// capture, model alias rewrite, upstream headers and auth, the reverse proxy,
// failure diagnostics, and the request record. ap is the posture captured at
// handler entry; every decision here reads it, so a mid-request profile
// switch cannot tear the in-flight request.
func (sp *sessionProxy) forwardToUpstream(w http.ResponseWriter, r *http.Request, ap *posture, start time.Time, t forwardTarget) {
	// Capture tap. MUST be BEFORE modelalias.RewriteRequest below: the
	// rewrite consumes/replaces r.Body, so a post-rewrite tap sees nothing.
	var bodyRef *model.RequestBodyRef
	if ap.l.captureBodies && r.Body != nil {
		bodyRef = sp.teeRequestBody(r, t.logicalHost)
	}
	var aliasCtx *modelalias.Context
	if t.applyAuth {
		var err error
		aliasCtx, err = modelalias.RewriteRequest(r, ap.r.modelAlias)
		if err != nil {
			sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
				Timestamp:       time.Now(),
				SessionID:       sp.session.public.ID,
				Severity:        "error",
				ErrorClass:      "model_alias_rewrite_failed",
				Summary:         err.Error(),
				UpstreamHost:    t.logicalHost,
				SuggestedAction: "use Claude logical model IDs and configure CCWRAP modelAliases for provider-specific upstream IDs",
			})
			// Anchor the spilled inbound body to a partial RequestRecord so
			// the inspect drawer surfaces what was attempted. Without this,
			// captureBodies=on + rewrite-rejected requests leave orphan body
			// files on disk (bounded by bodystore LRU but unreachable through
			// /recent).
			sp.supervisor.recordRequest(sp.session.public.ID, model.RequestRecord{
				// Timestamp is the request boundary (handler entry), matching
				// the success path below. Stamping time.Now() (the failure
				// moment) would order the record after a sibling success that
				// entered the handler later.
				Timestamp:             start,
				SessionID:             sp.session.public.ID,
				Method:                r.Method,
				LogicalTargetHost:     t.logicalHost,
				ActualUpstreamHost:    t.upstreamHost,
				Path:                  pathForRecord(r.URL, isAnthropicHost(t.logicalHost)),
				StatusCode:            http.StatusBadGateway,
				LatencyMS:             time.Since(start).Milliseconds(),
				StreamState:           model.StreamStateUnknown,
				RequestHeaders:        r.Header.Clone(),
				BodyRef:               bodyRef,
				ActiveProfileName:     ap.r.profileName,
				ActiveProfileProvider: ap.r.profileProvider,
			})
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Upstream-view tap. ALL in-supervisor body mutators MUST run BEFORE
	// this point — today that is modelalias.RewriteRequest only (model
	// field rewrite + Claude-Code system-block stripping). The
	// ReverseProxy.Rewrite hook downstream only edits URL/headers, never
	// body; if a future feature mutates pr.Out.Body we must move the tap.
	// Skip when modelalias did not actually change the body (Rewritten=false)
	// — there's no second view to capture and disk would carry a duplicate.
	var upstreamBodyRef *model.RequestBodyRef
	if ap.l.captureBodies && aliasCtx != nil && aliasCtx.Rewritten && r.Body != nil {
		upstreamBodyRef = sp.teeRequestBody(r, t.logicalHost)
	}
	// Native-TLS uses a SEPARATE upstream transport (its own cache) so the
	// egress-aware utls handshake never touches the shared plain transport
	// that telemetry/forward reuse. upstreamTransportForStack selects it for
	// EVERY Anthropic host when nativeTLS is on (so no *.anthropic.com dial
	// leaks a Go fingerprint); default nativeTLS=false keeps the plain path.
	// MITM'd requests mirror their own connection's ClientHello shape.
	upstreamTr := sp.upstreamTransportForStack(t.logicalHost, ap.r.egress, ap.l.nativeTLS, t.hello)
	// Response-body tap: when capture is on, the ModifyResponse hook below
	// tees the streamed response into a spill WITHOUT buffering (preserves
	// SSE streaming). Set from the tee's Close, read by recordRequest below
	// — Close runs before ServeHTTP returns, so the ref is visible there.
	var responseBodyRef *model.RequestBodyRef
	tap := newFailureTap(ap, aliasCtx, sp.supervisor.unmaskCredentials)
	proxy := &httputil.ReverseProxy{
		Transport:     upstreamTr,
		FlushInterval: -1,
		// httputil logs mid-stream copy errors; the launcher's stderr is
		// Claude Code's terminal, so they go to the session log instead.
		ErrorLog: sp.supervisor.logger,
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := t.url(pr.In.URL)
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.Out.RequestURI = ""
			pr.Out.Header.Del("Proxy-Connection")
			pr.Out.Header.Del("Proxy-Authorization")
			// Force identity ONLY for model-alias response normalization,
			// which must rewrite an uncompressed body. Capture does NOT
			// force identity: it preserves Claude's real Accept-Encoding
			// (full upstream request fidelity) and decodes the captured
			// copy ccwrap-side instead (see decodeCapturedBody).
			if aliasCtx != nil && aliasCtx.NormalizeResponse {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
			if t.applyAuth {
				upstreamheaders.Apply(pr.Out.Header, copyUpstreamHeaders(ap.r.upstreamHeaders.Headers))
				applyAuthOverride(pr.Out.Header, ap.r.overrideAuth)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if err := modelalias.RewriteResponse(resp, aliasCtx); err != nil {
				return err
			}
			// Tee the (post-rewrite) response stream into a spill. The tee
			// copies each chunk as ReverseProxy reads+flushes it, so the
			// client keeps streaming the ORIGINAL (possibly compressed)
			// bytes; the full captured copy lands on Close, where it is
			// decoded per Content-Encoding and (for OAuth credential paths)
			// redacted. Non-decodable bodies are withheld, never spilled raw.
			if ap.l.captureBodies && resp.Body != nil {
				if st := sp.session.bodies; st != nil {
					redact := shouldRedactBody(t.logicalHost, r.URL.Path)
					enc := resp.Header.Get("Content-Encoding")
					resp.Body = newResponseBodyTee(resp.Body, responseBodyCapBytes, func(buf []byte, truncated bool) {
						decoded, decTrunc, ok := decodeCapturedBody(buf, enc, responseBodyCapBytes)
						if !ok {
							ref := st.put(newBodyID(), bodyDecodeFailedSentinel)
							ref.Truncated = true
							responseBodyRef = ref
							return
						}
						spill := decoded
						if redact {
							spill = redactJSONBody(decoded, sp.supervisor.unmaskCredentials)
						}
						ref := st.put(newBodyID(), spill)
						if truncated || decTrunc {
							ref.Truncated = true
						}
						responseBodyRef = ref
					})
				}
			}
			tap.observe(resp)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			summary := err.Error()
			errClass := classifyUpstreamError(err, "upstream_unreachable")
			tap.noUpstream(summary, routeSuggestion(errClass), issueForError(errClass, t.upstreamHost))
			sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
				Timestamp:       time.Now(),
				SessionID:       sp.session.public.ID,
				Severity:        "error",
				ErrorClass:      errClass,
				Summary:         summary,
				UpstreamHost:    t.upstreamHost,
				SuggestedAction: routeSuggestion(errClass),
			})
			sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
				Timestamp: time.Now(),
				SessionID: sp.session.public.ID,
				Category:  "upstream",
				Summary:   "upstream request failed",
				Detail:    summary,
			})
			http.Error(w, summary, http.StatusBadGateway)
		},
	}
	rw := newCaptureResponseWriter(w)
	proxy.ServeHTTP(rw, withWireOrder(r, t.wireOrder))
	latency := time.Since(start)
	sp.session.noteHelloUse(t.hello, t.logicalHost, r.Method, r.URL.Path)
	var helloKey string
	if t.hello != nil {
		helloKey = t.hello.key
	}
	record := model.RequestRecord{
		Timestamp:          start,
		SessionID:          sp.session.public.ID,
		Method:             r.Method,
		ClientHelloKey:     helloKey,
		LogicalTargetHost:  t.logicalHost,
		ActualUpstreamHost: t.upstreamHost,
		Path:               pathForRecord(r.URL, isAnthropicHost(t.logicalHost)),
		StatusCode:         rw.StatusCode(),
		LatencyMS:          latency.Milliseconds(),
		StreamState:        detectStreamState(r, rw.HeaderSnapshot()),
		// Inbound headers, cloned (record outlives the request in a
		// 250-deep ring; do not alias r.Header).
		RequestHeaders: r.Header.Clone(),
		// File-backed body refs, nil unless capture is on. UpstreamBodyRef
		// is set only when modelalias actually changed the body.
		BodyRef:         bodyRef,
		UpstreamBodyRef: upstreamBodyRef,
		ResponseBodyRef: responseBodyRef,
		// Stamp identity from the captured *posture — the same posture the
		// request was routed under, so a post-switch request in flight is
		// recorded against the posture it ran on, not the latest posture.
		ActiveProfileName:     ap.r.profileName,
		ActiveProfileProvider: ap.r.profileProvider,
	}
	tap.stamp(&record)
	sp.supervisor.recordRequest(sp.session.public.ID, record)
}
