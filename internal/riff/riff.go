// Package riff walks the chunk structure of RIFF files (WebP, WAV and AVI) over
// a core.Source, without loading chunk payloads. A multi-gigabyte AVI costs the
// same memory as a tiny WebP.
//
// A typical walk over a single-form file:
//
//	root, form, err := riff.Open(src)
//	if err != nil {
//		return err
//	}
//	for c, err := range riff.Children(ctx, src, root) {
//		if err != nil {
//			return err // the file is damaged: fail closed
//		}
//		// c.ID, c.DataOff, c.Size, c.End
//	}
//	// root.End is the logical end of the file. Bytes after it are not part of it.
//
// The iterators yield errors in-line rather than through a separate Err method,
// because forgetting to check one would make a truncated file look like a
// complete one, and that is exactly the mistake a privacy tool cannot make.
//
// AVI files over 1 GiB (OpenDML) hold further RIFF chunks after the first, so an
// AVI handler should walk them with Chunks(ctx, src, 0, src.Size()) instead.
//
// Children reads one level. A handler that recurses into nested LIST chunks must
// cap its own depth, because a hostile file can nest them millions of levels deep.
package riff

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// headerSize is the four-character ID plus the little-endian uint32 size.
	headerSize = 8
	// formSize is the four-character type at the start of RIFF and LIST payloads.
	formSize = 4

	idRIFF = "RIFF"
	idLIST = "LIST"
)

// Chunk describes one chunk. It holds only offsets, so the payload stays in the
// source until a handler asks for it, for example through core.CopyRange.
type Chunk struct {
	// ID is the four-character chunk identifier, such as "EXIF" or "LIST". It
	// comes from the file: compare it, and if it has to be shown, put it through
	// core.SafeText and into a Finding's Value, which clean does not print. Field,
	// Where, warnings and errors are printed, so they must not hold it.
	ID string
	// Offset is where the chunk's 8-byte header starts.
	Offset int64
	// DataOff is where the payload starts.
	DataOff int64
	// Size is the payload size, not including any pad byte.
	Size int64
	// End is where the next sibling starts. Chunks are padded to an even length,
	// so End is one past the payload when Size is odd. The exception is an odd
	// chunk that ends exactly at its parent's end: many writers omit that final
	// pad, and there End equals DataOff+Size. A pad byte's value is not checked,
	// so a handler keeping a chunk should write its own zero rather than copy it.
	End int64
}

// IsList reports whether the chunk contains subchunks after a four-character
// type: a "LIST" chunk or a "RIFF" form.
func (c Chunk) IsList() bool {
	return c.ID == idLIST || c.ID == idRIFF
}

// Form returns the form type of a RIFF file, such as "WEBP", "WAVE" or "AVI ",
// or "" if head does not start like one. It reads only the first 12 bytes of
// head, so it suits Handler.Match, and it checks nothing else: Open does that.
func Form(head []byte) string {
	if len(head) < headerSize+formSize || string(head[:4]) != idRIFF {
		return ""
	}
	return string(head[headerSize : headerSize+formSize])
}

// Open validates the RIFF header at the start of src and returns the root chunk
// and its form type. The root must lie within the file, so a truncated file
// fails here. Bytes after root.End are left for the handler to judge.
//
// RIFX (big-endian) and RF64/BW64 (64-bit) are real but different formats, so
// they return core.ErrUnsupported instead of being misread.
func Open(src core.Source) (root Chunk, form string, err error) {
	if src.Size() < headerSize+formSize {
		return Chunk{}, "", core.Malformedf("riff: %d bytes is too short for a RIFF header", src.Size())
	}
	buf := make([]byte, headerSize+formSize)
	if err := readAt(src, buf, 0); err != nil {
		return Chunk{}, "", err
	}
	switch id := string(buf[:4]); id {
	case idRIFF:
	case "RIFX", "RF64", "BW64":
		return Chunk{}, "", core.Unsupportedf("riff: the %s variant is not supported", id)
	default:
		return Chunk{}, "", core.Unsupportedf("riff: the file is not RIFF")
	}
	root, err = parseHeader(buf, 0, src.Size())
	if err != nil {
		return Chunk{}, "", err
	}
	if root.Size < formSize {
		return Chunk{}, "", core.Malformedf("riff: RIFF chunk of %d bytes has no room for a form type", root.Size)
	}
	return root, string(buf[headerSize:]), nil
}

