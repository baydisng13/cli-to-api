package wizard

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"cli-to-api/internal/adapter"
)

func newTest(input string) (*Wizard, *bytes.Buffer) {
	var out bytes.Buffer
	w := NewWith(strings.NewReader(input), &out)
	w.NoColor = true
	return w, &out
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		expr    string
		n       int
		current map[int]bool
		want    []int
		wantErr bool
	}{
		{expr: "1", n: 3, want: []int{0}},
		{expr: "1,3", n: 3, want: []int{0, 2}},
		{expr: "1 3", n: 3, want: []int{0, 2}},
		{expr: "1-3", n: 3, want: []int{0, 1, 2}},
		{expr: "3,1-2", n: 3, want: []int{0, 1, 2}},
		{expr: "2,2", n: 3, want: []int{1}}, // duplicates collapse
		{expr: "all", n: 2, want: []int{0, 1}},
		{expr: "none", n: 2, want: nil},
		{expr: "", n: 3, current: map[int]bool{1: true}, want: []int{1}},
		{expr: "0", n: 3, wantErr: true},
		{expr: "4", n: 3, wantErr: true},
		{expr: "3-1", n: 3, wantErr: true},
		{expr: "x", n: 3, wantErr: true},
	}
	for _, c := range cases {
		got, err := parseSelection(c.expr, c.n, c.current)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSelection(%q) = %v, want error", c.expr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSelection(%q): %v", c.expr, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseSelection(%q) = %v, want %v", c.expr, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseSelection(%q) = %v, want %v", c.expr, got, c.want)
				break
			}
		}
	}
}

func TestTextDefaultsAndRequired(t *testing.T) {
	w, _ := newTest("\nhello\n")
	got, err := w.Text("label", "fallback", false)
	if err != nil || got != "fallback" {
		t.Fatalf("Text empty = %q, %v; want fallback", got, err)
	}
	got, err = w.Text("label", "", true)
	if err != nil || got != "hello" {
		t.Fatalf("Text = %q, %v; want hello", got, err)
	}
}

func TestTextRequiredReasks(t *testing.T) {
	w, out := newTest("\n\nfinally\n")
	got, err := w.Text("label", "", true)
	if err != nil || got != "finally" {
		t.Fatalf("Text = %q, %v; want finally", got, err)
	}
	if n := strings.Count(out.String(), "required"); n != 2 {
		t.Errorf("re-asked %d times, want 2", n)
	}
}

func TestSelectOneRejectsOutOfRangeAndDisabled(t *testing.T) {
	items := []Item{
		{Label: "a"},
		{Label: "b", Disabled: "not installed"},
		{Label: "c"},
	}
	w, out := newTest("9\n2\n3\n")
	idx, err := w.SelectOne("pick", items)
	if err != nil {
		t.Fatalf("SelectOne: %v", err)
	}
	if idx != 2 {
		t.Errorf("idx = %d, want 2", idx)
	}
	s := out.String()
	if !strings.Contains(s, "between 1 and 3") {
		t.Error("out-of-range input was not rejected")
	}
	if !strings.Contains(s, "unavailable") {
		t.Error("disabled item was selectable")
	}
}

func TestConfirm(t *testing.T) {
	for _, c := range []struct {
		in   string
		def  bool
		want bool
	}{
		{"y\n", false, true},
		{"yes\n", false, true},
		{"n\n", true, false},
		{"\n", true, true},
		{"\n", false, false},
	} {
		w, _ := newTest(c.in)
		got, err := w.Confirm("ok?", c.def)
		if err != nil {
			t.Fatalf("Confirm(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Confirm(%q, def=%v) = %v, want %v", c.in, c.def, got, c.want)
		}
	}
}

func TestEOFAborts(t *testing.T) {
	w, _ := newTest("")
	if _, err := w.Text("label", "", true); err != ErrAborted {
		t.Errorf("err = %v, want ErrAborted", err)
	}
}

// ---------- full flow ----------

type fakeAdapter struct {
	id      string
	models  []adapter.Model
	opts    []adapter.Option
	lastOpt map[string]string
}

func (f *fakeAdapter) ID() string          { return f.id }
func (f *fakeAdapter) DisplayName() string { return "Fake " + f.id }
func (f *fakeAdapter) Detect(map[string]string) (string, bool) {
	return "/fake/" + f.id, true
}
func (f *fakeAdapter) Models(_ context.Context, opts map[string]string) ([]adapter.Model, error) {
	f.lastOpt = opts
	return f.models, nil
}
func (f *fakeAdapter) Generate(context.Context, adapter.Request, adapter.Emit) (adapter.Result, error) {
	return adapter.Result{}, nil
}
func (f *fakeAdapter) OptionSpec() []adapter.Option { return f.opts }

func TestCreateKeyFlow(t *testing.T) {
	fake := &fakeAdapter{
		id: "wizardfake",
		models: []adapter.Model{
			{ID: "m-fast", DisplayName: "Fast"},
			{ID: "m-slow", DisplayName: "Slow"},
			{ID: "m-huge", DisplayName: "Huge"},
		},
		opts: []adapter.Option{
			{Key: "bin", Label: "Binary", Default: "fakebin"},
			{Key: "api_key", Label: "API key", Secret: true},
		},
	}
	adapter.Register(fake)

	// Pick our fake adapter by its position in the sorted registry.
	pos := 0
	for i, a := range adapter.All() {
		if a.ID() == fake.id {
			pos = i + 1
		}
	}

	input := strings.Join([]string{
		itoa(pos), // which adapter
		"",        // bin: accept default
		"s3cr3t",  // api_key
		"1,3",     // models
		"my-key",  // name
	}, "\n") + "\n"

	w, _ := newTest(input)
	res, err := w.CreateKey(context.Background())
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if res.Name != "my-key" {
		t.Errorf("Name = %q, want my-key", res.Name)
	}
	if res.Profile.Adapter != fake.id {
		t.Errorf("Adapter = %q, want %q", res.Profile.Adapter, fake.id)
	}
	if got := res.Profile.Models; len(got) != 2 || got[0] != "m-fast" || got[1] != "m-huge" {
		t.Errorf("Models = %v, want [m-fast m-huge]", got)
	}
	if res.Profile.DefaultModel != "m-fast" {
		t.Errorf("DefaultModel = %q, want m-fast", res.Profile.DefaultModel)
	}
	if res.Profile.Options["bin"] != "fakebin" {
		t.Errorf("Options[bin] = %q, want fakebin", res.Profile.Options["bin"])
	}
	// A secret must never land in the plaintext options map.
	if _, leaked := res.Profile.Options["api_key"]; leaked {
		t.Error("secret option leaked into Profile.Options")
	}
	if res.Secrets["api_key"] != "s3cr3t" {
		t.Errorf("Secrets[api_key] = %q, want s3cr3t", res.Secrets["api_key"])
	}
	// Options collected before the model listing must reach the adapter.
	if fake.lastOpt["bin"] != "fakebin" {
		t.Errorf("Models() saw opts %v, want bin=fakebin", fake.lastOpt)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
