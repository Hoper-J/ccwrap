package supervisor

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"

	utls "github.com/refraction-networking/utls"
)

// fingerprintOrigin is a TLS origin that records the raw ClientHello bytes it
// receives on each accepted connection (so the test can fingerprint what
// actually arrived on the wire), then serves a plain HTTP/1.1 200 response.
type fingerprintOrigin struct {
	addr string            // loopback host:port the listener is bound to
	ca   *x509.Certificate // throwaway CA that signed the leaf (trust via nativeRootsForTest)
	mu   sync.Mutex        // guards received
	rcvd [][]byte          // raw ClientHello bytes per accepted conn
}

func (o *fingerprintOrigin) received() [][]byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([][]byte, len(o.rcvd))
	copy(out, o.rcvd)
	return out
}

// startFingerprintOrigin starts a TLS origin whose leaf is valid for certHost
// (plus a loopback IP SAN). Its accept loop peeks the raw ClientHello via the
// production peekClientHello helper, records it, then hands the replay conn to
// tls.Server and answers HTTP/1.1 with "200 OK\r\n\r\nhello".
func startFingerprintOrigin(t *testing.T, certHost string) *fingerprintOrigin {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "native-tls e2e CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: certHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{certHost},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	srvCert := tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		NextProtos:   []string{"http/1.1"},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("origin listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	o := &fingerprintOrigin{addr: ln.Addr().String(), ca: caCert}

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Peek the raw ClientHello off the wire (record framing
				// preserved -- utls.Fingerprinter wants raw records), record it,
				// then continue the handshake on the replay conn.
				hello, replay, err := peekClientHello(c)
				if err != nil {
					return
				}
				o.mu.Lock()
				o.rcvd = append(o.rcvd, append([]byte(nil), hello...))
				o.mu.Unlock()

				tc := tls.Server(replay, tlsCfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				// Minimal HTTP/1.1: read the request line + headers, then answer.
				br := bufio.NewReader(tc)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				body := "hello"
				fmt.Fprintf(tc, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}(raw)
		}
	}()

	return o
}

// connectProxy is a minimal HTTP CONNECT egress proxy. It handles
// "CONNECT host:port" by dialing the supplied target (the real origin addr,
// regardless of the requested host) and piping bytes both ways, recording the
// CONNECT host:port it was asked to reach.
type connectProxy struct {
	addr   string // loopback host:port the proxy listens on
	target string // real origin addr every CONNECT is wired to
	mu     sync.Mutex
	seen   []string // CONNECT targets observed
}

func (p *connectProxy) connects() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.seen))
	copy(out, p.seen)
	return out
}

// startConnectProxy starts the CONNECT proxy, wiring every CONNECT to target.
func startConnectProxy(t *testing.T, target string) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	p := &connectProxy{addr: ln.Addr().String(), target: target}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(client net.Conn) {
				br := bufio.NewReader(client)
				req, err := http.ReadRequest(br)
				if err != nil {
					_ = client.Close()
					return
				}
				if req.Method != http.MethodConnect {
					_, _ = io.WriteString(client, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
					_ = client.Close()
					return
				}
				p.mu.Lock()
				p.seen = append(p.seen, req.Host) // CONNECT host:port
				p.mu.Unlock()

				upstream, err := net.Dial("tcp", p.target)
				if err != nil {
					_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					_ = client.Close()
					return
				}
				if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					_ = upstream.Close()
					_ = client.Close()
					return
				}
				// Pipe bytes both ways. Drain anything the client already
				// buffered in br before the raw splice begins.
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					if n := br.Buffered(); n > 0 {
						buf := make([]byte, n)
						_, _ = io.ReadFull(br, buf)
						_, _ = upstream.Write(buf)
					}
					_, _ = io.Copy(upstream, client)
					if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
				}()
				go func() {
					defer wg.Done()
					_, _ = io.Copy(client, upstream)
					if cw, ok := client.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
				}()
				wg.Wait()
				_ = upstream.Close()
				_ = client.Close()
			}(c)
		}
	}()

	return p
}

