package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func mustLoad(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	return s
}

func key(id string) *Key { return &Key{ID: id, Name: "key-" + id, Hash: "pbkdf2$sha256$1$AA$AA"} }

func TestDefaultDir(t *testing.T) {
	t.Setenv("CLI_TO_API_HOME", "/tmp/explicit")
	if d, err := DefaultDir(); err != nil || d != "/tmp/explicit" {
		t.Fatalf("override: got %q, %v", d, err)
	}

	t.Setenv("CLI_TO_API_HOME", "")
	if runtime.GOOS == "windows" {
		// Windows has no XDG convention: the directory comes from
		// os.UserConfigDir (%AppData%), and only the override above applies.
		d, err := DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(d) != "cli-to-api" || !filepath.IsAbs(d) {
			t.Fatalf("windows default: got %q", d)
		}
		return
	}

	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	want := filepath.Join("/tmp/xdg", "cli-to-api")
	if d, err := DefaultDir(); err != nil || d != want {
		t.Fatalf("xdg: got %q, %v", d, err)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	d, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(d, filepath.Join(".config", "cli-to-api")) {
		t.Fatalf("home fallback: got %q", d)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	s := mustLoad(t, dir)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("expected no keys, got %d", len(got))
	}
	if s.Dir() != dir {
		t.Fatalf("Dir() = %q", s.Dir())
	}
	// Loading must not create anything; the dir appears on first write.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Load created %s", dir)
	}
	if err := s.Add(key("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFileName)); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestAddListRevokeDelete(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)

	for _, id := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		if err := s.Add(key(id)); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	if got := s.List(); len(got) != 3 || got[0].ID != "aaaaaaaa" || got[2].ID != "cccccccc" {
		t.Fatalf("List: %+v", got)
	}

	k, ok := s.ByID("bbbbbbbb")
	if !ok || k.Name != "key-bbbbbbbb" {
		t.Fatalf("ByID: %+v, %v", k, ok)
	}
	if _, ok := s.ByID("nope"); ok {
		t.Fatal("ByID found a key that does not exist")
	}

	// ByID hands out a copy, so a caller cannot mutate stored state.
	k.Revoked = true
	if again, _ := s.ByID("bbbbbbbb"); again.Revoked {
		t.Fatal("ByID returned a live pointer into the store")
	}

	if err := s.Revoke("bbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.ByID("bbbbbbbb"); !again.Revoked {
		t.Fatal("Revoke did not stick")
	}
	if err := s.Revoke("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revoke(missing): %v", err)
	}

	if err := s.Delete("aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ByID("aaaaaaaa"); ok {
		t.Fatal("Delete did not remove the key")
	}
	if err := s.Delete("aaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing): %v", err)
	}

	// Everything must survive a reopen.
	s2 := mustLoad(t, dir)
	if got := s2.List(); len(got) != 2 {
		t.Fatalf("after reload: %d keys", len(got))
	}
	if k, _ := s2.ByID("bbbbbbbb"); k == nil || !k.Revoked {
		t.Fatal("revocation did not persist")
	}
}

func TestAddRejectsDuplicateID(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	if err := s.Add(key("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(key("aaaaaaaa")); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate id: got %v, want ErrDuplicateID", err)
	}
	if got := s.List(); len(got) != 1 {
		t.Fatalf("duplicate was stored anyway: %d keys", len(got))
	}
	if err := s.Add(&Key{}); err == nil {
		t.Fatal("expected an error for a key with no id")
	}
}

func TestProfileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)
	k := key("aaaaaaaa")
	k.Profile = Profile{
		Adapter:       "claude",
		Models:        []string{"m1", "m2"},
		DefaultModel:  "m1",
		Options:       map[string]string{"bin": "claude"},
		SecretOptions: map[string]string{"api_key": "v1:AAAA"},
		Concurrency:   2,
		TimeoutSec:    90,
	}
	if err := s.Add(k); err != nil {
		t.Fatal(err)
	}
	got, _ := mustLoad(t, dir).ByID("aaaaaaaa")
	if got.Profile.Adapter != "claude" || got.Profile.DefaultModel != "m1" ||
		len(got.Profile.Models) != 2 || got.Profile.SecretOptions["api_key"] != "v1:AAAA" ||
		got.Profile.Concurrency != 2 || got.Profile.TimeoutSec != 90 {
		t.Fatalf("profile did not round trip: %+v", got.Profile)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	s := mustLoad(t, dir)
	if err := s.Add(key("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode %o, want 600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode %o, want 700", got)
	}

	// Repeated saves must not leave temp files behind or widen the mode.
	for i := 0; i < 3; i++ {
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	fi, _ = os.Stat(filepath.Join(dir, ConfigFileName))
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode after resave: %o", got)
	}
}

