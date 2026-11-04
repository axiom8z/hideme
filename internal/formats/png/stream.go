package png

import (
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"

	"github.com/axiom8z/hideme/internal/core"
)

// maxImageBytes caps how much image data one file may decompress in total. Checking
// a stream costs time but no memory, and compression's limit of about 1000 to 1 lets
// a small file demand a lot of it. This is more than any real PNG holds: 16 GiB is
// a picture 65,000 pixels square at 32 bits.
const maxImageBytes = 16 << 30

// imageStream is one zlib stream of image data, spread over IDAT or fdAT chunks.
type imageStream struct {
	width, height uint32
	where         string
	pieces        []streamPiece
}

// streamPiece is the part of one chunk that belongs to a stream.
type streamPiece struct {
	part int    // index into cleaner.parts
	typ  string // the chunk's type
	head []byte // what comes before the image data: fdAT's sequence number
	data []byte
}

// pieceReader reads a stream's pieces in turn and counts what it hands out. It
// implements io.ByteReader, so zlib reads no further than the end of the stream and
// the count is exact.
type pieceReader struct {
	pieces   []streamPiece
	offset   int
	consumed int64
}

func (r *pieceReader) next() bool {
	for len(r.pieces) > 0 && r.offset == len(r.pieces[0].data) {
		r.pieces = r.pieces[1:]
		r.offset = 0
	}
	return len(r.pieces) > 0
}

func (r *pieceReader) ReadByte() (byte, error) {
	if !r.next() {
		return 0, io.EOF
	}
	b := r.pieces[0].data[r.offset]
	r.offset++
	r.consumed++
	return b, nil
}

func (r *pieceReader) Read(p []byte) (int, error) {
	if !r.next() {
		return 0, io.EOF
	}
	n := copy(p, r.pieces[0].data[r.offset:])
	r.offset += n
	r.consumed += int64(n)
	return n, nil
}

// checkStream decompresses a stream to learn where it ends, because decoders stop
// there and whatever comes next is no part of the picture. That part is cut. It is
// a good place to hide things: a plain chunk called IDAT passes as image data and
// every decoder skips it.
//
// Data compressed into the stream past the last row cannot be cut without
// re-encoding, so it stays and the user hears about it.
func (c *cleaner) checkStream(ctx context.Context, s *imageStream) error {
	reader := &pieceReader{pieces: s.pieces}
	total, err := c.inflateStream(ctx, reader)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, s.where)
	}
	if need := rawImageSize(c.header, s.width, s.height); total > need {
		c.reportExtraData(total-need, s.where)
	}
	c.cutAfter(s, reader.consumed)
	return nil
}

// inflateStream reads the stream to its end, which needs no memory for its output,
// and returns how many bytes it decompressed to.
//
// A stream that runs out of input was all read as compressed data, so nothing in it
// went unparsed and the picture is merely incomplete, and a wrong checksum still
// marks where the stream ends. Anything else means the decoder would lose its place
// with bytes still to come, so nothing can say what those bytes are.
func (c *cleaner) inflateStream(ctx context.Context, reader *pieceReader) (int64, error) {
	zr, err := c.imageReader(reader)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, nil
	}
	if err != nil {
		return 0, core.Malformedf("png: image data is not a zlib stream: %v", err)
	}
	buf := c.inflateBuffer
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := zr.Read(buf)
		total += int64(n)
		if c.imageBudget -= int64(n); c.imageBudget < 0 {
			return 0, fmt.Errorf("%w: png: image data expands past %d bytes", core.ErrTooLarge, int64(maxImageBytes))
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, zlib.ErrChecksum):
			return total, nil
		default:
			return 0, core.Malformedf("png: image data is damaged: %v", err)
		}
	}
}

// imageReader returns a reader for one stream. An animation can have as many streams
// as it can fit frames of a few dozen bytes, so the reader and its buffer are reused:
// making them anew spent gigabytes on a file of a few megabytes.
func (c *cleaner) imageReader(reader *pieceReader) (io.Reader, error) {
	if c.inflateBuffer == nil {
		c.inflateBuffer = make([]byte, 64<<10)
	}
	if c.imageZlib != nil {
		if err := c.imageZlib.(zlib.Resetter).Reset(reader, nil); err != nil {
			return nil, err
		}
		return c.imageZlib, nil
	}
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, err
	}
	c.imageZlib = zr
	return zr, nil
}

// reportExtraData says that a stream holds more than the picture has room for.
func (c *cleaner) reportExtraData(extra int64, where string) {
	c.add(core.Finding{
		Category: core.CatHidden,
		Risk:     core.RiskHigh,
		Field:    "Extra data in the compressed image",
		Value:    fmt.Sprintf("%d bytes", extra),
		Where:    where,
		Action:   core.ActionRetain,
		Reason:   "cannot be removed without re-encoding the picture",
	})
	c.rep.Warn("the compressed image data holds %d bytes more than the picture needs; they stay in the output, because removing them would mean re-encoding", extra)
}

// cutAfter drops everything in a stream past its first consumed bytes. A chunk left
// with nothing goes entirely, unless it is the first, since a file needs an IDAT, or
// its number has to stay for the sequence of the chunks after it to hold.
func (c *cleaner) cutAfter(s *imageStream, consumed int64) {
	var dropped int64
	for i, p := range s.pieces {
		size := int64(len(p.data))
		keep := min(consumed, size)
		consumed -= keep
		dropped += size - keep
		switch {
		case keep == 0 && len(p.head) == 0 && i > 0:
			c.parts[p.part] = nil
		case keep == size:
		default:
			payload := append(append([]byte{}, p.head...), p.data[:keep]...)
			c.parts[p.part] = buildChunk(p.typ, payload)
		}
	}
	if dropped > 0 {
		c.remove(core.CatHidden, core.RiskHigh, "Data after the compressed image", core.ByteCount(int(dropped)), s.where)
	}
}

// rawImageSize returns how many bytes a stream decompresses to when it holds exactly
// a picture of this size, with each row's filter byte. It saturates: a size that
// large has no room for extra data to be noticed in, and maxImageBytes stops first.
func rawImageSize(h imageHeader, width, height uint32) int64 {
	pixelBits := uint64(h.depth) * uint64(channels(h.colorType))
	plane := func(w, rows uint64) uint64 {
		if w == 0 || rows == 0 {
			return 0
		}
		high, low := bits.Mul64((w*pixelBits+7)/8+1, rows)
		if high != 0 || low > math.MaxInt64 {
			return math.MaxInt64
		}
		return low
	}
	var total uint64
	if !h.interlaced {
		total = plane(uint64(width), uint64(height))
	} else {
		for _, pass := range adam7 {
			w := (uint64(width) + pass.dx - pass.x0 - 1) / pass.dx
			rows := (uint64(height) + pass.dy - pass.y0 - 1) / pass.dy
			total = min(total+plane(w, rows), math.MaxInt64)
		}
	}
	return int64(total)
}

// adam7 lists the seven interlace passes: the first pixel of each and the distance
// between pixels.
var adam7 = []struct{ x0, y0, dx, dy uint64 }{
	{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2},
}

func channels(colorType byte) int {
	switch colorType {
	case colorRGB:
		return 3
	case colorGrayAlpha:
		return 2
	case colorRGBA:
		return 4
	}
	return 1 // gray, and a palette image's single index
}
