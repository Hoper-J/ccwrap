package supervisor

import (
	"sync"
	"testing"
)

// TestRecoverGoroutine confirms the background-goroutine panic backstop
// actually contains a panic instead of letting it terminate the runtime
// (which, since the supervisor shares the launcher process, would crash
// ccwrap and orphan Claude). A goroutine whose body panics must return
// normally once recoverGoroutine is deferred at its top.
func TestRecoverGoroutine(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	survived := false
	go func() {
		defer func() {
			// If recoverGoroutine swallowed the panic, control reaches here
			// and the outer recover sees nothing.
			if r := recover(); r != nil {
				t.Errorf("panic escaped recoverGoroutine: %v", r)
			}
			survived = true
			wg.Done()
		}()
		defer recoverGoroutine("test.panicker")
		panic("boom")
	}()
	wg.Wait()
	if !survived {
		t.Fatalf("goroutine did not complete after a recovered panic")
	}
}