func TestAtomicSaveProducesValidJSON(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if err := s.Add(key(id)); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("on-disk config is not valid JSON: %v", err)
	}
	if cfg.Version != CurrentVersion || len(cfg.Keys) != 2 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestCorruptJSONIsAHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted corrupt JSON")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the file: %v", err)
	}
	// Above all, the bad file must still be there for the user to repair.
	raw, rerr := os.ReadFile(path)
	if rerr != nil || string(raw) != "{not json" {
		t.Fatalf("Load touched the corrupt file: %q, %v", raw, rerr)
	}
}

func TestSetAddr(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)
	if err := s.SetAddr("127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, dir).Addr(); got != "127.0.0.1:8080" {
		t.Fatalf("Addr = %q", got)
	}
}

func TestTouchUsage(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)
	if err := s.Add(key("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.TouchUsage("aaaaaaaa"); err != nil {
			t.Fatal(err)
		}
	}
	k, _ := s.ByID("aaaaaaaa")
	if k.Requests != 5 {
		t.Fatalf("Requests = %d, want 5", k.Requests)
	}
	if k.LastUsedAt == nil {
		t.Fatal("LastUsedAt not set")
	}
	if err := s.TouchUsage("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TouchUsage(missing): %v", err)
	}

	// Writes are throttled, so the counters only have to reach disk once
	// Save() is called explicitly.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	k2, _ := mustLoad(t, dir).ByID("aaaaaaaa")
	if k2.Requests != 5 || k2.LastUsedAt == nil {
		t.Fatalf("usage did not persist: %+v", k2)
	}
}

func TestConcurrentAddKeepsEveryKey(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a'+i/16)) + string(rune('a'+i%16)) + "aaaaaa"
			if err := s.Add(&Key{ID: id, Name: id}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Add: %v", err)
	}

	if got := len(s.List()); got != n {
		t.Fatalf("in memory: %d keys, want %d", got, n)
	}
	if got := len(mustLoad(t, dir).List()); got != n {
		t.Fatalf("on disk: %d keys, want %d", got, n)
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	dir := t.TempDir()
	s := mustLoad(t, dir)
	if err := s.Add(key("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.ByID("aaaaaaaa")
				s.List()
				_ = s.TouchUsage("aaaaaaaa")
			}
		}()
	}
	wg.Wait()

	k, _ := s.ByID("aaaaaaaa")
	if k.Requests != 8*50 {
		t.Fatalf("Requests = %d, want %d", k.Requests, 8*50)
	}
}

// A second Store over the same directory stands in for a second process:
// each mutation re-reads before writing, so neither view loses the other's
// keys.
func TestTwoStoresOverOneDirectory(t *testing.T) {
	dir := t.TempDir()
	a := mustLoad(t, dir)
	b := mustLoad(t, dir)

	if err := a.Add(key("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(key("bbbbbbbb")); err != nil {
		t.Fatal(err)
	}
	if got := len(mustLoad(t, dir).List()); got != 2 {
		t.Fatalf("on disk: %d keys, want 2", got)
	}
}

// A revoke performed by another process (the `key revoke` command, while the
// server runs) must be visible to the server's very next lookup. This failed
// in an end-to-end run: the server kept serving a revoked key until something
// else happened to trigger a reload.
func TestExternalRevokeVisibleImmediately(t *testing.T) {
	dir := t.TempDir()

	server, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	k := &Key{ID: "abcd1234", Name: "e2e", Hash: "x"}
	if err := server.Add(k); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := server.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got, ok := server.ByID("abcd1234"); !ok || got.Revoked {
		t.Fatalf("key should start usable, got %+v ok=%v", got, ok)
	}

	// A separate Store handle stands in for the separate `key revoke` process.
	cli, err := Load(dir)
	if err != nil {
		t.Fatalf("Load (second handle): %v", err)
	}
	if err := cli.Revoke("abcd1234"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := cli.Save(); err != nil {
		t.Fatalf("Save (second handle): %v", err)
	}

	got, ok := server.ByID("abcd1234")
	if !ok {
		t.Fatal("key vanished after an external revoke")
	}
	if !got.Revoked {
		t.Error("server handle still reports the key as usable after an external revoke")
	}
}

// A corrupt or half-written file must not wipe the keys already in memory.
func TestRefreshKeepsConfigWhenFileGoesBad(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Add(&Key{ID: "keep0001", Name: "keep", Hash: "x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupting config: %v", err)
	}
	if _, ok := s.ByID("keep0001"); !ok {
		t.Error("a corrupt file on disk dropped the in-memory keys")
	}
}
