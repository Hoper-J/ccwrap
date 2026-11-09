package ui

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// terminalReady: Windows terminals do not set TERM, so a console counts
// unless TERM is "dumb". Output to it is styled with escape sequences, which
// a console takes only with virtual terminal processing on (see EnableVT).
func terminalReady(f *os.File) bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	h := windows.Handle(f.Fd())
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(h, &info) != nil {
		return true // console input
	}
	var mode uint32
	return windows.GetConsoleMode(h, &mode) == nil && mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0
}

// EnableVT turns on virtual terminal processing for the consoles among
// files, and returns a func that puts their modes back as they were.
func EnableVT(files ...*os.File) (restore func()) {
	type saved struct {
		h    windows.Handle
		mode uint32
	}
	var undo []saved
	for _, f := range files {
		if f == nil {
			continue
		}
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil || mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
			continue
		}
		if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil {
			undo = append(undo, saved{h, mode})
		}
	}
	return func() {
		for _, s := range undo {
			_ = windows.SetConsoleMode(s.h, s.mode)
		}
	}
}
