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

	"github.com/Hoper-J/ccwrap/internal/model"
)

// A client that opens a MITM tunnel and then sends nothing is dropped once
// the deadline passes. The deadline covers reading its ClientHello (native
// TLS on) as well as the TLS handshake, so the hijacked connection and its
// goroutine are not held until the session ends.
func TestMITMDropsASilentClient(t *testing.T) {
	prev := mitmHandshakeTimeout
	mitmHandshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mitmHandshakeTimeout = prev })
	_, sess := routedSessionPaths(t, model.SessionRouteRequest{
		APIBaseURL: "https://api.anthropic.com", ExactUpstreamHost: "api.anthropic.com", ExactUpstreamBase: "https://api.anthropic.com",
		RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceExplicit,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone,
		Egress:     model.EgressConfig{Mode: "direct", Source: "test"},
		FailPolicy: model.FailClosed, NativeTLS: true,
	})

	c, err := net.Dial("tcp", sess.ProxyListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("a silent MITM tunnel was kept open: %v", err)
	}
}
