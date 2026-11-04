package jpegmeta

import (
	"bytes"
	"testing"
)

func TestFrameMarkers(t *testing.T) {
	for marker := 0; marker < 256; marker++ {
		want := marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC
		if got := IsFrame(byte(marker)); got != want {
			t.Errorf("IsFrame(%#02x) = %t", marker, got)
		}
	}
}

func TestSegmentShapes(t *testing.T) {
	quant := append([]byte{0x01}, make([]byte, 64)...)
	wide := append([]byte{0x12}, make([]byte, 128)...)
	huffman := append(append([]byte{0x10}, make([]byte, 15)...), 2, 'a', 'b')
	cases := []struct {
		name  string
		valid bool
		got   bool
	}{
		{"one quant table", true, ValidTables(quant, QuantEntry)},
		{"two quant tables", true, ValidTables(append(bytes.Clone(quant), wide...), QuantEntry)},
		{"nothing", false, ValidTables(nil, QuantEntry)},
		{"quant table with a byte over", false, ValidTables(append(bytes.Clone(quant), 0), QuantEntry)},
		{"quant table cut short", false, ValidTables(quant[:64], QuantEntry)},
		{"quant precision that does not exist", false, ValidTables(append([]byte{0x21}, make([]byte, 64)...), QuantEntry)},
		{"quant id that does not exist", false, ValidTables(append([]byte{0x04}, make([]byte, 64)...), QuantEntry)},
		{"huffman table", true, ValidTables(huffman, HuffmanEntry)},
		{"huffman table with a byte over", false, ValidTables(append(bytes.Clone(huffman), 0), HuffmanEntry)},
		{"huffman symbols past the end", false, ValidTables(huffman[:len(huffman)-1], HuffmanEntry)},
		{"too many huffman symbols", false, ValidTables(append(append([]byte{0x00}, bytes.Repeat([]byte{0xFF}, 16)...), make([]byte, 4080)...), HuffmanEntry)},
		{"arithmetic DC bounds", true, ValidTables([]byte{0x00, 0x10}, ArithmeticEntry)},
		{"arithmetic DC bounds reversed", false, ValidTables([]byte{0x00, 0x01}, ArithmeticEntry)},
		{"arithmetic AC value", true, ValidTables([]byte{0x10, 63}, ArithmeticEntry)},
		{"arithmetic AC value out of range", false, ValidTables([]byte{0x10, 64}, ArithmeticEntry)},
		{"arithmetic entry cut short", false, ValidTables([]byte{0x00, 0x10, 0x00}, ArithmeticEntry)},
		{"restart interval", true, ValidTables([]byte{0, 8}, RestartEntry)},
		{"restart interval with a byte over", false, ValidTables([]byte{0, 8, 0}, RestartEntry)},
		{"frame", true, ValidFrame([]byte{8, 0, 1, 0, 1, 1, 1, 0x11, 0})},
		{"frame with a byte over", false, ValidFrame([]byte{8, 0, 1, 0, 1, 1, 1, 0x11, 0, 0})},
		{"frame with no components", false, ValidFrame([]byte{8, 0, 1, 0, 1, 0})},
		{"scan", true, ValidScan([]byte{1, 1, 0, 0, 63, 0})},
		{"scan with a byte over", false, ValidScan([]byte{1, 1, 0, 0, 63, 0, 0})},
		{"scan with too many components", false, ValidScan(append([]byte{5}, make([]byte, 13)...))},
		{"empty scan", false, ValidScan(nil)},
	}
	for _, c := range cases {
		if c.got != c.valid {
			t.Errorf("%s: got %t", c.name, c.got)
		}
	}
}

func TestNextResource(t *testing.T) {
	// A one-byte name fills two bytes, and an odd value gets a pad byte.
	block := []byte("8BIM\x04\x04\x01a\x00\x00\x00\x03xyz\x00rest")
	id, value, rest, ok := NextResource(block)
	if !ok || id != ResourceIPTC || string(value) != "xyz" || string(rest) != "rest" {
		t.Fatalf("got %#x %q %q %t", id, value, rest, ok)
	}
	for _, signature := range []string{"PHUT", "AgHg", "DCSR", "MeSa"} {
		if _, _, _, ok := NextResource(append([]byte(signature), block[4:]...)); !ok {
			t.Errorf("signature %s refused", signature)
		}
	}
	for name, bad := range map[string][]byte{
		"empty":           nil,
		"short":           []byte("8BIM\x04"),
		"other signature": []byte("XXXX\x04\x04\x00\x00\x00\x00\x00\x00"),
		"name overruns":   []byte("8BIM\x04\x04\xFFabcdef"),
		"size overruns":   []byte("8BIM\x04\x04\x00\x00\xFF\xFF\xFF\xFFabc"),
		"no size":         []byte("8BIM\x04\x04\x00\x00\x00"),
	} {
		if _, _, _, ok := NextResource(bad); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNextDataset(t *testing.T) {
	record, dataset, value, rest, ok := NextDataset([]byte("\x1C\x02\x50\x00\x03abcrest"))
	if !ok || record != IPTCApplicationRecord || dataset != 80 || string(value) != "abc" || string(rest) != "rest" {
		t.Fatalf("got %d %d %q %q %t", record, dataset, value, rest, ok)
	}
	// The long form: the size field says a two-byte size follows.
	_, _, value, rest, ok = NextDataset([]byte("\x1C\x02\x50\x80\x02\x00\x03abcrest"))
	if !ok || string(value) != "abc" || string(rest) != "rest" {
		t.Fatalf("long form: got %q %q %t", value, rest, ok)
	}
	for name, bad := range map[string][]byte{
		"empty":             nil,
		"short":             []byte("\x1C\x02\x50"),
		"wrong marker":      []byte("X\x02\x50\x00\x00"),
		"value overruns":    []byte("\x1C\x02\x50\x00\xC8abc"),
		"size field wide":   []byte("\x1C\x02\x50\x80\x09abcdefghi"),
		"size field cut":    []byte("\x1C\x02\x50\x80\x04ab"),
		"long form overrun": []byte("\x1C\x02\x50\x80\x02\xFF\xFFabc"),
	} {
		if _, _, _, _, ok := NextDataset(bad); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
