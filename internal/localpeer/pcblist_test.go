package localpeer

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"testing"
)

// testdata/pcblist_n_darwin25.bin is a real net.inet.tcp.pcblist_n buffer
// from Darwin 25.2 (macOS 26), cut down to a handful of sockets plus the
// original header and trailer. Every expectation below was checked when it
// was captured: lsof for the uid-501 test sockets, netstat -anv (pid, matched
// by gencnt) plus ps for the sockets of root, _mdnsresponder (65) and
// _networkd (24).
func TestParsePCBListRealDarwinBuffer(t *testing.T) {
	buf, err := os.ReadFile("testdata/pcblist_n_darwin25.bin")
	if err != nil {
		t.Fatal(err)
	}
	got := map[sockKey]sockInfo{}
	if err := parsePCBList(buf, func(k sockKey, info sockInfo) bool {
		got[k] = info
		return true
	}); err != nil {
		t.Fatalf("parsePCBList: %v", err)
	}
	want := map[sockKey]sockInfo{
		// sing-box (root) connected to a local proxy.
		newSockKey(ap("127.0.0.1:49384"), ap("127.0.0.1:7890")): {uid: 0},
		// IPv4 listener, an IPv4 dialer and the accepted side.
		newSockKey(ap("127.0.0.1:47125"), ap("0.0.0.0:0")):       {uid: 501},
		newSockKey(ap("127.0.0.1:49278"), ap("127.0.0.1:47125")): {uid: 501},
		newSockKey(ap("127.0.0.1:47125"), ap("127.0.0.1:49278")): {uid: 501},
		// AF_INET6 socket connected to ::ffff:127.0.0.1 (the kernel flips it
		// to INP_IPV4) and its accepted side.
		newSockKey(ap("127.0.0.1:49279"), ap("127.0.0.1:47125")): {uid: 501},
		newSockKey(ap("127.0.0.1:47125"), ap("127.0.0.1:49279")): {uid: 501},
		// ::1 listener and pair.
		newSockKey(ap("[::1]:47125"), ap("[::]:0")):      {uid: 501},
		newSockKey(ap("[::1]:49282"), ap("[::1]:47125")): {uid: 501},
		newSockKey(ap("[::1]:47125"), ap("[::1]:49282")): {uid: 501},
		newSockKey(ap("[::]:53"), ap("[::]:0")):          {uid: 65},
		newSockKey(ap("0.0.0.0:60194"), ap("0.0.0.0:0")): {uid: 24}, // tcp46 listener (INP_IPV4|INP_IPV6)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sockets, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("socket %v = %+v (present %v), want %+v", k, g, ok, w)
		}
	}
}

// pcbGroup builds one socket's six records with the field offsets the parser
// relies on and the record sizes Darwin 25 emits.
type pcbGroup struct {
	local, remote netip.AddrPort
	vflag         byte
	uid           uint32
	inpp, soPCB   uint64
	family        uint32
	inpLen        int
}

func (g pcbGroup) bytes() []byte {
	le := binary.LittleEndian
	record := func(kind uint32, n int) []byte {
		r := make([]byte, (n+7)&^7)
		le.PutUint32(r, uint32(n))
		le.PutUint32(r[4:], kind)
		return r
	}
	inpLen := g.inpLen
	if inpLen == 0 {
		inpLen = 104
	}
	inp := record(xsoInpcb, inpLen)
	if inpLen >= inpMinLen {
		le.PutUint64(inp[inpInpp:], g.inpp)
		binary.BigEndian.PutUint16(inp[inpFport:], g.remote.Port())
		binary.BigEndian.PutUint16(inp[inpLport:], g.local.Port())
		inp[inpVflag] = g.vflag
		if g.vflag&inpIPv4 != 0 {
			l, r := g.local.Addr().As4(), g.remote.Addr().As4()
			copy(inp[inpLaddr+12:], l[:])
			copy(inp[inpFaddr+12:], r[:])
		} else {
			l, r := g.local.Addr().As16(), g.remote.Addr().As16()
			copy(inp[inpLaddr:], l[:])
			copy(inp[inpFaddr:], r[:])
		}
	}
	so := record(xsoSocket, 104)
	if g.soPCB != 0 {
		le.PutUint64(so[soPCB:], g.soPCB)
		le.PutUint32(so[soProtocol:], ipprotoTCP)
		le.PutUint32(so[soFamily:], g.family)
		le.PutUint32(so[soUID:], g.uid)
	}
	out := append(inp, so...)
	out = append(out, record(xsoRcvbuf, 32)...)
	out = append(out, record(xsoSndbuf, 32)...)
	out = append(out, record(xsoStats, 136)...)
	return append(out, record(xsoTcpcb, 204)...)
}

