//go:build windows

package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireLockBreaksStaleLock covers the recovery the unix path gets for
// free from the kernel: a lock file left behind by a dead process must not
// wedge every later run.
func TestAcquireLockBreaksStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		release := Acquire(path)
		release()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(lockAcquireTimeout + 2*time.Second):
		t.Fatal("stale lock was not broken")
	}
}

// TestAcquireLockTimesOutOnLiveLock checks the bounded wait: a lock held by a
// live peer is waited on, then abandoned, never waited on forever.
func TestAcquireLockTimesOutOnLiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	held := Acquire(path)
	defer held()

	prev := lockAcquireTimeout
	lockAcquireTimeout = 200 * time.Millisecond
	defer func() { lockAcquireTimeout = prev }()

	start := time.Now()
	Acquire(path)()
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("returned after %v, want it to wait out the timeout", elapsed)
	}
}
