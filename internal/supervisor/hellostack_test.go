package supervisor

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

// clientHelloOf returns the record-framed ClientHello a Go TLS client with cfg
// sends.
func clientHelloOf(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := cfg.Clone()
	c.ServerName = "api.anthropic.com"
	c.InsecureSkipVerify = true
	go func() { _ = tls.Client(a, c).Handshake() }()
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, _, err := peekClientHello(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var (
	alpnShape  = &tls.Config{NextProtos: []string{"http/1.1"}, CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256}}
	plainShape = &tls.Config{CurvePreferences: []tls.CurveID{tls.CurveP256}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}}
)

func TestRegisterHelloStackGroupsByShape(t *testing.T) {
	sess := &sessionState{}
	a1, a2 := clientHelloOf(t, alpnShape), clientHelloOf(t, alpnShape)
	if bytes.Equal(a1, a2) {
		t.Fatal("two handshakes should differ in random and key share")
	}
	b := clientHelloOf(t, plainShape)
	sa1, sa2, sb := sess.registerHelloStack(a1), sess.registerHelloStack(a2), sess.registerHelloStack(b)
	if sa1 == nil || sa1 != sa2 {
		t.Fatalf("hellos of one shape must share a stack: %p %p", sa1, sa2)
	}
	if sb == nil || sb == sa1 {
		t.Fatalf("a different shape must get its own stack")
	}
	if !bytes.Equal(sa1.raw, a1) {
		t.Fatal("a stack replays the first hello of its shape")
	}
	if sess.registerHelloStack([]byte("not a hello")) != nil {
		t.Fatal("an unparsable hello has no shape")
	}
}

func TestRegisterHelloStackIsBounded(t *testing.T) {
	sess := &sessionState{}
	curves := []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384, tls.CurveP521}
	n := 0
	// Distinct shapes: every non-empty curve subset, with and without ALPN.
	for mask := 1; mask < 1<<len(curves) && n < maxHelloStacks+2; mask++ {
		for _, alpn := range [][]string{nil, {"http/1.1"}} {
			var cs []tls.CurveID
			for i, c := range curves {
				if mask&(1<<i) != 0 {
					cs = append(cs, c)
				}
			}
			s := sess.registerHelloStack(clientHelloOf(t, &tls.Config{CurvePreferences: cs, NextProtos: alpn}))
			if n < maxHelloStacks && s == nil {
				t.Fatalf("shape %d should be tracked", n)
			}
			if n >= maxHelloStacks && s != nil {
				t.Fatalf("shape %d is past maxHelloStacks and must fall back", n)
			}
			n++
		}
	}
}

func TestHelloStackViewsPutTheMessagesStackFirst(t *testing.T) {
	sess := &sessionState{}
	other := sess.registerHelloStack(clientHelloOf(t, plainShape))
	api := sess.registerHelloStack(clientHelloOf(t, alpnShape))
	if v := sess.helloStackViews(); len(v) != 2 || v[0].Key != other.key || !v[0].Main {
		t.Fatalf("before any /v1/messages the first seen leads: %+v", v)
	}
	sess.noteHelloUse(other, "api.anthropic.com", http.MethodGet, "/api/claude_code/settings?x=1")
	sess.noteHelloUse(api, "api.anthropic.com", http.MethodPost, "/v1/messages?beta=true")
	sess.noteHelloUse(api, "api.anthropic.com", http.MethodPost, "/v1/messages?beta=true")
	v := sess.helloStackViews()
	if len(v) != 2 || v[0].Key != api.key || !v[0].Main || v[1].Main {
		t.Fatalf("the stack that carried /v1/messages leads: %+v", v)
	}
	if strings.Join(v[0].Paths, ",") != "/v1/messages" || v[0].Requests != 2 || strings.Join(v[1].Paths, ",") != "/api/claude_code/settings" {
		t.Fatalf("paths are deduplicated and stripped of queries: %+v", v)
	}
	// Only POST /v1/messages on an Anthropic API host marks the main stack.
	s := &sessionState{}
	x := s.registerHelloStack(clientHelloOf(t, plainShape))
	y := s.registerHelloStack(clientHelloOf(t, alpnShape))
	s.noteHelloUse(y, "statsig.anthropic.com", http.MethodPost, "/v1/messages")
	s.noteHelloUse(y, "api.anthropic.com", http.MethodGet, "/v1/messages")
	if v := s.helloStackViews(); v[0].Key != x.key {
		t.Fatalf("no POST /v1/messages on the API host yet: %+v", v)
	}
}

