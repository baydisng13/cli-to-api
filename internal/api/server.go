// Package api is the OpenAI-compatible HTTP surface. It owns the wire format
// and nothing else: authentication, key storage and secret decryption are
// supplied by the caller through Options, so this package stays testable
// without a filesystem, a keyring or a real CLI on PATH.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cli-to-api/internal/adapter"
	"cli-to-api/internal/store"
)

// Authenticator resolves a presented bearer token to the key it belongs to.
//
// Implementations signal failure by returning an error that wraps one of the
// sentinels below; anything else is treated as an internal error. Returning a
// bare error is safe but produces a 500, so wrap deliberately.
type Authenticator interface {
	Authenticate(token string) (*store.Key, error)
	// Allowed resolves the requested model against the key's profile,
	// returning the model to actually run. An empty model means "the client
	// named none", and the implementation is expected to substitute the
	// profile default. Refusals must wrap ErrModelNotAllowed.
	Allowed(k *store.Key, model string) (string, error)
}

// Conversations records which key owns which backend conversation, so a
// conversation id handed back to a client can be resumed later — and only by
// the key that created it.
//
// A conversation id is not a credential: it travels in request bodies and
// logs. Ownership is what makes it safe, which is why Resume takes the key id
// and answers a plain yes/no rather than returning a record to trust.
type Conversations interface {
	// Resume reports whether this key owns this conversation. An id that does
	// not exist and an id owned by another key must be indistinguishable here.
	Resume(convID, keyID string) (bool, error)
	// Remember binds a conversation to the key that created it, or refreshes
	// one that key already owns.
	Remember(convID, keyID, adapterID, model string) error
}

// UsageRecorder is told about each served request. Must be cheap.
type UsageRecorder interface {
	TouchUsage(keyID string) error
}

// Sentinel errors the glue layer wraps so this package can map a failure to
// the right status code and OpenAI error code.
var (
	// ErrNoKey means no credential was presented at all.
	ErrNoKey = errors.New("api: no api key presented")
	// ErrInvalidKey means a credential was presented but matched nothing.
	ErrInvalidKey = errors.New("api: invalid api key")
	// ErrRevokedKey means the key exists but has been revoked.
	ErrRevokedKey = errors.New("api: api key revoked")
	// ErrModelNotAllowed means the key's profile does not permit the model.
	ErrModelNotAllowed = errors.New("api: model not allowed for this key")
)

// Options configures New. Only Auth is strictly required; everything else has
// a working default.
type Options struct {
	// Auth resolves tokens to keys and models to runnable models.
	Auth Authenticator
	// Usage is notified once per authenticated request. Optional.
	Usage UsageRecorder
	// Logger receives one line in and one line out per request. Optional;
	// nil means the standard logger.
	Logger *log.Logger
	// Version is reported by /healthz.
	Version string
	// Timeout is the per-request default, used when the key's profile does
	// not set TimeoutSec. Zero means DefaultTimeout.
	Timeout time.Duration
	// Conversations enables conversation continuity. When nil, a request that
	// asks to resume is refused and no conversation ids are handed out.
	Conversations Conversations

	// ResolveOptions returns the adapter options for a key with secrets
	// already decrypted. nil means "use Profile.Options as-is".
	ResolveOptions func(*store.Key) (map[string]string, error)
}

// DefaultTimeout caps a request when neither the profile nor Options say.
const DefaultTimeout = 10 * time.Minute

// modelsTTL is how long an adapter's enumerated model list is reused. Asking
// a CLI what it can run costs a subprocess and a second of latency.
const modelsTTL = 5 * time.Minute

type server struct {
	opts Options
	log  *log.Logger

	reqCounter atomic.Uint64

	semMu sync.Mutex
	sems  map[string]*semaphore

	modelsMu    sync.Mutex
	modelsCache map[string]modelsEntry
}

