package gif

import (
	"encoding/binary"
	"math"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	maxLZWCodes = 4096
	maxLZWBits  = 12
)

// lzwWalker follows a frame's compressed data far enough to know where it ends. It
// tracks only the code numbering and how many pixels each code stands for, so it needs
// no memory for pixels, and it steps through codes as browsers do, including their
// habit of never widening past 12 bits while waiting for a clear code.
type lzwWalker struct {
	litWidth   uint
	width      uint
	clear, end int32
	next, prev int32
	acc        uint32
	bits       uint
	pixels     int64
	want       int64
	lengths    [maxLZWCodes]int32 // how many pixels each code in the table stands for
}

func (w *lzwWalker) start(litWidth byte, want int64) {
	w.litWidth = uint(litWidth)
	w.clear = 1 << litWidth
	w.end = w.clear + 1
	w.acc, w.bits, w.pixels = 0, 0, 0
	w.want = want
	if want == 0 {
		w.want = math.MaxInt64 // no size to go by, so only the end code ends it
	}
	w.resetTable()
}

func (w *lzwWalker) resetTable() {
	w.width = w.litWidth + 1
	w.next = w.clear + 2
	w.prev = -1
}

// feed reads the payload of one sub-block. When the stream ends in it, feed reports
// done and how many of its bytes the stream used. Decoders stop there, whether at the
// end code or, for the many encoders that leave that out, once the frame has all its
// pixels. A code past the last pixel may still be sharing a byte with the last real
// code, so its bytes count as used, and at most one spare code's worth stays.
func (w *lzwWalker) feed(payload []byte) (used int, done bool, err error) {
	for i, b := range payload {
		w.acc |= uint32(b) << w.bits
		w.bits += 8
		for w.bits >= w.width {
			code := int32(w.acc & (1<<w.width - 1))
			w.acc >>= w.width
			w.bits -= w.width
			switch {
			case code == w.clear:
				w.resetTable()
			case code == w.end, w.pixels >= w.want:
				return i + 1, true, nil
			default:
				if err := w.dataCode(code); err != nil {
					return 0, false, err
				}
			}
		}
	}
	return len(payload), false, nil
}

func (w *lzwWalker) dataCode(code int32) error {
	if w.prev < 0 {
		if code >= w.clear {
			return core.Malformedf("gif: image data starts with the invalid code %d", code)
		}
		w.pixels++
		w.prev = code
		return nil
	}
	if code > w.next {
		return core.Malformedf("gif: image data holds code %d where the table only reaches %d", code, w.next)
	}
	if code < w.next {
		w.pixels += int64(w.length(code))
	} else {
		w.pixels += int64(w.length(w.prev)) + 1
	}
	if w.next < maxLZWCodes {
		w.lengths[w.next] = w.length(w.prev) + 1
		w.next++
		if w.next&(1<<w.width-1) == 0 && w.next < maxLZWCodes && w.width < maxLZWBits {
			w.width++
		}
	}
	w.prev = code
	return nil
}

// length returns how many pixels a code stands for. Single-pixel codes are not stored,
// because a file can have a million frames and the table is only reset by clear codes.
func (w *lzwWalker) length(code int32) int32 {
	if code < w.clear {
		return 1
	}
	return w.lengths[code]
}

// framePixels returns how many pixels an image descriptor declares.
func framePixels(descriptor []byte) int64 {
	const sizeOffset = 5
	width := binary.LittleEndian.Uint16(descriptor[sizeOffset:])
	height := binary.LittleEndian.Uint16(descriptor[sizeOffset+2:])
	return int64(width) * int64(height)
}
