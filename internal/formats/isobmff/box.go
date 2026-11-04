package isobmff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

// box locates one box. Its offsets are absolute in the file for a top-level box,
// and relative to the enclosing image for a nested one.
type box struct {
	typ   string
	start int64 // first byte of the header
	body  int64 // first byte of the payload
	end   int64 // just past the payload
}

func (b box) size() int64 { return b.end - b.start }

// parseHeader reads the box header at the front of b. pos is where b starts, and
// avail is how many bytes the enclosing scope still holds from there, which must
// contain the whole box. A size field of zero means the box runs to the end of
// that scope. Offsets cannot overflow because avail never exceeds the file size.
func parseHeader(b []byte, pos, avail int64) (box, bool) {
	if len(b) < 8 || avail < 8 {
		return box{}, false
	}
	typ := string(b[4:8])
	switch size := binary.BigEndian.Uint32(b); size {
	case 0:
		return box{typ, pos, pos + 8, pos + avail}, true
	case 1:
		if len(b) < 16 {
			return box{}, false
		}
		large := binary.BigEndian.Uint64(b[8:])
		if large < 16 || large > uint64(avail) {
			return box{}, false
		}
		return box{typ, pos, pos + 16, pos + int64(large)}, true
	default:
		if size < 8 || int64(size) > avail {
			return box{}, false
		}
		return box{typ, pos, pos + 8, pos + int64(size)}, true
	}
}

// plausibleType reports whether a top-level type could be real: printable ASCII
// or the copyright sign QuickTime uses. Trailers appended by phones can have a
// first word that happens to pass as a size, and this keeps them from being
// taken for a box.
func plausibleType(typ string) bool {
	for i := range len(typ) {
		if c := typ[i]; (c < 0x20 || c > 0x7E) && c != 0xA9 {
			return false
		}
	}
	return true
}

// scanTop calls visit for each top-level box until visit returns false or the
// bytes stop forming boxes. It returns where it stopped, which is the file size
// if every byte belongs to a box.
func scanTop(src core.Source, visit func(box) bool) (int64, error) {
	var head [16]byte
	size := src.Size()
	pos := int64(0)
	for pos < size {
		avail := size - pos
		if avail < 8 {
			return pos, nil
		}
		n := min(avail, int64(len(head)))
		if err := readFull(src, head[:n], pos); err != nil {
			return pos, err
		}
		b, ok := parseHeader(head[:n], pos, avail)
		if !ok || !plausibleType(b.typ) {
			return pos, nil
		}
		if !visit(b) {
			return pos, nil
		}
		pos = b.end
	}
	return pos, nil
}

func readFull(src core.Source, p []byte, off int64) error {
	n, err := src.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("read: %w", err)
}

// image is a top-level box held in memory while it is edited. A nested box's
// offsets index data directly.
type image struct {
	data  []byte
	off   int64 // where data[0] sits in the file
	dirty bool
}

// zero clears data[start:end] and reports whether it held anything.
func (m *image) zero(start, end int64) bool {
	region := m.data[start:end]
	if core.AllZero(region) {
		return false
	}
	clear(region)
	m.dirty = true
	return true
}

// free turns a nested box into a free box of the same size, whatever form its
// header takes, so that nothing after it moves.
func (m *image) free(b box) {
	copy(m.data[b.start+4:], "free")
	clear(m.data[b.body:b.end])
	m.dirty = true
}

// eachBox is the forgiving walk used when only describing a box that is being
// removed whole. A parsing mistake here can only make a report less specific.
func eachBox(data []byte, visit func(typ string, payload []byte)) {
	pos := int64(0)
	end := int64(len(data))
	for end-pos >= 8 {
		if binary.BigEndian.Uint32(data[pos:]) == 0 {
			return
		}
		b, ok := parseHeader(data[pos:], pos, end-pos)
		if !ok {
			return
		}
		visit(b.typ, data[b.body:b.end])
		pos = b.end
	}
}

