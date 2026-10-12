package supervisor

// Wire-order fidelity for the native (Anthropic) upstream leg.
//
// ccwrap terminates Claude Code's TLS and re-sends each request with net/http,
// which canonicalizes header names, writes Host/User-Agent/Content-Length
// first and the rest sorted, and drops Connection as hop-by-hop. Upstream would
// see that Go layout under Claude Code's mirrored TLS fingerprint — a
// combination no real client produces. So the MITM records the header order
// and casing Claude Code actually sent (wireOrderRecorder), and the native
// transport's connections re-lay the head net/http writes into that layout,
// keeping net/http's final values (wireOrderConn).
//
// The order reaches the connection through httptrace.GotConn, never through a
// header, so nothing ccwrap-specific can leak upstream. It only ever decides
// layout: a missing or mismatched order means net/http's layout is sent, with
// the request otherwise unchanged.

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
)

const (
	maxWireHead       = 64 << 10 // larger heads are passed through unrecorded
	maxQueuedWireHead = 32       // orders awaiting a handler, per connection
)

var headEnd = []byte("\r\n\r\n")

type wireHeader struct {
	Name  string // as written on the wire
	Value string
}

// wireOrder is one request's head as the client wrote it.
type wireOrder struct {
	Method  string
	Target  string // request-target from the request line
	Headers []wireHeader
}

// parseRequestHead splits a raw request head (request line, header lines,
// terminating blank line) into the request line and header fields.
func parseRequestHead(head []byte) (string, []wireHeader, bool) {
	s := strings.TrimSuffix(strings.TrimLeft(string(head), "\r\n"), "\r\n\r\n")
	lines := strings.Split(s, "\r\n")
	if lines[0] == "" {
		return "", nil, false
	}
	headers := make([]wireHeader, 0, len(lines)-1)
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") { // malformed or obs-fold
			return "", nil, false
		}
		headers = append(headers, wireHeader{Name: name, Value: strings.Trim(value, " \t")})
	}
	return lines[0], headers, true
}

// bodyLength reports how many body bytes follow a request head; ok is false
// when the body is not length-delimited (chunked), so it cannot be skipped.
func bodyLength(headers []wireHeader) (int64, bool) {
	var n int64
	for _, h := range headers {
		switch {
		case strings.EqualFold(h.Name, "Transfer-Encoding"):
			return 0, false
		case strings.EqualFold(h.Name, "Content-Length"):
			v, err := strconv.ParseInt(h.Value, 10, 64)
			if err != nil || v < 0 {
				return 0, false
			}
			n = v
		}
	}
	return n, true
}

