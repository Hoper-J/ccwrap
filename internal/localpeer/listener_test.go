package localpeer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type lookupResult struct {
	uid   uint32
	err   error
	panic bool
}

// scriptedListener wraps a real loopback listener with an injected owner
// lookup whose answer the test sets before each dial.
type scriptedListener struct {
	*listener
	mu   sync.Mutex
	next lookupResult
	logs []string
}

const testSelf = 1000

func newScriptedListener(t *testing.T) *scriptedListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &scriptedListener{}
	s.listener = newListener(ln, testSelf, func(net.Conn) (uint32, error) {
		s.mu.Lock()
		r := s.next
		s.mu.Unlock()
		if r.panic {
			panic("lookup blew up")
		}
		return r.uid, r.err
	}, func(format string, args ...any) {
		s.mu.Lock()
		s.logs = append(s.logs, fmt.Sprintf(format, args...))
		s.mu.Unlock()
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *scriptedListener) set(r lookupResult) {
	s.mu.Lock()
	s.next = r
	s.mu.Unlock()
}

func (s *scriptedListener) logText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.logs, "\n")
}

// dialOutcome dials the listener and reports whether the connection was
// served (the test server answers "ok") or closed unanswered.
func dialOutcome(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	switch {
	case err == nil:
		return strings.TrimSpace(line)
	case errors.Is(err, io.EOF) || strings.Contains(err.Error(), "reset"):
		return "closed"
	default:
		t.Fatalf("read: %v", err)
		return ""
	}
}

func serveOK(s *scriptedListener) {
	go func() {
		for {
			c, err := s.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "ok\n")
			_ = c.Close()
		}
	}()
}

func TestListenerAdmitsOwnUIDAndRootOnly(t *testing.T) {
	s := newScriptedListener(t)
	serveOK(s)
	cases := []struct {
		name string
		res  lookupResult
		want string
	}{
		{"own uid", lookupResult{uid: testSelf}, "ok"},
		{"root", lookupResult{uid: 0}, "ok"},
		{"other uid", lookupResult{uid: 1001}, "closed"},
		{"other uid again", lookupResult{uid: 1001}, "closed"},
		{"orphaned peer", lookupResult{err: ErrOrphaned}, "closed"},
		{"peer not found", lookupResult{err: fmt.Errorf("x: %w", ErrNotFound)}, "ok"},
		{"unsupported", lookupResult{err: ErrUnsupported}, "ok"},
		{"layout", lookupResult{err: fmt.Errorf("%w: test", errLayout)}, "ok"},
		{"lookup panics", lookupResult{panic: true}, "ok"},
	}
	for _, tc := range cases {
		s.set(tc.res)
		if got := dialOutcome(t, s.Addr().String()); got != tc.want {
			t.Errorf("%s: outcome %q, want %q", tc.name, got, tc.want)
		}
	}
	logs := s.logText()
	if n := strings.Count(logs, "refused connection from uid 1001"); n != 1 {
		t.Errorf("uid 1001 refusal logged %d times, want once:\n%s", n, logs)
	}
	for _, want := range []string{"session ports accept only uid 1000 and root", "CCWRAP_LOCAL_PEER_CHECK=0", "no owning process", ErrNotFound.Error(), ErrUnsupported.Error(), errLayout.Error(), "panicked"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
	// One line each: uid 1001, orphaned, and four lookup-failure kinds
	// (not found, unsupported, layout, other); admitted peers are silent.
	if n := strings.Count(logs, "\n") + 1; n != 6 {
		t.Errorf("got %d log lines, want 6:\n%s", n, logs)
	}
}

func TestListenerLogsEachFailureKindOnce(t *testing.T) {
	s := newScriptedListener(t)
	serveOK(s)
	s.set(lookupResult{err: ErrNotFound})
	for i := 0; i < 3; i++ {
		if got := dialOutcome(t, s.Addr().String()); got != "ok" {
			t.Fatalf("dial %d: %q", i, got)
		}
	}
	if n := strings.Count(s.logText(), "could not identify"); n != 1 {
		t.Fatalf("lookup failure logged %d times, want 1:\n%s", n, s.logText())
	}
}

func TestListenerAcceptReturnsErrClosedAfterClose(t *testing.T) {
	s := newScriptedListener(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.Accept()
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
	if _, err := s.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Accept = %v, want net.ErrClosed", err)
	}
}

func TestListenerHTTPServeExitsOnClose(t *testing.T) {
	s := newScriptedListener(t)
	s.set(lookupResult{uid: testSelf})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	})}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(s) }()
	resp, err := http.Get("http://" + s.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "served" {
		t.Fatalf("body %q", body)
	}
	_ = s.Close()
	select {
	case err := <-served:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve returned %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}

func TestListenerSlowLookupDoesNotBlockOthers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	l := newListener(ln, testSelf, func(net.Conn) (uint32, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
		}
		return testSelf, nil
	}, nil)
	defer l.Close()

	slow, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	<-entered
	fast, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()

	got := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			got <- c
		}
	}()
	select {
	case c := <-got:
		if c.RemoteAddr().String() != fast.LocalAddr().String() {
			t.Fatalf("first admitted conn is from %v, want the fast dialer %v", c.RemoteAddr(), fast.LocalAddr())
		}
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked lookup stalled Accept for other connections")
	}
	close(release)
	select {
	case c := <-got:
		if c.RemoteAddr().String() != slow.LocalAddr().String() {
			t.Fatalf("second admitted conn is from %v, want %v", c.RemoteAddr(), slow.LocalAddr())
		}
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("slow connection never admitted")
	}
}

