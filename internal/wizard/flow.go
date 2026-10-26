package wizard

import (
	"context"
	"fmt"
	"time"

	"cli-to-api/internal/adapter"
	"cli-to-api/internal/store"
)

// Result is what a completed key-creation flow produced. Secrets are kept
// apart from Profile.Options so the caller can encrypt them before they ever
// reach disk; the wizard itself never persists anything.
type Result struct {
	Name    string
	Profile store.Profile
	Secrets map[string]string // adapter option key -> plaintext
}

// modelListTimeout bounds the "what can this CLI run?" call. Enumerating
// models shells out to the backend, which can be slow or hung.
const modelListTimeout = 45 * time.Second

// CreateKey walks the operator through choosing a backend, configuring it,
// and picking which models the new key may use.
func (w *Wizard) CreateKey(ctx context.Context) (*Result, error) {
	ad, err := w.pickAdapter()
	if err != nil {
		return nil, err
	}

	opts, secrets, err := w.askOptions(ad)
	if err != nil {
		return nil, err
	}

	models, err := w.pickModels(ctx, ad, opts)
	if err != nil {
		return nil, err
	}

	name, err := w.Text("Name this key:", defaultKeyName(ad), true)
	if err != nil {
		return nil, err
	}

	profile := store.Profile{
		Adapter: ad.ID(),
		Models:  models,
		Options: opts,
	}
	if len(models) > 0 {
		profile.DefaultModel = models[0]
	}
	return &Result{Name: name, Profile: profile, Secrets: secrets}, nil
}

func defaultKeyName(ad adapter.Adapter) string {
	return ad.ID() + "-key"
}

// pickAdapter lists every registered backend, marking the ones actually
// installed. Missing ones stay visible but unselectable, so the list doubles
// as documentation of what this tool supports.
func (w *Wizard) pickAdapter() (adapter.Adapter, error) {
	all := adapter.All()
	if len(all) == 0 {
		return nil, fmt.Errorf("no adapters are registered")
	}
	items := make([]Item, 0, len(all))
	for _, a := range all {
		it := Item{Label: a.ID(), Note: a.DisplayName()}
		if path, ok := a.Detect(nil); ok {
			if path != "" {
				it.Note += "  found: " + path
			}
		} else if requiresSetup(a) {
			// Not installed is different from not-yet-configured: an adapter
			// whose options supply the target (a shell command, an upstream
			// URL) cannot be detected before those are answered.
			it.Note += "  configure below"
		} else {
			it.Disabled = "not installed"
		}
		items = append(items, it)
	}
	idx, err := w.SelectOne("Which CLI should this key use?", items)
	if err != nil {
		return nil, err
	}
	return all[idx], nil
}

// requiresSetup reports whether an adapter's detectability depends on options
// the operator has not been asked for yet.
func requiresSetup(a adapter.Adapter) bool {
	for _, o := range a.OptionSpec() {
		if o.Required {
			return true
		}
	}
	return false
}

func (w *Wizard) askOptions(ad adapter.Adapter) (opts, secrets map[string]string, err error) {
	opts = map[string]string{}
	secrets = map[string]string{}
	spec := ad.OptionSpec()
	if len(spec) == 0 {
		return opts, secrets, nil
	}
	w.Dim("\nConfigure %s:", ad.DisplayName())
	for _, o := range spec {
		var val string
		if o.Secret {
			val, err = w.Secret("  " + o.Label + ":")
		} else {
			val, err = w.Text("  "+o.Label+":", o.Default, o.Required)
		}
		if err != nil {
			return nil, nil, err
		}
		if val == "" {
			continue // an unset optional knob stays out of the config entirely
		}
		if o.Secret {
			secrets[o.Key] = val
		} else {
			opts[o.Key] = val
		}
	}
	return opts, secrets, nil
}

// pickModels asks the backend what it can run and lets the operator scope the
// key to a subset. An empty list would mean "any model", which is deliberately
// not offered here: a key created by the wizard is always explicitly scoped.
func (w *Wizard) pickModels(ctx context.Context, ad adapter.Adapter, opts map[string]string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()

	w.Dim("\nAsking %s what it can run...", ad.ID())
	models, err := ad.Models(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing models from %s: %w", ad.ID(), err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s reported no models", ad.ID())
	}

	items := make([]Item, 0, len(models))
	for _, m := range models {
		items = append(items, Item{Label: m.ID, Note: m.DisplayName})
	}
	idxs, err := w.SelectMany("Which models may this key use?", items, []int{0})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, models[i].ID)
	}
	return out, nil
}
