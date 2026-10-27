package secret

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, keyLen)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := NewBoxWithKey(testKey())
	for _, pt := range []string{"", "sk-abc123", strings.Repeat("x", 4096), "ünïcödé \n\t"} {
		ct, err := b.Seal(pt)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if ct == pt && pt != "" {
			t.Fatal("ciphertext equals plaintext")
		}
		if !strings.HasPrefix(ct, prefixV1) {
			t.Fatalf("missing version prefix: %q", ct)
		}
		got, err := b.Open(ct)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got != pt {
			t.Fatalf("round trip: got %q want %q", got, pt)
		}
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	b := NewBoxWithKey(testKey())
	a, _ := b.Seal("same")
	c, _ := b.Seal("same")
	if a == c {
		t.Fatal("two seals of the same plaintext produced identical ciphertext")
	}
}

func TestOpenTamperedFails(t *testing.T) {
	b := NewBoxWithKey(testKey())
	ct, err := b.Seal("secret-value")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ct, prefixV1))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	bad := prefixV1 + base64.StdEncoding.EncodeToString(raw)

	got, err := b.Open(bad)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered ciphertext: got err %v, want ErrDecrypt", err)
	}
	if got != "" {
		t.Fatalf("tampered ciphertext returned %q", got)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	ct, err := NewBoxWithKey(testKey()).Seal("secret-value")
	if err != nil {
		t.Fatal(err)
	}
	other := make([]byte, keyLen)
	if _, err := NewBoxWithKey(other).Open(ct); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
}

func TestOpenUnknownPrefixFails(t *testing.T) {
	b := NewBoxWithKey(testKey())
	for _, in := range []string{"", "plaintext", "v2:AAAA", "v1:not-base64!!", prefixV1 + base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := b.Open(in); !errors.Is(err, ErrUnknownFormat) {
			t.Fatalf("Open(%q): got %v, want ErrUnknownFormat", in, err)
		}
	}
}

func TestNewBoxWithKeyRejectsWrongSize(t *testing.T) {
	if _, err := NewBoxWithKey([]byte("too short")).Seal("x"); err == nil {
		t.Fatal("expected error for a short key")
	}
}

func TestFileKeyCreatesModeThenReuses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")

	k1, err := fileKey(dir)
	if err != nil {
		t.Fatalf("fileKey: %v", err)
	}
	if len(k1) != keyLen {
		t.Fatalf("key length %d, want %d", len(k1), keyLen)
	}

	path := filepath.Join(dir, keyFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows maps a mode onto the read-only attribute alone, so the 0600
	// assertion is meaningful on unix only; see perm_windows.go.
	if runtime.GOOS != "windows" {
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Fatalf("key file mode %o, want 600", got)
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := di.Mode().Perm(); got != 0o700 {
			t.Fatalf("config dir mode %o, want 700", got)
		}
	}

	k2, err := fileKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) != string(k2) {
		t.Fatal("second call regenerated the data key")
	}
}

func TestFileKeyRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fileKey(dir); err == nil {
		t.Fatal("expected an error for a malformed key file")
	}
}

// TestOpenPicksABackendDeterministically checks the property the build tags
// exist for: two Opens on the same machine and config dir must agree on the
// key, so a value sealed by one run is readable by the next.
func TestOpenPicksABackendDeterministically(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// Open would read, and on a fresh account write, the real login
		// keychain of whoever runs the suite. Not a test's business.
		t.Skip("darwin Open touches the user's login keychain")
	}
	dir := t.TempDir()

	b1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ct, err := b1.Seal("hunter2")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	b2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	got, err := b2.Open(ct)
	if err != nil {
		t.Fatalf("Open(ciphertext): %v", err)
	}
	if got != "hunter2" {
		t.Errorf("round trip = %q, want hunter2", got)
	}
}

// TestPlatformKeyIsAbsentOffDarwin pins the explicit, compile-time choice:
// only darwin talks to an OS keystore, everything else uses the key file and
// says so rather than discovering it through a failed call.
func TestPlatformKeyIsAbsentOffDarwin(t *testing.T) {
	_, err := platformKey()
	if runtime.GOOS == "darwin" {
		return // may succeed or fail depending on the keychain; both are fine
	}
	if !errors.Is(err, errNoKeyring) {
		t.Fatalf("platformKey() error = %v, want errNoKeyring", err)
	}
}
