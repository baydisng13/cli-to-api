package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cli-to-api/internal/store"
)

// sseEvents splits an SSE body into its "data:" payloads.
func sseEvents(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("frame does not start with %q: %q", "data: ", frame)
		}
		out = append(out, strings.TrimPrefix(frame, "data: "))
	}
	return out
}

func TestStreamingFraming(t *testing.T) {
	h := newHarness(t, func(k *store.Key) { k.Profile.Options["chunks"] = "Al|pha| beta" })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-2", true, "hi")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if ab := rec.Header().Get("X-Accel-Buffering"); ab != "no" {
		t.Errorf("X-Accel-Buffering = %q", ab)
	}

	ev := sseEvents(t, rec.Body.String())
	if len(ev) != 6 {
		t.Fatalf("got %d events, want 6: %q", len(ev), ev)
	}
	if ev[len(ev)-1] != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", ev[len(ev)-1])
	}

	var first chatResponse
	if err := json.Unmarshal([]byte(ev[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Object != "chat.completion.chunk" {
		t.Errorf("object = %q", first.Object)
	}
	if first.Model != "fake-2" {
		t.Errorf("model = %q, want fake-2", first.Model)
	}
	if first.Choices[0].Delta == nil || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first delta = %+v, want role assistant", first.Choices[0].Delta)
	}
	if first.Choices[0].Delta.Content != "" {
		t.Errorf("first chunk carried content %q", first.Choices[0].Delta.Content)
	}

	var got []string
	for _, raw := range ev[1:4] {
		var c chatResponse
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c.Choices[0].Delta.Content)
	}
	if strings.Join(got, "|") != "Al|pha| beta" {
		t.Errorf("content deltas = %q, want Al|pha| beta in order", got)
	}

	var last chatResponse
	if err := json.Unmarshal([]byte(ev[4]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", last.Choices[0].FinishReason)
	}
	if first.ID == "" || last.ID != first.ID {
		t.Errorf("chunk ids differ: %q vs %q", first.ID, last.ID)
	}
}

func TestStreamingMidStreamErrorRidesAlong(t *testing.T) {
	h := newHarness(t, func(k *store.Key) {
		k.Profile.Options["chunks"] = "partial"
		k.Profile.Options["fail"] = "backend exploded"
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", true, "hi")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)

	// Headers were already sent, so the status must still be 200.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	ev := sseEvents(t, rec.Body.String())
	last := ev[len(ev)-1]
	if last == "[DONE]" {
		t.Fatalf("stream ended with [DONE] despite the backend failing: %q", ev)
	}
	var e apiError
	if err := json.Unmarshal([]byte(last), &e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Error.Message, "backend exploded") {
		t.Errorf("error frame = %+v", e.Error)
	}
	if e.Error.Type != typeBackendError {
		t.Errorf("error type = %q", e.Error.Type)
	}
}

func TestNonStreamingShapeAndUsageEstimate(t *testing.T) {
	h := newHarness(t, func(k *store.Key) { k.Profile.Options["chunks"] = "Hello| world" })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", false, "a prompt of some length")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message == nil {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	if got := resp.Choices[0].Message.Content; got != "Hello world" {
		t.Errorf("content = %q", got)
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v", resp.Choices[0].FinishReason)
	}
	// The fake reports no usage, so the 4-chars-per-token estimate applies.
	wantPrompt := approxTokens("a prompt of some length")
	wantCompletion := approxTokens("Hello world")
	if resp.Usage == nil || resp.Usage.PromptTokens != wantPrompt || resp.Usage.CompletionTokens != wantCompletion {
		t.Fatalf("usage = %+v, want estimate %d/%d", resp.Usage, wantPrompt, wantCompletion)
	}
	if resp.Usage.TotalTokens != wantPrompt+wantCompletion {
		t.Errorf("total = %d", resp.Usage.TotalTokens)
	}
	if n := h.usage.count("abc12345"); n != 1 {
		t.Errorf("TouchUsage called %d times, want 1", n)
	}
}

func TestNonStreamingUsesReportedUsage(t *testing.T) {
	h := newHarness(t, func(k *store.Key) { k.Profile.Options["usage"] = "1" })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", false, "hi")))
	req.Header.Set("X-Api-Key", testToken)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 22 || resp.Usage.TotalTokens != 33 {
		t.Fatalf("usage = %+v, want the adapter's own 11/22/33", resp.Usage)
	}
}

func TestAuthFailures(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*store.Key)
		auth    func(*http.Request)
		model   string
		status  int
		code    string
		msgHas  string
		msgNone string
	}{
		{
			name:   "missing token",
			auth:   func(*http.Request) {},
			model:  "fake-1",
			status: http.StatusUnauthorized,
			code:   codeInvalidKey,
			msgHas: "missing API key",
		},
		{
			name:   "bad token",
			auth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
			model:  "fake-1",
			status: http.StatusUnauthorized,
			code:   codeInvalidKey,
			msgHas: "invalid API key",
		},
		{
			name:   "revoked key",
			setup:  func(k *store.Key) { k.Revoked = true },
			auth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) },
			model:  "fake-1",
			status: http.StatusUnauthorized,
			code:   codeRevokedKey,
			msgHas: "revoked",
		},
		{
			name:   "model not allowed",
			auth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) },
			model:  "gpt-4o",
			status: http.StatusForbidden,
			code:   codeModelNotAllowed,
			msgHas: "fake-1, fake-2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.setup)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(tc.model, false, "hi")))
			tc.auth(req)
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			var e apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			if e.Error.Type != typeInvalidRequest {
				t.Errorf("type = %q, want %q", e.Error.Type, typeInvalidRequest)
			}
			if e.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Error.Code, tc.code)
			}
			if !strings.Contains(e.Error.Message, tc.msgHas) {
				t.Errorf("message = %q, want it to contain %q", e.Error.Message, tc.msgHas)
			}
			if strings.Contains(e.Error.Message, testToken) {
				t.Errorf("message leaked the token: %q", e.Error.Message)
			}
		})
	}
}

