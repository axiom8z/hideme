package pdf

import (
	"bytes"
	"context"
	"errors"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxObjectNumber is the highest object number PDF's own implementation
	// limits allow. It also bounds the cross-reference map.
	maxObjectNumber = 8388607
	// maxRefHops bounds a chain of objects that are nothing but a reference to
	// the next. Real files have at most one hop, and a loop reads as null.
	maxRefHops = 32
	// maxLookupDepth bounds lookups nested inside one another, which happen when
	// reading one object needs another: an indirect /Length or /Filter. Real
	// files nest once or twice. An unbounded chain written by an attacker would
	// overflow the stack, which Go cannot recover from.
	maxLookupDepth = 8
)

type entryKind uint8

const (
	entryFree entryKind = iota
	entryInUse
	entryCompressed
)

// xrefEntry says where an object lives. A compressed object names its object
// stream by number while its cross-reference data is being read in, because that
// stream's own entry may be in an older section. Recovery, which can see one
// number defined twice, gives the stream's offset and leaves container at -1.
type xrefEntry struct {
	kind      entryKind
	offset    int
	gen       int
	container int
	index     int
}

type cachedObject struct {
	val object
	err error
}

// objectStream is a decoded object stream with its directory.
type objectStream struct {
	data    []byte
	numbers []int
	offsets []int // absolute within data
}

// document is one parsed PDF. It is built per call, so a handler holds no state
// between files and concurrent use is safe.
type document struct {
	ctx    context.Context
	data   []byte
	limits limits

	budget  valueBudget
	work    int64 // bytes the parser has passed over
	maxWork int64
	decoded int64 // bytes of streams decoded into memory
	scanned int64 // bytes decompressed only to look through them

	inlineBudget int64 // bytes left for the search for inline images
	depth        int   // lookups currently nested

	entries  map[int]xrefEntry
	cache    map[int]cachedObject
	objStms  map[int]*objectStream
	trailers []*pdfDict // newest first

	root pdfRef
	info object
	id   object

	sections   int // cross-reference sections, not counting hybrid streams
	recovered  bool
	superseded int // recovery only: object definitions a later one replaced

	roles        map[int]role
	hasSignature bool
	orphans      int
}

func newDocument(ctx context.Context, data []byte, lim limits) *document {
	return &document{
		ctx:     ctx,
		data:    data,
		limits:  lim,
		budget:  valueBudget{limit: lim.maxValues},
		maxWork: 4*int64(len(data)) + 1<<20,
		entries: make(map[int]xrefEntry),
		cache:   make(map[int]cachedObject),
		objStms: make(map[int]*objectStream),

		inlineBudget: maxInlineWork,
	}
}

// load parses data through its cross-reference data, or by scanning for objects
// if that turns out to be damaged. Conditions no scan can improve on, such as
// encryption, are final.
func load(ctx context.Context, data []byte, lim limits) (*document, error) {
	if !bytes.HasPrefix(data, pdfHeader) {
		return nil, core.Unsupportedf("no %%PDF- header")
	}
	d := newDocument(ctx, data, lim)
	err := d.loadFromXref()
	if err == nil {
		err = d.analyze()
	}
	if err == nil {
		return d, nil
	}
	if isFinal(err) {
		return nil, err
	}
	d = newDocument(ctx, data, lim)
	if err := d.loadByScanning(); err != nil {
		return nil, err
	}
	if err := d.analyze(); err != nil {
		return nil, err
	}
	return d, nil
}

