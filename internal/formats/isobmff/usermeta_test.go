package isobmff

import (
	"math"
	"testing"
	"unicode/utf16"
)

func TestDataValue(t *testing.T) {
	be := func(dataType uint32, value []byte) []byte { return cat(be32(dataType), be32(0), value) }
	float32Bits := be32(math.Float32bits(1.5))
	float64Bits := be64(math.Float64bits(2.5))
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"too short", []byte{1, 2}, ""},
		{"UTF-8", be(1, []byte("Test")), "Test"},
		{"UTF-16", be(2, []byte{0, 'T', 0, 'e'}), "Te"},
		{"JPEG", be(13, make([]byte, 7)), "JPEG image, 7 bytes"},
		{"PNG", be(14, make([]byte, 7)), "PNG image, 7 bytes"},
		{"integer", be(21, []byte{1, 0}), "256"},
		{"integer too long", be(22, make([]byte, 9)), "9 bytes"},
		{"float32", be(23, float32Bits), "1.5"},
		{"float64", be(24, float64Bits), "2.5"},
		{"float32 of the wrong size", be(23, make([]byte, 3)), "3 bytes"},
		{"implicit text", be(0, []byte("Test")), "Test"},
		{"implicit binary", be(0, []byte{0xFF, 0xFE}), "2 bytes"},
		{"unknown type", be(99, make([]byte, 4)), "4 bytes"},
	}
	for _, c := range cases {
		if got := dataValue(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTextForms(t *testing.T) {
	var units []byte
	for _, u := range utf16.Encode([]rune("Test")) {
		units = append(units, be16(u)...)
	}
	cases := []struct {
		name, got, want string
	}{
		{"3GPP UTF-8", languageString(cat(make([]byte, 6), []byte("Test\x00"))), "Test"},
		{"3GPP UTF-16", languageString(cat(make([]byte, 6), []byte{0xFE, 0xFF}, units)), "Test"},
		{"3GPP too short", languageString([]byte{0}), ""},
		{"QuickTime text", internationalText(qtText("Test")), "Test"},
		{"QuickTime length past the end", internationalText([]byte{0xFF, 0xFF, 0, 0, 'T'}), "\xff\xff\x00\x00T"},
		{"QuickTime too short", internationalText([]byte("Te")), "Te"},
		{"Pascal name", pascalOrCString([]byte("\x04Test")), "Test"},
		{"C name", pascalOrCString([]byte("Test\x00")), "Test"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestMovieTime(t *testing.T) {
	if got := movieTime(fakeTime); got != "2001-02-03 04:05:06 UTC" {
		t.Errorf("got %q", got)
	}
	// A value this large would overflow a time.Duration if it reached one.
	if got := movieTime(1 << 62); got != "4611686018427387904 seconds after 1904" {
		t.Errorf("got %q", got)
	}
}
