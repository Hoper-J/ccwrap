package preflight

import "testing"

func TestStripUserinfo(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"non-url passthrough", "direct", "direct"},
		{"scheme-less", "host:port/path", "host:port/path"},
		{"no userinfo", "https://example.com/p", "https://example.com/p"},
		{"user only", "https://alice@example.com/p", "https://example.com/p"},
		{"user pw", "https://alice:secret@example.com/p", "https://example.com/p"},
		{"empty user empty pw", "https://:@example.com/", "https://example.com/"},
		{"@ in path", "https://example.com/path-with-@-in-it", "https://example.com/path-with-@-in-it"},
		{"user @ in path", "https://alice@example.com/path/with/@/in/it", "https://example.com/path/with/@/in/it"},
		{"malformed url", "ht!tp://??not a url", "ht!tp://??not a url"},
		{"user pw with port", "http://u:p@host:8080/x?q=1#f", "http://host:8080/x?q=1#f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripUserinfo(tc.in)
			if got != tc.want {
				t.Errorf("stripUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafeProfileView_StripsEgressUserinfo(t *testing.T) {
	// Construct a minimal *Result whose ProfileView().EgressSummary contains
	// userinfo. ProfileView identity is exposed as Name (= ActiveProfileName)
	// and ProviderLabel (composite "name (provider)") — see profile.go:416-444.
	r := &Result{
		ActiveProfileName:     "p1",
		ActiveProfileProvider: "prov",
	}
	r.Egress.Summary = "http://alice:secret@proxy.example.com:3128"
	view := SafeProfileView(r)
	if view.EgressSummary != "http://proxy.example.com:3128" {
		t.Errorf("SafeProfileView EgressSummary = %q, want stripped form", view.EgressSummary)
	}
	if view.Name != "p1" || view.ProviderLabel != "p1 (prov)" {
		t.Errorf("identity fields not preserved: %+v", view)
	}
}
