package ui

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestShortID(t *testing.T) {
	cases := map[string]string{
		"74c5fcaf812ff6d5": "74c5fcaf",
		"74c5fcaf":         "74c5fcaf",
		"abc":              "abc",
		"":                 "",
	}
	for in, want := range cases {
		if got := ShortID(in); got != want {
			t.Fatalf("ShortID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortMethodLabel(t *testing.T) {
	cases := []struct {
		rec  model.RequestRecord
		want string
	}{
		{model.RequestRecord{Method: "POST"}, "POST"},
		{model.RequestRecord{Method: "GET", Synthetic: true}, "SYNTH GET"},
		{model.RequestRecord{Synthetic: true}, "SYNTH"},
		{model.RequestRecord{}, "REQUEST"},
	}
	for _, tc := range cases {
		if got := ShortMethodLabel(tc.rec); got != tc.want {
			t.Fatalf("ShortMethodLabel(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
	syn := model.RequestRecord{Method: "GET", Synthetic: true}
	if syn.MethodLabel() != "SYNTHETIC" {
		t.Fatalf("Web MethodLabel must stay SYNTHETIC, got %q", syn.MethodLabel())
	}
}

func TestClaudeVersionFromUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.296 (external, sdk-cli)":  "2.1.296",
		"claude-cli/2.1.319":                      "2.1.319",
		"claude-cli/2.2.0-beta.1 (external, cli)": "2.2.0-beta.1",
		"claude-cli/latest":                       "",
		"curl/8.7.1":                              "",
		"Mozilla/5.0 claude-cli/2.1.296":          "",
		"":                                        "",
	} {
		if got := ClaudeVersionFromUserAgent(ua); got != want {
			t.Errorf("ClaudeVersionFromUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}
