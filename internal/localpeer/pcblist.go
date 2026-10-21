package localpeer

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Darwin's net.inet.tcp.pcblist_n sysctl (xnu bsd/netinet/in_pcblist.c,
// get_pcblist_n) returns a struct xinpgen header, then per TCP socket six
// records — xinpcb_n, xsocket_n, xsockbuf_n (rcv, snd), xsockstat_n,
// xtcpcb_n — then a trailing xinpgen. Each per-socket record starts
// {u32 len, u32 kind} and is padded to 8 bytes. The structs come from
// bsd/netinet/in_pcb.h and bsd/sys/socketvar.h, which the public SDK does not
// ship; both are packed to 4 bytes (#pragma pack(4)), so their 64-bit fields
// sit on 4-byte boundaries. The offsets below were checked on Darwin 25
// (macOS 26) against lsof, netstat -anv (pid + gencnt per socket) and ps,
// including sockets of root and of other uids.
const (
	xinpgenLen = 24 // sizeof(struct xinpgen); also how the trailer is recognised

	xsoSocket = 0x001
	xsoRcvbuf = 0x002
	xsoSndbuf = 0x004
	xsoStats  = 0x008
	xsoInpcb  = 0x010
	xsoTcpcb  = 0x020
	xsoAllTCP = xsoSocket | xsoRcvbuf | xsoSndbuf | xsoStats | xsoInpcb | xsoTcpcb

	// struct xinpcb_n
	inpInpp   = 8  // u_int64_t xi_inpp (kernel pointer hash)
	inpFport  = 16 // u_short, network byte order
	inpLport  = 18 // u_short, network byte order
	inpVflag  = 44 // u_char: INP_IPV4 0x1, INP_IPV6 0x2
	inpFaddr  = 48 // 16-byte union: in6_addr, or in_addr_4in6 with the IPv4 address at +12
	inpLaddr  = 64
	inpMinLen = 80

	inpIPv4 = 0x1
	inpIPv6 = 0x2

	// struct xsocket_n
	soPCB      = 28 // u_int64_t so_pcb: the same hash as xi_inpp of this socket's pcb
	soProtocol = 36
	soFamily   = 40
	soUID      = 64 // uid_t so_uid: the creating credential's uid
	soMinLen   = 68

	ipprotoTCP   = 6
	afInet       = 2
	afInetDarwin = 30 // AF_INET6 on Darwin
)

// parsePCBList walks a pcblist_n buffer and calls visit for every TCP socket
// until visit returns false. Anything that does not match the expected record
// stream is reported as errLayout rather than guessed at.
func parsePCBList(buf []byte, visit func(sockKey, sockInfo) bool) error {
	le := binary.LittleEndian
	if len(buf) < xinpgenLen || le.Uint32(buf) != xinpgenLen {
		return fmt.Errorf("%w: pcblist_n header", errLayout)
	}
	var inp, so []byte
	seen := uint32(0)
	off := xinpgenLen
	for {
		if off+8 > len(buf) {
			return fmt.Errorf("%w: pcblist_n ends without a trailer", errLayout)
		}
		n := int(le.Uint32(buf[off:]))
		if n <= xinpgenLen {
			if n != xinpgenLen || seen != 0 {
				return fmt.Errorf("%w: pcblist_n trailer at %d", errLayout, off)
			}
			return nil
		}
		if off+n > len(buf) {
			return fmt.Errorf("%w: pcblist_n record overruns buffer at %d", errLayout, off)
		}
		kind := le.Uint32(buf[off+4:])
		if kind&xsoAllTCP != kind || kind&(kind-1) != 0 || kind == 0 || seen&kind != 0 {
			return fmt.Errorf("%w: pcblist_n record kind %#x at %d", errLayout, kind, off)
		}
		seen |= kind
		switch kind {
		case xsoInpcb:
			if n < inpMinLen {
				return fmt.Errorf("%w: xinpcb_n is %d bytes", errLayout, n)
			}
			inp = buf[off : off+n]
		case xsoSocket:
			if n < soMinLen {
				return fmt.Errorf("%w: xsocket_n is %d bytes", errLayout, n)
			}
			so = buf[off : off+n]
		}
		off += (n + 7) &^ 7
		if seen != xsoAllTCP {
			continue
		}
		seen = 0
		key, info, ok, err := decodePCB(inp, so)
		if err != nil {
			return err
		}
		if ok && !visit(key, info) {
			return nil
		}
	}
}

// decodePCB pairs one socket's xinpcb_n and xsocket_n. ok is false for rows
// without a usable address family (nothing to match against).
func decodePCB(inp, so []byte) (sockKey, sockInfo, bool, error) {
	le := binary.LittleEndian
	pcb := le.Uint64(so[soPCB:])
	if pcb == 0 {
		// The kernel zero-fills xsocket_n when the pcb has no socket; the
		// zero uid it leaves behind must not read as root.
		key, ok := decodeAddrs(inp)
		return key, sockInfo{orphan: true}, ok, nil
	}
	if pcb != le.Uint64(inp[inpInpp:]) {
		return sockKey{}, sockInfo{}, false, fmt.Errorf("%w: xsocket_n.so_pcb does not match xinpcb_n.xi_inpp", errLayout)
	}
	if proto, fam := le.Uint32(so[soProtocol:]), le.Uint32(so[soFamily:]); proto != ipprotoTCP || (fam != afInet && fam != afInetDarwin) {
		return sockKey{}, sockInfo{}, false, fmt.Errorf("%w: xsocket_n protocol %d family %d", errLayout, proto, fam)
	}
	key, ok := decodeAddrs(inp)
	return key, sockInfo{uid: le.Uint32(so[soUID:])}, ok, nil
}

func decodeAddrs(inp []byte) (sockKey, bool) {
	var laddr, faddr netip.Addr
	switch vflag := inp[inpVflag]; {
	case vflag&inpIPv4 != 0:
		laddr = netip.AddrFrom4([4]byte(inp[inpLaddr+12 : inpLaddr+16]))
		faddr = netip.AddrFrom4([4]byte(inp[inpFaddr+12 : inpFaddr+16]))
	case vflag&inpIPv6 != 0:
		laddr = netip.AddrFrom16([16]byte(inp[inpLaddr : inpLaddr+16]))
		faddr = netip.AddrFrom16([16]byte(inp[inpFaddr : inpFaddr+16]))
	default:
		return sockKey{}, false
	}
	lport := binary.BigEndian.Uint16(inp[inpLport:])
	fport := binary.BigEndian.Uint16(inp[inpFport:])
	return newSockKey(netip.AddrPortFrom(laddr, lport), netip.AddrPortFrom(faddr, fport)), true
}
