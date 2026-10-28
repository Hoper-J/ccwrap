//go:build livefp

package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestLiveClaudeHeaderWireOrder is the LIVE check that ccwrap forwards the REAL
// Claude Code's request head in Claude Code's own layout (header order and
// name casing), not net/http's sorted, canonicalized one. It runs claude twice
// with the environment ccwrap gives it — bearer placeholder, host-managed flag:
//
//  1. directly, behind a CONNECT proxy to a local origin posing as
//     api.anthropic.com: the head Claude Code itself puts on the wire;
//  2. through ccwrap (a bearer profile to a local plain-HTTP upstream): the
//     head ccwrap forwards.
//
// The header names, in order and casing, must match. Nothing leaves the
// machine. claude comes from CCWRAP_LIVEFP_CLAUDE_BIN or PATH; without it the
// test skips, unless CCWRAP_LIVEFP_REQUIRE_CLAUDE=1 (the weekly workflow).
//
//	go test ./cmd/ccwrap/ -tags livefp -run TestLiveClaudeHeaderWireOrder -v
func TestLiveClaudeHeaderWireOrder(t *testing.T) {
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
	// Not t.TempDir: a just-killed claude can still be flushing into HOME, and
	// a failed cleanup must not fail the check.
	root, err := os.MkdirTemp("", "livefp-wire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ccwrapBin := filepath.Join(root, "ccwrap-livefp")
	if out, err := exec.Command("go", "build", "-o", ccwrapBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build ccwrap: %v\n%s", err, out)
	}
	const placeholder = "ccwrap-livefp-placeholder"

	// 1. Claude Code directly.
	caFile := filepath.Join(root, "origin-ca.pem")
	direct := startLiveHeadSink(t, true, caFile)
	proxy := startLiveConnectProxy(t, direct.addr)
	home := mkLiveHome(t, root, "home-direct")
	runLiveChild(t, direct, claudeBin, home, []string{
		"HTTPS_PROXY=http://" + proxy, "HTTP_PROXY=http://" + proxy,
		"NODE_EXTRA_CA_CERTS=" + caFile,
		"ANTHROPIC_AUTH_TOKEN=" + placeholder,
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
	})
	want := direct.firstMessagesHead(t)

	// 2. The same Claude Code through ccwrap.
	upstream := startLiveHeadSink(t, false, "")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := fmt.Sprintf(`{"default":"livefp","profiles":{"livefp":{"provider":"livefp","base_url":"http://%s","auth":{"mode":"ccwrap_bearer","key":"sk-livefp-not-real"},"egress":{"mode":"direct"}}}}`, upstream.addr)
	if err := os.WriteFile(filepath.Join(state, "profiles.json"), []byte(profiles), 0o600); err != nil {
		t.Fatal(err)
	}
	home = mkLiveHome(t, root, "home-ccwrap")
	runLiveChild(t, upstream, ccwrapBin, home, []string{
		"CCWRAP_STATE_DIR=" + state, "CCWRAP_NO_INIT=1", "CCWRAP_NO_UPDATE_CHECK=1", "CCWRAP_QUIET=1",
		"TZ=America/Los_Angeles",
	}, "--claude-bin", claudeBin)
	got := upstream.firstMessagesHead(t)

	wantNames, gotNames := headerNames(want), headerNames(got)
	t.Logf("Claude Code: %s", strings.Join(wantNames, ", "))
	if strings.Join(gotNames, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("ccwrap's upstream head does not keep Claude Code's layout\n claude: %s\n ccwrap: %s",
			strings.Join(wantNames, ", "), strings.Join(gotNames, ", "))
	}
	t.Log("MATCH: ccwrap forwards Claude Code's header order and casing")
}

func mkLiveHome(t *testing.T, root, name string) string {
	t.Helper()
	home := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// runLiveChild runs bin -p in an isolated HOME until sink has a /v1/messages
// head or 90s pass; the process is killed either way.
func runLiveChild(t *testing.T, sink *liveHeadSink, bin, home string, env []string, args ...string) {
	t.Helper()
	args = append(args, "-p", "hi")
	cmd := exec.Command(bin, args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"TMPDIR=" + os.TempDir(), "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CCWRAP_NO_UI=1",
	}, env...)
	cmd.Dir = home
	// Own process group: killing ccwrap alone would orphan its claude child
	// off Linux (no Pdeathsig there).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	done := make(chan struct{})
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.After(90 * time.Second)
	for !sink.hasMessages() {
		select {
		case <-done:
		case <-deadline:
		case <-time.After(100 * time.Millisecond):
			continue
		}
		break
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-done
}

// liveHeadSink records raw request heads (TLS for api.anthropic.com when
// useTLS, plain HTTP otherwise) and answers a non-retryable 400.
type liveHeadSink struct {
	addr  string
	mu    sync.Mutex
	heads []string
}

func (s *liveHeadSink) messagesHead() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.heads {
		if strings.HasPrefix(h, "POST ") && strings.Contains(strings.SplitN(h, "\r\n", 2)[0], "/v1/messages") {
			return h, true
		}
	}
	return "", false
}

func (s *liveHeadSink) hasMessages() bool {
	_, ok := s.messagesHead()
	return ok
}

func (s *liveHeadSink) firstMessagesHead(t *testing.T) string {
	t.Helper()
	h, ok := s.messagesHead()
	if !ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		t.Fatalf("no POST /v1/messages reached the sink (heads: %q)", s.heads)
	}
	return h
}

func startLiveHeadSink(t *testing.T, useTLS bool, caFile string) *liveHeadSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if useTLS {
		ln = tls.NewListener(ln, liveOriginTLS(t, caFile))
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &liveHeadSink{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					var head strings.Builder
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						head.WriteString(line)
						if line == "\r\n" {
							break
						}
					}
					req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(head.String())))
					if err != nil {
						return
					}
					if req.ContentLength > 0 {
						if _, err := io.CopyN(io.Discard, br, req.ContentLength); err != nil {
							return
						}
					}
					s.mu.Lock()
					s.heads = append(s.heads, head.String())
					s.mu.Unlock()
					body := `{"type":"error","error":{"type":"invalid_request_error","message":"livefp sink"}}`
					if _, err := fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return s
}

// liveOriginTLS issues a throwaway CA (written to caFile for
// NODE_EXTRA_CA_CERTS) and an api.anthropic.com leaf.
func liveOriginTLS(t *testing.T, caFile string) *tls.Config {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "livefp CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "api.anthropic.com"},
		DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
}

// startLiveConnectProxy wires every CONNECT to target.
func startLiveConnectProxy(t *testing.T, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				go func() {
					if n := br.Buffered(); n > 0 {
						b, _ := br.Peek(n)
						_, _ = up.Write(b)
					}
					_, _ = io.Copy(up, c)
				}()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// headerNames lists a raw head's header names in wire order and casing.
func headerNames(head string) []string {
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(head), "\r\n")[1:] {
		if name, _, ok := strings.Cut(l, ":"); ok {
			names = append(names, name)
		}
	}
	return names
}
