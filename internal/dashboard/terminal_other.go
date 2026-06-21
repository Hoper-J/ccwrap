//go:build !unix

package dashboard

import (
	"os"
	"strconv"
)

func termWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

func watchTerminalResize(ch chan<- struct{}, stop <-chan struct{}) {
	<-stop
}
