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
