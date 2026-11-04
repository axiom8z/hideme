package textscan

import (
	"bytes"
	"testing"
)

func utf16(s string, low bool) []byte {
	var out []byte
	for _, c := range []byte(s) {
		if low {
			out = append(out, c, 0)
		} else {
			out = append(out, 0, c)
		}
	}
	return out
}

func TestHas(t *testing.T) {
	const text = "Test Author, C:/Users"
	binary := []byte{0xFF, 0x01, 0x80}
	for name, data := range map[string][]byte{
		"single bytes":        []byte(text),
		"inside binary":       bytes.Join([][]byte{binary, []byte(text), binary}, nil),
		"UTF-16 little":       utf16(text, true),
		"UTF-16 big":          utf16(text, false),
		"exactly long enough": []byte("Test Author "),
	} {
		if !Has(data) {
			t.Errorf("%s: not found", name)
		}
	}
	for name, data := range map[string][]byte{
		"nothing":           nil,
		"one byte":          {'a'},
		"binary":            bytes.Repeat(binary, 40),
		"one byte too few":  []byte("Test Author"),
		"long run of U":     bytes.Repeat([]byte{'U'}, 100),
		"two values":        bytes.Repeat([]byte("ab"), 50),
		"four values":       bytes.Repeat([]byte("abcd"), 25),
		"broken by binary":  []byte("Test Au\x01thor, C:/Us\x01ers"),
		"UTF-16 with holes": bytes.Repeat([]byte{'a', 1}, 50),
		"not word text":     bytes.Repeat([]byte("(){}[]!?"), 10),
	} {
		if Has(data) {
			t.Errorf("%s: found text", name)
		}
	}
}
