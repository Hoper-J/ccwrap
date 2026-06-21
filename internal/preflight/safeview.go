package preflight

import "net/url"

// stripUserinfo returns s with any URL userinfo (user[:password]@) removed.
// On parse failure or when u.User == nil, returns s unchanged.
//
// Edge cases (see safeview_test.go for the contract): empty string, non-URL
// strings like "direct", scheme-less URLs, URLs with @ in path/query/fragment
// (the parser localizes userinfo to the authority section), malformed URLs.
func stripUserinfo(s string) string {
	if s == "" {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	return u.String()
}

// SafeProfileView returns the result's ProfileView with EgressSummary userinfo
// stripped. It wraps raw ProfileView to prevent caller-forgetting-strip.
//
// Today's (*Result).ProfileView().EgressSummary flows through egress.redact which
// preserves the username (see egress.go). This wrapper post-processes the view
// through stripUserinfo so no surface (ccwrap profile {ls,status,switch} CLI,
// SwitchOutcome.View, the inspect UI) can accidentally serialize an
// unstripped EgressSummary.
func SafeProfileView(r *Result) ProfileView {
	v := r.ProfileView()
	v.EgressSummary = stripUserinfo(v.EgressSummary)
	return v
}
