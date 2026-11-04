package main

import (
	"io"
	"os"
)

// colorEnabled decides whether to use colour: only for a real terminal, only without
// NO_COLOR (see no-color.org), and only where the platform can show it. Anything else
// gets plain text, so that logs and pipes never contain escape codes.
func colorEnabled(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return terminalSupportsColor(f)
}
