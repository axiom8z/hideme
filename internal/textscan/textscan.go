// Package textscan looks for readable text in binary data that a handler has to keep,
// such as codec headers, so that the user can be told it stays.
package textscan

import "strings"

const (
	// minRun is how many text characters in a row count as readable text. Any fewer
	// turn up by chance in binary data. It means a short name can go unnoticed, which
	// the limitations of the handlers using this say.
	minRun = 12
	// minVariety rules out the long runs of one or two values that binary data is full
	// of: a Vorbis setup header, for one, holds dozens of U in a row.
	minVariety = 5
)

// Overlap is how much consecutive reads of a long region must share for Has to see a
// run that crosses from one to the next, in either encoding.
const Overlap = 2 * minRun

// Has looks for a run of text in single bytes or in UTF-16 of either byte order, which
// differ only by where the zero bytes fall.
func Has(b []byte) bool {
	return hasRun(b, 1) || hasRun(b, 2) || (len(b) > 0 && hasRun(b[1:], 2))
}

// isText is deliberately narrower than "printable", which would match too much binary
// data: letters, digits, and what separates words, paths and addresses.
func isText(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(" ._-:/\\@,'", c) >= 0
}

// hasRun walks b in steps of one or two bytes, and in the second case requires the byte
// after each character to be zero.
func hasRun(b []byte, step int) bool {
	run := 0
	for i := 0; i+step <= len(b); i += step {
		if !isText(b[i]) || (step == 2 && b[i+1] != 0) {
			run = 0
			continue
		}
		run++
		if run >= minRun && varied(b, i, step) {
			return true
		}
	}
	return false
}

// varied looks back over the last minRun characters.
func varied(b []byte, end, step int) bool {
	var seen [256]bool
	distinct := 0
	for n := range minRun {
		c := b[end-n*step]
		if !seen[c] {
			seen[c] = true
			distinct++
		}
	}
	return distinct >= minVariety
}
