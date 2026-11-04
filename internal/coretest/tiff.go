package coretest

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// TIFF type codes, for Entry.Type.
const (
	TypeByte      uint16 = 1
	TypeASCII     uint16 = 2
	TypeShort     uint16 = 3
	TypeLong      uint16 = 4
	TypeRational  uint16 = 5
	TypeUndefined uint16 = 7
	TypeSRational uint16 = 10
)

// Tags the encoder writes itself to link an IFD to its children.
const (
	tagJPEGThumbnail       = 0x0201
	tagJPEGThumbnailLength = 0x0202
	tagExifIFD             = 0x8769
	tagGPSIFD              = 0x8825
	tagInteropIFD          = 0xA005
)

// Entry is one tag in an IFD.
type Entry struct {
	Tag uint16
	// Type is the TIFF type code. Zero derives it from the Go type of Value.
	Type uint16
	// Count overrides the element count, for building damaged blocks. Zero
	// derives it from Value.
	Count uint32
	// Value is a string (ASCII, NUL-terminated on encoding), []byte, uint16,
	// []uint16, uint32, []uint32 or [][2]uint32 (numerator, denominator).
	Value any
}

// IFD describes a TIFF directory and everything linked from it, so tests can
// build EXIF blocks and whole TIFF files without hand-computing offsets.
type IFD struct {
	Entries []Entry
	// Exif, GPS and Interop become sub-IFDs linked through their pointer tags.
	Exif, GPS, Interop *IFD
	// Thumbnail is stored as a JPEG thumbnail with its offset and length tags.
	Thumbnail []byte
	// Next is the following IFD in the chain, IFD1 when this one is IFD0.
	Next *IFD
}

// Encode serialises the directory tree as a complete TIFF block. It panics on
// a Value of an unsupported Go type, which is always a mistake in the test.
func (ifd *IFD) Encode(order binary.ByteOrder) []byte {
	e := tiffEncoder{order: order}
	if order == binary.ByteOrder(binary.BigEndian) {
		e.buf = []byte{'M', 'M', 0, 42, 0, 0, 0, 8}
	} else {
		e.buf = []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	}
	e.writeIFD(ifd)
	return e.buf
}

type tiffEncoder struct {
	order binary.ByteOrder
	buf   []byte
}

// planned is an entry during encoding, with the child or data that is placed
// after the table and patched in through the entry's value slot.
type planned struct {
	Entry
	child     *IFD
	thumbnail []byte
}

func (ifd *IFD) plan() []planned {
	entries := make([]planned, 0, len(ifd.Entries)+5)
	for _, entry := range ifd.Entries {
		entries = append(entries, planned{Entry: entry})
	}
	link := func(tag uint16, child *IFD) {
		if child != nil {
			entries = append(entries, planned{Entry: Entry{Tag: tag, Value: uint32(0)}, child: child})
		}
	}
	link(tagExifIFD, ifd.Exif)
	link(tagGPSIFD, ifd.GPS)
	link(tagInteropIFD, ifd.Interop)
	if ifd.Thumbnail != nil {
		entries = append(entries,
			planned{Entry: Entry{Tag: tagJPEGThumbnail, Value: uint32(0)}, thumbnail: ifd.Thumbnail},
			planned{Entry: Entry{Tag: tagJPEGThumbnailLength, Value: uint32(len(ifd.Thumbnail))}},
		)
	}
	// TIFF requires ascending tags, and independent readers may rely on it.
	slices.SortStableFunc(entries, func(a, b planned) int { return int(a.Tag) - int(b.Tag) })
	return entries
}

// writeIFD appends the directory, its values and its children, returning the
// directory's offset. It addresses e.buf by index because appending moves it.
func (e *tiffEncoder) writeIFD(ifd *IFD) uint32 {
	e.alignEven()
	entries := ifd.plan()
	start := len(e.buf)
	e.buf = append(e.buf, make([]byte, 2+12*len(entries)+4)...)
	e.order.PutUint16(e.buf[start:], uint16(len(entries)))

	slot := func(i int) int { return start + 2 + 12*i }
	for i, entry := range entries {
		e.writeEntry(slot(i), entry.Entry)
	}
	for i, entry := range entries {
		switch {
		case entry.child != nil:
			off := e.writeIFD(entry.child)
			e.order.PutUint32(e.buf[slot(i)+8:], off)
		case entry.thumbnail != nil:
			e.alignEven()
			e.order.PutUint32(e.buf[slot(i)+8:], uint32(len(e.buf)))
			e.buf = append(e.buf, entry.thumbnail...)
		}
	}
	if ifd.Next != nil {
		off := e.writeIFD(ifd.Next)
		e.order.PutUint32(e.buf[slot(len(entries)):], off)
	}
	return uint32(start)
}

func (e *tiffEncoder) writeEntry(at int, entry Entry) {
	typ, count, value := encodeValue(e.order, entry.Value)
	if entry.Type != 0 {
		typ = entry.Type
	}
	if entry.Count != 0 {
		count = entry.Count
	}
	e.order.PutUint16(e.buf[at:], entry.Tag)
	e.order.PutUint16(e.buf[at+2:], typ)
	e.order.PutUint32(e.buf[at+4:], count)
	if len(value) <= 4 {
		copy(e.buf[at+8:], value)
		return
	}
	e.alignEven()
	e.order.PutUint32(e.buf[at+8:], uint32(len(e.buf)))
	e.buf = append(e.buf, value...)
}

func (e *tiffEncoder) alignEven() {
	if len(e.buf)%2 == 1 {
		e.buf = append(e.buf, 0)
	}
}

func encodeValue(order binary.ByteOrder, value any) (typ uint16, count uint32, raw []byte) {
	switch v := value.(type) {
	case string:
		raw = append([]byte(v), 0)
		return TypeASCII, uint32(len(raw)), raw
	case []byte:
		return TypeUndefined, uint32(len(v)), v
	case uint16:
		return encodeValue(order, []uint16{v})
	case []uint16:
		raw = make([]byte, 2*len(v))
		for i, x := range v {
			order.PutUint16(raw[2*i:], x)
		}
		return TypeShort, uint32(len(v)), raw
	case uint32:
		return encodeValue(order, []uint32{v})
	case []uint32:
		raw = make([]byte, 4*len(v))
		for i, x := range v {
			order.PutUint32(raw[4*i:], x)
		}
		return TypeLong, uint32(len(v)), raw
	case [][2]uint32:
		raw = make([]byte, 8*len(v))
		for i, pair := range v {
			order.PutUint32(raw[8*i:], pair[0])
			order.PutUint32(raw[8*i+4:], pair[1])
		}
		return TypeRational, uint32(len(v)), raw
	}
	panic(fmt.Sprintf("coretest: unsupported TIFF value type %T", value))
}
