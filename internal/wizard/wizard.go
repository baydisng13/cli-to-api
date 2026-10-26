// Package wizard is a dependency-free interactive prompt set: numbered
// single-select, numbered multi-select, free text, and non-echoing secrets.
//
// Deliberately line-based rather than a full-screen TUI: it works over ssh,
// in a dumb terminal, and its transcript stays readable in scrollback.
package wizard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ErrAborted is returned when the user hits EOF (Ctrl-D) or answers no to a
// confirmation that gates the rest of the flow.
var ErrAborted = errors.New("aborted")

// Item is one choice in a select prompt.
type Item struct {
	Label    string // primary text, e.g. "agy"
	Note     string // dim suffix, e.g. "Antigravity CLI — found at /usr/local/bin/agy"
	Disabled string // when non-empty, the item is shown but cannot be picked, and this says why
}

// Wizard reads answers from in and renders prompts to out.
type Wizard struct {
	in  *bufio.Reader
	out io.Writer
	// NoColor disables ANSI styling; set automatically when out is not a terminal.
	NoColor bool
}

// New returns a Wizard over stdin/stdout.
func New() *Wizard { return NewWith(os.Stdin, os.Stdout) }

// NewWith returns a Wizard over arbitrary streams, which is what tests use.
func NewWith(in io.Reader, out io.Writer) *Wizard {
	w := &Wizard{in: bufio.NewReader(in), out: out}
	if os.Getenv("NO_COLOR") != "" {
		w.NoColor = true
	}
	if f, ok := out.(*os.File); !ok || !isTerminal(f) {
		w.NoColor = true
	}
	return w
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

func (w *Wizard) style(code, s string) string {
	if w.NoColor {
		return s
	}
	return code + s + ansiReset
}

func (w *Wizard) printf(format string, args ...any) {
	fmt.Fprintf(w.out, format, args...)
}

// Info prints a plain line.
func (w *Wizard) Info(format string, args ...any) { w.printf(format+"\n", args...) }

// Dim prints a de-emphasized line.
func (w *Wizard) Dim(format string, args ...any) {
	w.printf("%s\n", w.style(ansiDim, fmt.Sprintf(format, args...)))
}

// Success prints a line marked as good news.
func (w *Wizard) Success(format string, args ...any) {
	w.printf("%s %s\n", w.style(ansiGreen, "ok"), fmt.Sprintf(format, args...))
}

func (w *Wizard) readLine() (string, error) {
	line, err := w.in.ReadString('\n')
	if err != nil {
		if err == io.EOF && strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line), nil
		}
		if err == io.EOF {
			return "", ErrAborted
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Text asks a free-text question. An empty answer falls back to def; when def
// is empty and required is set, it re-asks.
func (w *Wizard) Text(label, def string, required bool) (string, error) {
	for {
		if def != "" {
			w.printf("%s %s %s ", w.style(ansiBold, "?"), label, w.style(ansiDim, "["+def+"]"))
		} else {
			w.printf("%s %s ", w.style(ansiBold, "?"), label)
		}
		ans, err := w.readLine()
		if err != nil {
			return "", err
		}
		if ans == "" {
			ans = def
		}
		if ans == "" && required {
			w.Dim("  required")
			continue
		}
		return ans, nil
	}
}

// Secret asks for a value without echoing it. Falls back to a visible prompt
// when the terminal cannot be put into no-echo mode, and says so, so the user
// is never misled about whether their input is on screen.
func (w *Wizard) Secret(label string) (string, error) {
	restore, ok := disableEcho()
	if !ok {
		w.Dim("  (terminal echo could not be disabled; input will be visible)")
	}
	w.printf("%s %s ", w.style(ansiBold, "?"), label)
	ans, err := w.readLine()
	if ok {
		restore()
		w.printf("\n")
	}
	return ans, err
}

// disableEcho shells out to stty, which avoids a terminal library dependency.
// The returned func restores the previous mode.
func disableEcho() (restore func(), ok bool) {
	if !isTerminal(os.Stdin) {
		return func() {}, false
	}
	prev, err := exec.Command("stty", "-f", "/dev/tty", "-g").Output()
	if err != nil {
		return func() {}, false
	}
	if err := exec.Command("stty", "-f", "/dev/tty", "-echo").Run(); err != nil {
		return func() {}, false
	}
	return func() {
		_ = exec.Command("stty", "-f", "/dev/tty", strings.TrimSpace(string(prev))).Run()
	}, true
}

// Confirm asks a yes/no question.
func (w *Wizard) Confirm(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		w.printf("%s %s %s ", w.style(ansiBold, "?"), label, w.style(ansiDim, "["+hint+"]"))
		ans, err := w.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		w.Dim("  answer y or n")
	}
}

// SelectOne renders a numbered list and returns the chosen index.
func (w *Wizard) SelectOne(label string, items []Item) (int, error) {
	if len(items) == 0 {
		return 0, errors.New("nothing to choose from")
	}
	w.printf("%s %s\n", w.style(ansiBold, "?"), label)
	w.renderItems(items, nil)
	for {
		w.printf("%s ", w.style(ansiDim, "  number >"))
		ans, err := w.readLine()
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(ans)
		if err != nil || n < 1 || n > len(items) {
			w.Dim("  enter a number between 1 and %d", len(items))
			continue
		}
		if why := items[n-1].Disabled; why != "" {
			w.Dim("  %s is unavailable: %s", items[n-1].Label, why)
			continue
		}
		return n - 1, nil
	}
}

// SelectMany renders a numbered list and returns the chosen indexes. Input
// accepts comma or space separated numbers and inclusive ranges ("1-3"), plus
// "all" and "none". preselected indexes are offered as the default.
func (w *Wizard) SelectMany(label string, items []Item, preselected []int) ([]int, error) {
	if len(items) == 0 {
		return nil, errors.New("nothing to choose from")
	}
	sel := map[int]bool{}
	for _, i := range preselected {
		if i >= 0 && i < len(items) {
			sel[i] = true
		}
	}
	w.printf("%s %s\n", w.style(ansiBold, "?"), label)
	w.renderItems(items, sel)
	w.Dim("  numbers, ranges (1-3), \"all\", or \"none\"; empty keeps the marked ones")
	for {
		w.printf("%s ", w.style(ansiDim, "  select >"))
		ans, err := w.readLine()
		if err != nil {
			return nil, err
		}
		picked, err := parseSelection(ans, len(items), sel)
		if err != nil {
			w.Dim("  %s", err)
			continue
		}
		if len(picked) == 0 {
			w.Dim("  pick at least one")
			continue
		}
		for _, i := range picked {
			if why := items[i].Disabled; why != "" {
				w.Dim("  %s is unavailable: %s", items[i].Label, why)
				return nil, fmt.Errorf("item %d unavailable", i+1)
			}
		}
		return picked, nil
	}
}

func (w *Wizard) renderItems(items []Item, sel map[int]bool) {
	for i, it := range items {
		mark := " "
		if sel != nil {
			mark = " "
			if sel[i] {
				mark = "x"
			}
			mark = "[" + mark + "]"
		}
		line := fmt.Sprintf("  %2d) %s %s", i+1, mark, it.Label)
		if sel == nil {
			line = fmt.Sprintf("  %2d) %s", i+1, it.Label)
		}
		if it.Note != "" {
			line += "  " + w.style(ansiDim, it.Note)
		}
		if it.Disabled != "" {
			line += "  " + w.style(ansiRed, "("+it.Disabled+")")
		}
		w.printf("%s\n", line)
	}
}

// parseSelection turns a selection expression into sorted, de-duplicated
// zero-based indexes. An empty expression keeps the current selection.
func parseSelection(expr string, n int, current map[int]bool) ([]int, error) {
	expr = strings.TrimSpace(strings.ToLower(expr))
	chosen := map[int]bool{}
	switch expr {
	case "":
		for i := range current {
			if current[i] {
				chosen[i] = true
			}
		}
		return sortedKeys(chosen), nil
	case "all":
		for i := 0; i < n; i++ {
			chosen[i] = true
		}
		return sortedKeys(chosen), nil
	case "none":
		return nil, nil
	}

	fields := strings.FieldsFunc(expr, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	for _, f := range fields {
		if lo, hi, ok := strings.Cut(f, "-"); ok {
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil || a < 1 || b > n || a > b {
				return nil, fmt.Errorf("bad range %q", f)
			}
			for i := a; i <= b; i++ {
				chosen[i-1] = true
			}
			continue
		}
		v, err := strconv.Atoi(f)
		if err != nil || v < 1 || v > n {
			return nil, fmt.Errorf("bad selection %q", f)
		}
		chosen[v-1] = true
	}
	return sortedKeys(chosen), nil
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
