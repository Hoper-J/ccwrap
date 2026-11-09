package localpeer

import (
	"net/netip"
	"testing"

	"golang.org/x/sys/windows"
)

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// putPort stores port the way the table does: network order in the low two
// bytes of a DWORD.
func putPort(b []byte, port uint16) { b[0], b[1], b[2], b[3] = byte(port>>8), byte(port), 0, 0 }

type winRow struct {
	key        sockKey
	state, pid uint32
}

func parseWinTable(t *testing.T, table []byte, family uint32) []winRow {
	t.Helper()
	var got []winRow
	if err := parseTCPTable(table, family, func(k sockKey, state, pid uint32) bool {
		got = append(got, winRow{k, state, pid})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestParseTCPTableIPv4(t *testing.T) {
	table := make([]byte, 4+2*tcpRowSize)
	putLE32(table, 2)
	r := table[4:]
	putLE32(r[0:], 5) // ESTABLISHED
	copy(r[4:8], []byte{127, 0, 0, 1})
	putPort(r[8:], 50123)
	copy(r[12:16], []byte{127, 0, 0, 1})
	putPort(r[16:], 8080)
	putLE32(r[20:], 4321)
	r = r[tcpRowSize:]
	putLE32(r[0:], tcpStateTimeWait)
	copy(r[4:8], []byte{127, 0, 0, 1})
	putPort(r[8:], 8080)
	copy(r[12:16], []byte{127, 0, 0, 1})
	putPort(r[16:], 50000)

	got := parseWinTable(t, table, windows.AF_INET)
	want := []winRow{
		{newSockKey(netip.MustParseAddrPort("127.0.0.1:50123"), netip.MustParseAddrPort("127.0.0.1:8080")), 5, 4321},
		{newSockKey(netip.MustParseAddrPort("127.0.0.1:8080"), netip.MustParseAddrPort("127.0.0.1:50000")), tcpStateTimeWait, 0},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
}

// A dual-stack socket's v4-mapped addresses compare equal to IPv4 ones.
func TestParseTCPTableIPv6Unmaps(t *testing.T) {
	table := make([]byte, 4+tcp6RowSize)
	putLE32(table, 1)
	r := table[4:]
	copy(r[0:16], netip.MustParseAddr("::ffff:127.0.0.1").AsSlice())
	putPort(r[20:], 50123)
	copy(r[24:40], netip.MustParseAddr("::ffff:127.0.0.1").AsSlice())
	putPort(r[44:], 8080)
	putLE32(r[48:], 5)
	putLE32(r[52:], 99)

	got := parseWinTable(t, table, windows.AF_INET6)
	want := newSockKey(netip.MustParseAddrPort("127.0.0.1:50123"), netip.MustParseAddrPort("127.0.0.1:8080"))
	if len(got) != 1 || got[0].key != want || got[0].pid != 99 {
		t.Fatalf("rows = %+v, want one row %v pid 99", got, want)
	}
}

func TestParseTCPTableRejectsOverlongCount(t *testing.T) {
	table := make([]byte, 4+tcpRowSize)
	putLE32(table, 2)
	if err := parseTCPTable(table, windows.AF_INET, func(sockKey, uint32, uint32) bool { return true }); err == nil {
		t.Fatal("a row count past the table's end must be refused")
	}
}

func TestProcessOwnerOfThisProcessIsSelf(t *testing.T) {
	if got := processOwner(uint32(windows.GetCurrentProcessId())); got.orphan || got.uid == otherUser || got.uid == 0 {
		t.Fatalf("processOwner(self) = %+v, want this process's own uid", got)
	}
	if got := processOwner(systemPID); got.orphan || got.uid != 0 {
		t.Fatalf("processOwner(System) = %+v, want root", got)
	}
}
