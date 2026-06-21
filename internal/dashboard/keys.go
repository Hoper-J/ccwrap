package dashboard

import (
	"os"
	"sync"

	"golang.org/x/term"
)

// startKeyReader puts stdin in raw mode and streams single-byte key
// presses to keyCh until stop closes. It returns (restore, active):
//
//   - restore: ALWAYS non-nil and idempotent. The caller MUST
//     `defer restore()` in the main Run goroutine. Restoring from
//     Run (not from the reader goroutine) is the critical fix: the
//     reader goroutine blocks in os.Stdin.Read and cannot be
//     interrupted portably, so a defer inside it would never run on
//     a `q`/ctx exit and would leave the terminal in raw mode.
//   - active: false when `in` is not a TTY (no raw mode entered);
//     the dashboard then runs in its existing append mode.
//
// The reader goroutine may remain blocked in Read after restore();
// that is acceptable — the process exits immediately afterwards and
// the terminal is already restored, so no corruption is observable.
func startKeyReader(in *os.File, keyCh chan<- rune, stop <-chan struct{}) (restore func(), active bool) {
	noop := func() {}
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return noop, false
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return noop, false
	}
	var once sync.Once
	restore = func() { once.Do(func() { _ = term.Restore(fd, oldState) }) }
	go func() {
		buf := make([]byte, 1)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := in.Read(buf)
			if err != nil {
				return
			}
			if n == 1 {
				select {
				case keyCh <- rune(buf[0]):
				case <-stop:
					return
				}
			}
		}
	}()
	return restore, true
}
