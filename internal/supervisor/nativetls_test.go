package supervisor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// standInHello builds a real Chrome ClientHello via utls and record-frames it.
// Real undici testdata arrives in a later task; a utls-built hello is a valid
// stand-in for exercising upstream verification.
func standInHello(t *testing.T) []byte {
	t.Helper()
	stub, _ := net.Pipe()
	defer stub.Close()
	uc := utls.UClient(stub, &utls.Config{ServerName: "api.anthropic.com"}, utls.HelloChrome_Auto)
	if err := uc.BuildHandshakeState(); err != nil {
		t.Fatalf("BuildHandshakeState: %v", err)
	}
	body := uc.HandshakeState.Hello.Raw // incl 4-byte handshake header
	if len(body) < 4 {
		t.Fatalf("utls produced an implausibly short ClientHello (%d bytes)", len(body))
	}
	// Hello.Raw already carries the 4-byte handshake header that
	// buildClientHelloRecords re-adds; slice [4:] to avoid a double header.
	return buildClientHelloRecords(body[4:], 4096)
}

// startTLSOrigin generates a throwaway CA + a leaf valid for certHost, starts a
// TLS listener that accepts+handshakes+closes one connection per accept, and
// returns the listener address and the CA cert.
func startTLSOrigin(t *testing.T, certHost string) (addr string, ca *x509.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "native-tls test CA"},
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
		// The origin listens on loopback, so nativeTLSDial derives host
		// "127.0.0.1" from the dial addr and verifies against it. Include the
		// loopback IP SAN so the real-verification mirror + fallback paths pass
		// while the certHost DNS SAN keeps name-mismatch cases honest.
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}

	srvCert := tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		// Advertise http/1.1 only so the fallback path's NextProtos:["http/1.1"]
		// negotiation assertion in nativetls_dial_test.go is meaningful (never h2).
		NextProtos: []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Drive the handshake to completion, then close.
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
				_ = c.Close()
			}(c)
		}
	}()

	return ln.Addr().String(), caCert
}

// genLeaf generates a throwaway CA and a leaf certificate valid for host (DNS
// SAN), returning both as parsed *x509.Certificate. It mirrors startTLSOrigin's
// cert-gen approach but yields parsed certs so a test can drive
// utlsCfg().VerifyConnection directly with a crafted utls.ConnectionState.
func genLeaf(t *testing.T, host string) (leaf *x509.Certificate, ca *x509.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "native-tls test CA"},
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
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leafCert, caCert
}

// poolTrusting returns an x509.CertPool that trusts ca.
func poolTrusting(ca *x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca)
	return p
}

// TestUtlsCfgVerifyConnectionEnforces isolates utlsCfg's MANDATORY
// VerifyConnection as THE enforcer (the spec's central claim: utls is a lagging
// crypto/tls fork, so we do not trust its internal InsecureSkipVerify:false —
// OUR VerifyConnection independently re-verifies chain+hostname against the given
// roots). It drives the closure DIRECTLY with crafted utls.ConnectionStates,
// proving it accepts a valid chain+host and DENIES on both hostname mismatch and
// an untrusted root. It would go RED if VerifyConnection were stubbed to
// `return nil`.
func TestUtlsCfgVerifyConnectionEnforces(t *testing.T) {
	leaf, ca := genLeaf(t, "api.anthropic.com")
	cs := utls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}

	// Valid chain + matching host -> accept.
	if err := utlsCfg("api.anthropic.com", poolTrusting(ca)).VerifyConnection(cs); err != nil {
		t.Fatalf("valid chain+host must be accepted by VerifyConnection, got %v", err)
	}

	// Hostname mismatch (cert valid for api.anthropic.com, ServerName is another)
	// -> deny. Proves VerifyConnection checks DNSName itself, not utls internals.
	if err := utlsCfg("wrong.example.com", poolTrusting(ca)).VerifyConnection(cs); err == nil {
		t.Fatal("VerifyConnection must DENY on hostname mismatch (independent DNSName check)")
	}

	// Untrusted root (empty pool) -> deny. Proves VerifyConnection checks the
	// chain against the given roots itself.
	if err := utlsCfg("api.anthropic.com", x509.NewCertPool()).VerifyConnection(cs); err == nil {
		t.Fatal("VerifyConnection must DENY when the chain does not verify against the given roots")
	}
}

func TestNativeTLSVerification(t *testing.T) {
	hello := standInHello(t) // a real ClientHello (utls-built; real undici testdata lands later)

	// A TLS origin whose leaf is signed by a throwaway CA, valid for "api.anthropic.com".
	origin, originCA := startTLSOrigin(t, "api.anthropic.com")
	emptyPool := x509.NewCertPool()

	dialVerifyOnly := func(serverName string, roots *x509.CertPool) error {
		raw, err := net.Dial("tcp", origin)
		if err != nil {
			t.Fatalf("tcp dial: %v", err)
		}
		defer raw.Close()
		return nativeUTLSHandshake(raw, serverName, roots, hello)
	}

	// (a) untrusted CA -> verification failure on the utls path.
	if err := dialVerifyOnly("api.anthropic.com", emptyPool); err == nil {
		t.Fatal("expected x509 verification failure with an untrusted CA pool")
	}
	// (b) structural: utlsCfg must never weaken verification.
	cfg := utlsCfg("api.anthropic.com", emptyPool)
	if cfg.InsecureSkipVerify {
		t.Fatal("utlsCfg must never set InsecureSkipVerify=true")
	}
	if cfg.VerifyConnection == nil {
		t.Fatal("utlsCfg must set a mandatory VerifyConnection")
	}
	// (c) positive control: trust the CA -> success.
	trust := x509.NewCertPool()
	trust.AddCert(originCA)
	if err := dialVerifyOnly("api.anthropic.com", trust); err != nil {
		t.Fatalf("expected success with the trusted CA, got %v", err)
	}
	// (d) ServerName mismatch (valid for api.anthropic.com, dialed as another name) -> fail.
	if err := dialVerifyOnly("wrong.example.com", trust); err == nil {
		t.Fatal("expected hostname verification failure when ServerName != cert SAN")
	}
}
