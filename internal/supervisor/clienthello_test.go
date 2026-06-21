package supervisor

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// buildClientHelloRecords frames a handshake-message body into TLS records of
// at most maxPayload bytes each (small maxPayload forces fragmentation).
func buildClientHelloRecords(hsBody []byte, maxPayload int) []byte {
	hs := append([]byte{0x01, byte(len(hsBody) >> 16), byte(len(hsBody) >> 8), byte(len(hsBody))}, hsBody...)
	var out []byte
	for len(hs) > 0 {
		n := len(hs)
		if n > maxPayload {
			n = maxPayload
		}
		var hdr [5]byte
		hdr[0] = 0x16
		hdr[1], hdr[2] = 0x03, 0x01
		binary.BigEndian.PutUint16(hdr[3:5], uint16(n))
		out = append(out, hdr[:]...)
		out = append(out, hs[:n]...)
		hs = hs[n:]
	}
	return out
}

func TestPeekClientHelloAssemblesAcrossRecords(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, 700) // > maxPayload below -> multi-record
	full := buildClientHelloRecords(body, 256)
	trailer := []byte("AFTER-HELLO-BYTES")

	c1, c2 := net.Pipe()
	go func() {
		_, _ = c2.Write(full)
		_, _ = c2.Write(trailer)
		_ = c2.Close()
	}()
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))

	raw, replay, err := peekClientHello(c1)
	if err != nil {
		t.Fatalf("peekClientHello: %v", err)
	}
	if !bytes.Equal(raw, full) {
		t.Fatalf("peeked %d bytes, want the full %d-byte hello (all records)", len(raw), len(full))
	}
	got, _ := io.ReadAll(replay)
	if !bytes.Equal(got, append(append([]byte{}, full...), trailer...)) {
		t.Fatalf("replay conn did not reproduce hello+trailer (got %d bytes)", len(got))
	}
}

func TestCaptureMirroredHelloStoresParseableSpec(t *testing.T) {
	// Build a realistic Chrome ClientHello via utls. The stub conn is never
	// written to / read from -- BuildHandshakeState only marshals the message.
	stub, _ := net.Pipe()
	defer stub.Close()
	uc := utls.UClient(stub, &utls.Config{ServerName: "api.anthropic.com"}, utls.HelloChrome_Auto)
	if err := uc.BuildHandshakeState(); err != nil {
		t.Fatalf("BuildHandshakeState: %v", err)
	}
	helloBody := uc.HandshakeState.Hello.Raw // marshaled ClientHello handshake message (incl 4-byte hs header)
	if len(helloBody) < 4 {
		t.Fatalf("utls produced an implausibly short ClientHello (%d bytes)", len(helloBody))
	}
	// buildClientHelloRecords prepends its own 4-byte handshake header, so feed
	// it the body AFTER that header (it re-frames identically).
	framed := buildClientHelloRecords(helloBody[4:], 4096)

	c1, c2 := net.Pipe()
	go func() {
		_, _ = c2.Write(framed)
		_ = c2.Close()
	}()
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))

	sess := &sessionState{}
	replay := captureMirroredHello(sess, c1)

	stored := sess.mirroredHelloRaw.Load()
	if stored == nil {
		t.Fatalf("captureMirroredHello stored nothing")
	}
	if !bytes.Equal(*stored, framed) {
		t.Fatalf("stored %d bytes, want the framed %d-byte record", len(*stored), len(framed))
	}
	if replay == nil {
		t.Fatalf("captureMirroredHello returned a nil replay conn")
	}
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(*stored)
	if err != nil {
		t.Fatalf("FingerprintClientHello on stored bytes: %v", err)
	}
	if len(spec.CipherSuites) < 1 {
		t.Fatalf("fingerprinted spec has no cipher suites; stored bytes are not a real ClientHello")
	}
}