type semaphore struct {
	ch chan struct{}
	n  int // size the channel was built for, so a profile change rebuilds it
}

type modelsEntry struct {
	models []adapter.Model
	at     time.Time
}

// New returns the HTTP handler for the whole API surface. Every route is
// served both with and without the /v1 prefix, because clients disagree about
// whether the base URL already contains it.
func New(opts Options) http.Handler {
	s := &server{
		opts:        opts,
		log:         opts.Logger,
		sems:        map[string]*semaphore{},
		modelsCache: map[string]modelsEntry{},
	}
	if s.log == nil {
		s.log = log.Default()
	}
	if s.opts.Timeout <= 0 {
		s.opts.Timeout = DefaultTimeout
	}
	if s.opts.ResolveOptions == nil {
		s.opts.ResolveOptions = func(k *store.Key) (map[string]string, error) {
			return k.Profile.Options, nil
		}
	}

	mux := http.NewServeMux()
	// Plain path patterns, not method patterns: an unknown method on a known
	// path must answer 405, not fall through to the catch-all 404.
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/models", s.handleModels)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleNotFound)

	return s.logging(mux)
}

func (s *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := s.reqCounter.Add(1)
		ctx := context.WithValue(r.Context(), reqIDKey{}, id)
		s.log.Printf("[%d] --> %s %s", id, r.Method, r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
		s.log.Printf("[%d] <-- %s %s (%s)", id, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

type reqIDKey struct{}

func reqID(ctx context.Context) uint64 {
	id, _ := ctx.Value(reqIDKey{}).(uint64)
	return id
}

// ---------- auth ----------

// token pulls the credential out of wherever the client put it. Clients in the
// wild use all three of these.
func token(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := cutPrefixFold(h, "bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	if h := strings.TrimSpace(r.Header.Get("X-Api-Key")); h != "" {
		return h
	}
	return strings.TrimSpace(r.URL.Query().Get("api_key"))
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// authenticate resolves the request's credential and records the touch. It
// writes the error response itself and reports whether the caller may proceed.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (*store.Key, bool) {
	tok := token(r)
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "missing API key: send it as 'Authorization: Bearer <key>', the X-Api-Key header, or ?api_key=", typeInvalidRequest, codeInvalidKey)
		return nil, false
	}
	k, err := s.opts.Auth.Authenticate(tok)
	if err != nil || k == nil {
		switch {
		case errors.Is(err, ErrRevokedKey):
			writeError(w, http.StatusUnauthorized, "this API key has been revoked", typeInvalidRequest, codeRevokedKey)
		case err == nil || errors.Is(err, ErrInvalidKey) || errors.Is(err, ErrNoKey):
			writeError(w, http.StatusUnauthorized, "invalid API key", typeInvalidRequest, codeInvalidKey)
		default:
			s.log.Printf("[%d]     auth error: %v", reqID(r.Context()), err)
			writeError(w, http.StatusInternalServerError, "authentication failed", typeServerError, "")
		}
		return nil, false
	}
	if k.Revoked {
		writeError(w, http.StatusUnauthorized, "this API key has been revoked", typeInvalidRequest, codeRevokedKey)
		return nil, false
	}
	// Accounting is best effort: a bookkeeping failure must not cost the
	// client their answer.
	if s.opts.Usage != nil {
		if err := s.opts.Usage.TouchUsage(k.ID); err != nil {
			s.log.Printf("[%d]     usage record failed for key %s: %v", reqID(r.Context()), k.ID, err)
		}
	}
	return k, true
}

// resolveModel maps the requested model through the key's profile, writing the
// 403 itself when the key may not use it.
func (s *server) resolveModel(w http.ResponseWriter, k *store.Key, requested string) (string, bool) {
	model, err := s.opts.Auth.Allowed(k, requested)
	if err == nil {
		return model, true
	}
	if errors.Is(err, ErrModelNotAllowed) {
		name := requested
		if name == "" {
			name = "(none)"
		}
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("model %q is not allowed for this key; allowed models: %s", name, allowedList(k)),
			typeInvalidRequest, codeModelNotAllowed)
		return "", false
	}
	s.log.Printf("    model resolution failed for key %s: %v", k.ID, err)
	writeError(w, http.StatusInternalServerError, "cannot resolve model: "+err.Error(), typeServerError, "")
	return "", false
}

