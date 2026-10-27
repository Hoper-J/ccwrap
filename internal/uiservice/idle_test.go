package uiservice

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/testutil"
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

// Both ui servers close a keep-alive connection that goes quiet, so a client
// that never closes (a script, an old ccwrap) cannot pile them up.
func TestUIServersCloseIdleConnections(t *testing.T) {
	prev := connIdleTimeout
	connIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { connIdleTimeout = prev })
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)

	u, err := net.Dial("unix", r.info.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if !closesIdleConn(t, u, "/api/v1/meta", "unix") {
		t.Fatal("ui.sock kept an idle keep-alive connection open")
	}
	h, err := net.Dial("tcp", r.info.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	if !closesIdleConn(t, h, "/api/v1/meta", r.info.HTTPAddr) {
		t.Fatal("the ui's HTTP port kept an idle keep-alive connection open")
	}
	streamStaysOpen(t, r.info.HTTPAddr, "tcp", "/api/v1/events", r.info.HTTPAddr)
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
