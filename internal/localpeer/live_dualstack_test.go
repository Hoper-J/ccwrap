//go:build linux || darwin

package localpeer

import (
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// A dual-stack AF_INET6 socket connecting to ::ffff:127.0.0.1 reaches the
// IPv4 listener too; its table row must still be found.
func TestLivePeerUIDOfDualStackDialer(t *testing.T) {
	c := acceptOne(t, func(addr *net.TCPAddr) io.Closer {
		fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Skipf("no IPv6 sockets: %v", err)
		}
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
			_ = syscall.Close(fd)
			t.Skipf("cannot clear IPV6_V6ONLY: %v", err)
		}
		sa := &syscall.SockaddrInet6{Port: addr.Port}
		copy(sa.Addr[:], net.ParseIP("::ffff:127.0.0.1").To16())
		if err := syscall.Connect(fd, sa); err != nil {
			_ = syscall.Close(fd)
			t.Skipf("v4-mapped connect unsupported here: %v", err)
		}
		return os.NewFile(uintptr(fd), "dual-stack-dialer")
	})
	uid, err := PeerUID(c)
	if err != nil {
		t.Fatalf("PeerUID: %v", err)
	}
	if want := uint32(os.Geteuid()); uid != want {
		t.Fatalf("PeerUID = %d, want own euid %d", uid, want)
	}
}
