// Package exif reads TIFF-structured EXIF blocks and writes the only EXIF block
// HideMe ever keeps: a minimal one holding just the orientation tag.
//
// A handler that meets an EXIF block needs three calls, and running the same
// three in Inspect and Clean is what keeps those two in agreement:
//
//	d, _ := exif.Parse(exif.StripPrefix(block))
//	for _, f := range d.Findings("APP1/Exif", opts) {
//		report.Add(f)
//	}
//	keep := d.Replacement(opts) // nil means drop the block entirely
//
// The parse error may be ignored there because Parse always returns a usable
// Data and a damaged block reports itself through Findings. Every block is
// replaced or removed whole, so this package only has to read blocks, never to
// edit them, and a parser mistake can make a report less specific but cannot
// leave metadata behind.
package exif

import (
	"bytes"
	"encoding/binary"
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
)

// Prefix is the signature JPEG APP1 segments put in front of the TIFF data.
const Prefix = "Exif\x00\x00"

// StripPrefix returns block without a leading Prefix. Other containers, such as
// PNG eXIf and WebP EXIF chunks, store the TIFF data with no prefix at all, and
// their blocks pass through unchanged.
func StripPrefix(block []byte) []byte {
	return bytes.TrimPrefix(block, []byte(Prefix))
}

const (
	headerSize = 8
	entrySize  = 12
	tiffMagic  = 42

	// These caps bound the work a hostile block can demand. Real files stay far
	// below them: a phone photo has about five IFDs and a hundred entries. A
	// block over a cap fails to parse, which only makes its report less specific.
	maxIFDs          = 16
	maxEntriesPerIFD = 4096
	maxTotalEntries  = 16384
)

// Tags that shape the directory structure rather than describe the image.
const (
	tagOrientation = 0x0112
	tagExifIFD     = 0x8769
	tagGPSIFD      = 0x8825
	tagInteropIFD  = 0xA005
)

// TIFF field types.
const (
	typeByte      = 1
	typeASCII     = 2
	typeShort     = 3
	typeLong      = 4
	typeRational  = 5
	typeSByte     = 6
	typeUndefined = 7
	typeSShort    = 8
	typeSLong     = 9
	typeSRational = 10
	typeFloat     = 11
	typeDouble    = 12
	typeIFD       = 13
)

// TypeSize returns the size of one element, or 0 for a type TIFF does not
// define, whose value therefore cannot be located.
func TypeSize(typ uint16) uint64 {
	switch typ {
	case typeByte, typeASCII, typeSByte, typeUndefined:
		return 1
	case typeShort, typeSShort:
		return 2
	case typeLong, typeSLong, typeFloat, typeIFD:
		return 4
	case typeRational, typeSRational, typeDouble:
		return 8
	}
	return 0
}

// ifdKind says which tag namespace a directory uses. GPS and Interop tags reuse
// the low numbers, so the same tag means different things in different kinds.
type ifdKind int

const (
	kindRoot    ifdKind = iota // IFD0, the image's own directory
	kindExif                   // the Exif sub-IFD; shares the root's tag numbers
	kindChained                // IFD1 and later; shares the root's tag numbers
	kindGPS
	kindInterop
)

type entry struct {
	tag   uint16
	typ   uint16
	count uint32
	value []byte // nil if the type is unknown or the value overruns the block
}

type directory struct {
	name    string // the last part of Finding.Where, such as "GPS"
	kind    ifdKind
	entries []entry
}

// find returns the first entry with the given tag. Later duplicates are invalid
// TIFF and end up in the catch-all count, so a hostile block cannot flood the
// report by repeating a tag.
func (dir *directory) find(tag uint16) (entry, bool) {
	for _, e := range dir.entries {
		if e.tag == tag {
			return e, true
		}
	}
	return entry{}, false
}

// Data is a parsed EXIF block. It keeps references into the slice given to
// Parse, which must stay unmodified while Data is in use.
type Data struct {
	order   binary.ByteOrder
	size    int
	minimal bool
	err     error
	dirs    []directory
}

// Parse reads a raw TIFF block, following IFD0, the Exif, GPS and Interop
// sub-IFDs, and IFD1 and any IFDs chained after it.
//
// The returned Data is never nil. If the block is damaged, Parse returns an
// error wrapping core.ErrMalformed along with a Data holding whatever could
// still be decoded, so a block with one bad MakerNote can still report its GPS
// coordinates. Such a Data reports the damage through Findings. The only thing it
// can offer to keep is the orientation, which Replacement writes afresh.
func Parse(data []byte) (*Data, error) {
	d := &Data{size: len(data), minimal: IsMinimal(data)}
	first, err := d.readHeader(data)
	if err != nil {
		d.err = err
		return d, err
	}
	w := walker{data: data, d: d, seen: make(map[uint32]bool)}
	for i, off := 0, first; off != 0; i++ {
		kind := kindChained
		if i == 0 {
			kind = kindRoot
		}
		off = w.readIFD(off, "IFD"+strconv.Itoa(i), kind)
	}
	return d, d.err
}

