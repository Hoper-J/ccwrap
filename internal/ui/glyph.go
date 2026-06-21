package ui

import "strings"

// StatusGlyph maps a doctor check status to its glyph,
// colored via the palette. pass→green ✓, warn→yellow ⚠, fail→red ✗.
func StatusGlyph(p Palette, status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pass", "ok":
		return p.Green("✓")
	case "warn", "warning":
		return p.Yellow("⚠")
	case "fail", "error":
		return p.Red("✗")
	default:
		return p.Dim("·")
	}
}
