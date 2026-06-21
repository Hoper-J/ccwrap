package egress

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestResolveAutoFromEnvironment(t *testing.T) {
	cfg, notes, err := Resolve("auto", map[string]string{
		"HTTPS_PROXY": "http://proxy.example:8443",
		"NO_PROXY":    "internal.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "auto" || cfg.HTTPSProxy == "" || cfg.Source != "inherited_env" {
		t.Fatalf("unexpected cfg: %#v", cfg)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %#v", notes)
	}
}

func TestResolveAllProxySocks5Accepted(t *testing.T) {
	cfg, notes, err := Resolve("auto", map[string]string{
		"ALL_PROXY": "socks5://proxy.example:1080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "auto" || cfg.HTTPSProxy != "socks5://proxy.example:1080" {
		t.Fatalf("expected socks5 proxy, got %#v", cfg)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %#v", notes)
	}
}

func TestResolveExplicitSocks5(t *testing.T) {
	cfg, _, err := Resolve("socks5://user:pass@proxy.example:1080", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "explicit" || cfg.HTTPSProxy != "socks5://user:pass@proxy.example:1080" {
		t.Fatalf("unexpected cfg: %#v", cfg)
	}
}

func TestParseProxyURLSchemes(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		u, err := parseProxyURL(scheme + "://proxy:1080")
		if err != nil {
			t.Fatalf("scheme %s: %v", scheme, err)
		}
		if u.Scheme != scheme {
			t.Fatalf("expected scheme %s, got %s", scheme, u.Scheme)
		}
	}
	if _, err := parseProxyURL("socks4://proxy:1080"); err == nil {
		t.Fatal("expected error for socks4 scheme")
	}
}

func TestIsSOCKSEgress(t *testing.T) {
	if IsSOCKSEgress(model.EgressConfig{HTTPSProxy: "http://proxy:8080"}) {
		t.Fatal("http should not be SOCKS")
	}
	if !IsSOCKSEgress(model.EgressConfig{HTTPSProxy: "socks5://proxy:1080"}) {
		t.Fatal("socks5 should be SOCKS")
	}
	if !IsSOCKSEgress(model.EgressConfig{HTTPSProxy: "socks5h://proxy:1080"}) {
		t.Fatal("socks5h should be SOCKS")
	}
}

// fakeSOCKS5Server runs a minimal SOCKS5 server that accepts CONNECT and
// dials the target. Returns listener address; caller must close listener.
func fakeSOCKS5Server(t *testing.T, requireAuth bool) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5Conn(conn, requireAuth)
		}
	}()
	return ln
}

