package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cli-to-api/internal/adapter"
	"cli-to-api/internal/store"
)

const sessionAdapterID = "apitest-session"

func init() { adapter.Register(&sessionFake{}) }

// sessionFake is a backend that can resume. It records what the HTTP layer
// handed it, which is how these tests check message trimming and id passing.
type sessionFake struct {
	mu       sync.Mutex
	lastReq  adapter.Request
	newConvI int
}

var sessFake = &sessionFake{}

func (*sessionFake) ID() string                              { return sessionAdapterID }
func (*sessionFake) DisplayName() string                     { return "Fake Session Adapter" }
func (*sessionFake) Detect(map[string]string) (string, bool) { return "/dev/null", true }
func (*sessionFake) OptionSpec() []adapter.Option            { return nil }
func (*sessionFake) SupportsSessions() bool                  { return true }
func (*sessionFake) Models(_ context.Context, _ map[string]string) ([]adapter.Model, error) {
	return []adapter.Model{{ID: "fake-1"}}, nil
}

func (*sessionFake) Generate(_ context.Context, req adapter.Request, emit adapter.Emit) (adapter.Result, error) {
	sessFake.mu.Lock()
	sessFake.lastReq = req
	sessFake.mu.Unlock()

	if err := emit("ok"); err != nil {
		return adapter.Result{}, err
	}
	conv := req.ConversationID
	if conv == "" {
		conv = "conv-brand-new"
	}
	return adapter.Result{ConversationID: conv}, nil
}

func lastSessionRequest() adapter.Request {
	sessFake.mu.Lock()
	defer sessFake.mu.Unlock()
	return sessFake.lastReq
}

// stubConversations is an in-memory ownership table with the same
// indistinguishability rule the real store must honour.
type stubConversations struct {
	mu    sync.Mutex
	owner map[string]string // convID -> keyID
	err   error
	calls int
}

func newStubConversations() *stubConversations {
	return &stubConversations{owner: map[string]string{}}
}

func (c *stubConversations) Resume(convID, keyID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	return c.owner[convID] == keyID && keyID != "", nil
}

func (c *stubConversations) Remember(convID, keyID, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.owner[convID] = keyID
	return nil
}

func sessionHarness(t *testing.T, conv Conversations) (http.Handler, *store.Key) {
	t.Helper()
	k := testKey(func(k *store.Key) {
		k.Profile.Adapter = sessionAdapterID
		k.Profile.Models = []string{"fake-1"}
		k.Profile.DefaultModel = "fake-1"
	})
	h := New(Options{
		Auth:          stubAuth{keys: map[string]*store.Key{testToken: k}},
		Usage:         newStubUsage(),
		Logger:        discardLogger(),
		Conversations: conv,
		Timeout:       5 * time.Second,
	})
	return h, k
}

