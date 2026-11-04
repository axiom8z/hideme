package main

import (
	"os"
	"syscall"
)

const enableVirtualTerminalProcessing = 0x0004

var procSetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// terminalSupportsColor asks the console to accept ANSI codes. Consoles older than
// Windows 10 refuse, and those get plain text.
func terminalSupportsColor(f *os.File) bool {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return false // not a console: a file or a pipe
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	if err := procSetConsoleMode.Find(); err != nil {
		return false
	}
	ok, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return ok != 0
}
