package profiletest

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
)

// newSocks5Stub starts an in-process minimal SOCKS5 server that
// accepts no-auth handshakes and tunnels CONNECT to backendAddr.
// Returns the listener and an atomic counter incremented per accepted
// CONNECT (caller uses it to assert the proxy was actually used).
//
// Handshake bytes:
//
//	client → server: 0x05 0x01 0x00            (version 5, 1 method, no-auth)
//	server → client: 0x05 0x00                 (no-auth selected)
//	client → server: 0x05 0x01 0x00 ATYP ADDR PORT  (CONNECT)
//	server → client: 0x05 0x00 0x00 0x01 0 0 0 0 0 0  (success, bogus BND)
//
// then bidirectional copy with backend.
func newSocks5Stub(t *testing.T, backendAddr string) (net.Listener, *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var connectCount int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go handleSocks5(c, backendAddr, &connectCount)
		}
	}()
	return ln, &connectCount
}

func handleSocks5(c net.Conn, backendAddr string, counter *int32) {
	defer c.Close()
	// Greeting: read VER + NMETHODS, then the methods byte(s).
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	// Reply: no-auth selected
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	// CONNECT request prefix: VER CMD RSV ATYP — 4 bytes
	connPrefix := make([]byte, 4)
	if _, err := io.ReadFull(c, connPrefix); err != nil {
		return
	}
	// Drain the address bytes based on ATYP.
	switch connPrefix[3] {
	case 0x01: // IPv4: 4 bytes
		if _, err := io.ReadFull(c, make([]byte, 4)); err != nil {
			return
		}
	case 0x03: // domain: 1 length byte + N name bytes
		addrLen := make([]byte, 1)
		if _, err := io.ReadFull(c, addrLen); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, int(addrLen[0]))); err != nil {
			return
		}
	case 0x04: // IPv6: 16 bytes
		if _, err := io.ReadFull(c, make([]byte, 16)); err != nil {
			return
		}
	default:
		return
	}
	// Drain the 2-byte port.
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		return
	}
	atomic.AddInt32(counter, 1)
	// Reply: success, BND.ADDR=0.0.0.0, BND.PORT=0
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	// Tunnel to backend
	backend, err := net.Dial("tcp", backendAddr)
	if err != nil {
		return
	}
	defer backend.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, backend); done <- struct{}{} }()
	<-done
}
