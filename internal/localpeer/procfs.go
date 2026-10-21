package localpeer

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// parseProcNetTCP reads Linux /proc/net/tcp or /proc/net/tcp6 text and calls
// visit for every socket row until visit returns false.
//
// Row shape (net/ipv4/tcp_ipv4.c, net/ipv6/tcp_ipv6.c):
//
//	sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode ...
//	0: 0100007F:1F90 0100007F:D3A2 01 00000000:00000000 00:00000000 00000000 1000 0 123456 ...
//
// Addresses are the in-memory __be32 words printed with %08X, so each word's
// bytes come back through the host byte order (order: little-endian on
// amd64/arm64); IPv6 is four such words. Ports are plain big-endian hex.
// Sockets no process holds any more print inode 0, and once the kernel turns
// them into TIME_WAIT-style entries (TIME_WAIT, or FIN_WAIT2 after the owner
// closed) they also print uid 0, so inode-0 rows are reported as orphans.
func parseProcNetTCP(r io.Reader, order binary.ByteOrder, visit func(sockKey, sockInfo) bool) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] == "sl" {
			continue
		}
		if len(fields) < 10 {
			return fmt.Errorf("%w: short /proc/net/tcp row %q", errLayout, sc.Text())
		}
		local, err := parseProcAddr(fields[1], order)
		if err != nil {
			return err
		}
		remote, err := parseProcAddr(fields[2], order)
		if err != nil {
			return err
		}
		uid, err := strconv.ParseUint(fields[7], 10, 32)
		if err != nil {
			return fmt.Errorf("%w: uid column %q", errLayout, fields[7])
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			return fmt.Errorf("%w: inode column %q", errLayout, fields[9])
		}
		if !visit(newSockKey(local, remote), sockInfo{uid: uint32(uid), orphan: inode == 0}) {
			return nil
		}
	}
	return sc.Err()
}

func parseProcAddr(s string, order binary.ByteOrder) (netip.AddrPort, error) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	if !ok || len(hexPort) != 4 {
		return netip.AddrPort{}, fmt.Errorf("%w: address %q", errLayout, s)
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%w: port %q", errLayout, s)
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, fmt.Errorf("%w: address %q", errLayout, s)
	}
	// hex.DecodeString read each word most-significant byte first; rewrite it
	// in host order to get the original network-order bytes back.
	var b [16]byte
	for i := 0; i < len(raw); i += 4 {
		order.PutUint32(b[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	var addr netip.Addr
	if len(raw) == 4 {
		addr = netip.AddrFrom4([4]byte(b[:4]))
	} else {
		addr = netip.AddrFrom16(b)
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}