// assign records the only permitted child of a given type. A second one is
// refused: parsers disagree on which wins, so what decides whether a track is
// kept here could differ from what a player reads.
func assign(slot **box, b box) error {
	if *slot != nil {
		return core.Malformedf("more than one %q box in the same parent", b.typ)
	}
	*slot = &b
	return nil
}

// cursor reads big-endian fields from a box payload. The first read past the end
// sets bad and every later read returns zero, so a parser can read a whole
// structure and check once.
type cursor struct {
	b   []byte
	pos int
	bad bool
}

func (c *cursor) take(n int) []byte {
	if c.bad || n < 0 || n > len(c.b)-c.pos {
		c.bad = true
		return nil
	}
	out := c.b[c.pos : c.pos+n]
	c.pos += n
	return out
}

func (c *cursor) u8() uint8 {
	if b := c.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (c *cursor) u16() uint16 {
	if b := c.take(2); b != nil {
		return binary.BigEndian.Uint16(b)
	}
	return 0
}

func (c *cursor) u32() uint32 {
	if b := c.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

// sized reads an unsigned field of n bytes. ISOBMFF permits 0, 4 and 8 in the
// tables that use this; a size of zero means the field is absent.
func (c *cursor) sized(n int) uint64 {
	switch n {
	case 0:
		return 0
	case 4:
		return uint64(c.u32())
	default:
		if b := c.take(8); b != nil {
			return binary.BigEndian.Uint64(b)
		}
		return 0
	}
}

// cstring reads up to a NUL or the end of the payload, which some writers use
// in place of the final NUL.
func (c *cursor) cstring() string {
	if c.bad {
		return ""
	}
	rest := c.b[c.pos:]
	for i, x := range rest {
		if x == 0 {
			c.pos += i + 1
			return string(rest[:i])
		}
	}
	c.pos = len(c.b)
	return string(rest)
}

func (c *cursor) remaining() int { return len(c.b) - c.pos }

// keptLength returns how much of a kept box's payload its own fields account for,
// or -1 if its layout is not that simple or the payload is too short to tell. No
// reader looks beyond that point, so a writer can leave anything there.
func keptLength(typ string, p []byte) int64 {
	// Boxes with no version and flags first.
	switch typ {
	case "txas":
		return 0
	case "irot", "imir", "a1op":
		return 1
	case "lsel":
		return 2
	case "clli":
		return 4
	case "pasp":
		return 8
	case "fiel":
		return 2
	case "gama":
		return 4
	case "frma":
		return 4
	case "enda":
		return 2
	case "load":
		return 16
	case "avcC":
		return avcConfigLength(p)
	case "hvcC":
		return hevcConfigLength(p)
	case "av1C":
		return av1ConfigLength(p)
	case "mdcv":
		return 24
	case "clap":
		return 32
	case "colr":
		switch {
		case len(p) >= 4 && string(p[:4]) == "nclx":
			return 11
		case len(p) >= 4 && string(p[:4]) == "nclc":
			return 10
		case len(p) >= 8 && (string(p[:4]) == "prof" || string(p[:4]) == "rICC"):
			// An ICC profile starts with its own size, and a real one has a 128-byte header.
			const profileHeader = 128
			if n := int64(binary.BigEndian.Uint32(p[4:])); n >= profileHeader && 4+n <= int64(len(p)) {
				return 4 + n
			}
		}
		return -1
	}
	if len(p) < 4 {
		return -1
	}
	version, flags := p[0], fullBoxFlags(p)
	wide := func(v0, v1 int64) int64 {
		if version == 1 {
			return v1
		}
		return v0
	}
	// entries is a table with a 32-bit count at the given offset.
	entries := func(at, entry int64) int64 {
		if int64(len(p)) < at+4 {
			return -1
		}
		return at + 4 + int64(binary.BigEndian.Uint32(p[at:]))*entry
	}
	switch typ {
	case "mvhd":
		return wide(100, 112)
	case "tkhd":
		return wide(84, 96)
	case "mdhd":
		return wide(24, 36)
	case "pitm":
		return wide(6, 8)
	case "nmhd", "sthd":
		return 4
	case "smhd", "mfhd", "mfro":
		return 8
	case "vmhd", "clef", "prof", "enof", "ispe", "rloc":
		return 12
	case "esds":
		return esdsLength(p)
	case "vpcC":
		const initSize = 10 // where the size of the initialisation data is, which follows it
		if len(p) < initSize+2 {
			return -1
		}
		return initSize + 2 + int64(binary.BigEndian.Uint16(p[initSize:]))
	case "sgpd":
		return sampleGroupLength(p)
	case "subs":
		return subSampleLength(p)
	case "tfra":
		return fragmentIndexLength(p)
	case "elng":
		if i := bytes.IndexByte(p[4:], 0); i >= 0 {
			return 4 + int64(i) + 1
		}
		return -1
	case "gmin":
		return 16
	case "text":
		return 36
	case "leva":
		return levelAssignmentLength(p)
	case "hmhd":
		return 20
	case "trex":
		return 24
	case "mehd", "tfdt":
		return wide(8, 12)
	case "cslg":
		return wide(24, 44)
	case "elst":
		return entries(4, wide(12, 20))
	case "stts", "ctts", "stsh", "co64":
		return entries(4, 8)
	case "stsc":
		return entries(4, 12)
	case "stss", "stps", "stco":
		return entries(4, 4)
	case "sbgp":
		return entries(wide(8, 12), 8)
	case "stsz":
		if len(p) >= 8 && binary.BigEndian.Uint32(p[4:]) != 0 {
			return 12 // one size for every sample, so no table follows
		}
		return entries(8, 4)
	case "stz2":
		if len(p) < 12 || (p[7] != 4 && p[7] != 8 && p[7] != 16) {
			return -1
		}
		return 12 + (int64(binary.BigEndian.Uint32(p[8:]))*int64(p[7])+7)/8
	case "pixi":
		if len(p) < 5 {
			return -1
		}
		return 5 + int64(p[4])
	case "tfhd":
		n := int64(8)
		for _, field := range [...]struct {
			flag uint32
			size int64
		}{{0x1, 8}, {0x2, 4}, {0x8, 4}, {0x10, 4}, {0x20, 4}} {
			if flags&field.flag != 0 {
				n += field.size
			}
		}
		return n
	case "trun":
		var perSample, head int64
		for _, flag := range [...]uint32{0x100, 0x200, 0x400, 0x800} {
			if flags&flag != 0 {
				perSample += 4
			}
		}
		for _, flag := range [...]uint32{0x1, 0x4} {
			if flags&flag != 0 {
				head += 4
			}
		}
		if n := entries(4, perSample); n >= 0 {
			return n + head
		}
	}
	return -1
}

// appendBox appends a box with an ordinary 32-bit header. Callers only rebuild
// the children of a meta box, which is capped far below 4 GiB.
func appendBox(dst []byte, typ string, payload ...[]byte) []byte {
	size := 8
	for _, p := range payload {
		size += len(p)
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(size))
	dst = append(dst, typ...)
	for _, p := range payload {
		dst = append(dst, p...)
	}
	return dst
}

// metaBody returns the children of a meta box's payload. ISO makes meta a full
// box, but QuickTime writes it as a plain box, so the first child's type shows
// where the children begin.
func metaBody(p []byte) []byte {
	if len(p) >= 8 {
		switch string(p[4:8]) {
		case "hdlr", "keys", "ilst", "free":
			return p
		}
	}
	if len(p) >= 4 {
		return p[4:]
	}
	return nil
}

// fullBoxFlags returns the 24-bit flags of a full box payload of at least 4 bytes.
func fullBoxFlags(p []byte) uint32 {
	return binary.BigEndian.Uint32(p) & 0xFFFFFF
}

// handlerType returns the handler type from an hdlr payload.
func handlerType(p []byte) (string, error) {
	if len(p) < 12 {
		return "", core.Malformedf("hdlr box of %d bytes is too short", len(p))
	}
	return string(p[8:12]), nil
}
