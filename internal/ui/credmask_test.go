package ui

import (
	"net/http"
	"strings"
	"testing"
)

// Migrated from cmd/ccwrap (the struct-preserving masker moved here so the
// supervisor wire path and `ccwrap capture` share one implementation and one
// deny-list — see MaskCredentialHeaders).

func TestMaskCredentialHeaders_StructurePreserving(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	h.Set("X-Api-Key", "sk-ant-XYZ123456789")
	h.Set("Content-Type", "application/json")

	got := MaskCredentialHeaders(h)

	auth := got.Get("Authorization")
	if strings.Contains(auth, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatalf("secret leaked: %q", auth)
	}
	if !strings.HasPrefix(auth, "Bearer sk-ant-") {
		t.Fatalf("scheme/prefix not preserved: %q", auth)
	}
	if !strings.Contains(auth, "redacted") {
		t.Fatalf("no redaction marker: %q", auth)
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("non-credential header must pass through unchanged")
	}
	if strings.Contains(got.Get("X-Api-Key"), "XYZ123456789") {
		t.Fatalf("x-api-key secret leaked: %q", got.Get("X-Api-Key"))
	}
}

// TestMaskCredentialValue_ShortSecretNoMajorityLeak guards the half-length cap:
// a short secret must never leak a majority of its characters via the kept
// prefix. An 8-char key keeps at most 4 chars (8/2), so its masked form must
// contain no more than 4 of the original characters.
func TestMaskCredentialValue_ShortSecretNoMajorityLeak(t *testing.T) {
	const secret = "12345678" // 8 chars
	masked := maskCredentialValue(secret)
	leaked := 0
	for i := 1; i <= len(secret); i++ {
		if strings.Contains(masked, secret[:i]) {
			leaked = i
		}
	}
	if leaked > len(secret)/2 {
		t.Fatalf("short secret leaked %d/%d chars (majority): masked=%q", leaked, len(secret), masked)
	}
	if strings.Contains(masked, secret) {
		t.Fatalf("full short secret leaked: %q", masked)
	}
}

func TestMaskCredentialHeaders_DoesNotMutateInput(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-secret")
	_ = MaskCredentialHeaders(h)
	if h.Get("Authorization") != "Bearer sk-secret" {
		t.Fatalf("input header must not be mutated")
	}
}

// TestMaskCredentialHeaders_MasksEveryDenyListHeader pins that the masker
// covers the WHOLE deny-list (the single source of truth shared with
// ClassifyHeader), so no credential header reaches the wire raw, and that
// benign headers pass through. Replaces the old cmd/ccwrap emit-path test that
// duplicated the list.
func TestMaskCredentialHeaders_MasksEveryDenyListHeader(t *testing.T) {
	h := http.Header{}
	for _, name := range CredentialDenyList() {
		h.Set(name, "SECRET-"+name)
	}
	h.Set("Content-Type", "application/json")

	got := MaskCredentialHeaders(h)

	for _, name := range CredentialDenyList() {
		v := got.Get(name)
		if strings.Contains(v, "SECRET-"+name) {
			t.Fatalf("deny-list header %q leaked its secret: %q", name, v)
		}
		if v == "" {
			t.Fatalf("deny-list header %q dropped entirely (must be masked, not removed)", name)
		}
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("benign Content-Type must pass through unchanged, got %q", got.Get("Content-Type"))
	}
}