func xinpgen() []byte {
	b := make([]byte, xinpgenLen)
	binary.LittleEndian.PutUint32(b, xinpgenLen)
	return b
}

func pcbBuffer(groups ...[]byte) []byte {
	out := xinpgen()
	for _, g := range groups {
		out = append(out, g...)
	}
	return append(out, xinpgen()...)
}

func v4Group(local, remote string, uid uint32) pcbGroup {
	return pcbGroup{local: ap(local), remote: ap(remote), vflag: inpIPv4, uid: uid, inpp: 0xabc, soPCB: 0xabc, family: afInet}
}

func parseAll(buf []byte) (map[sockKey]sockInfo, error) {
	got := map[sockKey]sockInfo{}
	err := parsePCBList(buf, func(k sockKey, info sockInfo) bool {
		got[k] = info
		return true
	})
	return got, err
}

func TestParsePCBListSynthetic(t *testing.T) {
	v6 := pcbGroup{local: ap("[::1]:5000"), remote: ap("[::1]:6000"), vflag: inpIPv6, uid: 77, inpp: 7, soPCB: 7, family: afInetDarwin}
	got, err := parseAll(pcbBuffer(v4Group("127.0.0.1:5000", "127.0.0.1:6000", 501).bytes(), v6.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if g := got[newSockKey(ap("127.0.0.1:5000"), ap("127.0.0.1:6000"))]; g != (sockInfo{uid: 501}) {
		t.Errorf("v4 socket = %+v", g)
	}
	if g := got[newSockKey(ap("[::1]:5000"), ap("[::1]:6000"))]; g != (sockInfo{uid: 77}) {
		t.Errorf("v6 socket = %+v", g)
	}
}

func TestParsePCBListEmptyTable(t *testing.T) {
	got, err := parseAll(pcbBuffer())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestParsePCBListSocketlessPCBIsOrphan(t *testing.T) {
	g := v4Group("127.0.0.1:5000", "127.0.0.1:6000", 0)
	g.soPCB = 0 // kernel zero-fills xsocket_n when the pcb has no socket
	got, err := parseAll(pcbBuffer(g.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if info := got[newSockKey(ap("127.0.0.1:5000"), ap("127.0.0.1:6000"))]; !info.orphan {
		t.Fatalf("socketless pcb = %+v, want orphan", info)
	}
}

func TestParsePCBListRejectsUnexpectedLayout(t *testing.T) {
	good := v4Group("127.0.0.1:5000", "127.0.0.1:6000", 501)
	mismatch := good
	mismatch.soPCB = 0xdef // so_pcb must equal xi_inpp of the same socket
	wrongFamily := good
	wrongFamily.family = 1
	shortInp := good
	shortInp.inpLen = inpMinLen - 4
	full := pcbBuffer(good.bytes())
	dupKind := append(xinpgen(), good.bytes()[:104]...) // xinpcb_n twice
	dupKind = append(dupKind, good.bytes()...)
	dupKind = append(dupKind, xinpgen()...)
	badHeader := append([]byte(nil), full...)
	binary.LittleEndian.PutUint32(badHeader, 32)
	partial := append(xinpgen(), good.bytes()[:208]...) // group cut short by the trailer
	partial = append(partial, xinpgen()...)
	overrun := append([]byte(nil), full[:len(full)-xinpgenLen]...)
	overrun = append(overrun, 0xff, 0, 0, 0, xsoInpcb, 0, 0, 0)
	unknownKind := append([]byte(nil), full...)
	binary.LittleEndian.PutUint32(unknownKind[xinpgenLen+4:], 0x40)

	for name, buf := range map[string][]byte{
		"so_pcb mismatch":     pcbBuffer(mismatch.bytes()),
		"unexpected family":   pcbBuffer(wrongFamily.bytes()),
		"short xinpcb_n":      pcbBuffer(shortInp.bytes()),
		"duplicate kind":      dupKind,
		"bad header":          badHeader,
		"empty":               nil,
		"no trailer":          full[:len(full)-xinpgenLen],
		"partial group":       partial,
		"record overruns":     overrun,
		"unknown record kind": unknownKind,
	} {
		if _, err := parseAll(buf); !errors.Is(err, errLayout) {
			t.Errorf("%s: err = %v, want errLayout", name, err)
		}
	}
}
