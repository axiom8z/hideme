package main

// isSeparator knows both separators Windows accepts. Neither can occur in a file
// name, so splitting on them cannot hide part of a name from the sanitiser.
func isSeparator(c byte) bool { return c == '/' || c == '\\' }
