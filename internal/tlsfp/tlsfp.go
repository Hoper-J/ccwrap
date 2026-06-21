package tlsfp

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Result is the computed fingerprint set for a captured ClientHello.
type Result struct {
	JA3       string `json:"ja3"`
	JA4       string `json:"ja4"`
	Peetprint string `json:"peetprint"`
}

// Compute parses raw ClientHello bytes and returns the three fingerprints. It
// returns an error (never panics) on malformed/multi-record input.
func Compute(raw []byte) (Result, error) {
	h, err := parseClientHello(raw)
	if err != nil {
		return Result{}, err
	}
	return Result{JA3: ja3(h), JA4: ja4(h), Peetprint: peetprint(h)}, nil
}

// clientHelloFields holds exactly what the three fingerprints need, in wire order.
type clientHelloFields struct {
	legacyVersion   uint16
	cipherSuites    []uint16
	extIDs          []uint16 // wire order (GREASE included; filter at use)
	hasSNI          bool
	supportedGroups []uint16
	ecPointFormats  []uint8
	sigAlgs         []uint16
	sigAlgsCert     []uint16 // TrackMe's 2nd sig-algs ext (0x0035): folds into peetprint sig_algs, NOT JA4_c (see parseClientHello)
	supportedVers   []uint16
	alpn            []string
	pskModes        []uint8
	certCompression []uint16 // ext 27 (compress_certificate); undici sends none
}

func isGREASE(v uint16) bool { return (v>>8) == (v&0xff) && (v&0x0f) == 0x0a }

func filterGREASE(in []uint16) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		if !isGREASE(v) {
			out = append(out, v)
		}
	}
	return out
}

func (h clientHelloFields) extIDsGREASEFiltered() []uint16 { return filterGREASE(h.extIDs) }

func joinDecU16(in []uint16) string {
	s := make([]string, len(in))
	for i, v := range in {
		s[i] = strconv.Itoa(int(v))
	}
	return strings.Join(s, "-")
}

func joinDecU8(in []uint8) string {
	s := make([]string, len(in))
	for i, v := range in {
		s[i] = strconv.Itoa(int(v))
	}
	return strings.Join(s, "-")
}

// ja3 = MD5( version,ciphers,extensions,curves,ec_point_formats ); decimal IDs,
// GREASE excluded, wire order, SNI value not included.
func ja3(h clientHelloFields) string {
	parts := []string{
		strconv.Itoa(int(h.legacyVersion)),
		joinDecU16(filterGREASE(h.cipherSuites)),
		joinDecU16(filterGREASE(h.extIDs)),
		joinDecU16(filterGREASE(h.supportedGroups)),
		joinDecU8(h.ecPointFormats),
	}
	sum := md5.Sum([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])
}

// sha256Hex12 returns the first 12 hex chars of SHA256(s).
func sha256Hex12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// hexCSV renders each value as 4-hex-lowercase, comma-joined.
func hexCSV(in []uint16) string {
	s := make([]string, len(in))
	for i, v := range in {
		s[i] = fmt.Sprintf("%04x", v)
	}
	return strings.Join(s, ",")
}

// sortU16 returns an ascending-sorted copy of in.
func sortU16(in []uint16) []uint16 {
	out := make([]uint16, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// excludeU16 returns a copy of in with any value in drop removed (order preserved).
func excludeU16(in []uint16, drop ...uint16) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		skip := false
		for _, d := range drop {
			if v == d {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, v)
		}
	}
	return out
}

// ja4Version maps a TLS version word to the JA4 2-char code.
func ja4Version(v uint16) string {
	switch v {
	case 0x0304:
		return "13"
	case 0x0303:
		return "12"
	case 0x0302:
		return "11"
	case 0x0301:
		return "10"
	default:
		return "00"
	}
}

// ja4 = JA4_a _ JA4_b _ JA4_c (FoxIO JA4 spec).
func ja4(h clientHelloFields) string {
	ciphers := filterGREASE(h.cipherSuites)
	exts := filterGREASE(h.extIDs)

	// JA4_a.
	proto := "t" // TCP (QUIC would be "q"; not distinguishable here).
	// Version from supported_versions max (GREASE excluded), fallback legacyVersion.
	verWord := h.legacyVersion
	maxVer := uint16(0)
	for _, v := range filterGREASE(h.supportedVers) {
		if v > maxVer {
			maxVer = v
		}
	}
	if maxVer != 0 {
		verWord = maxVer
	}
	ver := ja4Version(verWord)
	sni := "i"
	if h.hasSNI {
		sni = "d"
	}
	cc := len(ciphers)
	if cc > 99 {
		cc = 99
	}
	ec := len(exts)
	if ec > 99 {
		ec = 99
	}
	alpn2 := "00"
	if len(h.alpn) > 0 && len(h.alpn[0]) > 0 {
		p := h.alpn[0]
		alpn2 = string(p[0]) + string(p[len(p)-1])
	}
	ja4a := fmt.Sprintf("%s%s%s%02d%02d%s", proto, ver, sni, cc, ec, alpn2)

	// JA4_b: sorted GREASE-excluded ciphers; empty -> 000000000000 (FoxIO).
	ja4b := "000000000000"
	if len(ciphers) > 0 {
		ja4b = sha256Hex12(hexCSV(sortU16(ciphers)))
	}

	// JA4_c: sorted GREASE-excluded extensions, dropping ONLY SNI 0x0000 and ALPN
	// 0x0010 (padding 0x0015 is RETAINED — the FoxIO spec excludes only SNI+ALPN)
	// _ signature_algorithms in WIRE ORDER. Empty sorted-ext list -> 000000000000.
	extsForC := sortU16(excludeU16(exts, 0x0000, 0x0010))
	ja4c := "000000000000"
	if len(extsForC) > 0 {
		ja4c = sha256Hex12(hexCSV(extsForC) + "_" + hexCSV(h.sigAlgs))
	}

	return ja4a + "_" + ja4b + "_" + ja4c
}

