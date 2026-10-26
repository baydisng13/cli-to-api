package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock makes expiry and LRU ordering deterministic without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustLoad(t *testing.T, dir string, opts ...Option) *Sessions {
	t.Helper()
	s, err := Load(dir, opts...)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	return s
}

// withClock swaps in a fake clock; the handle must not have been used yet.
func withClock(s *Sessions, c *fakeClock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = c.now
	s.lastFlush = c.now()
}

func rec(id, keyID string) Session {
	return Session{ID: id, KeyID: keyID, Adapter: "agy", Model: "gemini-3-pro"}
}

func TestBindThenLookupByOwner(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	if err := s.Bind(rec("conv-1", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	got, err := s.Lookup("conv-1", "key-a")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Adapter != "agy" || got.Model != "gemini-3-pro" || got.KeyID != "key-a" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.LastUsedAt.IsZero() {
		t.Fatalf("timestamps not stamped: %+v", got)
	}
	if s.Path() != filepath.Join(s.Dir(), SessionsFileName) {
		t.Fatalf("Path() = %q", s.Path())
	}
}

// The core security property: a stranger's lookup of a real id must be
// byte-for-byte the same answer as a lookup of an id that never existed.
func TestLookupByOtherKeyIsIndistinguishable(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	if err := s.Bind(rec("conv-1", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	gotSess, mismatchErr := s.Lookup("conv-1", "key-b")
	if gotSess != (Session{}) {
		t.Fatalf("leaked a record to a non-owner: %+v", gotSess)
	}
	_, unknownErr := s.Lookup("conv-does-not-exist", "key-b")

	if !errors.Is(mismatchErr, ErrNotFound) || !errors.Is(unknownErr, ErrNotFound) {
		t.Fatalf("errors: mismatch=%v unknown=%v", mismatchErr, unknownErr)
	}
	if mismatchErr != unknownErr {
		t.Fatalf("different error values: %#v vs %#v", mismatchErr, unknownErr)
	}
	if mismatchErr.Error() != unknownErr.Error() {
		t.Fatalf("different messages: %q vs %q", mismatchErr.Error(), unknownErr.Error())
	}
	if strings.Contains(mismatchErr.Error(), "conv-1") || strings.Contains(mismatchErr.Error(), "key-") {
		t.Fatalf("error message leaks identifiers: %q", mismatchErr.Error())
	}

	// Neither does List hand a non-owner anything.
	if got := s.List("key-b"); len(got) != 0 {
		t.Fatalf("List for non-owner returned %d records", len(got))
	}
	// Nor Forget.
	if err := s.Forget("conv-1", "key-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Forget by non-owner: %v", err)
	}
	if _, err := s.Lookup("conv-1", "key-a"); err != nil {
		t.Fatalf("owner lost its record to a stranger's Forget: %v", err)
	}
}

func TestExpiredRecordIsInvisibleAndPruned(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := mustLoad(t, dir, WithTTL(time.Hour))
	withClock(s, clock)

	if err := s.Bind(rec("old", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	clock.advance(2 * time.Hour)
	if err := s.Bind(rec("new", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if _, err := s.Lookup("old", "key-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired record visible: %v", err)
	}
	if _, err := s.Lookup("new", "key-a"); err != nil {
		t.Fatalf("live record lost: %v", err)
	}
	if got := s.List("key-a"); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("List = %+v", got)
	}

	// The second Bind's save ran a prune, so the expired record is off disk.
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"old"`) {
		t.Fatalf("expired record still on disk: %s", raw)
	}
}

func TestEvictionIsLeastRecentlyUsedNotOldestCreated(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := mustLoad(t, t.TempDir(), WithMaxRecords(3))
	withClock(s, clock)

	for _, id := range []string{"a", "b", "c"} {
		if err := s.Bind(rec(id, "key-a")); err != nil {
			t.Fatalf("Bind %s: %v", id, err)
		}
		clock.advance(time.Minute)
	}
	// "a" is the oldest created, but is used again right before the overflow.
	if err := s.Touch("a"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	clock.advance(time.Minute)
	if err := s.Bind(rec("d", "key-a")); err != nil {
		t.Fatalf("Bind d: %v", err)
	}

	if _, err := s.Lookup("b", "key-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("least-recently-used record survived eviction: %v", err)
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, err := s.Lookup(id, "key-a"); err != nil {
			t.Fatalf("Lookup %s after eviction: %v", id, err)
		}
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)
	if err := s.Bind(rec("conv-1", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := s.Touch("conv-1"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, SessionsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %v", perm)
	}

	fresh := mustLoad(t, dir)
	got, err := fresh.Lookup("conv-1", "key-a")
	if err != nil {
		t.Fatalf("Lookup after reload: %v", err)
	}
	if got.Turns != 1 {
		t.Fatalf("buffered Turns not flushed by Save: %+v", got)
	}
	if _, err := fresh.Lookup("conv-1", "key-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ownership binding lost across reload: %v", err)
	}
}

func TestExternalWriteIsNoticed(t *testing.T) {
	dir := t.TempDir()
	first := mustLoad(t, dir)
	second := mustLoad(t, dir)

	if err := first.Bind(rec("conv-1", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := second.Bind(rec("conv-2-longer-id", "key-a")); err != nil {
		t.Fatalf("Bind (second handle): %v", err)
	}

	if _, err := first.Lookup("conv-2-longer-id", "key-a"); err != nil {
		t.Fatalf("first handle did not notice the external write: %v", err)
	}
	// The second handle re-read before writing, so nothing was clobbered.
	if _, err := second.Lookup("conv-1", "key-a"); err != nil {
		t.Fatalf("external write clobbered the first handle's record: %v", err)
	}
}

func TestCorruptJSONErrorsAndPreservesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SessionsFileName)
	const garbage = "{not json at all"
	if err := os.WriteFile(path, []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir)
	if err == nil {
		t.Fatalf("Load accepted corrupt JSON: %+v", s)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the path: %v", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil || string(raw) != garbage {
		t.Fatalf("corrupt file was destroyed: %q, %v", raw, readErr)
	}
}

func TestTouchIsBuffered(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	if err := s.Bind(rec("conv-1", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	s.mu.RLock()
	afterBind := s.writes
	s.mu.RUnlock()
	if afterBind != 1 {
		t.Fatalf("Bind wrote %d times, want 1", afterBind)
	}

	for i := 0; i < 500; i++ {
		if err := s.Touch("conv-1"); err != nil {
			t.Fatalf("Touch %d: %v", i, err)
		}
	}
	s.mu.RLock()
	afterTouch := s.writes
	s.mu.RUnlock()
	if afterTouch != afterBind {
		t.Fatalf("500 Touch calls caused %d disk writes", afterTouch-afterBind)
	}

	// The buffered state is still correct in memory, and Save persists it.
	got, err := s.Lookup("conv-1", "key-a")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Turns != 500 {
		t.Fatalf("Turns = %d, want 500", got.Turns)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fresh := mustLoad(t, s.Dir())
	if got, err := fresh.Lookup("conv-1", "key-a"); err != nil || got.Turns != 500 {
		t.Fatalf("after Save: %+v, %v", got, err)
	}
}

func TestConcurrentUse(t *testing.T) {
	s := mustLoad(t, t.TempDir())

	const workers = 8
	const perWorker = 25
	var touches int64

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", w)
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("conv-%d-%d", w, i)
				if err := s.Bind(rec(id, key)); err != nil {
					t.Errorf("Bind %s: %v", id, err)
					return
				}
				if err := s.Touch(id); err != nil {
					t.Errorf("Touch %s: %v", id, err)
					return
				}
				atomic.AddInt64(&touches, 1)
				if _, err := s.Lookup(id, key); err != nil {
					t.Errorf("Lookup %s: %v", id, err)
					return
				}
				if _, err := s.Lookup(id, "key-other"); !errors.Is(err, ErrNotFound) {
					t.Errorf("Lookup %s by stranger: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fresh := mustLoad(t, s.Dir())
	for w := 0; w < workers; w++ {
		if got := fresh.List(fmt.Sprintf("key-%d", w)); len(got) != perWorker {
			t.Fatalf("worker %d: %d records survived, want %d", w, len(got), perWorker)
		}
	}
}

func TestBindRejectsUnownedAndForeignRecords(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	if err := s.Bind(Session{ID: "x"}); err == nil {
		t.Fatal("Bind accepted a record with no owning key")
	}
	if err := s.Bind(Session{KeyID: "key-a"}); err == nil {
		t.Fatal("Bind accepted a record with no id")
	}
	if err := s.Bind(rec("conv-1", "key-a")); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Rebinding someone else's id must not take it over.
	if err := s.Bind(rec("conv-1", "key-b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Bind over a foreign id: %v", err)
	}
	got, err := s.Lookup("conv-1", "key-a")
	if err != nil || got.KeyID != "key-a" {
		t.Fatalf("ownership changed: %+v, %v", got, err)
	}
}