func (d *Data) readHeader(data []byte) (uint32, error) {
	if len(data) < headerSize {
		return 0, core.Malformedf("exif: %d bytes is too short for a TIFF header", len(data))
	}
	switch string(data[:2]) {
	case "II":
		d.order = binary.LittleEndian
	case "MM":
		d.order = binary.BigEndian
	default:
		return 0, core.Malformedf("exif: the byte order mark is not II or MM")
	}
	if magic := d.order.Uint16(data[2:]); magic != tiffMagic {
		return 0, core.Malformedf("exif: TIFF magic number is %d, not %d", magic, tiffMagic)
	}
	first := d.order.Uint32(data[4:])
	if first < headerSize {
		return 0, core.Malformedf("exif: IFD0 offset %d points into the header", first)
	}
	return first, nil
}

// walker holds the state of one Parse. It keeps going after damage so that the
// report stays as specific as it can be, remembering only the first error.
type walker struct {
	data  []byte
	d     *Data
	seen  map[uint32]bool
	total int
}

func (w *walker) fail(format string, args ...any) {
	if w.d.err == nil {
		w.d.err = core.Malformedf("exif: "+format, args...)
	}
}

// bytesAt returns n bytes at off. Both values are uint64 and off comes from a
// uint32 while n is at most a uint32 times eight, so neither the comparison nor
// the slice bounds can overflow.
func (w *walker) bytesAt(off, n uint64) ([]byte, bool) {
	size := uint64(len(w.data))
	if off > size || n > size-off {
		return nil, false
	}
	return w.data[off : off+n], true
}

// readIFD reads the directory at off and everything linked from it, and returns
// the offset of the next directory in the chain, or 0 when there is none.
func (w *walker) readIFD(off uint32, name string, kind ifdKind) uint32 {
	switch {
	case off < headerSize:
		w.fail("%s offset %d points into the header", name, off)
		return 0
	case w.seen[off]:
		w.fail("%s at offset %d was already visited, so the directories form a cycle", name, off)
		return 0
	case len(w.seen) == maxIFDs:
		w.fail("more than %d IFDs", maxIFDs)
		return 0
	}
	w.seen[off] = true

	raw, ok := w.bytesAt(uint64(off), 2)
	if !ok {
		w.fail("%s offset %d is past the end of the %d-byte block", name, off, len(w.data))
		return 0
	}
	count := int(w.d.order.Uint16(raw))
	if count > maxEntriesPerIFD {
		w.fail("%s has %d entries, over the limit of %d", name, count, maxEntriesPerIFD)
		return 0
	}
	if w.total+count > maxTotalEntries {
		w.fail("more than %d entries in total", maxTotalEntries)
		return 0
	}
	w.total += count

	tableStart := uint64(off) + 2
	available := (uint64(len(w.data)) - tableStart) / entrySize
	if uint64(count) > available {
		w.fail("%s entry table overruns the block: %d entries declared, %d fit", name, count, available)
		count = int(available)
	}
	entries := make([]entry, count)
	for i := range entries {
		raw := w.data[tableStart+uint64(i)*entrySize:][:entrySize]
		entries[i] = w.readEntry(raw, name)
	}
	w.d.dirs = append(w.d.dirs, directory{name: name, kind: kind, entries: entries})

	if kind == kindRoot || kind == kindExif || kind == kindChained {
		w.followSubIFDs(entries, name)
	}
	if kind != kindRoot && kind != kindChained {
		// A sub-IFD has no business chaining onward and writers often leave
		// garbage in that field, so reading it would invent damage.
		return 0
	}
	// A missing next pointer at the very end of a block is a common writer
	// quirk that hides nothing, so it is the same as an explicit end of chain.
	next, ok := w.bytesAt(tableStart+uint64(count)*entrySize, 4)
	if !ok {
		return 0
	}
	return w.d.order.Uint32(next)
}

func (w *walker) readEntry(raw []byte, ifdName string) entry {
	order := w.d.order
	e := entry{tag: order.Uint16(raw), typ: order.Uint16(raw[2:]), count: order.Uint32(raw[4:])}
	size := TypeSize(e.typ)
	if size == 0 {
		return e
	}
	total := uint64(e.count) * size
	if total <= 4 {
		e.value = raw[8 : 8+total]
		return e
	}
	off := order.Uint32(raw[8:])
	value, ok := w.bytesAt(uint64(off), total)
	if !ok {
		w.fail("%s tag %#04x: %d-byte value at offset %d overruns the %d-byte block", ifdName, e.tag, total, off, len(w.data))
		return e
	}
	e.value = value
	return e
}

func (w *walker) followSubIFDs(entries []entry, parent string) {
	for _, e := range entries {
		var name string
		var kind ifdKind
		switch e.tag {
		case tagExifIFD:
			name, kind = "Exif", kindExif
		case tagGPSIFD:
			name, kind = "GPS", kindGPS
		case tagInteropIFD:
			name, kind = "Interop", kindInterop
		default:
			continue
		}
		off, ok := e.number(w.d.order)
		if !ok {
			w.fail("%s tag %#04x is not a valid pointer to the %s IFD", parent, e.tag, name)
			continue
		}
		// A zero pointer is how some writers say a sub-IFD is absent.
		if off != 0 {
			w.readIFD(off, name, kind)
		}
	}
}
