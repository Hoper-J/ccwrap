package supervisor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

type sessionProxy struct {
	supervisor       *Supervisor
	session          *sessionState
	listener         net.Listener
	httpServer       *http.Server
	mitmTimeout      time.Duration // mitmHandshakeTimeout, read once when the proxy is made
	closeOnce        sync.Once
	mu               sync.Mutex
	closed           bool
	inner            map[*http.Server]struct{}
	tracked          map[net.Conn]struct{}
	transports       map[string]*http.Transport
	nativeTransports map[string]*http.Transport            // SEPARATE native-TLS cache; Anthropic-route ReverseProxy only. Never shared with telemetry/forward.
	stackTransports  map[stackTransportKey]*http.Transport // native-TLS cache per egress and ClientHello shape (hellostack.go); guarded by mu
	wg               sync.WaitGroup
	pinnedTelemetry  map[string]struct{} // telemetry hosts that rejected our MITM cert this session -> fall back to tunnel; guarded by mu
}

// testHookAfterApCapture is the deterministic torn-read seam. It is nil in
// production and invoked exactly once per request entry, immediately after
// `ap := sess.active.Load()` in every forwarding handler (forward proxy, MITM,
// blind tunnel), before any routing decision branches. Tests set this to a func
// that mutates sess.active (e.g. Store(B)) and then assert the in-flight request
// completes entirely under the captured `ap`, proving that no routing read is
// post-capture lazy.
var testHookAfterApCapture func()

func newSessionProxy(supervisor *Supervisor, session *sessionState) (*sessionProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen session proxy: %w", err)
	}
	ln = newLocalPeerListener(ln, supervisor.logger)
	sp := &sessionProxy{
		supervisor:       supervisor,
		session:          session,
		listener:         ln,
		inner:            map[*http.Server]struct{}{},
		tracked:          map[net.Conn]struct{}{},
		transports:       map[string]*http.Transport{},
		nativeTransports: map[string]*http.Transport{},
		stackTransports:  map[stackTransportKey]*http.Transport{},
		mitmTimeout:      mitmHandshakeTimeout,
	}
	sp.httpServer = &http.Server{
		Handler:           http.HandlerFunc(sp.handleProxy),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       proxyIdleTimeout,
	}
	go func() {
		err := sp.httpServer.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			supervisor.logger.Printf("session proxy serve error: %v", err)
		}
	}()
	return sp, nil
}

// mitmHandshakeTimeout bounds a MITM tunnel's start: reading the client's
// ClientHello (native TLS) and the TLS handshake. A variable for tests.
var mitmHandshakeTimeout = 20 * time.Second

func (sp *sessionProxy) ListenAddr() string {
	return sp.listener.Addr().String()
}

func (sp *sessionProxy) Close() error {
	var err error
	sp.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sp.mu.Lock()
		sp.closed = true
		inner := make([]*http.Server, 0, len(sp.inner))
		for srv := range sp.inner {
			inner = append(inner, srv)
		}
		tracked := make([]net.Conn, 0, len(sp.tracked))
		for conn := range sp.tracked {
			tracked = append(tracked, conn)
		}
		// Every cache: plain, native TLS per egress, native TLS per hello shape.
		transports := make([]*http.Transport, 0, len(sp.transports)+len(sp.nativeTransports)+len(sp.stackTransports))
		for _, tr := range sp.transports {
			transports = append(transports, tr)
		}
		for _, tr := range sp.nativeTransports {
			transports = append(transports, tr)
		}
		for _, tr := range sp.stackTransports {
			transports = append(transports, tr)
		}
		sp.mu.Unlock()
		if sp.listener != nil {
			_ = sp.listener.Close()
		}
		for _, srv := range inner {
			_ = srv.Shutdown(ctx)
		}
		for _, conn := range tracked {
			_ = conn.Close()
		}
		for _, tr := range transports {
			tr.CloseIdleConnections()
		}
		if sp.httpServer != nil {
			err = sp.httpServer.Shutdown(ctx)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			sp.wg.Wait()
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
	})
	return err
}

func (sp *sessionProxy) registerInnerServer(srv *http.Server) bool {
	if srv == nil {
		return false
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return false
	}
	sp.inner[srv] = struct{}{}
	return true
}

func (sp *sessionProxy) isClosed() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.closed
}

func (sp *sessionProxy) unregisterInnerServer(srv *http.Server) {
	if srv == nil {
		return
	}
	sp.mu.Lock()
	delete(sp.inner, srv)
	sp.mu.Unlock()
}

