package supervisor

import (
	"net/http"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// newTestSessionProxy builds a minimal *sessionProxy carrying just the pieces
// the transport-cache helpers touch: a Supervisor whose base transport mirrors
// the production clone source (ForceAttemptHTTP2:true) and the two cache maps.
// nativeUpstreamTransport only inspects sp.supervisor.transport + the caches;
// it never invokes the returned DialTLSContext, so no live sessionState is
// needed.
func newTestSessionProxy(t *testing.T) *sessionProxy {
	t.Helper()
	return &sessionProxy{
		supervisor: &Supervisor{
			transport: &http.Transport{ForceAttemptHTTP2: true},
		},
		transports:       map[string]*http.Transport{},
		nativeTransports: map[string]*http.Transport{},
	}
}

// httpProxyEgressForTest returns an HTTP-proxy EgressConfig so the PLAIN
// upstreamTransport populates Proxy != nil — proving the native path's
// Proxy=nil is a distinct behavior, not a coincidence of a direct egress.
func httpProxyEgressForTest() model.EgressConfig {
	return model.EgressConfig{
		Mode:       "explicit",
		HTTPProxy:  "http://proxy.example.com:3128",
		HTTPSProxy: "http://proxy.example.com:3128",
		Source:     "test-native-transport",
	}
}

// TestUpstreamTransportForGate locks the no-leak gate: with native-TLS on, EVERY
// *.anthropic.com host (not just the API host) dials through the native
// (fingerprint-mirroring, fail-closed) transport, so no Anthropic host ever
// receives a Go TLS fingerprint. Non-Anthropic hosts and native-TLS-off always
// use the plain transport. This is the single guard against a dial site silently
// leaking a Go fingerprint to Anthropic.
func TestUpstreamTransportForGate(t *testing.T) {
	sp := newTestSessionProxy(t)
	eg := httpProxyEgressForTest()
	native := sp.nativeUpstreamTransport(eg) // get-or-create; identity-stable per egress
	plain := sp.upstreamTransport(eg)

	cases := []struct {
		host      string
		nativeTLS bool
		want      *http.Transport
		why       string
	}{
		{"api.anthropic.com", true, native, "api must mirror"},
		{"api-staging.anthropic.com", true, native, "api-staging must mirror"},
		{"statsig.anthropic.com", true, native, "non-api anthropic must ALSO mirror — no Go-fingerprint leak"},
		{"console.anthropic.com", true, native, "any *.anthropic.com host must mirror"},
		{"datadoghq.com", true, plain, "non-anthropic uses the plain transport"},
		{"example.com", true, plain, "non-anthropic uses the plain transport"},
		{"api.anthropic.com", false, plain, "native-tls off => plain even for anthropic"},
	}
	for _, c := range cases {
		if got := sp.upstreamTransportFor(c.host, eg, c.nativeTLS); got != c.want {
			t.Errorf("upstreamTransportFor(%q, nativeTLS=%v) chose the wrong transport — %s", c.host, c.nativeTLS, c.why)
		}
	}
}

func TestNativeUpstreamTransportSeparate(t *testing.T) {
	sp := newTestSessionProxy(t)
	eg := httpProxyEgressForTest()

	nt := sp.nativeUpstreamTransport(eg)
	if nt.DialTLSContext == nil {
		t.Fatal("native transport must set DialTLSContext")
	}
	if nt.Proxy != nil {
		t.Fatal("native transport must set Proxy=nil (egress moves into the dial)")
	}
	if nt.DialContext != nil {
		t.Fatal("native transport must set DialContext=nil")
	}
	if nt.ForceAttemptHTTP2 {
		t.Fatal("native transport must set ForceAttemptHTTP2=false (h1)")
	}
	pt := sp.upstreamTransport(eg)
	if pt == nt {
		t.Fatal("native and plain transports must be different objects")
	}
	if pt.DialTLSContext != nil {
		t.Fatal("the plain transport must NOT have DialTLSContext (telemetry/forward unaffected)")
	}
	if pt.Proxy == nil {
		t.Fatal("the plain transport for an HTTP-proxy egress must still set Proxy (egress intact)")
	}
	if nt2 := sp.nativeUpstreamTransport(eg); nt2 != nt {
		t.Fatal("native transport must be cached per egress")
	}
}
