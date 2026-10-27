//go:build !windows

package adapter

import (
	"context"
	"os/exec"
	"strings"
)

// shellName is what Detect reports as the "binary" backing this adapter.
func shellName() string { return "sh" }

// shellCommand runs cmdline through the POSIX shell.
func shellCommand(ctx context.Context, cmdline string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", cmdline)
}

// quotePrompt renders s as one POSIX shell word. Single quotes suppress every
// expansion, and the only character they cannot contain is a single quote,
// which is spliced back in, so this never has to refuse.
func quotePrompt(s string) (string, bool) {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", true
}