// extIDs returns the wire extension IDs of a fingerprinted ClientHello spec, in
// order, using utls's own Writer to obtain each typed extension's two-byte ID.
// Comparing these lists proves the received hello carried the same extension
// set/ordering as the stored one (parity beyond just cipher suites).
func extIDs(t *testing.T, spec *utls.ClientHelloSpec) []uint16 {
	t.Helper()
	ids := make([]uint16, 0, len(spec.Extensions))
	for _, ext := range spec.Extensions {
		// GREASE extensions are normalized to a single placeholder ID so the
		// comparison is about extension *kinds*, not the random GREASE value
		// (utls's Fingerprinter already rewrites GREASE to a placeholder on
		// Write, so received and stored agree regardless).
		if _, ok := ext.(*utls.UtlsGREASEExtension); ok {
			ids = append(ids, 0x0a0a)
			continue
		}
		// The fingerprinter strips the SNI value (it's user-controlled, not part
		// of the fingerprint), so *SNIExtension serializes to Len()==0. Its wire
		// ID is fixed at 0x0000 (server_name); pin it directly.
		if _, ok := ext.(*utls.SNIExtension); ok {
			ids = append(ids, 0x0000)
			continue
		}
		// utls extensions serialize themselves via Read, but require a buffer
		// sized to the full extension wire length (Len()); the first two bytes
		// are the extension ID.
		buf := make([]byte, ext.Len())
		if len(buf) < 2 {
			t.Fatalf("extension %T reports Len()=%d (< 2)", ext, ext.Len())
		}
		if _, err := ext.Read(buf); err != nil && err != io.EOF {
			t.Fatalf("extension %T Read: %v", ext, err)
		}
		ids = append(ids, uint16(buf[0])<<8|uint16(buf[1]))
	}
	return ids
}

func fingerprint(t *testing.T, raw []byte) *utls.ClientHelloSpec {
	t.Helper()
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(raw)
	if err != nil {
		t.Fatalf("FingerprintClientHello: %v", err)
	}
	return spec
}

func alpnOf(spec *utls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			return a.AlpnProtocols
		}
	}
	return nil
}

// peetFieldsOf pulls the peetprint-relevant extension VALUES (not just IDs) out
// of a fingerprinted spec, in wire order: supported_groups (curves),
// signature_algorithms, supported_versions, psk_key_exchange_modes. peetprint
// hashes these alongside ciphers/extension-IDs/ALPN (which the e2e already
// compares), so asserting these equal between the stored template and each
// received hello proves ccwrap's OUTBOUND mirror preserves the displayed
// peetprint — the shown fingerprint is what actually goes on the wire. Values
// are flattened to []uint16 (modes widened from uint8) so a single equalU16
// compares each list.
func peetFieldsOf(spec *utls.ClientHelloSpec) (curves, sigAlgs, versions, pskModes []uint16) {
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.SupportedCurvesExtension:
			for _, c := range e.Curves {
				curves = append(curves, uint16(c))
			}
		case *utls.SignatureAlgorithmsExtension:
			for _, s := range e.SupportedSignatureAlgorithms {
				sigAlgs = append(sigAlgs, uint16(s))
			}
		case *utls.SupportedVersionsExtension:
			versions = append(versions, e.Versions...)
		case *utls.PSKKeyExchangeModesExtension:
			for _, m := range e.Modes {
				pskModes = append(pskModes, uint16(m))
			}
		}
	}
	return curves, sigAlgs, versions, pskModes
}

