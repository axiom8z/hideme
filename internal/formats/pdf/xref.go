package pdf

import (
	"bytes"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// loadFromXref reads the chain of cross-reference sections from newest to
// oldest. The first definition seen of each object wins, so older revisions lose
// to newer ones without being looked at, and a section's free entries stop older
// sections from bringing a deleted object back.
func (d *document) loadFromXref() error {
	off, err := d.startXref()
	if err != nil {
		return err
	}
	seen := make(map[int]bool)
	for {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		if seen[off] {
			return core.Malformedf("cross-reference chain loops at offset %d", off)
		}
		seen[off] = true
		trailer, err := d.readSection(off)
		if err != nil {
			return err
		}
		d.sections++
		prev := trailer.get("Prev")
		if prev == nil {
			break
		}
		n, ok := prev.(int64)
		if !ok || n < 0 || n >= int64(len(d.data)) {
			return core.Malformedf("invalid /Prev in a trailer")
		}
		if n == 0 {
			break // some writers use 0 to mean there is no previous section
		}
		off = int(n)
	}
	return d.adoptTrailers()
}

func (d *document) startXref() (int, error) {
	i := bytes.LastIndex(d.data, []byte("startxref"))
	if i < 0 {
		return 0, core.Malformedf("no startxref")
	}
	p := parser{data: d.data, pos: i + len("startxref"), budget: &d.budget}
	off, ok := p.unsigned()
	if !ok || off >= len(d.data) {
		return 0, core.Malformedf("invalid startxref")
	}
	return off, nil
}

// readSection reads one section, a classic table or a cross-reference stream,
// and returns its trailer dictionary.
func (d *document) readSection(off int) (*pdfDict, error) {
	p := parser{data: d.data, pos: off, budget: &d.budget}
	p.skipSpace()
	if bytes.HasPrefix(d.data[p.pos:], []byte("xref")) {
		return d.readTable(&p)
	}
	return d.readXrefStream(p.pos)
}

func (d *document) addEntry(num int, e xrefEntry) error {
	if num < 0 || num > maxObjectNumber {
		return core.Malformedf("object number %d is out of range", num)
	}
	if _, ok := d.entries[num]; !ok {
		d.entries[num] = e
	}
	return nil
}

func (d *document) readTable(p *parser) (*pdfDict, error) {
	start := p.pos
	p.pos += len("xref")
	var free []int
	for !p.hasKeyword("trailer") {
		first, ok1 := p.unsigned()
		count, ok2 := p.unsigned()
		if !ok1 || !ok2 {
			return nil, core.Malformedf("invalid cross-reference subsection at offset %d", p.pos)
		}
		for i := range count {
			offset, ok1 := p.unsigned()
			gen, ok2 := p.unsigned()
			if !ok1 || !ok2 {
				return nil, core.Malformedf("invalid cross-reference entry at offset %d", p.pos)
			}
			switch {
			case p.hasKeyword("f"):
				free = append(free, first+i)
			case !p.hasKeyword("n"):
				return nil, core.Malformedf("invalid cross-reference entry at offset %d", p.pos)
			case offset == 0:
				free = append(free, first+i) // some writers mark unused objects this way
			default:
				if err := d.addEntry(first+i, xrefEntry{kind: entryInUse, offset: offset, gen: gen, container: -1}); err != nil {
					return nil, err
				}
			}
		}
	}
	val, err := p.value(0)
	if err != nil {
		return nil, err
	}
	trailer, ok := val.(*pdfDict)
	if !ok {
		return nil, core.Malformedf("trailer is not a dictionary")
	}
	if err := d.spendWork(p.pos - start); err != nil {
		return nil, err
	}
	d.trailers = append(d.trailers, trailer)

	// In a hybrid file the table hides objects by marking them free, and the
	// companion stream defines them. So the stream goes before the free entries
	// but still after the table's in-use ones, and before any older section.
	if v := trailer.get("XRefStm"); v != nil {
		n, ok := v.(int64)
		if !ok || n <= 0 || n >= int64(len(d.data)) {
			return nil, core.Malformedf("invalid /XRefStm in a trailer")
		}
		if _, err := d.readXrefStream(int(n)); err != nil {
			return nil, err
		}
	}
	for _, num := range free {
		if err := d.addEntry(num, xrefEntry{kind: entryFree, container: -1}); err != nil {
			return nil, err
		}
	}
	return trailer, nil
}

func (d *document) readXrefStream(off int) (*pdfDict, error) {
	p := parser{data: d.data, pos: off, budget: &d.budget}
	if _, _, err := p.objectHeader(); err != nil {
		return nil, err
	}
	val, err := d.parseBody(&p)
	if workErr := d.spendWork(p.pos - off); workErr != nil {
		return nil, workErr
	}
	if err != nil {
		return nil, err
	}
	s, ok := val.(*pdfStream)
	if !ok {
		return nil, core.Malformedf("no cross-reference data at offset %d", off)
	}
	if t, _ := s.dict.get("Type").(pdfName); t != "XRef" {
		return nil, core.Malformedf("stream at offset %d is not a cross-reference stream", off)
	}
	d.trailers = append(d.trailers, s.dict)
	data, err := d.decodeStream(s)
	if err != nil {
		return nil, err
	}
	return s.dict, d.readXrefEntries(s.dict, data)
}

func (d *document) readXrefEntries(dict *pdfDict, data []byte) error {
	widths, ok := dict.get("W").(pdfArray)
	if !ok || len(widths) != 3 {
		return core.Malformedf("invalid /W in a cross-reference stream")
	}
	var w [3]int
	for i := range w {
		n, ok := widths[i].(int64)
		if !ok || n < 0 || n > 8 {
			return core.Malformedf("invalid /W in a cross-reference stream")
		}
		w[i] = int(n)
	}
	rowLen := w[0] + w[1] + w[2]
	if rowLen == 0 {
		return core.Malformedf("invalid /W in a cross-reference stream")
	}
	index, err := xrefIndex(dict)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(index); i += 2 {
		first, count := index[i], index[i+1]
		if count > int64(len(data)/rowLen) {
			return core.Malformedf("cross-reference stream is shorter than its /Index says")
		}
		for j := range count {
			row := data[:rowLen]
			data = data[rowLen:]
			typ := uint64(1) // the default when the type field has width 0
			if w[0] > 0 {
				typ = bigEndian(row[:w[0]])
			}
			f2 := bigEndian(row[w[0] : w[0]+w[1]])
			f3 := bigEndian(row[w[0]+w[1]:])
			if first+j > maxObjectNumber {
				return core.Malformedf("object number %d is out of range", first+j)
			}
			if err := d.addXrefRow(int(first+j), typ, f2, f3); err != nil {
				return err
			}
		}
	}
	return nil
}

// addXrefRow records one row. Out-of-range fields are clamped to values that fail
// when the object is looked up, so a bad entry only matters if something uses it.
func (d *document) addXrefRow(num int, typ, f2, f3 uint64) error {
	const clamp = 1 << 31
	switch {
	case typ == 1 && f2 != 0: // like a table, some streams use offset 0 for unused
		return d.addEntry(num, xrefEntry{
			kind:      entryInUse,
			offset:    int(min(f2, uint64(len(d.data)))),
			gen:       int(min(f3, clamp)),
			container: -1,
		})
	case typ == 2:
		return d.addEntry(num, xrefEntry{
			kind:      entryCompressed,
			container: int(min(f2, clamp)),
			index:     int(min(f3, clamp)),
		})
	}
	// Type 0 is free, and the specification says to read unknown types as null.
	return d.addEntry(num, xrefEntry{kind: entryFree, container: -1})
}

func xrefIndex(dict *pdfDict) ([]int64, error) {
	v, ok := dict.get("Index").(pdfArray)
	if !ok {
		size, ok := dict.get("Size").(int64)
		if !ok || size < 0 {
			return nil, core.Malformedf("invalid /Size in a cross-reference stream")
		}
		return []int64{0, size}, nil
	}
	out := make([]int64, 0, len(v))
	for _, e := range v {
		n, ok := e.(int64)
		if !ok || n < 0 {
			return nil, core.Malformedf("invalid /Index in a cross-reference stream")
		}
		out = append(out, n)
	}
	return out, nil
}

func bigEndian(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func encryptedError() error {
	return fmt.Errorf("%w: the document has an /Encrypt dictionary", core.ErrEncrypted)
}

// adoptTrailers takes the newest /Root, /Info and /ID from the trailers, which
// are in newest-first order. A linearized file keeps them only in its first-page
// trailer, and an update may repeat them or not.
func (d *document) adoptTrailers() error {
	found := false
	for _, t := range d.trailers {
		if t.has("Encrypt") {
			return encryptedError()
		}
		if v := t.get("Root"); v != nil && !found {
			r, ok := v.(pdfRef)
			if !ok {
				return core.Malformedf("/Root is not a reference")
			}
			d.root, found = r, true
		}
		if d.info == nil {
			d.info = t.get("Info")
		}
		if d.id == nil {
			d.id = t.get("ID")
		}
	}
	if !found {
		return core.Malformedf("no trailer has a /Root")
	}
	return nil
}