func TestCaptureClientHelloKeepsTheFirstAsDefault(t *testing.T) {
	sess := &sessionState{}
	capture := func(cfg *tls.Config) *helloStack {
		t.Helper()
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		c := cfg.Clone()
		c.ServerName, c.InsecureSkipVerify = "api.anthropic.com", true
		go func() { _ = tls.Client(a, c).Handshake() }()
		_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, s := captureClientHello(sess, b)
		return s
	}
	first := capture(plainShape)
	second := capture(alpnShape)
	if first == nil || second == nil || first == second {
		t.Fatalf("two shapes, two stacks: %v %v", first, second)
	}
	if p := sess.mirroredHelloRaw.Load(); p == nil || !bytes.Equal(*p, first.raw) {
		t.Fatal("the first hello stays the session default")
	}
}

func TestNativeStackTransportCachesPerShapeAndEgress(t *testing.T) {
	sp := &sessionProxy{
		supervisor:       &Supervisor{transport: &http.Transport{}},
		session:          &sessionState{},
		transports:       map[string]*http.Transport{},
		nativeTransports: map[string]*http.Transport{},
	}
	a := sp.session.registerHelloStack(clientHelloOf(t, alpnShape))
	b := sp.session.registerHelloStack(clientHelloOf(t, plainShape))
	direct := model.EgressConfig{Mode: "direct", Source: "test"}
	other := model.EgressConfig{Mode: "explicit", HTTPSProxy: "http://127.0.0.1:9", Source: "test"}
	ta, tb := sp.nativeStackTransport(direct, a), sp.nativeStackTransport(direct, b)
	if ta == tb || ta != sp.nativeStackTransport(direct, a) {
		t.Fatal("one transport per shape, reused for that shape")
	}
	if sp.nativeStackTransport(direct, nil) != sp.nativeUpstreamTransport(direct) {
		t.Fatal("no shape: the session-default transport")
	}
	if sp.upstreamTransportForStack("api.anthropic.com", direct, true, a) != ta {
		t.Fatal("native TLS on an Anthropic host selects the shape's transport")
	}
	if sp.upstreamTransportForStack("api.anthropic.com", direct, false, a) == ta {
		t.Fatal("native TLS off never uses a mirroring transport")
	}
	to := sp.nativeStackTransport(other, a)
	sp.drainSupersededTransports(egressTransportKey(other))
	if _, kept := sp.stackTransports[stackTransportKey{egress: egressTransportKey(other), shape: a.key}]; !kept {
		t.Fatal("the active egress's shape transports survive a drain")
	}
	if len(sp.stackTransports) != 1 || sp.nativeStackTransport(other, a) != to {
		t.Fatal("other egresses' shape transports are drained")
	}
}

// shapeOrigin is an HTTPS upstream for api.anthropic.com that records, per
// connection, the ClientHello it was opened with and the paths served on it.
type shapeOrigin struct {
	addr  string
	ca    *x509.Certificate
	mu    sync.Mutex
	conns []*shapeConn
	// messagesStatus answers POST /v1/messages (200 when zero); everything
	// else gets 200 and "{}".
	messagesStatus int
}

type shapeConn struct {
	hello []byte
	paths []string
}

func (o *shapeOrigin) snapshot() []shapeConn {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]shapeConn, len(o.conns))
	for i, c := range o.conns {
		out[i] = shapeConn{hello: c.hello, paths: append([]string(nil), c.paths...)}
	}
	return out
}

