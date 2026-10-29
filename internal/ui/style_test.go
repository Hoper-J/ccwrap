package ui

import (
	"os"
	"testing"
)

// /dev/null is a character device but not a terminal: input redirected from
// it must not be prompted for, and output to it gets no colors.
func TestIsTerminalRejectsDevNullAndPipes(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if IsTerminal(null) {
		t.Fatal("IsTerminal(/dev/null) = true")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r) || IsTerminal(w) {
		t.Fatal("IsTerminal(pipe) = true")
	}
	if IsTerminal(nil) {
		t.Fatal("IsTerminal(nil) = true")
	}
}
