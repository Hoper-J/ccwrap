package dashboard

import (
	"sync/atomic"
	"testing"
	"time"
)

// The dashboard redraws when the console's width changes, and only then.
func TestWatchWidthSignalsChanges(t *testing.T) {
	var width atomic.Int64
	width.Store(80)
	ch := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		watchWidth(func() int { return int(width.Load()) }, 5*time.Millisecond, ch, stop)
		close(done)
	}()
	select {
	case <-ch:
		t.Fatal("signaled without a change")
	case <-time.After(50 * time.Millisecond):
	}
	width.Store(120)
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal after the width changed")
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchWidth kept running after stop")
	}
}