func startShapeOrigin(t *testing.T) *shapeOrigin {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "shape CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafT := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "api.anthropic.com"},
		DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafT, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	o := &shapeOrigin{addr: ln.Addr().String(), ca: ca}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				raw, replay, err := peekClientHello(c)
				if err != nil {
					return
				}
				sc := &shapeConn{hello: raw}
				o.mu.Lock()
				o.conns = append(o.conns, sc)
				o.mu.Unlock()
				tc := tls.Server(replay, cfg)
				br := bufio.NewReader(tc)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					o.mu.Lock()
					sc.paths = append(sc.paths, req.URL.Path)
					status := o.messagesStatus
					o.mu.Unlock()
					reply := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
					if req.Method == http.MethodPost && req.URL.Path == "/v1/messages" && status != 0 {
						body := `{"type":"error","error":{"type":"invalid_request_error","message":"shape origin"}}`
						reply = fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", status, http.StatusText(status), len(body), body)
					}
					if _, err := io.WriteString(tc, reply); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return o
}

// shapedClaudeClient is claudeSideClient with its own TLS settings, i.e. its
// own ClientHello shape.
func shapedClaudeClient(t *testing.T, caBundle string, sess *model.Session, shape *tls.Config) (send func(head, body string) int) {
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
	cfg := shape.Clone()
	cfg.ServerName, cfg.RootCAs = "api.anthropic.com", mitmPool
	conn := tls.Client(raw, cfg)
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

// Claude Code runs more than one TLS stack. Through the real supervisor on a
// first-party route, each MITM'd connection's requests must reach the upstream
// over connections opened with that connection's own ClientHello shape, and
// keep-alive must never carry one shape's requests over another's connection.
func TestSessionProxyMirrorsEachConnectionsHelloUpstream(t *testing.T) {
	origin := startShapeOrigin(t)
	proxy := startConnectProxy(t, origin.addr)
	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)
	nativeRootsForTest = pool
	t.Cleanup(func() { nativeRootsForTest = nil })
	proxyURL := "http://" + proxy.addr
	caBundle, sess := routedSession(t, model.SessionRouteRequest{
		APIBaseURL: "https://api.anthropic.com", ExactUpstreamHost: "api.anthropic.com", ExactUpstreamBase: "https://api.anthropic.com",
		RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceExplicit,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone,
		Egress:     model.EgressConfig{Mode: "explicit", HTTPProxy: proxyURL, HTTPSProxy: proxyURL, Source: "test", Summary: proxyURL},
		FailPolicy: model.FailClosed, NativeTLS: true,
	})
	other := shapedClaudeClient(t, caBundle, sess, plainShape)
	api := shapedClaudeClient(t, caBundle, sess, alpnShape)
	getHead := rawHead("GET /api/claude_code/settings HTTP/1.1", "Host: api.anthropic.com", "Accept: application/json", "Connection: keep-alive")
	for i := 0; i < 2; i++ {
		if code := other(getHead, ""); code != http.StatusOK {
			t.Fatalf("settings: status %d", code)
		}
		if code := api(claudeHead("/v1/messages?beta=true", "2"), "{}"); code != http.StatusOK {
			t.Fatalf("messages: status %d", code)
		}
	}

	resp, err := http.Get("http://" + sess.ProxyListenAddr + "/native-tls")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		JA4    string `json:"ja4"`
		Stacks []struct {
			Key   string   `json:"key"`
			JA4   string   `json:"ja4"`
			Main  bool     `json:"main"`
			Paths []string `json:"paths"`
		} `json:"stacks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if len(info.Stacks) != 2 || !info.Stacks[0].Main || strings.Join(info.Stacks[0].Paths, ",") != "/v1/messages" ||
		strings.Join(info.Stacks[1].Paths, ",") != "/api/claude_code/settings" || info.JA4 != info.Stacks[0].JA4 {
		t.Fatalf("GET /native-tls must list both shapes, the /v1/messages one first: %+v", info)
	}
	want := map[string]string{"/v1/messages": info.Stacks[0].Key, "/api/claude_code/settings": info.Stacks[1].Key}

	conns := origin.snapshot()
	served := 0
	for _, c := range conns {
		if len(c.paths) == 0 {
			continue
		}
		fp, err := tlsfp.Compute(c.hello)
		if err != nil {
			t.Fatal(err)
		}
		got := helloShapeKey(fp)
		for _, p := range c.paths {
			served++
			if want[p] != got {
				t.Errorf("upstream connection carrying %s (paths %v) was opened with shape %s (%s), want %s", p, c.paths, got, fp.JA4, want[p])
			}
		}
	}
	if served != 4 {
		t.Fatalf("origin served %d requests, want 4 (conns %d)", served, len(conns))
	}
}