func (sp *sessionProxy) trackConn(conn net.Conn) net.Conn {
	if conn == nil {
		return nil
	}
	tc := &trackedNetConn{Conn: conn}
	tc.onClose = func() { sp.untrackConn(tc) }
	sp.mu.Lock()
	sp.tracked[tc] = struct{}{}
	sp.mu.Unlock()
	return tc
}

func (sp *sessionProxy) untrackConn(conn net.Conn) {
	if conn == nil {
		return
	}
	sp.mu.Lock()
	delete(sp.tracked, conn)
	sp.mu.Unlock()
}

func (sp *sessionProxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		host, port := splitHostPort(r.Host)
		if host == "" {
			http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
			return
		}
		if isAnthropicHost(host) {
			sp.handleAnthropicMITM(w, r, normalizeProxyHost(host), port)
			return
		}
		if sp.shouldCaptureTelemetry(host) {
			sp.handleTelemetryMITM(w, r, host, port)
			return
		}
		sp.handleBlindTunnel(w, r, host, port)
		return
	}
	if isForwardProxyRequest(r) {
		sp.handleForwardProxyRequest(w, r)
		return
	}
	sp.handleInfoRequest(w, r)
}

// handleInfoRequest answers a request addressed to the proxy itself rather
// than sent through it. The port only proxies: the session is looked at in
// ccwrap ui, which reaches it over the control socket. The reply is fixed
// text, so it gives a rebound or cross-site page nothing to read.
func (sp *sessionProxy) handleInfoRequest(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "ccwrap: this port is a session proxy, not a page; to look at the session, run `ccwrap open`", http.StatusNotFound)
}

// handleInfoRecentBody is the lazy body-delivery endpoint. Like
// handleInfoRecent it is scoped to this single-session proxy
// (sp.session) and does not key off the query params — session=/id=
// are accepted but the single-session proxy already scopes itself, so
// a mismatched/absent session is not a 500 (matches the sibling). When
// the spilled body file for id is present it is streamed verbatim as
// application/json (it is already JSON); a missing id, no body store,
// or an unreadable file (evicted / capture off / write failed) yields
// a 404 carrying a short JSON reason. The shared writeJSON helper
// cannot emit a non-200 (it never calls WriteHeader, so the status
// would commit before its Content-Type Set and be sniffed to
// text/plain); a JSON reason is required, so the 404 path sets the JSON
// Content-Type first, then WriteHeader(404), then encodes — the
// documented fallback, not http.Error (which is text/plain).
// isBodyID reports whether s is a well-formed body id (the only shape
// newBodyID/newID can produce: non-empty, length-bounded, lowercase
// hex). Rejecting anything else closes the ?id= path-traversal vector
// before it can reach the filesystem (security: data-egress endpoint).
func isBodyID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// writeBody serves a captured body by id, or a JSON miss.
func (sp *sessionProxy) writeBody(w http.ResponseWriter, id string) {
	st := sp.session.bodies
	if st == nil || !isBodyID(id) {
		writeBodyMissJSON(w, "not available")
		return
	}
	b, err := st.load(id)
	if err != nil {
		writeBodyMissJSON(w, "body not retained (evicted, capture off, or write failed)")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

// writeBodyMissJSON emits the body-miss 404 with a JSON reason. It mirrors
// the shared writeJSON idiom (Content-Type: application/json, an
// indented json.Encoder) but is status-aware: the Content-Type is set
// before WriteHeader(404) so the status commits with the JSON type
// rather than a sniffed text/plain. Kept local to proxy.go; not
// http.Error, which is text/plain.
func writeBodyMissJSON(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{"error": reason})
}

// handleNativeTLSInfo serves the read-only GET /native-tls JSON endpoint: the
// Claude Code ClientHellos ccwrap mirrors, with their JA3/JA4/peetprint.
// "stacks" lists every shape seen on MITM'd connections (hellostack.go), main
// one first: the shape that carried /v1/messages, else the first seen. The
// top-level fields describe the main shape, or the session-default hello
// before any shape is known (and for a pinned hello, which has none).
// Nil-safe: before the first Anthropic connection it reports captured:false.
// A hello carries no credentials (the TLS handshake precedes any HTTP
// request), so none of this leaks secrets.
func (sp *sessionProxy) handleNativeTLSInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := sp.session.mirroredHelloRaw.Load()
	if p == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"captured": false})
		return
	}
	raw := *p
	source := "captured"
	note := "Claude Code's ClientHello that ccwrap mirrors; outbound SNI is rewritten per dial, so all three fingerprints equal what is sent upstream."
	stacks := sp.session.helloStackViews()
	if len(stacks) > 1 {
		note = "Claude Code uses more than one TLS stack; each connection's requests go upstream mirroring its own ClientHello. The first is the one its /v1/messages calls use. Outbound SNI is rewritten per dial."
	}
	if sp.session.nativeTLSHelloLoaded {
		source = "loaded"
		note = "Loaded from CCWRAP_NATIVE_TLS_HELLO — a pinned fingerprint, not the live client's. Outbound SNI is rewritten per dial."
		stacks = nil
	}
	if len(stacks) > 0 {
		raw = stacks[0].raw
	}
	out := map[string]any{
		"captured":         true,
		"source":           source,
		"client_hello_hex": hex.EncodeToString(raw),
		"note":             note,
	}
	if res, err := tlsfp.Compute(raw); err == nil {
		out["ja3"] = res.JA3
		out["ja4"] = res.JA4
		out["peetprint"] = res.Peetprint
	} else {
		out["parse_error"] = err.Error()
	}
	if len(stacks) > 0 {
		out["stacks"] = stacks
	}
	_ = json.NewEncoder(w).Encode(out)
}

