//go:build livefp

package supervisor

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

// TestLiveClaudeFingerprintMirror is the LIVE check against the REAL Claude
// Code binary: a native (Bun) build that runs more than one TLS stack (its
// /v1/messages calls use one ClientHello, most of its other calls another), so
// TestLiveFingerprintCompare's undici cannot see it drift. Nothing leaves the
// machine: a CONNECT proxy wires every connection to a local origin posing as
// api.anthropic.com, which records each connection's ClientHello and the
// requests made on it.
//
//  1. Claude Code directly: the ClientHello shape(s) each request path uses.
//     A /v1/messages request must be among them.
//  2. HARD: every shape survives the production mirror dial with identical
//     JA3/JA4/peetprint; otherwise native TLS (fail-closed) would block the
//     requests that use it.
//  3. HARD: the same Claude Code through a real in-process supervisor on a
//     first-party route: every request reaches the upstream with one of Claude
//     Code's own shapes for its path, /v1/messages with the /v1/messages one
//     (per-connection mirroring, hellostack.go).
//  4. INFO: a stack differing from testdata/claude_baseline.json logs a
//     CLAUDE_FINGERPRINT_DRIFT line (the weekly workflow turns it into a
//     warning). CCWRAP_LIVEFP_UPDATE_BASELINE=1 rewrites the committed hellos
//     and baseline from this capture instead.
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
	// Not t.TempDir: a just-finished claude can still be flushing into HOME,
	// and a failed cleanup must not fail the check.
	root, err := os.MkdirTemp("", "livefp-claude-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// 1. Claude Code directly.
	direct := startShapeOrigin(t)
	direct.messagesStatus = 400 // non-retryable: claude -p stops after one call
	directProxy := startConnectProxy(t, direct.addr)
	env := liveClaudeEnv(t, root, "home-direct", "http://"+directProxy.addr, writeCAFile(t, root, direct.ca))
	version := claudeVersion(claudeBin, env)
	runLiveClaude(t, claudeBin, env)
	own := shapesByPath(t, direct)
	messages := own.byPath["/v1/messages"]
	if len(messages) == 0 {
		t.Fatalf("Claude Code %s sent no /v1/messages to the origin (paths: %v)", version, own.paths())
	}
	if len(messages) > 1 {
		t.Logf("Claude Code %s used %d shapes for /v1/messages; the baseline keeps the first", version, len(messages))
	}
	t.Logf("live Claude Code %s: %d ClientHello shape(s)", version, len(own.shapes))
	for _, key := range own.order {
		s := own.shapes[key]
		t.Logf("  %s JA3=%s JA4=%s peet=%s paths=%v", key, s.fp.JA3, s.fp.JA4, s.fp.Peetprint, s.paths)
	}

	// 2. Every shape survives the production mirror.
	for _, key := range own.order {
		s := own.shapes[key]
		if out := mirrorFingerprint(t, s.raw); out != s.fp {
			t.Fatalf("LIVE MIRROR MISMATCH for Claude Code %s (paths %v)\n claude=%+v\n ccwrap=%+v", version, s.paths, s.fp, out)
		}
	}
	t.Log("MATCH: ccwrap's mirrored hellos == Claude Code's, JA3+JA4+peetprint identical")

	// 3. Through ccwrap, each request keeps its own connection's shape.
	upstream := startShapeOrigin(t)
	upstream.messagesStatus = 400
	upstreamProxy := startConnectProxy(t, upstream.addr)
	pool := x509.NewCertPool()
	pool.AddCert(upstream.ca)
	nativeRootsForTest = pool
	t.Cleanup(func() { nativeRootsForTest = nil })
	egressURL := "http://" + upstreamProxy.addr
	caBundle, sess := routedSession(t, model.SessionRouteRequest{
		APIBaseURL: "https://api.anthropic.com", ExactUpstreamHost: "api.anthropic.com", ExactUpstreamBase: "https://api.anthropic.com",
		RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceExplicit,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone,
		Egress:     model.EgressConfig{Mode: "explicit", HTTPProxy: egressURL, HTTPSProxy: egressURL, Source: "livefp", Summary: egressURL},
		FailPolicy: model.FailClosed, NativeTLS: true,
	})
	runLiveClaude(t, claudeBin, liveClaudeEnv(t, root, "home-ccwrap", "http://"+sess.ProxyListenAddr, caBundle))
	through := shapesByPath(t, upstream)
	if len(through.byPath["/v1/messages"]) == 0 {
		t.Fatalf("no /v1/messages reached the upstream through ccwrap (paths: %v)", through.paths())
	}
	for path, keys := range through.byPath {
		allowed := own.byPath[path]
		if len(allowed) == 0 {
			continue // Claude Code did not call this path in the direct run
		}
		for key := range keys {
			if !allowed[key] {
				t.Errorf("through ccwrap, %s went upstream with JA4 %s; Claude Code itself used %v for it", path, through.shapes[key].fp.JA4, own.ja4s(allowed))
			}
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	t.Log("MATCH: through ccwrap every request kept its own connection's ClientHello shape upstream")

	// 4. Baseline: one record per role.
	live := map[string]*liveShape{"messages": own.shapes[own.first(messages)]}
	if other := own.mostUsedExcept(live["messages"].key); other != nil {
		live["other"] = other
	}
	if os.Getenv("CCWRAP_LIVEFP_UPDATE_BASELINE") != "" {
		writeClaudeBaseline(t, live, version)
		return
	}
	want := readClaudeBaseline(t)
	recorded := map[string]claudeStackRecord{}
	for _, st := range want.Stacks {
		recorded[st.Role] = st
	}
	for _, role := range []string{"messages", "other"} {
		got, rec := live[role], recorded[role]
		switch {
		case got == nil && rec.Role == "":
		case got == nil:
			t.Logf("CLAUDE_FINGERPRINT_DRIFT Claude Code %s no longer shows a separate %q stack (baseline from Claude Code %s has JA4 %s). Review, then refresh with CCWRAP_LIVEFP_UPDATE_BASELINE=1.", version, role, want.ClaudeVersion, rec.JA4)
		case got.fp != rec.fingerprint():
			t.Logf("CLAUDE_FINGERPRINT_DRIFT Claude Code %s %q stack (JA4 %s) differs from the committed baseline (Claude Code %s, JA4 %s); the mirror still holds. Review, then refresh with CCWRAP_LIVEFP_UPDATE_BASELINE=1.", version, role, got.fp.JA4, want.ClaudeVersion, rec.JA4)
		default:
			t.Logf("%q stack matches the committed baseline (captured from Claude Code %s)", role, want.ClaudeVersion)
		}
	}
}

// liveShape is one ClientHello shape a run produced, with the paths requested
// over connections that used it.
type liveShape struct {
	key   string
	raw   []byte
	fp    tlsfp.Result
	conns int
	paths []string
}

type liveShapes struct {
	shapes map[string]*liveShape
	order  []string
	byPath map[string]map[string]bool // path -> shape keys
}

// shapesByPath groups the origin's api.anthropic.com connections by
// ClientHello shape.
func shapesByPath(t *testing.T, o *shapeOrigin) liveShapes {
	t.Helper()
	out := liveShapes{shapes: map[string]*liveShape{}, byPath: map[string]map[string]bool{}}
	for _, c := range o.snapshot() {
		if helloServerName(c.hello) != claudeAPIHost {
			continue
		}
		fp, err := tlsfp.Compute(c.hello)
		if err != nil {
			t.Fatalf("tlsfp.Compute on a live Claude Code hello: %v", err)
		}
		key := helloShapeKey(fp)
		s := out.shapes[key]
		if s == nil {
			s = &liveShape{key: key, raw: c.hello, fp: fp}
			out.shapes[key] = s
			out.order = append(out.order, key)
		}
		s.conns++
		for _, p := range c.paths {
			if out.byPath[p] == nil {
				out.byPath[p] = map[string]bool{}
			}
			out.byPath[p][key] = true
			if !containsString(s.paths, p) {
				s.paths = append(s.paths, p)
			}
		}
	}
	return out
}

func (l liveShapes) paths() []string {
	var ps []string
	for p := range l.byPath {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

func (l liveShapes) ja4s(keys map[string]bool) []string {
	var out []string
	for k := range keys {
		out = append(out, l.shapes[k].fp.JA4)
	}
	sort.Strings(out)
	return out
}

// first returns the earliest-seen key among keys.
func (l liveShapes) first(keys map[string]bool) string {
	for _, k := range l.order {
		if keys[k] {
			return k
		}
	}
	return ""
}

// mostUsedExcept is the shape with the most connections other than key.
func (l liveShapes) mostUsedExcept(key string) *liveShape {
	var best *liveShape
	for _, k := range l.order {
		if s := l.shapes[k]; k != key && (best == nil || s.conns > best.conns) {
			best = s
		}
	}
	return best
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// liveClaudeEnv is an isolated environment for claude -p through proxyURL,
// trusting caFile. The caller's own Claude settings may set HTTPS_PROXY, which
// would beat the process env; the dummy key gets -p past "not logged in" and
// only ever reaches the local origin.
func liveClaudeEnv(t *testing.T, root, name, proxyURL, caFile string) []string {
	t.Helper()
	home := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"TMPDIR=" + os.TempDir(),
		"HTTPS_PROXY=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"NO_PROXY=127.0.0.1,localhost",
		"NODE_EXTRA_CA_CERTS=" + caFile,
		"ANTHROPIC_API_KEY=sk-ant-livefp-dummy",
		"DISABLE_AUTOUPDATER=1",
	}
}

// runLiveClaude runs claude -p in env's HOME until it exits or 120s pass; its
// process group is killed either way.
func runLiveClaude(t *testing.T, bin string, env []string) {
	t.Helper()
	cmd := exec.Command(bin, "-p", "hi")
	cmd.Env = env
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			cmd.Dir = strings.TrimPrefix(kv, "HOME=")
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start claude: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		t.Log("claude -p did not exit within 120s; killing it")
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-done
}

func writeCAFile(t *testing.T, root string, ca *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(root, "origin-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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

func writeClaudeBaseline(t *testing.T, live map[string]*liveShape, version string) {
	t.Helper()
	b := claudeBaseline{
		ClaudeVersion: version,
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
		Note:          "raw ClientHellos of the real Claude Code binary via the CONNECT path (SNI api.anthropic.com), one per TLS stack: \"messages\" carried /v1/messages, \"other\" most of the other calls. Captured by TestLiveClaudeFingerprintMirror; ja3/ja4/peetprint are Go-computed (internal/tlsfp); regenerate everything together with CCWRAP_LIVEFP_UPDATE_BASELINE=1",
	}
	for _, role := range []string{"messages", "other"} {
		s := live[role]
		if s == nil {
			continue
		}
		file := "claude_hello_" + role + ".bin"
		if err := os.WriteFile(filepath.Join("testdata", file), s.raw, 0o644); err != nil {
			t.Fatal(err)
		}
		b.Stacks = append(b.Stacks, claudeStackRecord{Role: role, File: file, JA3: s.fp.JA3, JA4: s.fp.JA4, Peetprint: s.fp.Peetprint, Paths: s.paths})
	}
	bj, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeBaselinePath, append(bj, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote the %d committed hello(s) + %s from Claude Code %s", len(b.Stacks), claudeBaselinePath, version)
}
