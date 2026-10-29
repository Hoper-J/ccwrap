package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// closesIdleConn sends one keep-alive request over c, reads the reply, then
// stays quiet; it reports whether the server closed the connection.
func closesIdleConn(t *testing.T, c net.Conn, path, host string) bool {
	t.Helper()
	defer c.Close()
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, host); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = br.ReadByte()
	return errors.Is(err, io.EOF)
}

// A keep-alive client that goes quiet holds a connection, a goroutine and
// its buffers on the server until the client closes. An idle timeout bounds
// that whatever the client does; event streams are unaffected, since the
// timeout only runs between requests.
func TestControlAndProxyServersCloseIdleConnections(t *testing.T) {
	prevControl, prevProxy := controlIdleTimeout, proxyIdleTimeout
	controlIdleTimeout, proxyIdleTimeout = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { controlIdleTimeout, proxyIdleTimeout = prevControl, prevProxy })
	srv, sess := newTestSessionForCreate(t)

	c, err := net.Dial("unix", srv.paths.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if !closesIdleConn(t, c, "/v1/status", "unix") {
		t.Fatal("the control socket kept an idle keep-alive connection open")
	}
	p, err := net.Dial("tcp", sess.proxy.ListenAddr())
	if err != nil {
		t.Fatal(err)
	}
	if !closesIdleConn(t, p, "/", sess.proxy.ListenAddr()) {
		t.Fatal("the session proxy kept an idle keep-alive connection open")
	}
	streamStaysOpen(t, srv.paths.SocketPath, "unix", "/api/v1/events", "unix")
}

// streamStaysOpen opens an event stream, reads its first event, and checks
// the stream is still open after sitting quiet for longer than the idle
// timeout: the timeout only runs between requests.
func streamStaysOpen(t *testing.T, addr, network, path, host string) {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, host); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v %v", resp, err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err = io.ReadAll(br)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("the event stream ended while quiet (%v); the idle timeout must not cut it", err)
	}
}
