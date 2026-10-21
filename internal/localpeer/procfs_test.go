package localpeer

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// Rows in the exact kernel format (seq_printf in tcp4_seq_show /
// tcp6_seq_show, little-endian host). 0x0CEA = 3306, 0xD3A2 = 54178.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 33725 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0CEA 0100007F:D3A2 01 00000000:00000000 00:00000000 00000000  1000        0 33726 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:D3A2 0100007F:0CEA 01 00000000:00000000 00:00000000 00000000  1001        0 33727 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:D3A4 0100007F:0CEA 06 00000000:00000000 03:00001770 00000000     0        0 0 3 0000000000000000
   4: 0F02000A:0016 0101A8C0:E1C2 01 00000000:00000000 02:000A2D4B 00000000     0        0 4242 2 0000000000000000 20 4 31 10 -1
`

const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0CEB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 40001 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:D3A6 0000000000000000FFFF00000100007F:0CEA 01 00000000:00000000 00:00000000 00000000  1002        0 40002 1 0000000000000000 20 4 30 10 -1
   2: 00000000000000000000000001000000:0CEB 00000000000000000000000001000000:D3A8 01 00000000:00000000 00:00000000 00000000  1000        0 40003 1 0000000000000000 20 4 30 10 -1
   3: B80D0120000000000000000001000000:0050 B80D0120000000000000000002000000:C000 01 00000000:00000000 00:00000000 00000000    33        0 40004 1 0000000000000000 20 4 30 10 -1
`

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func collectProc(t *testing.T, text string) map[sockKey]sockInfo {
	t.Helper()
	got := map[sockKey]sockInfo{}
	err := parseProcNetTCP(strings.NewReader(text), binary.LittleEndian, func(k sockKey, info sockInfo) bool {
		got[k] = info
		return true
	})
	if err != nil {
		t.Fatalf("parseProcNetTCP: %v", err)
	}
	return got
}

func TestParseProcNetTCPIPv4(t *testing.T) {
	got := collectProc(t, procNetTCP)
	want := map[sockKey]sockInfo{
		newSockKey(ap("127.0.0.1:3306"), ap("0.0.0.0:0")):       {uid: 1000},
		newSockKey(ap("127.0.0.1:3306"), ap("127.0.0.1:54178")): {uid: 1000},
		newSockKey(ap("127.0.0.1:54178"), ap("127.0.0.1:3306")): {uid: 1001},
		newSockKey(ap("127.0.0.1:54180"), ap("127.0.0.1:3306")): {uid: 0, orphan: true}, // TIME_WAIT: uid 0, inode 0
		newSockKey(ap("10.0.2.15:22"), ap("192.168.1.1:57794")): {uid: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("row %v = %+v (present %v), want %+v", k, g, ok, w)
		}
	}
}

func TestParseProcNetTCP6(t *testing.T) {
	got := collectProc(t, procNetTCP6)
	want := map[sockKey]sockInfo{
		newSockKey(ap("[::]:3307"), ap("[::]:0")): {uid: 1000},
		// A dual-stack socket connected to 127.0.0.1 is listed v4-mapped; the
		// key is unmapped so it matches the IPv4 side of the connection.
		newSockKey(ap("127.0.0.1:54182"), ap("127.0.0.1:3306")):       {uid: 1002},
		newSockKey(ap("[::1]:3307"), ap("[::1]:54184")):               {uid: 1000},
		newSockKey(ap("[2001:db8::1]:80"), ap("[2001:db8::2]:49152")): {uid: 33},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("row %v = %+v (present %v), want %+v", k, g, ok, w)
		}
	}
}

func TestParseProcNetTCPStopsWhenVisitSaysSo(t *testing.T) {
	calls := 0
	err := parseProcNetTCP(strings.NewReader(procNetTCP), binary.LittleEndian, func(sockKey, sockInfo) bool {
		calls++
		return calls < 2
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v, want 2 calls and no error", calls, err)
	}
}

func TestParseProcNetTCPHeaderOnly(t *testing.T) {
	if got := collectProc(t, strings.SplitAfter(procNetTCP, "\n")[0]); len(got) != 0 {
		t.Fatalf("header-only input produced rows: %v", got)
	}
}

func TestParseProcNetTCPRejectsMalformedRows(t *testing.T) {
	for name, row := range map[string]string{
		"short row":         "   0: 0100007F:0CEA 00000000:0000 0A\n",
		"odd address":       "   0: 0100007:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 1 1\n",
		"bad port":          "   0: 0100007F:CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 1 1\n",
		"no colon":          "   0: 0100007F0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 1 1\n",
		"non-hex":           "   0: 0100007G:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 1 1\n",
		"non-numeric uid":   "   0: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  abc 0 1 1\n",
		"non-numeric inode": "   0: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 x 1\n",
	} {
		err := parseProcNetTCP(strings.NewReader(row), binary.LittleEndian, func(sockKey, sockInfo) bool { return true })
		if !errors.Is(err, errLayout) {
			t.Errorf("%s: err = %v, want errLayout", name, err)
		}
	}
}

func TestParseProcAddrHonoursHostByteOrder(t *testing.T) {
	// A big-endian host prints the same 127.0.0.1 as 7F000001.
	got, err := parseProcAddr("7F000001:0CEA", binary.BigEndian)
	if err != nil || got != ap("127.0.0.1:3306") {
		t.Fatalf("parseProcAddr big-endian = %v, %v", got, err)
	}
}
