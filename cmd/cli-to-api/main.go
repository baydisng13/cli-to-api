// Command cli-to-api serves local agentic CLIs behind an OpenAI-compatible
// API, with per-key scoping to one backend and an explicit model allowlist.
package main

import (
	"fmt"
	"os"
	"strings"

	"cli-to-api/internal/store"

	// Adapters register themselves; this blank import is what puts them in
	// the registry.
	_ "cli-to-api/internal/adapter"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `cli-to-api — serve local agentic CLIs behind an OpenAI-compatible API

usage:
  cli-to-api start [--addr :8080]      run the server for every stored key
  cli-to-api key create                interactive wizard: pick a CLI, pick models, get a key
  cli-to-api key list                  show stored keys
  cli-to-api key revoke <id>           disable a key, keeping its record
  cli-to-api key delete <id>           remove a key permanently
  cli-to-api doctor                    report which backends are installed and answering
  cli-to-api version

config lives in $CLI_TO_API_HOME, else $XDG_CONFIG_HOME/cli-to-api, else ~/.config/cli-to-api
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "cli-to-api: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "start", "serve":
		return cmdStart(args[1:])
	case "key", "keys":
		return cmdKey(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

// openStore loads the config from the standard location, or from the path in
// CLI_TO_API_HOME.
func openStore() (*store.Store, error) {
	dir, err := store.DefaultDir()
	if err != nil {
		return nil, err
	}
	return store.Load(dir)
}

// shortID renders a key id for display next to its name.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
