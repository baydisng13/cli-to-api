// Package auth issues, stores and checks the bearer tokens that gate the
// local OpenAI-compatible endpoint.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cli-to-api/internal/store"
)

const (
	// Prefix makes a leaked token greppable in logs and repos, the way
	// provider keys are.
	Prefix = "cta_live_"

	// idLen is the public half: 8 lowercase hex characters, enough to stay
	// unique across the handful of keys one machine will ever hold while
	// being short enough to print in `key list`.
	idLen = 8

	// secretBytes is the private half. 256 bits of entropy is the reason the
	// iteration count below can stay small.
	secretBytes = 32

	// hashIterations is defense in depth, not a password KDF. The secret is
	// 256 random bits, so no amount of stretching changes the attacker's
	// best strategy; the iteration count exists only to blunt an offline
	// attack on a stolen config file. Verification runs on every request,
	// so it is tuned to stay in the single-digit milliseconds.
	hashIterations = 10000

	hashKeyLen = 32
	saltBytes  = 16

	hashScheme = "pbkdf2"
	hashPRF    = "sha256"
)

var (
	// ErrInvalidKey covers a malformed token, an unknown id, and a wrong
	// secret alike: the caller gets one answer, so probing cannot tell which
	// ids exist.
	ErrInvalidKey = errors.New("auth: invalid api key")

	// ErrRevoked is separate because the operator revoked this key on
	// purpose and a precise message saves a support round trip. It only
	// reaches a caller who already presented a valid key id.
	ErrRevoked = errors.New("auth: api key has been revoked")

	// ErrModelNotAllowed means the key is good but its profile does not
	// cover the requested model.
	ErrModelNotAllowed = errors.New("auth: model not allowed for this key")
)

// b64 is unpadded base64url throughout, so a token is a single selectable
// word with no '=' to be mangled by a shell or a URL.
var b64 = base64.RawURLEncoding

// Generate mints a new key. The plaintext is returned exactly once; only the
// id and hash are ever persisted.
func Generate() (plaintext, id, hash string, err error) {
	idRaw := make([]byte, idLen/2)
	if _, err = rand.Read(idRaw); err != nil {
		return "", "", "", fmt.Errorf("auth: generate id: %w", err)
	}
	id = hex.EncodeToString(idRaw)

	secretRaw := make([]byte, secretBytes)
	if _, err = rand.Read(secretRaw); err != nil {
		return "", "", "", fmt.Errorf("auth: generate secret: %w", err)
	}
	secret := b64.EncodeToString(secretRaw)

	hash, err = Hash(secret)
	if err != nil {
		return "", "", "", err
	}
	return Prefix + id + "_" + secret, id, hash, nil
}

// Hash encodes the stored form of a secret. The scheme, PRF, iteration count
// and salt all travel with the digest so the parameters can be raised later
// without invalidating keys issued today.
func Hash(secret string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	dk, err := pbkdf2.Key(sha256.New, secret, salt, hashIterations, hashKeyLen)
	if err != nil {
		return "", fmt.Errorf("auth: derive hash: %w", err)
	}
	return strings.Join([]string{
		hashScheme,
		hashPRF,
		strconv.Itoa(hashIterations),
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(dk),
	}, "$"), nil
}

// Parse splits a presented bearer token into its public id and secret halves.
// It never touches the store, so an obviously malformed token costs nothing.
func Parse(presented string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(presented, Prefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || secret == "" || len(id) != idLen {
		return "", "", false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", "", false
		}
	}
	return id, secret, true
}

// Verify checks a secret against a stored hash.
func Verify(hash, secret string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 5 || parts[0] != hashScheme || parts[1] != hashPRF {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, secret, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Lookup is the slice of the store the verifier needs. Keeping it narrow
// lets the API layer inject a cache, and lets tests avoid the filesystem.
type Lookup interface {
	ByID(id string) (*store.Key, bool)
}

// Verifier resolves presented bearer tokens to keys.
type Verifier struct {
	keys Lookup

	// decoyHash is verified when no key matches, so an unknown id costs
	// roughly the same wall time as a wrong secret. It is derived once at
	// construction from a throwaway secret.
	decoyHash string
}

// NewVerifier wires a verifier to a key source.
func NewVerifier(keys Lookup) *Verifier {
	h, err := Hash("decoy")
	if err != nil {
		// Hash only fails if the system RNG does; leaving decoyHash empty
		// simply skips the timing equalizer rather than breaking auth.
		h = ""
	}
	return &Verifier{keys: keys, decoyHash: h}
}

// Resolve turns a presented token into the key it names.
func (v *Verifier) Resolve(presented string) (*store.Key, error) {
	id, secret, ok := Parse(presented)
	if !ok {
		return nil, ErrInvalidKey
	}
	k, found := v.keys.ByID(id)
	if !found {
		v.burn(secret)
		return nil, ErrInvalidKey
	}
	// Revocation is checked before the hash so a revoked key cannot be used
	// as an oracle for whether its secret is still correct.
	if k.Revoked {
		v.burn(secret)
		return nil, ErrRevoked
	}
	if !Verify(k.Hash, secret) {
		return nil, ErrInvalidKey
	}
	return k, nil
}

// burn spends the same work a real verification would, so the failure paths
// do not leak which one was taken.
func (v *Verifier) burn(secret string) {
	if v.decoyHash != "" {
		Verify(v.decoyHash, secret)
	}
}

// Allowed resolves the model a request asked for against the key's profile.
// An empty Models list means the profile places no restriction, which is the
// sensible default for a personal CLI-backed adapter that may expose models
// the wizard never enumerated.
func (v *Verifier) Allowed(k *store.Key, model string) (resolved string, err error) {
	if k == nil {
		return "", ErrInvalidKey
	}
	if model == "" {
		return k.Profile.DefaultModel, nil
	}
	if len(k.Profile.Models) == 0 {
		return model, nil
	}
	for _, m := range k.Profile.Models {
		if m == model {
			return model, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
}
