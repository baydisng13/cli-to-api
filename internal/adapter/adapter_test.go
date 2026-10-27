package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// collector returns an Emit that appends to a slice, plus the slice pointer.
func collector(chunks *[]string) Emit {
	return func(c string) error {
		*chunks = append(*chunks, c)
		return nil
	}
}

func joined(chunks []string) string { return strings.Join(chunks, "") }

// ---------- registry ----------

type fakeAdapter struct{ id string }

func (f fakeAdapter) ID() string                            { return f.id }
func (f fakeAdapter) DisplayName() string                   { return "Fake " + f.id }
func (fakeAdapter) Detect(map[string]string) (string, bool) { return "", false }
func (fakeAdapter) OptionSpec() []Option                    { return nil }
func (fakeAdapter) Models(context.Context, map[string]string) ([]Model, error) {
	return nil, nil
}
func (fakeAdapter) Generate(context.Context, Request, Emit) (Result, error) {
	return Result{}, nil
}

func TestRegistry(t *testing.T) {
	const id = "zz-test-fake"
	Register(fakeAdapter{id: id})

	got, err := Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if got.DisplayName() != "Fake "+id {
		t.Errorf("DisplayName = %q", got.DisplayName())
	}
	if _, err := Get("nope"); err == nil {
		t.Error("Get on unknown id: want error, got nil")
	}

	all := All()
	var ids []string
	for _, a := range all {
		ids = append(ids, a.ID())
	}
	for _, want := range []string{"agy", "echo", "shellcmd", "upstream", id} {
		if !contains(ids, want) {
			t.Errorf("All() missing %q (got %v)", want, ids)
		}
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID() > all[i].ID() {
			t.Fatalf("All() not sorted: %v", ids)
		}
	}

	defer func() {
		if recover() == nil {
			t.Error("duplicate Register: want panic")
		}
	}()
	Register(fakeAdapter{id: id})
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestRequestPrompt(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want string
	}{
		{"single passes through", []Message{{Role: "user", Text: "hi there"}}, "hi there"},
		{
			"multi becomes role blocks",
			[]Message{{Role: "system", Text: "be terse"}, {Role: "user", Text: "hi"}},
			"SYSTEM: be terse\n\nUSER: hi",
		},
		{
			"empty messages are skipped",
			[]Message{{Role: "system", Text: ""}, {Role: "user", Text: "hi"}, {Role: "assistant", Text: "yo"}},
			"USER: hi\n\nASSISTANT: yo",
		},
		{"none", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Request{Messages: tt.msgs}).Prompt(); got != tt.want {
				t.Errorf("Prompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------- agy ----------

func TestParseAgyStream(t *testing.T) {
	const okStream = `Fetching available models...
{"event":"init","conversation_id":"conv-1","init":{"conversation_id":"conv-1"}}
{"event":"step_update","step_update":{"step_type":"tool_call","text_delta":"ls -la"}}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"Hello"}}
{"event":"step_update","step_update":{"step_type":"thinking","text_delta":"hmm"}}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":" world"}}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":""}}
not json at all
{"event":"result","result":{"status":"SUCCESS","usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}}
`

	tests := []struct {
		name      string
		stream    string
		wantText  string
		wantUsage Usage
		wantConv  string
		wantErr   string
	}{
		{
			name:      "only agent_response deltas are emitted",
			stream:    okStream,
			wantText:  "Hello world",
			wantUsage: Usage{PromptTokens: 11, CompletionTokens: 5, TotalTokens: 16},
			wantConv:  "conv-1",
		},
		{
			name: "total is derived when agy omits it",
			stream: `{"event":"result","result":{"status":"SUCCESS","usage":{"input_tokens":3,"output_tokens":4}}}
`,
			wantUsage: Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
		},
		{
			name: "non-SUCCESS result is an error carrying agy's message",
			stream: `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"partial"}}
{"event":"result","result":{"status":"ERROR","error":"model overloaded"}}
`,
			wantText: "partial",
			wantErr:  "model overloaded",
		},
		{
			name: "status stands in when there is no message",
			stream: `{"event":"result","result":{"status":"CANCELLED"}}
`,
			wantErr: "CANCELLED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var chunks []string
			res, err := parseAgyStream(strings.NewReader(tt.stream), collector(&chunks))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
			}
			if got := joined(chunks); got != tt.wantText {
				t.Errorf("text = %q, want %q", got, tt.wantText)
			}
			if res.Usage != tt.wantUsage {
				t.Errorf("usage = %+v, want %+v", res.Usage, tt.wantUsage)
			}
			if tt.wantConv != "" && res.ConversationID != tt.wantConv {
				t.Errorf("conversation = %q, want %q", res.ConversationID, tt.wantConv)
			}
		})
	}
}

func TestParseAgyStreamStopsOnEmitError(t *testing.T) {
	sentinel := errors.New("client gone")
	stream := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"one"}}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"two"}}
