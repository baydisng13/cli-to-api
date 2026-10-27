//go:build !windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// configBase returns the directory our config directory is created inside.
//
// os.UserConfigDir() is not used here on purpose: on darwin it returns
// ~/Library/Application Support and ignores XDG_CONFIG_HOME, which would both
// move every existing install's config and make the layout differ between
// darwin and linux for no benefit. The XDG convention is what this tool has
// always used on unix, so it stays.
func configBase() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}