func allowedList(k *store.Key) string {
	if len(k.Profile.Models) == 0 {
		return "(any model this backend supports)"
	}
	return strings.Join(k.Profile.Models, ", ")
}

// ---------- per-key concurrency ----------

// acquire takes one slot of the key's concurrency budget, queueing rather than
// rejecting when the budget is full: each request may spawn a heavy CLI
// subprocess, and a client that waits is better than a client that retries.
// A client that gives up while queued takes its slot request with it.
func (s *server) acquire(ctx context.Context, k *store.Key) (func(), error) {
	sem := s.semFor(k)
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) semFor(k *store.Key) chan struct{} {
	n := k.Profile.Concurrency
	if n <= 0 {
		n = 1
	}
	s.semMu.Lock()
	defer s.semMu.Unlock()
	if cur, ok := s.sems[k.ID]; ok && cur.n == n {
		return cur.ch
	}
	sem := &semaphore{ch: make(chan struct{}, n), n: n}
	s.sems[k.ID] = sem
	return sem.ch
}

// ---------- routes ----------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.opts.Version,
	})
}

func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	s.log.Printf("[%d]     unhandled %s %s body=%s", reqID(r.Context()), r.Method, r.URL.Path, truncate(string(body), 300))
	writeError(w, http.StatusNotFound, "unknown endpoint "+r.URL.Path, typeInvalidRequest, "")
}

type modelOut struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	k, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	ids := k.Profile.Models
	if len(ids) == 0 {
		// An empty allow-list means "anything the backend can run", so ask
		// the backend.
		ad, err := adapter.Get(k.Profile.Adapter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), typeServerError, "")
			return
		}
		opts, err := s.opts.ResolveOptions(k)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot read adapter options: "+err.Error(), typeServerError, "")
			return
		}
		ms, err := s.adapterModels(r.Context(), ad, opts)
		if err != nil {
			s.log.Printf("[%d]     %s models failed: %v", reqID(r.Context()), ad.ID(), err)
		}
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 && k.Profile.DefaultModel != "" {
		ids = []string{k.Profile.DefaultModel}
	}

	now := time.Now().Unix()
	data := make([]modelOut, 0, len(ids))
	for _, id := range ids {
		data = append(data, modelOut{ID: id, Object: "model", Created: now, OwnedBy: k.Profile.Adapter})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// adapterModels caches per adapter id, since enumeration shells out.
func (s *server) adapterModels(ctx context.Context, ad adapter.Adapter, opts map[string]string) ([]adapter.Model, error) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	if e, ok := s.modelsCache[ad.ID()]; ok && time.Since(e.at) < modelsTTL {
		return e.models, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ms, err := ad.Models(ctx, opts)
	if err != nil {
		return nil, err
	}
	s.modelsCache[ad.ID()] = modelsEntry{models: ms, at: time.Now()}
	return ms, nil
}

// ---------- helpers ----------

func (s *server) newID() string {
	return fmt.Sprintf("chatcmpl-%d%d", time.Now().UnixNano(), s.reqCounter.Load())
}

// approxTokens is a rough 4-chars-per-token estimate, used only when the
// backend does not report real counts.
func approxTokens(s string) int {
	n := len(s) / 4
	if n == 0 && len(s) > 0 {
		return 1
	}
	return n
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, typ, code string) {
	var e apiError
	e.Error.Message = msg
	e.Error.Type = typ
	e.Error.Code = code
	writeJSON(w, status, e)
}
