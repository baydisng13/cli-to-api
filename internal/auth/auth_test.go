package auth

import (
	"errors"
	"strings"
	"testing"

	"cli-to-api/internal/store"
)

func TestGenerateParseVerifyRoundTrip(t *testing.T) {
	pt, id, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(pt, Prefix) {
		t.Fatalf("token %q lacks prefix %q", pt, Prefix)
	}
	if len(id) != idLen {
		t.Fatalf("id %q length %d, want %d", id, len(id), idLen)
	}
	if strings.Contains(hash, pt) {
		t.Fatal("hash contains the plaintext token")
	}

	gotID, secret, ok := Parse(pt)
	if !ok {
		t.Fatalf("Parse(%q) failed", pt)
	}
	if gotID != id {
		t.Fatalf("parsed id %q, want %q", gotID, id)
	}
	if !Verify(hash, secret) {
		t.Fatal("Verify rejected the freshly generated secret")
	}
	if Verify(hash, secret+"x") {
		t.Fatal("Verify accepted a wrong secret")
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pt, id, _, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if seen[pt] {
			t.Fatal("duplicate token")
		}
		seen[pt] = true
		if _, _, ok := Parse(Prefix + id + "_x"); !ok {
			t.Fatalf("generated id %q does not parse", id)
		}
	}
}

func TestHashFormat(t *testing.T) {
	h, err := Hash("some-secret")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 5 {
		t.Fatalf("hash %q has %d fields, want 5", h, len(parts))
	}
	if parts[0] != "pbkdf2" || parts[1] != "sha256" {
		t.Fatalf("unexpected scheme fields: %q", h)
	}
	if parts[2] != "10000" {
		t.Fatalf("iteration field %q", parts[2])
	}
	if !Verify(h, "some-secret") {
		t.Fatal("Verify failed on its own output")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{
		"", "pbkdf2$sha256$10000", "pbkdf2$sha512$10000$AAAA$AAAA",
		"scrypt$sha256$10000$AAAA$AAAA", "pbkdf2$sha256$abc$AAAA$AAAA",
		"pbkdf2$sha256$10000$!!!$AAAA", "pbkdf2$sha256$10000$AAAA$!!!",
		"pbkdf2$sha256$0$AAAA$AAAA", "pbkdf2$sha256$10000$AAAA$",
	} {
		if Verify(h, "x") {
			t.Fatalf("Verify accepted malformed hash %q", h)
		}
	}
}

func TestParseRejectsBadTokens(t *testing.T) {
	for _, in := range []string{
		"", "nope", "cta_live_", "cta_live_abc_secret",
		"cta_live_abcdef12", "cta_live_abcdef12_", "cta_live_ABCDEF12_secret",
		"cta_live_abcdefgh_secret", "cta_test_abcdef12_secret",
	} {
		if _, _, ok := Parse(in); ok {
			t.Fatalf("Parse accepted %q", in)
		}
	}
}

// memStore is a Lookup backed by a map, so auth tests never touch disk.
type memStore map[string]*store.Key

func (m memStore) ByID(id string) (*store.Key, bool) {
	k, ok := m[id]
	if !ok {
		return nil, false
	}
	cp := *k
	return &cp, true
}

func TestVerifierResolve(t *testing.T) {
	pt, id, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	m := memStore{id: {ID: id, Name: "dev", Hash: hash}}
	v := NewVerifier(m)

	k, err := v.Resolve(pt)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if k.ID != id {
		t.Fatalf("resolved id %q, want %q", k.ID, id)
	}

	if _, err := v.Resolve("garbage"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("malformed token: got %v", err)
	}
	if _, err := v.Resolve(Prefix + "deadbeef_whatever"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("unknown id: got %v", err)
	}
	if _, err := v.Resolve(pt + "x"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("wrong secret: got %v", err)
	}
}

func TestVerifierRejectsRevoked(t *testing.T) {
	pt, id, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	m := memStore{id: {ID: id, Hash: hash, Revoked: true}}
	v := NewVerifier(m)

	k, err := v.Resolve(pt)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("got %v, want ErrRevoked", err)
	}
	if k != nil {
		t.Fatal("revoked key was returned anyway")
	}
	if errors.Is(err, ErrInvalidKey) {
		t.Fatal("revoked must be distinguishable from invalid")
	}
}

func TestAllowed(t *testing.T) {
	v := NewVerifier(memStore{})

	anyModel := &store.Key{Profile: store.Profile{}}
	if got, err := v.Allowed(anyModel, "whatever"); err != nil || got != "whatever" {
		t.Fatalf("empty Models list: got %q, %v", got, err)
	}
	if got, err := v.Allowed(anyModel, ""); err != nil || got != "" {
		t.Fatalf("no model, no default: got %q, %v", got, err)
	}

	withDefault := &store.Key{Profile: store.Profile{
		Models:       []string{"claude-sonnet", "claude-opus"},
		DefaultModel: "claude-sonnet",
	}}
	if got, err := v.Allowed(withDefault, ""); err != nil || got != "claude-sonnet" {
		t.Fatalf("default model: got %q, %v", got, err)
	}
	if got, err := v.Allowed(withDefault, "claude-opus"); err != nil || got != "claude-opus" {
		t.Fatalf("listed model: got %q, %v", got, err)
	}
	got, err := v.Allowed(withDefault, "gpt-4")
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("unlisted model: got %v, want ErrModelNotAllowed", err)
	}
	if got != "" {
		t.Fatalf("rejected model still resolved to %q", got)
	}

	if _, err := v.Allowed(nil, "x"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("nil key: got %v", err)
	}
}
