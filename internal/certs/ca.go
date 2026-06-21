package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
)

type BundleStatus struct {
	Mode            string
	BundlePath      string
	HasSystemCA     bool
	HasCCWRAPRoot   bool
	SystemCAWarning string
}

type Manager struct {
	paths app.Paths

	mu              sync.Mutex
	caCert          *x509.Certificate
	caKey           *ecdsa.PrivateKey
	leafCert        map[string]tls.Certificate
	bundleStatus    BundleStatus
	cachedSystemCAs []byte // populated lazily on first successful read; survives for Manager lifetime
}

func NewManager(paths app.Paths) *Manager {
	return &Manager{paths: paths, leafCert: map[string]tls.Certificate{}}
}

func (m *Manager) CertPath() string {
	return m.paths.CACertPath
}

func (m *Manager) BundlePath() string {
	return m.paths.CABundlePath
}

func (m *Manager) BundleStatus() BundleStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bundleStatus
}

func (m *Manager) EnsureCA() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paths.CALockPath == "" {
		m.paths.CALockPath = filepath.Join(m.paths.StateDir, "locks", "ca.lock")
	}
	if err := app.EnsurePaths(m.paths); err != nil {
		return err
	}
	unlock, err := acquireLock(m.paths.CALockPath)
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.ensureLocalCALocked(); err != nil {
		return err
	}
	status, err := m.ensureCompositeBundleLocked()
	if err != nil {
		return err
	}
	m.bundleStatus = status
	return nil
}

func (m *Manager) IssueServerCert(host string) (tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLocalCALocked(); err != nil {
		return tls.Certificate{}, err
	}
	if cert, ok := m.leafCert[host]; ok {
		leaf := cert.Leaf
		if leaf == nil && len(cert.Certificate) > 0 {
			leaf, _ = x509.ParseCertificate(cert.Certificate[0])
		}
		if leaf != nil && time.Until(leaf.NotAfter) > 24*time.Hour {
			return cert, nil
		}
		delete(m.leafCert, host)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().Add(-30 * time.Minute)
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             now,
		NotAfter:              now.Add(45 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
	}
	if ip := net.ParseIP(host); ip != nil {
		tpl.DNSNames = nil
		tpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, m.caCert, &leafKey.PublicKey, m.caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create leaf cert: %w", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal leaf key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	fullChain := append(append([]byte{}, leafPEM...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.caCert.Raw})...)
	cert, err := tls.X509KeyPair(fullChain, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load leaf tls keypair: %w", err)
	}
	if parsed, err := x509.ParseCertificate(der); err == nil {
		cert.Leaf = parsed
	}
	m.leafCert[host] = cert
	return cert, nil
}

func (m *Manager) ensureLocalCALocked() error {
	if m.caCert != nil && m.caKey != nil {
		return nil
	}
	certPEM, certErr := os.ReadFile(m.paths.CACertPath)
	keyPEM, keyErr := os.ReadFile(m.paths.CAKeyPath)
	if certErr == nil && keyErr == nil {
		certBlock, _ := pem.Decode(certPEM)
		if certBlock == nil {
			return fmt.Errorf("decode CA cert PEM")
		}
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			return fmt.Errorf("parse CA cert: %w", err)
		}
		keyBlock, _ := pem.Decode(keyPEM)
		if keyBlock == nil {
			return fmt.Errorf("decode CA key PEM")
		}
		key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return fmt.Errorf("parse CA key: %w", err)
		}
		m.caCert = cert
		m.caKey = key
		return nil
	}
	if certErr != nil && !os.IsNotExist(certErr) {
		return certErr
	}
	if keyErr != nil && !os.IsNotExist(keyErr) {
		return keyErr
	}
	certMissing := certErr != nil && os.IsNotExist(certErr)
	keyMissing := keyErr != nil && os.IsNotExist(keyErr)
	if certMissing != keyMissing {
		present, missing := m.paths.CACertPath, m.paths.CAKeyPath
		if certMissing {
			present, missing = missing, present
		}
		return fmt.Errorf(
			"local CA is in a partial state: %s exists but %s is missing; "+
				"remove the orphan file to regenerate a fresh root (you will need "+
				"to re-install the new root in your trust store) or restore the "+
				"missing file from backup", present, missing)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}
	now := time.Now().Add(-1 * time.Hour)
	serial, err := randSerial()
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "ccwrap local root CA",
			Organization: []string{"ccwrap"},
		},
		NotBefore:             now,
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("parse generated CA cert: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writePrivateAtomic(m.paths.CAKeyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writePrivateAtomic(m.paths.CACertPath, certPEM, 0o600); err != nil {
		return err
	}
	m.caCert = cert
	m.caKey = key
	return nil
}

