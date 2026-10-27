package localpeer

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// maxScreening bounds concurrent owner lookups. Past it the accept loop
// waits and new connections queue in the kernel backlog.
const maxScreening = 8

// NewListener wraps ln so Accept only yields connections whose peer socket
// belongs to this process's uid or to root; other uids are closed on arrival.
// When the owner cannot be determined the connection is let through
// (fail-open), so an unknown peer is never treated worse than by an unwrapped
// listener. Peers that already closed their end (no owning process) are
// dropped: their uid cannot be trusted and nobody is left to read a reply.
//
// Lookups run off the Accept path, one goroutine per pending connection, so a
// slow lookup delays only its own connection. logf gets one line per refused
// uid and one per kind of lookup failure.
func NewListener(ln net.Listener, logf func(format string, args ...any)) net.Listener {
	return newListener(ln, uint32(os.Geteuid()), PeerUID, logf, policy{what: "port"})
}

// policy is how a listener treats peers beyond its own uid and root.
type policy struct {
	what   string // the port, as log lines name it ("session port")
	strict bool   // refuse a peer whose owner cannot be determined
}

type listener struct {
	net.Listener
	self    uint32
	peerUID func(net.Conn) (uint32, error)
	logf    func(format string, args ...any)
	policy  policy

	conns     chan net.Conn
	errs      chan error
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	acceptMu  sync.RWMutex // read-held by each Accept call; Close write-locks it to wait them out

	logMu  sync.Mutex
	logged map[string]bool
}

func newListener(ln net.Listener, self uint32, peerUID func(net.Conn) (uint32, error), logf func(string, ...any), p policy) *listener {
	l := &listener{
		Listener: ln,
		self:     self,
		peerUID:  peerUID,
		logf:     logf,
		policy:   p,
		conns:    make(chan net.Conn),
		errs:     make(chan error),
		slots:    make(chan struct{}, maxScreening),
		done:     make(chan struct{}),
		logged:   map[string]bool{},
	}
	go l.acceptLoop()
	return l
}

// Accept returns the next admitted connection, or the inner listener's error
// (net.ErrClosed after Close, so http.Server.Serve exits as before).
func (l *listener) Accept() (net.Conn, error) {
	l.acceptMu.RLock()
	defer l.acceptMu.RUnlock()
	select {
	case c := <-l.conns:
		select {
		case <-l.done:
			_ = c.Close()
			return nil, net.ErrClosed
		default:
			return c, nil
		}
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close unblocks pending Accept calls and, like closing a plain TCP listener,
// returns only after they have returned, so a Serve loop sees the error before
// a following http.Server.Shutdown looks for listeners to close.
func (l *listener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	err := l.Listener.Close()
	// In-flight Accept calls hold the read lock; taking the write lock waits
	// them out.
	l.acceptMu.Lock()
	l.acceptMu.Unlock()
	return err
}

func (l *listener) acceptLoop() {
	for {
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			return
		}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.slots
			// Hand every error to Accept so http.Server applies its usual
			// temporary-error backoff; only a closed listener ends the loop.
			select {
			case l.errs <- err:
			case <-l.done:
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go l.screen(c)
	}
}

func (l *listener) screen(c net.Conn) {
	uid, err := l.lookup(c)
	<-l.slots
	if !l.admit(c, uid, err) {
		_ = c.Close()
		return
	}
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *listener) lookup(c net.Conn) (uid uint32, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("owner lookup panicked: %v", r)
		}
	}()
	return l.peerUID(c)
}

// admit applies the policy: own uid and root pass, any other identified uid
// and ownerless peers are refused, an unidentified peer passes unless the
// listener is strict.
func (l *listener) admit(c net.Conn, uid uint32, err error) bool {
	switch {
	case err == nil && (uid == l.self || uid == 0):
		return true
	case err == nil:
		l.logOnce(fmt.Sprintf("uid %d", uid), "local peer check: refused connection from uid %d (%v); the %s accepts only uid %d and root (%s=0 disables this check)", uid, c.RemoteAddr(), l.policy.what, l.self, DisableEnv)
		return false
	case errors.Is(err, ErrOrphaned):
		l.logOnce("orphaned", "local peer check: refused connection from %v: %v", c.RemoteAddr(), err)
		return false
	case l.policy.strict:
		l.logOnce("lookup "+failureKind(err), "local peer check: could not identify the owner of %v (%v); refusing it, since the %s accepts only uid %d and root (%s=0 disables this check) — further failures of this kind are not logged", c.RemoteAddr(), err, l.policy.what, l.self, DisableEnv)
		return false
	default:
		l.logOnce("lookup "+failureKind(err), "local peer check: could not identify the owner of %v (%v); allowing it — further failures of this kind are not logged", c.RemoteAddr(), err)
		return true
	}
}

func failureKind(err error) string {
	for _, known := range []error{ErrNotFound, ErrUnsupported, errLayout} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "other"
}

func (l *listener) logOnce(key, format string, args ...any) {
	if l.logf == nil {
		return
	}
	l.logMu.Lock()
	first := !l.logged[key]
	l.logged[key] = true
	l.logMu.Unlock()
	if first {
		l.logf(format, args...)
	}
}
