//go:build linux

package localpeer

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
)

// platformLookup scans /proc/net/tcp, then /proc/net/tcp6 (dual-stack
// sockets connected to an IPv4 address are listed there, v4-mapped), and
// stops once every key has a live row.
func platformLookup(keys ...sockKey) (map[sockKey]sockInfo, error) {
	rows := newRowSet(keys)
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			// No tcp6 just means IPv6 is off; no tcp means no usable procfs.
			if path == "/proc/net/tcp6" && errors.Is(err, fs.ErrNotExist) {
				break
			}
			return nil, err
		}
		err = parseProcNetTCP(f, binary.NativeEndian, rows.add)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if rows.complete() {
			break
		}
	}
	return rows.found, nil
}
