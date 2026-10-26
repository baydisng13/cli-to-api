package adapter

import (
	"context"
	"strings"
)

func init() { Register(echo{}) }

// echo streams the prompt back, word by word. It exists so the server can be
// smoke-tested end to end with no CLI installed and no network.
type echo struct{}

func (echo) ID() string                              { return "echo" }
func (echo) DisplayName() string                     { return "Echo (testing)" }
func (echo) OptionSpec() []Option                    { return nil }
func (echo) Detect(map[string]string) (string, bool) { return "builtin", true }

func (echo) Models(context.Context, map[string]string) ([]Model, error) {
	return []Model{{ID: "echo", DisplayName: "Echo"}}, nil
}

func (echo) Generate(ctx context.Context, req Request, emit Emit) (Result, error) {
	var res Result
	res.Model = "echo"
	for _, tok := range tokenize("echo> " + req.Prompt()) {
		// No subprocess to kill, but a cancelled client should still stop us.
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := emit(tok); err != nil {
			return res, err
		}
	}
	return res, nil
}

// tokenize splits text into word-ish pieces so echoed output streams like a
// model rather than arriving in one lump.
func tokenize(s string) []string {
	fields := strings.SplitAfter(s, " ")
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