`
	n := 0
	_, err := parseAgyStream(strings.NewReader(stream), func(string) error {
		n++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if n != 1 {
		t.Errorf("emit called %d times, want 1 (must stop immediately)", n)
	}
}

func TestParseAgyModels(t *testing.T) {
	out := "Fetching available models...\ngemini-3.8-flash-low\tGemini 3.8 Flash (Low)\nclaude-x\tClaude X\n\n"
	got := parseAgyModels(out)
	want := []Model{
		{ID: "gemini-3.8-flash-low", DisplayName: "Gemini 3.8 Flash (Low)"},
		{ID: "claude-x", DisplayName: "Claude X"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAgyDetectMissingBinary(t *testing.T) {
	a := agy{}
	if _, ok := a.Detect(map[string]string{"bin": "/definitely/not/here/agy"}); ok {
		t.Error("Detect on a missing absolute path: want false")
	}
	// A nil option map must not panic and falls back to the default binary.
	if path, _ := a.Detect(nil); path == "" {
		t.Error("Detect(nil) returned an empty path")
	}
}

func TestAgyOptionSpecKeys(t *testing.T) {
	var keys []string
	for _, o := range (agy{}).OptionSpec() {
		keys = append(keys, o.Key)
	}
	if len(keys) != 3 || keys[0] != "bin" || keys[1] != "flags" || keys[2] != "workdir" {
		t.Errorf("OptionSpec keys = %v, want [bin flags workdir]", keys)
	}
}

// Spawning adapters must not run in the server's own directory: these CLIs are
// autonomous agents that write files.
func TestWorkdirDefaultsToThrowawayDir(t *testing.T) {
	dir, cleanup, err := workdir(nil)
	if err != nil {
		t.Fatalf("workdir: %v", err)
	}
	if dir == "" {
		t.Fatal("workdir returned an empty directory")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp workdir not created: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp workdir survived cleanup: %v", err)
	}
}

func TestWorkdirHonorsConfiguredPath(t *testing.T) {
	want := t.TempDir()
	dir, cleanup, err := workdir(map[string]string{"workdir": want})
	if err != nil {
		t.Fatalf("workdir: %v", err)
	}
	defer cleanup()
	if dir != want {
		t.Errorf("workdir = %q, want %q", dir, want)
	}
	cleanup()
	if _, err := os.Stat(want); err != nil {
		t.Errorf("cleanup removed a caller-owned directory: %v", err)
	}
}

// A shell command must actually execute inside the chosen directory.
func TestShellcmdRunsInWorkdir(t *testing.T) {
	requirePOSIXShell(t)
	want := t.TempDir()
	var got string
	_, err := (shellcmd{}).Generate(context.Background(), Request{
		Messages: []Message{{Role: "user", Text: "x"}},
		Options:  map[string]string{"cmd": "pwd", "workdir": want},
	}, func(c string) error { got += c; return nil })
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// macOS reports /private/var... for /var..., so compare resolved paths.
	resolved, _ := filepath.EvalSymlinks(want)
	if g := strings.TrimSpace(got); g != want && g != resolved {
		t.Errorf("ran in %q, want %q", g, want)
	}
}

// ---------- shellcmd ----------

func TestShellcmdGenerate(t *testing.T) {
	requirePOSIXShell(t)
	tests := []struct {
		name    string
		cmd     string
		prompt  string
		want    string
		wantErr string
	}{
		{"stdin mode", "tr a-z A-Z", "hello there", "HELLO THERE", ""},
		{"placeholder mode", "printf '%s' {{PROMPT}} | tr a-z A-Z", "hello there", "HELLO THERE", ""},
		{"quotes survive placeholder substitution", "printf '%s' {{PROMPT}}", "it's fine", "it's fine", ""},
		{"failure reports stderr", "echo boom 1>&2; exit 3", "x", "", "boom"},
		{"missing cmd option", "", "x", "", `option "cmd" is required`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var chunks []string
			req := Request{
				Messages: []Message{{Role: "user", Text: tt.prompt}},
				Options:  map[string]string{"cmd": tt.cmd},
			}
			_, err := (shellcmd{}).Generate(context.Background(), req, collector(&chunks))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
			}
			if got := strings.TrimRight(joined(chunks), "\n"); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShellcmdModelsAndDetect(t *testing.T) {
	s := shellcmd{}
	if _, ok := s.Detect(nil); ok {
		t.Error("Detect with no cmd: want false")
	}
	path, ok := s.Detect(map[string]string{"cmd": "cat"})
	if !ok || path != shellName() {
		t.Errorf("Detect = (%q, %t), want (%s, true)", path, ok, shellName())
	}

	models, err := s.Models(context.Background(), nil)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "custom" {
		t.Errorf("Models = %+v, want one synthetic 'custom'", models)
	}
	models, _ = s.Models(context.Background(), map[string]string{"model_name": "my-cli"})
	if len(models) != 1 || models[0].ID != "my-cli" {
		t.Errorf("Models = %+v, want one synthetic 'my-cli'", models)
	}
}

// ---------- echo ----------

func TestEchoGenerate(t *testing.T) {
	var chunks []string
	req := Request{Messages: []Message{{Role: "user", Text: "hello big world"}}}
	res, err := (echo{}).Generate(context.Background(), req, collector(&chunks))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got, want := joined(chunks), "echo> hello big world"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if len(chunks) != 4 {
		t.Errorf("chunks = %d (%q), want 4 word-ish pieces", len(chunks), chunks)
	}
	if res.Model != "echo" {
		t.Errorf("Result.Model = %q, want echo", res.Model)
	}
}

func TestEchoCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (echo{}).Generate(ctx, Request{Messages: []Message{{Text: "a b"}}}, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// ---------- upstream ----------

const sseFixture = `: keep-alive

