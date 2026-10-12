package supervisor

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// crlf joins lines into a raw request head (request line, headers, blank line).
func crlf(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n") + "\r\n\r\n")
}

// claudeOrder is the layout Claude Code (Bun) puts on the wire: SDK headers in
// SDK order and casing, then the runtime's Connection/Host/Accept-Encoding/
// Content-Length.
func claudeOrder() *wireOrder {
	return &wireOrder{Method: "POST", Target: "/v1/messages?beta=true", Headers: []wireHeader{
		{"Accept", "application/json"},
		{"Content-Type", "application/json"},
		{"User-Agent", "claude-cli/2.1.296 (external, cli)"},
		{"X-Stainless-OS", "MacOS"},
		{"anthropic-version", "2023-06-01"},
		{"authorization", "Bearer ccwrap-placeholder"},
		{"x-app", "cli"},
		{"Connection", "keep-alive"},
		{"Host", "api.anthropic.com"},
		{"Accept-Encoding", "gzip, deflate, br, zstd"},
		{"Content-Length", "12"},
	}}
}

// goHead is what net/http writes for that request after ccwrap's rewrite:
// Host/User-Agent/Content-Length first, the rest sorted and canonicalized,
// Connection dropped as hop-by-hop.
func goHead(auth ...string) []byte {
	lines := []string{
		"POST /prefix/v1/messages?beta=true HTTP/1.1",
		"Host: gw.example:8443",
		"User-Agent: claude-cli/2.1.296 (external, cli)",
		"Content-Length: 15",
		"Accept: application/json",
		"Accept-Encoding: gzip, deflate, br, zstd",
		"Anthropic-Version: 2023-06-01",
	}
	lines = append(lines, auth...)
	lines = append(lines, "Content-Type: application/json", "X-App: cli", "X-Stainless-Os: MacOS")
	return crlf(lines...)
}

func TestReorderRequestHeadRestoresClientLayout(t *testing.T) {
	got := reorderRequestHead(goHead("Authorization: Bearer real-gateway-key"), claudeOrder())
	want := crlf(
		"POST /prefix/v1/messages?beta=true HTTP/1.1",
		"Accept: application/json",
		"Content-Type: application/json",
		"User-Agent: claude-cli/2.1.296 (external, cli)",
		"X-Stainless-OS: MacOS",
		"anthropic-version: 2023-06-01",
		"authorization: Bearer real-gateway-key",
		"x-app: cli",
		"Connection: keep-alive",
		"Host: gw.example:8443",
		"Accept-Encoding: gzip, deflate, br, zstd",
		"Content-Length: 15",
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("reordered head:\n%s\nwant:\n%s", got, want)
	}
}

// An x-api-key profile strips the client's authorization and sets X-Api-Key:
// the new credential takes the stripped header's slot and casing style.
func TestReorderRequestHeadPutsReplacedCredentialInItsSlot(t *testing.T) {
	got := string(reorderRequestHead(goHead("X-Api-Key: real-gateway-key"), claudeOrder()))
	if !strings.Contains(got, "anthropic-version: 2023-06-01\r\nx-api-key: real-gateway-key\r\nx-app: cli\r\n") {
		t.Fatalf("credential not placed in the stripped header's slot:\n%s", got)
	}
	if strings.Contains(strings.ToLower(got), "authorization") {
		t.Fatalf("stripped authorization came back:\n%s", got)
	}
}

