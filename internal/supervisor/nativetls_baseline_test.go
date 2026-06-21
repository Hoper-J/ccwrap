package supervisor

// Offline undici-fingerprint parity baseline.
//
// MAINTENANCE: this test reads a REAL undici ClientHello committed at
// internal/supervisor/testdata/undici_clienthello.bin (captured by running
// scripts/gen-undici-baseline.mjs once -- NOT in CI). It is the drift guard that
// fails loudly if a utls or Node/undici bump changes how the committed hello
// parses or is reproduced. When bumping the refraction-networking/utls
// dependency you MUST, together: (1) update the go.mod require, (2) update
// utlsPinnedVersion (guarded by TestUTLSVersionPinned), (3) re-run
// node scripts/gen-undici-baseline.mjs to recapture the hello + baseline.json,
// and (4) re-run this test and update the hard-coded GenericExtension set below
// if it legitimately changed. A govulncheck ./... gate is recommended in CI as a
// companion to this pin.

import (
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/tlsfp"

	utls "github.com/refraction-networking/utls"
)

// TestUndiciBaselineFingerprints is the cross-package anchor: tlsfp.Compute on
// the committed REAL undici ClientHello must reproduce the JA3/JA4/peetprint
// hashes recorded in testdata/undici_baseline.json. Those same three hashes are
// tls.peet.ws-cross-checked for this node/undici, so this binds ccwrap's Go
// fingerprinter to an externally-validated truth. The baseline JSON unmarshals
// into map[string]string because every value (node_version, captured_at, the
// three hashes, note) is a string. Regenerate the .bin + baseline.json together
// (scripts/gen-undici-baseline.mjs) when Node/undici moves.
func TestUndiciBaselineFingerprints(t *testing.T) {
	raw, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatalf("read committed undici ClientHello: %v", err)
	}
	bj, err := os.ReadFile("testdata/undici_baseline.json")
	if err != nil {
		t.Fatalf("read baseline json: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(bj, &m); err != nil {
		t.Fatalf("unmarshal baseline json (all values must be strings): %v", err)
	}
	r, err := tlsfp.Compute(raw)
	if err != nil {
		t.Fatalf("tlsfp.Compute on committed undici hello: %v", err)
	}
	if r.JA3 != m["ja3"] || r.JA4 != m["ja4"] || r.Peetprint != m["peetprint"] {
		t.Errorf("tlsfp.Compute != baseline:\n got  ja3=%s ja4=%s peet=%s\n want ja3=%s ja4=%s peet=%s",
			r.JA3, r.JA4, r.Peetprint, m["ja3"], m["ja4"], m["peetprint"])
	}
}

// genericExtIDs returns, in order, the wire IDs of every extension that utls
// parsed as a *GenericExtension (i.e. an extension it did NOT model with a typed
// struct). The set is the drift guard: a utls bump that starts (or stops)
// natively modelling one of undici's extensions changes this set and trips the
// assertion, forcing a conscious review.
func genericExtIDs(spec *utls.ClientHelloSpec) []uint16 {
	var ids []uint16
	for _, ext := range spec.Extensions {
		if g, ok := ext.(*utls.GenericExtension); ok {
			ids = append(ids, g.Id)
		}
	}
	return ids
}

func TestUndiciFingerprintBaseline(t *testing.T) {
	// (0) The pinned version constant must be present (cross-checks Task 1; the
	// real go.mod pin is enforced by TestUTLSVersionPinned).
	if utlsPinnedVersion == "" {
		t.Fatal("utlsPinnedVersion must be set (the utls pin the baseline is verified against)")
	}

	// (1) Read the committed REAL undici ClientHello (no node, no network).
	raw, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatalf("read committed undici ClientHello: %v", err)
	}
	if len(raw) < 64 || raw[0] != 0x16 || raw[1] != 0x03 {
		t.Fatalf("committed hello is not a TLS handshake record: len=%d first=0x%x 0x%x", len(raw), raw[0], raw[1])
	}

	// (2) It still parses under the pinned utls, with at least one cipher suite.
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(raw)
	if err != nil {
		t.Fatalf("FingerprintClientHello on the committed undici hello: %v", err)
	}
	if len(spec.CipherSuites) == 0 {
		t.Fatal("committed undici hello parsed to zero cipher suites")
	}

	// (3) Drift guard: the set of extensions utls leaves as *GenericExtension must
	// be STABLE. wantGeneric is the EMPIRICAL set the committed undici 7.21.0 hello
	// yields under utls v1.8.2 (determined by running this test once): exactly
	// [22] = encrypt_then_mac (RFC 7366); utls models it as
	// fakeExtensionEncryptThenMAC, so it round-trips as a *GenericExtension. If a
	// utls bump changes which extensions parse natively this set changes and the
	// test fails -- recapture the baseline and update this slice deliberately.
	wantGeneric := []uint16{22}
	gotGeneric := genericExtIDs(spec)
	if !equalU16(gotGeneric, wantGeneric) {
		t.Fatalf("GenericExtension set drifted\n got=%v\n want=%v\n(recapture the baseline + update wantGeneric if this is an intentional utls/undici bump)", gotGeneric, wantGeneric)
	}

	// (4) End-to-end parity: mirror the committed hello over a real loopback TLS
	// origin via the production nativeUTLSDialOver path; the origin peeks what
	// actually arrived and we assert the received fingerprint EQUALS the committed
	// one (same cipher suites + extension-ID list as the Task 9 E2E parity check).
	const originHost = "nativetls-baseline.test"
	origin := startFingerprintOrigin(t, originHost)

	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)

	rawConn, err := net.DialTimeout("tcp", origin.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial origin: %v", err)
	}
	// Verify against the DNS host (the leaf carries an originHost SAN), NOT the
	// loopback IP: utls/crypto omits SNI for IP ServerNames, which would drop the
	// server_name extension on the wire and make the received hello legitimately
	// differ from the committed one (which carries SNI for "localhost"). Using the
	// DNS host keeps SNI present so the full extension-ID parity holds, while the
	// connection still rides the real loopback dial.
	conn, err := nativeUTLSDialOver(rawConn, originHost, pool, raw)
	if err != nil {
		_ = rawConn.Close()
		t.Fatalf("nativeUTLSDialOver mirroring the committed undici hello: %v", err)
	}
	_ = conn.Close()

	rcvd := origin.received()
	if len(rcvd) == 0 {
		t.Fatal("origin recorded no ClientHello")
	}
	recvSpec := fingerprint(t, rcvd[0])
	if !equalU16(recvSpec.CipherSuites, spec.CipherSuites) {
		t.Fatalf("cipher suites differ\n received=%v\n committed=%v", recvSpec.CipherSuites, spec.CipherSuites)
	}
	gotExts := extIDs(t, recvSpec)
	wantExts := extIDs(t, spec)
	if !equalU16(gotExts, wantExts) {
		t.Fatalf("extension IDs differ\n received=%v (n=%d)\n committed=%v (n=%d)", gotExts, len(gotExts), wantExts, len(wantExts))
	}
}
