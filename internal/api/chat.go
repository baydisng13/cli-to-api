package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cli-to-api/internal/adapter"
	"cli-to-api/internal/store"
)

// ---------- OpenAI wire types ----------
//
// These are the only place the OpenAI shapes are spelled out; handlers and the
// SSE writer both go through them.

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string, or array of content parts
}

// text flattens both the plain-string and the multipart content shapes.
func (m message) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(m.Content)
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
	// ConversationID asks the backend to continue a prior conversation
	// instead of starting a fresh one. Not part of the OpenAI schema, so the
	// X-Conversation-Id header is accepted too, for clients that will not
	// send unknown body fields.
	ConversationID string `json:"conversation_id"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      *chatMsgOut     `json:"message,omitempty"`
	Delta        *chatMsgOut     `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

type chatMsgOut struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	// ConversationID lets a client continue this exchange later. Empty for
	// backends that have no such concept.
	ConversationID string `json:"conversation_id,omitempty"`
	Usage          *usage `json:"usage,omitempty"`
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// OpenAI error vocabulary, kept in one place so the strings cannot drift.
const (
	typeInvalidRequest = "invalid_request_error"
	typeServerError    = "server_error"
	typeBackendError   = "backend_error"

	codeInvalidKey      = "invalid_api_key"
	codeRevokedKey      = "api_key_revoked"
	codeModelNotAllowed = "model_not_allowed"
	codeNoConversation  = "conversation_not_found"
	codeNoSessions      = "sessions_unsupported"
)

// ---------- handler ----------

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only", typeInvalidRequest, "")
		return
	}
	k, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body: "+err.Error(), typeInvalidRequest, "")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), typeInvalidRequest, "")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty", typeInvalidRequest, "")
		return
	}

	model, ok := s.resolveModel(w, k, req.Model)
	if !ok {
		return
	}
	ad, err := adapter.Get(k.Profile.Adapter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), typeServerError, "")
		return
	}

	convID := req.ConversationID
	if convID == "" {
		convID = r.Header.Get("X-Conversation-Id")
	}
	msgs := req.Messages
	if convID != "" {
		if !s.resumeAllowed(w, ad, k, convID) {
			return
		}
		// The backend already holds the history. Replaying the client's whole
		// messages array would make the model read the conversation twice, so
		// only the newest turn is sent.
		msgs = latestTurn(msgs)
		if len(msgs) == 0 {
			writeError(w, http.StatusBadRequest,
				"resuming a conversation requires a new user message", typeInvalidRequest, "")
			return
		}
		// Known up front on the resume path, so clients can correlate before
		// a single byte of the answer arrives.
		w.Header().Set("X-Conversation-Id", convID)
	}
	opts, err := s.opts.ResolveOptions(k)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read adapter options: "+err.Error(), typeServerError, "")
		return
	}

	areq := adapter.Request{
		Model:          model,
		Messages:       toMessages(msgs),
		Options:        opts,
		ConversationID: convID,
	}
	prompt := areq.Prompt()
	// The key id is public; the token itself never reaches the log.
	s.log.Printf("[%d]     key=%s adapter=%s model=%q stream=%t msgs=%d prompt=%q",
		reqID(r.Context()), k.ID, ad.ID(), model, req.Stream, len(msgs), truncate(prompt, 200))
	if convID != "" {
		s.log.Printf("[%d]     resuming conversation %s", reqID(r.Context()), convID)
	}

	// Queue for a slot before spending a subprocess, and keep the client's
	// cancellation authoritative while queued.
	release, err := s.acquire(r.Context(), k)
	if err != nil {
		return // client gave up waiting; nothing useful left to say
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), s.timeoutFor(k))
	defer cancel()

	chunks, done := generate(ctx, ad, areq, func(res adapter.Result) {
		s.rememberConversation(k, ad.ID(), model, res.ConversationID)
	})
	if req.Stream {
		s.streamChunks(ctx, w, model, chunks, done)
		return
	}
	s.bufferChunks(w, model, prompt, chunks, done)
}

func (s *server) timeoutFor(k *store.Key) time.Duration {
	if k.Profile.TimeoutSec > 0 {
		return time.Duration(k.Profile.TimeoutSec) * time.Second
	}
	return s.opts.Timeout
}