func handleSOCKS5Conn(conn net.Conn, requireAuth bool) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Greeting
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	if requireAuth {
		conn.Write([]byte{0x05, 0x02})
		// Read auth
		var authHdr [2]byte
		if _, err := io.ReadFull(conn, authHdr[:]); err != nil {
			return
		}
		user := make([]byte, authHdr[1])
		io.ReadFull(conn, user)
		var plen [1]byte
		io.ReadFull(conn, plen[:])
		pass := make([]byte, plen[0])
		io.ReadFull(conn, pass)
		if string(user) == "testuser" && string(pass) == "testpass" {
			conn.Write([]byte{0x01, 0x00}) // success
		} else {
			conn.Write([]byte{0x01, 0x01}) // fail
			return
		}
	} else {
		conn.Write([]byte{0x05, 0x00})
	}

	// CONNECT request
	var req [4]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return
	}
	var targetAddr string
	switch req[3] {
	case 0x01: // IPv4
		var ip [4]byte
		io.ReadFull(conn, ip[:])
		var port [2]byte
		io.ReadFull(conn, port[:])
		targetAddr = net.JoinHostPort(net.IP(ip[:]).String(), portStr(port))
	case 0x03: // domain
		var dlen [1]byte
		io.ReadFull(conn, dlen[:])
		domain := make([]byte, dlen[0])
		io.ReadFull(conn, domain)
		var port [2]byte
		io.ReadFull(conn, port[:])
		targetAddr = net.JoinHostPort(string(domain), portStr(port))
	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	targetConn, err := net.DialTimeout("tcp", targetAddr, 3*time.Second)
	if err != nil {
		conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer targetConn.Close()

	// Success reply
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	_ = conn.SetDeadline(time.Time{})

	// Relay — close both sides when either direction finishes
	go func() {
		io.Copy(targetConn, conn)
		targetConn.Close()
	}()
	io.Copy(conn, targetConn)
	conn.Close()
}

func portStr(b [2]byte) string {
	p := binary.BigEndian.Uint16(b[:])
	return fmt.Sprintf("%d", p)
}

// withoutBypassFloor disables the always-on NO_PROXY bypass floor for the
// duration of one test. The proxy-dial tests below put BOTH the fake proxy
// and the fake upstream on 127.0.0.1, so the floor (which bypasses loopback
// targets) would otherwise short-circuit the exact proxy path under test —
// DialContext would dial the upstream directly and never traverse the proxy.
// These tests do not call t.Parallel(), so mutating the package var is safe.
func withoutBypassFloor(t *testing.T) {
	t.Helper()
	saved := defaultBypassFloor
	defaultBypassFloor = nil
	t.Cleanup(func() { defaultBypassFloor = saved })
}

func TestDialSOCKS5ProxyNoAuth(t *testing.T) {
	withoutBypassFloor(t)
	// Start a real HTTP server as the target
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	// Start fake SOCKS5 server
	socksLn := fakeSOCKS5Server(t, false)
	defer socksLn.Close()

	cfg := model.EgressConfig{
		Mode:       "explicit",
		HTTPSProxy: "socks5://" + socksLn.Addr().String(),
		HTTPProxy:  "socks5://" + socksLn.Addr().String(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// DialContext through SOCKS5 to the upstream
	conn, err := DialContext(ctx, cfg, "tcp", upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send a raw HTTP request through the tunnel
	fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestDialSOCKS5ProxyWithAuth(t *testing.T) {
	withoutBypassFloor(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("authed"))
	}))
	defer upstream.Close()

	socksLn := fakeSOCKS5Server(t, true)
	defer socksLn.Close()

	cfg := model.EgressConfig{
		Mode:       "explicit",
		HTTPSProxy: "socks5://testuser:testpass@" + socksLn.Addr().String(),
		HTTPProxy:  "socks5://testuser:testpass@" + socksLn.Addr().String(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := DialContext(ctx, cfg, "tcp", upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "authed") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestDialSOCKS5ProxyAuthFail(t *testing.T) {
	withoutBypassFloor(t)
	socksLn := fakeSOCKS5Server(t, true)
	defer socksLn.Close()

	cfg := model.EgressConfig{
		Mode:       "explicit",
		HTTPSProxy: "socks5://wrong:creds@" + socksLn.Addr().String(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := DialContext(ctx, cfg, "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveAutoPrefersLowercaseEnv(t *testing.T) {
	cfg, _, err := Resolve("auto", map[string]string{
		"HTTPS_PROXY": "http://upper:8443",
		"https_proxy": "http://lower:9443",
		"NO_PROXY":    "upper.local",
		"no_proxy":    "lower.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPSProxy != "http://lower:9443" {
		t.Fatalf("expected lowercase https_proxy to win, got %#v", cfg)
	}
	if cfg.NoProxy != "lower.local" {
		t.Fatalf("expected lowercase no_proxy to win, got %#v", cfg)
	}
}

func TestShouldBypassMatchesClaudeCodeNoProxySemantics(t *testing.T) {
	if shouldBypass("api.example.com:8443", ".example.com:8443") {
		t.Fatal("did not expect port-qualified dotted token to suffix-match")
	}
	if !shouldBypass("api.example.com:443", ".example.com") {
		t.Fatal("expected leading-dot suffix rule to match subdomain")
	}
	if !shouldBypass("example.com:443", ".example.com") {
		t.Fatal("expected leading-dot suffix rule to match apex")
	}
	if !shouldBypass("example.com:443", "example.com") {
		t.Fatal("expected exact host to bypass")
	}
	if shouldBypass("api.example.com:443", "example.com") {
		t.Fatal("did not expect bare domain to match subdomain")
	}
	if !shouldBypass("localhost:8080", "localhost") {
		t.Fatal("expected localhost to bypass")
	}
	// 203.0.113.0/24 is RFC5737 TEST-NET-3 (public, non-private) — exercises
	// the host:port exact-match path without colliding with the bypass floor.
	if !shouldBypass("203.0.113.9:8443", "203.0.113.9:8443") {
		t.Fatal("expected exact host:port to bypass")
	}
}

// TestShouldBypassDefaultFloor — the always-on bypass floor: loopback,
// link-local/IMDS, and RFC1918 private ranges are never proxied even when
// NO_PROXY is empty. Matches the default behavior of curl / Go net/http /
// Python requests and Claude Code's NO_PROXY_LIST floor
// (src/upstreamproxy/upstreamproxy.ts). Closes the footgun where a
// configured egress proxy would otherwise see loopback + cloud-IMDS traffic.
func TestShouldBypassDefaultFloor(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		// loopback — bypassed even with empty NO_PROXY
		{"localhost:8080", true},
		{"127.0.0.1:443", true},
		{"127.0.0.5:443", true},
		{"[::1]:443", true},
		// link-local + cloud IMDS (169.254.169.254)
		{"169.254.169.254:80", true},
		// RFC1918 private ranges
		{"10.1.2.3:443", true},
		{"172.16.0.1:443", true},
		{"172.31.255.254:443", true},
		{"192.168.1.1:443", true},
		// public / non-private — must NOT be bypassed
		{"203.0.113.7:443", false},
		{"172.32.0.1:443", false}, // just outside 172.16.0.0/12
		{"api.anthropic.com:443", false},
		{"8.8.8.8:443", false},
	}
	for _, c := range cases {
		if got := shouldBypass(c.target, ""); got != c.want {
			t.Errorf("shouldBypass(%q, \"\") = %v, want %v", c.target, got, c.want)
		}
	}
}

// TestShouldBypassCIDRToken — a CIDR entry in NO_PROXY matches IPs in range.
// Before the floor work, CIDR tokens were silently inert (treated as literal
// hostnames that no real IP could ever equal); adding CIDR matching for the
// floor also fixes user-supplied CIDR NO_PROXY entries.
func TestShouldBypassCIDRToken(t *testing.T) {
	// 100.64.0.0/10 is RFC6598 CGNAT space — NOT in the floor, so a match
	// here can only come from the user token's CIDR logic.
	if !shouldBypass("100.64.0.5:443", "100.64.0.0/10") {
		t.Error("expected CIDR NO_PROXY token to match an in-range IP")
	}
	if shouldBypass("100.128.0.1:443", "100.64.0.0/10") {
		t.Error("did not expect out-of-range IP to match CIDR token")
	}
	if shouldBypass("example.com:443", "100.64.0.0/10") {
		t.Error("a hostname target must never match a CIDR token")
	}
}

// TestShouldBypassFloorIsAdditive — user NO_PROXY entries layer ON TOP of the
// floor, never replace it. A user setting NO_PROXY for a corporate host must
// still get loopback bypassed by the floor.
func TestShouldBypassFloorIsAdditive(t *testing.T) {
	if !shouldBypass("corp.internal:443", "corp.internal") {
		t.Error("user NO_PROXY token must still bypass")
	}
	if !shouldBypass("127.0.0.1:443", "corp.internal") {
		t.Error("floor must still bypass loopback when NO_PROXY names a different host")
	}
	if shouldBypass("api.anthropic.com:443", "corp.internal") {
		t.Error("an unrelated public host must not bypass")
	}
}

func TestProxyURLForRequestHonorsDefaultPortsForNoProxy(t *testing.T) {
	cfg := model.EgressConfig{
		Mode:       "auto",
		HTTPSProxy: "http://proxy.example:8443",
		NoProxy:    "example.com:8443",
	}
	defaultHTTPSReq, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyURLForRequest(cfg, defaultHTTPSReq.URL); got == nil {
		t.Fatal("https://example.com should not bypass NO_PROXY=example.com:8443")
	}
	explicit8443Req, err := http.NewRequest(http.MethodGet, "https://example.com:8443/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyURLForRequest(cfg, explicit8443Req.URL); got != nil {
		t.Fatalf("https://example.com:8443 should bypass, got proxy %#v", got)
	}
}
