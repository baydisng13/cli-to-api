//go:build windows

package wizard

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows has no stty. The console mode API is reachable from the standard
// library through a lazy kernel32 binding, which keeps this package free of
// module dependencies.
const enableEchoInput = 0x0004 // ENABLE_ECHO_INPUT, wincon.h

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// disableEcho clears ENABLE_ECHO_INPUT on the console attached to stdin and
// returns the func that puts the previous mode back.
//
// Only the echo bit is touched: ENABLE_LINE_INPUT stays on so Enter still
// terminates the line for the bufio reader, and ENABLE_PROCESSED_INPUT stays
// on so Ctrl-C still aborts.
//
// GetConsoleMode fails when stdin is a pipe or a file rather than a console,
// which is the honest "cannot disable echo" answer the caller warns about.
func disableEcho() (restore func(), ok bool) {
	h := syscall.Handle(os.Stdin.Fd())

	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return func() {}, false
	}
	if r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode&^enableEchoInput)); r == 0 {
		return func() {}, false
	}
	return func() {
		procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	}, true
}
