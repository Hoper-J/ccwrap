package ui

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// Console output counts as a terminal once virtual terminal processing is
// on; EnableVT turns it on and its restore puts the mode back.
func TestEnableVTMakesTheConsoleATerminal(t *testing.T) {
	t.Setenv("TERM", "")
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no console: %v", err)
	}
	defer out.Close()
	h := windows.Handle(out.Fd())
	var orig uint32
	if err := windows.GetConsoleMode(h, &orig); err != nil {
		t.Skipf("no console mode: %v", err)
	}
	defer windows.SetConsoleMode(h, orig)
	off := orig &^ windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if err := windows.SetConsoleMode(h, off); err != nil {
		t.Fatal(err)
	}
	if IsTerminal(out) {
		t.Fatal("console output without virtual terminal processing counts as a terminal")
	}
	restore := EnableVT(out)
	if !IsTerminal(out) {
		t.Fatal("console output is not a terminal after EnableVT")
	}
	restore()
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil || mode != off {
		t.Fatalf("mode after restore = %#x, %v; want %#x", mode, err, off)
	}
}

// Console input counts as a terminal without TERM, so prompts show;
// TERM=dumb still turns that off.
func TestConsoleInputIsATerminal(t *testing.T) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no console: %v", err)
	}
	defer in.Close()
	t.Setenv("TERM", "")
	if !IsTerminal(in) {
		t.Fatal("console input is not a terminal")
	}
	t.Setenv("TERM", "dumb")
	if IsTerminal(in) {
		t.Fatal("console input counts as a terminal under TERM=dumb")
	}
}
