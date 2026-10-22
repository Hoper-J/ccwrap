package supervisor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

// h2Shape offers HTTP/2 the way curl does.
var h2Shape = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}

// A hello offering HTTP/2 becomes neither a shape nor the session default;
// the first hello that can be mirrored does.
func TestCaptureClientHelloSkipsHTTP2Hellos(t *testing.T) {
	sess := &sessionState{}
	capture := func(cfg *tls.Config) (*helloStack, bool) {
		t.Helper()
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		c := cfg.Clone()
		c.ServerName, c.InsecureSkipVerify = "api.anthropic.com", true
		go func() { _ = tls.Client(a, c).Handshake() }()
		_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, s, skipped := captureClientHello(sess, b)
		return s, skipped
	}
	if s, skipped := capture(h2Shape); s != nil || !skipped {
		t.Fatalf("HTTP/2 hello: stack %v, skipped %v", s, skipped)
	}
	if sess.mirroredHelloRaw.Load() != nil || len(sess.helloStacks) != 0 {
		t.Fatal("an HTTP/2 hello must not be stored")
	}
	s, skipped := capture(alpnShape)
	if s == nil || skipped {
		t.Fatalf("HTTP/1.1 hello: stack %v, skipped %v", s, skipped)
	}
	if p := sess.mirroredHelloRaw.Load(); p == nil || !bytes.Equal(*p, s.raw) {
		t.Fatal("the first mirrorable hello becomes the session default")
	}
}

// Against an upstream that speaks HTTP/2, a client whose hello offers h2 still
// gets through: its request goes upstream over the session default shape, and
// no upstream connection negotiates HTTP/2.
func TestSessionProxyDoesNotMirrorHTTP2HellosUpstream(t *testing.T) {
	origin := startShapeOriginALPN(t, "h2", "http/1.1")
	proxy := startConnectProxy(t, origin.addr)
	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)
	nativeRootsForTest = pool
	t.Cleanup(func() { nativeRootsForTest = nil })
	proxyURL := "http://" + proxy.addr
	paths, sess := routedSessionPaths(t, model.SessionRouteRequest{
		APIBaseURL: "https://api.anthropic.com", ExactUpstreamHost: "api.anthropic.com", ExactUpstreamBase: "https://api.anthropic.com",
		RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceExplicit,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone,
		Egress:     model.EgressConfig{Mode: "explicit", HTTPProxy: proxyURL, HTTPSProxy: proxyURL, Source: "test", Summary: proxyURL},
		FailPolicy: model.FailClosed, NativeTLS: true,
	})
	api := shapedClaudeClient(t, paths.CABundlePath, sess, alpnShape)
	if code := api(claudeHead("/v1/messages?beta=true", "2"), "{}"); code != http.StatusOK {
		t.Fatalf("messages: status %d", code)
	}
	curlLike := shapedClaudeClient(t, paths.CABundlePath, sess, h2Shape)
	getHead := rawHead("GET /api/claude_code/settings HTTP/1.1", "Host: api.anthropic.com", "Accept: application/json", "Connection: keep-alive")
	if code := curlLike(getHead, ""); code != http.StatusOK {
		t.Fatalf("HTTP/2-offering client: status %d", code)
	}

	resp, err := http.Get("http://" + sess.ProxyListenAddr + "/native-tls")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		Stacks []struct {
			Key string `json:"key"`
		} `json:"stacks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if len(info.Stacks) != 1 {
		t.Fatalf("only the HTTP/1.1 shape is tracked: %+v", info.Stacks)
	}
	served := 0
	for _, c := range origin.snapshot() {
		if c.h2 {
			t.Error("an upstream connection negotiated HTTP/2")
		}
		if len(c.paths) == 0 {
			continue
		}
		fp, err := tlsfp.Compute(c.hello)
		if err != nil {
			t.Fatal(err)
		}
		if got := helloShapeKey(fp); got != info.Stacks[0].Key {
			t.Errorf("upstream connection for %v used shape %s, want the default %s", c.paths, got, info.Stacks[0].Key)
		}
		served += len(c.paths)
	}
	if served != 2 {
		t.Fatalf("origin served %d requests, want 2", served)
	}

	trace, err := control.NewClient(paths.SocketPath).Trace(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trace {
		if tr.Category == "native_tls" {
			return
		}
	}
	t.Fatalf("no native_tls trace for the HTTP/2 client: %+v", trace)
}
