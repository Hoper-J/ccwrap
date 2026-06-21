//go:build livefp

package supervisor

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

// waitHelloCount polls the origin until it has recorded at least n ClientHellos
// (or the deadline elapses), returning the recorded slice.
func waitHelloCount(t *testing.T, o *fingerprintOrigin, n int, d time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		got := o.received()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("origin recorded %d ClientHello(s), want >= %d within %s", len(got), n, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLiveFingerprintCompare is a LIVE before/after fingerprint check. The
// `livefp` build tag keeps it out of normal `go test ./...` runs; it executes
// on demand (manual -tags livefp invocation) and WEEKLY IN CI via the
// fingerprint-drift workflow — which hard-fails if this file or test name
// goes missing, so do not rename/move it without updating
// .github/workflows/fingerprint-drift.yml.
//
// It answers, on the wire and live: does the fingerprint ccwrap ACTUALLY emits
// on its mirrored upstream dial equal the fingerprint a REAL undici client emits?
//
//	前 (before): a real Node built-in-undici (fetch) client hits a local
//	            fingerprint-capturing TLS origin; the origin peeks the raw
//	            ClientHello undici actually wrote on the wire.
//	后 (after):  that exact captured hello is driven through the PRODUCTION
//	            nativeUTLSDialOver (the same dial the real binary uses) to the
//	            same origin, which peeks ccwrap's actual outbound ClientHello.
//
// Both raw hellos are run through tlsfp.Compute and the JA3/JA4/peetprint triples
// are asserted identical. The committed undici baseline is printed for context
// (it may differ if THIS node bundles a different undici than the baseline's).
//
//	go test ./internal/supervisor/ -tags livefp -run TestLiveFingerprintCompare -v
func TestLiveFingerprintCompare(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found; live undici capture unavailable")
	}

	const host = "localhost" // a name (not 127.0.0.1) so undici emits SNI, like production
	origin := startFingerprintOrigin(t, host)

	_, port, err := net.SplitHostPort(origin.addr)
	if err != nil {
		t.Fatalf("split origin addr: %v", err)
	}
	url := fmt.Sprintf("https://%s:%s/", host, port)

	// --- 前: live undici (Node built-in fetch) -> origin. NODE_TLS_REJECT_
	// UNAUTHORIZED=0 lets the handshake complete against the throwaway-CA origin;
	// the ClientHello is captured before verification regardless.
	script := fmt.Sprintf(`fetch(%q).then(r=>r.text()).then(()=>process.exit(0)).catch(e=>{console.error('fetch error:',String(e));process.exit(0)})`, url)
	cmd := exec.Command("node", "-e", script)
	cmd.Env = append(os.Environ(), "NODE_TLS_REJECT_UNAUTHORIZED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node undici run: %v\n%s", err, out)
	}

	before := waitHelloCount(t, origin, 1, 10*time.Second)
	undiciRaw := before[0]
	cntAfterUndici := len(before)

	// --- 后: production mirror dial replays the captured undici hello -> origin.
	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)
	rawConn, err := net.DialTimeout("tcp", origin.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial origin for mirror: %v", err)
	}
	conn, err := nativeUTLSDialOver(rawConn, host, pool, undiciRaw)
	if err != nil {
		_ = rawConn.Close()
		t.Fatalf("nativeUTLSDialOver mirroring the live undici hello: %v", err)
	}
	_ = conn.Close()

	all := waitHelloCount(t, origin, cntAfterUndici+1, 10*time.Second)
	ccwrapRaw := all[cntAfterUndici] // first hello recorded AFTER undici's

	// --- compute + compare ---
	fpBefore, err := tlsfp.Compute(undiciRaw)
	if err != nil {
		t.Fatalf("tlsfp.Compute(前 undici): %v", err)
	}
	fpAfter, err := tlsfp.Compute(ccwrapRaw)
	if err != nil {
		t.Fatalf("tlsfp.Compute(后 ccwrap): %v", err)
	}

	t.Logf("前 live undici  (%d bytes): JA3=%s  JA4=%s  peet=%s", len(undiciRaw), fpBefore.JA3, fpBefore.JA4, fpBefore.Peetprint)
	t.Logf("后 ccwrap mirror (%d bytes): JA3=%s  JA4=%s  peet=%s", len(ccwrapRaw), fpAfter.JA3, fpAfter.JA4, fpAfter.Peetprint)

	if fpBefore != fpAfter {
		t.Fatalf("LIVE FINGERPRINT MISMATCH (前 != 后)\n 前=%+v\n 后=%+v", fpBefore, fpAfter)
	}
	t.Log("MATCH: ccwrap's outbound fingerprint == live undici's, JA3+JA4+peetprint all identical")

	// Context: compare against the committed baseline (informational — a different
	// bundled undici would legitimately differ, so this does NOT fail the test).
	if bj, err := os.ReadFile("testdata/undici_baseline.json"); err == nil {
		var m map[string]string
		if json.Unmarshal(bj, &m) == nil {
			match := fpBefore.JA3 == m["ja3"] && fpBefore.JA4 == m["ja4"] && fpBefore.Peetprint == m["peetprint"]
			t.Logf("committed baseline (node %s): JA3=%s  JA4=%s  peet=%s  -> live-undici matches baseline: %v",
				m["node_version"], m["ja3"], m["ja4"], m["peetprint"], match)
		}
	}
}
