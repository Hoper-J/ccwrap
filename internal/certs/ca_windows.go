package certs

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/crypto/x509roots/fallback/bundle"
	"golang.org/x/sys/windows"
)

// cryptENotFound ends a certificate store enumeration, and says a
// certificate has no purposes set.
const cryptENotFound = syscall.Errno(0x80092004)

// serverAuthOID is the purpose "Server Authentication".
const serverAuthOID = "1.3.6.1.5.5.7.3.1"

var procCertGetEnhancedKeyUsage = windows.NewLazySystemDLL("crypt32.dll").NewProc("CertGetEnhancedKeyUsage")

// readPlatformSystemCAs returns, as PEM, the roots tools in a session trust
// for TLS: Mozilla's list, which pip and the OpenSSL builds of git carry,
// and the roots in this machine's Windows stores that Windows trusts for
// server authentication, which brings in the roots a company deployed.
// Windows fetches the rest of its roots only when something first needs
// one, so its stores alone miss most of them.
func readPlatformSystemCAs() ([]byte, error) {
	var out bytes.Buffer
	seen := map[string]bool{}
	add := func(der []byte) error {
		if seen[string(der)] {
			return nil
		}
		seen[string(der)] = true
		return pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	for root := range bundle.Roots() {
		// A distrust date is a constraint PEM cannot carry.
		if root.Constraint != nil {
			continue
		}
		if err := add(root.Certificate); err != nil {
			return nil, err
		}
	}
	disallowed := map[string]bool{}
	if err := eachStoreCert("Disallowed", func(der []byte, _ *windows.CertContext) error {
		disallowed[string(der)] = true
		return nil
	}); err != nil {
		return nil, err
	}
	for _, name := range []string{"ROOT", "AuthRoot"} {
		if err := eachStoreCert(name, func(der []byte, cert *windows.CertContext) error {
			if disallowed[string(der)] || !trustedForServerAuth(cert) {
				return nil
			}
			return add(der)
		}); err != nil {
			return nil, err
		}
	}
	if out.Len() == 0 {
		return nil, errors.New("no root certificates found")
	}
	return out.Bytes(), nil
}

// eachStoreCert calls f with each certificate in the Windows system store
// name, the current user's view of it, which includes the machine's.
func eachStoreCert(name string, f func(der []byte, cert *windows.CertContext) error) error {
	store, err := windows.CertOpenSystemStore(0, windows.StringToUTF16Ptr(name))
	if err != nil {
		return fmt.Errorf("open Windows %s certificate store: %w", name, err)
	}
	defer windows.CertCloseStore(store, 0)
	var cert *windows.CertContext
	for {
		// Passing the previous certificate frees it.
		cert, err = windows.CertEnumCertificatesInStore(store, cert)
		if errors.Is(err, cryptENotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Windows %s certificate store: %w", name, err)
		}
		if err := f(unsafe.Slice(cert.EncodedCert, cert.Length), cert); err != nil {
			windows.CertFreeCertificateContext(cert)
			return err
		}
	}
}

// trustedForServerAuth reports whether Windows trusts cert to anchor TLS
// server certificates: the purposes its store gives the certificate, when
// it gives any, include server authentication.
func trustedForServerAuth(cert *windows.CertContext) bool {
	var size uint32
	r, _, err := procCertGetEnhancedKeyUsage.Call(uintptr(unsafe.Pointer(cert)), windows.CERT_FIND_PROP_ONLY_ENHKEY_USAGE_FLAG, 0, uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return errors.Is(err, cryptENotFound) // no purposes set: every purpose
	}
	buf := make([]byte, size)
	usage := (*windows.CertEnhKeyUsage)(unsafe.Pointer(&buf[0]))
	r, _, err = procCertGetEnhancedKeyUsage.Call(uintptr(unsafe.Pointer(cert)), windows.CERT_FIND_PROP_ONLY_ENHKEY_USAGE_FLAG, uintptr(unsafe.Pointer(usage)), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return errors.Is(err, cryptENotFound)
	}
	if usage.Length == 0 {
		// An empty list is every purpose when the call also reports
		// "not found", and no purpose otherwise.
		return errors.Is(err, cryptENotFound)
	}
	oids := unsafe.Slice(usage.UsageIdentifiers, usage.Length)
	for _, oid := range oids {
		if windows.BytePtrToString(oid) == serverAuthOID {
			return true
		}
	}
	return false
}
