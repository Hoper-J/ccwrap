package update

import "testing"

func TestEligible(t *testing.T) {
	// Mirrors the five build shapes of cmd/ccwrap/version.go's
	// versionString(): only clean release versions participate in
	// notices; dev builds are never nagged by their own older tag.
	cases := []struct {
		name    string
		current string
		want    bool
	}{
		{"release binary", "0.1.1+9944cdc", true},
		{"go install / exact tag", "0.1.1", true},
		{"in-tree pseudo-version", "0.1.2-0.20260702120000-9944cdcabcde", false},
		{"dirty release build", "0.1.1+9944cdc.dirty", false},
		{"sentinel no-stamp", "0.0.0", false},
		{"sentinel with sha", "0.0.0+9944cdc", false},
		{"garbage", "not-a-version", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Eligible(tc.current); got != tc.want {
				t.Fatalf("Eligible(%q) = %v, want %v", tc.current, got, tc.want)
			}
		})
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		name            string
		current, latest string
		want            bool
	}{
		{"newer patch", "0.2.0", "0.2.1", true},
		{"same version", "0.2.1", "0.2.1", false},
		{"older latest", "0.2.1", "0.2.0", false},
		{"build metadata ignored", "0.2.0+9944cdc", "0.2.1", true},
		{"build metadata ignored equal", "0.2.1+9944cdc", "0.2.1", false},
		{"invalid latest", "0.2.0", "banana", false},
		{"invalid current", "banana", "0.2.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Newer(tc.current, tc.latest); got != tc.want {
				t.Fatalf("Newer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDisabledAndInCI(t *testing.T) {
	if !Disabled(mapGetenv(map[string]string{"CCWRAP_NO_UPDATE_CHECK": "1"}), false) {
		t.Fatal("env CCWRAP_NO_UPDATE_CHECK=1 should disable")
	}
	if !Disabled(mapGetenv(nil), true) {
		t.Fatal("flag should disable")
	}
	if Disabled(mapGetenv(map[string]string{"CCWRAP_NO_UPDATE_CHECK": "0"}), false) {
		t.Fatal("CCWRAP_NO_UPDATE_CHECK=0 should NOT disable")
	}
	// Truthy variants match parseBoolLaunchFlag semantics: 1/true/yes/on
	// (case-insensitive) disable; any other value does not.
	for val, want := range map[string]bool{
		"true":   true,
		"yes":    true,
		"on":     true,
		"TRUE":   true,
		"banana": false,
	} {
		if got := Disabled(mapGetenv(map[string]string{"CCWRAP_NO_UPDATE_CHECK": val}), false); got != want {
			t.Fatalf("Disabled(CCWRAP_NO_UPDATE_CHECK=%q) = %v, want %v", val, got, want)
		}
	}
	if Disabled(mapGetenv(nil), false) {
		t.Fatal("default should be enabled")
	}
	if !InCI(mapGetenv(map[string]string{"CI": "true"})) {
		t.Fatal("CI=true should be CI")
	}
	if InCI(mapGetenv(map[string]string{"CI": "  "})) {
		t.Fatal("blank CI should not be CI")
	}
}
