package profiles

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSafeViewExcludesAllSecrets mirrors the
// TestFormatProfileLine_StripsURLUserinfo leak-vector list in
// cmd/ccwrap/profile_test.go, extended for the catalog-wire surface: the
// env NAME must never be emitted.
func TestSafeViewExcludesAllSecrets(t *testing.T) {
	p := Profile{
		Name:     "alpha",
		Provider: "Acme",
		BaseURL:  "https://carol:topsecret@gateway.example.com/v1",
		Auth: &AuthSpec{
			Mode:   "ccwrap_bearer",
			Key:    "sk-live-secretvalue",
			KeyEnv: "CCWRAP_TEST_PROFILE_KEY_ENV",
		},
		ModelAliases: map[string]string{"sonnet": "anthropic/x", "opus": "anthropic/y"},
		UpstreamHeaders: map[string]string{
			"X-Api-Key":  "sk-header-secretvalue",
			"X-Override": "override-secret",
		},
		Egress: EgressSpec{Mode: "http", URL: "http://proxyuser:proxypw@egress.example.com:3128"},
	}
	view := p.SafeView()
	if view.Name != "alpha" || view.Provider != "Acme" || view.BaseURLHost != "gateway.example.com" {
		t.Fatalf("identity: %#v", view)
	}
	if view.Auth == nil {
		t.Fatal("Auth = nil; want non-nil for profile with auth block")
	}
	if view.Auth.Mode != "ccwrap_bearer" {
		t.Fatalf("Auth.Mode = %q, want ccwrap_bearer", view.Auth.Mode)
	}
	if !view.Auth.HasInlineKey || !view.Auth.HasKeyEnv {
		t.Fatalf("flags: HasInlineKey=%v HasKeyEnv=%v (want both true)", view.Auth.HasInlineKey, view.Auth.HasKeyEnv)
	}
	if view.ModelAliasCount != 2 || view.UpstreamHeaderCount != 2 {
		t.Fatalf("counts: alias=%d hdr=%d", view.ModelAliasCount, view.UpstreamHeaderCount)
	}
	if view.EgressMode != "http" || view.EgressHost != "egress.example.com:3128" {
		t.Fatalf("egress: %q / %q", view.EgressMode, view.EgressHost)
	}
	// Re-serialize and search the bytes for every leak vector.
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{
		"carol", "topsecret", "carol:topsecret",
		"proxyuser", "proxypw",
		"sk-live-secretvalue",
		"sk-header-secretvalue", "override-secret",
		"CCWRAP_TEST_PROFILE_KEY_ENV", // env NAME is a leak vector
	} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("SafeView JSON leaks %q\n  got: %s", leak, blob)
		}
	}
}

func TestSafeViewInheritEgressNoHost(t *testing.T) {
	p := Profile{Name: "n", Provider: "P", BaseURL: "https://x.example", Egress: EgressSpec{Mode: "inherit"}}
	v := p.SafeView()
	if v.EgressMode != "inherit" || v.EgressHost != "" {
		t.Fatalf("inherit egress: %q / %q", v.EgressMode, v.EgressHost)
	}
}

// TestSafeView_BaseURL_StripsUserinfo verifies BaseURL carries the full URL
// (scheme + host + port + path) but with userinfo removed — the only
// secret-bearing component of a URL. Required for the popover edit form
// (web.go buildEditForm) to populate the base_url input usefully.
func TestSafeView_BaseURL_StripsUserinfo(t *testing.T) {
	p := &Profile{BaseURL: "https://user:pw@api.example.com:8080/v1"}
	v := p.SafeView()
	if v.BaseURL != "https://api.example.com:8080/v1" {
		t.Fatalf("BaseURL: got %q, want userinfo-stripped", v.BaseURL)
	}
	if strings.Contains(v.BaseURL, "user:pw") || strings.Contains(v.BaseURL, "@") {
		t.Fatalf("userinfo leaked: %q", v.BaseURL)
	}
}

// TestSafeView_EgressURL_ProxyModesOnly verifies EgressURL is populated
// for every proxy mode (http, socks5, socks5h) with userinfo stripped, and
// empty for inherit and direct. The Profiles form edits the proxy URL from
// this field, so a proxy mode without it could not be saved again.
func TestSafeView_EgressURL_ProxyModesOnly(t *testing.T) {
	for _, tc := range []struct{ mode, url, want, host string }{
		{"http", "http://user:pw@proxy:8080", "http://proxy:8080", "proxy:8080"},
		{"socks5", "socks5://user:pw@proxy:1080", "socks5://proxy:1080", "proxy:1080"},
		{"socks5h", "socks5h://proxy:1080", "socks5h://proxy:1080", "proxy:1080"},
		{"inherit", "http://anything", "", ""}, // ignored when mode=inherit
		{"direct", "", "", ""},
	} {
		p := &Profile{BaseURL: "https://api.example.com", Egress: EgressSpec{Mode: tc.mode, URL: tc.url}}
		v := p.SafeView()
		if v.EgressURL != tc.want || v.EgressHost != tc.host {
			t.Errorf("mode %s: EgressURL=%q EgressHost=%q, want %q %q", tc.mode, v.EgressURL, v.EgressHost, tc.want, tc.host)
		}
	}
}
