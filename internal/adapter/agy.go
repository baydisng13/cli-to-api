package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func init() { Register(agy{}) }

// agy drives the Antigravity CLI in print mode with NDJSON output. Unlike the
// generic shell backend it understands the event stream, so tool calls and
// thinking steps never leak into the assistant message.
type agy struct{}

func (agy) ID() string          { return "agy" }
func (agy) DisplayName() string { return "Antigravity CLI" }

// SupportsSessions satisfies SessionCapable: agy reports a conversation_id on
// its init event and accepts it back via --conversation to resume with the
// prior context.
func (agy) SupportsSessions() bool { return true }

func (agy) OptionSpec() []Option {
	return []Option{
		{Key: "bin", Label: "Path to the agy binary", Default: "agy"},
		{Key: "flags", Label: "Extra flags for every invocation", Default: ""},
		WorkdirOption,
	}
}

// agyBin resolves the configured binary, tolerating a nil option map.
func agyBin(opts map[string]string) string {
	if b := strings.TrimSpace(opts["bin"]); b != "" {
		return expandHome(b)
	}
	return "agy"
}

// expandHome resolves a leading ~, which users type but exec does not expand.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}

func (agy) Detect(opts map[string]string) (string, bool) {
	bin := agyBin(opts)
	// An explicit path is checked directly; a bare name goes through $PATH.
	if strings.ContainsRune(bin, filepath.Separator) {
		fi, err := os.Stat(bin)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			return bin, false
		}
		return bin, true
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return bin, false
	}
	return path, true
}

func (agy) Models(ctx context.Context, opts map[string]string) ([]Model, error) {
	bin := agyBin(opts)
	out, err := exec.CommandContext(ctx, bin, "models").Output()
	if err != nil {
		return nil, fmt.Errorf("%s models failed: %w", bin, err)
	}
	return parseAgyModels(string(out)), nil
}

// parseAgyModels reads the "<id>\t<display name>" table. Rows without a tab
// are progress chatter ("Fetching available models...") and are dropped.
func parseAgyModels(out string) []Model {
	var models []Model
	for _, line := range strings.Split(out, "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			name = id
		}
		models = append(models, Model{ID: id, DisplayName: name})
	}
	return models
}

// agyArgs builds the agy argv for one request. It is split out from Generate
// so argument construction can be tested without spawning a process.
func agyArgs(req Request) []string {
	args := []string{"-p", req.Prompt(), "--output-format", "stream-json"}
	if req.Model != "" {
		// An unknown id surfaces as an agy error, which is what you want.
		args = append(args, "--model", req.Model)
	}
	extra := strings.Fields(strings.TrimSpace(req.Options["flags"]))
	if req.ConversationID != "" {
		// The request's id wins over anything the operator pinned in flags: a
		// conversation flag in the profile applies to every invocation, so
		// honouring it would silently merge all callers' traffic into one
		// thread. Drop the configured flag rather than passing two.
		extra = stripConversationFlags(extra)
		args = append(args, "--conversation", req.ConversationID)
	}
	return append(args, extra...)
}

// stripConversationFlags removes an operator-configured conversation flag (and
// its value, when it takes one) from extra flags.
func stripConversationFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		name, _, hasValue := strings.Cut(f, "=")
		switch name {
		case "--conversation", "-c":
			if !hasValue {
				i++ // also drop the id that follows
			}
		case "--continue":
			// A boolean "resume the last conversation" flag: no value to skip.
		default:
			out = append(out, f)
		}
	}
	return out
}

func (a agy) Generate(ctx context.Context, req Request, emit Emit) (Result, error) {
	bin := agyBin(req.Options)

	args := agyArgs(req)

	// CommandContext kills the child when ctx is cancelled.
	dir, cleanupDir, err := workdir(req.Options)
	if err != nil {
		return Result{}, err
	}
	defer cleanupDir()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("cannot start %s: %w", bin, err)
	}

	res, parseErr := parseAgyStream(stdout, emit)
	res.Model = req.Model
	if parseErr != nil {
		// The client is gone or agy reported a failure: do not wait for a
		// process that may keep streaming for minutes.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return res, parseErr
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("agy backend failed: %s", truncate(msg, 500))
	}
	return res, nil
}

// agyEvent is the subset of Antigravity's stream-json NDJSON we care about.
type agyEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Init           struct {
		ConversationID string `json:"conversation_id"`
	} `json:"init"`
	StepUpdate struct {
		ConversationID string    `json:"conversation_id"`
		State          string    `json:"state"`
		StepType       string    `json:"step_type"`
		TextDelta      string    `json:"text_delta"`
		Usage          *agyUsage `json:"usage"`
	} `json:"step_update"`
	Result struct {
		Status   string    `json:"status"`
		Response string    `json:"response"`
		Error    string    `json:"error"`
		Usage    *agyUsage `json:"usage"`
	} `json:"result"`
}

type agyUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// parseAgyStream turns the NDJSON event stream into emitted text. It is split
// out from Generate so the protocol can be tested without spawning agy.
func parseAgyStream(r io.Reader, emit Emit) (Result, error) {
	var res Result
	var resultErr error

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // the init event is large
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue // progress noise like "Fetching available models..."
		}
		var ev agyEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "init":
			res.ConversationID = ev.ConversationID
			if res.ConversationID == "" {
				res.ConversationID = ev.Init.ConversationID
			}
		case "step_update":
			su := ev.StepUpdate
			if su.StepType != "agent_response" || su.TextDelta == "" {
				continue // tool calls, thinking, status transitions
			}
			if err := emit(su.TextDelta); err != nil {
				return res, err
			}
			if su.Usage != nil {
				res.Usage = convertAgyUsage(su.Usage)
			}
		case "result":
			if ev.Result.Usage != nil {
				res.Usage = convertAgyUsage(ev.Result.Usage)
			}
			if ev.Result.Status != "" && ev.Result.Status != "SUCCESS" {
				msg := ev.Result.Error
				if msg == "" {
					msg = ev.Result.Status
				}
				resultErr = fmt.Errorf("agy returned %s: %s", ev.Result.Status, truncate(msg, 300))
			}
		}
	}
	if resultErr != nil {
		return res, resultErr
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("reading agy output: %w", err)
	}
	return res, nil
}

func convertAgyUsage(u *agyUsage) Usage {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: total}
}