// peetprint = MD5 of
//
//	tls_versions|protos|groups|sig_algs|psk_mode|comp_algs|cipher_suites|extensions
//
// (tls.peet.ws / pagpeter TrackMe CalculatePeetPrint). Segments joined by "|",
// inner values joined by "-". GREASE is rendered as the literal "GREASE" (NOT
// dropped) in tls_versions, groups, cipher_suites and extensions. ALPN names are
// mapped h2->2, http/1.1->1.1, http/1.0->1.0 (others dropped). groups exclude the
// magic value 6969. Only EXTENSIONS are sorted, and lexicographically as strings
// (sort.Strings) AFTER GREASE substitution. psk_mode is the FIRST psk_key_exchange
// mode byte. comp_algs = certCompression (compress_certificate, ext 27). sig_algs
// folds 0x000d then 0x0035 (TrackMe matches both into its sig_algs segment; see
// parseClientHello for the 0x0035-not-0x0032 parity note) — unlike JA4_c, which
// uses 0x000d only (FoxIO). The 0x000d-then-0x0035 order matches TrackMe for the
// normal ascending extension order; a hello sending 0x0035 before 0x000d (not seen
// in practice) would differ.
func peetprint(h clientHelloFields) string {
	tlsVersions := peetU16(h.supportedVers, false)
	groups := peetU16(h.supportedGroups, true)
	suites := peetU16(h.cipherSuites, false)

	exts := peetU16(h.extIDs, false)
	sort.Strings(exts)

	pskMode := ""
	if len(h.pskModes) > 0 {
		pskMode = strconv.Itoa(int(h.pskModes[0]))
	}

	seg := []string{
		strings.Join(tlsVersions, "-"),
		strings.Join(peetALPN(h.alpn), "-"),
		strings.Join(groups, "-"),
		joinDecU16(append(append([]uint16(nil), h.sigAlgs...), h.sigAlgsCert...)),
		pskMode,
		joinDecU16(h.certCompression),
		strings.Join(suites, "-"),
		strings.Join(exts, "-"),
	}
	sum := md5.Sum([]byte(strings.Join(seg, "|")))
	return hex.EncodeToString(sum[:])
}

// peetU16 renders each value as decimal, substituting the literal "GREASE" for
// GREASE values (TrackMe keeps GREASE as a string rather than dropping it). When
// dropMagic is set, the curve magic value 6969 is treated like GREASE.
func peetU16(in []uint16, dropMagic bool) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if isGREASE(v) || (dropMagic && v == 6969) {
			out = append(out, "GREASE")
		} else {
			out = append(out, strconv.Itoa(int(v)))
		}
	}
	return out
}

// peetALPN maps ALPN protocol names per TrackMe: h2->2, http/1.1->1.1,
// http/1.0->1.0. Any other protocol name is dropped.
func peetALPN(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		switch strings.ToLower(p) {
		case "h2":
			out = append(out, "2")
		case "http/1.1":
			out = append(out, "1.1")
		case "http/1.0":
			out = append(out, "1.0")
		}
	}
	return out
}

type br struct {
	b []byte
	i int
}

func (r *br) u8() (uint8, bool) {
	if r.i+1 > len(r.b) {
		return 0, false
	}
	v := r.b[r.i]
	r.i++
	return v, true
}
func (r *br) u16() (uint16, bool) {
	if r.i+2 > len(r.b) {
		return 0, false
	}
	v := uint16(r.b[r.i])<<8 | uint16(r.b[r.i+1])
	r.i += 2
	return v, true
}
func (r *br) u24() (int, bool) {
	if r.i+3 > len(r.b) {
		return 0, false
	}
	v := int(r.b[r.i])<<16 | int(r.b[r.i+1])<<8 | int(r.b[r.i+2])
	r.i += 3
	return v, true
}
func (r *br) take(n int) ([]byte, bool) {
	if n < 0 || r.i+n > len(r.b) {
		return nil, false
	}
	v := r.b[r.i : r.i+n]
	r.i += n
	return v, true
}