func postChat(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNewConversationIsReturnedAndRemembered(t *testing.T) {
	conv := newStubConversations()
	h, k := sessionHarness(t, conv)

	w := postChat(t, h, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ConversationID != "conv-brand-new" {
		t.Errorf("conversation_id = %q, want conv-brand-new", resp.ConversationID)
	}
	conv.mu.Lock()
	owner := conv.owner["conv-brand-new"]
	conv.mu.Unlock()
	if owner != k.ID {
		t.Errorf("conversation bound to %q, want key %q", owner, k.ID)
	}
}

func TestResumeTrimsHistoryToLatestTurn(t *testing.T) {
	conv := newStubConversations()
	h, k := sessionHarness(t, conv)
	_ = conv.Remember("conv-1", k.ID, sessionAdapterID, "fake-1")

	body := `{"conversation_id":"conv-1","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":"second"}]}`
	w := postChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	got := lastSessionRequest()
	if got.ConversationID != "conv-1" {
		t.Errorf("adapter got ConversationID %q, want conv-1", got.ConversationID)
	}
	// The backend already holds the history; replaying it would double-count.
	if len(got.Messages) != 1 || got.Messages[0].Text != "second" {
		t.Errorf("adapter got messages %+v, want only the latest user turn", got.Messages)
	}
	if h := w.Header().Get("X-Conversation-Id"); h != "conv-1" {
		t.Errorf("X-Conversation-Id = %q, want conv-1", h)
	}
}

func TestResumeAcceptsHeaderForm(t *testing.T) {
	conv := newStubConversations()
	h, k := sessionHarness(t, conv)
	_ = conv.Remember("conv-hdr", k.ID, sessionAdapterID, "fake-1")

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-Conversation-Id", "conv-hdr")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := lastSessionRequest().ConversationID; got != "conv-hdr" {
		t.Errorf("adapter got ConversationID %q, want conv-hdr", got)
	}
}

// The security property: a conversation id is not a credential. Another key
// presenting a real id must get the same answer as for an invented one.
func TestResumeAcrossKeysIsIndistinguishableFromUnknown(t *testing.T) {
	conv := newStubConversations()
	h, _ := sessionHarness(t, conv)
	_ = conv.Remember("conv-someone-else", "a-different-key", sessionAdapterID, "fake-1")

	stolen := postChat(t, h, `{"conversation_id":"conv-someone-else","messages":[{"role":"user","content":"hi"}]}`)
	invented := postChat(t, h, `{"conversation_id":"conv-never-existed","messages":[{"role":"user","content":"hi"}]}`)

	if stolen.Code != http.StatusNotFound {
		t.Errorf("borrowed conversation status = %d, want 404", stolen.Code)
	}
	if invented.Code != stolen.Code {
		t.Errorf("status differs: borrowed %d vs invented %d", stolen.Code, invented.Code)
	}
	if stolen.Body.String() != invented.Body.String() {
		t.Errorf("response body reveals which id is real:\n borrowed: %s\n invented: %s",
			stolen.Body.String(), invented.Body.String())
	}
}

func TestResumeRejectedWhenAdapterCannotResume(t *testing.T) {
	conv := newStubConversations()
	k := testKey(nil) // the plain fake adapter, which has no session support
	h := New(Options{
		Auth:          stubAuth{keys: map[string]*store.Key{testToken: k}},
		Usage:         newStubUsage(),
		Logger:        discardLogger(),
		Conversations: conv,
	})
	w := postChat(t, h, `{"conversation_id":"whatever","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), codeNoSessions) {
		t.Errorf("body %s, want code %s", w.Body.String(), codeNoSessions)
	}
}

func TestResumeRejectedWhenServerHasNoConversationStore(t *testing.T) {
	h, _ := sessionHarness(t, nil)
	w := postChat(t, h, `{"conversation_id":"whatever","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

// A stateless request must behave exactly as before: full history replayed,
// nothing recorded.
func TestWithoutConversationIDNothingChanges(t *testing.T) {
	conv := newStubConversations()
	h, _ := sessionHarness(t, conv)

	body := `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":"second"}]}`
	if w := postChat(t, h, body); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := lastSessionRequest()
	if got.ConversationID != "" {
		t.Errorf("ConversationID = %q, want empty", got.ConversationID)
	}
	if len(got.Messages) != 3 {
		t.Errorf("adapter got %d messages, want the full history of 3", len(got.Messages))
	}
}

func TestResumeRequiresANewUserMessage(t *testing.T) {
	conv := newStubConversations()
	h, k := sessionHarness(t, conv)
	_ = conv.Remember("conv-2", k.ID, sessionAdapterID, "fake-1")

	w := postChat(t, h, `{"conversation_id":"conv-2","messages":[{"role":"assistant","content":"only an assistant turn"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestStreamingReportsConversationIDOnFinalChunk(t *testing.T) {
	conv := newStubConversations()
	h, _ := sessionHarness(t, conv)

	w := postChat(t, h, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("stream did not terminate with [DONE]")
	}
	var found string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "data: ")
		if line == "" || line == "[DONE]" {
			continue
		}
		var ev struct {
			ConversationID string `json:"conversation_id"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.ConversationID != "" {
			found = ev.ConversationID
		}
	}
	if found != "conv-brand-new" {
		t.Errorf("streamed conversation_id = %q, want conv-brand-new", found)
	}
}

// A failure to record ownership must not fail a request whose answer already
// succeeded; it costs continuity, not correctness.
func TestRememberFailureDoesNotFailTheRequest(t *testing.T) {
	conv := &failingConversations{}
	h, _ := sessionHarness(t, conv)
	w := postChat(t, h, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
}

type failingConversations struct{}

func (failingConversations) Resume(string, string) (bool, error) { return false, nil }
func (failingConversations) Remember(string, string, string, string) error {
	return fmt.Errorf("disk on fire")
}
