package supervisor

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

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
	// https keeps its egress inside the TLS dial: Proxy must not claim it
	// (Transport would bypass DialTLSContext). http:// upstreams take the same
	// egress as the plain transport — see TestNativeUpstreamTransportHTTPEgress.
	if u, err := nt.Proxy(mustRequest(t, "https://api.anthropic.com/v1/messages")); u != nil || err != nil {
		t.Fatalf("native Proxy(https) = %v, %v; want nil (egress stays in the TLS dial)", u, err)
	}
	// With an HTTP-proxy egress, DialContext only ever reaches the proxy (or a
	// bypassed target): a direct dial, wrapped so request heads keep Claude
	// Code's wire order. Behavioral check: it reaches a local listener
	// directly and hands back a wireOrderConn.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := nt.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("native DialContext must dial directly: %v", err)
	}
	_ = c.Close()
	if _, ok := c.(*wireOrderConn); !ok {
		t.Fatalf("native DialContext returned %T, want *wireOrderConn", c)
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

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// recordingListener counts the connections something dials to it.
func recordingListener(t *testing.T) (string, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// http:// upstreams on the native transport must honor the egress exactly like
// the plain transport does — never dial out directly past it. An HTTP proxy
// gets an absolute-form forward (Proxy); SOCKS and TLS proxies get a tunnel
// (DialContext through egress.DialContext); loopback/private targets stay
// direct (the egress bypass floor).
func TestNativeUpstreamTransportHTTPEgress(t *testing.T) {
	sp := newTestSessionProxy(t)

	nt := sp.nativeUpstreamTransport(httpProxyEgressForTest())
	u, err := nt.Proxy(mustRequest(t, "http://gw.example.com:3000/v1/messages"))
	if err != nil || u == nil || u.String() != "http://proxy.example.com:3128" {
		t.Fatalf("native Proxy(http public) = %v, %v; want the egress proxy", u, err)
	}
	if u, _ := nt.Proxy(mustRequest(t, "http://127.0.0.1:8000/v1/messages")); u != nil {
		t.Fatalf("native Proxy(http loopback) = %v; want nil (bypass floor)", u)
	}

	for _, scheme := range []string{"socks5h", "https"} {
		t.Run(scheme+" egress tunnels", func(t *testing.T) {
			addr, dials := recordingListener(t)
			eg := model.EgressConfig{Mode: "explicit", HTTPProxy: scheme + "://" + addr, HTTPSProxy: scheme + "://" + addr, Source: "test"}
			tr := sp.nativeUpstreamTransport(eg)
			if u, _ := tr.Proxy(mustRequest(t, "http://gw.example.com:3000/v1/messages")); u != nil {
				t.Fatalf("Proxy(http) = %v; a %s egress must tunnel via DialContext instead", u, scheme)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if c, err := tr.DialContext(ctx, "tcp", "gw.example.com:3000"); err == nil {
				_ = c.Close()
			}
			if dials() == 0 {
				t.Fatalf("an http:// upstream dial did not go through the %s egress", scheme)
			}
		})
	}
}
