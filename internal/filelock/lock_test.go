package filelock

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAcquireLockSerialises exercises the contract both implementations owe
// store.mutate: whoever holds the lock has it alone, and release hands it on.
func TestAcquireLockSerialises(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	var mu sync.Mutex
	inside, maxInside := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := Acquire(path)
			defer release()

			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if maxInside != 1 {
		t.Fatalf("%d holders at once, want 1", maxInside)
	}
}

// TestAcquireLockReleaseIsReusable checks that a released lock can be taken
// again by the same process, which is the common case: several mutate calls.
func TestAcquireLockReleaseIsReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	for i := 0; i < 3; i++ {
		release := Acquire(path)
		if release == nil {
			t.Fatal("Acquire returned nil release func")
		}
		release()
	}
}

// TestAcquireLockUnwritableDirDegrades pins the "best effort" promise: when
// the lock cannot be taken at all, callers still get a usable release func
// rather than a panic or a refusal.
func TestAcquireLockUnwritableDirDegrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "x.lock")
	release := Acquire(path)
	if release == nil {
		t.Fatal("Acquire returned nil release func")
	}
	release()
}