func TestRevokedDistinctFromInvalid(t *testing.T) {
	bad := newHarness(t, nil)
	rev := newHarness(t, func(k *store.Key) { k.Revoked = true })

	msg := func(h *harness, tok string) string {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", false, "hi")))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		var e apiError
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e.Error.Message
	}
	if a, b := msg(bad, "nope"), msg(rev, testToken); a == b {
		t.Fatalf("revoked and invalid share the message %q", a)
	}
}

func TestCredentialLocations(t *testing.T) {
	cases := []struct {
		name string
		path string
		fn   func(*http.Request)
	}{
		{"bearer", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) }},
		{"bearer lowercase", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+testToken) }},
		{"x-api-key", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("X-Api-Key", testToken) }},
		{"query param", "/v1/chat/completions?api_key=" + testToken, func(*http.Request) {}},
		{"no v1 prefix", "/chat/completions", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(chatBody("fake-1", false, "hi")))
			tc.fn(req)
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMultipartContentFlattening(t *testing.T) {
	h := newHarness(t, func(k *store.Key) { k.Profile.Options["chunks"] = "ok" })
	body := `{"model":"fake-1","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"first part "},` +
		`{"type":"image_url","image_url":{"url":"http://x/y.png"}},` +
		`{"type":"text","text":"second part"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	// The flattened prompt drives the usage estimate, which is the only place
	// the handler's view of the prompt is observable from outside.
	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if want := approxTokens("first part second part"); resp.Usage.PromptTokens != want {
		t.Errorf("prompt tokens = %d, want %d (parts should concatenate)", resp.Usage.PromptTokens, want)
	}

	// And the flattening itself, at the unit level.
	var m message
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.text(); got != "ab" {
		t.Errorf("message.text() = %q, want ab", got)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":"plain"}`), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.text(); got != "plain" {
		t.Errorf("message.text() = %q, want plain", got)
	}
}

