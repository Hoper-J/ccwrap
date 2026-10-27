package ui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDisplayDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	cases := map[string]string{
		home:                                "~",
		filepath.Join(home, "work", "repo"): filepath.Join("~", "work", "repo"),
		home + "x":                          home + "x",
		"/opt/repo":                         "/opt/repo",
		"":                                  "",
	}
	for in, want := range cases {
		if got := DisplayDir(in); got != want {
			t.Errorf("DisplayDir(%q) = %q, want %q", in, got, want)
		}
	}
}
