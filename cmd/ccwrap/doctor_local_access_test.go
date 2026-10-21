package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/localpeer"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

func TestAddLocalAccessCheck(t *testing.T) {
	self := uint32(os.Geteuid())
	noEnv := func(string) string { return "" }
	cases := []struct {
		name        string
		getenv      func(string) string
		uid         uint32
		err         error
		status      string
		summaryHave []string
		selfTestRun bool
	}{
		{"pass", noEnv, self, nil, "pass", []string{fmt.Sprintf("only your own processes (uid %d) and root", self)}, true},
		{"unavailable", noEnv, 0, localpeer.ErrUnsupported, "warn", []string{"unavailable", localpeer.ErrUnsupported.Error(), localAccessExposure}, true},
		{"misidentified", noEnv, self + 1, nil, "fail", []string{fmt.Sprintf("uid %d (expected %d)", self+1, self)}, true},
		{"disabled", func(k string) string {
			if k == localpeer.DisableEnv {
				return "0"
			}
			return ""
		}, 0, errors.New("must not run"), "warn", []string{"disabled (CCWRAP_LOCAL_PEER_CHECK=0)", localAccessExposure}, false},
	}
	for _, tc := range cases {
		var got []string
		ran := false
		addLocalAccessCheck(func(name, status, summary, detail string) {
			got = append(got, name, status, summary, detail)
		}, tc.getenv, func() (uint32, error) {
			ran = true
			return tc.uid, tc.err
		})
		if len(got) != 4 || got[0] != "local_access" || got[1] != tc.status {
			t.Errorf("%s: added %q, want one local_access row with status %s", tc.name, got, tc.status)
			continue
		}
		for _, want := range tc.summaryHave {
			if !strings.Contains(got[2], want) {
				t.Errorf("%s: summary %q missing %q", tc.name, got[2], want)
			}
		}
		if ran != tc.selfTestRun {
			t.Errorf("%s: self-test ran=%v, want %v", tc.name, ran, tc.selfTestRun)
		}
	}
}

func TestDoctorReportsLocalAccess(t *testing.T) {
	report := runDoctorJSON(t, testutil.ShortAppPaths(t, "c.sock"), []string{"--json"})
	check, ok := doctorChecksByName(report)["local_access"]
	if !ok {
		t.Fatal("doctor report has no local_access row")
	}
	if check.Status != "pass" && check.Status != "warn" {
		t.Fatalf("local_access status %q: %s", check.Status, check.Summary)
	}
	t.Logf("local_access: %s — %s", check.Status, check.Summary)
}
