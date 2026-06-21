package supervisor

import (
	"context"
	"crypto/x509"
	"errors"
	"net/url"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"

	utls "github.com/refraction-networking/utls"
)

// directEgressForTest returns the "direct, no proxy" egress config. With no
// HTTP(S) proxy set, egress.DialContext falls through to a plain net.Dialer.
func directEgressForTest() model.EgressConfig {
	return model.EgressConfig{Mode: "direct", Source: "test", Summary: "direct"}
}

// TestClassifyUpstreamErrorNativeTLSBlocked locks the dedicated error class: a
// fail-closed block surfaces as native_tls_blocked even after the http.Transport
// wraps the dial error (e.g. in a *url.Error, as the ReverseProxy ErrorHandler
// sees it), distinguishing it from a real upstream outage; other errors keep the
// generic fallback class.
func TestClassifyUpstreamErrorNativeTLSBlocked(t *testing.T) {
	blockErr := &nativeTLSBlockedError{reason: "handshake failed"}
	if got := classifyUpstreamError(blockErr, "upstream_unreachable"); got != "native_tls_blocked" {
		t.Errorf("bare block error => %q, want native_tls_blocked", got)
	}
	// As the transport hands it to the ReverseProxy ErrorHandler:
	wrapped := &url.Error{Op: "Get", URL: "https://api.anthropic.com", Err: blockErr}
	if got := classifyUpstreamError(wrapped, "upstream_unreachable"); got != "native_tls_blocked" {
		t.Errorf("transport-wrapped block error => %q, want native_tls_blocked", got)
	}
	if got := classifyUpstreamError(errors.New("connection refused"), "upstream_unreachable"); got != "upstream_unreachable" {
		t.Errorf("non-block error => %q, want fallback upstream_unreachable", got)
	}
}

func TestNativeTLSDial(t *testing.T) {
	origin, originCA := startTLSOrigin(t, "api.anthropic.com")
	pool := x509.NewCertPool()
	pool.AddCert(originCA)
	nativeRootsForTest = pool
	t.Cleanup(func() { nativeRootsForTest = nil })

	hello := standInHello(t)
	stored := append([]byte(nil), hello...)
	sp := &sessionProxy{session: &sessionState{}}
	sp.session.mirroredHelloRaw.Store(&stored)

	eg := directEgressForTest() // the "direct" model.EgressConfig (read egress.go)
	dial := sp.nativeTLSDial(eg)

	// mirror path -> *utls.UConn
	c, err := dial(context.Background(), "tcp", origin)
	if err != nil {
		t.Fatalf("native dial: %v", err)
	}
	if _, ok := c.(*utls.UConn); !ok {
		t.Fatalf("expected *utls.UConn on the mirror path, got %T", c)
	}
	_ = c.Close()

	// fail-closed: a forced mirror failure BLOCKS the request — it returns an
	// error, never a degraded (Go-fingerprinted) stdlib conn.
	forceNativeTLSFail = true
	if c2, err := dial(context.Background(), "tcp", origin); err == nil {
		_ = c2.Close()
		t.Fatal("fail-closed: a forced mirror failure must return an error, not a degraded conn")
	}
	forceNativeTLSFail = false

	// panic guard: a utls panic is recovered into a blocked-request error, never
	// a process crash (the dial goroutine has no recover of its own).
	forceNativeTLSPanic = true
	t.Cleanup(func() { forceNativeTLSPanic = false })
	if c3, err := dial(context.Background(), "tcp", origin); err == nil {
		_ = c3.Close()
		t.Fatal("a utls panic must be recovered into a blocked-request error, not crash")
	}
}
