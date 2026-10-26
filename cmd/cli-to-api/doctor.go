package main

import (
	"context"
	"fmt"
	"time"

	"cli-to-api/internal/adapter"
)

// cmdDoctor reports which backends are present and whether they actually
// answer, which is the first question when a key stops working.
func cmdDoctor(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("cli-to-api %s\n\n", version)

	if st, err := openStore(); err == nil {
		fmt.Printf("config: %s\n", st.Path())
		active, revoked := 0, 0
		for _, k := range st.List() {
			if k.Revoked {
				revoked++
			} else {
				active++
			}
		}
		fmt.Printf("keys:   %d active, %d revoked\n\n", active, revoked)
	} else {
		fmt.Printf("config: unavailable (%v)\n\n", err)
	}

	for _, a := range adapter.All() {
		path, ok := a.Detect(nil)
		switch {
		case !ok:
			fmt.Printf("  %s  %s\n", pad(a.ID(), 10), "not installed or not configured")
			continue
		default:
			fmt.Printf("  %s  %s", pad(a.ID(), 10), path)
		}
		// Listing models proves the backend runs, not just that a file exists.
		models, err := a.Models(ctx, nil)
		if err != nil {
			fmt.Printf("  — models unavailable: %v\n", err)
			continue
		}
		fmt.Printf("  — %d model(s)", len(models))
		if len(models) > 0 {
			fmt.Printf(": %s", models[0].ID)
			if len(models) > 1 {
				fmt.Printf(", …")
			}
		}
		fmt.Println()
	}
	return nil
}