func (m *Manager) ensureCompositeBundleLocked() (BundleStatus, error) {
	if m.caCert == nil {
		return BundleStatus{}, fmt.Errorf("local CA not loaded")
	}
	ccwrapPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.caCert.Raw})
	systemPEM, sysErr := m.systemCABundleLocked()
	if sysErr != nil {
		if err := writePrivateAtomic(m.paths.CABundlePath, ccwrapPEM, 0o600); err != nil {
			return BundleStatus{}, err
		}
		return BundleStatus{
			Mode:            "ccwrap_only",
			BundlePath:      m.paths.CABundlePath,
			HasSystemCA:     false,
			HasCCWRAPRoot:   len(bytes.TrimSpace(ccwrapPEM)) > 0,
			SystemCAWarning: sysErr.Error(),
		}, nil
	}
	combined := append(append([]byte{}, systemPEM...), ccwrapPEM...)
	if err := writePrivateAtomic(m.paths.CABundlePath, combined, 0o600); err != nil {
		return BundleStatus{}, err
	}
	return BundleStatus{
		Mode:          "composite",
		BundlePath:    m.paths.CABundlePath,
		HasSystemCA:   len(bytes.TrimSpace(systemPEM)) > 0,
		HasCCWRAPRoot: len(bytes.TrimSpace(ccwrapPEM)) > 0,
	}, nil
}

func containsPEMCertificate(data []byte) bool {
	return bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----"))
}

// systemCABundleLocked returns the system CA bundle PEM, fetching it
// once per Manager. macOS pays a `security find-certificate` shell-out
// (and a separate `security list-keychains` probe) per fetch — caching
// for the lifetime of the Manager turns ccwrap's many EnsureCA call sites
// (preflight, supervisor.Run, doctor) into one external invocation
// instead of many. Cache misses re-shell-out only on a previous error.
// Caller must hold m.mu.
func (m *Manager) systemCABundleLocked() ([]byte, error) {
	if len(m.cachedSystemCAs) > 0 {
		return m.cachedSystemCAs, nil
	}
	data, err := readSystemCABundle()
	if err == nil && len(data) > 0 {
		m.cachedSystemCAs = data
	}
	return data, err
}

func readSystemCABundle() ([]byte, error) {
	if override := strings.TrimSpace(os.Getenv("CCWRAP_SYSTEM_CA_BUNDLE")); override != "" {
		data, err := os.ReadFile(override)
		if err != nil {
			return nil, fmt.Errorf("read system CA bundle override %s: %w", override, err)
		}
		if !containsPEMCertificate(data) {
			return nil, fmt.Errorf("CCWRAP_SYSTEM_CA_BUNDLE override %s does not contain a PEM CERTIFICATE block", override)
		}
		return data, nil
	}
	switch runtime.GOOS {
	case "darwin":
		return readDarwinSystemCAs()
	default:
		return readLinuxSystemCAs()
	}
}

func readLinuxSystemCAs() ([]byte, error) {
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	} {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read system CA bundle %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("system CA bundle not found")
}

func readDarwinSystemCAs() ([]byte, error) {
	const timeout = 15 * time.Second
	securityBin := strings.TrimSpace(os.Getenv("CCWRAP_DARWIN_SECURITY_BIN"))
	if securityBin == "" {
		securityBin = "security"
	}
	keychains, err := darwinKeychains(securityBin, timeout)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := append([]string{"find-certificate", "-a", "-p"}, keychains...)
	cmd := exec.CommandContext(ctx, securityBin, args...)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("export macOS system CAs timed out after %s", timeout)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("export macOS system CAs: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("export macOS system CAs: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("macOS system CA export returned no certificates")
	}
	return out, nil
}

func darwinKeychains(securityBin string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityBin, "list-keychains", "-d", "user")
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("discover macOS keychain search list timed out after %s", timeout)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("discover macOS keychain search list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("discover macOS keychain search list: %w", err)
	}
	keychains := parseDarwinKeychainList(out)
	keychains = appendIfMissing(keychains, "/Library/Keychains/System.keychain")
	keychains = appendIfMissing(keychains, "/System/Library/Keychains/SystemRootCertificates.keychain")
	if len(keychains) == 0 {
		return nil, fmt.Errorf("macOS keychain search list is empty")
	}
	return keychains, nil
}

func parseDarwinKeychainList(out []byte) []string {
	var keychains []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if unquoted, err := strconv.Unquote(line); err == nil {
			line = unquoted
		}
		if line == "" {
			continue
		}
		keychains = appendIfMissing(keychains, line)
	}
	return keychains
}

func appendIfMissing(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}

func writePrivateAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

func acquireLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
