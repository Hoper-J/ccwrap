package supervisor

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
)

const (
	tlsRecordHeaderLen = 5
	tlsRecordHandshake = 0x16
	maxClientHelloSize = 1 << 16 // 64 KiB safety bound
)

// peekClientHello reads whole TLS handshake records off conn until a complete
// ClientHello handshake message is assembled, returns the exact raw bytes
// consumed (record headers included -- utls.Fingerprinter wants raw records),
// and a net.Conn that replays those bytes followed by the rest of conn. The
// returned conn MUST be used in place of conn for the subsequent tls.Server
// handshake. conn's Write/Close/Deadline are preserved.
func peekClientHello(conn net.Conn) ([]byte, net.Conn, error) {
	br := bufio.NewReader(conn)
	var raw []byte
	needHandshake := 0 // total handshake-message bytes (incl 4-byte hs header); 0 until known
	have := 0          // handshake-message bytes accumulated so far
	for {
		hdr, err := br.Peek(tlsRecordHeaderLen)
		if err != nil {
			return nil, nil, err
		}
		if hdr[0] != tlsRecordHandshake {
			return nil, nil, errors.New("peekClientHello: first record is not a handshake")
		}
		recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
		rec := make([]byte, tlsRecordHeaderLen+recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return nil, nil, err
		}
		raw = append(raw, rec...)
		payload := rec[tlsRecordHeaderLen:]
		if needHandshake == 0 {
			if len(payload) < 4 {
				return nil, nil, errors.New("peekClientHello: short handshake header")
			}
			needHandshake = 4 + (int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3]))
		}
		have += len(payload)
		if have >= needHandshake {
			break
		}
		if len(raw) > maxClientHelloSize {
			return nil, nil, errors.New("peekClientHello: ClientHello too large")
		}
	}
	// Replay = the consumed hello (raw) FIRST, then br's remaining buffer + live
	// conn (br wraps conn, so reading br to EOF yields the post-hello stream).
	return raw, &replayConn{Conn: conn, r: io.MultiReader(bytes.NewReader(raw), br)}, nil
}

// captureMirroredHello peeks CC's ClientHello off conn, stores it on the session
// (once), and returns the replay conn to use in place of conn for tls.Server. On
// any peek error it stores nothing and returns the original conn — no hello is
// captured, so the fail-closed upstream dialer blocks the dial (native_tls_blocked)
// rather than falling back to a Go fingerprint. Idempotent: a no-op if already captured.
func captureMirroredHello(sess *sessionState, conn net.Conn) net.Conn {
	if sess == nil || sess.mirroredHelloRaw.Load() != nil {
		return conn
	}
	raw, replay, err := peekClientHello(conn)
	if err != nil {
		return conn
	}
	cp := append([]byte(nil), raw...)
	sess.mirroredHelloRaw.Store(&cp)
	return replay
}

// replayConn serves the peeked/buffered bytes before the live conn while
// preserving the underlying net.Conn's Write/Close/Deadline behavior.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (rc *replayConn) Read(p []byte) (int, error) { return rc.r.Read(p) }
