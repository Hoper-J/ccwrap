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
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

type sessionProxy struct {
	supervisor       *Supervisor
	session          *sessionState
	listener         net.Listener
	httpServer       *http.Server
	closeOnce        sync.Once
	mu               sync.Mutex
	closed           bool
	inner            map[*http.Server]struct{}
	tracked          map[net.Conn]struct{}
	transports       map[string]*http.Transport
	nativeTransports map[string]*http.Transport // SEPARATE native-TLS cache; Anthropic-route ReverseProxy only. Never shared with telemetry/forward.
	wg               sync.WaitGroup
	pinnedTelemetry  map[string]struct{} // telemetry hosts that rejected our MITM cert this session -> fall back to tunnel; guarded by mu
	webAllowedHosts  map[string]struct{} // extra Hosts admitted to info endpoints (CCWRAP_WEB_ALLOWED_HOSTS, launch-time snapshot); read-only after construction. See webhosts.go.
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
	sp := &sessionProxy{
		supervisor:       supervisor,
		session:          session,
		listener:         ln,
		inner:            map[*http.Server]struct{}{},
		tracked:          map[net.Conn]struct{}{},
		transports:       map[string]*http.Transport{},
		nativeTransports: map[string]*http.Transport{},
		webAllowedHosts:  webAllowedHostsFromEnv(),
	}
	sp.httpServer = &http.Server{
		Handler:           http.HandlerFunc(sp.handleProxy),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		err := sp.httpServer.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			supervisor.logger.Printf("session proxy serve error: %v", err)
		}
	}()
	return sp, nil
}

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
		transports := make([]*http.Transport, 0, len(sp.transports))
		for _, tr := range sp.transports {
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

func (sp *sessionProxy) handleInfoRequest(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding guard (see webhosts.go): refuse info requests whose Host
	// is neither loopback-shaped nor explicitly allowed. Fires BEFORE any
	// dispatch so no endpoint — including the page that embeds the profile
	// token — is reachable from a rebound hostname. Deliberately does not
	// echo r.Host back (no reflected content for a hostile origin).
	if !sp.infoHostAllowed(r.Host) {
		http.Error(w, "ccwrap: refusing info request for an unrecognized Host header (DNS-rebinding guard); if you are deliberately serving this dashboard through a tunnel or reverse proxy, relaunch with CCWRAP_WEB_ALLOWED_HOSTS=<that-hostname>", http.StatusMisdirectedRequest)
		return
	}
	// Method-aware dispatch for the browser-facing profile endpoints. The
	// GET/HEAD-only guard below would reject POST /profile/switch before path
	// inspection; this branch handles them first and returns. Unknown method on
	// these paths is 405 (not 404); unknown PATHS still fall through to the
	// default 404.
	switch r.URL.Path {
	case "/profile/switch":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sp.handleProfileSwitch(w, r)
		return
	case "/profile/test":
		sp.handleProfileTest(w, r)
		return
	case "/profile/test-egress":
		sp.handleEgressProbe(w, r)
		return
	case "/profile/set-default":
		sp.handleProfileSetDefault(w, r)
		return
	case "/profile/add":
		sp.handleProfileAdd(w, r)
		return
	case "/profile/edit":
		sp.handleProfileEdit(w, r)
		return
	case "/profile/rm":
		sp.handleProfileRm(w, r)
		return
	case "/profile/catalog":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sp.handleProfileCatalog(w, r)
		return
	case "/capture/bodies":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sp.handleCaptureBodies(w, r)
		return
	case "/capture/telemetry":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sp.handleCaptureTelemetry(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "ccwrap session proxy supports CONNECT, HTTP forward proxy requests, plus GET/HEAD info endpoints", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/", "":
		sp.handleInfoPage(w, r)
	case "/healthz":
		sp.handleInfoHealthz(w, r)
	case "/recent":
		sp.handleInfoRecent(w, r)
	case "/recent/body":
		sp.handleInfoRecentBody(w, r)
	case "/native-tls":
		sp.handleNativeTLSInfo(w, r)
	case "/native-tls/clienthello.bin":
		sp.handleNativeTLSClientHello(w, r)
	case "/events":
		sp.handleInfoEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (sp *sessionProxy) handleInfoPage(w http.ResponseWriter, r *http.Request) {
	sess := sp.session.snapshot()
	requests := sp.supervisor.listRequests(sess.ID)
	errors := sp.supervisor.listErrors(sess.ID)
	trace := sp.supervisor.listTrace(sess.ID)
	lastActivity := latestActivityTimestamp(requests, errors, trace)

	const activityCap = 50
	classCounts := map[string]int{}
	for _, rec := range requests {
		classCounts[recordClass(rec)]++
	}
	classCounts["error"] += len(errors)
	classCounts["trace"] += len(trace)
	// ?class= preselects the Activity filter chip and class-filters the
	// first-paint rows (filter BEFORE the newest-N cap — the server twin of
	// the JS rebuild's filter-aware capping, so the chip and the visible
	// list can never disagree). On ended (non-live) pages the chips render
	// as plain links carrying this param: the no-JS filter path. Unknown
	// values fall back to the default class.
	defaultClass := defaultActivityClass(classCounts["forwarded-api"])
	switch q := r.URL.Query().Get("class"); q {
	case "all", "forwarded-api", "synthetic", "tunnel", "telemetry", "error", "trace":
		defaultClass = q
	}
	// unmask flows from the launch-fixed CCWRAP_UNMASK_CREDENTIALS env so
	// credential headers render raw in the drawer when the user opted in.
	// sess.CaptureBodiesUnmasked is the session-mirrored copy of
	// supervisor.unmaskCredentials.
	fReqs, fErrs, fTrace := filterActivityClass(defaultClass, requests, errors, trace)
	activityRows := unifiedActivityRows(fReqs, fErrs, fTrace, activityCap, false, sess.CaptureBodiesUnmasked)
	activityEmpty := webActivityEmptyLive
	if sess.State == model.StateEnded {
		activityEmpty = "No traffic was recorded for this session."
	}
	classes := []webClassCount{
		{"all", "All", len(requests) + len(errors) + len(trace)},
		{"forwarded-api", "Forwarded API", classCounts["forwarded-api"]},
		{"synthetic", "Synthetic", classCounts["synthetic"]},
		{"tunnel", "Tunnel", classCounts["tunnel"]},
		{"telemetry", "Telemetry", classCounts["telemetry"]},
		{"error", "Errors", classCounts["error"]},
		{"trace", "Trace", classCounts["trace"]},
	}
	bootRequests := make([]classifiedRecord, 0, len(requests))
	for _, rec := range requests {
		bootRequests = append(bootRequests, classifiedRecord{Class: recordClass(rec), RequestRecord: rec})
	}

	heroState, heroVariant := webHeroVariant(sess)
	heroMeta := ""
	switch {
	case sess.State == model.StateEnded && sess.EndedAt != nil:
		heroMeta = "ended " + activityAgeLabel(*sess.EndedAt)
		if !sess.CreatedAt.IsZero() {
			heroMeta += fmt.Sprintf(" · %s wall", sess.EndedAt.Sub(sess.CreatedAt).Round(time.Second))
		}
	case sess.ClaudePID > 0:
		heroMeta = fmt.Sprintf("claude pid %d", sess.ClaudePID)
		if up := uptimeLabel(sess.CreatedAt); up != "" {
			heroMeta += " · up " + up
		}
	}

	// Profile cell variant inputs. Cheap: one os.Stat + one disk read on each
	// /info page render (rare; not on hot SSE path).
	profilesPath := profiles.DefaultPath(sp.supervisor.paths.StateDir)
	hasProfilesFile := false
	profileCount := 0
	if file, err := profiles.Load(profilesPath); err == nil && file != nil {
		hasProfilesFile = true
		profileCount = len(file.Profiles)
	}

	claudeID := latestClaudeSessionID(requests)
	claudeLabel := ""
	// Tab title carries the short conversation id so parallel sessions are
	// tellable apart at tab level; updateClaudeSession mirrors this live.
	title := "CCWRAP Session"
	if claudeID != "" {
		claudeLabel = "session " + shortSessionID(claudeID)
		title = "CCWRAP · " + claudeLabel
	}

	page := webPageData{
		Title:             title,
		Heading:           "CCWRAP Session",
		FaviconHref:       faviconHref(heroVariant),
		Subtitle:          fallback(sess.Name, "Local diagnostics for this session."),
		SessionLabel:      claudeLabel,
		ClaudeSessionFull: claudeID,
		HeroState:         heroState,
		HeroVariant:       heroVariant,
		HeroMeta:          heroMeta,
		HeroSentence:      ui.SessionPosture(&sess, lastWebError(errors)),
		Ribbon:            webRibbonFromSession(sess, activityAgeLabel(lastActivity), hasProfilesFile, profileCount),
		Summary:           webSummaryFromSession(sess),
		Links: []webLink{
			{Label: "Health JSON", Href: "/healthz", Icon: "i-activity"},
			{Label: "Recent JSON", Href: "/recent", Icon: "i-clock"},
		},
		ActivityTitle:       "Activity",
		ActivityEmpty:       activityEmpty,
		ActivityRows:        activityRows,
		Classes:             classes,
		DefaultClass:        defaultClass,
		LiveEnabled:         sess.State != model.StateEnded,
		LastActivityLabel:   activityAgeLabel(lastActivity),
		LastActivityRFC3339: activityRFC3339(lastActivity),
		HasProfilesFile:     hasProfilesFile,
		ProfileCount:        profileCount,
		BootstrapB64: bootstrapB64(pageBootstrap{
			EventsURL:    "/events",
			LastActivity: activityRFC3339(lastActivity),
			Session:      &sess,
			Requests:     bootRequests,
			// Full retention rings (not a truncated 8/10): the JS mirror counts
			// state-array lengths for the filter pills, so a short bootstrap
			// window would corrupt the counts on load. requests above is already
			// the full ring (bootRequests); errors/trace match it.
			Errors:              tailErrorsForWeb(errors, maxSessionErrors),
			Trace:               tailTraceForWeb(trace, maxSessionTrace),
			HeaderDenyList:      ui.CredentialDenyList(),
			ProfileToken:        sp.session.profileToken,
			ClaudeSessionHeader: claudeSessionHeader,
		}),
	}
	renderWebPage(w, page)
}

func (sp *sessionProxy) handleInfoEvents(w http.ResponseWriter, r *http.Request) {
	sp.supervisor.serveEventStream(w, r, sp.session.public.ID)
}

func (sp *sessionProxy) handleInfoHealthz(w http.ResponseWriter, r *http.Request) {
	sess := sp.session.snapshot()
	writeJSON(w, map[string]any{
		"schema_version": model.ControlAPIVersion,
		"session": map[string]any{
			"id":                                     sess.ID,
			"state":                                  sess.State,
			"proxy":                                  proxyInfoURL(sess.ProxyListenAddr),
			"session_url":                            "/",
			"upstream":                               sess.ExactUpstreamBase,
			"route":                                  sess.RouteSource,
			"route_config_source":                    sess.RouteConfigSource,
			"route_class":                            sess.RouteClass,
			"model_alias_mode":                       sess.ModelAliasMode,
			"model_alias_count":                      sess.ModelAliasCount,
			"model_alias_fingerprint":                sess.ModelAliasFingerprint,
			"model_alias_provider_model_passthrough": sess.ModelAliasProviderModelPassthrough,
			"upstream_header_count":                  sess.UpstreamHeaderCount,
			"upstream_header_source":                 sess.UpstreamHeaderSource,
			"upstream_header_fingerprint":            sess.UpstreamHeaderFingerprint,
			"auth_mode":                              sess.AuthMode,
			"auth_source":                            sess.AuthSource,
			"auth_config_source":                     sess.AuthConfigSource,
			"auth_policy":                            sess.AuthPolicy,
			"auth_bootstrap":                         sess.AuthBootstrap,
			"auth_bootstrap_kind":                    sess.AuthBootstrapKind,
			"egress_mode":                            sess.EgressMode,
			"egress_source":                          sess.EgressSource,
			"egress_summary":                         sess.EgressSummary,
		},
		"counts": map[string]any{
			"requests": sess.RecentRequestCount,
			"errors":   sess.RecentErrorCount,
		},
	})
}

func (sp *sessionProxy) handleInfoRecent(w http.ResponseWriter, r *http.Request) {
	sess := sp.session.snapshot()
	writeJSON(w, map[string]any{
		"schema_version": model.ControlAPIVersion,
		"session": map[string]any{
			"id":                                     sess.ID,
			"name":                                   sess.Name,
			"state":                                  sess.State,
			"proxy_listen_addr":                      sess.ProxyListenAddr,
			"session_url":                            "/",
			"proxy":                                  proxyInfoURL(sess.ProxyListenAddr),
			"api_base_url":                           sess.APIBaseURL,
			"route_source":                           sess.RouteSource,
			"route_config_source":                    sess.RouteConfigSource,
			"route_class":                            sess.RouteClass,
			"model_alias_mode":                       sess.ModelAliasMode,
			"model_alias_count":                      sess.ModelAliasCount,
			"model_alias_fingerprint":                sess.ModelAliasFingerprint,
			"model_alias_provider_model_passthrough": sess.ModelAliasProviderModelPassthrough,
			"upstream_header_count":                  sess.UpstreamHeaderCount,
			"upstream_header_source":                 sess.UpstreamHeaderSource,
			"upstream_header_fingerprint":            sess.UpstreamHeaderFingerprint,
			"auth_mode":                              sess.AuthMode,
			"auth_source":                            sess.AuthSource,
			"auth_config_source":                     sess.AuthConfigSource,
			"auth_policy":                            sess.AuthPolicy,
			"auth_bootstrap":                         sess.AuthBootstrap,
			"auth_bootstrap_kind":                    sess.AuthBootstrapKind,
			"exact_upstream_host":                    sess.ExactUpstreamHost,
			"exact_upstream_base":                    sess.ExactUpstreamBase,
			"egress_mode":                            sess.EgressMode,
			"egress_source":                          sess.EgressSource,
			"egress_summary":                         sess.EgressSummary,
			"fail_policy":                            sess.FailPolicy,
			"recent_request_count":                   sess.RecentRequestCount,
			"recent_error_count":                     sess.RecentErrorCount,
			"active_profile_name":                    sess.ActiveProfileName,     // reconnect path
			"active_profile_provider":                sess.ActiveProfileProvider, // reconnect path
			"capture_bodies":                         sess.CaptureBodies,         // capture-toggle reconnect path: page reload + SSE rejoin re-syncs the Bodies cell from /recent
			"capture_telemetry":                      sess.CaptureTelemetry,      // telemetry-capture toggle; reconnect path re-syncs the Bodies cell from /recent
			"native_tls":                             sess.NativeTLS,             // native-tls state (active/blocked); reconnect path re-syncs the NATIVE TLS cell from /recent
			"native_tls_fallbacks":                   sess.NativeTLSFallbacks,    // native-tls block count; reconnect path
			"native_tls_loaded":                      sess.NativeTLSLoaded,       // mirrored hello was LOADED (CCWRAP_NATIVE_TLS_HELLO); reconnect path keeps the "loaded" detail
			"capture_bodies_unmasked":                sess.CaptureBodiesUnmasked, // CCWRAP_UNMASK_CREDENTIALS=1 surface; immutable for the session
			"missing_auth_env":                       sess.MissingAuthEnv,        // empty in healthy + Case B; non-empty in Case A — Auth cell renders missing-state on reconnect
		},
		// Full retention rings so a reconnect resync restores the same window
		// the bootstrap shipped (not a narrower 20/20/30 that would shrink the
		// activity feed below LIMITS.activity). Credential headers are already
		// masked store-side, so the full window carries no raw secret.
		"requests": tailRequestsForWeb(sp.supervisor.listRequests(sess.ID), maxSessionRequests),
		"errors":   tailErrorsForWeb(sp.supervisor.listErrors(sess.ID), maxSessionErrors),
		"trace":    tailTraceForWeb(sp.supervisor.listTrace(sess.ID), maxSessionTrace),
	})
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

// SECURITY: id is attacker-controlled and already URL-decoded by
// r.URL.Query().Get. isBodyID rejects anything that is not a bare
// lowercase-hex token before it can reach the filesystem, closing the
// ?id=../../… path-traversal / arbitrary-*.json read on this
// data-egress endpoint. bodyStore.load has independent defense-in-depth
// containment.
func (sp *sessionProxy) handleInfoRecentBody(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
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

// handleNativeTLSInfo serves the read-only GET /native-tls JSON endpoint:
// the mirrored Claude Code ClientHello (hex) plus its computed JA3/JA4/
// peetprint fingerprints. Nil-safe: before the first Anthropic dial the
// mirrored hello is unset and the endpoint reports captured:false without
// touching the raw bytes. The hello carries no credentials (TLS handshake
// precedes any HTTP request), so the hex/fingerprints leak no secrets.
func (sp *sessionProxy) handleNativeTLSInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := sp.session.mirroredHelloRaw.Load()
	if p == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"captured": false})
		return
	}
	raw := *p
	source := "captured"
	note := "Claude Code ClientHello that ccwrap mirrors; outbound SNI is rewritten per dial, so all three fingerprints equal what is sent upstream."
	if sp.session.nativeTLSHelloLoaded {
		source = "loaded"
		note = "Loaded from CCWRAP_NATIVE_TLS_HELLO — a pinned fingerprint, not the live client's. Outbound SNI is rewritten per dial."
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
	_ = json.NewEncoder(w).Encode(out)
}

// handleNativeTLSClientHello serves the raw mirrored ClientHello bytes as a
// download (GET /native-tls/clienthello.bin). Nil-safe: before the first
// Anthropic dial the mirrored hello is unset and the endpoint 404s. The hello
// carries no credentials (the TLS handshake precedes any HTTP request), so the
// raw bytes leak no secrets.
func (sp *sessionProxy) handleNativeTLSClientHello(w http.ResponseWriter, r *http.Request) {
	p := sp.session.mirroredHelloRaw.Load()
	if p == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="clienthello.bin"`)
	_, _ = w.Write(*p)
}

// handleProfileCatalog returns the browser-facing fresh-fetched catalog.
// Wrapper over Supervisor.profileCatalogFor — same in-process call, no
// socket trip. GET/HEAD only (the dispatcher enforces this). Emits the
// compact wire form (no indent) that control/types_test.go locks:
// "has_profiles_file":false and "items":[] without inter-token
// whitespace, so browser parsing and contract assertions stay in sync.
func (sp *sessionProxy) handleProfileCatalog(w http.ResponseWriter, r *http.Request) {
	resp := sp.supervisor.profileCatalogFor(sp.session)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleProfileSwitch is the browser-facing POST endpoint that wraps
// Supervisor.SwitchProfile. CSRF token check runs FIRST — before body
// decode — so a missing/wrong token consumes no resources and has no
// side effects (defense-in-depth). Always returns 200 with the
// SwitchOutcome JSON on a valid request; outcome.Result categorizes
// success/refused/rejected.
func (sp *sessionProxy) handleProfileSwitch(w http.ResponseWriter, r *http.Request) {
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden: csrf token missing or invalid", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	outcome := sp.supervisor.SwitchProfile(sp.session.public.ID, req.Name)
	writeJSON(w, outcome)
}

// handleCaptureBodies is the browser-facing POST endpoint that flips the
// per-session request-body capture flag at runtime, via the hot-swap
// invariant (Supervisor.SetCaptureBodies — atomic.Pointer swap +
// publicProjection mirror under sess.mu). CSRF check runs FIRST so a missing
// or wrong X-CCWRAP-Profile-Token consumes no resources and has no side effects
// (defense-in-depth — same pattern as /profile/switch).
//
// Wire contract:
//
//	POST /capture/bodies
//	  X-CCWRAP-Profile-Token: <token>
//	  Content-Type: application/json
//	  body: {"enable": <bool>}    // REQUIRED; absent ⇒ 400, not implied false
//	→ 200 {"enabled": <bool>}     // confirms applied state (idempotent on same value)
//
// Past bodies remain inspectable after a toggle-off; the toggle only gates
// new request captures.
func (sp *sessionProxy) handleCaptureBodies(w http.ResponseWriter, r *http.Request) {
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden: csrf token missing or invalid", http.StatusForbidden)
		return
	}
	var req struct {
		// *bool so an absent field is distinguishable from "enable":false —
		// preventing an accidentally-empty body from silently disabling capture.
		Enable *bool `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Enable == nil {
		http.Error(w, "missing required field: enable", http.StatusBadRequest)
		return
	}
	snap, err := sp.supervisor.SetCaptureBodies(sp.session.public.ID, *req.Enable)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"enabled": snap.CaptureBodies})
}

// handleCaptureTelemetry is the browser-facing POST endpoint that flips the
// per-session telemetry-capture toggle (the opt-in transparent telemetry MITM).
//
// Wire contract:
//
//	POST /capture/telemetry
//	  X-CCWRAP-Profile-Token: <token>
//	  Content-Type: application/json
//	  body: {"enable": <bool>}    // REQUIRED; absent -> 400, not implied false
//	-> 200 {"enabled": <bool>}    // confirms applied state (idempotent on same value)
func (sp *sessionProxy) handleCaptureTelemetry(w http.ResponseWriter, r *http.Request) {
	if !sp.session.matchProfileToken(r.Header.Get("X-CCWRAP-Profile-Token")) {
		http.Error(w, "forbidden: csrf token missing or invalid", http.StatusForbidden)
		return
	}
	var req struct {
		// *bool so an absent field is distinguishable from "enable":false —
		// preventing an accidentally-empty body from silently disabling capture.
		Enable *bool `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Enable == nil {
		http.Error(w, "missing required field: enable", http.StatusBadRequest)
		return
	}
	snap, err := sp.supervisor.SetCaptureTelemetry(sp.session.public.ID, *req.Enable)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"enabled": snap.CaptureTelemetry})
}

func (sp *sessionProxy) handleForwardProxyRequest(w http.ResponseWriter, r *http.Request) {
	sp.supervisor.markSessionActive(sp.session.public.ID)
	start := time.Now()
	// Per-request capture: snapshot the immutable posture at handler
	// entry, BEFORE any routing read. Every routing decision in this request
	// (third-party blocker, resolveForwardTarget, transport, upstream headers,
	// auth override, capture-bodies gate, record stamping) closes over this
	// `ap` — a mid-request Store(B) cannot tear the in-flight request.
	// createSession installs a safe-zero *posture, so ap is never nil
	// here; the defensive nil-coerce is belt-and-suspenders against any future
	// regression. The seam fires once per ServeHTTP entry; nil in production.
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
		sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
			Timestamp:       time.Now(),
			SessionID:       sp.session.public.ID,
			Severity:        "error",
			ErrorClass:      errClass,
			Summary:         err.Error(),
			UpstreamHost:    logicalHost,
			SuggestedAction: routeSuggestion(errClass),
		})
		http.Error(w, err.Error(), http.StatusBadGateway)
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
	// Capture tap, before ServeHTTP forwards/consumes the body.
	// Synthetic/blocked requests returned earlier via
	// handleThirdPartySyntheticOrBlock and never reach here. This tee MUST
	// precede any r.Body-consuming step — both modelalias.RewriteRequest (next)
	// and proxy.ServeHTTP (below).
	var bodyRefForThisRequest *model.RequestBodyRef
	if ap.l.captureBodies && r.Body != nil {
		b, rerr := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		// Best-effort, faithfulness-first: only record a BodyRef when the
		// full inbound body was read. On a read error the (partial) body is
		// still forwarded unchanged, but it is NOT spilled — the inspector
		// must never show a silently-truncated body as complete. Disk-write
		// failure is handled separately (endpoint 404).
		if st := sp.session.bodies; st != nil && rerr == nil && len(b) > 0 {
			// OAuth-host bodies: redact JSON-field credentials
			// (refresh_token, access_token, ...) before spilling. The
			// original `b` already went to r.Body above, so the upstream
			// still receives the unmodified body — redaction affects only
			// the ccwrap-internal observability surface (drawer + spill file).
			spillBytes := b
			if shouldRedactBody(logicalHost, r.URL.Path) {
				spillBytes = redactJSONBody(b, sp.supervisor.unmaskCredentials)
			}
			bodyRefForThisRequest = st.put(newBodyID(), spillBytes)
		}
	}
	// Model alias rewrite. Mirrors the mitmHandler hook so HTTP forward
	// and CONNECT/MITM paths apply aliasing identically — without this,
	// a third-party gateway profile configured with mode=http base_url
	// would have logical model IDs (e.g. "claude-haiku-4-5") leak to the
	// upstream when the client used plain-HTTP proxy semantics. Same
	// applyAuth gate as the MITM path (isAnthropicAPIHost on logical),
	// same error class, same fail-closed BadGateway disposition.
	var aliasCtx *modelalias.Context
	if applyAuth {
		var rewriteErr error
		aliasCtx, rewriteErr = modelalias.RewriteRequest(r, ap.r.modelAlias)
		if rewriteErr != nil {
			sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
				Timestamp:       time.Now(),
				SessionID:       sp.session.public.ID,
				Severity:        "error",
				ErrorClass:      "model_alias_rewrite_failed",
				Summary:         rewriteErr.Error(),
				UpstreamHost:    logicalHost,
				SuggestedAction: "use Claude logical model IDs and configure CCWRAP modelAliases for provider-specific upstream IDs",
			})
			// Anchor the spilled inbound body to a partial RequestRecord
			// so the inspect drawer can still surface what was attempted.
			// Without this, captureBodies=on + rewrite-rejected requests
			// leave orphan body files on disk (bounded by bodystore LRU,
			// but unreachable through /recent). Mirrors the same pattern
			// in mitmHandler.
			sp.supervisor.recordRequest(sp.session.public.ID, model.RequestRecord{
				// Timestamp is the request boundary (handler entry), matching
				// the success path below. Latency runs from start to the
				// rewrite-rejection point. Stamping time.Now() (the failure
				// moment) here would order the record after a sibling success
				// that entered the handler later.
				Timestamp:             start,
				SessionID:             sp.session.public.ID,
				Method:                r.Method,
				LogicalTargetHost:     logicalHost,
				ActualUpstreamHost:    actualUpstreamHost,
				Path:                  pathForRecord(r.URL, isAnthropicHost(logicalHost)),
				StatusCode:            http.StatusBadGateway,
				LatencyMS:             time.Since(start).Milliseconds(),
				StreamState:           model.StreamStateUnknown,
				RequestHeaders:        r.Header.Clone(),
				BodyRef:               bodyRefForThisRequest,
				ActiveProfileName:     ap.r.profileName,
				ActiveProfileProvider: ap.r.profileProvider,
			})
			http.Error(w, rewriteErr.Error(), http.StatusBadGateway)
			return
		}
	}
	// Upstream-view tap: same rationale as in mitmHandler. Captures the
	// post-rewrite body so the inspect drawer can show what actually
	// reached the upstream. Skip when modelalias did not rewrite — the
	// drawer's "upstream body" is identical to the inbound capture and
	// a duplicate spill wastes disk.
	var upstreamBodyRefForThisRequest *model.RequestBodyRef
	if ap.l.captureBodies && aliasCtx != nil && aliasCtx.Rewritten && r.Body != nil {
		b, rerr := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		if st := sp.session.bodies; st != nil && rerr == nil && len(b) > 0 {
			spillBytes := b
			if shouldRedactBody(logicalHost, r.URL.Path) {
				spillBytes = redactJSONBody(b, sp.supervisor.unmaskCredentials)
			}
			upstreamBodyRefForThisRequest = st.put(newBodyID(), spillBytes)
		}
	}
	proxy := &httputil.ReverseProxy{
		Transport:     sp.upstreamTransportFor(logicalHost, ap.r.egress, ap.l.nativeTLS),
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = cloneURL(target)
			pr.Out.Host = target.Host
			pr.Out.RequestURI = ""
			pr.Out.Header.Del("Proxy-Connection")
			pr.Out.Header.Del("Proxy-Authorization")
			// When modelalias normalizes the response body, we must see
			// an uncompressed upstream stream (RewriteResponse re-encodes
			// the logical-name mapping). Mirrors mitmHandler.
			if aliasCtx != nil && aliasCtx.NormalizeResponse {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
			if applyAuth {
				upstreamheaders.Apply(pr.Out.Header, copyUpstreamHeaders(ap.r.upstreamHeaders.Headers))
				applyAuthOverride(pr.Out.Header, ap.r.overrideAuth)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			return modelalias.RewriteResponse(resp, aliasCtx)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			cls := classifyUpstreamError(err, errClass)
			sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
				Timestamp:       time.Now(),
				SessionID:       sp.session.public.ID,
				Severity:        "error",
				ErrorClass:      cls,
				Summary:         err.Error(),
				UpstreamHost:    actualUpstreamHost,
				SuggestedAction: routeSuggestion(cls),
			})
			sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
				Timestamp: time.Now(),
				SessionID: sp.session.public.ID,
				Category:  "upstream",
				Summary:   "forward proxy request failed",
				Detail:    err.Error(),
			})
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
	}
	rw := newCaptureResponseWriter(w)
	proxy.ServeHTTP(rw, r)
	latency := time.Since(start)
	sp.supervisor.recordRequest(sp.session.public.ID, model.RequestRecord{
		Timestamp:          start,
		SessionID:          sp.session.public.ID,
		Method:             r.Method,
		LogicalTargetHost:  logicalHost,
		ActualUpstreamHost: actualUpstreamHost,
		Path:               pathForRecord(r.URL, isAnthropicHost(logicalHost)),
		StatusCode:         rw.StatusCode(),
		LatencyMS:          latency.Milliseconds(),
		StreamState:        detectStreamState(r, rw.HeaderSnapshot()),
		// Inbound headers, cloned (the record outlives the request in a
		// 250-deep ring; do not alias r.Header).
		RequestHeaders: r.Header.Clone(),
		// File-backed body ref (nil unless capture on).
		BodyRef: bodyRefForThisRequest,
		// Post-rewrite body — nil unless modelalias actually changed it.
		UpstreamBodyRef: upstreamBodyRefForThisRequest,
		// Stamp identity from the captured *posture — the same
		// posture the request was routed under, so a post-switch request
		// in flight is recorded against the posture-it-ran-on, not the
		// latest posture.
		ActiveProfileName:     ap.r.profileName,
		ActiveProfileProvider: ap.r.profileProvider,
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
	_ = tlsConn.SetDeadline(time.Now().Add(20 * time.Second))
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
		proxy.ServeHTTP(rw, r)
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

	// Native-TLS opt-in: synchronously mirror CC's raw ClientHello before the
	// CC<->ccwrap tls.Server handshake consumes it, so the native-TLS upstream
	// dialer can replay it. Captures once per session; hands tls.Server a replay
	// conn so the inner handshake is unaffected. Default off => exact prior path.
	if ap := sp.session.active.Load(); ap != nil && ap.l.nativeTLS {
		clientConn = captureMirroredHello(sp.session, clientConn)
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
	_ = tlsConn.SetDeadline(time.Now().Add(20 * time.Second))
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

	server := &http.Server{
		Handler:           sp.mitmHandler(logicalHost),
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

func (sp *sessionProxy) mitmHandler(logicalHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sp.supervisor.markSessionActive(sp.session.public.ID)
		start := time.Now()
		// Per-request capture: snapshot the immutable posture at handler
		// entry, BEFORE any routing read. Every routing decision (third-party
		// blocker, resolveUpstream, capture-bodies gate, model alias rewrite,
		// transport selection, upstream headers + auth override in the closure)
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
		// Declared here so it is in scope at the recordRequest call
		// below; set only by the pre-rewrite tee. Synthetic/blocked
		// requests return before the tee ⇒ stays nil.
		var bodyRefForThisRequest *model.RequestBodyRef
		if sp.handleThirdPartySyntheticOrBlock(w, r, logicalHost, start, ap) {
			return
		}
		base, upstreamHost, errClass, err := sp.resolveUpstream(logicalHost, ap)
		if err != nil {
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
		baseForRequest := cloneURL(base)
		upstreamHostForRequest := upstreamHost
		applyAuth := isAnthropicAPIHost(logicalHost)
		// Request-time fail-closed at the MITM boundary. Mirrors the gate in
		// handleForwardProxyRequest — same invariant applied at both
		// forwarding paths.
		if sp.maybeRefuseAuthMissing(w, r, ap, applyAuth, logicalHost, start) {
			return
		}
		// Capture tap. MUST be BEFORE modelalias.RewriteRequest below: the
		// rewrite consumes/replaces r.Body, so a post-rewrite tap sees nothing
		// (empirically established). Tee once, restore r.Body for the forward,
		// hand the buffer to the async writer.
		if ap.l.captureBodies && r.Body != nil {
			b, rerr := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
			// Best-effort, faithfulness-first: only record a BodyRef when the
			// full inbound body was read. On a read error the (partial) body is
			// still forwarded unchanged, but it is NOT spilled — the inspector
			// must never show a silently-truncated body as complete. Disk-write
			// failure is handled separately (endpoint 404).
			if st := sp.session.bodies; st != nil && rerr == nil && len(b) > 0 {
				// OAuth-host bodies: redact JSON-field credentials
				// (refresh_token, access_token, ...) before spilling. The
				// original `b` already went to r.Body above, so the upstream
				// still receives the unmodified body — redaction affects only
				// the ccwrap-internal observability surface (drawer + spill file).
				spillBytes := b
				if shouldRedactBody(logicalHost, r.URL.Path) {
					spillBytes = redactJSONBody(b, sp.supervisor.unmaskCredentials)
				}
				bodyRefForThisRequest = st.put(newBodyID(), spillBytes)
			}
		}
		var aliasCtx *modelalias.Context
		if applyAuth {
			aliasCtx, err = modelalias.RewriteRequest(r, ap.r.modelAlias)
			if err != nil {
				sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
					Timestamp:       time.Now(),
					SessionID:       sp.session.public.ID,
					Severity:        "error",
					ErrorClass:      "model_alias_rewrite_failed",
					Summary:         err.Error(),
					UpstreamHost:    logicalHost,
					SuggestedAction: "use Claude logical model IDs and configure CCWRAP modelAliases for provider-specific upstream IDs",
				})
				// Anchor the spilled inbound body to a partial RequestRecord
				// so the inspect drawer surfaces what was attempted. Without
				// this, captureBodies=on + rewrite-rejected requests leave
				// orphan body files on disk (bounded by bodystore LRU but
				// unreachable through /recent).
				sp.supervisor.recordRequest(sp.session.public.ID, model.RequestRecord{
					// Timestamp is the request boundary (handler entry), matching
					// the success path below.
					Timestamp:             start,
					SessionID:             sp.session.public.ID,
					Method:                r.Method,
					LogicalTargetHost:     logicalHost,
					ActualUpstreamHost:    upstreamHostForRequest,
					Path:                  pathAndQuery(r.URL),
					StatusCode:            http.StatusBadGateway,
					LatencyMS:             time.Since(start).Milliseconds(),
					StreamState:           model.StreamStateUnknown,
					RequestHeaders:        r.Header.Clone(),
					BodyRef:               bodyRefForThisRequest,
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
		var upstreamBodyRefForThisRequest *model.RequestBodyRef
		if ap.l.captureBodies && aliasCtx != nil && aliasCtx.Rewritten && r.Body != nil {
			b, rerr := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
			if st := sp.session.bodies; st != nil && rerr == nil && len(b) > 0 {
				spillBytes := b
				if shouldRedactBody(logicalHost, r.URL.Path) {
					spillBytes = redactJSONBody(b, sp.supervisor.unmaskCredentials)
				}
				upstreamBodyRefForThisRequest = st.put(newBodyID(), spillBytes)
			}
		}
		// Native-TLS uses a SEPARATE upstream transport (its own cache) so the
		// egress-aware utls handshake never touches the shared plain transport
		// that telemetry/forward reuse. upstreamTransportFor selects it for EVERY
		// Anthropic host when nativeTLS is on (so no *.anthropic.com dial leaks a
		// Go fingerprint); default nativeTLS=false keeps the plain path.
		upstreamTr := sp.upstreamTransportFor(logicalHost, ap.r.egress, ap.l.nativeTLS)
		// Response-body tap: when capture is on, the ModifyResponse hook below
		// tees the streamed response into a spill WITHOUT buffering (preserves
		// SSE streaming). Set from the tee's Close, read by recordRequest below
		// — Close runs before ServeHTTP returns, so the ref is visible there.
		var responseBodyRefForThisRequest *model.RequestBodyRef
		proxy := &httputil.ReverseProxy{
			Transport:     upstreamTr,
			FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				target := joinTargetURL(baseForRequest, pr.In.URL)
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
				if applyAuth {
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
						redact := shouldRedactBody(logicalHost, r.URL.Path)
						enc := resp.Header.Get("Content-Encoding")
						resp.Body = newResponseBodyTee(resp.Body, responseBodyCapBytes, func(buf []byte, truncated bool) {
							decoded, decTrunc, ok := decodeCapturedBody(buf, enc, responseBodyCapBytes)
							if !ok {
								ref := st.put(newBodyID(), bodyDecodeFailedSentinel)
								ref.Truncated = true
								responseBodyRefForThisRequest = ref
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
							responseBodyRefForThisRequest = ref
						})
					}
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				summary := err.Error()
				errClass := classifyUpstreamError(err, "upstream_unreachable")
				sp.supervisor.recordError(sp.session.public.ID, model.ErrorRecord{
					Timestamp:       time.Now(),
					SessionID:       sp.session.public.ID,
					Severity:        "error",
					ErrorClass:      errClass,
					Summary:         summary,
					UpstreamHost:    upstreamHostForRequest,
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
		proxy.ServeHTTP(rw, r)
		latency := time.Since(start)
		sp.supervisor.recordRequest(sp.session.public.ID, model.RequestRecord{
			Timestamp:          start,
			SessionID:          sp.session.public.ID,
			Method:             r.Method,
			LogicalTargetHost:  logicalHost,
			ActualUpstreamHost: upstreamHostForRequest,
			Path:               pathAndQuery(r.URL),
			StatusCode:         rw.StatusCode(),
			LatencyMS:          latency.Milliseconds(),
			StreamState:        detectStreamState(r, rw.HeaderSnapshot()),
			// Inbound headers, cloned (record outlives the request in a
			// 250-deep ring; do not alias r.Header).
			RequestHeaders: r.Header.Clone(),
			// File-backed body ref (nil unless capture on).
			BodyRef: bodyRefForThisRequest,
			// Post-rewrite body — nil unless the request actually went
			// through modelalias body mutation.
			UpstreamBodyRef: upstreamBodyRefForThisRequest,
			// Captured upstream response body (nil unless capture on).
			ResponseBodyRef: responseBodyRefForThisRequest,
			// Stamp identity from the captured *posture (mitm handler
			// closes over the captured ap — same posture resolveUpstream /
			// RewriteRequest ran under).
			ActiveProfileName:     ap.r.profileName,
			ActiveProfileProvider: ap.r.profileProvider,
		})
	})
}
