package adapter

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func init() { Register(shellcmd{}) }

// shellcmd runs an arbitrary shell command and streams its stdout verbatim.
// It is the escape hatch for CLIs that have no structured output mode.
type shellcmd struct{}

func (shellcmd) ID() string          { return "shellcmd" }
func (shellcmd) DisplayName() string { return "Custom shell command" }

func (shellcmd) OptionSpec() []Option {
	return []Option{
		{Key: "cmd", Label: "Shell command ({{PROMPT}} is substituted, else prompt goes to stdin)", Required: true},
		{Key: "model_name", Label: "Model name to advertise", Default: "custom"},
		WorkdirOption,
	}
}

func (shellcmd) Detect(opts map[string]string) (string, bool) {
	// There is nothing to look up: the command is whatever the user typed, and
	// sh always exists. Only an empty command makes this adapter unusable.
	return "sh", strings.TrimSpace(opts["cmd"]) != ""
}

func (shellcmd) Models(_ context.Context, opts map[string]string) ([]Model, error) {
	name := strings.TrimSpace(opts["model_name"])
	if name == "" {
		name = "custom"
	}
	return []Model{{ID: name, DisplayName: name}}, nil
}

func (shellcmd) Generate(ctx context.Context, req Request, emit Emit) (Result, error) {
	cmdline := strings.TrimSpace(req.Options["cmd"])
	if cmdline == "" {
		return Result{}, fmt.Errorf("shellcmd: option %q is required", "cmd")
	}

	prompt := req.Prompt()
	// Commands that name the prompt get it as an argument; everything else
	// reads it from stdin, which is the safer default for long prompts.
	usesPlaceholder := strings.Contains(cmdline, "{{PROMPT}}")
	if usesPlaceholder {
		cmdline = strings.ReplaceAll(cmdline, "{{PROMPT}}", shellQuote(prompt))
	}

	dir, cleanupDir, err := workdir(req.Options)
	if err != nil {
		return Result{}, err
	}
	defer cleanupDir()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = dir
	if !usesPlaceholder {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	res := Result{Model: modelLabel(req)}

	// Stream whatever bytes appear, as they appear.
	rd := bufio.NewReader(stdout)
	buf := make([]byte, 4096)
	for {
		n, rerr := rd.Read(buf)
		if n > 0 {
			if err := emit(string(buf[:n])); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return res, err
			}
		}
		if rerr != nil {
			break // io.EOF or pipe closed
		}
	}

	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("shell command failed: %s", truncate(msg, 500))
	}
	return res, nil
}

// modelLabel reports what this backend "ran", which is only ever a label.
func modelLabel(req Request) string {
	if req.Model != "" {
		return req.Model
	}
	if n := strings.TrimSpace(req.Options["model_name"]); n != "" {
		return n
	}
	return "custom"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// truncate shortens a backend error for the wire; newlines would otherwise
// break the single-line JSON error message into something unreadable.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
