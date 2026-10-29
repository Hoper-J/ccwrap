package supervisor

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// connCountingServer is a plain HTTP server that reports how many client
// connections it has open.
func connCountingServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	open := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case http.StateNew:
			open++
		case http.StateClosed, http.StateHijacked:
			open--
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return open
	}
}

// keepIdleConn makes one request through tr, leaving an idle keep-alive
// connection to srv in tr's pool.
func keepIdleConn(t *testing.T, tr *http.Transport, srv *httptest.Server) {
	t.Helper()
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// openSettles waits for the server's open-connection count to reach want.
func openSettles(open func() int, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for open() != want {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// Closing a session drops the idle upstream connections of every transport
// cache: the plain one and the two native-TLS ones (per egress, per hello
// shape).
func TestSessionProxyCloseDropsIdleConnsOfEveryCache(t *testing.T) {
	srv, open := connCountingServer(t)
	sp := newTestSessionProxy(t)
	sp.stackTransports = map[stackTransportKey]*http.Transport{}
	for _, tr := range []*http.Transport{{}, {}, {}} {
		keepIdleConn(t, tr, srv)
	}
	plain, native, stack := &http.Transport{}, &http.Transport{}, &http.Transport{}
	keepIdleConn(t, plain, srv)
	keepIdleConn(t, native, srv)
	keepIdleConn(t, stack, srv)
	sp.transports["plain"] = plain
	sp.nativeTransports["native"] = native
	sp.stackTransports[stackTransportKey{egress: "e", shape: "s"}] = stack
	if !openSettles(open, 6) {
		t.Fatalf("setup: %d connections open, want 6", open())
	}
	_ = sp.Close()
	// The three throwaway transports above keep theirs; the session's three
	// caches must have let go of all of their idle connections.
	if !openSettles(open, 3) {
		t.Fatalf("%d upstream connection(s) open after the session closed, want the 3 outside the session", open())
	}
}
