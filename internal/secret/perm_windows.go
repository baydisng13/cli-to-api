//go:build windows

package secret

import "os"

// restrictFile is a no-op on Windows, and this is the honest version of that.
//
// Go maps a file mode onto the read-only attribute and nothing else: 0600
// grants no protection here and os.Chmod would only create the impression of
// one. What actually protects the key is the ACL the file inherits from its
// parent, and %AppData% is per-user by default - so the common case is sound,
// but a key file placed somewhere world-readable through CLI_TO_API_HOME is
// world-readable, with no warning from this code.
//
// A real fix is DPAPI (CryptProtectData), which would make the file useless to
// anyone but the owning account regardless of its ACL.
func restrictFile(*os.File) error { return nil }
