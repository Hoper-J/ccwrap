package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

// Claude Code runs more than one TLS stack. 2.1.296, behind a proxy as it
// always is under ccwrap, sends POST /v1/messages and /api/eval with a
// ClientHello offering ALPN http/1.1, and most of its other calls (settings,
// bootstrap, mcp-registry, event logging) with a second, ALPN-less one. So
// ccwrap does not mirror one hello for the whole session: each MITM'd
// connection's ClientHello is grouped by shape, and the requests on that
// connection go upstream over connections built from the same shape.

const (
	// maxHelloStacks bounds the distinct shapes tracked per session; a
	// connection with a further shape falls back to the session default.
	maxHelloStacks = 8
	// maxStackPaths bounds the request paths kept per shape for display.
	maxStackPaths = 6
)

// helloStack is one ClientHello shape seen on the session's MITM'd
// connections.
type helloStack struct {
	key string // shape id over JA3, JA4 and peetprint
	raw []byte // first hello seen with this shape; replayed for its upstream dials
	fp  tlsfp.Result
	// Guarded by sessionState.helloMu.
	paths    []string // distinct request paths, first maxStackPaths
	requests int
	messages bool // carried POST /v1/messages on an Anthropic API host
}

// helloShapeKey identifies a ClientHello shape. JA3 keeps the extension and
// group order, JA4 and peetprint the rest; client random, session id, key
// shares and SNI are in none of them, so they never split a shape.
func helloShapeKey(fp tlsfp.Result) string {
	sum := sha256.Sum256([]byte(fp.JA3 + "|" + fp.JA4 + "|" + fp.Peetprint))
	return hex.EncodeToString(sum[:8])
}

// captureClientHello peeks Claude Code's ClientHello off a MITM'd conn and
// returns the conn to hand tls.Server (it replays the peeked bytes) plus the
// hello's shape. The first hello also becomes the session default
// (mirroredHelloRaw), used where no per-connection shape applies. With a hello
// pinned by CCWRAP_NATIVE_TLS_HELLO nothing is peeked: every dial mirrors the
// pinned hello. On a peek error nothing is stored and conn is returned as is,
// so a session with no hello at all still fails closed.
func captureClientHello(sess *sessionState, conn net.Conn) (net.Conn, *helloStack) {
	if sess == nil {
		return conn, nil
	}
	sess.mu.RLock()
	pinned := sess.nativeTLSHelloLoaded
	sess.mu.RUnlock()
	if pinned {
		return conn, nil
	}
	raw, replay, err := peekClientHello(conn)
	if err != nil {
		return conn, nil
	}
	cp := append([]byte(nil), raw...)
	sess.mirroredHelloRaw.CompareAndSwap(nil, &cp)
	return replay, sess.registerHelloStack(cp)
}

// registerHelloStack returns the session's stack for raw's shape, adding it if
// new. nil when raw does not parse or the session already tracks
// maxHelloStacks shapes.
func (sess *sessionState) registerHelloStack(raw []byte) *helloStack {
	fp, err := tlsfp.Compute(raw)
	if err != nil {
		return nil
	}
	key := helloShapeKey(fp)
	sess.helloMu.Lock()
	defer sess.helloMu.Unlock()
	for _, s := range sess.helloStacks {
		if s.key == key {
			return s
		}
	}
	if len(sess.helloStacks) >= maxHelloStacks {
		return nil
	}
	s := &helloStack{key: key, raw: raw, fp: fp}
	sess.helloStacks = append(sess.helloStacks, s)
	return s
}

// noteHelloUse records that a request went upstream over stack s.
func (sess *sessionState) noteHelloUse(s *helloStack, logicalHost, method, path string) {
	if s == nil {
		return
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	sess.helloMu.Lock()
	defer sess.helloMu.Unlock()
	s.requests++
	if method == http.MethodPost && path == "/v1/messages" && isAnthropicAPIHost(logicalHost) {
		s.messages = true
	}
	for _, p := range s.paths {
		if p == path {
			return
		}
	}
	if len(s.paths) < maxStackPaths {
		s.paths = append(s.paths, path)
	}
}

// helloStackView is a stack's display snapshot.
type helloStackView struct {
	Key       string   `json:"key"`
	JA3       string   `json:"ja3"`
	JA4       string   `json:"ja4"`
	Peetprint string   `json:"peetprint"`
	Main      bool     `json:"main"`
	Paths     []string `json:"paths,omitempty"`
	Requests  int      `json:"requests"`
	raw       []byte
}

// helloStackViews snapshots the session's stacks, the main one first: the
// stack that carried /v1/messages, else the first seen.
func (sess *sessionState) helloStackViews() []helloStackView {
	sess.helloMu.Lock()
	defer sess.helloMu.Unlock()
	views := make([]helloStackView, 0, len(sess.helloStacks))
	main := -1
	for i, s := range sess.helloStacks {
		if main < 0 && s.messages {
			main = i
		}
		views = append(views, helloStackView{
			Key: s.key, JA3: s.fp.JA3, JA4: s.fp.JA4, Peetprint: s.fp.Peetprint,
			Paths: append([]string(nil), s.paths...), Requests: s.requests, raw: s.raw,
		})
	}
	if len(views) == 0 {
		return views
	}
	if main < 0 {
		main = 0
	}
	views[main].Main = true
	views[0], views[main] = views[main], views[0]
	return views
}

type helloStackCtxKey struct{}

func withHelloStack(ctx context.Context, s *helloStack) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, helloStackCtxKey{}, s)
}

func helloStackFrom(ctx context.Context) *helloStack {
	s, _ := ctx.Value(helloStackCtxKey{}).(*helloStack)
	return s
}

// stackTransportKey caches one native transport per egress and hello shape.
type stackTransportKey struct{ egress, shape string }

// nativeStackTransport is nativeUpstreamTransport for the requests of one
// hello shape: a transport of its own (so keep-alive never carries one
// shape's requests over another's connections) whose dials mirror s's hello.
// A nil s (no per-connection shape) gets the session-default transport.
func (sp *sessionProxy) nativeStackTransport(cfg model.EgressConfig, s *helloStack) *http.Transport {
	if s == nil {
		return sp.nativeUpstreamTransport(cfg)
	}
	key := stackTransportKey{egress: egressTransportKey(cfg), shape: s.key}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if tr := sp.stackTransports[key]; tr != nil {
		return tr
	}
	if sp.stackTransports == nil {
		sp.stackTransports = map[stackTransportKey]*http.Transport{}
	}
	tr := sp.newNativeTransport(cfg, s.raw)
	sp.stackTransports[key] = tr
	return tr
}