func TestListenerCloseDropsScreenedConnections(t *testing.T) {
	s := newScriptedListener(t)
	s.set(lookupResult{uid: testSelf})
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond) // screened, waiting for an Accept that never comes
	_ = s.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("pending connection still open after Close: %v", err)
	}
}

func TestPeerUIDUsesBothSidesOfTheSnapshot(t *testing.T) {
	local, remote := ap("127.0.0.1:4000"), ap("127.0.0.1:50000")
	ours := newSockKey(local, remote)
	peer := newSockKey(remote, local)
	cases := []struct {
		name    string
		rows    []map[sockKey]sockInfo // one per call
		wantUID uint32
		wantErr error
		calls   int
	}{
		{"peer found", []map[sockKey]sockInfo{{ours: {uid: 7}, peer: {uid: 42}}}, 42, nil, 1},
		{"peer found on retry", []map[sockKey]sockInfo{{ours: {uid: 7}}, {ours: {uid: 7}, peer: {uid: 0}}}, 0, nil, 2},
		{"peer never found", []map[sockKey]sockInfo{{}, {}}, 0, ErrNotFound, 2},
		{"peer orphaned", []map[sockKey]sockInfo{{peer: {orphan: true}}}, 0, ErrOrphaned, 1},
		{"own socket misread", []map[sockKey]sockInfo{{ours: {uid: 99}, peer: {uid: 7}}}, 0, errLayout, 1},
	}
	defer func(orig func(...sockKey) (map[sockKey]sockInfo, error)) { lookupSockets = orig }(lookupSockets)
	for _, tc := range cases {
		calls := 0
		lookupSockets = func(keys ...sockKey) (map[sockKey]sockInfo, error) {
			if len(keys) != 2 || keys[0] != peer || keys[1] != ours {
				t.Fatalf("%s: lookup keys %v", tc.name, keys)
			}
			r := tc.rows[calls]
			calls++
			return r, nil
		}
		uid, err := peerUID(local, remote, 7)
		if uid != tc.wantUID || !errors.Is(err, tc.wantErr) || calls != tc.calls {
			t.Errorf("%s: uid=%d err=%v calls=%d, want uid=%d err=%v calls=%d", tc.name, uid, err, calls, tc.wantUID, tc.wantErr, tc.calls)
		}
	}
	lookupSockets = func(...sockKey) (map[sockKey]sockInfo, error) { return nil, ErrUnsupported }
	if _, err := peerUID(local, remote, 7); !errors.Is(err, ErrUnsupported) {
		t.Errorf("platform error not propagated: %v", err)
	}
}

func TestPeerUIDUnmapsAddresses(t *testing.T) {
	defer func(orig func(...sockKey) (map[sockKey]sockInfo, error)) { lookupSockets = orig }(lookupSockets)
	lookupSockets = func(keys ...sockKey) (map[sockKey]sockInfo, error) {
		return map[sockKey]sockInfo{newSockKey(ap("127.0.0.1:50000"), ap("127.0.0.1:4000")): {uid: 3}}, nil
	}
	uid, err := peerUID(ap("[::ffff:127.0.0.1]:4000"), netip.AddrPortFrom(netip.MustParseAddr("::ffff:127.0.0.1"), 50000), 7)
	if err != nil || uid != 3 {
		t.Fatalf("uid=%d err=%v", uid, err)
	}
}

func TestRowSetPrefersLiveRows(t *testing.T) {
	k := newSockKey(ap("127.0.0.1:1"), ap("127.0.0.1:2"))
	other := newSockKey(ap("127.0.0.1:3"), ap("127.0.0.1:4"))
	s := newRowSet([]sockKey{k, other})
	if !s.add(k, sockInfo{orphan: true}) {
		t.Fatal("scan stopped with only an orphan row")
	}
	s.add(k, sockInfo{uid: 5})
	s.add(k, sockInfo{orphan: true})
	if got := s.found[k]; got != (sockInfo{uid: 5}) {
		t.Fatalf("row = %+v, want the live one", got)
	}
	if s.add(other, sockInfo{uid: 6}) {
		t.Fatal("scan should stop once every key has a live row")
	}
}

func TestDisabled(t *testing.T) {
	for v, want := range map[string]bool{"0": true, " 0 ": true, "": false, "1": false, "false": false, "off": false} {
		if got := Disabled(func(string) string { return v }); got != want {
			t.Errorf("Disabled(%q) = %v, want %v", v, got, want)
		}
	}
}
