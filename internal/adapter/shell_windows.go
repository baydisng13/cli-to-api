//go:build windows

package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// comspec is the command interpreter, honouring COMSPEC when the environment
// sets one (it is how a constrained or redirected shell is pointed at).
func comspec() string {
	if c := strings.TrimSpace(os.Getenv("COMSPEC")); c != "" {
		return c
	}
	return "cmd.exe"
}

// shellName is what Detect reports as the "binary" backing this adapter.
func shellName() string { return filepath.Base(comspec()) }

// shellCommand runs cmdline through cmd.exe.
//
// The command line is built by hand rather than left to os/exec. Go quotes
// arguments by the C runtime's rules, which cmd.exe does not follow, so a
// command containing quotes would arrive mangled. With /S, cmd's rule is
// simple and documented: strip the first and last quote of the /C argument and
// take everything between them verbatim. That is exactly what is wanted here,
// since cmdline is already a shell command the user wrote.
func shellCommand(ctx context.Context, cmdline string) *exec.Cmd {
	shell := comspec()
	cmd := exec.CommandContext(ctx, shell, "/S", "/C", cmdline)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: syscall.EscapeArg(shell) + ` /S /C "` + cmdline + `"`,
	}
	return cmd
}

// quotePrompt renders s as one cmd.exe argument, or reports that it cannot.
//
// cmd.exe has no complete escaping scheme: a double quote inside a quoted
// string cannot be escaped, and %VAR% expansion happens inside quotes. Rather
// than ship quoting that silently corrupts a prompt - or worse, lets one
// become a second command - anything that cannot be quoted exactly is refused,
// and Generate tells the user to let the prompt go to stdin instead. That path
// has no quoting problem at all and is the recommended mode on Windows.
func quotePrompt(s string) (string, bool) {
	if strings.ContainsAny(s, "\"%!\r\n\x00") {
		return "", false
	}
	// Inside double quotes cmd treats & | < > ^ as literal text, and the
	// characters that would still be special were rejected above.
	return `"` + s + `"`, true
}
