// Package localpeer identifies which local user owns the other end of a
// loopback TCP connection, so a session port can refuse other users.
//
// A ccwrap session listens on 127.0.0.1:<random>; that one port is both the
// HTTP proxy Claude Code uses (requests leave with the user's gateway
// credential injected) and the dashboard/info endpoints (captured bodies).
// Loopback binding keeps the network out but not other accounts on the same
// machine, and a token in the proxy URL is no defence because HTTPS_PROXY
// reaches Claude Code on argv, which `ps` shows to everyone. The kernel does
// know who opened the connecting socket, so the listener asks it: the peer
// socket is the row whose local end is our remote address and whose remote
// end is our local address.
//
// Trust boundary: the ccwrap process's own uid and root. Same-uid processes
// can already read profiles.json; root can read anything.
package localpeer

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"time"
)

// DisableEnv names the escape hatch: CCWRAP_LOCAL_PEER_CHECK=0 turns the
// check off (read when a session proxy is created).
const DisableEnv = "CCWRAP_LOCAL_PEER_CHECK"

// Disabled reports whether the escape hatch is set.
func Disabled(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(DisableEnv)) == "0"
}

var (
	// ErrUnsupported: this OS has no lookup implementation.
	ErrUnsupported = errors.New("socket owner lookup is not supported on " + runtime.GOOS)
	// ErrNotFound: the kernel table has no row for the peer socket.
	ErrNotFound = errors.New("peer socket not found in the kernel TCP table")
	// ErrOrphaned: the peer row exists but no process holds it any more
	// (closed / TIME_WAIT). Such rows can report uid 0 on Linux, so their
	// uid is never trusted.
	ErrOrphaned = errors.New("peer socket has no owning process")
	// errLayout: the kernel table did not look like what the parser expects;
	// the lookup reports unavailable instead of guessing.
	errLayout = errors.New("unexpected kernel socket table layout")
)

// sockKey is one TCP socket as the kernel lists it: its own address and the
// address it is connected to. Addresses are unmapped (::ffff:a.b.c.d → a.b.c.d)
// so a dual-stack socket matches its IPv4 counterpart.
type sockKey struct {
	local, remote netip.AddrPort
}

func newSockKey(local, remote netip.AddrPort) sockKey {
	return sockKey{
		local:  netip.AddrPortFrom(local.Addr().Unmap(), local.Port()),
		remote: netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port()),
	}
}

// sockInfo is what a table row says about a socket's owner.
type sockInfo struct {
	uid    uint32
	orphan bool // no process holds the socket; uid is not meaningful
}

// lookupSockets is the platform table read (lookup_<os>.go). It returns rows
// for whichever of keys it finds; a missing key is simply absent.
var lookupSockets = platformLookup

// rowSet collects table rows for a fixed set of keys. A live row wins over an
// orphan one for the same tuple (a reused tuple can briefly list both).
type rowSet struct {
	want  map[sockKey]bool
	found map[sockKey]sockInfo
	live  int
}

func newRowSet(keys []sockKey) *rowSet {
	s := &rowSet{want: make(map[sockKey]bool, len(keys)), found: make(map[sockKey]sockInfo, len(keys))}
	for _, k := range keys {
		s.want[k] = true
	}
	return s
}

// add records a row and returns false once every key has a live row, so a
// scan can stop early.
func (s *rowSet) add(k sockKey, info sockInfo) bool {
	if s.want[k] {
		if prev, seen := s.found[k]; !seen || (prev.orphan && !info.orphan) {
			s.found[k] = info
			if !info.orphan {
				s.live++
			}
		}
	}
	return !s.complete()
}

func (s *rowSet) complete() bool { return s.live == len(s.want) }

// retryDelay spaces the single retry when the peer row is not (yet) listed.
const retryDelay = 5 * time.Millisecond

// PeerUID returns the uid that owns the peer socket of conn, a loopback TCP
// connection this process accepted.
func PeerUID(conn net.Conn) (uint32, error) {
	local, err := addrPort(conn.LocalAddr())
	if err != nil {
		return 0, err
	}
	remote, err := addrPort(conn.RemoteAddr())
	if err != nil {
		return 0, err
	}
	return peerUID(local, remote, uint32(os.Geteuid()))
}

func peerUID(local, remote netip.AddrPort, self uint32) (uint32, error) {
	ours := newSockKey(local, remote)
	peer := newSockKey(remote, local)
	for attempt := 0; ; attempt++ {
		rows, err := lookupSockets(peer, ours)
		if err != nil {
			return 0, err
		}
		// Our own accepted socket is in the same snapshot and must read back
		// as our uid; if it does not, the parser is misreading the table and
		// its answer for the peer is worthless.
		if own, ok := rows[ours]; ok && !own.orphan && own.uid != self {
			return 0, fmt.Errorf("%w: own socket reports uid %d, expected %d", errLayout, own.uid, self)
		}
		if p, ok := rows[peer]; ok {
			if p.orphan {
				return 0, ErrOrphaned
			}
			return p.uid, nil
		}
		if attempt == 1 {
			return 0, ErrNotFound
		}
		time.Sleep(retryDelay)
	}
}

func addrPort(a net.Addr) (netip.AddrPort, error) {
	tcp, ok := a.(*net.TCPAddr)
	if !ok || tcp == nil {
		return netip.AddrPort{}, fmt.Errorf("not a TCP address: %v", a)
	}
	return tcp.AddrPort(), nil
}

// SelfTest dials a throwaway loopback listener and resolves the dialer's uid
// through the same lookup a session listener uses. It returns the uid the
// lookup found (which should be this process's euid) or why it could not.
func SelfTest() (uint32, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	dialer, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer dialer.Close()
	select {
	case c, ok := <-accepted:
		if !ok {
			return 0, errors.New("self-test listener failed to accept")
		}
		defer c.Close()
		return PeerUID(c)
	case <-time.After(2 * time.Second):
		return 0, errors.New("self-test listener did not accept in time")
	}
}
