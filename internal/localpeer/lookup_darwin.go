//go:build darwin

package localpeer

import (
	"errors"

	"golang.org/x/sys/unix"
)

// platformLookup snapshots the TCP pcb list (see pcblist.go). Readable
// without privileges, and it lists every user's sockets.
func platformLookup(keys ...sockKey) (map[sockKey]sockInfo, error) {
	var buf []byte
	var err error
	// The size probe and the read are separate calls; ENOMEM means the table
	// outgrew the kernel's slack in between, so ask again a couple of times.
	for attempt := 0; attempt < 3; attempt++ {
		buf, err = unix.SysctlRaw("net.inet.tcp.pcblist_n")
		if !errors.Is(err, unix.ENOMEM) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	rows := newRowSet(keys)
	if err := parsePCBList(buf, rows.add); err != nil {
		return nil, err
	}
	return rows.found, nil
}
