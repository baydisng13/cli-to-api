//go:build darwin

package secret

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os/exec"
	"os/user"
	"strings"
)

// keychainService is the generic-password service name under which the data
// key lives in the macOS Keychain.
const keychainService = "cli-to-api"

// platformKey reads the data key from the macOS Keychain, creating it if it is
// not there yet. Errors are returned bare: the caller falls back to the key
// file, and the error text must never carry key material.
func platformKey() ([]byte, error) {
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
