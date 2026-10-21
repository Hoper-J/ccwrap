package ui

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestDisplayPath(t *testing.T) {
	cases := map[string]string{
		"/v1/messages?beta=true":        "/v1/messages",
		"/v1/messages":                  "/v1/messages",
		"/v1/models?beta=true&limit=20": "/v1/models?limit=20",
		"/v1/models?limit=20&beta=true": "/v1/models?limit=20",
		"/api/oauth/profile?beta=false": "/api/oauth/profile?beta=false",
		"/v1/messages?":                 "/v1/messages",
		"":                              "",
	}
	for in, want := range cases {
		if got := DisplayPath(in); got != want {
			t.Errorf("DisplayPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorSeverityLabel(t *testing.T) {
	if got := ErrorSeverityLabel(model.ErrorRecord{Severity: "warn"}); got != "warning" {
		t.Fatalf("warn → %q", got)
	}
	for _, sev := range []string{"error", ""} {
		if got := ErrorSeverityLabel(model.ErrorRecord{Severity: sev}); got != "error" {
			t.Fatalf("%q → %q", sev, got)
		}
	}
}
