package supervisor

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// headOrigin is an HTTPS upstream that records every raw request head it
// receives (keep-alive aware) and answers each with a small JSON 200.
type headOrigin struct {
	addr  string
	ca    *x509.Certificate
	mu    sync.Mutex
	heads []string
	conns int
}

func (o *headOrigin) snapshot() ([]string, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.heads...), o.conns
}

func startHeadOrigin(t *testing.T) *headOrigin {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wire-order CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	o := serveHeads(t, ln)
	o.ca = ca
	return o
}

// startPlainHeadSink is headOrigin over plain HTTP — also usable as an
// absolute-form forward proxy that answers every request itself.
func startPlainHeadSink(t *testing.T) *headOrigin {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return serveHeads(t, ln)
}

func serveHeads(t *testing.T, ln net.Listener) *headOrigin {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	o := &headOrigin{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			o.mu.Lock()
			o.conns++
			o.mu.Unlock()
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					var head strings.Builder
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						head.WriteString(line)
						if line == "\r\n" {
							break
						}
					}
					req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(head.String())))
					if err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, br, req.ContentLength); err != nil && req.ContentLength > 0 {
						return
					}
					o.mu.Lock()
					o.heads = append(o.heads, head.String())
					o.mu.Unlock()
					if _, err := io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return o
}

func rawHead(lines ...string) string { return strings.Join(lines, "\r\n") + "\r\n\r\n" }

// routedSession starts a supervisor and one session routed by route.
func routedSession(t *testing.T, route model.SessionRouteRequest) (string, *model.Session) {
	t.Helper()
	paths := testutil.ShortAppPaths(t, "w.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "wire-order"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, route); err != nil {
		t.Fatal(err)
	}
	return paths.CABundlePath, sess
}

