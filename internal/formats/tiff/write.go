package tiff

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/axiom8z/hideme/internal/core"
)

// layout assigns every output offset before anything is written, because each
// directory comes before the values and strips it points to. Pages go one after
// another: directory, then its values in tag order, then its strips in index
// order. The order depends only on what is kept, so a cleaned file lays out
// exactly as it already is, and that is what makes cleaning idempotent.
func (d *document) layout(limit int64) error {
	cursor := int64(headerSize)
	var overflow bool
	place := func(n int64) uint32 {
		cursor += cursor & 1 // TIFF requires values and directories on even offsets
		off := cursor
		cursor += n
		if off > math.MaxUint32 || cursor > limit {
			overflow = true
		}
		return uint32(off)
	}
	for _, pg := range d.pages {
		pg.offset = place(directoryOverhead + int64(len(pg.fields))*entrySize)
		for i := range pg.fields {
			if f := &pg.fields[i]; f.outOfLine() {
				f.offset = place(f.size())
			}
		}
		pg.stripOffs = make([]uint32, len(pg.strips))
		counts := make([]uint32, len(pg.strips))
		for i, s := range pg.strips {
			size := s.outSize()
			if size == 0 {
				continue
			}
			pg.stripOffs[i] = place(size)
			counts[i] = uint32(size)
		}
		for i := range pg.fields {
			switch pg.fields[i].tag {
			case pg.offsetsTag:
				pg.fields[i].array = pg.stripOffs
			case pg.countsTag:
				pg.fields[i].array = counts
			}
		}
		if overflow {
			return fmt.Errorf("%w: the output would pass %d bytes, beyond what TIFF offsets or the growth limit of %d allow", core.ErrTooLarge, cursor, limit)
		}
	}
	return nil
}

// write emits the document as layout placed it.
func (d *document) write(ctx context.Context, dst io.Writer) error {
	w := &positionWriter{dst: dst}
	header := make([]byte, headerSize)
	if d.order == binary.ByteOrder(binary.BigEndian) {
		copy(header, magicBig)
	} else {
		copy(header, magicLittle)
	}
	d.order.PutUint32(header[4:], d.pages[0].offset)
	if err := w.write(header); err != nil {
		return err
	}
	for i, pg := range d.pages {
		var next uint32
		if i+1 < len(d.pages) {
			next = d.pages[i+1].offset
		}
		if err := d.writePage(ctx, w, pg, next); err != nil {
			return err
		}
	}
	return nil
}

func (d *document) writePage(ctx context.Context, w *positionWriter, pg *page, next uint32) error {
	if err := w.padTo(ctx, int64(pg.offset)); err != nil {
		return err
	}
	if err := w.write(d.encodeDirectory(pg, next)); err != nil {
		return err
	}
	for i := range pg.fields {
		f := &pg.fields[i]
		if !f.outOfLine() {
			continue
		}
		if err := w.padTo(ctx, int64(f.offset)); err != nil {
			return err
		}
		if err := d.writeValue(ctx, w, f); err != nil {
			return err
		}
	}
	for i, s := range pg.strips {
		if s.outSize() == 0 {
			continue
		}
		if err := w.padTo(ctx, int64(pg.stripOffs[i])); err != nil {
			return err
		}
		if err := d.copySpan(ctx, w, s); err != nil {
			return err
		}
	}
	return nil
}

func (d *document) encodeDirectory(pg *page, next uint32) []byte {
	buf := make([]byte, directoryOverhead+len(pg.fields)*entrySize)
	d.order.PutUint16(buf, uint16(len(pg.fields)))
	for i := range pg.fields {
		f := &pg.fields[i]
		e := buf[2+i*entrySize:]
		d.order.PutUint16(e, f.tag)
		d.order.PutUint16(e[2:], f.typ)
		d.order.PutUint32(e[4:], f.count)
		switch {
		case f.outOfLine():
			d.order.PutUint32(e[8:], f.offset)
		case f.array != nil:
			d.order.PutUint32(e[8:], f.array[0])
		default:
			copy(e[8:12], f.inline[:])
		}
	}
	d.order.PutUint32(buf[2+len(pg.fields)*entrySize:], next)
	return buf
}

func (d *document) writeValue(ctx context.Context, w *positionWriter, f *field) error {
	if f.array == nil {
		return d.copySpan(ctx, w, f.value)
	}
	buf := make([]byte, 4*len(f.array))
	for i, v := range f.array {
		d.order.PutUint32(buf[4*i:], v)
	}
	return w.write(buf)
}

func (d *document) copySpan(ctx context.Context, w *positionWriter, s span) error {
	if len(s.cuts) == 0 {
		return core.CopyRange(ctx, w, d.src, s.off, s.size)
	}
	return core.CopyPatched(ctx, w, io.NewSectionReader(d.src, s.off, s.size), s.cuts)
}

// positionWriter tracks the output position, so that padding is computed from
// where the bytes actually are rather than repeating layout's arithmetic.
type positionWriter struct {
	dst io.Writer
	pos int64
}

func (w *positionWriter) Write(b []byte) (int, error) {
	n, err := w.dst.Write(b)
	w.pos += int64(n)
	return n, err
}

func (w *positionWriter) write(b []byte) error {
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// padTo writes zeros up to pos. Padding is always zero, so it can never carry
// over what the input had in that place.
func (w *positionWriter) padTo(ctx context.Context, pos int64) error {
	return core.WriteZeros(ctx, w, pos-w.pos)
}
