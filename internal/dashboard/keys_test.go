package dashboard

import (
	"os"
	"testing"
)

func TestKeyReaderNonTTYIsNoOp(t *testing.T) {
	// A pipe is not a terminal; startKeyReader must report inactive,
	// never enter raw mode, and still return a safe non-nil restore.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	keyCh := make(chan rune, 1)
	stop := make(chan struct{})
	restore, active := startKeyReader(r, keyCh, stop)
	if active {
		t.Fatalf("startKeyReader on a pipe must be inactive (non-TTY)")
	}
	if restore == nil {
		t.Fatalf("restore must always be non-nil (safe to defer even when inactive)")
	}
	restore() // must be a safe no-op when inactive
	close(stop)
}