data: {"id":"chatcmpl-1","model":"gpt-test","choices":[{"delta":{"role":"assistant"}}]}

data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-1","choices":[{"delta":{"content":" world"}}]}

data: {"id":"chatcmpl-1","choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}

data: [DONE]

data: {"choices":[{"delta":{"content":"never"}}]}
`

func TestUpstreamGenerate(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseFixture)
	}))
	defer srv.Close()

	var chunks []string
	req := Request{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Text: "hi"}},
		Options:  map[string]string{"url": srv.URL + "/", "api_key": "sk-123"},
	}
	res, err := (upstream{}).Generate(context.Background(), req, collector(&chunks))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got, want := joined(chunks), "Hello world"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if want := (Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9}); res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBody, `"content":"hi"`) {
		t.Errorf("request body = %s", gotBody)
	}
}

func TestUpstreamGenerateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"no credit"}}`, http.StatusPaymentRequired)
	}))
	defer srv.Close()

	req := Request{Options: map[string]string{"url": srv.URL}}
	_, err := (upstream{}).Generate(context.Background(), req, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no credit") {
		t.Fatalf("err = %v, want it to mention the upstream body", err)
	}
}

func TestUpstreamModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"a/b","name":"A B"},{"id":"c"},{"id":""}]}`)
	}))
	defer srv.Close()

	got, err := (upstream{}).Models(context.Background(), map[string]string{"url": srv.URL})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	want := []Model{{ID: "a/b", DisplayName: "A B"}, {ID: "c", DisplayName: "c"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUpstreamModelsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := (upstream{}).Models(context.Background(), map[string]string{"url": srv.URL}); err == nil {
		t.Error("want an error for a non-2xx /models response")
	}
	if _, err := (upstream{}).Models(context.Background(), nil); err == nil {
		t.Error("want an error when url is unset")
	}
}

func TestUpstreamDetect(t *testing.T) {
	if _, ok := (upstream{}).Detect(nil); ok {
		t.Error("Detect with no url: want false")
	}
	path, ok := (upstream{}).Detect(map[string]string{"url": "https://x/v1/"})
	if !ok || path != "https://x/v1" {
		t.Errorf("Detect = (%q, %t), want (https://x/v1, true)", path, ok)
	}
}

func TestUpstreamOptionSpecSecrets(t *testing.T) {
	for _, o := range (upstream{}).OptionSpec() {
		if o.Key == "api_key" && (!o.Secret || !o.Required) {
			t.Errorf("api_key option = %+v, want Secret and Required", o)
		}
	}
}

// ---------- conversation resume ----------

func TestCanResume(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"agy", true},
		{"echo", false},
		{"shellcmd", false},
		{"upstream", false},
	}
	for _, tc := range cases {
		a, err := Get(tc.id)
		if err != nil {
			t.Fatalf("Get(%q): %v", tc.id, err)
		}
		if got := CanResume(a); got != tc.want {
			t.Errorf("CanResume(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// argAfter returns the value following flag in args, and whether flag is present.
func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

func countArg(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

func TestAgyArgsConversation(t *testing.T) {
	req := Request{
		Model:          "m1",
		Messages:       []Message{{Role: "user", Text: "hi"}},
		ConversationID: "conv-123",
	}
	args := agyArgs(req)
	got, ok := argAfter(args, "--conversation")
	if !ok {
		t.Fatalf("--conversation missing from %v", args)
	}
	if got != "conv-123" {
		t.Errorf("--conversation = %q, want %q", got, "conv-123")
	}
	// The pre-existing behaviour must be untouched.
	if m, _ := argAfter(args, "--model"); m != "m1" {
		t.Errorf("--model = %q, want m1", m)
	}
	if p, _ := argAfter(args, "-p"); p != "hi" {
		t.Errorf("-p = %q, want hi", p)
	}
	if f, _ := argAfter(args, "--output-format"); f != "stream-json" {
		t.Errorf("--output-format = %q, want stream-json", f)
	}
}

func TestAgyArgsNoConversation(t *testing.T) {
	req := Request{Messages: []Message{{Role: "user", Text: "hi"}}}
	args := agyArgs(req)
	if _, ok := argAfter(args, "--conversation"); ok {
		t.Errorf("--conversation present without a ConversationID: %v", args)
	}
	if _, ok := argAfter(args, "--model"); ok {
		t.Errorf("--model present for an empty model: %v", args)
	}
}

func TestAgyArgsKeepsConfiguredFlags(t *testing.T) {
	req := Request{
		Messages: []Message{{Role: "user", Text: "hi"}},
		Options:  map[string]string{"flags": "--yolo --verbose"},
	}
	args := agyArgs(req)
	if !contains(args, "--yolo") || !contains(args, "--verbose") {
		t.Errorf("configured flags dropped: %v", args)
	}
}

func TestAgyArgsConversationFlagGuard(t *testing.T) {
	cases := []struct {
		name  string
		flags string
	}{
		{"long form with value", "--conversation pinned-id --verbose"},
		{"long form with equals", "--conversation=pinned-id --verbose"},
		{"short form with value", "-c pinned-id --verbose"},
		{"continue flag", "--continue --verbose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{
				Messages:       []Message{{Role: "user", Text: "hi"}},
				Options:        map[string]string{"flags": tc.flags},
				ConversationID: "req-id",
			}
			args := agyArgs(req)
			if n := countArg(args, "--conversation"); n != 1 {
				t.Errorf("--conversation appears %d times, want 1: %v", n, args)
			}
			if got, _ := argAfter(args, "--conversation"); got != "req-id" {
				t.Errorf("--conversation = %q, want the request's id: %v", got, args)
			}
			if contains(args, "pinned-id") {
				t.Errorf("operator-pinned id leaked into args: %v", args)
			}
			if contains(args, "-c") || contains(args, "--continue") || contains(args, "--conversation=pinned-id") {
				t.Errorf("configured conversation flag not stripped: %v", args)
			}
			if !contains(args, "--verbose") {
				t.Errorf("unrelated configured flag dropped: %v", args)
			}
		})
	}
}

func TestAgyArgsConversationFlagKeptWithoutRequestID(t *testing.T) {
	// No request id: the operator's explicit choice stands, and we still never
	// add a second flag.
	req := Request{
		Messages: []Message{{Role: "user", Text: "hi"}},
		Options:  map[string]string{"flags": "--conversation pinned-id"},
	}
	args := agyArgs(req)
	if n := countArg(args, "--conversation"); n != 1 {
		t.Errorf("--conversation appears %d times, want 1: %v", n, args)
	}
	if got, _ := argAfter(args, "--conversation"); got != "pinned-id" {
		t.Errorf("--conversation = %q, want pinned-id", got)
	}
}

func TestParseAgyStreamResumeKeepsConversationID(t *testing.T) {
	// agy echoes the same conversation_id back on the init event when resuming.
	const ndjson = `{"event":"init","conversation_id":"conv-123"}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"forty-two"}}
{"event":"result","result":{"status":"SUCCESS","usage":{"input_tokens":14084,"output_tokens":7}}}
`
	var chunks []string
	res, err := parseAgyStream(strings.NewReader(ndjson), collector(&chunks))
	if err != nil {
		t.Fatalf("parseAgyStream: %v", err)
	}
	if res.ConversationID != "conv-123" {
		t.Errorf("ConversationID = %q, want conv-123", res.ConversationID)
	}
	if joined(chunks) != "forty-two" {
		t.Errorf("text = %q, want forty-two", joined(chunks))
	}
	if res.Usage.PromptTokens != 14084 || res.Usage.TotalTokens != 14091 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// requirePOSIXShell skips tests whose command lines are sh scripts. The
// adapter itself is cross-platform; `tr`, `pwd` and friends are not.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test command lines are POSIX sh, not cmd.exe")
	}
}

// TestQuotePromptRoundTrip checks that whatever quotePrompt accepts really
// does reach the command as a single, unmodified argument. On unix that is
// every string; on Windows the unquotable ones are refused instead, which is
// the property asserted in TestShellcmdRefusesUnquotablePrompt.
func TestQuotePromptRoundTrip(t *testing.T) {
	requirePOSIXShell(t)
	for _, prompt := range []string{
		"plain",
		"it's fine",
		`double "quoted"`,
		"semi; echo pwned",
		"$(echo pwned)",
		"`echo pwned`",
		"back\\slash",
		"100% of $HOME",
		"two\nlines",
	} {
		quoted, ok := quotePrompt(prompt)
		if !ok {
			t.Fatalf("quotePrompt(%q) refused on a POSIX shell", prompt)
		}
		var chunks []string
		req := Request{
			Messages: []Message{{Role: "user", Text: prompt}},
			Options:  map[string]string{"cmd": "printf '%s' {{PROMPT}}"},
		}
		if _, err := (shellcmd{}).Generate(context.Background(), req, collector(&chunks)); err != nil {
			t.Fatalf("Generate(%q): %v (quoted as %s)", prompt, err, quoted)
		}
		if got := joined(chunks); got != prompt {
			t.Errorf("round trip of %q produced %q", prompt, got)
		}
	}
}

// TestShellcmdRefusesUnquotablePrompt is the Windows half of the contract: a
// prompt cmd.exe cannot quote must produce a clear error naming the stdin
// workaround, never a mangled or injected command line.
func TestShellcmdRefusesUnquotablePrompt(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd.exe quoting limits apply to Windows only")
	}
	var chunks []string
	req := Request{
		Messages: []Message{{Role: "user", Text: `a "quoted" %PATH%`}},
		Options:  map[string]string{"cmd": "more {{PROMPT}}"},
	}
	_, err := (shellcmd{}).Generate(context.Background(), req, collector(&chunks))
	if err == nil {
		t.Fatal("want an error for an unquotable prompt, got nil")
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("error = %v, want it to point at the stdin workaround", err)
	}
}