func TestNativeTLSE2E(t *testing.T) {
	// Origin host: a non-loopback *name* so egress.DialContext's unconditional
	// loopback bypass floor (127.0.0.0/8 in defaultBypassFloor) does NOT bypass
	// the proxy. The proxy wires every CONNECT to the origin's real loopback
	// addr; the leaf cert carries both this DNS SAN and a 127.0.0.1 IP SAN.
	const originHost = "nativetls-origin.test"

	origin := startFingerprintOrigin(t, originHost)
	proxy := startConnectProxy(t, origin.addr)

	_, originPort, err := net.SplitHostPort(origin.addr)
	if err != nil {
		t.Fatalf("split origin addr: %v", err)
	}
	originHostPort := net.JoinHostPort(originHost, originPort)
	originURL := "https://" + originHostPort + "/"

	// Trust the origin CA on the native-TLS path.
	pool := x509.NewCertPool()
	pool.AddCert(origin.ca)
	nativeRootsForTest = pool
	t.Cleanup(func() { nativeRootsForTest = nil })

	// Egress config: an HTTP-proxy egress pointing both HTTP and HTTPS at the
	// local CONNECT proxy (the same shape egress.Resolve emits for an explicit
	// proxy flag). NoProxy left empty -> the .test name routes through the proxy.
	proxyURL := "http://" + proxy.addr
	egCfg := model.EgressConfig{
		Mode:       "explicit",
		HTTPProxy:  proxyURL,
		HTTPSProxy: proxyURL,
		Source:     "native-tls-e2e",
		Summary:    proxyURL,
	}

	// Store CC's captured ClientHello (utls-built stand-in, record-framed).
	stored := standInHello(t)
	storedCopy := append([]byte(nil), stored...)

	sp := &sessionProxy{
		supervisor: &Supervisor{
			transport: &http.Transport{ForceAttemptHTTP2: true},
		},
		session:          &sessionState{},
		transports:       map[string]*http.Transport{},
		nativeTransports: map[string]*http.Transport{},
	}
	sp.session.mirroredHelloRaw.Store(&storedCopy)

	tr := sp.nativeUpstreamTransport(egCfg)

	// 3 concurrent GETs through the native transport + egress proxy. Concurrency
	// is the -race proof: each dial re-parses a fresh utls spec, no shared state.
	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
			resp, err := client.Get(originURL)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			b, rerr := io.ReadAll(resp.Body)
			if rerr != nil {
				errs[i] = rerr
				return
			}
			codes[i] = resp.StatusCode
			if string(b) != "hello" {
				errs[i] = fmt.Errorf("body=%q want %q", string(b), "hello")
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if codes[i] != 200 {
			t.Fatalf("request %d: status %d, want 200", i, codes[i])
		}
	}

	// Egress traversed: the proxy must have recorded a CONNECT to the origin
	// host:port (not a direct dial).
	connects := proxy.connects()
	if len(connects) < n {
		t.Fatalf("proxy saw %d CONNECTs, want >= %d (egress bypassed?)", len(connects), n)
	}
	sawOrigin := false
	for _, c := range connects {
		if c == originHostPort {
			sawOrigin = true
			break
		}
	}
	if !sawOrigin {
		t.Fatalf("proxy CONNECTs %v did not include %q", connects, originHostPort)
	}

	// Parity: fingerprint the RECEIVED hello (at the origin) and the STORED hello
	// and assert equal cipher suites + equal extension-ID lists + equal ALPN.
	rcvd := origin.received()
	if len(rcvd) == 0 {
		t.Fatal("origin recorded no ClientHello")
	}
	storedSpec := fingerprint(t, stored)
	for idx, got := range rcvd {
		recvSpec := fingerprint(t, got)

		if !equalU16(recvSpec.CipherSuites, storedSpec.CipherSuites) {
			t.Fatalf("conn %d: cipher suites differ\n received=%v\n stored=%v", idx, recvSpec.CipherSuites, storedSpec.CipherSuites)
		}
		gotExts := extIDs(t, recvSpec)
		wantExts := extIDs(t, storedSpec)
		if !equalU16(gotExts, wantExts) {
			t.Fatalf("conn %d: extension IDs differ\n received=%v (n=%d)\n stored=%v (n=%d)", idx, gotExts, len(gotExts), wantExts, len(wantExts))
		}
		if !equalStr(alpnOf(recvSpec), alpnOf(storedSpec)) {
			t.Fatalf("conn %d: ALPN differs\n received=%v\n stored=%v", idx, alpnOf(recvSpec), alpnOf(storedSpec))
		}

		// Outbound peetprint-order parity: peetprint hashes the ordered VALUES of
		// supported_groups, signature_algorithms, supported_versions, and
		// psk_key_exchange_modes (beyond the ciphers/extension-IDs/ALPN already
		// checked above). Assert each list is byte-for-byte preserved between the
		// stored template and what arrived on the wire, so the displayed peetprint
		// equals what ccwrap actually sends.
		rCurves, rSig, rVers, rPSK := peetFieldsOf(recvSpec)
		sCurves, sSig, sVers, sPSK := peetFieldsOf(storedSpec)
		if !equalU16(rCurves, sCurves) {
			t.Fatalf("conn %d: supported_groups (curves) differ\n received=%v\n stored=%v", idx, rCurves, sCurves)
		}
		if !equalU16(rSig, sSig) {
			t.Fatalf("conn %d: signature_algorithms differ\n received=%v\n stored=%v", idx, rSig, sSig)
		}
		if !equalU16(rVers, sVers) {
			t.Fatalf("conn %d: supported_versions differ\n received=%v\n stored=%v", idx, rVers, sVers)
		}
		if !equalU16(rPSK, sPSK) {
			t.Fatalf("conn %d: psk_key_exchange_modes differ\n received=%v\n stored=%v", idx, rPSK, sPSK)
		}
	}

	// Fail-closed: force the utls mirror to fail and prove the request is BLOCKED
	// — no degraded stdlib (Go-fingerprinted) handshake reaches the origin. The
	// dialer returns an error, so the round-trip fails rather than de-anonymizing.
	forceNativeTLSFail = true
	t.Cleanup(func() { forceNativeTLSFail = false })

	fbClient := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	if resp, err := fbClient.Get(originURL); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("fail-closed: a forced mirror failure must fail the request, got status %d", resp.StatusCode)
	}
}

func equalU16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
