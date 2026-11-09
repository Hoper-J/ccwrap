package dashboard

import (
	"os"
	"strconv"
	"time"

	"golang.org/x/term"
)

func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

// watchTerminalResize polls the width: Windows has no SIGWINCH, and a
// console reports resizes as input events, on the input the key reader reads.
func watchTerminalResize(ch chan<- struct{}, stop <-chan struct{}) {
	watchWidth(termWidth, 250*time.Millisecond, ch, stop)
}

// watchWidth signals ch each time width() changes, checking every interval
// until stop closes.
func watchWidth(width func() int, every time.Duration, ch chan<- struct{}, stop <-chan struct{}) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	last := width()
	for {
		select {
		case <-tick.C:
			if w := width(); w != last {
				last = w
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		case <-stop:
			return
		}
	}
}
