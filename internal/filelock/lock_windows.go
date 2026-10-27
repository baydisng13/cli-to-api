//go:build windows

package filelock

import (
	"fmt"
	"os"
	"time"
)

// Windows has no flock, and reaching LockFileEx would mean taking a dependency
// on golang.org/x/sys. This package is deliberately dependency-free, so the
// Windows lock is built from an atomic create (O_CREATE|O_EXCL) instead.
//
// The guarantee is weaker than the unix one in exactly one way: the kernel
// does not drop the lock when the holder dies, so a crashed process would
// otherwise wedge every later run. Hence the stale-lock break below, which in
// turn means two processes can hold the "lock" simultaneously if one of them
// stalls for longer than lockStaleAfter between acquiring and releasing. Every
// write through this lock is a read-modify-write ending in an atomic rename,
// so the worst case is a lost update, not a corrupt config file - the same
// failure mode the unix path already accepts when flock is unsupported.
var (
	// lockAcquireTimeout bounds the wait, so a stuck peer degrades into the
	// same unsynchronised-but-working behaviour as a filesystem without locks.
	lockAcquireTimeout = 5 * time.Second

	// lockStaleAfter is how old a lock file must be before it is assumed to
	// belong to a dead process. Generous next to the milliseconds a
	// read-modify-write actually takes.
	lockStaleAfter = 30 * time.Second
)

// Acquire takes the lock described above and returns its release func.
// Signature and failure behaviour match the unix implementation: on failure
// the caller gets a no-op and proceeds unserialised.
func Acquire(path string) func() {
	noop := func() {}
	deadline := time.Now().Add(lockAcquireTimeout)
	backoff := time.Millisecond

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// The pid is purely diagnostic: it tells an operator staring at a
			// leftover lock file which run left it behind.
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }
		}
		if !os.IsExist(err) {
			return noop // unwritable directory; degrade rather than refuse
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > lockStaleAfter {
			// Break it and retry at once. A racing breaker is harmless: both
			// sides then contend for the fresh create, and one loses.
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return noop
		}
		time.Sleep(backoff)
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
}
