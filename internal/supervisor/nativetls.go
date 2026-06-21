package supervisor

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/model"

	utls "github.com/refraction-networking/utls"
)

// utlsPinnedVersion is the exact refraction-networking/utls version this build
// is verified against. Bump it ONLY together with the go.mod dependency and a
// re-run of the offline parity test (added in a later task), because utls is a
// crypto/tls fork whose ClientHello reproduction can change between versions.
// See the design spec, "Dependency".
const utlsPinnedVersion = "v1.8.2"

// utlsCfg builds the upstream TLS config. INVARIANTS (design spec): no
// InsecureSkipVerify; ServerName=host (the real dial host); RootCAs=roots
// (system pool in prod; ccwrap's MITM CA NEVER added); MinVersion>=TLS1.2; and a
// MANDATORY VerifyConnection enforcing chain+hostname in our own code (utls is a
// lagging crypto/tls fork -- do not trust InsecureSkipVerify=false alone).
func utlsCfg(host string, roots *x509.CertPool) *utls.Config {
	return &utls.Config{
		ServerName:         host,
		RootCAs:            roots,
		InsecureSkipVerify: false,
		MinVersion:         utls.VersionTLS12,
		VerifyConnection: func(cs utls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("native-tls: no peer certificates")
			}
			opts := x509.VerifyOptions{
				DNSName:       host,
				Roots:         roots,
				Intermediates: x509.NewCertPool(),
			}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			chains, err := cs.PeerCertificates[0].Verify(opts)
			if err != nil {
				return err
			}
			if len(chains) == 0 {
				return errors.New("native-tls: empty verified chain")
			}
			return nil
		},
	}
}

// ValidateLoadedHello checks that raw is a ClientHello the native-TLS dialer can
// mirror (parseable by the SAME utls Fingerprinter the dial uses) AND does not
// offer HTTP/2 in ALPN (ccwrap forces HTTP/1.1; an h2-offering hello would
// negotiate a protocol ccwrap cannot speak). Used at launch for fail-fast
// validation of CCWRAP_NATIVE_TLS_HELLO.
func ValidateLoadedHello(raw []byte) error {
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(raw)
	if err != nil {
		return fmt.Errorf("native-tls hello: not a parseable ClientHello: %w", err)
	}
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			for _, p := range a.AlpnProtocols {
				if p == "h2" {
					return errors.New("native-tls hello offers HTTP/2 ALPN; ccwrap forces HTTP/1.1 — load an HTTP/1.1 client's hello (e.g. Node/undici)")
				}
			}
		}
	}
	return nil
}

// nativeUTLSDialOver mirrors CC's ClientHello (rawHello) over rawConn to host,
// verifying with roots. The spec is re-parsed here (fresh per dial -- utls
// mutates extension state per connection, so specs MUST NOT be shared) and its
// wire SNI is rewritten to host (ApplyPreset only fills SNI when empty).
func nativeUTLSDialOver(rawConn net.Conn, host string, roots *x509.CertPool, rawHello []byte) (net.Conn, error) {
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(rawHello)
	if err != nil {
		return nil, err
	}
	for _, ext := range spec.Extensions {
		if sni, ok := ext.(*utls.SNIExtension); ok {
			sni.ServerName = host
		}
	}
	u := utls.UClient(rawConn, utlsCfg(host, roots), utls.HelloCustom)
	if err := u.ApplyPreset(spec); err != nil {
		return nil, err
	}
	if err := u.Handshake(); err != nil {
		return nil, err
	}
	return u, nil
}

// nativeUTLSHandshake is a verify-only seam: dial+verify then close, returning
// the handshake error (nil on success).
func nativeUTLSHandshake(rawConn net.Conn, host string, roots *x509.CertPool, rawHello []byte) error {
	c, err := nativeUTLSDialOver(rawConn, host, roots, rawHello)
	if c != nil {
		_ = c.Close()
	}
	return err
}

// nativeRootsForTest, when non-nil, overrides the trust anchor used by the
// native-TLS path. It is nil in production (so nativeRoots falls back to the
// system pool) and set by tests to a throwaway CA pool.
var nativeRootsForTest *x509.CertPool

