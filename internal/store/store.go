package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// ConfigFileName is the single document holding every key.
	ConfigFileName = "config.json"

	// lockFileName is a sibling of the config file rather than the config
	// file itself: Save() renames a temp file over config.json, which swaps
	// the inode, and an flock held on the old inode would protect nothing.
	lockFileName = "config.json.lock"

	// CurrentVersion is the schema version written to new configs.
	CurrentVersion = 1

	// usageFlushInterval throttles the disk write caused by TouchUsage.
	// Usage counters are telemetry, not authorization state, so trading
	// a few seconds of durability for one fsync per request is the right
	// side of that bargain.
	usageFlushInterval = 5 * time.Second
)

var (
	// ErrNotFound is returned when no key carries the requested id.
	ErrNotFound = errors.New("store: key not found")

	// ErrDuplicateID guards the invariant the whole lookup path relies on:
	// one id, one key.
	ErrDuplicateID = errors.New("store: key id already exists")
)

// DefaultDir resolves the configuration directory, honouring an explicit
// override first and the XDG convention second.
func DefaultDir() (string, error) {
	if d := os.Getenv("CLI_TO_API_HOME"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "cli-to-api"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "cli-to-api"), nil
}

// usageDelta is the unflushed part of a key's usage counters.
type usageDelta struct {
	requests int64
	lastUsed time.Time
}

// Store is the in-process handle on the config document. It is safe for
// concurrent use, and serialises against other processes with an advisory
// file lock held across each read-modify-write cycle.
type Store struct {
	mu   sync.RWMutex
	dir  string
	path string
	cfg  *Config

	pending   map[string]*usageDelta
	lastFlush time.Time

	// loadedMod/loadedSize identify the file revision currently in memory, so
	// a write by another process (a `key revoke` while the server runs) can be
	// noticed without re-reading the file on every lookup.
	loadedMod  time.Time
	loadedSize int64
}

