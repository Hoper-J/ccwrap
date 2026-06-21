package supervisor

import (
	"net/http"
	"testing"
)

// TestApplyAuthOverride_NilOverride_PreservesHeaders documents the
// empirical verification of the auth-override abandonment rationale.
//
// Claim under verification: "ccwrap's default OAuth+third-party flow does
// NOT leak the user's OAuth bearer to the third-party upstream."
//
// This test pins ONE half of the proof: applyAuthOverride(h, nil) is a
// no-op. It does NOT strip Authorization/X-API-Key/etc. — header values
// pass through verbatim.
//
// That means if the proxy reaches applyAuthOverride with a nil override
// AND applyAuth=true, the original headers reach the upstream. So why
// is there no leak in default operation?
//
// The OTHER half of the proof lives in
// TestHandleAnthropicMITM_RefusesWhenAuthMissing (proxy_test.go):
// when ap.authBootstrap==Missing (third-party route + no ccwrap credential),
// maybeRefuseAuthMissing intercepts BEFORE applyAuthOverride is reached
// and returns 502. Upstream receives ZERO requests.
//
// Together: in the default OAuth-mode + third-party-profile scenario:
//  1. preflight.hiddenAuthContract returns AuthPolicyCCPOverrideFailClosed
//     + AuthBootstrap=Missing (preflight.go)
//  2. proxy's maybeRefuseAuthMissing fires, returns 502
//  3. applyAuthOverride is never reached
//  4. The user's OAuth bearer does NOT leak
//
// The ONLY documented leak path is AuthPolicyUnsafePassthrough — opt-in
// via the --allow-auth-passthrough-to-third-party launch flag. In that
// mode: AuthBootstrap=NotNeeded (preflight.go), so
// maybeRefuseAuthMissing does NOT fire, applyAuthOverride(nil) preserves
// headers, and the OAuth bearer DOES reach the third-party. By name
// ("unsafe") and by the flag's explicit opt-in, this is acceptable.
func TestApplyAuthOverride_NilOverride_PreservesHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-oat-fake-oauth")
	headers.Set("X-API-Key", "fake-api-key")
	headers.Set("X-Custom", "preserved")

	applyAuthOverride(headers, nil)

	// Critical: with nil override, no header is touched (early return).
	if got := headers.Get("Authorization"); got != "Bearer sk-ant-oat-fake-oauth" {
		t.Errorf("applyAuthOverride(nil) modified Authorization: got %q, want preserved", got)
	}
	if got := headers.Get("X-API-Key"); got != "fake-api-key" {
		t.Errorf("applyAuthOverride(nil) modified X-API-Key: got %q", got)
	}
	if got := headers.Get("X-Custom"); got != "preserved" {
		t.Errorf("applyAuthOverride(nil) modified non-auth header: got %q", got)
	}
}
