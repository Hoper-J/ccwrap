package webguard

import (
	"net/http/httptest"
	"testing"
)

func TestSameSite(t *testing.T) {
	extra := ParseAllowedHosts("tunnel.example.com")
	cases := []struct {
		host, origin, fetchSite string
		want                    bool
	}{
		{"127.0.0.1:5000", "", "", true},                                  // a tool: no browser headers
		{"127.0.0.1:5000", "http://127.0.0.1:5000", "same-origin", true},  // the page itself
		{"127.0.0.1:5000", "", "none", true},                              // typed by the user
		{"127.0.0.1:5000", "https://evil.example", "", false},             // another site's page
		{"127.0.0.1:5000", "", "cross-site", false},                       // marked cross-site
		{"127.0.0.1:5000", "", "same-site", false},                        // a sibling origin
		{"127.0.0.1:5000", "http://127.0.0.1:6000", "same-origin", false}, // another loopback port
		{"tunnel.example.com", "https://tunnel.example.com", "", true},    // an allowed tunnel host
		{"127.0.0.1:5000", "null", "", false},                             // opaque origin
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "http://"+tc.host+"/api", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
		}
		if got := SameSite(r, extra); got != tc.want {
			t.Errorf("host=%s origin=%q fetch-site=%q: %v, want %v", tc.host, tc.origin, tc.fetchSite, got, tc.want)
		}
	}
}