// ListType reads the four-character type at the start of a LIST or RIFF chunk,
// such as "INFO" or "movi".
func ListType(src core.Source, c Chunk) (string, error) {
	if err := checkList(c); err != nil {
		return "", err
	}
	buf := make([]byte, formSize)
	if err := readAt(src, buf, c.DataOff); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Children iterates over the subchunks of a LIST or RIFF chunk, skipping its
// type. A subchunk that overruns the parent yields an error, and so does walking
// a chunk that is not a list, because a payload is not chunk data.
func Children(ctx context.Context, src core.Source, parent Chunk) iter.Seq2[Chunk, error] {
	if err := checkList(parent); err != nil {
		return func(yield func(Chunk, error) bool) { yield(Chunk{}, err) }
	}
	return Chunks(ctx, src, parent.DataOff+formSize, parent.DataOff+parent.Size)
}

func checkList(c Chunk) error {
	if !c.IsList() {
		return core.Malformedf("riff: the chunk at offset %d is not a list", c.Offset)
	}
	if c.Size < formSize {
		return core.Malformedf("riff: the list at offset %d is %d bytes, too small for a list type", c.Offset, c.Size)
	}
	return nil
}

// Chunks iterates over the sequence of chunks in the byte range [start, end).
// It stops at the first error, which it yields with a zero Chunk: a chunk that
// overruns end, bytes too few to be a chunk header, a read failure, or the
// context being cancelled. Stray bytes count as damage because they could hold
// anything, and a handler cannot vouch for what it did not parse.
func Chunks(ctx context.Context, src core.Source, start, end int64) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		if start < 0 || start > end || end > src.Size() {
			yield(Chunk{}, core.Malformedf("riff: range %d to %d is outside the %d-byte file", start, end, src.Size()))
			return
		}
		header := make([]byte, headerSize)
		for pos := start; pos < end; {
			if err := ctx.Err(); err != nil {
				yield(Chunk{}, err)
				return
			}
			if end-pos < headerSize {
				yield(Chunk{}, core.Malformedf("riff: %d stray bytes at offset %d are too few for a chunk header", end-pos, pos))
				return
			}
			if err := readAt(src, header, pos); err != nil {
				yield(Chunk{}, err)
				return
			}
			c, err := parseHeader(header, pos, end)
			if err != nil {
				yield(Chunk{}, err)
				return
			}
			if !yield(c, nil) {
				return
			}
			pos = c.End
		}
	}
}

// parseHeader builds the chunk whose header is at the start of raw, located at
// off inside a parent ending at parentEnd. Callers guarantee the header itself
// fits before parentEnd, so comparing against the room left, rather than adding
// the file's size to an offset, cannot overflow however large the Source claims
// to be.
func parseHeader(raw []byte, off, parentEnd int64) (Chunk, error) {
	size := int64(binary.LittleEndian.Uint32(raw[4:]))
	c := Chunk{ID: string(raw[:4]), Offset: off, DataOff: off + headerSize, Size: size}
	if size > parentEnd-c.DataOff {
		return Chunk{}, core.Malformedf("riff: the chunk at offset %d declares %d bytes, overrunning its parent, which ends at offset %d",
			off, size, parentEnd)
	}
	c.End = c.DataOff + size
	if size%2 == 1 && c.End < parentEnd {
		c.End++
	}
	return c, nil
}

// readAt fills buf from off. io.ReaderAt allows io.EOF alongside a full read at
// the very end of the input, so only a short read counts as a failure.
func readAt(src core.Source, buf []byte, off int64) error {
	n, err := src.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("riff: read %d bytes at offset %d: %w", len(buf), off, err)
}
