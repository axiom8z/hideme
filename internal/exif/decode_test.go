package exif

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeText(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"NUL-terminated", []byte("Test\x00"), "Test"},
		{"stops at the first NUL", []byte("Test\x00hidden"), "Test"},
		{"no terminator", []byte("Test"), "Test"},
		{"empty", nil, ""},
		{"only a terminator", []byte{0}, ""},
		{"trims padding", []byte("  Test   \x00"), "Test"},
		{"only spaces", []byte("      \x00"), ""},
		{"UTF-8 stays UTF-8", []byte("M\xC3\xBCller\x00"), "Müller"},
		{"Latin-1 copyright sign", []byte("\xA9 Test\x00"), "© Test"},
		{"Latin-1 letters", []byte("M\xFCller"), "Müller"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeText(tt.in); got != tt.want {
				t.Errorf("decodeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeTextIsBounded(t *testing.T) {
	got := decodeText(bytes.Repeat([]byte("a"), 10*maxTextBytes))
	if len(got) != maxTextBytes {
		t.Errorf("decoded %d bytes, want %d", len(got), maxTextBytes)
	}
}

// TestLongUTF8IsNotMistakenForLatin1 is the reason clipUTF8 exists: cutting a
// valid value through the middle of a character must not garble the whole value.
func TestLongUTF8IsNotMistakenForLatin1(t *testing.T) {
	for _, width := range []string{"é", "東", "😀"} {
		for pad := range 4 {
			in := strings.Repeat("a", pad) + strings.Repeat(width, maxTextBytes)
			got := decodeText([]byte(in))
			if !strings.HasPrefix(got, strings.Repeat("a", pad)+width) {
				t.Errorf("%q with %d leading bytes decoded to %q...", width, pad, got[:min(len(got), 16)])
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("%q with %d leading bytes did not stay clean UTF-8", width, pad)
			}
		}
	}
}

func TestClipUTF8(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"shorter than the limit", "abc", 10, "abc"},
		{"exactly the limit", "abc", 3, "abc"},
		{"plain cut", "abcd", 3, "abc"},
		{"does not split a two-byte letter", "aé", 2, "a"},
		{"keeps a whole two-byte letter", "aéb", 3, "aé"},
		{"does not split a four-byte letter", "a😀", 3, "a"},
		{"does not split it at the next byte", "a😀", 4, "a"},
		{"keeps a whole four-byte letter", "a😀b", 5, "a😀"},
		{"Latin-1 bytes are cut plainly", "a\xFF\xFF\xFF", 2, "a\xFF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(clipUTF8([]byte(tt.in), tt.limit)); got != tt.want {
				t.Errorf("clipUTF8(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}

func TestDecodeUTF16(t *testing.T) {
	le, be := binary.ByteOrder(binary.LittleEndian), binary.ByteOrder(binary.BigEndian)
	tests := []struct {
		name  string
		in    []byte
		order binary.ByteOrder
		want  string
	}{
		{"little-endian", utf16Bytes(le, "Test"), le, "Test"},
		{"big-endian", utf16Bytes(be, "Test"), be, "Test"},
		{"terminator stops it", append(utf16Bytes(le, "Test"), 0, 0, 'x', 0), le, "Test"},
		{"little-endian mark overrides the default", append([]byte{0xFF, 0xFE}, utf16Bytes(le, "Test")...), be, "Test"},
		{"big-endian mark overrides the default", append([]byte{0xFE, 0xFF}, utf16Bytes(be, "Test")...), le, "Test"},
		{"non-Latin text", utf16Bytes(le, "Müller 東京"), le, "Müller 東京"},
		{"surrogate pair", utf16Bytes(le, "a😀b"), le, "a😀b"},
		{"unpaired surrogate is replaced", []byte{0x00, 0xD8, 'x', 0x00}, le, "�x"},
		{"odd trailing byte is ignored", append(utf16Bytes(le, "Test"), 'x'), le, "Test"},
		{"padding is trimmed", utf16Bytes(le, "  Test  "), le, "Test"},
		{"empty", nil, le, ""},
		{"one byte", []byte{'x'}, le, ""},
		{"only a mark", []byte{0xFF, 0xFE}, le, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeUTF16(tt.in, tt.order); got != tt.want {
				t.Errorf("decodeUTF16(%x) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeUTF16IsBounded(t *testing.T) {
	got := decodeUTF16(bytes.Repeat([]byte{'a', 0}, 10*maxTextBytes), binary.LittleEndian)
	if len(got) != maxTextBytes/2 {
		t.Errorf("decoded %d characters, want %d", len(got), maxTextBytes/2)
	}
}

func TestXPTagsAreLittleEndianInAnyBlock(t *testing.T) {
	e := entry{typ: typeByte, value: utf16Bytes(binary.LittleEndian, "Test")}
	for _, order := range bothOrders {
		if got := utf16LEValue(e, order); got != "Test" {
			t.Errorf("%v: %q", order, got)
		}
	}
}

func TestUserComment(t *testing.T) {
	le, be := binary.ByteOrder(binary.LittleEndian), binary.ByteOrder(binary.BigEndian)
	tests := []struct {
		name  string
		in    []byte
		order binary.ByteOrder
		want  string
	}{
		{"ASCII", []byte("ASCII\x00\x00\x00Test"), le, "Test"},
		{"undefined code holds text", []byte("\x00\x00\x00\x00\x00\x00\x00\x00Test"), le, "Test"},
		{"Unicode in a little-endian block", append([]byte("UNICODE\x00"), utf16Bytes(le, "Test")...), le, "Test"},
		{"Unicode in a big-endian block", append([]byte("UNICODE\x00"), utf16Bytes(be, "Test")...), be, "Test"},
		{"Unicode with its own mark", append([]byte("UNICODE\x00\xFF\xFE"), utf16Bytes(le, "Test")...), be, "Test"},
		{"JIS is named, not shown as noise", []byte("JIS\x00\x00\x00\x00\x00\x1b$B$\"\x1b(B"), le, "JIS-encoded text, 8 bytes"},
		{"writer that ignored the code field", []byte("A longer plain comment"), le, "A longer plain comment"},
		{"shorter than the code field", []byte("short"), le, "short"},
		{"only the code field", []byte("ASCII\x00\x00\x00"), le, ""},
		{"empty", nil, le, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := entry{typ: typeUndefined, value: tt.in}
			if got := userCommentValue(e, tt.order); got != tt.want {
				t.Errorf("userCommentValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEntrySizes(t *testing.T) {
	tests := []struct {
		typ  uint16
		size uint64
	}{
		{typeByte, 1}, {typeASCII, 1}, {typeShort, 2}, {typeLong, 4}, {typeRational, 8},
		{typeSByte, 1}, {typeUndefined, 1}, {typeSShort, 2}, {typeSLong, 4}, {typeSRational, 8},
		{typeFloat, 4}, {typeDouble, 8}, {typeIFD, 4},
		{0, 0}, {14, 0}, {16, 0}, {0xFFFF, 0},
	}
	for _, tt := range tests {
		if got := TypeSize(tt.typ); got != tt.size {
			t.Errorf("TypeSize(%d) = %d, want %d", tt.typ, got, tt.size)
		}
	}
	// A count this large overflows a 32-bit multiplication for every sized type.
	if got := (entry{typ: typeDouble, count: 0xFFFFFFFF}).byteSize(); got != 8*0xFFFFFFFF {
		t.Errorf("byteSize wrapped: %d", got)
	}
	if got := (entry{typ: 99, count: 7}).byteSize(); got != 7 {
		t.Errorf("byteSize of an unknown type = %d", got)
	}
}

func TestRationalBounds(t *testing.T) {
	order := binary.BigEndian
	raw := append(order.AppendUint32(nil, 1), order.AppendUint32(nil, 2)...)
	e := entry{typ: typeRational, value: raw}
	if v, ok := e.rational(0, order); !ok || v != 0.5 {
		t.Errorf("rational(0) = %v, %v", v, ok)
	}
	if _, ok := e.rational(1, order); ok {
		t.Error("rational(1) read past the end")
	}
	if _, ok := (entry{typ: typeRational}).rational(0, order); ok {
		t.Error("rational on a nil value succeeded")
	}
	if _, ok := (entry{typ: typeLong, value: raw}).rational(0, order); ok {
		t.Error("rational on a LONG succeeded")
	}
	signed := append(order.AppendUint32(nil, 0xFFFFFFFF), order.AppendUint32(nil, 2)...)
	if v, ok := (entry{typ: typeSRational, value: signed}).rational(0, order); !ok || v != -0.5 {
		t.Errorf("signed rational = %v, %v", v, ok)
	}
	if v, ok := (entry{typ: typeRational, value: signed}).rational(0, order); !ok || v != float64(0xFFFFFFFF)/2 {
		t.Errorf("unsigned rational = %v, %v", v, ok)
	}
	zero := append(order.AppendUint32(nil, 5), order.AppendUint32(nil, 0)...)
	for _, typ := range []uint16{typeRational, typeSRational} {
		if _, ok := (entry{typ: typ, value: zero}).rational(0, order); ok {
			t.Errorf("type %d: a zero denominator decoded", typ)
		}
	}
}

func TestNumberRejectsUnusableEntries(t *testing.T) {
	order := binary.LittleEndian
	tests := []struct {
		name string
		e    entry
		want uint32
		ok   bool
	}{
		{"SHORT", entry{typ: typeShort, count: 1, value: []byte{6, 0}}, 6, true},
		{"LONG", entry{typ: typeLong, count: 1, value: []byte{6, 0, 0, 0}}, 6, true},
		{"IFD", entry{typ: typeIFD, count: 1, value: []byte{6, 0, 0, 0}}, 6, true},
		{"count of two", entry{typ: typeShort, count: 2, value: []byte{6, 0, 6, 0}}, 0, false},
		{"count of zero", entry{typ: typeShort, count: 0, value: []byte{}}, 0, false},
		{"nil value", entry{typ: typeLong, count: 1}, 0, false},
		{"BYTE", entry{typ: typeByte, count: 1, value: []byte{6}}, 0, false},
		{"rational", entry{typ: typeRational, count: 1, value: make([]byte, 8)}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.e.number(order)
			if got != tt.want || ok != tt.ok {
				t.Errorf("number() = %d, %v, want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