func isFinal(err error) bool {
	return errors.Is(err, core.ErrEncrypted) || errors.Is(err, core.ErrTooLarge) ||
		errors.Is(err, core.ErrUnsupported) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// spendWork accounts for bytes the parser passed over. In a well-formed file
// objects do not overlap, so the total stays near the file size. Hostile
// cross-reference data can point many entries into one long string and make each
// parse nearly the whole file, which this stops.
func (d *document) spendWork(n int) error {
	d.work += int64(n)
	if d.work > d.maxWork {
		return core.Malformedf("objects overlap beyond any valid layout")
	}
	return nil
}

// lookup returns the value of a reference. exists is false for free and missing
// objects, and for a generation that does not match, all of which read as null.
func (d *document) lookup(r pdfRef) (val object, exists bool, err error) {
	e, ok := d.entries[r.num]
	if !ok || e.kind == entryFree || e.gen != r.gen {
		return nil, false, nil
	}
	if c, ok := d.cache[r.num]; ok {
		return c.val, true, c.err
	}
	if d.depth >= maxLookupDepth {
		return nil, true, core.Malformedf("objects depend on one another too deeply")
	}
	d.depth++
	defer func() { d.depth-- }()
	if e.kind == entryInUse {
		val, err = d.parseAt(r.num, e)
	} else {
		val, err = d.parseCompressed(r.num, e)
	}
	if isFinal(err) {
		return nil, true, err
	}
	d.cache[r.num] = cachedObject{val, err}
	return val, true, err
}

func (d *document) parseAt(num int, e xrefEntry) (object, error) {
	if e.offset >= len(d.data) {
		return nil, core.Malformedf("object %d is past the end of the file", num)
	}
	p := parser{data: d.data, pos: e.offset, budget: &d.budget}
	gotNum, gotGen, err := p.objectHeader()
	if err != nil {
		return nil, err
	}
	if gotNum != num || gotGen != e.gen {
		return nil, core.Malformedf("cross-reference for object %d points at object %d", num, gotNum)
	}
	val, err := d.parseBody(&p)
	if workErr := d.spendWork(p.pos - e.offset); workErr != nil {
		return nil, workErr
	}
	return val, err
}

// objectHeader reads "num gen obj".
func (p *parser) objectHeader() (num, gen int, err error) {
	num, ok1 := p.unsigned()
	gen, ok2 := p.unsigned()
	if !ok1 || !ok2 || !p.hasKeyword("obj") {
		return 0, 0, core.Malformedf("object header expected at offset %d", p.pos)
	}
	return num, gen, nil
}

// parseBody reads what follows "obj": a value and, after a dictionary, possibly
// a stream.
func (d *document) parseBody(p *parser) (object, error) {
	val, err := p.value(0)
	if err != nil {
		return nil, err
	}
	dict, ok := val.(*pdfDict)
	if !ok {
		return val, nil
	}
	p.skipSpace()
	if !bytes.HasPrefix(p.data[p.pos:], []byte("stream")) {
		return dict, nil
	}
	p.pos += len("stream")
	p.skipStreamEOL()
	length, known := d.declaredLength(dict.get("Length"))
	end, after, err := d.streamEnd(p.pos, length, known)
	if err != nil {
		return nil, err
	}
	raw := p.data[p.pos:end]
	p.pos = after
	return &pdfStream{dict: dict, raw: raw}, nil
}

// skipStreamEOL passes the line end after the stream keyword. Writers that put
// spaces before it or use a lone CR break the rules but are common enough.
func (p *parser) skipStreamEOL() {
	q := p.pos
	for q < len(p.data) && (p.data[q] == ' ' || p.data[q] == '\t') {
		q++
	}
	if q >= len(p.data) || (p.data[q] != '\r' && p.data[q] != '\n') {
		return
	}
	p.pos = q
	if p.data[p.pos] == '\r' {
		p.pos++
	}
	if p.pos < len(p.data) && p.data[p.pos] == '\n' {
		p.pos++
	}
}

// declaredLength resolves /Length. known is false when the value cannot be used,
// in which case the end of the stream is found by searching.
func (d *document) declaredLength(v object) (length int64, known bool) {
	val, err := d.deref(v)
	if err != nil {
		return 0, false
	}
	n, ok := val.(int64)
	return n, ok
}

// streamEnd trusts /Length only if "endstream" really follows it. Otherwise it
// searches, and takes off the line end that belongs to the keyword. What a search
// costs counts as work, so a file of streams with no endstream cannot make each
// one read to the end of the file.
func (d *document) streamEnd(start int, length int64, known bool) (end, after int, err error) {
	if known && length >= 0 && length <= int64(len(d.data)-start) {
		end = start + int(length)
		q := end
		for q < len(d.data) && isSpace(d.data[q]) {
			q++
		}
		if bytes.HasPrefix(d.data[q:], []byte("endstream")) {
			return end, q + len("endstream"), nil
		}
	}
	i := bytes.Index(d.data[start:], []byte("endstream"))
	if i < 0 {
		if err := d.spendWork(len(d.data) - start); err != nil {
			return 0, 0, err
		}
		return 0, 0, core.Malformedf("stream at offset %d has no endstream", start)
	}
	if err := d.spendWork(i); err != nil {
		return 0, 0, err
	}
	end = start + i
	if end > start && d.data[end-1] == '\n' {
		end--
	}
	if end > start && d.data[end-1] == '\r' {
		end--
	}
	return end, start + i + len("endstream"), nil
}

// parseCompressed reads an object out of its object stream.
func (d *document) parseCompressed(num int, e xrefEntry) (object, error) {
	off := e.offset
	if e.container >= 0 {
		c, ok := d.entries[e.container]
		if !ok || c.kind != entryInUse {
			return nil, core.Malformedf("object %d is in object stream %d, which does not exist", num, e.container)
		}
		off = c.offset
	}
	stm, err := d.objectStreamAt(off)
	if err != nil {
		return nil, err
	}
	if e.index < 0 || e.index >= len(stm.numbers) || stm.numbers[e.index] != num {
		return nil, core.Malformedf("object stream has no object %d at index %d", num, e.index)
	}
	p := parser{data: stm.data, pos: stm.offsets[e.index], budget: &d.budget}
	val, err := p.value(0)
	if workErr := d.spendWork(p.pos - stm.offsets[e.index]); workErr != nil {
		return nil, workErr
	}
	return val, err
}

// objectStreamAt decodes the object stream at a file offset, once. A damaged one
// is remembered as nil.
func (d *document) objectStreamAt(off int) (*objectStream, error) {
	if stm, ok := d.objStms[off]; ok {
		if stm == nil {
			return nil, core.Malformedf("object stream at offset %d is damaged", off)
		}
		return stm, nil
	}
	stm, err := d.readObjectStream(off)
	if err != nil {
		if !isFinal(err) {
			d.objStms[off] = nil
		}
		return nil, err
	}
	d.objStms[off] = stm
	return stm, nil
}

func (d *document) readObjectStream(off int) (*objectStream, error) {
	if off >= len(d.data) {
		return nil, core.Malformedf("object stream is past the end of the file")
	}
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
		return nil, core.Malformedf("object stream at offset %d is not a stream", off)
	}
	return d.decodeObjectStream(s)
}

