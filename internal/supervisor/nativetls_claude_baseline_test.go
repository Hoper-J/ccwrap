package supervisor

// Offline Claude Code fingerprint parity baseline.
//
// Claude Code ships as a native (Bun) binary with more than one TLS stack: its
// /v1/messages calls use one ClientHello, most of its other calls another
// (hellostack.go). This test replays the REAL hello of each, committed under
// testdata/ and listed in claude_baseline.json by role, through the production
// mirror dial, so a utls bump that stops reproducing either fails here, in
// normal CI — before native TLS's fail-closed dial blocks those requests.
//
// MAINTENANCE: regenerate the .bin files + claude_baseline.json together from
// the real binary (go test ./internal/supervisor/ -tags livefp -run
// TestLiveClaudeFingerprintMirror with CCWRAP_LIVEFP_UPDATE_BASELINE=1) when the
// weekly drift watch reports Claude Code drift, then re-run this test and update
// wantGeneric below if it legitimately changed.

import (
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/tlsfp"

	utls "github.com/refraction-networking/utls"
)

const (
	claudeBaselinePath = "testdata/claude_baseline.json"
	// claudeAPIHost is the SNI Claude Code sends in production; mirroring the
	// committed hellos under the same name keeps the server_name extension
	// (and its length) identical.
	claudeAPIHost = "api.anthropic.com"
)

// claudeBaseline is testdata/claude_baseline.json.
type claudeBaseline struct {
	ClaudeVersion string              `json:"claude_version"`
	CapturedAt    string              `json:"captured_at"`
	Stacks        []claudeStackRecord `json:"stacks"`
	Note          string              `json:"note"`
}

// claudeStackRecord is one Claude Code TLS stack: "messages" is the hello its
// /v1/messages calls use, "other" the one most of its other calls use.
type claudeStackRecord struct {
	Role      string   `json:"role"`
	File      string   `json:"file"`
	JA3       string   `json:"ja3"`
	JA4       string   `json:"ja4"`
	Peetprint string   `json:"peetprint"`
	Paths     []string `json:"paths,omitempty"`
}

func (r claudeStackRecord) fingerprint() tlsfp.Result {
	return tlsfp.Result{JA3: r.JA3, JA4: r.JA4, Peetprint: r.Peetprint}
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
		t.Fatalf("nativeUTLSDialOver could not mirror the Claude Code hello (native TLS would block its requests): %v", err)
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
	base := readClaudeBaseline(t)
	roles := map[string]bool{}
	for _, st := range base.Stacks {
		roles[st.Role] = true
	}
	if !roles["messages"] {
		t.Fatalf("%s must record the /v1/messages stack", claudeBaselinePath)
	}
	// The extensions utls leaves as *GenericExtension, per stack (empirical
	// for Claude Code 2.1.296 under utls v1.8.2).
	wantGeneric := map[string][]uint16{"messages": nil, "other": nil}
	for _, st := range base.Stacks {
		t.Run(st.Role, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", st.File))
			if err != nil {
				t.Fatalf("read committed Claude Code ClientHello: %v", err)
			}
			// (1) The committed hello still fingerprints to the recorded hashes.
			in, err := tlsfp.Compute(raw)
			if err != nil {
				t.Fatalf("tlsfp.Compute on committed Claude Code hello: %v", err)
			}
			if in != st.fingerprint() {
				t.Fatalf("committed %s hello != %s:\n got  %+v\n want %+v", st.Role, claudeBaselinePath, in, st.fingerprint())
			}
			// (2) Drift guard on how utls models it.
			spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(raw)
			if err != nil {
				t.Fatalf("FingerprintClientHello on the committed Claude Code hello: %v", err)
			}
			if got := genericExtIDs(spec); !equalU16(got, wantGeneric[st.Role]) {
				t.Fatalf("GenericExtension set drifted\n got=%v\n want=%v\n(regenerate the baseline + update wantGeneric if this is an intentional utls/Claude Code bump)", got, wantGeneric[st.Role])
			}
			// (3) The production mirror reproduces it on the wire.
			if out := mirrorFingerprint(t, raw); out != in {
				t.Fatalf("mirrored hello differs from the committed Claude Code hello\n in=%+v\nout=%+v", in, out)
			}
		})
	}
}
