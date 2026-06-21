package profiles

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestLock_SerializesConcurrentRMW proves Lock serializes a read-modify-write
// even between goroutines in ONE process — each Lock() opens its own fd, the
// same open-file-description mechanism that makes flock serialize across
// processes. The counter file is mutated non-atomically (read int, write
// int+1); without serialization increments are lost and the final count is
// short of workers*perWorker.
func TestLock_SerializesConcurrentRMW(t *testing.T) {
	stateDir := t.TempDir()
	counterPath := filepath.Join(stateDir, "counter")
	if err := os.WriteFile(counterPath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 8, 60
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				unlock, err := Lock(stateDir)
				if err != nil {
					t.Errorf("Lock: %v", err)
					return
				}
				b, _ := os.ReadFile(counterPath)
				n, _ := strconv.Atoi(string(b))
				_ = os.WriteFile(counterPath, []byte(strconv.Itoa(n+1)), 0o600)
				unlock()
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(counterPath)
	got, _ := strconv.Atoi(string(b))
	if want := workers * perWorker; got != want {
		t.Fatalf("lost updates: counter = %d, want %d (Lock did not serialize the RMW)", got, want)
	}
}

// TestLock_ReleaseAllowsReacquire proves unlock actually releases — a second
// Lock after unlock must not block.
func TestLock_ReleaseAllowsReacquire(t *testing.T) {
	stateDir := t.TempDir()
	unlock, err := Lock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	done := make(chan struct{})
	go func() {
		u2, err := Lock(stateDir)
		if err == nil {
			u2()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("second Lock blocked after unlock — release failed")
	}
}

// TestLock_BlocksWhileHeld proves a second acquisition does NOT proceed while
// the first is held (mutual exclusion), then proceeds once released.
func TestLock_BlocksWhileHeld(t *testing.T) {
	stateDir := t.TempDir()
	unlock, err := Lock(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	go func() {
		u2, err := Lock(stateDir)
		if err == nil {
			defer u2()
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second Lock acquired while the first was still held")
	case <-time.After(200 * time.Millisecond):
		// good — still blocked
	}
	unlock()
	select {
	case <-acquired:
		// good — released, second acquired
	case <-time.After(3 * time.Second):
		t.Fatal("second Lock never acquired after release")
	}
}
