// Package store persists keys and their profiles to disk so they survive a
// restart. This file is the shared contract; the implementation lives
// alongside it.
package store

import "time"

// Profile is everything a key needs in order to serve a request: which
// backend, which models that key may ask for, and the adapter options.
type Profile struct {
	Adapter      string            `json:"adapter"`                 // adapter.Adapter.ID()
	Models       []string          `json:"models"`                  // allowed model ids; empty means "any"
	DefaultModel string            `json:"default_model,omitempty"` // used when the request names none
	Options      map[string]string `json:"options,omitempty"`       // plaintext adapter options
	// SecretOptions holds adapter options marked Secret, encrypted at rest.
	// Values are opaque ciphertext; use the secret package to open them.
	SecretOptions map[string]string `json:"secret_options,omitempty"`
	Concurrency   int               `json:"concurrency,omitempty"` // max in-flight requests; 0 means default (1)
	TimeoutSec    int               `json:"timeout_sec,omitempty"` // per-request cap; 0 means default
}

// Key is one issued API key. The secret itself is never stored: only its
// hash, and the id that lets a presented key be looked up in one step.
type Key struct {
	ID         string     `json:"id"`   // short public id embedded in the key, e.g. "a7f3e91c"
	Name       string     `json:"name"` // human label from the wizard
	Hash       string     `json:"hash"` // encoded hash of the secret half
	Profile    Profile    `json:"profile"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Requests   int64      `json:"requests"`
	Revoked    bool       `json:"revoked,omitempty"`
}

// Config is the whole on-disk document.
type Config struct {
	Version int    `json:"version"`
	Addr    string `json:"addr,omitempty"` // default listen address for `start`
	Keys    []*Key `json:"keys"`
}
