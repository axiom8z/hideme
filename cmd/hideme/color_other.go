//go:build !windows

package main

import "os"

func terminalSupportsColor(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return os.Getenv("TERM") != "dumb"
}
