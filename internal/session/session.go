// Package session persists conversation bindings: the mapping from a
// backend CLI's conversation id to the API key that is allowed to resume it.
//
// The security property this package exists to enforce is that a conversation
// id is not a credential. Ids are minted by the backend CLI, travel through
// clients, and may be logged, guessed, or leaked; nothing about holding one
// proves anything. Every record is therefore bound to the key that created it,
// and a lookup presenting a different key is answered exactly as if the id did
// not exist.
//
// It is a sibling of the store package, not a layer on top of it: these
// records are high-churn, best-effort state, and must never share a file with
// the document that holds key hashes and encrypted credentials.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	// SessionsFileName lives beside config.json in the same config dir.
	SessionsFileName = "sessions.json"

	// lockFileName is a sibling of the data file for the same reason as in
	// the store package: Save renames a temp file over sessions.json, so an
	// flock held on the replaced inode would guard nothing.
	lockFileName = "sessions.json.lock"

	// CurrentVersion is the schema version written to new files.
	CurrentVersion = 1

	// DefaultTTL expires a binding this long after its last use. A day is
	// chosen to outlive a working session without keeping a resumable handle
	// on someone's conversation alive indefinitely: the shorter the window,
	// the smaller the value of a leaked id.
	DefaultTTL = 24 * time.Hour

	// DefaultMaxRecords caps the file. At roughly 200 bytes per record this
	// is about a megabyte, which stays cheap to parse on every start while
	// being far more conversations than a local single-user proxy accumulates
	// inside one TTL window. Over the cap, the least recently used go first:
	// an old conversation nobody has resumed is the one least likely to be
	// resumed next.
	DefaultMaxRecords = 5000

	// touchFlushInterval throttles the disk write caused by Touch, which runs
	// on every proxied request. The buffered fields are LastUsedAt and Turns:
	// a crash can lose up to this much of them, which costs at worst an early
	// expiry and an undercounted turn total. Nothing authorizes off them.
	touchFlushInterval = 5 * time.Second
)

// ErrNotFound is returned both for an id that does not exist and for one
// owned by a different key. The two cases are deliberately indistinguishable
// to the caller: same error value, same message, no extra detail.
var ErrNotFound = errors.New("session: not found")

// Session is one conversation binding. Fields stay short because thousands of
// these are encoded on every flush.
type Session struct {
	ID         string    `json:"id"` // backend conversation id, opaque to us
	KeyID      string    `json:"k"`  // the key that created it: the ownership binding
	Adapter    string    `json:"a"`  // adapter id that owns this conversation
	Model      string    `json:"m"`  // model it was started with
	CreatedAt  time.Time `json:"c"`  // first bound
	LastUsedAt time.Time `json:"u"`  // drives expiry and LRU eviction
	Turns      int       `json:"t"`  // completed exchanges, informational
}

// file is the on-disk document.
type file struct {
	Version  int        `json:"version"`
	Sessions []*Session `json:"sessions"`
}

// touchDelta is the unflushed part of a record's usage fields.
type touchDelta struct {
	turns    int
	lastUsed time.Time
}

// Sessions is the in-process handle on sessions.json. It is safe for
// concurrent use and serialises against other processes with an advisory file
// lock held across each read-modify-write cycle.
type Sessions struct {
	mu   sync.RWMutex
	dir  string
	path string

	// TTL and MaxRecords may be overridden after Load and before use.
	ttl        time.Duration
	maxRecords int

	recs map[string]*Session

	pending   map[string]*touchDelta
	lastFlush time.Time

	// loadedMod/loadedSize identify the file revision in memory, so a write
	// by another process is noticed without re-reading on every lookup.
	loadedMod  time.Time
	loadedSize int64

	// now is a seam for tests; production always reads the wall clock.
	now func() time.Time

	// writes counts completed atomic writes, so a test can assert that Touch
	// really is buffered rather than inferring it from timestamps.
	writes int64

	// mismatches counts lookups that found the id but under a different key.
	// It is an internal signal only: it is never returned, never logged at a
	// level the requester can observe, and never attributed to an id, because
	// doing any of that would hand back the existence bit this package is
	// built to withhold.
	mismatches int64
}

// Option configures a Sessions handle at Load time.
type Option func(*Sessions)

// WithTTL overrides how long a binding survives after its last use.
func WithTTL(d time.Duration) Option {
	return func(s *Sessions) {
		if d > 0 {
			s.ttl = d
		}
	}
}

// WithMaxRecords overrides the record cap.
func WithMaxRecords(n int) Option {
	return func(s *Sessions) {
		if n > 0 {
			s.maxRecords = n
		}
	}
}