// reorderRequestHead re-lays goHead (the head net/http wrote) in the order and
// name casing of the client's original request; every value comes from goHead.
// Headers ccwrap removed stay removed, a Connection: keep-alive net/http dropped
// as hop-by-hop is restored, and headers ccwrap added (the credential override,
// upstream headers) take the slot of the first header it removed, in that
// header's casing style, else go last.
func reorderRequestHead(goHead []byte, order *wireOrder) []byte {
	if order == nil {
		return goHead
	}
	line, final, ok := parseRequestHead(goHead)
	if !ok {
		return goHead
	}
	used := make([]bool, len(final))
	has := func(name string) bool {
		for _, h := range final {
			if strings.EqualFold(h.Name, name) {
				return true
			}
		}
		return false
	}
	var laid []wireHeader
	slot, slotLower := -1, false
	for _, oh := range order.Headers {
		matched := false
		for i, fh := range final {
			if !used[i] && strings.EqualFold(fh.Name, oh.Name) {
				used[i], matched = true, true
				laid = append(laid, wireHeader{Name: oh.Name, Value: fh.Value})
				break
			}
		}
		switch {
		case matched:
		case strings.EqualFold(oh.Name, "Connection") && strings.EqualFold(oh.Value, "keep-alive") && !has("Connection"):
			laid = append(laid, oh)
		case slot < 0:
			slot, slotLower = len(laid), oh.Name == strings.ToLower(oh.Name)
		}
	}
	var added []wireHeader
	for i, fh := range final {
		if used[i] {
			continue
		}
		if slotLower {
			fh.Name = strings.ToLower(fh.Name)
		}
		added = append(added, fh)
	}
	if slot < 0 {
		slot = len(laid)
	}
	out := make([]wireHeader, 0, len(laid)+len(added))
	out = append(out, laid[:slot]...)
	out = append(out, added...)
	out = append(out, laid[slot:]...)

	var b bytes.Buffer
	b.WriteString(line)
	b.WriteString("\r\n")
	for _, h := range out {
		b.WriteString(h.Name)
		b.WriteString(": ")
		b.WriteString(h.Value)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// wireOrderRecorder observes the plaintext request stream of one MITM
// connection and queues each request's head as the client wrote it. It never
// alters the bytes. Bodies are skipped by Content-Length; on anything it
// cannot frame (a chunked body, an oversized or malformed head) it stops, and
// later requests fall back to net/http's layout.
type wireOrderRecorder struct {
	net.Conn

	mu      sync.Mutex
	head    []byte
	body    int64 // body bytes still to skip
	stopped bool
	queue   []*wireOrder
}

func (r *wireOrderRecorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	if n > 0 {
		r.observe(p[:n])
	}
	return n, err
}

func (r *wireOrderRecorder) observe(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(b) > 0 && !r.stopped {
		if r.body > 0 {
			k := min(int64(len(b)), r.body)
			r.body -= k
			b = b[k:]
			continue
		}
		r.head = append(r.head, b...)
		i := bytes.Index(r.head, headEnd)
		if i < 0 {
			if len(r.head) > maxWireHead {
				r.stop()
			}
			return
		}
		head, rest := r.head[:i+len(headEnd)], r.head[i+len(headEnd):]
		line, headers, ok := parseRequestHead(head)
		method, target, _ := strings.Cut(line, " ")
		target, _, _ = strings.Cut(target, " ")
		if !ok || target == "" {
			r.stop()
			return
		}
		if len(r.queue) == maxQueuedWireHead {
			r.queue = r.queue[1:]
		}
		r.queue = append(r.queue, &wireOrder{Method: method, Target: target, Headers: headers})
		n, framed := bodyLength(headers)
		if !framed {
			r.stop()
			return
		}
		r.body = n
		b = append([]byte(nil), rest...)
		r.head = nil
	}
}

func (r *wireOrderRecorder) stop() {
	r.stopped = true
	r.head = nil
}

// pop returns the order recorded for the request a handler is now serving,
// discarding orders of requests http.Server answered without a handler.
func (r *wireOrderRecorder) pop(method, target string) *wireOrder {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.queue) > 0 {
		o := r.queue[0]
		r.queue = r.queue[1:]
		if o.Method == method && o.Target == target {
			return o
		}
	}
	return nil
}

type wireOrderRecorderKey struct{}

func withWireOrderRecorder(ctx context.Context, r *wireOrderRecorder) context.Context {
	return context.WithValue(ctx, wireOrderRecorderKey{}, r)
}

// popWireOrder returns the client's recorded head for req, or nil.
func popWireOrder(req *http.Request) *wireOrder {
	r, _ := req.Context().Value(wireOrderRecorderKey{}).(*wireOrderRecorder)
	if r == nil {
		return nil
	}
	return r.pop(req.Method, req.RequestURI)
}

// withWireOrder hands order to whichever upstream connection the transport
// picks for req (new or reused); only wireOrderConns act on it.
func withWireOrder(req *http.Request, order *wireOrder) *http.Request {
	if order == nil {
		return req
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if c, ok := info.Conn.(*wireOrderConn); ok {
				c.setNext(order)
			}
		},
	}))
}

// wireOrderConn is a native-transport upstream connection that re-lays each
// request head net/http writes into the order set for that request (see
// withWireOrder). Bodies and responses pass through untouched; after a body it
// cannot frame (chunked) it passes everything through.
type wireOrderConn struct {
	net.Conn

	mu          sync.Mutex
	next        *wireOrder
	head        []byte
	body        int64
	passthrough bool
}

// wireOrderDial wraps dial so every connection it returns is a wireOrderConn.
func wireOrderDial(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &wireOrderConn{Conn: c}, nil
	}
}

func (c *wireOrderConn) setNext(o *wireOrder) {
	c.mu.Lock()
	c.next = o
	c.mu.Unlock()
}

func (c *wireOrderConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	out := c.relay(p)
	c.mu.Unlock()
	if len(out) > 0 {
		if _, err := c.Conn.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// relay returns the bytes to send for p, holding back an incomplete head.
func (c *wireOrderConn) relay(p []byte) []byte {
	if c.passthrough {
		return p
	}
	var out []byte
	for len(p) > 0 {
		if c.body > 0 {
			k := min(int64(len(p)), c.body)
			out = append(out, p[:k]...)
			c.body -= k
			p = p[k:]
			continue
		}
		c.head = append(c.head, p...)
		i := bytes.Index(c.head, headEnd)
		if i < 0 {
			if len(c.head) > maxWireHead {
				out = append(out, c.head...)
				c.head, c.passthrough = nil, true
			}
			break
		}
		head, rest := c.head[:i+len(headEnd)], c.head[i+len(headEnd):]
		order := c.next
		c.next = nil
		out = append(out, reorderRequestHead(head, order)...)
		_, headers, _ := parseRequestHead(head)
		n, framed := bodyLength(headers)
		if !framed {
			out = append(out, rest...)
			c.head, c.passthrough = nil, true
			break
		}
		c.body = n
		p = append([]byte(nil), rest...)
		c.head = nil
	}
	return out
}
