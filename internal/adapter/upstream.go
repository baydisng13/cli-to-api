package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func init() { Register(upstream{}) }

// upstream forwards to another OpenAI-compatible API (OpenRouter, a local
// llama.cpp server, ...). The reference proxy piped raw bytes through; here we
// parse the SSE instead, because the Adapter contract is text chunks and
// usage, not an opaque response body.
type upstream struct{}

func (upstream) ID() string          { return "upstream" }
func (upstream) DisplayName() string { return "Upstream OpenAI-compatible API" }

func (upstream) OptionSpec() []Option {
	return []Option{
		{Key: "url", Label: "Base URL (e.g. https://openrouter.ai/api/v1)", Required: true},
		{Key: "api_key", Label: "API key", Secret: true, Required: true},
		{Key: "referer", Label: "HTTP-Referer sent upstream (OpenRouter attribution)", Default: "http://localhost/cli-to-api"},
		{Key: "title", Label: "X-Title sent upstream (OpenRouter attribution)", Default: "cli-to-api"},
	}
}

// upstreamURL normalizes the base URL, tolerating a nil option map and a
// trailing slash.
func upstreamURL(opts map[string]string) string {
	return strings.TrimSuffix(strings.TrimSpace(opts["url"]), "/")
}

func (upstream) Detect(opts map[string]string) (string, bool) {
	u := upstreamURL(opts)
	return u, u != ""
}

func (upstream) Models(ctx context.Context, opts map[string]string) ([]Model, error) {
	base := upstreamURL(opts)
	if base == "" {
		return nil, fmt.Errorf("upstream: option %q is required", "url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req, opts)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream models request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upstream models returned %s: %s", resp.Status, truncate(strings.TrimSpace(string(body)), 300))
	}

	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("upstream models returned unreadable JSON: %w", err)
	}
	models := make([]Model, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		models = append(models, Model{ID: m.ID, DisplayName: name})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("upstream models returned no models")
	}
	return models, nil
}

func setUpstreamHeaders(req *http.Request, opts map[string]string) {
	if key := strings.TrimSpace(opts["api_key"]); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// OpenRouter attribution headers; harmless elsewhere.
	if v := strings.TrimSpace(opts["referer"]); v != "" {
		req.Header.Set("HTTP-Referer", v)
	}
	if v := strings.TrimSpace(opts["title"]); v != "" {
		req.Header.Set("X-Title", v)
	}
}

type upstreamChunk struct {
	Model   string `json:"model"`
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (upstream) Generate(ctx context.Context, req Request, emit Emit) (Result, error) {
	base := upstreamURL(req.Options)
	if base == "" {
		return Result{}, fmt.Errorf("upstream: option %q is required", "url")
	}

	msgs := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": m.Text})
	}
	body, err := json.Marshal(map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	})
	if err != nil {
		return Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	setUpstreamHeaders(httpReq, req.Options)

	// No client timeout: a streamed completion is long-lived, and ctx already
	// carries the caller's deadline and cancellation.
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("upstream returned %s: %s", resp.Status, truncate(strings.TrimSpace(string(raw)), 300))
	}

	return parseUpstreamSSE(resp.Body, req.Model, emit)
}

// parseUpstreamSSE reads an OpenAI-style SSE stream, emitting each content
// delta. Split out from Generate so it can be exercised against a fixture.
func parseUpstreamSSE(r io.Reader, fallbackModel string, emit Emit) (Result, error) {
	res := Result{Model: fallbackModel}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // a single delta can be large
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // comments, event: lines, blank separators
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch upstreamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // keep-alive padding and other non-JSON frames
		}
		if ch.Error != nil && ch.Error.Message != "" {
			return res, fmt.Errorf("upstream error: %s", truncate(ch.Error.Message, 300))
		}
		if ch.Model != "" {
			res.Model = ch.Model
		}
		if ch.ID != "" {
			res.ConversationID = ch.ID
		}
		if ch.Usage != nil {
			res.Usage = Usage{
				PromptTokens:     ch.Usage.PromptTokens,
				CompletionTokens: ch.Usage.CompletionTokens,
				TotalTokens:      ch.Usage.TotalTokens,
			}
		}
		for _, c := range ch.Choices {
			if c.Delta.Content == "" {
				continue
			}
			if err := emit(c.Delta.Content); err != nil {
				return res, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("reading upstream stream: %w", err)
	}
	return res, nil
}