// claudeSideClient plays Claude Code: CONNECT api.anthropic.com through the
// session proxy, TLS to the MITM, then raw HTTP/1.1 requests on that one
// keep-alive connection. send returns the response status.
func claudeSideClient(t *testing.T, caBundle string, sess *model.Session) (send func(head, body string) int) {
	t.Helper()
	mitmPool := x509.NewCertPool()
	caPEM, err := os.ReadFile(caBundle)
	if err != nil || !mitmPool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("load ccwrap CA: %v", err)
	}
	raw, err := net.DialTimeout("tcp", sess.ProxyListenAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := io.WriteString(raw, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	conn := tls.Client(raw, &tls.Config{ServerName: "api.anthropic.com", RootCAs: mitmPool, NextProtos: []string{"http/1.1"}})
	if err := conn.Handshake(); err != nil {
		t.Fatal(err)
	}
	cr := bufio.NewReader(conn)
	return func(head, body string) int {
		t.Helper()
		if _, err := io.WriteString(conn, head+body); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(cr, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
}

func xAPIKeyRoute(base string, egress model.EgressConfig) model.SessionRouteRequest {
	return model.SessionRouteRequest{
		APIBaseURL:        base,
		RouteClass:        model.RouteClassThirdPartyHidden,
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModeOverrideXAPIKey,
		AuthSource:        model.AuthSourceAnthropicAPIKey,
		ExactUpstreamHost: mustParseURLHost(base),
		ExactUpstreamBase: base,
		FailPolicy:        model.FailClosed,
		Egress:            egress,
		NativeTLS:         true,
		OverrideAuth: &model.AuthOverride{
			Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey,
			HeaderName: "X-API-Key", HeaderValue: "sekret",
		},
	}
}

func mustParseURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u.Hostname()
}

// claudeHead is a Claude Code-shaped request head for the client side.
func claudeHead(target, contentLength string) string {
	return rawHead(
		"POST "+target+" HTTP/1.1",
		"Accept: application/json",
		"Content-Type: application/json",
		"User-Agent: claude-cli/2.1.296 (external, cli)",
		"X-Stainless-OS: MacOS",
		"anthropic-version: 2023-06-01",
		"authorization: Bearer ccwrap-placeholder",
		"x-app: cli",
		"Connection: keep-alive",
		"Host: api.anthropic.com",
		"Accept-Encoding: gzip, deflate, br, zstd",
		"Content-Length: "+contentLength,
	)
}

// Through the real supervisor and native-TLS route, the upstream request head
// must keep the client's (Claude Code's) header order and casing — not
// net/http's sorted, canonicalized layout — across keep-alive requests, with
// ccwrap's own changes (Host, credential override) applied in place.
func TestSessionProxyKeepsClientHeaderWireOrderUpstream(t *testing.T) {
	origin := startHeadOrigin(t)
	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)
	nativeRootsForTest = pool
	t.Cleanup(func() { nativeRootsForTest = nil })
	_, port, _ := net.SplitHostPort(origin.addr)
	upstream := "https://localhost:" + port
	caBundle, sess := routedSession(t, xAPIKeyRoute(upstream+"/prefix", model.EgressConfig{Mode: "direct", Source: "test", Summary: "direct"}))
	send := claudeSideClient(t, caBundle, sess)
	sendOK := func(head, body string) {
		t.Helper()
		if code := send(head, body); code != http.StatusOK {
			t.Fatalf("upstream status %d", code)
		}
	}

	body := `{"model":"claude-sonnet-4-6","messages":[]}`
	cl := strconv.Itoa(len(body))
	sendOK(claudeHead("/v1/messages?beta=true", cl), body)
	// Same connection, different layout: the second request keeps its own.
	sendOK(rawHead(
		"POST /v1/messages/count_tokens?beta=true HTTP/1.1",
		"x-app: cli",
		"Host: api.anthropic.com",
		"authorization: Bearer ccwrap-placeholder",
		"Content-Length: "+cl,
		"Connection: keep-alive",
	), body)

	host := "Host: localhost:" + port
	want := []string{
		rawHead(
			"POST /prefix/v1/messages?beta=true HTTP/1.1",
			"Accept: application/json",
			"Content-Type: application/json",
			"User-Agent: claude-cli/2.1.296 (external, cli)",
			"X-Stainless-OS: MacOS",
			"anthropic-version: 2023-06-01",
			"x-api-key: sekret",
			"x-app: cli",
			"Connection: keep-alive",
			host,
			"Accept-Encoding: gzip, deflate, br, zstd",
			"Content-Length: "+cl,
		),
		rawHead(
			"POST /prefix/v1/messages/count_tokens?beta=true HTTP/1.1",
			"x-app: cli",
			host,
			"x-api-key: sekret",
			"Content-Length: "+cl,
			"Connection: keep-alive",
		),
	}
	heads, conns := origin.snapshot()
	if len(heads) != len(want) {
		t.Fatalf("upstream saw %d request(s), want %d: %q", len(heads), len(want), heads)
	}
	for i := range want {
		if heads[i] != want[i] {
			t.Errorf("request %d upstream head:\n%s\nwant:\n%s", i+1, heads[i], want[i])
		}
	}
	if conns != 1 {
		t.Logf("upstream connections: %d (the reused-connection path was not exercised)", conns)
	}
}

// An http:// upstream on the native route honors the session egress: with an
// HTTP-proxy egress the request goes to the proxy as an absolute-form forward
// (Claude Code's header layout intact), never straight to the upstream; a
// loopback upstream stays direct (the egress bypass floor).
func TestSessionProxyHTTPUpstreamHonorsEgress(t *testing.T) {
	proxy := startPlainHeadSink(t)
	egressCfg := model.EgressConfig{Mode: "explicit", HTTPProxy: "http://" + proxy.addr, HTTPSProxy: "http://" + proxy.addr, Source: "test", Summary: "http://" + proxy.addr}
	body := `{"model":"claude-sonnet-4-6","messages":[]}`
	cl := strconv.Itoa(len(body))

	// gw.egress.test resolves nowhere: only the proxy can deliver it.
	caBundle, sess := routedSession(t, xAPIKeyRoute("http://gw.egress.test:8080/prefix", egressCfg))
	if code := claudeSideClient(t, caBundle, sess)(claudeHead("/v1/messages?beta=true", cl), body); code != http.StatusOK {
		t.Fatalf("status %d through the egress proxy", code)
	}
	want := rawHead(
		"POST http://gw.egress.test:8080/prefix/v1/messages?beta=true HTTP/1.1",
		"Accept: application/json",
		"Content-Type: application/json",
		"User-Agent: claude-cli/2.1.296 (external, cli)",
		"X-Stainless-OS: MacOS",
		"anthropic-version: 2023-06-01",
		"x-api-key: sekret",
		"x-app: cli",
		"Connection: keep-alive",
		"Host: gw.egress.test:8080",
		"Accept-Encoding: gzip, deflate, br, zstd",
		"Content-Length: "+cl,
	)
	heads, _ := proxy.snapshot()
	if len(heads) != 1 || heads[0] != want {
		t.Fatalf("egress proxy saw %q\nwant:\n%s", heads, want)
	}

	// Loopback upstream: direct, the proxy sees nothing more.
	local := startPlainHeadSink(t)
	caBundle, sess = routedSession(t, xAPIKeyRoute("http://"+local.addr, egressCfg))
	if code := claudeSideClient(t, caBundle, sess)(claudeHead("/v1/messages?beta=true", cl), body); code != http.StatusOK {
		t.Fatalf("status %d from the loopback upstream", code)
	}
	if heads, _ := local.snapshot(); len(heads) != 1 {
		t.Fatalf("loopback upstream saw %d request(s), want 1 (direct)", len(heads))
	}
	if heads, _ := proxy.snapshot(); len(heads) != 1 {
		t.Fatalf("a loopback upstream went through the egress proxy (%d requests)", len(heads))
	}
}