// forceNativeTLSFail is a test seam that forces a mirror failure (and therefore
// the fail-closed block) even when a captured ClientHello is present. Always
// false in production.
var forceNativeTLSFail bool

// forceNativeTLSPanic is a test seam that makes the utls mirror stage panic, so
// tests can prove the panic guard converts it into a fail-closed block instead
// of crashing the supervisor process. Always false in production.
var forceNativeTLSPanic bool

// safeUTLSDial runs the utls mirror handshake under a panic guard. utls parses
// the captured ClientHello and drives a full TLS state machine; a bug there
// would panic on the http.Transport dial goroutine, and -- since that goroutine
// has no recover() and native-TLS is now default-on for every session -- crash
// the whole supervisor. Converting a panic into an error routes it onto the same
// fail-closed branch as any other mirror failure: the request is blocked, the
// process survives.
func safeUTLSDial(rawConn net.Conn, host string, roots *x509.CertPool, rawHello []byte) (conn net.Conn, err error) {
	defer func() {
		if r := recover(); r != nil {
			conn, err = nil, fmt.Errorf("utls panic: %v", r)
		}
	}()
	if forceNativeTLSPanic {
		panic("forced utls panic (test)")
	}
	return nativeUTLSDialOver(rawConn, host, roots, rawHello)
}

// initialNativeTLSPosture seeds a session's NativeTLS posture at construction so
// the dashboard NATIVE TLS cell is present from first paint — BEFORE any upstream
// dial. "active" means the session is armed to mirror; the first dial confirms it
// or flips it to "blocked". Empty when the feature is off (no cell). Without this
// seed, public.NativeTLS stays "" until the first dial, so a dashboard opened
// earlier would never show the cell (the JS live-updater only patches an existing
// cell, it does not create one).
func initialNativeTLSPosture(on bool) string {
	if on {
		return "active"
	}
	return ""
}

// nativeRoots returns the trust anchor for native-TLS verification: the
// test-override pool when set, otherwise the host's system pool.
func nativeRoots() *x509.CertPool {
	if nativeRootsForTest != nil {
		return nativeRootsForTest
	}
	p, _ := x509.SystemCertPool()
	return p
}

// nativeTLSBlockedError marks a request the fail-closed native-TLS dialer
// BLOCKED because it could not mirror Claude Code's fingerprint. The ReverseProxy
// ErrorHandlers key off it (via classifyUpstreamError) to classify the failure as
// native_tls_blocked rather than a generic upstream_unreachable, so a fingerprint
// block is self-explaining and distinguishable from a real upstream outage.
type nativeTLSBlockedError struct{ reason string }

func (e *nativeTLSBlockedError) Error() string {
	return "native-tls: cannot mirror Claude Code fingerprint (" + e.reason + "); request blocked to avoid a de-anonymizing TLS handshake"
}

// classifyUpstreamError maps a ReverseProxy upstream error to an ErrorClass,
// distinguishing a fail-closed native-TLS block (native_tls_blocked) from a
// generic failure (fallback). Uses errors.As so it still sees the typed error
// after http.Transport wraps it (e.g. in a *url.Error).
func classifyUpstreamError(err error, fallback string) string {
	var blocked *nativeTLSBlockedError
	if errors.As(err, &blocked) {
		return "native_tls_blocked"
	}
	return fallback
}

