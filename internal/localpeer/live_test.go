//go:build linux || darwin || windows

package localpeer

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

// acceptOne dials a fresh loopback listener with dial and returns the
// accepted (server-side) connection.
func acceptOne(t *testing.T, dial func(addr *net.TCPAddr) io.Closer) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	client := dial(ln.Addr().(*net.TCPAddr))
	t.Cleanup(func() { _ = client.Close() })
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLivePeerUIDOfOwnDialer(t *testing.T) {
	c := acceptOne(t, func(addr *net.TCPAddr) io.Closer {
		d, err := net.Dial("tcp", addr.String())
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
	uid, err := PeerUID(c)
	if err != nil {
		t.Fatalf("PeerUID: %v", err)
	}
	if want := uint32(os.Geteuid()); uid != want {
		t.Fatalf("PeerUID = %d, want own euid %d", uid, want)
	}
}

func TestLiveSelfTest(t *testing.T) {
	uid, err := SelfTest()
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if want := uint32(os.Geteuid()); uid != want {
		t.Fatalf("SelfTest = %d, want %d", uid, want)
	}
}

// End to end with the real lookup: an HTTP server behind NewListener still
// serves this process's own requests.
func TestLiveListenerServesOwnProcess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	wrapped := NewListener(ln, func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	})}
	go func() { _ = srv.Serve(wrapped) }()
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 3; i++ {
		resp, err := client.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "served" {
			t.Fatalf("body %q", body)
		}
		client.CloseIdleConnections() // fresh connection, fresh lookup
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 0 {
		t.Fatalf("own connections produced log lines: %v", logs)
	}
}
