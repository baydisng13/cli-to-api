// Package secret encrypts adapter options that are marked secret, such as an
// upstream provider API key. These values must be handed back to the adapter
// verbatim at request time, so they are encrypted and recoverable rather than
// hashed.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const (
	// keychainService is the generic-password service name under which the
	// data key lives in the macOS Keychain.
	keychainService = "cli-to-api"

	// keyFileName is the fallback location, relative to the config dir.
	keyFileName = "masterkey"

	// prefixV1 tags the only ciphertext layout we currently emit. Keeping a
	// version in the string means a future scheme can be told apart from this
	// one instead of failing as corruption.
	prefixV1 = "v1:"

	keyLen   = 32 // AES-256
	nonceLen = 12 // GCM standard nonce size
)

var (
	// ErrUnknownFormat means the stored value was not produced by any scheme
	// this build understands. It is deliberately distinct from a decryption
	// failure so an operator can tell a stale config from a wrong key.
	ErrUnknownFormat = errors.New("secret: unrecognized ciphertext format")

	// ErrDecrypt covers both tampering and a mismatched data key; GCM cannot
	// distinguish the two, and neither should the message.
	ErrDecrypt = errors.New("secret: cannot decrypt value (wrong key or corrupted data)")
)

// Box seals and opens individual secret values.
type Box interface {
	Seal(plaintext string) (string, error)
	Open(ciphertext string) (string, error)
}

type box struct {
	aead cipher.AEAD
}

// NewBoxWithKey builds a Box directly from a 32-byte data key. It touches no
// Keychain and no disk, which is what makes the package testable.
func NewBoxWithKey(key []byte) Box {
	if len(key) != keyLen {
		// A wrong-sized key is a programming error, not a runtime condition;
		// returning a Box that fails every call keeps the signature simple
		// while still refusing to encrypt under a bad key.
		return brokenBox{fmt.Errorf("secret: data key must be %d bytes, got %d", keyLen, len(key))}
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return brokenBox{err}
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return brokenBox{err}
	}
	return &box{aead: aead}
}

type brokenBox struct{ err error }

func (b brokenBox) Seal(string) (string, error) { return "", b.err }
func (b brokenBox) Open(string) (string, error) { return "", b.err }

func (b *box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	// The nonce is prepended so each value is self-describing; GCM only
	// requires that a nonce never repeat under one key, not that it be secret.
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefixV1 + base64.StdEncoding.EncodeToString(out), nil
}

func (b *box) Open(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, prefixV1) {
		return "", ErrUnknownFormat
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, prefixV1))
	if err != nil {
		return "", ErrUnknownFormat
	}
	if len(raw) < nonceLen {
		return "", ErrUnknownFormat
	}
	nonce, body := raw[:nonceLen], raw[nonceLen:]
	pt, err := b.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(pt), nil
}

// Open returns a Box backed by the machine's data key, creating that key on
// first use. The Keychain is preferred because it survives a config-dir wipe
// and is protected by the login session; the key file exists so the tool still
// works on Linux, in CI, and over SSH where `security` is absent or blocked.
func Open(dir string) (Box, error) {
	if key, err := keychainKey(); err == nil {
		return NewBoxWithKey(key), nil
	}
	key, err := fileKey(dir)
	if err != nil {
		return nil, err
	}
	return NewBoxWithKey(key), nil
}

// keychainKey reads the data key from the macOS Keychain, creating it if it is
// not there yet. Errors are returned bare: callers fall back, and the error
// text must never carry key material.
func keychainKey() ([]byte, error) {
	if _, err := exec.LookPath("security"); err != nil {
		return nil, err
	}
	acct, err := currentAccount()
	if err != nil {
		return nil, err
	}

	out, err := exec.Command("security", "find-generic-password",
		"-a", acct, "-s", keychainService, "-w").Output()
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(out)))
		if derr == nil && len(key) == keyLen {
			return key, nil
		}
		// A malformed entry is not something to silently overwrite: a stale
		// value may still be the only way to read existing ciphertext.
		return nil, errors.New("secret: keychain entry is not a valid data key")
	}

	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// -U updates in place if a (bad) entry somehow exists, avoiding a
	// duplicate-item error. The key is passed as an argument, which is visible
	// in the process list for an instant; the Keychain CLI offers no better
	// path, and the file fallback has the same exposure profile.
	cmd := exec.Command("security", "add-generic-password",
		"-a", acct, "-s", keychainService, "-w", hex.EncodeToString(key), "-U")
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return key, nil
}

func currentAccount() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.Username == "" {
		return "", errors.New("secret: empty username")
	}
	return u.Username, nil
}

// fileKey reads, or creates, the 0600 key file in the config dir.
func fileKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, keyFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(key) != keyLen {
			return nil, fmt.Errorf("secret: %s is not a valid data key", path)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("secret: read %s: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secret: create %s: %w", dir, err)
	}
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// O_EXCL so a racing process cannot have its key silently replaced by
	// ours, which would make its existing ciphertext unreadable.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fileKey(dir)
		}
		return nil, fmt.Errorf("secret: create %s: %w", path, err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key)); err != nil {
		f.Close()
		return nil, fmt.Errorf("secret: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("secret: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("secret: close %s: %w", path, err)
	}
	return key, nil
}
