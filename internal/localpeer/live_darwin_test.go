//go:build darwin

package localpeer

import (
	"net"
	"net/netip"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// The whole live pcb list must parse — every record, not just the rows a
// lookup happens to stop at — and list a fresh connection on both ends with
// this process's uid.
func TestLiveDarwinPCBListParses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		t.Fatalf("sysctl net.inet.tcp.pcblist_n: %v", err)
	}
	rows := map[sockKey]sockInfo{}
	if err := parsePCBList(buf, func(k sockKey, info sockInfo) bool {
		rows[k] = info
		return true
	}); err != nil {
		t.Fatalf("live pcblist_n did not parse: %v", err)
	}
	dl := d.LocalAddr().(*net.TCPAddr).AddrPort()
	al := a.LocalAddr().(*net.TCPAddr).AddrPort()
	self := uint32(os.Geteuid())
	for name, k := range map[string]sockKey{
		"listener": newSockKey(al, netip.AddrPortFrom(netip.IPv4Unspecified(), 0)),
		"dialer":   newSockKey(dl, al),
		"accepted": newSockKey(al, dl),
	} {
		got, ok := rows[k]
		if !ok || got.orphan || got.uid != self {
			t.Errorf("%s %v = %+v (present %v), want uid %d", name, k, got, ok, self)
		}
	}
	t.Logf("parsed %d live TCP sockets", len(rows))
}
