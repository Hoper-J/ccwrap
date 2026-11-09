package localpeer

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows lists a TCP socket with the id of the process holding it, not a
// user, so a row's owner is that process's user. Two answers matter: this
// process's user, reported as its euid like on Unix, and SYSTEM, reported as
// root (0); anyone else is otherUser. A process the current user cannot
// open is not theirs: they can open all of their own.

// RootName is what messages call the superuser, whose connections pass.
const RootName = "SYSTEM"

const (
	otherUser           = ^uint32(0) - 1 // never this process's "euid", ^uint32(0)
	tcpTableOwnerPIDAll = 5              // TCP_TABLE_OWNER_PID_ALL
	tcpStateTimeWait    = 11             // MIB_TCP_STATE_TIME_WAIT
	systemPID           = 4              // the System process
	tcpRowSize          = 24             // MIB_TCPROW_OWNER_PID
	tcp6RowSize         = 56             // MIB_TCP6ROW_OWNER_PID
)

var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

var selfSID = sync.OnceValues(func() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
})

// ownerName names uid for a listener running as self: on Windows a peer is
// only ever this user, SYSTEM or someone else.
func ownerName(uid, self uint32) string {
	switch uid {
	case self:
		if sid, err := selfSID(); err == nil {
			if account, domain, _, err := sid.LookupAccount(""); err == nil {
				return domain + `\` + account
			}
		}
		return "this user"
	case 0:
		return RootName
	}
	return "another user"
}

// platformLookup reads the IPv4 TCP table, then the IPv6 one (dual-stack
// sockets connected to an IPv4 address are listed there, v4-mapped), and
// stops once every key has a live row.
func platformLookup(keys ...sockKey) (map[sockKey]sockInfo, error) {
	rows := newRowSet(keys)
	owners := map[uint32]sockInfo{}
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		table, err := tcpTable(family)
		if err != nil {
			// No IPv6 table just means IPv6 is off.
			if family == windows.AF_INET6 {
				break
			}
			return nil, err
		}
		err = parseTCPTable(table, family, func(k sockKey, state, pid uint32) bool {
			if !rows.want[k] {
				return true
			}
			info, ok := owners[pid]
			if !ok {
				info = processOwner(pid)
				owners[pid] = info
			}
			if state == tcpStateTimeWait {
				info = sockInfo{orphan: true}
			}
			return rows.add(k, info)
		})
		if err != nil {
			return nil, err
		}
		if rows.complete() {
			break
		}
	}
	return rows.found, nil
}

// tcpTable is the system's TCP table for family with owning process ids:
// MIB_TCPTABLE_OWNER_PID or MIB_TCP6TABLE_OWNER_PID.
func tcpTable(family uint32) ([]byte, error) {
	size := uint32(16 << 10)
	for range 5 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), tcpTableOwnerPIDAll, 0)
		switch err := windows.Errno(r); err {
		case 0:
			return buf, nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10 // it can grow again before the next call
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", err)
		}
	}
	return nil, errors.New("GetExtendedTcpTable: the TCP table kept growing")
}

// parseTCPTable calls row for each socket in table until it returns false.
// DWORD fields are little-endian; addresses and the low two bytes of the
// port fields are in network order.
func parseTCPTable(table []byte, family uint32, row func(k sockKey, state, pid uint32) bool) error {
	if len(table) < 4 {
		return errLayout
	}
	n := int(le32(table))
	size, start := tcpRowSize, 4
	if family == windows.AF_INET6 {
		size = tcp6RowSize
	}
	if n < 0 || n > (len(table)-start)/size {
		return fmt.Errorf("%w: %d rows do not fit a %d-byte table", errLayout, n, len(table))
	}
	for i := 0; i < n; i++ {
		r := table[start+i*size : start+(i+1)*size]
		var local, remote netip.AddrPort
		var state, pid uint32
		if family == windows.AF_INET6 {
			local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])), port(r[20:24]))
			remote = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[24:40])), port(r[44:48]))
			state, pid = le32(r[48:52]), le32(r[52:56])
		} else {
			state = le32(r[0:4])
			local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[4:8])), port(r[8:12]))
			remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[12:16])), port(r[16:20]))
			pid = le32(r[20:24])
		}
		if !row(newSockKey(local, remote), state, pid) {
			return nil
		}
	}
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func port(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// processOwner maps the user of process pid to a uid as described above.
func processOwner(pid uint32) sockInfo {
	switch pid {
	case 0:
		return sockInfo{orphan: true}
	case systemPID:
		return sockInfo{uid: 0}
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return sockInfo{orphan: true} // exited since the table was read
	}
	if err != nil {
		return sockInfo{uid: otherUser}
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return sockInfo{uid: otherUser}
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return sockInfo{uid: otherUser}
	}
	me, err := selfSID()
	switch {
	case err == nil && u.User.Sid.Equals(me):
		return sockInfo{uid: uint32(os.Geteuid())}
	case u.User.Sid.IsWellKnown(windows.WinLocalSystemSid):
		return sockInfo{uid: 0}
	}
	return sockInfo{uid: otherUser}
}
