package supervisor

// Offline Claude Code fingerprint parity baseline.
//
// Claude Code ships as a native (Bun) binary, so the hello ccwrap mirrors in
// production is NOT the node/undici one the undici baseline covers. This test
// replays a REAL Claude Code ClientHello committed at
// testdata/claude_clienthello.bin through the production mirror dial, so a utls
// bump that stops reproducing it fails here, in normal CI — before it can turn
// native TLS's fail-closed dial into a block on every Anthropic request.
//
// MAINTENANCE: regenerate the .bin + claude_baseline.json together from the
// real binary (go test ./internal/supervisor/ -tags livefp -run
// TestLiveClaudeFingerprintMirror with CCWRAP_LIVEFP_UPDATE_BASELINE=1) when the
// weekly drift watch reports Claude Code drift, then re-run this test and update
// wantGeneric below if it legitimately changed.

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

const (
	claudeHelloPath    = "testdata/claude_clienthello.bin"
	claudeBaselinePath = "testdata/claude_baseline.json"
	// claudeAPIHost is the SNI Claude Code sends in production; mirroring the
	// committed hello under the same name keeps the server_name extension (and
	// its length) identical.
	claudeAPIHost = "api.anthropic.com"
)

// claudeBaseline is testdata/claude_baseline.json; every value is a string.
type claudeBaseline struct {
	ClaudeVersion string `json:"claude_version"`
	CapturedAt    string `json:"captured_at"`
	JA3           string `json:"ja3"`
	JA4           string `json:"ja4"`
	Peetprint     string `json:"peetprint"`
	Note          string `json:"note"`
}

func readClaudeBaseline(t *testing.T) claudeBaseline {
	t.Helper()
	bj, err := os.ReadFile(claudeBaselinePath)
	if err != nil {
		t.Fatalf("read %s: %v", claudeBaselinePath, err)
	}
	var b claudeBaseline
	if err := json.Unmarshal(bj, &b); err != nil {
		t.Fatalf("unmarshal %s: %v", claudeBaselinePath, err)
	}
	return b
}

// mirrorFingerprint replays raw through the production nativeUTLSDialOver to a
// loopback origin (cert and SNI for claudeAPIHost) and returns the fingerprint
// of the hello that actually arrived on the wire.
func mirrorFingerprint(t *testing.T, raw []byte) tlsfp.Result {
	t.Helper()
	origin := startFingerprintOrigin(t, claudeAPIHost)
	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)
	rawConn, err := net.DialTimeout("tcp", origin.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial origin: %v", err)
	}
	conn, err := nativeUTLSDialOver(rawConn, claudeAPIHost, pool, raw)
	if err != nil {
		_ = rawConn.Close()
		t.Fatalf("nativeUTLSDialOver could not mirror the Claude Code hello (native TLS would block every Anthropic request): %v", err)
	}
	_ = conn.Close()
	got := waitHelloCount(t, origin, 1, 10*time.Second)
	fp, err := tlsfp.Compute(got[0])
	if err != nil {
		t.Fatalf("tlsfp.Compute on the mirrored hello: %v", err)
	}
	return fp
}

func TestClaudeBaselineMirrors(t *testing.T) {
	raw, err := os.ReadFile(claudeHelloPath)
	if err != nil {
		t.Fatalf("read committed Claude Code ClientHello: %v", err)
	}
	want := readClaudeBaseline(t)

	// (1) The committed hello still fingerprints to the recorded hashes.
	in, err := tlsfp.Compute(raw)
	if err != nil {
		t.Fatalf("tlsfp.Compute on committed Claude Code hello: %v", err)
	}
	if in.JA3 != want.JA3 || in.JA4 != want.JA4 || in.Peetprint != want.Peetprint {
		t.Fatalf("committed hello != %s:\n got  ja3=%s ja4=%s peet=%s\n want ja3=%s ja4=%s peet=%s",
			claudeBaselinePath, in.JA3, in.JA4, in.Peetprint, want.JA3, want.JA4, want.Peetprint)
	}

	// (2) Drift guard on how utls models it: the extensions utls leaves as
	// *GenericExtension (empirical for Claude Code 2.1.296 under utls v1.8.2).
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(raw)
	if err != nil {
		t.Fatalf("FingerprintClientHello on the committed Claude Code hello: %v", err)
	}
	wantGeneric := []uint16(nil)
	if got := genericExtIDs(spec); !equalU16(got, wantGeneric) {
		t.Fatalf("GenericExtension set drifted\n got=%v\n want=%v\n(regenerate the baseline + update wantGeneric if this is an intentional utls/Claude Code bump)", got, wantGeneric)
	}

	// (3) The production mirror reproduces it on the wire.
	if out := mirrorFingerprint(t, raw); out != in {
		t.Fatalf("mirrored hello differs from the committed Claude Code hello\n in=%+v\nout=%+v", in, out)
	}
}
