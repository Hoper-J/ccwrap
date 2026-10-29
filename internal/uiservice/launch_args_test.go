package uiservice

import (
	"strings"
	"testing"
)

// The launch command is fixed; only these arguments ever reach it.
func TestLaunchArgs(t *testing.T) {
	id := "0b6c2a4e-5f1d-4c3b-9a8e-7d6f5e4c3b2a"
	for _, tc := range []struct{ profile, mode, session, want string }{
		{"", "new", "", ""},
		{"gw", "new", "", "--profile gw"},
		{"gw", "continue", "", "--profile gw -- --continue"},
		{"", "resume", "", "-- --resume"},
		{"gw", "resume", id, "--profile gw -- --resume " + id},
	} {
		if got := strings.Join(launchArgs(tc.profile, tc.mode, tc.session), " "); got != tc.want {
			t.Errorf("launchArgs(%q, %q, %q) = %q, want %q", tc.profile, tc.mode, tc.session, got, tc.want)
		}
	}
}