func u16list(b []byte) []uint16 {
	out := make([]uint16, 0, len(b)/2)
	for i := 0; i+2 <= len(b); i += 2 {
		out = append(out, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return out
}

// parseClientHello consumes stored bytes (record header + handshake header +
// ClientHello body) and returns the fields. NEVER panics: every read is
// bounds-checked. Malformed/multi-record -> error (graceful "unparseable").
func parseClientHello(raw []byte) (clientHelloFields, error) {
	var f clientHelloFields
	r := &br{b: raw}
	rtype, ok := r.u8()
	if !ok || rtype != 0x16 {
		return f, errors.New("tlsfp: not a TLS handshake record")
	}
	if _, ok = r.u16(); !ok {
		return f, errors.New("tlsfp: short record version")
	}
	if _, ok = r.u16(); !ok {
		return f, errors.New("tlsfp: short record length")
	}
	htype, ok := r.u8()
	if !ok || htype != 0x01 {
		return f, errors.New("tlsfp: not a ClientHello")
	}
	if _, ok = r.u24(); !ok {
		return f, errors.New("tlsfp: short handshake length")
	}
	if f.legacyVersion, ok = r.u16(); !ok {
		return f, errors.New("tlsfp: short client_version")
	}
	if _, ok = r.take(32); !ok {
		return f, errors.New("tlsfp: short random")
	}
	sidLen, ok := r.u8()
	if !ok {
		return f, errors.New("tlsfp: short session_id length")
	}
	if _, ok = r.take(int(sidLen)); !ok {
		return f, errors.New("tlsfp: short session_id")
	}
	csLen, ok := r.u16()
	if !ok {
		return f, errors.New("tlsfp: short cipher_suites length")
	}
	cs, ok := r.take(int(csLen))
	if !ok {
		return f, errors.New("tlsfp: short cipher_suites")
	}
	f.cipherSuites = u16list(cs)
	cmLen, ok := r.u8()
	if !ok {
		return f, errors.New("tlsfp: short compression length")
	}
	if _, ok = r.take(int(cmLen)); !ok {
		return f, errors.New("tlsfp: short compression")
	}
	extLen, ok := r.u16()
	if !ok {
		return f, errors.New("tlsfp: no extensions")
	}
	extBytes, ok := r.take(int(extLen))
	if !ok {
		return f, errors.New("tlsfp: short extensions")
	}
	er := &br{b: extBytes}
	for er.i < len(er.b) {
		id, ok := er.u16()
		if !ok {
			return f, errors.New("tlsfp: short extension id")
		}
		ln, ok := er.u16()
		if !ok {
			return f, errors.New("tlsfp: short extension length")
		}
		data, ok := er.take(int(ln))
		if !ok {
			return f, errors.New("tlsfp: short extension data")
		}
		f.extIDs = append(f.extIDs, id)
		switch id {
		case 0x0000:
			f.hasSNI = true
		case 0x000a:
			if len(data) >= 2 {
				f.supportedGroups = u16list(data[2:])
			}
		case 0x000b:
			if len(data) >= 1 {
				f.ecPointFormats = data[1:]
			}
		case 0x000d:
			if len(data) >= 2 {
				f.sigAlgs = u16list(data[2:])
			}
		// TrackMe parity, NOT IANA: tls.peet.ws folds a SECOND sig-algs extension
		// into its peetprint sig_algs segment. Its parser matches ext 0x0035 and
		// labels it "signature_algorithms_cert (50)" — but IANA's real
		// signature_algorithms_cert is 0x0032, so that label is a TrackMe bug. We
		// match 0x0035 (its actual byte-level behavior) to reproduce peetprint
		// byte-for-byte. Do NOT "correct" this to 0x0032 — it would break parity.
		// (Same wire format as 0x000d: 2-byte list length + u16 algs.)
		case 0x0035:
			if len(data) >= 2 {
				f.sigAlgsCert = u16list(data[2:])
			}
		case 0x0010:
			f.alpn = parseALPN(data)
		case 0x001b:
			if len(data) >= 1 {
				f.certCompression = u16list(data[1:])
			}
		case 0x002b:
			if len(data) >= 1 {
				f.supportedVers = u16list(data[1:])
			}
		case 0x002d:
			if len(data) >= 1 {
				f.pskModes = data[1:]
			}
		}
	}
	return f, nil
}

func parseALPN(data []byte) []string {
	if len(data) < 2 {
		return nil
	}
	r := &br{b: data}
	_, _ = r.u16()
	var out []string
	for r.i < len(r.b) {
		n, ok := r.u8()
		if !ok {
			break
		}
		name, ok := r.take(int(n))
		if !ok {
			break
		}
		out = append(out, string(name))
	}
	return out
}
