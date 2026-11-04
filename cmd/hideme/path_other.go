//go:build !windows

package main

// isSeparator knows only the slash, since a backslash is an ordinary character in a
// file name here and is sanitised along with the rest of it.
func isSeparator(c byte) bool { return c == '/' }
