package supervisor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// TestApplyAuthOverride_StripsAllCredentialShapes — every header name
// the strip list at routeresolve.go::applyAuthOverride covers must be
// removed from the upstream-bound header set when an override is
// applied. The inspect-side deny list must cover the same credential
// shapes the wire strip already handles: Api-Key, X-Apikey,
// X-Gateway-Key, X-LitellM-Key, X-Provider-Key, X-Provider-Token. The
// test also asserts each strip-list name is classified HeaderCredential
// by the inspect renderer so the two lists cannot drift out of sync
// undetected.
func TestApplyAuthOverride_StripsAllCredentialShapes(t *testing.T) {
	credentialShapes := []string{
		"Authorization",
		"Proxy-Authorization",
		"X-API-Key",
		"X-Api-Key",
		"X-Apikey",
		"Api-Key",
		"X-Gateway-Key",
		"X-LitellM-Key",
		"X-Provider-Key",
		"X-Provider-Token",
		"Cookie",
	}

	// 1. applyAuthOverride must strip every shape from the wire.
	h := http.Header{}
	for _, name := range credentialShapes {
		h.Set(name, "SECRET-"+name)
	}
	h.Set("Anthropic-Version", "2023-06-01") // structural header, must survive
	applyAuthOverride(h, &model.AuthOverride{
		HeaderName:  "Authorization",
		HeaderValue: "Bearer ccwrap-injected",
		Source:      model.AuthSourceCCWRAPUpstreamAPIKey,
	})
	for _, name := range credentialShapes {
		if name == "Authorization" {
			continue // override sets Authorization to a new value
		}
		if got := h.Get(name); got != "" {
			t.Errorf("applyAuthOverride did not strip %q (got %q) — defeats hidden-mode credential separation", name, got)
		}
	}
	if h.Get("Authorization") != "Bearer ccwrap-injected" {
		t.Errorf("Authorization replacement missing")
	}
	if h.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("structural header lost in cred strip")
	}

	// 2. Every strip-list shape must classify as HeaderCredential in the
	// inspect renderer so the captured headers don't leak through /recent.
	// This locks the two lists together — adding to one without the
	// other fails CI.
	for _, name := range credentialShapes {
		if got := ui.ClassifyHeader(name); got != ui.HeaderCredential {
			t.Errorf("ClassifyHeader(%q) = %v, want HeaderCredential (header is stripped from upstream but would render raw in inspect drawer)", name, got)
		}
	}
}

// TestApplyAuthOverride_StripListMatchesDenyList — the REVERSE direction
// of the strip/deny-list alignment: every header redacted in the inspect
// drawer (ui.CredentialDenyList) must also be stripped from the upstream
// wire by applyAuthOverride. Without this, a credential is hidden in
// /recent but FORWARDED to the upstream — a defense-in-depth gap. Cookie
// is one such case: it is redacted in the inspect drawer, so it must
// also be stripped from the wire.
//
// The matrix logic: seed an http.Header with one header for every
// name in CredentialDenyList; run applyAuthOverride; assert every
// entry is gone (except Authorization, which override replaces).
func TestApplyAuthOverride_StripListMatchesDenyList(t *testing.T) {
	h := http.Header{}
	for _, name := range ui.CredentialDenyList() {
		h.Set(name, "SECRET-"+name)
	}
	applyAuthOverride(h, &model.AuthOverride{
		HeaderName:  "Authorization",
		HeaderValue: "Bearer ccwrap-injected",
		Source:      model.AuthSourceCCWRAPUpstreamAPIKey,
	})
	for _, name := range ui.CredentialDenyList() {
		if strings.EqualFold(name, "authorization") {
			continue // override sets a new value at this name
		}
		if got := h.Get(name); got != "" {
			t.Errorf("CredentialDenyList includes %q (redacted in inspect) but applyAuthOverride did NOT strip it (got %q) — secret leaks to upstream while being hidden in /recent", name, got)
		}
	}
}

// TestApplyAuthOverride_LowercaseAndMixedCase — http.Header.Del is
// already case-insensitive (CanonicalMIMEHeaderKey), but verify the
// strip list works for clients that send creds in any case form.
func TestApplyAuthOverride_LowercaseAndMixedCase(t *testing.T) {
	h := http.Header{}
	// Use SetCanonical-bypassing assignments so the keys land literally.
	h["authorization"] = []string{"Bearer secret"}
	h["x-gateway-key"] = []string{"gw-secret"}
	h["X-PROVIDER-KEY"] = []string{"prov-secret"}
	applyAuthOverride(h, &model.AuthOverride{
		HeaderName:  "Authorization",
		HeaderValue: "Bearer new",
		Source:      model.AuthSourceCCWRAPUpstreamAPIKey,
	})
	// http.Header.Get/Del normalize so any case form should be stripped.
	for _, name := range []string{"authorization", "x-gateway-key", "X-PROVIDER-KEY", "Authorization", "X-Gateway-Key"} {
		if v := h.Get(name); strings.Contains(v, "secret") {
			t.Errorf("case form %q still contains secret value %q after strip", name, v)
		}
	}
}
