package supervisor

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// An upstream (or egress proxy) that accepts TCP and then never answers the
// TLS handshake must not hold the dial forever: every retry would pin
// another goroutine and socket. The native handshake gives up after its
// timeout.
func TestNativeHandshakeGivesUpOnASilentPeer(t *testing.T) {
	prev := nativeHandshakeTimeout
	nativeHandshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { nativeHandshakeTimeout = prev })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() }) // accept, never answer
		}
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	done := make(chan error, 1)
	go func() { done <- nativeUTLSHandshake(raw, "api.anthropic.com", x509.NewCertPool(), standInHello(t)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("handshake with a silent peer succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake with a silent peer is still waiting after 5s")
	}
}

// deadlineConn records the last deadline set on it.
type deadlineConn struct {
	net.Conn
	last time.Time
	sets int
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.last, c.sets = t, c.sets+1
	return c.Conn.SetDeadline(t)
}

// After a successful handshake the deadline is cleared, so a long response
// stream on the mirrored connection is not cut.
func TestNativeHandshakeClearsItsDeadline(t *testing.T) {
	origin, ca := startTLSOrigin(t, "api.anthropic.com")
	raw, err := net.Dial("tcp", origin)
	if err != nil {
		t.Fatal(err)
	}
	dc := &deadlineConn{Conn: raw}
	c, err := nativeUTLSDialOver(dc, "api.anthropic.com", poolTrusting(ca), standInHello(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if dc.sets != 2 || !dc.last.IsZero() {
		t.Fatalf("deadline set %d time(s), last %v; want set for the handshake, then cleared", dc.sets, dc.last)
	}
}
