package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestTryLockSeesAnotherHandlesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, b := open(t, path), open(t, path)
	if err := Lock(a); err != nil {
		t.Fatalf("Lock = %v", err)
	}
	if err := TryLock(b); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock while held = %v, want ErrLocked", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatalf("Unlock = %v", err)
	}
	if err := TryLock(b); err != nil {
		t.Fatalf("TryLock after Unlock = %v, want nil", err)
	}
	if err := Unlock(b); err != nil {
		t.Fatalf("Unlock = %v", err)
	}
}

func TestLockWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, b := open(t, path), open(t, path)
	if err := Lock(a); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- Lock(b) }()
	select {
	case err := <-got:
		t.Fatalf("Lock returned %v while another handle held the lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Lock after release = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock did not return after the holder released it")
	}
	_ = Unlock(b)
}

func TestClosingTheHandleReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := Lock(a); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	b := open(t, path)
	// Windows releases a closed handle's lock soon, not necessarily at once.
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := TryLock(b)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("TryLock after the holder closed its handle = %v, want nil", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = Unlock(b)
}
