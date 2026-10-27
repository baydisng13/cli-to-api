//go:build !windows

// Package filelock provides the advisory lock shared by the key store and the
// conversation store, so a read-modify-write cycle in one process is not
// clobbered by another. It exists as its own package because both stores need
// it and the implementation is per-platform.
package filelock

import (
	"os"
	"syscall"
)

// Acquire takes an exclusive advisory lock on path, returning the function
// that releases it. Unix gets a real kernel lock (flock), which the OS drops
// even if the process dies holding it.
//
// Best effort by design: on a filesystem that does not implement locking the
// release func is a no-op and callers proceed unserialised, which is a small
// race window rather than a refusal to run.
func Acquire(path string) func() {
	noop := func() {}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return noop
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return noop
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
