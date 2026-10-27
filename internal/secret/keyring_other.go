//go:build !darwin

package secret

// This build has no OS keystore backend, so the data key always lives in the
// 0600 key file next to the config. The choice is made here, at compile time,
// rather than by letting a keystore call fail at runtime: which store holds
// the key decides whether yesterday's ciphertext is readable today, and that
// must not depend on whether a daemon happened to answer.
//
// Linux: libsecret (secret-tool) would be the obvious backend, but its
// availability is not stable for one machine - it answers under a desktop
// session and fails over ssh or in CI, on the same box with the same config
// dir. A key written through it would then be unreadable from the sessions
// this tool is most often driven from, so the file is used unconditionally.
//
// Windows: DPAPI (CryptProtectData) is the proper answer and would bind the
// key to the user account, but it needs more than a GetConsoleMode-sized
// kernel32 binding to do safely. Until then, the file is the behaviour.

// platformKey reports that no OS keystore is available on this platform.
func platformKey() ([]byte, error) { return nil, errNoKeyring }