// Load opens sessions.json in dir. A missing file is normal and yields an
// empty set; malformed JSON is a hard error naming the path, and the file is
// left untouched so the user can look at it before anything overwrites it.
func Load(dir string, opts ...Option) (*Sessions, error) {
	s := &Sessions{
		dir:        dir,
		path:       filepath.Join(dir, SessionsFileName),
		ttl:        DefaultTTL,
		maxRecords: DefaultMaxRecords,
		recs:       map[string]*Session{},
		pending:    map[string]*touchDelta{},
		now:        time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	s.lastFlush = s.now()
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir reports the directory this file lives in.
func (s *Sessions) Dir() string { return s.dir }

// Path reports the sessions file location, which is what error messages show.
func (s *Sessions) Path() string { return s.path }

func (s *Sessions) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

func (s *Sessions) reloadLocked() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.recs = map[string]*Session{}
			s.noteRevisionLocked()
			s.applyPendingLocked()
			return nil
		}
		return fmt.Errorf("session: read %s: %w", s.path, err)
	}
	var doc file
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("session: %s is not valid JSON (refusing to overwrite it): %w", s.path, err)
	}
	recs := make(map[string]*Session, len(doc.Sessions))
	for _, r := range doc.Sessions {
		if r == nil || r.ID == "" || r.KeyID == "" {
			continue // a record with no owner could not be looked up safely
		}
		cp := *r
		recs[cp.ID] = &cp
	}
	s.recs = recs
	s.noteRevisionLocked()
	s.applyPendingLocked()
	s.pruneExpiredLocked()
	return nil
}

func (s *Sessions) noteRevisionLocked() {
	if fi, err := os.Stat(s.path); err == nil {
		s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size()
	} else {
		s.loadedMod, s.loadedSize = time.Time{}, 0
	}
}