func (d *document) decodeObjectStream(s *pdfStream) (*objectStream, error) {
	if d.nameOf(s.dict, "Type") != "ObjStm" {
		return nil, core.Malformedf("compressed object's container is not an object stream")
	}
	n, ok1 := d.resolve(s.dict.get("N")).(int64)
	first, ok2 := d.resolve(s.dict.get("First")).(int64)
	if !ok1 || !ok2 || n < 0 || first < 0 {
		return nil, core.Malformedf("object stream directory is invalid")
	}
	data, err := d.decodeStream(s)
	if err != nil {
		return nil, err
	}
	d.maxWork += 2 * int64(len(data))
	if first > int64(len(data)) || n > (first+1)/2 {
		return nil, core.Malformedf("object stream directory does not fit its data")
	}
	stm := &objectStream{data: data}
	p := parser{data: data[:first], budget: &d.budget}
	for range n {
		num, ok1 := p.unsigned()
		off, ok2 := p.unsigned()
		if !ok1 || !ok2 || num > maxObjectNumber || int64(off) > int64(len(data))-first {
			return nil, core.Malformedf("object stream directory is invalid")
		}
		stm.numbers = append(stm.numbers, num)
		stm.offsets = append(stm.offsets, int(first)+off)
	}
	return stm, nil
}

// deref follows references to a value.
func (d *document) deref(v object) (object, error) {
	for range maxRefHops {
		r, ok := v.(pdfRef)
		if !ok {
			return v, nil
		}
		val, _, err := d.lookup(r)
		if err != nil {
			return nil, err
		}
		v = val
	}
	return nil, nil
}

// resolve is deref for reading values once analyze has succeeded. Everything
// reachable has parsed by then, so a failure here can only concern an object
// that is not reachable, which reads as null.
func (d *document) resolve(v object) object {
	val, err := d.deref(v)
	if err != nil {
		return nil
	}
	return val
}

func (d *document) nameOf(dict *pdfDict, key string) string {
	n, _ := d.resolve(dict.get(key)).(pdfName)
	return string(n)
}
