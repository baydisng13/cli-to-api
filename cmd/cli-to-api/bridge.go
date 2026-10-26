package main

import (
	"errors"
	"fmt"
	"time"

	"cli-to-api/internal/api"
	"cli-to-api/internal/auth"
	"cli-to-api/internal/secret"
	"cli-to-api/internal/session"
	"cli-to-api/internal/store"
)

// authBridge adapts auth.Verifier to the interface the HTTP layer declares.
// The two packages deliberately do not import each other: api states what it
// needs, auth implements the credential rules, and this file translates
// between their error vocabularies.
type authBridge struct {
	v *auth.Verifier
}

func (a authBridge) Authenticate(token string) (*store.Key, error) {
	k, err := a.v.Resolve(token)
	if err == nil {
		return k, nil
	}
	switch {
	case errors.Is(err, auth.ErrRevoked):
		return nil, fmt.Errorf("%w", api.ErrRevokedKey)
	case errors.Is(err, auth.ErrInvalidKey):
		return nil, fmt.Errorf("%w", api.ErrInvalidKey)
	default:
		return nil, err
	}
}

func (a authBridge) Allowed(k *store.Key, model string) (string, error) {
	resolved, err := a.v.Allowed(k, model)
	if err == nil {
		return resolved, nil
	}
	if errors.Is(err, auth.ErrModelNotAllowed) {
		// Keep the original message (it names the offending model) while
		// giving the HTTP layer a sentinel it can map to 403.
		return "", fmt.Errorf("%s: %w", err, api.ErrModelNotAllowed)
	}
	return "", err
}

// optionResolver decrypts a profile's secret options and merges them with the
// plaintext ones, so adapters receive a single flat map and never learn that
// some values were encrypted at rest.
func optionResolver(box secret.Box) func(*store.Key) (map[string]string, error) {
	return func(k *store.Key) (map[string]string, error) {
		merged := make(map[string]string, len(k.Profile.Options)+len(k.Profile.SecretOptions))
		for key, v := range k.Profile.Options {
			merged[key] = v
		}
		for key, sealed := range k.Profile.SecretOptions {
			plain, err := box.Open(sealed)
			if err != nil {
				// Naming the option is safe; its value is not.
				return nil, fmt.Errorf("decrypting option %q for key %s: %w", key, shortID(k.ID), err)
			}
			merged[key] = plain
		}
		return merged, nil
	}
}

// convBridge adapts the session store to the yes/no ownership question the
// HTTP layer asks. The api package deliberately never sees a session record:
// it only needs to know whether this key may continue this conversation.
type convBridge struct {
	s *session.Sessions
}

func (c convBridge) Resume(convID, keyID string) (bool, error) {
	if _, err := c.s.Lookup(convID, keyID); err != nil {
		if errors.Is(err, session.ErrNotFound) {
			// Unknown, expired, and owned-by-someone-else are the same answer
			// on purpose: a guessed id must not reveal that it is real.
			return false, nil
		}
		return false, err
	}
	if err := c.s.Touch(convID); err != nil {
		// Losing a timestamp update is not worth refusing the request.
		return true, nil
	}
	return true, nil
}

func (c convBridge) Remember(convID, keyID, adapterID, model string) error {
	now := time.Now()
	return c.s.Bind(session.Session{
		ID:         convID,
		KeyID:      keyID,
		Adapter:    adapterID,
		Model:      model,
		CreatedAt:  now,
		LastUsedAt: now,
	})
}
