package ui

import (
	"fmt"
	"time"
)

// Uptime is how long ago ts was, in its largest whole unit: "45s", "12m",
// "3h", "2d". Zero ts reads as "".
func Uptime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	age := time.Since(ts)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
}
