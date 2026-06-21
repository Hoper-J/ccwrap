package tlsfp

import (
	"os"
	"strings"
	"testing"
)

func undiciHello(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../supervisor/testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseClientHello_Undici(t *testing.T) {
	h, err := parseClientHello(undiciHello(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h.legacyVersion != 0x0303 {
		t.Errorf("legacy_version=%#x want 0x0303", h.legacyVersion)
	}
	if n := len(filterGREASE(h.cipherSuites)); n != 52 {
		t.Errorf("GREASE-excluded ciphers=%d want 52", n)
	}
	if n := len(h.extIDsGREASEFiltered()); n != 12 {
		t.Errorf("GREASE-excluded extensions=%d want 12", n)
	}
	if !h.hasSNI {
		t.Error("SNI must be present (d flag)")
	}
}

func TestJA3_Undici(t *testing.T) {
	h, err := parseClientHello(undiciHello(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := ja3(h); got != "983846581fdb62fafdb21d2282592c57" {
		t.Errorf("ja3=%s want 983846581fdb62fafdb21d2282592c57", got)
	}
}

func TestJA4_Undici(t *testing.T) {
	h, err := parseClientHello(undiciHello(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := ja4(h); got != "t13d5212h1_b262b3658495_8e6e362c5eac" {
		t.Errorf("ja4=%s want t13d5212h1_b262b3658495_8e6e362c5eac", got)
	}
}

func TestPeetprint_Undici(t *testing.T) {
	h, err := parseClientHello(undiciHello(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := peetprint(h); got != "20e60f2e3b9bcfc67c414021832c076e" {
		t.Errorf("peetprint=%s want 20e60f2e3b9bcfc67c414021832c076e", got)
	}
}

func TestCompute_Undici(t *testing.T) {
	r, err := Compute(undiciHello(t))
	if err != nil {
		t.Fatal(err)
	}
	if r.JA3 != "983846581fdb62fafdb21d2282592c57" ||
		r.JA4 != "t13d5212h1_b262b3658495_8e6e362c5eac" ||
		r.Peetprint != "20e60f2e3b9bcfc67c414021832c076e" {
		t.Errorf("Compute mismatch: %+v", r)
	}
}

func TestCompute_Malformed(t *testing.T) {
	for _, bad := range [][]byte{nil, {}, {0x16, 0x03}, {0x17, 0x03, 0x03, 0x00, 0x05}} {
		if _, err := Compute(bad); err == nil {
			t.Errorf("Compute(%v) must error, not panic", bad)
		}
	}
}

func TestIsGREASE(t *testing.T) {
	for _, v := range []uint16{0x0a0a, 0x1a1a, 0x2a2a, 0xfafa} {
		if !isGREASE(v) {
			t.Errorf("%#x must be GREASE", v)
		}
	}
	for _, v := range []uint16{0x1301, 0x0017, 0xff01, 0x0a0b} {
		if isGREASE(v) {
			t.Errorf("%#x must NOT be GREASE", v)
		}
	}
}

type rawExt struct {
	id   uint16
	data []byte
}

func be16(n int) []byte { return []byte{byte(n >> 8), byte(n)} }

func u16b(in []uint16) []byte {
	out := make([]byte, 0, len(in)*2)
	for _, v := range in {
		out = append(out, byte(v>>8), byte(v))
	}
	return out
}

// buildHello assembles minimal raw ClientHello bytes (record + handshake framing)
// with the given cipher suites and extensions (in order) — for algorithm tests.
func buildHello(ciphers []uint16, exts []rawExt) []byte {
	var body []byte
	body = append(body, 0x03, 0x03)          // legacy_version TLS1.2
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0x00)                // session_id len 0
	cs := u16b(ciphers)
	body = append(body, be16(len(cs))...)
	body = append(body, cs...)
	body = append(body, 0x01, 0x00) // compression: 1 method (null)
	var extBytes []byte
	for _, e := range exts {
		extBytes = append(extBytes, be16(int(e.id))...)
		extBytes = append(extBytes, be16(len(e.data))...)
		extBytes = append(extBytes, e.data...)
	}
	body = append(body, be16(len(extBytes))...)
	body = append(body, extBytes...)
	hs := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))} // handshake hdr
	hs = append(hs, body...)
	rec := []byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))} // record hdr
	rec = append(rec, hs...)
	return rec
}

// TestJA4_EmptyLists locks the FoxIO empty-list placeholder: no ciphers -> JA4_b
// "000000000000"; no extensions -> JA4_c "000000000000".
func TestJA4_EmptyLists(t *testing.T) {
	h, err := parseClientHello(buildHello(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(ja4(h), "_")
	if len(parts) != 3 {
		t.Fatalf("ja4 has %d _-segments, want 3", len(parts))
	}
	if parts[1] != "000000000000" {
		t.Errorf("empty ciphers: JA4_b=%q want 000000000000", parts[1])
	}
	if parts[2] != "000000000000" {
		t.Errorf("empty extensions: JA4_c=%q want 000000000000", parts[2])
	}
}

// TestJA4_PaddingIncludedInC locks that padding (0x0015) is RETAINED in JA4_c's
// sorted extension list — the FoxIO spec excludes ONLY SNI 0x0000 and ALPN 0x0010.
// The expected JA4_c is computed INDEPENDENTLY from the spec rule (sorted ext hex
// incl. padding, "_", sigalgs wire order), NOT via ja4's own exclusion logic.
func TestJA4_PaddingIncludedInC(t *testing.T) {
	sigAlgs := rawExt{0x000d, append(be16(4), u16b([]uint16{0x0403, 0x0804})...)} // 2-byte list-len + 2 algs
	padding := rawExt{0x0015, []byte{0, 0, 0, 0}}
	suppVer := rawExt{0x002b, []byte{0x02, 0x03, 0x04}} // 1-byte list-len=2 + 0x0304
	h, err := parseClientHello(buildHello([]uint16{0x1301}, []rawExt{sigAlgs, padding, suppVer}))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(ja4(h), "_")
	wantC := sha256Hex12("000d,0015,002b_0403,0804")
	if parts[2] != wantC {
		t.Errorf("JA4_c=%q want %q — padding 0x0015 must be INCLUDED (FoxIO excludes only SNI/ALPN)", parts[2], wantC)
	}
}

// TestSigAlgsCert_FoldsIntoPeetprintNotJA4 locks: the 2nd sig-algs extension folds
// into peetprint's sig_algs (TrackMe) but NOT into JA4_c (FoxIO uses 0x000d only).
// It uses ext 0x0035 deliberately — that is the ID TrackMe byte-matches (NOT IANA's
// 0x0032); "correcting" the parser to 0x0032 leaves this hello's 0x0035 unparsed, so
// peetprint(hA)==peetprint(hB) and the first assertion fires. Two hellos with
// IDENTICAL extension IDs but different 0x0035 CONTENT isolate the effect: peetprint
// must differ; JA4 must be identical.
func TestSigAlgsCert_FoldsIntoPeetprintNotJA4(t *testing.T) {
	mk := func(certAlg uint16) clientHelloFields {
		d := rawExt{0x000d, append(be16(2), u16b([]uint16{0x0403})...)}  // signature_algorithms = [0x0403]
		c := rawExt{0x0035, append(be16(2), u16b([]uint16{certAlg})...)} // signature_algorithms_cert
		h, err := parseClientHello(buildHello([]uint16{0x1301}, []rawExt{d, c}))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	hA, hB := mk(0x0804), mk(0x0805) // same ext IDs, different 0x0035 content
	if peetprint(hA) == peetprint(hB) {
		t.Error("signature_algorithms_cert (0x0035) must fold into peetprint sig_algs (TrackMe) — peetprint should differ")
	}
	if ja4(hA) != ja4(hB) {
		t.Errorf("JA4 must NOT use signature_algorithms_cert (FoxIO uses 0x000d only): %q vs %q", ja4(hA), ja4(hB))
	}
}
