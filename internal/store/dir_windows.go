//go:build windows

package store

import (
	"fmt"
	"os"
)

// configBase returns %AppData% (os.UserConfigDir), the conventional home for
// per-user application state on Windows. There is no XDG convention to honour
// here; CLI_TO_API_HOME remains the override, as on every platform.
func configBase() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("store: locate config directory: %w", err)
	}
	return d, nil
}
