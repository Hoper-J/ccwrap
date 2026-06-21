package ui

import "testing"

func TestStatusGlyph(t *testing.T) {
	p := New(false) // color disabled → raw glyph only
	cases := map[string]string{
		"pass":  "✓",
		"warn":  "⚠",
		"fail":  "✗",
		"PASS":  "✓",
		"weird": "·",
	}
	for in, want := range cases {
		if got := StatusGlyph(p, in); got != want {
			t.Fatalf("StatusGlyph(%q) = %q, want %q", in, got, want)
		}
	}
	pc := New(true)
	if g := StatusGlyph(pc, "pass"); g == "✓" || g == "" {
		t.Fatalf("colored pass glyph should be ANSI-wrapped, got %q", g)
	}
}