// refreshIfChanged re-reads the file when disk holds a different revision
// than memory, so a second process's bindings become visible here. A stat is
// cheap next to the work a proxied request does.
func (s *Sessions) refreshIfChanged() {
	fi, err := os.Stat(s.path)
	if err != nil {
		return // a missing file leaves the last known good state in place
	}
	s.mu.RLock()
	same := fi.ModTime().Equal(s.loadedMod) && fi.Size() == s.loadedSize
	s.mu.RUnlock()
	if same {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if fi2, err := os.Stat(s.path); err == nil &&
		fi2.ModTime().Equal(s.loadedMod) && fi2.Size() == s.loadedSize {
		return
	}
	// A failed reload (half-written or corrupt file) keeps what we have
	// rather than dropping every binding mid-conversation.
	_ = s.reloadLocked()
}

// applyPendingLocked folds unflushed Touch deltas onto a freshly read map, so
// a reload triggered by an unrelated mutation does not lose them.
func (s *Sessions) applyPendingLocked() {
	for id, d := range s.pending {
		r, ok := s.recs[id]
		if !ok {
			continue
		}
		r.Turns += d.turns
		if d.lastUsed.After(r.LastUsedAt) {
			r.LastUsedAt = d.lastUsed
		}
	}
}

// expired reports whether a record has outlived the TTL.
func (s *Sessions) expired(r *Session, now time.Time) bool {
	return s.ttl > 0 && now.Sub(r.LastUsedAt) >= s.ttl
}

func (s *Sessions) pruneExpiredLocked() {
	now := s.now()
	for id, r := range s.recs {
		if s.expired(r, now) {
			delete(s.recs, id)
			delete(s.pending, id)
		}
	}
}

// evictLocked drops the least recently used records until the cap holds.
func (s *Sessions) evictLocked() {
	over := len(s.recs) - s.maxRecords
	if s.maxRecords <= 0 || over <= 0 {
		return
	}
	byUse := make([]*Session, 0, len(s.recs))
	for _, r := range s.recs {
		byUse = append(byUse, r)
	}
	sort.Slice(byUse, func(i, j int) bool {
		if byUse[i].LastUsedAt.Equal(byUse[j].LastUsedAt) {
			return byUse[i].ID < byUse[j].ID // deterministic on ties
		}
		return byUse[i].LastUsedAt.Before(byUse[j].LastUsedAt)
	})
	for i := 0; i < over; i++ {
		delete(s.recs, byUse[i].ID)
		delete(s.pending, byUse[i].ID)
	}
}

// mutate runs fn under both the process mutex and the cross-process file
// lock, re-reading first so a concurrent write from another process is not
// clobbered, and writing the result atomically.
func (s *Sessions) mutate(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock := s.flock()
	defer unlock()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	if fn != nil {
		if err := fn(); err != nil {
			return err
		}
	}
	s.pruneExpiredLocked()
	s.evictLocked()
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.pending = map[string]*touchDelta{}
	s.lastFlush = s.now()
	return nil
}

// flock takes an exclusive advisory lock. Best effort: on a filesystem
// without locking we would rather accept a small race than refuse to serve.
func (s *Sessions) flock() func() {
	noop := func() {}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return noop
	}
	f, err := os.OpenFile(filepath.Join(s.dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
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

// saveLocked writes atomically: temp file in the same directory, fsynced,
// then renamed, so a crash leaves either the old document or the new one.
func (s *Sessions) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("session: create %s: %w", s.dir, err)
	}

	list := make([]*Session, 0, len(s.recs))
	for _, r := range s.recs {
		list = append(list, r)
	}
	// Stable order keeps diffs and mtime/size change detection meaningful.
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	raw, err := json.Marshal(&file{Version: CurrentVersion, Sessions: list})
	if err != nil {
		return fmt.Errorf("session: encode sessions: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(s.dir, ".sessions-*.json")
	if err != nil {
		return fmt.Errorf("session: create temp file in %s: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	// Explicit despite CreateTemp's 0600: this file records who talks to whom.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("session: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("session: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("session: replace %s: %w", s.path, err)
	}
	s.writes++
	s.noteRevisionLocked()
	return nil
}

// Save flushes everything, including throttled Touch deltas.
func (s *Sessions) Save() error { return s.mutate(nil) }

// Bind records a new conversation, or refreshes one the same key already
// owns. Binding an id owned by another key fails with ErrNotFound: from the
// caller's side the id simply is not theirs, and saying more would confirm it
// exists. Bind is once per conversation, not per request, so it writes
// through rather than buffering.
func (s *Sessions) Bind(rec Session) error {
	if rec.ID == "" {
		return errors.New("session: conversation id is required")
	}
	if rec.KeyID == "" {
		// Without an owner there is nothing to check a resume against, which
		// would turn the id itself into the credential.
		return errors.New("session: key id is required")
	}
	return s.mutate(func() error {
		now := s.now()
		if existing, ok := s.recs[rec.ID]; ok {
			if existing.KeyID != rec.KeyID || s.expired(existing, now) {
				s.mismatches++
				return ErrNotFound
			}
			existing.Adapter = rec.Adapter
			existing.Model = rec.Model
			existing.LastUsedAt = now
			if rec.Turns > 0 {
				existing.Turns = rec.Turns
			}
			return nil
		}
		cp := rec
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = now
		}
		cp.LastUsedAt = now
		s.recs[cp.ID] = &cp
		return nil
	})
}

// Lookup returns the binding for id if, and only if, keyID owns it and it has
// not expired. Every other case is ErrNotFound with no further detail.
func (s *Sessions) Lookup(id, keyID string) (Session, error) {
	s.refreshIfChanged()
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.recs[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	if s.expired(r, s.now()) {
		delete(s.recs, id)
		delete(s.pending, id)
		return Session{}, ErrNotFound
	}
	if r.KeyID != keyID {
		s.mismatches++ // internal only; see the field comment
		return Session{}, ErrNotFound
	}
	return *r, nil
}

// Touch bumps LastUsedAt and the turn counter. It is on the hot path of every
// proxied request, so the update lands in memory immediately and reaches disk
// at most once per touchFlushInterval. Callers must have passed Lookup first:
// Touch takes no key because ownership was already proven there, and a Touch
// on an unknown id is simply a no-op error.
func (s *Sessions) Touch(id string) error {
	s.mu.Lock()

	r, ok := s.recs[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	now := s.now()
	if s.expired(r, now) {
		delete(s.recs, id)
		delete(s.pending, id)
		s.mu.Unlock()
		return ErrNotFound
	}

	r.Turns++
	r.LastUsedAt = now

	d, ok := s.pending[id]
	if !ok {
		d = &touchDelta{}
		s.pending[id] = d
	}
	d.turns++
	d.lastUsed = now

	due := now.Sub(s.lastFlush) >= touchFlushInterval
	s.mu.Unlock()

	if !due {
		return nil
	}
	return s.Save()
}

// List returns copies of the live bindings owned by keyID, most recently used
// first. Records owned by other keys are not merely filtered from the output,
// they are never counted: a caller must not be able to infer their number.
func (s *Sessions) List(keyID string) []Session {
	s.refreshIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now()
	out := make([]Session, 0, 8)
	for _, r := range s.recs {
		if r.KeyID != keyID || s.expired(r, now) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastUsedAt.Equal(out[j].LastUsedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

// Forget removes a binding the key owns. Forgetting an id owned by someone
// else, or one that never existed, is the same ErrNotFound.
func (s *Sessions) Forget(id, keyID string) error {
	return s.mutate(func() error {
		r, ok := s.recs[id]
		if !ok {
			return ErrNotFound
		}
		if r.KeyID != keyID {
			s.mismatches++
			return ErrNotFound
		}
		delete(s.recs, id)
		delete(s.pending, id)
		return nil
	})
}
