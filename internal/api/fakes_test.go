package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cli-to-api/internal/adapter"
	"cli-to-api/internal/store"
)

const fakeAdapterID = "apitest-fake"

func init() { adapter.Register(&fakeAdapter{}) }

// fakeAdapter is driven entirely by Request.Options so that tests stay
// independent of each other: no global state is mutated to change behaviour.
//
//	chunks: "|"-separated text chunks to emit (default "Hello| world")
//	fail:   non-empty -> return this error after emitting (mid-stream failure)
//	usage:  "1" -> report real token counts instead of leaving them zero
//	probe:  name of a probe registered with newProbe, for observing calls
type fakeAdapter struct{}

func (*fakeAdapter) ID() string          { return fakeAdapterID }
func (*fakeAdapter) DisplayName() string { return "Fake Test Adapter" }

func (*fakeAdapter) Detect(map[string]string) (string, bool) { return "/dev/null", true }

func (*fakeAdapter) OptionSpec() []adapter.Option { return nil }

func (*fakeAdapter) Models(ctx context.Context, opts map[string]string) ([]adapter.Model, error) {
	if opts["models_fail"] != "" {
		return nil, errors.New(opts["models_fail"])
	}
	return []adapter.Model{{ID: "fake-1", DisplayName: "Fake One"}, {ID: "fake-2", DisplayName: "Fake Two"}}, nil
}

func (*fakeAdapter) Generate(ctx context.Context, req adapter.Request, emit adapter.Emit) (adapter.Result, error) {
	p := probeFor(req.Options["probe"])
	if p != nil {
		p.enter()
		defer p.exit()
	}
	raw := req.Options["chunks"]
	if raw == "" {
		raw = "Hello| world"
	}
	for _, c := range strings.Split(raw, "|") {
		if err := emit(c); err != nil {
			return adapter.Result{}, err
		}
	}
	if p != nil && p.hold != nil {
		select {
		case <-p.hold:
		case <-ctx.Done():
			p.reportCtx(ctx.Err())
			return adapter.Result{}, ctx.Err()
		}
	}
	if msg := req.Options["fail"]; msg != "" {
		return adapter.Result{}, errors.New(msg)
	}
	res := adapter.Result{Model: req.Model}
	if req.Options["usage"] == "1" {
		res.Usage = adapter.Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33}
	}
	return res, nil
}

// ---------- probes ----------

type probe struct {
	mu        sync.Mutex
	active    int
	maxActive int
	starts    int

	started chan struct{} // one token per Generate entry
	hold    chan struct{} // Generate waits here before returning, if non-nil
	ctxErr  chan error    // receives ctx.Err() when cancelled while holding
}

var (
	probesMu sync.Mutex
	probes   = map[string]*probe{}
)

// newProbe registers a probe for the duration of the test. hold, when true,
// parks Generate after it has emitted so the test can observe overlap or
// cancellation.
func newProbe(t *testing.T, hold bool) (*probe, string) {
	t.Helper()
	name := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	p := &probe{started: make(chan struct{}, 64), ctxErr: make(chan error, 4)}
	if hold {
		p.hold = make(chan struct{})
	}
	probesMu.Lock()
	probes[name] = p
	probesMu.Unlock()
	t.Cleanup(func() {
		probesMu.Lock()
		delete(probes, name)
		probesMu.Unlock()
	})
	return p, name
}

func probeFor(name string) *probe {
	if name == "" {
		return nil
	}
	probesMu.Lock()
	defer probesMu.Unlock()
	return probes[name]
}

func (p *probe) enter() {
	p.mu.Lock()
	p.active++
	p.starts++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
}

func (p *probe) exit() {
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
}

func (p *probe) reportCtx(err error) {
	select {
	case p.ctxErr <- err:
	default:
	}
}

func (p *probe) snapshot() (active, maxActive, starts int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active, p.maxActive, p.starts
}

// ---------- stub auth ----------

type stubAuth struct {
	keys map[string]*store.Key // token -> key
}

func (a stubAuth) Authenticate(tok string) (*store.Key, error) {
	k, ok := a.keys[tok]
	if !ok {
		return nil, fmt.Errorf("stub: %w", ErrInvalidKey)
	}
	if k.Revoked {
		return nil, fmt.Errorf("stub: key %s: %w", k.ID, ErrRevokedKey)
	}
	return k, nil
}

func (a stubAuth) Allowed(k *store.Key, model string) (string, error) {
	if model == "" {
		if k.Profile.DefaultModel != "" {
			return k.Profile.DefaultModel, nil
		}
		return "fake-1", nil
	}
	if len(k.Profile.Models) == 0 {
		return model, nil
	}
	for _, m := range k.Profile.Models {
		if m == model {
			return model, nil
		}
	}
	return "", fmt.Errorf("stub: %q: %w", model, ErrModelNotAllowed)
}

type stubUsage struct {
	mu  sync.Mutex
	hit map[string]int
	err error
}

func newStubUsage() *stubUsage { return &stubUsage{hit: map[string]int{}} }

func (u *stubUsage) TouchUsage(id string) error {
	u.mu.Lock()
	u.hit[id]++
	u.mu.Unlock()
	return u.err
}

func (u *stubUsage) count(id string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hit[id]
}

// ---------- harness ----------

const testToken = "sk-test-token"

func testKey(mutate func(*store.Key)) *store.Key {
	k := &store.Key{
		ID:   "abc12345",
		Name: "test key",
		Profile: store.Profile{
			Adapter:      fakeAdapterID,
			Models:       []string{"fake-1", "fake-2"},
			DefaultModel: "fake-1",
			Options:      map[string]string{},
			Concurrency:  4,
		},
	}
	if mutate != nil {
		mutate(k)
	}
	return k
}

type harness struct {
	h     http.Handler
	key   *store.Key
	usage *stubUsage
}

func newHarness(t *testing.T, mutate func(*store.Key)) *harness {
	t.Helper()
	k := testKey(mutate)
	u := newStubUsage()
	h := New(Options{
		Auth:    stubAuth{keys: map[string]*store.Key{testToken: k}},
		Usage:   u,
		Logger:  log.New(io.Discard, "", 0),
		Version: "test-version",
		Timeout: 5 * time.Second,
	})
	return &harness{h: h, key: k, usage: u}
}

func chatBody(model string, stream bool, content string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":%q}]}`, model, stream, content)
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