// Load opens the config in dir. A missing file is normal (first run) and
// yields an empty config; malformed JSON is fatal, because silently starting
// from scratch would revoke every key the user owns without saying so.
func Load(dir string) (*Store, error) {
	s := &Store{
		dir:       dir,
		path:      filepath.Join(dir, ConfigFileName),
		pending:   map[string]*usageDelta{},
		lastFlush: time.Now(),
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir reports the directory this store lives in, so callers (the secret box,
// for instance) can place sibling files alongside it.
func (s *Store) Dir() string { return s.dir }

// Path reports the config file location, which is what error messages and
// the wizard show the user.
func (s *Store) Path() string { return s.path }

// Addr returns the configured default listen address, if any.
func (s *Store) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Addr
}

// SetAddr records the default listen address and persists it.
func (s *Store) SetAddr(addr string) error {
	return s.mutate(func(c *Config) error {
		c.Addr = addr
		return nil
	})
}

// reload replaces the in-memory config from disk.
func (s *Store) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

func (s *Store) reloadLocked() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.cfg = &Config{Version: CurrentVersion}
			return nil
		}
		return fmt.Errorf("store: read %s: %w", s.path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("store: %s is not valid JSON (refusing to overwrite it): %w", s.path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = CurrentVersion
	}
	if cfg.Keys == nil {
		cfg.Keys = []*Key{}
	}
	s.cfg = &cfg
	s.noteRevisionLocked()
	s.applyPendingLocked()
	return nil
}

// noteRevisionLocked records which revision of the file is now in memory.
func (s *Store) noteRevisionLocked() {
	if fi, err := os.Stat(s.path); err == nil {
		s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size()
	} else {
		s.loadedMod, s.loadedSize = time.Time{}, 0
	}
}

// refreshIfChanged reloads the config when the file on disk is a different
// revision than the one in memory.
//
// Revocation is a security control, so it has to take effect in a running
// server the moment `key revoke` writes the file — not whenever this process
// happens to reload for its own reasons. A stat per lookup is cheap next to
// the work a request actually does.
func (s *Store) refreshIfChanged() {
	fi, err := os.Stat(s.path)
	if err != nil {
		return // a missing file leaves the last known good config in place
	}
	s.mu.RLock()
	same := fi.ModTime().Equal(s.loadedMod) && fi.Size() == s.loadedSize
	s.mu.RUnlock()
	if same {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the write lock: another goroutine may have just reloaded.
	if fi2, err := os.Stat(s.path); err == nil &&
		fi2.ModTime().Equal(s.loadedMod) && fi2.Size() == s.loadedSize {
		return
	}
	// A failed reload (a half-written or corrupt file) keeps the config we
	// already have rather than dropping every key.
	_ = s.reloadLocked()
}

// applyPendingLocked folds unflushed usage counters back onto a freshly read
// config, so a reload triggered by an unrelated mutation does not drop them.
func (s *Store) applyPendingLocked() {
	if len(s.pending) == 0 {
		return
	}
	for _, k := range s.cfg.Keys {
		d, ok := s.pending[k.ID]
		if !ok {
			continue
		}
		k.Requests += d.requests
		if k.LastUsedAt == nil || d.lastUsed.After(*k.LastUsedAt) {
			t := d.lastUsed
			k.LastUsedAt = &t
		}
	}
}

// mutate runs fn under both the process mutex and the cross-process file
// lock, re-reading the config first so a concurrent `key create` in another
// process is not clobbered, and writing the result atomically.
func (s *Store) mutate(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock := s.flock()
	defer unlock()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	if fn != nil {
		if err := fn(s.cfg); err != nil {
			return err
		}
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.pending = map[string]*usageDelta{}
	s.lastFlush = time.Now()
	return nil
}

// flock takes an exclusive advisory lock on the lock file. Advisory locking
// is best effort: on a filesystem that does not support it we would rather
// serve the user with a small race window than refuse to run at all.
func (s *Store) flock() func() {
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

// saveLocked writes the config atomically: a temp file in the same directory
// (so the rename cannot cross filesystems), fsynced before the rename, so a
// crash leaves either the old document or the new one and never a partial.
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", s.dir, err)
	}
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode config: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(s.dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("store: create temp file in %s: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	// CreateTemp makes 0600 already, but say so explicitly: the file holds
	// key hashes and encrypted provider credentials.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("store: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("store: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: replace %s: %w", s.path, err)
	}
	return nil
}

// Save flushes the current state, including any throttled usage counters.
func (s *Store) Save() error { return s.mutate(nil) }

// Add stores a new key. The id must be unique: everything downstream treats
// it as a primary key.
func (s *Store) Add(k *Key) error {
	if k == nil || k.ID == "" {
		return errors.New("store: key must have an id")
	}
	return s.mutate(func(c *Config) error {
		for _, existing := range c.Keys {
			if existing.ID == k.ID {
				return fmt.Errorf("%w: %s", ErrDuplicateID, k.ID)
			}
		}
		cp := *k
		c.Keys = append(c.Keys, &cp)
		return nil
	})
}

// ByID returns a copy of the key with the given id. A copy, because callers
// run concurrently with writers and must not observe a half-updated key.
func (s *Store) ByID(id string) (*Key, bool) {
	s.refreshIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.cfg.Keys {
		if k.ID == id {
			cp := *k
			return &cp, true
		}
	}
	return nil, false
}

// List returns copies of every key, in insertion order.
func (s *Store) List() []*Key {
	s.refreshIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Key, 0, len(s.cfg.Keys))
	for _, k := range s.cfg.Keys {
		cp := *k
		out = append(out, &cp)
	}
	return out
}

// Revoke marks a key unusable but keeps it on disk, so `key list` can still
// explain why a token stopped working.
func (s *Store) Revoke(id string) error {
	return s.mutate(func(c *Config) error {
		for _, k := range c.Keys {
			if k.ID == id {
				k.Revoked = true
				return nil
			}
		}
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	})
}

// Delete removes a key entirely.
func (s *Store) Delete(id string) error {
	return s.mutate(func(c *Config) error {
		for i, k := range c.Keys {
			if k.ID == id {
				c.Keys = append(c.Keys[:i], c.Keys[i+1:]...)
				delete(s.pending, id)
				return nil
			}
		}
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	})
}

// TouchUsage bumps a key's request counter and last-used timestamp. It is on
// the hot path of every proxied request, so the update lands in memory
// immediately and reaches disk at most once per usageFlushInterval; a crash
// can therefore lose up to that much usage history, which is acceptable for
// counters that nothing depends on for correctness.
func (s *Store) TouchUsage(id string) error {
	s.mu.Lock()

	var found *Key
	for _, k := range s.cfg.Keys {
		if k.ID == id {
			found = k
			break
		}
	}
	if found == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	now := time.Now()
	found.Requests++
	t := now
	found.LastUsedAt = &t

	d, ok := s.pending[id]
	if !ok {
		d = &usageDelta{}
		s.pending[id] = d
	}
	d.requests++
	d.lastUsed = now

	due := now.Sub(s.lastFlush) >= usageFlushInterval
	s.mu.Unlock()

	if !due {
		return nil
	}
	return s.Save()
}
