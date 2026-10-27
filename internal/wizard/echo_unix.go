//go:build !windows

package wizard

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// disableEcho silences the terminal through stty, which avoids a terminal
// library dependency. The returned func restores the previous mode.
//
// The device flag is not portable: GNU coreutils spells it -F, BSD (darwin,
// the *BSDs) spells it -f. Rather than trusting a GOOS table alone we try the
// likely one first and fall back to the other, so an unusual userland (busybox
// on linux, GNU stty on darwin) still gets a silent prompt instead of a
// visible one.
func disableEcho() (restore func(), ok bool) {
	if !isTerminal(os.Stdin) {
		return func() {}, false
	}
	for _, flag := range sttyDeviceFlags() {
		prev, err := exec.Command("stty", flag, "/dev/tty", "-g").Output()
		if err != nil {
			continue
		}
		if err := exec.Command("stty", flag, "/dev/tty", "-echo").Run(); err != nil {
			continue
		}
		saved := strings.TrimSpace(string(prev))
		return func() {
			_ = exec.Command("stty", flag, "/dev/tty", saved).Run()
		}, true
	}
	return func() {}, false
}

// sttyDeviceFlags lists the "operate on this device" flags to try, best guess
// first.
func sttyDeviceFlags() []string {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return []string{"-f", "-F"}
	default:
		return []string{"-F", "-f"}
	}
}
