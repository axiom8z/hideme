package matroska

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"math/bits"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxIDWidth is the Matroska limit on an element ID. A header declaring more
	// is refused, so an ID always fits in a uint32.
	maxIDWidth   = 4
	maxSizeWidth = 8
	maxHeaderLen = maxIDWidth + maxSizeWidth

	// zeroScanBuffer bounds the memory used to look at a hostile payload of any
	// size.
	zeroScanBuffer = 64 << 10
)

// element locates one EBML element. It holds only offsets, so a cluster of many
// gigabytes costs no more than a tag.
type element struct {
	id      uint32 // with the length marker, as the Matroska specification writes IDs
	off     int64  // first byte of the ID
	dataOff int64  // first byte of the payload
	size    int64  // payload bytes; not meaningful while unknown is set
	unknown bool   // the size field held the reserved "unknown" value
}

func (e element) end() int64 { return e.dataOff + e.size }

// reader parses element headers from a Source, bounds-checking each one against
// the master that holds it.
type reader struct {
	ctx context.Context
	src core.Source
	buf []byte // allocated on the first zero scan
}

// header parses the element at off, which must fit entirely before limit. Only
// the caller knows whether an unknown size is legal, so that is left to it.
func (r *reader) header(off, limit int64) (element, error) {
	if off < 0 || off >= limit {
		return element{}, core.Malformedf("matroska: element offset %d is outside the range ending at %d", off, limit)
	}
	var raw [maxHeaderLen]byte
	head := raw[:min(limit-off, maxHeaderLen)]
	if err := r.read(head, off); err != nil {
		return element{}, err
	}
	id, idWidth, err := parseID(head)
	if err != nil {
		return element{}, fmt.Errorf("%w at offset %d", err, off)
	}
	size, sizeWidth, unknown, err := parseSize(head[idWidth:])
	if err != nil {
		return element{}, fmt.Errorf("%w at offset %d", err, off)
	}
	e := element{id: id, off: off, dataOff: off + int64(idWidth+sizeWidth), size: size, unknown: unknown}
	if !unknown && size > limit-e.dataOff {
		return element{}, core.Malformedf("matroska: element %#x at offset %d declares %d bytes, overrunning its parent, which ends at %d",
			id, off, size, limit)
	}
	return e, nil
}

// parseID reads an element ID. The all-zeros and all-ones values are reserved by
// the EBML specification, and treating them as IDs would let padding pass as an
// element. The exception is 0x80: Matroska defines ChapterDisplay with that ID in
// spite of the rule, and real chapters use it.
func parseID(b []byte) (id uint32, width int, err error) {
	if len(b) == 0 {
		return 0, 0, core.Malformedf("matroska: missing element ID")
	}
	width = bits.LeadingZeros8(b[0]) + 1
	if width > maxIDWidth {
		return 0, 0, core.Malformedf("matroska: invalid element ID starting with %#02x", b[0])
	}
	if len(b) < width {
		return 0, 0, core.Malformedf("matroska: truncated element ID")
	}
	for _, c := range b[:width] {
		id = id<<8 | uint32(c)
	}
	mask := uint32(1)<<(7*width) - 1
	if data := id & mask; (data == 0 && id != idChapterDisplay) || data == mask {
		return 0, 0, core.Malformedf("matroska: reserved element ID %#x", id)
	}
	return id, width, nil
}

// parseSize reads an element size. All value bits set means the size is unknown,
// which live WebM uses for segments and clusters that are still being written.
func parseSize(b []byte) (size int64, width int, unknown bool, err error) {
	if len(b) == 0 {
		return 0, 0, false, core.Malformedf("matroska: missing element size")
	}
	width = bits.LeadingZeros8(b[0]) + 1
	if width > maxSizeWidth {
		return 0, 0, false, core.Malformedf("matroska: invalid element size starting with %#02x", b[0])
	}
	if len(b) < width {
		return 0, 0, false, core.Malformedf("matroska: truncated element size")
	}
	var v uint64
	for _, c := range b[:width] {
		v = v<<8 | uint64(c)
	}
	mask := uint64(1)<<(7*width) - 1
	v &= mask
	if v == mask {
		return 0, width, true, nil
	}
	return int64(v), width, false, nil
}

// voidHeader returns the ID and size field of a Void element occupying exactly
// total bytes, so that replacing an element moves nothing after it.
//
// The field's width depends on the payload it describes, and all value bits set
// means "unknown", so a width of w can describe at most 2^(7w)-2 bytes. That
// makes 129 the first awkward total: one size byte would need the value 127,
// which is the reserved pattern, so it takes two size bytes and a payload of 126.
func voidHeader(total int64) ([]byte, error) {
	if total < 2 {
		return nil, core.Malformedf("matroska: %d bytes cannot hold a Void element", total)
	}
	for width := 1; width <= maxSizeWidth; width++ {
		payload := total - 1 - int64(width)
		if payload < 0 {
			break
		}
		if payload > int64(1)<<(7*width)-2 {
			continue
		}
		marked := uint64(payload) | uint64(1)<<(7*width)
		var raw [8]byte
		binary.BigEndian.PutUint64(raw[:], marked)
		return append([]byte{byte(idVoid)}, raw[8-width:]...), nil
	}
	return nil, fmt.Errorf("%w: a region of %d bytes is too long for one Void element", core.ErrTooLarge, total)
}

// children iterates over a master of known size. An unknown size is legal only
// for segments and clusters, which have their own walks, so anywhere else it
// means the file is damaged.
func (r *reader) children(m element) iter.Seq2[element, error] {
	return func(yield func(element, error) bool) {
		for pos := m.dataOff; pos < m.end(); {
			if err := r.ctx.Err(); err != nil {
				yield(element{}, err)
				return
			}
			e, err := r.header(pos, m.end())
			if err == nil && e.unknown {
				err = core.Malformedf("matroska: element %#x at offset %d has an unknown size, which only segments and clusters may have", e.id, e.off)
			}
			if err != nil {
				yield(element{}, err)
				return
			}
			if !yield(e, nil) {
				return
			}
			pos = e.end()
		}
	}
}

// isZero reports whether n bytes at off are all zero, reading in bounded chunks.
func (r *reader) isZero(off, n int64) (bool, error) {
	if r.buf == nil && n > 0 {
		r.buf = make([]byte, zeroScanBuffer)
	}
	return core.IsZeroRange(r.ctx, r.src, off, n, r.buf)
}

// readUint reads an unsigned integer payload, which EBML limits to eight bytes. An
// empty payload is zero by specification.
func (r *reader) readUint(e element) (uint64, error) {
	if e.size > 8 {
		return 0, core.Malformedf("matroska: integer element %#x at offset %d is %d bytes long", e.id, e.off, e.size)
	}
	var raw [8]byte
	if err := r.read(raw[8-e.size:], e.dataOff); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(raw[:]), nil
}

// peek returns up to limit leading payload bytes. It serves the report only, so
// a failed read gives nothing rather than an error; the copy that follows reads
// the same bytes and reports any real failure.
func (r *reader) peek(e element, limit int64) []byte {
	buf := make([]byte, min(e.size, limit))
	if r.read(buf, e.dataOff) != nil {
		return nil
	}
	return buf
}

// read fills buf from off. io.ReaderAt allows io.EOF alongside a full read at the
// very end of the input, so only a short read counts as a failure.
func (r *reader) read(buf []byte, off int64) error {
	n, err := r.src.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("matroska: read %d bytes at offset %d: %w", len(buf), off, err)
}
