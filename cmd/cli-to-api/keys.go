package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"cli-to-api/internal/auth"
	"cli-to-api/internal/secret"
	"cli-to-api/internal/store"
	"cli-to-api/internal/wizard"
)

func cmdKey(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cli-to-api key create|list|revoke|delete")
	}
	switch args[0] {
	case "create", "new", "add":
		return cmdKeyCreate(args[1:])
	case "list", "ls":
		return cmdKeyList()
	case "revoke":
		return cmdKeyMutate(args[1:], "revoke")
	case "delete", "rm":
		return cmdKeyMutate(args[1:], "delete")
	default:
		return fmt.Errorf("unknown key subcommand %q", args[0])
	}
}

func cmdKeyCreate(args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	box, err := secret.Open(st.Dir())
	if err != nil {
		return fmt.Errorf("opening the secret box: %w", err)
	}

	w := wizard.New()
	res, err := w.CreateKey(context.Background())
	if err != nil {
		if errors.Is(err, wizard.ErrAborted) {
			return errors.New("cancelled")
		}
		return err
	}

	// Encrypt before anything touches disk.
	if len(res.Secrets) > 0 {
		res.Profile.SecretOptions = make(map[string]string, len(res.Secrets))
		for k, plain := range res.Secrets {
			sealed, err := box.Seal(plain)
			if err != nil {
				return fmt.Errorf("encrypting option %q: %w", k, err)
			}
			res.Profile.SecretOptions[k] = sealed
		}
	}

	plaintext, id, hash, err := auth.Generate()
	if err != nil {
		return err
	}
	key := &store.Key{
		ID:        id,
		Name:      res.Name,
		Hash:      hash,
		Profile:   res.Profile,
		CreatedAt: time.Now(),
	}
	if err := st.Add(key); err != nil {
		return err
	}
	if err := st.Save(); err != nil {
		return err
	}

	addr := st.Addr()
	if addr == "" {
		addr = ":8080"
	}
	base := "http://localhost" + addr + "/v1"

	w.Info("")
	w.Success("key %s created", shortID(id))
	w.Info("")
	w.Info("  %s", plaintext)
	w.Dim("  ^ copy this now; only its hash is stored, so it cannot be shown again")
	w.Info("")
	w.Info("start the server:")
	w.Info("  cli-to-api start")
	w.Info("")
	w.Info("then call it:")
	w.Info("  curl %s/chat/completions \\", base)
	w.Info("    -H 'Authorization: Bearer %s' \\", plaintext)
	w.Info("    -H 'Content-Type: application/json' \\")
	w.Info("    -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'", res.Profile.DefaultModel)
	w.Info("")
	w.Dim("any OpenAI client works: base_url=%s, api_key=the key above", base)
	return nil
}

func cmdKeyList() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	keys := st.List()
	if len(keys) == 0 {
		fmt.Println("no keys yet — run: cli-to-api key create")
		return nil
	}
	fmt.Printf("%s  %s  %s  %s  %s\n",
		pad("ID", 10), pad("NAME", 20), pad("ADAPTER", 10), pad("MODELS", 34), "LAST USED")
	for _, k := range keys {
		models := strings.Join(k.Profile.Models, ",")
		if models == "" {
			models = "(any)"
		}
		if len(models) > 32 {
			models = models[:31] + "…"
		}
		last := "never"
		if k.LastUsedAt != nil {
			last = k.LastUsedAt.Format("2006-01-02 15:04")
		}
		name := k.Name
		if k.Revoked {
			name += " (revoked)"
		}
		fmt.Printf("%s  %s  %s  %s  %s\n",
			pad(shortID(k.ID), 10), pad(name, 20), pad(k.Profile.Adapter, 10), pad(models, 34), last)
	}
	return nil
}

func cmdKeyMutate(args []string, action string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: cli-to-api key %s <id>", action)
	}
	id := args[0]
	st, err := openStore()
	if err != nil {
		return err
	}
	k, ok := st.ByID(id)
	if !ok {
		return fmt.Errorf("no key with id %q (see: cli-to-api key list)", id)
	}

	if action == "delete" {
		// Deleting drops the record entirely, so confirm unless stdin is not
		// a terminal (scripted use).
		w := wizard.NewWith(os.Stdin, os.Stdout)
		ok, err := w.Confirm(fmt.Sprintf("Permanently delete key %s (%s)?", shortID(k.ID), k.Name), false)
		if err != nil || !ok {
			return errors.New("cancelled")
		}
		if err := st.Delete(id); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", shortID(id))
	} else {
		if err := st.Revoke(id); err != nil {
			return err
		}
		fmt.Printf("revoked %s — it will be rejected immediately\n", shortID(id))
	}
	return st.Save()
}