// nativeTLSDial builds the DialTLSContext for the native-TLS Anthropic
// transport over egress config eg. It dials egress, then mirrors CC's captured
// ClientHello over that conn (the *utls.UConn mirror path). On egress-dial
// failure it returns the error WITHOUT retry (the ReverseProxy ErrorHandler
// surfaces this as a 502).
//
// FAIL-CLOSED: if the fingerprint cannot be mirrored -- utls error, utls panic,
// no ClientHello captured yet, or a test-forced failure -- it BLOCKS the request
// (returns an error) instead of falling back to a stdlib (Go-fingerprinted)
// handshake. A degraded fingerprint carried under undici headers is the exact
// mismatch this feature exists to remove; emitting it would mark the traffic as
// a non-native client upstream. On the no-hello and forced paths nothing is sent
// (raw is closed before any handshake); on a utls-handshake failure the only
// bytes already on the wire are the mirrored undici ClientHello -- never a Go
// one. Combined with the upstreamTransportFor gate, which routes EVERY Anthropic
// host (not just the API host) through this dialer, no *.anthropic.com request
// ever carries a Go TLS fingerprint unless the operator disables the feature
// (CCWRAP_NATIVE_TLS=0).
func (sp *sessionProxy) nativeTLSDial(eg model.EgressConfig) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		raw, err := egress.DialContext(ctx, eg, "tcp", addr)
		if err != nil {
			return nil, err // egress down: surfaces as 502; do not retry
		}
		var rawHello []byte
		if p := sp.session.mirroredHelloRaw.Load(); p != nil {
			rawHello = *p
		}
		var reason string
		switch {
		case rawHello == nil:
			reason = "no captured ClientHello yet"
		case forceNativeTLSFail:
			reason = "forced (test)"
		default:
			conn, derr := safeUTLSDial(raw, host, nativeRoots(), rawHello)
			if derr == nil {
				sp.recordNativeTLS(true, "")
				return conn, nil
			}
			reason = derr.Error()
		}
		// Fail-closed: block rather than emit a degraded fingerprint. Close raw
		// before any ClientHello is written so nothing identifiable goes upstream.
		_ = raw.Close()
		sp.recordNativeTLS(false, reason)
		return nil, &nativeTLSBlockedError{reason: reason}
	}
}

// recordNativeTLS surfaces the native-fingerprint TLS state into session
// posture. Called from the dial hot path: active=true when CC's captured
// ClientHello is being mirrored upstream (parity delivered), active=false with a
// reason when the mirror could not be produced and the dial BLOCKED the request
// (fail-closed — no degraded stdlib fingerprint is ever sent; parity is not
// delivered and the request fails, which the user must SEE).
//
// It mirrors the SetCaptureTelemetry public-field discipline: one sess.mu
// critical section, a change-guard so the (potentially high-frequency) dial path
// does not churn touch()/broadcast on a steady state, and a snapshot/broadcast
// emitted only on an actual transition. A block drives session Health to error
// — the same direct-write Health discipline recordError uses (Health is
// last-activity-wins; there is no central compute() to OR into). The error write
// is gated so it never mutates an ended session; nativeTLSBlocked holds the live
// flag as the source of truth (it gates recordRequest's heal-to-ok).
func (sp *sessionProxy) recordNativeTLS(active bool, reason string) {
	sess := sp.session
	if sess == nil {
		return
	}
	want := "active"
	if !active {
		want = "blocked: " + reason
	}
	sess.mu.Lock()
	changed := sess.public.NativeTLS != want
	sess.public.NativeTLS = want
	sess.nativeTLSBlocked = !active
	if !active {
		// Count block EPISODES, not blocked dials: increment only on a transition
		// into a (distinct) blocked state. A persistent outage that blocks many
		// dials with the same reason is ONE episode, not an inflated per-dial tally.
		if changed {
			sess.public.NativeTLSFallbacks++
		}
		// Drive Health to error: the request was BLOCKED (fail-closed) rather
		// than de-anonymized. Mirror recordError's direct last-activity-wins
		// write; never mutate a terminal (ended) session.
		if sess.public.State != model.StateEnded {
			sess.public.Health = model.HealthError
		}
	}
	if changed {
		sess.public.UpdatedAt = time.Now()
	}
	sess.mu.Unlock()
	// Emit the activity touch + session_updated only on a real transition AND
	// only when a supervisor is wired (the dial path may run on a sessionProxy
	// without a supervisor — e.g. unit dial tests). The state mutation above is
	// unconditional; the fan-out is the best-effort, side-effect part.
	if changed && sp.supervisor != nil {
		sp.supervisor.touch()
		sp.supervisor.broadcast("session_updated", sess.public.ID, sess.snapshot())
	}
}
