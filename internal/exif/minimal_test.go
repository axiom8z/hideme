package exif

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

func TestMinimalLayout(t *testing.T) {
	want := []byte{
		'M', 'M', 0x00, 0x2A, 0x00, 0x00, 0x00, 0x08,
		0x00, 0x01,
		0x01, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01, 0x00, 0x06, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	if got := Minimal(6); !bytes.Equal(got, want) {
		t.Errorf("Minimal(6) = %x, want %x", got, want)
	}
	if len(want) != minimalSize {
		t.Errorf("minimalSize = %d, layout is %d bytes", minimalSize, len(want))
	}
}

func TestMinimalRejectsInvalidOrientations(t *testing.T) {
	for _, o := range []uint16{0, 9, 10, 0x0106, 0xFFFF} {
		if got := Minimal(o); got != nil {
			t.Errorf("Minimal(%d) = %x, want nil", o, got)
		}
	}
}

func TestMinimalRoundTrip(t *testing.T) {
	for o := uint16(1); o <= 8; o++ {
		block := Minimal(o)
		if !bytes.Equal(block, Minimal(o)) {
			t.Errorf("Minimal(%d) is not deterministic", o)
		}
		if !IsMinimal(block) {
			t.Errorf("IsMinimal(Minimal(%d)) = false", o)
		}
		if !IsMinimal(append([]byte(Prefix), block...)) {
			t.Errorf("IsMinimal with the JPEG prefix, orientation %d = false", o)
		}
		d := mustParse(t, block)
		if got := d.Orientation(); got != o {
			t.Errorf("Parse(Minimal(%d)).Orientation() = %d", o, got)
		}
		if len(d.dirs) != 1 || len(d.dirs[0].entries) != 1 {
			t.Errorf("Minimal(%d) parsed as %+v", o, d.dirs)
		}
		checkBlock(t, block)

		findings := d.Findings("W", core.Options{})
		if o == 1 {
			// An orientation of 1 is dropped, so a block holding it is not clean.
			if len(findings) != 1 || findings[0].Action != core.ActionRemove {
				t.Errorf("Minimal(1): findings = %+v", findings)
			}
			if d.Replacement(core.Options{}) != nil {
				t.Error("Minimal(1) offers a replacement")
			}
		} else {
			if len(findings) != 1 || findings[0].Field != "Orientation" || findings[0].Action != core.ActionRetain {
				t.Errorf("Minimal(%d): findings = %+v, want only the retained orientation", o, findings)
			}
			if got := d.Replacement(core.Options{}); !bytes.Equal(got, block) {
				t.Errorf("Minimal(%d) would not be kept byte for byte: %x", o, got)
			}
		}

		strict := d.Findings("W", core.Options{Strict: true})
		if len(strict) != 1 || strict[0].Field != "Orientation" || strict[0].Action != core.ActionRemove {
			t.Errorf("Minimal(%d) strict: findings = %+v", o, strict)
		}
		if d.Replacement(core.Options{Strict: true}) != nil {
			t.Errorf("Minimal(%d): strict mode offers a replacement", o)
		}
	}
}

// TestCleanOfFullBlockIsIdempotent walks the path a handler takes: a real block
// is replaced, and cleaning the result changes nothing and reports nothing else.
func TestCleanOfFullBlockIsIdempotent(t *testing.T) {
	for _, order := range bothOrders {
		for o := uint16(2); o <= 8; o++ {
			block := minimalInOrder(order, o)
			first := mustParse(t, block).Replacement(core.Options{})
			if !bytes.Equal(first, Minimal(o)) {
				t.Fatalf("%v: replacement = %x", order, first)
			}
			second := mustParse(t, first).Replacement(core.Options{})
			if !bytes.Equal(second, first) {
				t.Errorf("%v: second replacement = %x", order, second)
			}
		}
	}
}

// minimalInOrder holds only an orientation, like Minimal, in either byte order.
func minimalInOrder(order byteOrder, o uint16) []byte {
	value := uint32(o)
	if order.String() == binary.BigEndian.String() {
		value <<= 16
	}
	return appendIFD(rawHeader(order, headerSize), order, 0, rawEntry{tag: tagOrientation, typ: typeShort, count: 1, value: value})
}

func TestIsMinimalIsExact(t *testing.T) {
	block := Minimal(6)
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"only the prefix", []byte(Prefix)},
		{"one byte short", block[:len(block)-1]},
		{"one byte long", append(bytes.Clone(block), 0)},
		{"little-endian equivalent", minimalInOrder(binary.LittleEndian, 6)},
		{"two prefixes", append([]byte(Prefix+Prefix), block...)},
		{"orientation 0", withByte(block, minimalValueOffset, 0)},
		{"orientation 9", withByte(block, minimalValueOffset, 9)},
		{"orientation 255", withByte(block, minimalValueOffset, 255)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsMinimal(tt.in) {
				t.Errorf("IsMinimal(%x) = true", tt.in)
			}
		})
	}
	// Every other byte is structure, so changing any of them must be noticed: a
	// hidden byte anywhere is the exact thing this function exists to catch.
	for i := range block {
		if i == minimalValueOffset {
			continue
		}
		for _, value := range []byte{0x00, 0x01, 0xFF, block[i] ^ 0x80} {
			if value == block[i] {
				continue
			}
			if changed := withByte(block, i, value); IsMinimal(changed) {
				t.Errorf("byte %d changed to %#02x is still called minimal", i, value)
			}
		}
	}
}

func withByte(block []byte, i int, value byte) []byte {
	out := bytes.Clone(block)
	out[i] = value
	return out
}

func TestReplacementOfFullBlock(t *testing.T) {
	block := appendIFD(rawHeader(binary.LittleEndian, headerSize), binary.LittleEndian, 0,
		rawEntry{tag: tagOrientation, typ: typeShort, count: 1, value: 8})
	d := mustParse(t, block)
	if got := d.Orientation(); got != 8 {
		t.Fatalf("Orientation = %d", got)
	}
	if got := d.Replacement(core.Options{}); !bytes.Equal(got, Minimal(8)) {
		t.Errorf("Replacement = %x", got)
	}
}
