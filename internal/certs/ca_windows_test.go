package certs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/crypto/x509roots/fallback/bundle"
	"golang.org/x/sys/windows"
)

func TestReadPlatformSystemCAsExportsWindowsRoots(t *testing.T) {
	data, err := readPlatformSystemCAs()
	if err != nil {
		t.Fatal(err)
	}
	parsed, unparsed := 0, 0
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			unparsed++ // some old roots break Go's stricter parsing
			continue
		}
		parsed++
	}
	t.Logf("exported %d roots (%d Go cannot parse)", parsed+unparsed, unparsed)
	if parsed == 0 {
		t.Fatal("no usable root certificates exported")
	}
}

// Every root of Mozilla's list that PEM can carry is in the export, next to
// the machine's own roots.
func TestReadPlatformSystemCAsIncludesMozillasRoots(t *testing.T) {
	data, err := readPlatformSystemCAs()
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if exported[string(block.Bytes)] {
			t.Fatal("a root is exported twice")
		}
		exported[string(block.Bytes)] = true
	}
	mozilla := 0
	for root := range bundle.Roots() {
		if root.Constraint != nil {
			continue
		}
		mozilla++
		if !exported[string(root.Certificate)] {
			t.Fatal("a root of Mozilla's list is missing from the export")
		}
	}
	if mozilla < 100 {
		t.Fatalf("Mozilla's list has only %d roots", mozilla)
	}
	t.Logf("exported %d roots, %d of them Mozilla's", len(exported), mozilla)
}

var procCertSetCertificateContextProperty = windows.NewLazySystemDLL("crypt32.dll").NewProc("CertSetCertificateContextProperty")

// The purposes Windows sets for a root decide: none set means every
// purpose, a list must name server authentication.
func TestTrustedForServerAuth(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	const enhKeyUsagePropID = 9 // CERT_ENHKEY_USAGE_PROP_ID
	serverAuth := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}
	codeSigning := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 3}
	for _, tc := range []struct {
		name     string
		purposes []asn1.ObjectIdentifier // nil: none set
		want     bool
	}{
		{"none set", nil, true},
		{"server authentication", []asn1.ObjectIdentifier{codeSigning, serverAuth}, true},
		{"code signing only", []asn1.ObjectIdentifier{codeSigning}, false},
		{"no purpose", []asn1.ObjectIdentifier{}, false},
	} {
		cert, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &der[0], uint32(len(der)))
		if err != nil {
			t.Fatal(err)
		}
		if tc.purposes != nil {
			encoded, err := asn1.Marshal(tc.purposes)
			if err != nil {
				t.Fatal(err)
			}
			blob := windows.CryptDataBlob{Size: uint32(len(encoded)), Data: &encoded[0]}
			if r, _, err := procCertSetCertificateContextProperty.Call(uintptr(unsafe.Pointer(cert)), enhKeyUsagePropID, 0, uintptr(unsafe.Pointer(&blob))); r == 0 {
				t.Fatalf("%s: set purposes: %v", tc.name, err)
			}
		}
		if got := trustedForServerAuth(cert); got != tc.want {
			t.Errorf("%s: trustedForServerAuth = %v, want %v", tc.name, got, tc.want)
		}
		windows.CertFreeCertificateContext(cert)
	}
}

// A program reading the bundle, without letting it be replaced, does not
// stop a launch: the bundle stays as it was until the next launch after.
func TestEnsureCAKeepsTheBundleAProgramHasOpen(t *testing.T) {
	paths, system := testCAPaths(t, t.TempDir(), "-----BEGIN CERTIFICATE-----\nOLDROOT\n-----END CERTIFICATE-----\n")
	if err := NewManager(paths).EnsureCA(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.CABundlePath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(paths.CABundlePath), windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(system, []byte("-----BEGIN CERTIFICATE-----\nNEWROOT\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(paths)
	err = mgr.EnsureCA()
	stale := mgr.BundleStatus().StaleWarning
	after, _ := os.ReadFile(paths.CABundlePath)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatalf("EnsureCA while a program has the bundle open: %v", err)
	}
	if stale == "" || !bytes.Equal(before, after) {
		t.Fatalf("stale warning %q, bundle changed: %v", stale, !bytes.Equal(before, after))
	}
	mgr = NewManager(paths)
	if err := mgr.EnsureCA(); err != nil || mgr.BundleStatus().StaleWarning != "" {
		t.Fatalf("after the program let go: %v, %q", err, mgr.BundleStatus().StaleWarning)
	}
	if now, _ := os.ReadFile(paths.CABundlePath); !strings.Contains(string(now), "NEWROOT") {
		t.Fatal("the bundle was not updated once free")
	}
}
