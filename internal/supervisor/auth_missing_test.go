package supervisor

import (
	"strings"
	"testing"
)

// TestAuthMissingMessage_CaseA — profile names a missing env var. Message
// includes the literal env name + recovery options. The msg + returned envVar
// must match so the JSON body's `env_var` field stays consistent with the
// human-readable message.
func TestAuthMissingMessage_CaseA(t *testing.T) {
	msg, envVar := authMissingMessage("local", "ANTHROPIC_AUTH_TOKEN")
	if envVar != "ANTHROPIC_AUTH_TOKEN" {
		t.Errorf("envVar = %q, want ANTHROPIC_AUTH_TOKEN", envVar)
	}
	for _, want := range []string{
		`"local"`,
		"$ANTHROPIC_AUTH_TOKEN",
		"--profile inherit-env",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Case A msg missing %q; got %q", want, msg)
		}
	}
}

// TestAuthMissingMessage_CaseB — profile has no key_env named. Message
// MUST NOT mention a `$<env>` token (no specific env to suggest) and MUST
// recommend editing the profile config.
func TestAuthMissingMessage_CaseB(t *testing.T) {
	msg, envVar := authMissingMessage("foo", "")
	if envVar != "" {
		t.Errorf("envVar = %q, want empty (Case B)", envVar)
	}
	if strings.Contains(msg, "$") {
		t.Errorf("Case B msg must not name a specific env; got %q", msg)
	}
	for _, want := range []string{
		`"foo"`,
		"no auth source configured",
		"auth.key_env",
		"--profile inherit-env",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Case B msg missing %q; got %q", want, msg)
		}
	}
}

// TestAuthMissingMessage_EmptyProfileNameFallsBack — when no profile is
// active (inherit-env mode somehow reaching this gate, theoretically
// impossible since the gate requires AuthBootstrap=Missing which doesn't
// fire in inherit-env mode — but defensive), the message uses a
// placeholder rather than emit "" as the profile name.
func TestAuthMissingMessage_EmptyProfileNameFallsBack(t *testing.T) {
	msg, _ := authMissingMessage("", "ANTHROPIC_API_KEY")
	if !strings.Contains(msg, "(no profile)") {
		t.Errorf("empty profile name should fall back to '(no profile)'; got %q", msg)
	}
}

// TestAuthMissingSuggestion — short suggestion text branches on env. Case A
// names the env; Case B suggests editing profile.
func TestAuthMissingSuggestion(t *testing.T) {
	caseA := authMissingSuggestion("ANTHROPIC_AUTH_TOKEN")
	if !strings.Contains(caseA, "$ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("Case A suggestion must name the env; got %q", caseA)
	}
	caseB := authMissingSuggestion("")
	if strings.Contains(caseB, "$") {
		t.Errorf("Case B suggestion must not name a specific env; got %q", caseB)
	}
	if !strings.Contains(caseB, "auth.key_env") {
		t.Errorf("Case B suggestion should mention auth.key_env; got %q", caseB)
	}
}
