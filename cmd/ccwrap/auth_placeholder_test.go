package main

import (
	"strings"
	"testing"
)

// The placeholder is deliberately stable per profile (NOT per session):
// Claude Code's interactive launch stores an approval fingerprint of
// env ANTHROPIC_API_KEY in customApiKeyResponses, so a per-session
// random value re-triggers the "Detected a custom API key" dialog on
// every launch, while a stable one is approved once per profile.
func TestAuthPlaceholderStablePerProfile(t *testing.T) {
	a := newAuthPlaceholder("example")
	b := newAuthPlaceholder("example")
	if a != b {
		t.Fatalf("placeholder not stable for same profile: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "ccwrap-placeholder-") {
		t.Fatalf("placeholder lost its recognizable prefix: %q", a)
	}
}

func TestAuthPlaceholderDistinctAcrossProfiles(t *testing.T) {
	if newAuthPlaceholder("example") == newAuthPlaceholder("deepseek") {
		t.Fatal("different profiles must yield different placeholders")
	}
}

// Env-auth launches (no active profile) share one stable value too, and
// it must not collide with a profile literally named "env".
func TestAuthPlaceholderEmptyProfileNameStable(t *testing.T) {
	a := newAuthPlaceholder("")
	b := newAuthPlaceholder("  ")
	if a != b {
		t.Fatalf("no-profile placeholder not stable: %q vs %q", a, b)
	}
	if a == newAuthPlaceholder("env") {
		t.Fatal("no-profile placeholder collides with a profile named env")
	}
}

// The child echoes the placeholder back in x-api-key / Authorization
// headers to the loopback proxy, so the value must stay within header-safe
// token characters no matter what the profile is named. Sanitized names
// that fold to the same string must still yield distinct values.
func TestAuthPlaceholderHeaderSafeAndCollisionFree(t *testing.T) {
	for _, name := range []string{"我的 网关", "a b", "a_b", "tab\there"} {
		v := newAuthPlaceholder(name)
		for _, r := range v {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
			if !ok {
				t.Fatalf("placeholder for %q contains header-unsafe rune %q: %q", name, r, v)
			}
		}
	}
	if newAuthPlaceholder("a b") == newAuthPlaceholder("ab") {
		t.Fatal("sanitization collision: \"a b\" and \"ab\" produced the same placeholder")
	}
}
