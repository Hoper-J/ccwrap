package supervisor

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// A plain-HTTP proxy request goes through the same pipeline as a MITM'd one,
// so with capture on its response body is captured too.
func TestForwardProxyPathCapturesResponseBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer upstream.Close()
	client, sess, _ := streamingCaptureSession(t, "fwdcap.sock", upstream)

	hc := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustParse(t, "http://"+sess.ProxyListenAddr)),
	}}
	resp, err := hc.Post("http://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != `{"type":"message"}` {
		t.Fatalf("client body = %q", body)
	}

	requests := waitForRequestRecord(t, client, sess.ID, "forward-path record with a response body", func(rec model.RequestRecord) bool {
		return rec.Path == "/v1/messages" && rec.ResponseBodyRef != nil
	})
	var rec model.RequestRecord
	for _, r := range requests {
		if r.Path == "/v1/messages" && r.ResponseBodyRef != nil {
			rec = r
		}
	}
	if rec.BodyRef == nil {
		t.Fatal("request body was not captured")
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := pollBodyFile(t, filepath.Join(status.RuntimeDir, "bodies", rec.ResponseBodyRef.ID+".json")); string(got) != `{"type":"message"}` {
		t.Fatalf("spilled response body = %q", got)
	}
}

// A plain-HTTP proxy request that cannot be routed is refused with 502 and
// leaves both an error and a trace entry, like the MITM path.
func TestForwardProxyUnroutableRequestIsTraced(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	client, sess, _ := streamingCaptureSessionClass(t, "fwdroute.sock", upstream, model.RouteClassFirstParty)

	conn, err := net.Dial("tcp", sess.ProxyListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// An absolute URI with a port but no host name.
	if _, err := io.WriteString(conn, "GET http://:80/x HTTP/1.1\r\nHost: :80\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	errs, err := client.Errors(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) == 0 || errs[len(errs)-1].ErrorClass != "invalid_upstream_url" {
		t.Fatalf("errors = %+v, want invalid_upstream_url", errs)
	}
	trace, err := client.Trace(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trace {
		if tr.Category == "route" && tr.Summary == "route resolution failed" {
			return
		}
	}
	t.Fatalf("no route-failure trace: %+v", trace)
}
