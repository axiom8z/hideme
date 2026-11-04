package pdf

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// pdfDocEncoding holds the codes where PDFDocEncoding differs from Latin-1.
var pdfDocEncoding = map[byte]rune{
	0x18: '˘', 0x19: 'ˇ', 0x1A: 'ˆ', 0x1B: '˙',
	0x1C: '˝', 0x1D: '˛', 0x1E: '˚', 0x1F: '˜',
	0x80: '•', 0x81: '†', 0x82: '‡', 0x83: '…',
	0x84: '—', 0x85: '–', 0x86: 'ƒ', 0x87: '⁄',
	0x88: '‹', 0x89: '›', 0x8A: '−', 0x8B: '‰',
	0x8C: '„', 0x8D: '“', 0x8E: '”', 0x8F: '‘',
	0x90: '’', 0x91: '‚', 0x92: '™', 0x93: 'ﬁ',
	0x94: 'ﬂ', 0x95: 'Ł', 0x96: 'Œ', 0x97: 'Š',
	0x98: 'Ÿ', 0x99: 'Ž', 0x9A: 'ı', 0x9B: 'ł',
	0x9C: 'œ', 0x9D: 'š', 0x9E: 'ž', 0xA0: '€',
}

// decodeText turns a PDF text string into UTF-8. The byte order mark picks the
// encoding. FF FE is not in the specification, but some writers produce it.
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:])
	}
	var out strings.Builder
	for _, c := range b {
		if r, ok := pdfDocEncoding[c]; ok {
			out.WriteRune(r)
		} else {
			out.WriteRune(rune(c))
		}
	}
	return out.String()
}

// decodeUTF16 also drops language tags, which UTF-16 text strings mark by putting
// them between two U+001B escapes.
func decodeUTF16(b []byte, order binary.ByteOrder) string {
	units := make([]uint16, 0, len(b)/2)
	inTag := false
	for i := 0; i+1 < len(b); i += 2 {
		u := order.Uint16(b[i:])
		if u == 0x001B {
			inTag = !inTag
			continue
		}
		if !inTag {
			units = append(units, u)
		}
	}
	return string(utf16.Decode(units))
}

// formatDate renders a PDF date such as D:20010203040506+01'00' for a report. A
// value that does not fit that shape is returned as it stands, since it is still
// worth showing.
func formatDate(s string) string {
	t := strings.TrimPrefix(s, "D:")
	digits := 0
	for digits < len(t) && isDigit(t[digits]) {
		digits++
	}
	if digits < 4 || digits > 14 || digits%2 != 0 {
		return s
	}
	var out strings.Builder
	separators := []string{"", "-", "-", " ", ":", ":"}
	out.WriteString(t[:4])
	for i, pos := 1, 4; pos < digits; i, pos = i+1, pos+2 {
		out.WriteString(separators[i])
		out.WriteString(t[pos : pos+2])
	}
	zone := strings.ReplaceAll(t[digits:], "'", ":")
	zone = strings.TrimSuffix(zone, ":")
	if zone != "" {
		out.WriteString(" ")
		out.WriteString(zone)
	}
	return out.String()
}