func TestReorderRequestHeadKeepsAddedAndDropsRemovedHeaders(t *testing.T) {
	order := &wireOrder{Method: "GET", Target: "/v1/models", Headers: []wireHeader{
		{"Accept", "*/*"}, {"X-Removed", "1"}, {"Host", "api.anthropic.com"},
	}}
	head := crlf("GET /v1/models HTTP/1.1", "Host: api.anthropic.com", "Accept: */*", "X-Gateway-Tag: t")
	want := crlf("GET /v1/models HTTP/1.1", "Accept: */*", "X-Gateway-Tag: t", "Host: api.anthropic.com")
	if got := reorderRequestHead(head, order); !bytes.Equal(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestReorderRequestHeadWithoutOrderIsUnchanged(t *testing.T) {
	head := goHead("Authorization: Bearer k")
	if got := reorderRequestHead(head, nil); !bytes.Equal(got, head) {
		t.Fatalf("nil order changed the head:\n%s", got)
	}
}

// chunkConn serves fixed bytes to Read in small, uneven pieces and records
// whatever is written to it.
type chunkConn struct {
	net.Conn
	mu      sync.Mutex
	in      []byte
	sizes   []int
	written bytes.Buffer
}

func (c *chunkConn) Read(p []byte) (int, error) {
	if len(c.in) == 0 {
		return 0, io.EOF
	}
	n := 7
	if len(c.sizes) > 0 {
		n, c.sizes = c.sizes[0], c.sizes[1:]
	}
	n = min(n, len(p), len(c.in))
	copy(p, c.in[:n])
	c.in = c.in[n:]
	return n, nil
}

func (c *chunkConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(p)
}

func (c *chunkConn) Close() error                     { return nil }
func (c *chunkConn) SetDeadline(time.Time) error      { return nil }
func (c *chunkConn) SetReadDeadline(time.Time) error  { return nil }
func (c *chunkConn) SetWriteDeadline(time.Time) error { return nil }

func TestWireOrderRecorderQueuesEachRequestAcrossReads(t *testing.T) {
	body := "{\"x\":\"\r\n\r\n\"}" // a blank line inside the body must not look like a head
	stream := string(crlf("POST /v1/messages?beta=true HTTP/1.1", "accept: application/json", "Content-Length: "+strconv.Itoa(len(body)), "Host: api.anthropic.com")) + body +
		string(crlf("GET /v1/models HTTP/1.1", "Host: api.anthropic.com", "X-App: cli"))
	rec := &wireOrderRecorder{Conn: &chunkConn{in: []byte(stream), sizes: []int{3, 50, 1, 9, 200}}}
	got, err := io.ReadAll(rec)
	if err != nil || string(got) != stream {
		t.Fatalf("recorder altered the stream (err=%v)", err)
	}
	if o := rec.pop("GET", "/v1/models"); o == nil || o.Headers[0].Name != "Host" || o.Headers[1].Name != "X-App" {
		t.Fatalf("second request's order not matched past the first: %+v", o)
	}
	if o := rec.pop("POST", "/v1/messages?beta=true"); o != nil {
		t.Fatalf("the skipped first order must be gone: %+v", o)
	}
}

func TestWireOrderRecorderPopsInOrder(t *testing.T) {
	stream := string(crlf("POST /a HTTP/1.1", "content-length: 2", "Host: h")) + "{}" + string(crlf("POST /a HTTP/1.1", "Content-Length: 0", "host: h"))
	rec := &wireOrderRecorder{Conn: &chunkConn{in: []byte(stream)}}
	if _, err := io.ReadAll(rec); err != nil {
		t.Fatal(err)
	}
	first, second := rec.pop("POST", "/a"), rec.pop("POST", "/a")
	if first == nil || second == nil || first.Headers[0].Name != "content-length" || second.Headers[0].Name != "Content-Length" {
		t.Fatalf("orders popped out of sequence: %+v / %+v", first, second)
	}
}

// Chunked bodies cannot be skipped by length: the request's own order is kept,
// recording stops after it (later requests fall back to net/http's layout).
func TestWireOrderRecorderStopsAfterChunkedRequest(t *testing.T) {
	stream := string(crlf("POST /a HTTP/1.1", "Transfer-Encoding: chunked", "Host: h")) + "2\r\n{}\r\n0\r\n\r\n" + string(crlf("GET /b HTTP/1.1", "Host: h"))
	rec := &wireOrderRecorder{Conn: &chunkConn{in: []byte(stream)}}
	if _, err := io.ReadAll(rec); err != nil {
		t.Fatal(err)
	}
	if rec.pop("POST", "/a") == nil {
		t.Fatal("the chunked request's own order should be recorded")
	}
	if rec.pop("GET", "/b") != nil {
		t.Fatal("recording must stop after a chunked body")
	}
}

func TestWireOrderConnReordersEachRequestHead(t *testing.T) {
	under := &chunkConn{}
	c := &wireOrderConn{Conn: under}
	c.setNext(claudeOrder())
	first := append(goHead("Authorization: Bearer real-gateway-key"), []byte("{\"a\":\"b\"}12345")...) // 15-byte body
	// net/http may hand the head to the conn in pieces.
	for _, part := range [][]byte{first[:40], first[40:41], first[41:]} {
		if n, err := c.Write(part); err != nil || n != len(part) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	// A reused connection without a new order keeps net/http's layout.
	second := crlf("GET /v1/models HTTP/1.1", "Host: gw.example:8443", "Accept: */*")
	if _, err := c.Write(second); err != nil {
		t.Fatal(err)
	}
	want := string(reorderRequestHead(goHead("Authorization: Bearer real-gateway-key"), claudeOrder())) + "{\"a\":\"b\"}12345" + string(second)
	if got := under.written.String(); got != want {
		t.Fatalf("upstream bytes:\n%q\nwant:\n%q", got, want)
	}
}