// handleNativeTLSClientHello serves raw ClientHello bytes as a download (GET
// /native-tls/clienthello.bin): the shape named by ?key= (a "stacks" key of
// GET /native-tls), else the main shape, else the session-default hello.
// Nil-safe: before the first Anthropic connection, or for an unknown key, it
// 404s. A hello carries no credentials, so the raw bytes leak no secrets.
func (sp *sessionProxy) handleNativeTLSClientHello(w http.ResponseWriter, r *http.Request) {
	var raw []byte
	if p := sp.session.mirroredHelloRaw.Load(); p != nil {
		raw = *p
	}
	stacks := sp.session.helloStackViews()
	if sp.session.nativeTLSHelloLoaded {
		stacks = nil
	}
	if key := r.URL.Query().Get("key"); key != "" {
		raw = nil
		for _, s := range stacks {
			if s.Key == key {
				raw = s.raw
			}
		}
	} else if len(stacks) > 0 {
		raw = stacks[0].raw
	}
	if raw == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="clienthello.bin"`)
	_, _ = w.Write(raw)
}

func (sp *sessionProxy) handleForwardProxyRequest(w http.ResponseWriter, r *http.Request) {
	sp.supervisor.markSessionActive(sp.session.public.ID)
	start := time.Now()
	// Per-request capture: snapshot the immutable posture at handler
	// entry, BEFORE any routing read. Every routing decision in this request
	// (third-party blocker, resolveForwardTarget, and everything in
	// forwardToUpstream) closes over this `ap` — a mid-request Store(B)
	// cannot tear the in-flight request. createSession installs a safe-zero
	// *posture, so ap is never nil here; the defensive nil-coerce is
	// belt-and-suspenders against any future regression. The seam fires once
	// per ServeHTTP entry; nil in production.
	ap := sp.session.active.Load()
	if ap == nil {
		ap = &posture{}
	}
	if testHookAfterApCapture != nil {
		testHookAfterApCapture()
	}
	if logicalHost := forwardRequestLogicalHost(r); sp.handleThirdPartySyntheticOrBlock(w, r, logicalHost, start, ap) {
		return
	}
	target, logicalHost, actualUpstreamHost, applyAuth, errClass, err := sp.resolveForwardTarget(r, ap)
	if err != nil {
		sp.refuseUnroutable(w, logicalHost, errClass, err)
		return
	}
	// Request-time fail-closed. Forwarding to an Anthropic API host without a
	// ccwrap-owned credential would allow an un-authed forward to a hidden
	// upstream. The check is here rather than at preflight so launch always
	// succeeds and inspect tools stay reachable for recovery.
	if sp.maybeRefuseAuthMissing(w, r, ap, applyAuth, logicalHost, start) {
		return
	}
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "forward_proxy",
		Summary:   "forwarding plain HTTP request",
		Detail:    fmt.Sprintf("%s %s -> %s", r.Method, redactedForwardProxyURL(r.URL), target.Host),
	})
	sp.forwardToUpstream(w, r, ap, start, forwardTarget{
		logicalHost:  logicalHost,
		upstreamHost: actualUpstreamHost,
		applyAuth:    applyAuth,
		url:          func(*url.URL) *url.URL { return cloneURL(target) },
	})
}

func pathForRecord(u *url.URL, isAnthropicUpstream bool) string {
	if u == nil {
		return ""
	}
	if isAnthropicUpstream {
		return pathAndQuery(u)
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		return path + "?<redacted>"
	}
	return path
}

func forwardRequestLogicalHost(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	host := strings.TrimSpace(r.URL.Hostname())
	if host == "" {
		requestHost := strings.TrimSpace(r.URL.Host)
		if requestHost == "" {
			requestHost = strings.TrimSpace(r.Host)
		}
		rawHost, _ := splitHostPort(requestHost)
		host = rawHost
	}
	return normalizeProxyHost(host)
}

// resolveForwardTarget reads the request-captured `ap` instead of re-loading
// sess.active, so a mid-handler Store(B) cannot tear the in-flight routing
// decision. `ap` must be non-nil (handler entry guarantees this).
func (sp *sessionProxy) resolveForwardTarget(r *http.Request, ap *posture) (*url.URL, string, string, bool, string, error) {
	if r == nil || r.URL == nil {
		return nil, "", "", false, "invalid_upstream_url", fmt.Errorf("forward proxy request missing target URL")
	}
	target := cloneURL(r.URL)
	if target == nil {
		return nil, "", "", false, "invalid_upstream_url", fmt.Errorf("forward proxy request missing target URL")
	}
	if strings.TrimSpace(target.Host) == "" {
		target.Host = strings.TrimSpace(r.Host)
	}
	if strings.TrimSpace(target.Scheme) == "" {
		target.Scheme = "http"
	}
	logicalHost := normalizeProxyHost(target.Hostname())
	if logicalHost == "" {
		rawHost, _ := splitHostPort(target.Host)
		logicalHost = normalizeProxyHost(rawHost)
	}
	if logicalHost == "" {
		return nil, "", "", false, "invalid_upstream_url", fmt.Errorf("forward proxy request target host is empty")
	}
	if isAnthropicHost(logicalHost) {
		base, upstreamHost, errClass, err := sp.resolveUpstream(logicalHost, ap)
		if err != nil {
			return nil, logicalHost, upstreamHost, false, errClass, err
		}
		return joinTargetURL(base, target), logicalHost, upstreamHost, isAnthropicAPIHost(logicalHost), "upstream_unreachable", nil
	}
	return target, logicalHost, target.Hostname(), false, "upstream_unreachable", nil
}

// telemetryPinned reports whether host has been learned to reject our MITM cert
// (client cert-pinning) this session; such hosts fall back to blind tunnel.
// Guarded by sp.mu.
func (sp *sessionProxy) telemetryPinned(host string) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	_, ok := sp.pinnedTelemetry[host]
	return ok
}

// markTelemetryPinned records host as cert-pinning; subsequent CONNECTs to it
// self-heal to blind tunnel. Guarded by sp.mu.
func (sp *sessionProxy) markTelemetryPinned(host string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.pinnedTelemetry == nil {
		sp.pinnedTelemetry = map[string]struct{}{}
	}
	sp.pinnedTelemetry[host] = struct{}{}
}

// shouldCaptureTelemetry reports whether a CONNECT to host should take the
// transparent telemetry-MITM path: capture toggle on, host on the exact-host
// allowlist, and not learned-pinned this session.
func (sp *sessionProxy) shouldCaptureTelemetry(host string) bool {
	ap := sp.session.active.Load()
	if ap == nil || !ap.l.captureTelemetry {
		return false
	}
	if !isTelemetryCaptureHost(host) {
		return false
	}
	return !sp.telemetryPinned(host)
}

// telemetryBodyCapBytes bounds how many bytes of a telemetry request/response
// body ccwrap spills. Telemetry payloads are short JSON in practice; the cap is
// a safety bound for oversized/streaming responses (capture the prefix, stream
// the rest through unbuffered).
const telemetryBodyCapBytes = 1 << 20 // 1 MiB

// responseBodyCapBytes bounds how many bytes of an Anthropic response body the
// streaming tee retains in memory before spilling. The full response always
// streams through to the client unbuffered — only the captured copy is bounded,
// and bytes beyond the cap set RequestBodyRef.Truncated. A var (not const) so
// tests can lower it; 8 MiB is generous for a single LLM turn while capping the
// worst-case RAM held per in-flight request.
var responseBodyCapBytes = 8 << 20 // 8 MiB

// testHookTelemetryUpstream, when non-nil, returns a base URL (scheme://host)
// that overrides the transparent telemetry forward target, letting tests
// redirect an allowlisted host to a local stub. nil in production -> forward to
// https://<host> (the real telemetry endpoint).
var testHookTelemetryUpstream func(host string) string

// handleTelemetryMITM is the transparent telemetry-capture path: terminate TLS
// with ccwrap's cert, then forward the request AND response UNCHANGED to the
// real telemetry host while spilling both bodies for the inspector. No
// model-alias rewrite, no envelope strip, no auth injection (API-path only). On
// a cert-pinning handshake failure the host is marked pinned so subsequent
// CONNECTs self-heal to blind tunnel. Mirrors handleAnthropicMITM minus the
// auth/alias/resolveUpstream bits, plus the inner handler's response tap. Uses
// the literal CONNECT host (telemetry hosts are non-anthropic; no
// normalizeProxyHost).
func (sp *sessionProxy) handleTelemetryMITM(w http.ResponseWriter, r *http.Request, host, port string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	clientConn = sp.trackConn(clientConn)
	if err := writeConnectEstablished(clientConn); err != nil {
		_ = clientConn.Close()
		sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{Timestamp: time.Now(), SessionID: sp.session.public.ID, Category: "mitm", Summary: "telemetry connect handshake failed", Detail: net.JoinHostPort(host, port)})
		return
	}
	cert, err := sp.supervisor.ca.IssueServerCert(host)
	if err != nil {
		sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sp.session.public.ID,
			Severity:        "error",
			ErrorClass:      "tls_mitm_failed",
			Summary:         fmt.Sprintf("issue telemetry MITM cert failed: %v", err),
			UpstreamHost:    host,
			SuggestedAction: "run `ccwrap doctor` and verify CA state",
		})
		_ = clientConn.Close()
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	_ = tlsConn.SetDeadline(time.Now().Add(sp.mitmTimeout))
	if err := tlsConn.Handshake(); err != nil {
		// Cert-pinning fallback: the telemetry SDK rejected our cert. Mark the
		// host so subsequent CONNECTs self-heal to blind tunnel (this one event
		// is dropped; the handshake is already consumed).
		sp.markTelemetryPinned(host)
		sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
			Timestamp: time.Now(),
			SessionID: sp.session.public.ID,
			Category:  "mitm",
			Summary:   "telemetry handshake failed -- pinned, will tunnel",
			Detail:    host,
		})
		_ = tlsConn.Close()
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "mitm",
		Summary:   "telemetry capture established",
		Detail:    net.JoinHostPort(host, port),
	})
	server := &http.Server{
		Handler:           sp.telemetryMITMHandler(host),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	listener := newSingleConnListener(tlsConn)
	if !sp.registerInnerServer(server) {
		_ = listener.Close()
		_ = tlsConn.Close()
		return
	}
	sp.wg.Add(1)
	defer sp.wg.Done()
	defer sp.unregisterInnerServer(server)
	_ = server.Serve(listener)
}

// telemetryMITMHandler is the inner handler for the transparent telemetry path:
// it taps the request body, forwards request+response unchanged to the
// telemetry host (or the test override), and taps the response body up to the
// size cap. No auth, no model-alias, no upstream-header injection.
func (sp *sessionProxy) telemetryMITMHandler(host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sp.supervisor.markSessionActive(sp.session.public.ID)
		start := time.Now()
		ap := sp.session.active.Load()
		if ap == nil {
			ap = &posture{}
		}
		baseStr := "https://" + host
		if testHookTelemetryUpstream != nil {
			if o := testHookTelemetryUpstream(host); o != "" {
				baseStr = o
			}
		}
		baseURL, perr := url.Parse(baseStr)
		if perr != nil {
			http.Error(w, "bad telemetry upstream", http.StatusBadGateway)
			return
		}
		// Request-body tap. Unconditional: we only reach this handler when
		// telemetry capture is on. Tee once, restore r.Body for the forward.
		var requestBodyRef *model.RequestBodyRef
		if r.Body != nil {
			b, rerr := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
			if st := sp.session.bodies; st != nil && rerr == nil && len(b) > 0 {
				// Redact credential-named fields BEFORE capping so the masker
				// works on the full parseable JSON; the original bytes (b) were
				// already restored to r.Body above and stay unchanged on the
				// upstream forward.
				spill := redactTelemetryBody(b, sp.supervisor.unmaskCredentials)
				truncated := int64(len(spill)) > telemetryBodyCapBytes
				if truncated {
					spill = spill[:telemetryBodyCapBytes]
				}
				requestBodyRef = st.put(newBodyID(), spill)
				if truncated {
					requestBodyRef.Truncated = true
				}
			}
		}
		var responseBodyRef *model.RequestBodyRef
		proxy := &httputil.ReverseProxy{
			Transport:     sp.upstreamTransport(ap.r.egress),
			FlushInterval: -1,
			ErrorLog:      sp.supervisor.logger,
			Rewrite: func(pr *httputil.ProxyRequest) {
				// Transparent: target the telemetry host (or test override),
				// keep path/query, strip hop-by-hop proxy headers. NO auth, NO
				// model-alias, NO upstream-header injection.
				target := joinTargetURL(baseURL, pr.In.URL)
				pr.Out.URL = target
				pr.Out.Host = target.Host
				pr.Out.RequestURI = ""
				pr.Out.Header.Del("Proxy-Connection")
				pr.Out.Header.Del("Proxy-Authorization")
			},
			ModifyResponse: func(resp *http.Response) error {
				st := sp.session.bodies
				if st == nil || resp.Body == nil {
					return nil
				}
				orig := resp.Body
				prefix, rerr := io.ReadAll(io.LimitReader(orig, telemetryBodyCapBytes+1))
				if rerr != nil {
					// Give up capture; stream what we have + the rest, close orig.
					resp.Body = struct {
						io.Reader
						io.Closer
					}{io.MultiReader(bytes.NewReader(prefix), orig), orig}
					return nil
				}
				truncated := int64(len(prefix)) > telemetryBodyCapBytes
				capture := prefix
				if truncated {
					capture = prefix[:telemetryBodyCapBytes]
					// Stream prefix + remainder; Close() closes the upstream body.
					resp.Body = struct {
						io.Reader
						io.Closer
					}{io.MultiReader(bytes.NewReader(prefix), orig), orig}
				} else {
					// Whole body read; safe to close upstream now.
					_ = orig.Close()
					resp.Body = io.NopCloser(bytes.NewReader(prefix))
				}
				if len(capture) > 0 {
					// Redact only the spilled bytes; resp.Body was already
					// restored from the original prefix above and is untouched
					// on the client forward. When truncated, capture is an
					// incomplete-JSON prefix -> fail-open returns it raw.
					responseBodyRef = st.put(newBodyID(), redactTelemetryBody(capture, sp.supervisor.unmaskCredentials))
					if truncated {
						responseBodyRef.Truncated = true
					}
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				summary := err.Error()
				sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
					Timestamp:       time.Now(),
					SessionID:       sp.session.public.ID,
					Severity:        "error",
					ErrorClass:      "upstream_unreachable",
					Summary:         summary,
					UpstreamHost:    host,
					SuggestedAction: routeSuggestion("upstream_unreachable"),
				})
				http.Error(w, summary, http.StatusBadGateway)
			},
		}
		rw := newCaptureResponseWriter(w)
		aborted := serveAbortable(proxy, rw, r)
		latency := time.Since(start)
		sp.supervisor.recordRequest(sp.session.public.ID, model.RequestRecord{
			Timestamp:             start,
			SessionID:             sp.session.public.ID,
			Method:                r.Method,
			LogicalTargetHost:     host,
			ActualUpstreamHost:    host,
			Path:                  pathAndQuery(r.URL),
			StatusCode:            rw.StatusCode(),
			LatencyMS:             latency.Milliseconds(),
			StreamState:           detectStreamState(r, rw.HeaderSnapshot()),
			RequestHeaders:        r.Header.Clone(),
			BodyRef:               requestBodyRef,
			ResponseBodyRef:       responseBodyRef,
			ActiveProfileName:     ap.r.profileName,
			ActiveProfileProvider: ap.r.profileProvider,
		})
		if aborted {
			panic(http.ErrAbortHandler)
		}
	})
}

func (sp *sessionProxy) handleBlindTunnel(w http.ResponseWriter, r *http.Request, host, port string) {
	start := time.Now()
	sessionID := sp.session.public.ID
	address := net.JoinHostPort(host, port)
	logicalHost := normalizeProxyHost(host)
	// Per-request capture: snapshot the immutable posture at handler
	// entry, BEFORE any routing read. The egress config for DialContext below
	// comes from ap.r.egress (not sp.currentEgress()), so a Store(B)
	// landing between capture and dial cannot redirect the in-flight tunnel.
	// createSession installs a safe-zero *posture; the defensive
	// nil-coerce is belt-and-suspenders. The hook fires once per ServeHTTP
	// entry; nil in production.
	ap := sp.session.active.Load()
	if ap == nil {
		ap = &posture{}
	}
	if testHookAfterApCapture != nil {
		testHookAfterApCapture()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	targetConn, err := egress.DialContext(ctx, ap.r.egress, "tcp", address)
	if err != nil {
		sp.supervisor.recordError(sessionID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sessionID,
			Severity:        "error",
			ErrorClass:      "blind_tunnel_dial_failed",
			Summary:         fmt.Sprintf("target dial failed: %v", err),
			UpstreamHost:    logicalHost,
			SuggestedAction: "check target reachability and egress proxy configuration",
		})
		sp.supervisor.recordTrace(sessionID, model.TraceRecord{
			Timestamp: time.Now(),
			SessionID: sessionID,
			Category:  "connect",
			Summary:   "blind tunnel dial failed",
			Detail:    address,
		})
		http.Error(w, fmt.Sprintf("target dial failed: %v", err), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = targetConn.Close()
		sp.supervisor.recordError(sessionID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sessionID,
			Severity:        "error",
			ErrorClass:      "blind_tunnel_hijack_failed",
			Summary:         "response writer does not support hijack",
			UpstreamHost:    logicalHost,
			SuggestedAction: "retry with a standard HTTP/1.1 proxy client",
		})
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		_ = targetConn.Close()
		sp.supervisor.recordError(sessionID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sessionID,
			Severity:        "error",
			ErrorClass:      "blind_tunnel_hijack_failed",
			Summary:         fmt.Sprintf("hijack failed: %v", err),
			UpstreamHost:    logicalHost,
			SuggestedAction: "retry with a standard HTTP/1.1 proxy client",
		})
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	clientConn = sp.trackConn(clientConn)
	targetConn = sp.trackConn(targetConn)
	if err := writeConnectEstablished(clientConn); err != nil {
		_ = clientConn.Close()
		_ = targetConn.Close()
		sp.supervisor.recordError(sessionID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sessionID,
			Severity:        "error",
			ErrorClass:      "blind_tunnel_handshake_failed",
			Summary:         fmt.Sprintf("CONNECT handshake failed: %v", err),
			UpstreamHost:    logicalHost,
			SuggestedAction: "check client proxy behavior",
		})
		sp.supervisor.recordTrace(sessionID, model.TraceRecord{Timestamp: time.Now(), SessionID: sessionID, Category: "connect", Summary: "blind tunnel handshake failed", Detail: address})
		return
	}
	sp.supervisor.markSessionActive(sessionID)
	sp.supervisor.recordTrace(sessionID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sessionID,
		Category:  "connect",
		Summary:   "blind tunnel established",
		Detail:    address,
	})
	sp.supervisor.recordRequest(sessionID, model.RequestRecord{
		Timestamp:          start,
		SessionID:          sessionID,
		Method:             http.MethodConnect,
		LogicalTargetHost:  logicalHost,
		ActualUpstreamHost: logicalHost,
		Path:               address,
		StatusCode:         http.StatusOK,
		LatencyMS:          time.Since(start).Milliseconds(),
		StreamState:        model.StreamStateUnknown,
		// Stamp identity from the captured *posture (the blind-tunnel
		// branch closes over the captured ap).
		ActiveProfileName:     ap.r.profileName,
		ActiveProfileProvider: ap.r.profileProvider,
	})
	if sp.isClosed() {
		_ = clientConn.Close()
		_ = targetConn.Close()
		return
	}
	sp.wg.Add(2)
	go sp.proxyCopy(targetConn, clientConn)
	go sp.proxyCopy(clientConn, targetConn)
}

func (sp *sessionProxy) handleAnthropicMITM(w http.ResponseWriter, r *http.Request, logicalHost, port string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	clientConn = sp.trackConn(clientConn)
	if err := writeConnectEstablished(clientConn); err != nil {
		_ = clientConn.Close()
		sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{Timestamp: time.Now(), SessionID: sp.session.public.ID, Category: "mitm", Summary: "connect handshake failed", Detail: net.JoinHostPort(logicalHost, port)})
		return
	}

	// One deadline covers reading the ClientHello below and the TLS
	// handshake, so a client that opens the tunnel and goes silent is dropped.
	_ = clientConn.SetDeadline(time.Now().Add(sp.mitmTimeout))

	// Native TLS: peek CC's raw ClientHello before the CC<->ccwrap tls.Server
	// handshake consumes it, so this connection's requests go upstream
	// mirroring its shape (hellostack.go). tls.Server gets a replay conn, so
	// the inner handshake is unaffected. Native TLS off => exact prior path.
	var hello *helloStack
	if ap := sp.session.active.Load(); ap != nil && ap.l.nativeTLS {
		var skippedH2 bool
		clientConn, hello, skippedH2 = captureClientHello(sp.session, clientConn)
		if skippedH2 {
			sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
				Timestamp: time.Now(),
				SessionID: sp.session.public.ID,
				Category:  "native_tls",
				Summary:   "client offers HTTP/2, which ccwrap cannot mirror upstream; its requests use the session's default TLS fingerprint",
				Detail:    net.JoinHostPort(logicalHost, port),
			})
		}
	}

	cert, err := sp.supervisor.ca.IssueServerCert(logicalHost)
	if err != nil {
		sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sp.session.public.ID,
			Severity:        "error",
			ErrorClass:      "tls_mitm_failed",
			Summary:         fmt.Sprintf("issue MITM cert failed: %v", err),
			UpstreamHost:    logicalHost,
			SuggestedAction: "run `ccwrap doctor` and verify CA state",
		})
		_ = clientConn.Close()
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	_ = tlsConn.SetDeadline(time.Now().Add(sp.mitmTimeout))
	if err := tlsConn.Handshake(); err != nil {
		sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sp.session.public.ID,
			Severity:        "error",
			ErrorClass:      "tls_mitm_failed",
			Summary:         fmt.Sprintf("MITM handshake failed: %v", err),
			UpstreamHost:    logicalHost,
			SuggestedAction: "confirm child CA env attachment and trust behavior",
		})
		sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
			Timestamp: time.Now(),
			SessionID: sp.session.public.ID,
			Category:  "mitm",
			Summary:   "handshake failed",
			Detail:    logicalHost,
		})
		_ = tlsConn.Close()
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "mitm",
		Summary:   "handshake succeeded",
		Detail:    net.JoinHostPort(logicalHost, port),
	})

	// Record each request's header order and casing as Claude Code wrote it,
	// for the upstream re-lay (wireorder.go).
	wire := &wireOrderRecorder{Conn: tlsConn}
	server := &http.Server{
		Handler:           sp.mitmHandler(logicalHost),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return withHelloStack(withWireOrderRecorder(ctx, wire), hello)
		},
	}
	listener := newSingleConnListener(wire)
	if !sp.registerInnerServer(server) {
		_ = listener.Close()
		_ = tlsConn.Close()
		return
	}
	sp.wg.Add(1)
	defer sp.wg.Done()
	defer sp.unregisterInnerServer(server)
	_ = server.Serve(listener)
}

func (sp *sessionProxy) mitmHandler(logicalHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sp.supervisor.markSessionActive(sp.session.public.ID)
		start := time.Now()
		clientOrder := popWireOrder(r)
		// Per-request capture: snapshot the immutable posture at handler
		// entry, BEFORE any routing read. Every routing decision (third-party
		// blocker, resolveUpstream, and everything in forwardToUpstream)
		// closes over `ap` — a mid-request Store(B) cannot tear the in-flight
		// request. createSession installs a safe-zero *posture; the
		// defensive nil-coerce is belt-and-suspenders. The hook fires once per
		// ServeHTTP entry; nil in production.
		ap := sp.session.active.Load()
		if ap == nil {
			ap = &posture{}
		}
		if testHookAfterApCapture != nil {
			testHookAfterApCapture()
		}
		if sp.handleThirdPartySyntheticOrBlock(w, r, logicalHost, start, ap) {
			return
		}
		base, upstreamHost, errClass, err := sp.resolveUpstream(logicalHost, ap)
		if err != nil {
			sp.refuseUnroutable(w, logicalHost, errClass, err)
			return
		}
		sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
			Timestamp: time.Now(),
			SessionID: sp.session.public.ID,
			Category:  "route",
			Summary:   "forwarding request",
			// Trace strip: base may carry upstream-userinfo (the resolved
			// route's URL preserves whatever preflight gave it). The public
			// trace must never leak the credential. Wrap base.String() ONLY
			// — logicalHost is a host label.
			Detail: fmt.Sprintf("%s -> %s", logicalHost, stripUserinfoString(base.String())),
		})
		applyAuth := isAnthropicAPIHost(logicalHost)
		// Request-time fail-closed at the MITM boundary. Mirrors the gate in
		// handleForwardProxyRequest — same invariant applied at both
		// forwarding paths.
		if sp.maybeRefuseAuthMissing(w, r, ap, applyAuth, logicalHost, start) {
			return
		}
		hello := helloStackFrom(r.Context())
		if !ap.l.nativeTLS {
			hello = nil
		}
		baseForRequest := cloneURL(base)
		sp.forwardToUpstream(w, r, ap, start, forwardTarget{
			logicalHost:  logicalHost,
			upstreamHost: upstreamHost,
			applyAuth:    applyAuth,
			url:          func(in *url.URL) *url.URL { return joinTargetURL(baseForRequest, in) },
			hello:        hello,
			wireOrder:    clientOrder,
		})
	})
}
