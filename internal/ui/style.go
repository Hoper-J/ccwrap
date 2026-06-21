package ui

import (
	"fmt"
	"os"
	"strings"
)

type Palette struct {
	Enabled bool
}

func New(enabled bool) Palette { return Palette{Enabled: enabled} }

func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if (info.Mode() & os.ModeCharDevice) == 0 {
		return false
	}
	term := strings.ToLower(strings.TrimSpace(os.Getenv("TERM")))
	return term != "" && term != "dumb"
}

func (p Palette) wrap(code, s string) string {
	if !p.Enabled || s == "" {
		return s
	}
	return code + s + "\033[0m"
}

func (p Palette) Bold(s string) string    { return p.wrap("\033[1m", s) }
func (p Palette) Dim(s string) string     { return p.wrap("\033[2m", s) }
func (p Palette) Cyan(s string) string    { return p.wrap("\033[36m", s) }
func (p Palette) Green(s string) string   { return p.wrap("\033[32m", s) }
func (p Palette) Yellow(s string) string  { return p.wrap("\033[33m", s) }
func (p Palette) Red(s string) string     { return p.wrap("\033[31m", s) }
func (p Palette) Blue(s string) string    { return p.wrap("\033[34m", s) }
func (p Palette) Magenta(s string) string { return p.wrap("\033[35m", s) }

func (p Palette) Status(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "ok", "active", "attached":
		return p.Green(state)
	case "degraded", "warn", "warning", "launching":
		return p.Yellow(state)
	case "fail", "error":
		return p.Red(state)
	case "ended":
		// Lifecycle terminal state, not a failure — neutral, matching the
		// web hero's muted ended variant (danger-red here would inflate
		// every normally-closed session into an alarm).
		return p.Dim(state)
	default:
		return p.Cyan(state)
	}
}

func (p Palette) LabelValue(label, value string) string {
	return fmt.Sprintf("%s %s", p.Dim(label), value)
}

func Divider(ch string, n int) string {
	if n <= 0 {
		n = 72
	}
	return strings.Repeat(ch, n)
}
