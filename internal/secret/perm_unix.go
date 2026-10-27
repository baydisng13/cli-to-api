//go:build !windows

package secret

import "os"

// restrictFile makes the data key readable by its owner only. OpenFile was
// already given 0600, but the umask can only clear bits and a pre-existing
// file would keep its own mode, so the intent is stated explicitly here - the
// same way store.go chmods its temp file.
func restrictFile(f *os.File) error { return f.Chmod(0o600) }
