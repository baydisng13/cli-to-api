// Package adapter defines the seam between the OpenAI-shaped HTTP surface and
// whatever actually produces text: a local agentic CLI, a shell command, or an
// upstream OpenAI-compatible API.
//
// Adding support for a new CLI means adding one file in this package and
// calling Register from its init.
package adapter

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Model is one model an adapter can run.
type Model struct {
	ID          string // wire id, e.g. "gemini-3.8-flash-low"
	DisplayName string // human label, e.g. "Gemini 3.8 Flash (Low)"
}

// Message is one turn of the conversation, already flattened to text.
type Message struct {
	Role string // "system" | "user" | "assistant"
	Text string
}

// Request is a generation request, normalized away from the OpenAI wire types.
type Request struct {
	Model    string // may be empty: the adapter picks its own default
	Messages []Message
	// Options carries adapter-specific knobs from the key's profile
	// (flags, binary path, upstream URL...). Keys are adapter-defined.
	Options map[string]string
	// ConversationID resumes a prior conversation on backends that support it.
	// When set, Messages carries only the new turn: the backend already has the
	// history, and replaying it would have the model read everything twice.
	//
	// The contract lives in the HTTP layer: it owns the mapping from client
	// state to a backend conversation id, and it is what trims Messages down to
	// the new turn before calling Generate. An adapter does not reconcile the
	// two — it passes the id through to its backend and prompts with whatever
	// Messages it was handed. Resuming is a context-continuity feature, not a
	// token saving: the backend still re-reads the history on its side.
	//
	// Only adapters that satisfy SessionCapable will ever see this set; for
	// every other adapter it is empty and ignorable.
	ConversationID string
}

// Prompt flattens the conversation to a single string, which is all a
// one-shot CLI invocation can accept. A lone message is passed through as-is;
// multiple messages become "ROLE: text" blocks.
func (r Request) Prompt() string {
	if len(r.Messages) == 1 {
		return r.Messages[0].Text
	}
	var b []byte
	for _, m := range r.Messages {
		if m.Text == "" {
			continue
		}
		b = append(b, upper(m.Role)...)
		b = append(b, ": "...)
		b = append(b, m.Text...)
		b = append(b, "\n\n"...)
	}
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return string(b)
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 32
		}
	}
	return string(out)
}

// Usage is the token accounting an adapter reports. Zero values mean "the
// backend did not say", and the caller estimates instead.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Result is what a completed generation reports back.
type Result struct {
	Usage Usage
	// ConversationID, when the backend has such a concept, for log correlation.
	ConversationID string
	// Model actually used, when it differs from Request.Model.
	Model string
}

// Emit is called with each text chunk as it is produced. It returns an error
// when the client has gone away; an adapter must stop and return that error.
type Emit func(chunk string) error

// Adapter drives one backend.
type Adapter interface {
	// ID is the stable wire name used in config and on the CLI, e.g. "agy".
	ID() string

	// DisplayName is what the wizard shows, e.g. "Antigravity CLI".
	DisplayName() string

	// Detect reports where the backend lives and whether it is usable here.
	// opts carries the same adapter-specific options as Request.Options and
	// may be nil during discovery.
	Detect(opts map[string]string) (path string, ok bool)

	// Models lists what this backend can run. It may shell out; callers pass
	// a context with a timeout. Adapters that cannot enumerate (a raw shell
	// command, say) return a single synthetic model.
	Models(ctx context.Context, opts map[string]string) ([]Model, error)

	// Generate runs one request, calling emit for each chunk of assistant
	// text. It must not emit tool-call or reasoning noise. Returning an error
	// after emitting is allowed: the caller handles mid-stream failure.
	Generate(ctx context.Context, req Request, emit Emit) (Result, error)

	// OptionSpec describes the adapter-specific options the wizard should
	// ask about, in display order. May be empty.
	OptionSpec() []Option
}

// SessionCapable is implemented by adapters that can resume a prior
// conversation by id. Callers type-assert for it; an adapter that does not
// implement it is treated as stateless.
//
// It is deliberately NOT part of the Adapter interface: every existing adapter
// stays valid without edits, and a new one opts in by adding one method.
type SessionCapable interface {
	SupportsSessions() bool
}

// CanResume reports whether a is able to resume conversations.
func CanResume(a Adapter) bool {
	sc, ok := a.(SessionCapable)
	return ok && sc.SupportsSessions()
}

// Option is one adapter-specific setting the wizard can collect.
type Option struct {
	Key      string // key in Request.Options
	Label    string // question shown in the wizard
	Default  string
	Secret   bool // store encrypted, never echo (e.g. an upstream API key)
	Required bool
}

var (
	mu       sync.RWMutex
	registry = map[string]Adapter{}
)

// Register adds an adapter. Call it from an init function. Panics on a
// duplicate id, which is always a programming error.
func Register(a Adapter) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[a.ID()]; dup {
		panic("adapter: duplicate id " + a.ID())
	}
	registry[a.ID()] = a
}

// Get returns the adapter with this id.
func Get(id string) (Adapter, error) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[id]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", id)
	}
	return a, nil
}

// All returns every registered adapter, ordered by id.
func All() []Adapter {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}
