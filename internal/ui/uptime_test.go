package ui

import (
	"testing"
	"time"
)

func TestUptime(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{{30 * time.Second, "30s"}, {12 * time.Minute, "12m"}, {3 * time.Hour, "3h"}, {50 * time.Hour, "2d"}} {
		if got := Uptime(now.Add(-tc.ago)); got != tc.want {
			t.Errorf("Uptime(-%v) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if Uptime(time.Time{}) != "" {
		t.Error("zero time reads as empty")
	}
}
