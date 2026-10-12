//go:build livefp

package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/tlsfp"

	utls "github.com/refraction-networking/utls"
)

// TestLiveClaudeFingerprintMirror is the LIVE check against the REAL Claude
// Code binary — a native (Bun) build whose ClientHello is not undici's, so
// TestLiveFingerprintCompare cannot see it drift. It runs `claude -p` behind a
// CONNECT proxy wired to a local fingerprint origin: the same CONNECT-then-TLS
// path ccwrap's session proxy sees, with the production SNI, and nothing
// leaves the machine. Then:
//
//  1. HARD: the captured hello must survive the production mirror dial with
//     identical JA3/JA4/peetprint. If it does not, native TLS (fail-closed)
//     would block every Anthropic request on this Claude Code version.
//  2. INFO: on a mismatch with testdata/claude_baseline.json it logs a
//     CLAUDE_FINGERPRINT_DRIFT line (the weekly workflow turns it into a
//     warning). CCWRAP_LIVEFP_UPDATE_BASELINE=1 rewrites the committed hello +
//     baseline from this capture instead.
//
// claude comes from CCWRAP_LIVEFP_CLAUDE_BIN or PATH; without it the test skips,
// unless CCWRAP_LIVEFP_REQUIRE_CLAUDE=1 (set by the weekly workflow).
//
//	go test ./internal/supervisor/ -tags livefp -run TestLiveClaudeFingerprintMirror -v
func TestLiveClaudeFingerprintMirror(t *testing.T) {
	claudeBin := os.Getenv("CCWRAP_LIVEFP_CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin, _ = exec.LookPath("claude")
	}
	if claudeBin == "" {
		if os.Getenv("CCWRAP_LIVEFP_REQUIRE_CLAUDE") != "" {
			t.Fatal("claude not found and CCWRAP_LIVEFP_REQUIRE_CLAUDE is set")
		}
		t.Skip("claude not found; put it on PATH or set CCWRAP_LIVEFP_CLAUDE_BIN")
	}

	origin := startFingerprintOrigin(t, claudeAPIHost)
	proxy := startConnectProxy(t, origin.addr)

	// Isolated env: the caller's own Claude settings may set HTTPS_PROXY, which
	// would beat the process env. The dummy key gets -p past "not logged in";
	// it only ever reaches the local origin, and not even that (the handshake
	// fails on the throwaway CA before any HTTP).
	home := t.TempDir()
	proxyURL := "http://" + proxy.addr
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"TMPDIR=" + os.TempDir(),
		"HTTPS_PROXY=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"ANTHROPIC_API_KEY=sk-ant-livefp-dummy",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	version := claudeVersion(claudeBin, env)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, claudeBin, "-p", "hi")
	cmd.Env = env
	cmd.Dir = home
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start claude: %v", err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	raw := waitAPIHello(t, origin, proxy, 90*time.Second)
	cancel()

	in, err := tlsfp.Compute(raw)
	if err != nil {
		t.Fatalf("tlsfp.Compute on the live Claude Code hello: %v", err)
	}
	t.Logf("live Claude Code %s (%d bytes): JA3=%s JA4=%s peet=%s", version, len(raw), in.JA3, in.JA4, in.Peetprint)

	if out := mirrorFingerprint(t, raw); out != in {
		t.Fatalf("LIVE MIRROR MISMATCH for Claude Code %s\n claude=%+v\n ccwrap=%+v", version, in, out)
	}
	t.Log("MATCH: ccwrap's mirrored hello == Claude Code's, JA3+JA4+peetprint identical")

	if os.Getenv("CCWRAP_LIVEFP_UPDATE_BASELINE") != "" {
		writeClaudeBaseline(t, raw, in, version)
		return
	}
	want := readClaudeBaseline(t)
	if in.JA3 != want.JA3 || in.JA4 != want.JA4 || in.Peetprint != want.Peetprint {
		t.Logf("CLAUDE_FINGERPRINT_DRIFT Claude Code %s (JA4 %s) differs from the committed baseline (Claude Code %s, JA4 %s); the mirror still holds. Review, then refresh with CCWRAP_LIVEFP_UPDATE_BASELINE=1.",
			version, in.JA4, want.ClaudeVersion, want.JA4)
	} else {
		t.Logf("matches the committed baseline (captured from Claude Code %s)", want.ClaudeVersion)
	}
}

// waitAPIHello returns the first hello the origin received for claudeAPIHost.
// A hello utls cannot parse is returned as-is, so the caller fails on the real
// error rather than on a timeout.
func waitAPIHello(t *testing.T, o *fingerprintOrigin, p *connectProxy, d time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, raw := range o.received() {
			if _, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(raw); err != nil {
				return raw
			}
			if helloServerName(raw) == claudeAPIHost {
				return raw
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ClientHello for %s within %s (origin saw %d hello(s); CONNECTs: %v)", claudeAPIHost, d, len(o.received()), p.connects())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// helloServerName extracts the SNI host from a raw ClientHello record (utls's
// Fingerprinter drops the name, keeping only the extension).
func helloServerName(raw []byte) string {
	b := raw
	skip := func(n int) bool {
		if n < 0 || len(b) < n {
			return false
		}
		b = b[n:]
		return true
	}
	vec := func(lenBytes int) ([]byte, bool) {
		if len(b) < lenBytes {
			return nil, false
		}
		n := 0
		for _, c := range b[:lenBytes] {
			n = n<<8 | int(c)
		}
		b = b[lenBytes:]
		if len(b) < n {
			return nil, false
		}
		v := b[:n]
		b = b[n:]
		return v, true
	}
	// record header, handshake header, version, random
	if !skip(5 + 4 + 2 + 32) {
		return ""
	}
	for _, lenBytes := range []int{1, 2, 1} { // session_id, cipher_suites, compression
		if _, ok := vec(lenBytes); !ok {
			return ""
		}
	}
	exts, ok := vec(2)
	if !ok {
		return ""
	}
	b = exts
	for len(b) >= 4 {
		typ := int(b[0])<<8 | int(b[1])
		b = b[2:]
		data, ok := vec(2)
		if !ok {
			return ""
		}
		if typ != 0 {
			continue
		}
		// server_name_list<2>: name_type(1) host_name<2>
		b = data
		if list, ok := vec(2); ok && len(list) > 3 && list[0] == 0 {
			b = list[1:]
			if host, ok := vec(2); ok {
				return string(host)
			}
		}
		return ""
	}
	return ""
}

func claudeVersion(bin string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if fields := strings.Fields(string(out)); err == nil && len(fields) > 0 {
		return fields[0]
	}
	return "unknown"
}

func writeClaudeBaseline(t *testing.T, raw []byte, fp tlsfp.Result, version string) {
	t.Helper()
	b := claudeBaseline{
		ClaudeVersion: version,
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
		JA3:           fp.JA3,
		JA4:           fp.JA4,
		Peetprint:     fp.Peetprint,
		Note:          "raw ClientHello of the real Claude Code binary via the CONNECT path (SNI api.anthropic.com), captured by TestLiveClaudeFingerprintMirror; ja3/ja4/peetprint are Go-computed (internal/tlsfp); regenerate both files together with CCWRAP_LIVEFP_UPDATE_BASELINE=1",
	}
	bj, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeHelloPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeBaselinePath, append(bj, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s + %s from Claude Code %s", claudeHelloPath, claudeBaselinePath, version)
}