func TestPerKeyConcurrencyLimit(t *testing.T) {
	p, name := newProbe(t, true)
	h := newHarness(t, func(k *store.Key) {
		k.Profile.Concurrency = 1
		k.Profile.Options["probe"] = name
		k.Profile.Options["chunks"] = "x"
	})

	fire := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", false, "hi")))
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		return rec
	}

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	for i := range recs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); recs[i] = fire() }(i)
	}

	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("no request reached the adapter")
	}
	// Give the second request every chance to slip through the semaphore.
	time.Sleep(100 * time.Millisecond)
	if active, _, starts := p.snapshot(); active != 1 || starts != 1 {
		t.Fatalf("active=%d starts=%d, want 1 and 1: the semaphore let a second request in", active, starts)
	}

	close(p.hold)
	wg.Wait()

	if _, max, starts := p.snapshot(); max != 1 || starts != 2 {
		t.Fatalf("maxActive=%d starts=%d, want 1 and 2", max, starts)
	}
	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status %d, body %s", i, rec.Code, rec.Body.String())
		}
	}
}

func TestClientDisconnectCancelsAdapter(t *testing.T) {
	p, name := newProbe(t, true) // holds forever: only cancellation ends it
	h := newHarness(t, func(k *store.Key) {
		k.Profile.Options["probe"] = name
		k.Profile.Options["chunks"] = "first"
	})
	srv := httptest.NewServer(h.h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(chatBody("fake-1", true, "hi")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read far enough that the stream is genuinely live before hanging up.
	br := bufio.NewReader(resp.Body)
	for seen := 0; seen < 2; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			seen++
		}
	}
	cancel()

	select {
	case err := <-p.ctxErr:
		if err == nil {
			t.Fatal("adapter saw a nil context error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter context was never cancelled after the client hung up")
	}
	close(p.hold)
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/healthz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "ok" {
			t.Errorf("status field = %v", body["status"])
		}
		if body["version"] != "test-version" {
			t.Errorf("version field = %v", body["version"])
		}
	}
	if n := h.usage.count("abc12345"); n != 0 {
		t.Errorf("healthz touched usage %d times", n)
	}
}

func TestUnknownPathReturnsOpenAI404(t *testing.T) {
	h := newHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"prompt":"probe"}`))
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Type != typeInvalidRequest || !strings.Contains(e.Error.Message, "/v1/completions") {
		t.Errorf("error = %+v", e.Error)
	}
}

func TestModelsEndpoint(t *testing.T) {
	t.Run("from profile", func(t *testing.T) {
		h := newHarness(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var body struct {
			Object string     `json:"object"`
			Data   []modelOut `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Object != "list" || len(body.Data) != 2 || body.Data[0].ID != "fake-1" {
			t.Fatalf("models = %+v", body)
		}
	})

	t.Run("from adapter when profile list is empty", func(t *testing.T) {
		h := newHarness(t, func(k *store.Key) { k.Profile.Models = nil })
		req := httptest.NewRequest(http.MethodGet, "/models", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var body struct {
			Data []modelOut `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Data) != 2 || body.Data[1].ID != "fake-2" {
			t.Fatalf("models = %+v", body.Data)
		}
	})

	t.Run("needs auth", func(t *testing.T) {
		h := newHarness(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestResolveOptionsHookAndDefault(t *testing.T) {
	k := testKey(func(k *store.Key) { k.Profile.Options["chunks"] = "plain" })
	var seen *store.Key
	h := New(Options{
		Auth:   stubAuth{keys: map[string]*store.Key{testToken: k}},
		Logger: discardLogger(),
		ResolveOptions: func(key *store.Key) (map[string]string, error) {
			seen = key
			return map[string]string{"chunks": "decrypted"}, nil
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", false, "hi")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "decrypted" {
		t.Errorf("content = %q, want the hook's options to win", resp.Choices[0].Message.Content)
	}
	if seen != k {
		t.Error("hook was not called with the authenticated key")
	}
}

func TestChatRejectsNonPost(t *testing.T) {
	h := newHarness(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestUsageRecorderFailureIsNotFatal(t *testing.T) {
	k := testKey(nil)
	u := newStubUsage()
	u.err = io.ErrUnexpectedEOF
	h := New(Options{
		Auth:   stubAuth{keys: map[string]*store.Key{testToken: k}},
		Usage:  u,
		Logger: discardLogger(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("fake-1", false, "hi")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the request served anyway", rec.Code)
	}
}