func toMessages(in []message) []adapter.Message {
	out := make([]adapter.Message, 0, len(in))
	for _, m := range in {
		t := m.text()
		if t == "" {
			continue
		}
		out = append(out, adapter.Message{Role: m.Role, Text: t})
	}
	return out
}

// genDone reports how a generation ended. It is readable only after the chunk
// channel has closed.
type genDone struct {
	res adapter.Result
	err error
}

// generate runs the adapter on its own goroutine and hands back its text as a
// channel, so the caller can choose between streaming and buffering without
// the adapter knowing which. Emitting blocks on the reader, which is what
// gives backpressure and makes a client disconnect stop the subprocess.
func generate(ctx context.Context, ad adapter.Adapter, req adapter.Request, onResult func(adapter.Result)) (<-chan string, <-chan genDone) {
	out := make(chan string)
	done := make(chan genDone, 1)
	go func() {
		res, err := ad.Generate(ctx, req, func(chunk string) error {
			if chunk == "" {
				return nil
			}
			select {
			case out <- chunk:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(out)
		if err == nil && onResult != nil {
			onResult(res)
		}
		done <- genDone{res: res, err: err}
	}()
	return out, done
}

// bufferChunks collects the whole answer and returns one chat.completion.
func (s *server) bufferChunks(w http.ResponseWriter, model, prompt string, chunks <-chan string, done <-chan genDone) {
	var b strings.Builder
	for c := range chunks {
		b.WriteString(c)
	}
	d := <-done
	if d.err != nil {
		s.log.Printf("    backend error: %v", d.err)
		writeError(w, http.StatusBadGateway, d.err.Error(), typeBackendError, "")
		return
	}
	content := strings.TrimRight(b.String(), "\n")
	if d.res.Model != "" {
		model = d.res.Model
	}
	finish := "stop"
	writeJSON(w, http.StatusOK, chatResponse{
		ID:      s.newID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      &chatMsgOut{Role: "assistant", Content: content},
			FinishReason: &finish,
		}},
		Usage:          usageFor(d.res.Usage, prompt, content),
		ConversationID: d.res.ConversationID,
	})
}

// usageFor prefers the backend's own accounting and falls back to a rough
// 4-chars-per-token estimate when it reported nothing.
func usageFor(u adapter.Usage, prompt, content string) *usage {
	if u.PromptTokens != 0 || u.CompletionTokens != 0 || u.TotalTokens != 0 {
		total := u.TotalTokens
		if total == 0 {
			total = u.PromptTokens + u.CompletionTokens
		}
		return &usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: total}
	}
	p, c := approxTokens(prompt), approxTokens(content)
	return &usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c}
}

// resumeAllowed answers whether this request may continue the named
// conversation, writing the refusal itself when it may not.
func (s *server) resumeAllowed(w http.ResponseWriter, ad adapter.Adapter, k *store.Key, convID string) bool {
	if !adapter.CanResume(ad) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("the %s backend cannot resume conversations; omit conversation_id", ad.ID()),
			typeInvalidRequest, codeNoSessions)
		return false
	}
	if s.opts.Conversations == nil {
		writeError(w, http.StatusBadRequest,
			"conversation continuity is not enabled on this server", typeInvalidRequest, codeNoSessions)
		return false
	}
	ok, err := s.opts.Conversations.Resume(convID, k.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read conversation state", typeServerError, "")
		return false
	}
	if !ok {
		// Deliberately the same answer whether the conversation never existed
		// or belongs to someone else: a guessed id must not reveal that it is
		// a real one.
		writeError(w, http.StatusNotFound, "no such conversation for this API key",
			typeInvalidRequest, codeNoConversation)
		return false
	}
	return true
}

// rememberConversation binds a newly created conversation to its key. A
// failure here costs continuity, not correctness, so it is logged and
// swallowed rather than failing a request whose answer already succeeded.
func (s *server) rememberConversation(k *store.Key, adapterID, model, convID string) {
	if convID == "" || s.opts.Conversations == nil {
		return
	}
	if err := s.opts.Conversations.Remember(convID, k.ID, adapterID, model); err != nil {
		s.log.Printf("    recording conversation %s: %v", convID, err)
	}
}

// latestTurn keeps only the trailing user message (plus any system message,
// which backends treat as standing instructions rather than history).
func latestTurn(msgs []message) []message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i : i+1]
		}
	}
	return nil
}
