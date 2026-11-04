package pdf

import (
	"bytes"

	"github.com/axiom8z/hideme/internal/core"
)

// finder searches forward for a pattern, keeping its last answer. The recovery
// scan asks for "trailer" once per object, and without this a file with no
// trailer would cost one pass over the file for each object.
type finder struct {
	data    []byte
	pattern []byte
	at      int
	valid   bool
}

// next returns the first match at or after pos, or -1. pos must never decrease.
func (f *finder) next(pos int) int {
	if f.valid && (f.at < 0 || f.at >= pos) {
		return f.at
	}
	f.valid = true
	f.at = -1
	if i := bytes.Index(f.data[pos:], f.pattern); i >= 0 {
		f.at = pos + i
	}
	return f.at
}

// recovery is what the scan collects besides objects: the last catalog it saw, in
// case no trailer survived, and every trailer in file order.
type recovery struct {
	catalog  pdfRef
	trailers []*pdfDict
}

// loadByScanning rebuilds the object table by walking the file for "N G obj".
// File order decides: a later definition replaces an earlier one, which is how
// incremental updates work. Each object is parsed as it is found so that its
// stream data can be stepped over, because binary data may contain anything,
// including text that looks like another object.
func (d *document) loadByScanning() error {
	d.recovered = true
	objs := finder{data: d.data, pattern: []byte("obj")}
	trailers := finder{data: d.data, pattern: []byte("trailer")}
	rec := recovery{catalog: pdfRef{num: -1}}
	pos := 0
	for iteration := 0; ; iteration++ {
		if iteration%1024 == 0 {
			if err := d.ctx.Err(); err != nil {
				return err
			}
		}
		o, t := objs.next(pos), trailers.next(pos)
		if o < 0 && t < 0 {
			break
		}
		if t >= 0 && (o < 0 || t < o) {
			p := parser{data: d.data, pos: t + len("trailer"), budget: &d.budget}
			val, err := p.value(0)
			if workErr := d.spendWork(p.pos - t); workErr != nil {
				return workErr
			}
			if isFinal(err) {
				return err
			}
			pos = t + len("trailer")
			if dict, ok := val.(*pdfDict); ok && err == nil {
				rec.trailers = append(rec.trailers, dict)
				pos = p.pos
			}
			continue
		}
		pos = o + len("obj")
		num, gen, start, ok := d.objectHeaderBefore(o)
		if !ok {
			continue
		}
		p := parser{data: d.data, pos: pos, budget: &d.budget}
		val, err := d.parseBody(&p)
		if workErr := d.spendWork(p.pos - pos); workErr != nil {
			return workErr
		}
		if isFinal(err) {
			return err
		}
		if err != nil {
			// Remember the failure so that a reference to this object fails the
			// document, but do not let what may only be text that resembles an
			// object replace a definition that did parse.
			if _, defined := d.entries[num]; !defined {
				d.entries[num] = xrefEntry{kind: entryInUse, offset: start, gen: gen, container: -1}
				d.cache[num] = cachedObject{nil, err}
			}
			continue
		}
		d.define(num, xrefEntry{kind: entryInUse, offset: start, gen: gen, container: -1})
		d.cache[num] = cachedObject{val, nil}
		pos = p.pos
		if err := d.inspectScanned(val, pdfRef{num, gen}, start, &rec); err != nil {
			return err
		}
	}
	for i := len(rec.trailers) - 1; i >= 0; i-- {
		d.trailers = append(d.trailers, rec.trailers[i])
	}
	return d.adoptRecoveredRoot(rec.catalog)
}

// define records a definition, counting any it replaces. Those are what is left
// of earlier revisions, and recovery has no other way to count them.
func (d *document) define(num int, e xrefEntry) {
	if _, ok := d.entries[num]; ok {
		d.superseded++
	}
	d.entries[num] = e
}

// inspectScanned notes what a freshly found object means for the recovery: an
// object stream defines more objects, a cross-reference stream acts as a trailer,
// and a catalog is the fallback if no trailer survived.
func (d *document) inspectScanned(val object, r pdfRef, start int, rec *recovery) error {
	switch t := val.(type) {
	case *pdfDict:
		if isEncryptDict(t) {
			return encryptedError()
		}
		if ty, _ := t.get("Type").(pdfName); ty == "Catalog" {
			rec.catalog = r
		}
	case *pdfStream:
		switch ty, _ := t.dict.get("Type").(pdfName); ty {
		case "XRef":
			rec.trailers = append(rec.trailers, t.dict)
		case "ObjStm":
			return d.defineCompressed(t, start, rec)
		}
	}
	return nil
}

// isEncryptDict recognises an encryption dictionary by its shape. If the trailer
// that named it was lost, the strings and streams are still ciphertext, and
// rewriting them as they are would pass encrypted content off as cleaned.
func isEncryptDict(d *pdfDict) bool {
	return d.has("Filter") && d.has("O") && d.has("U")
}

func (d *document) defineCompressed(s *pdfStream, start int, rec *recovery) error {
	stm, err := d.decodeObjectStream(s)
	if isFinal(err) {
		return err
	}
	if err != nil {
		return nil // an unreadable stream defines nothing; the recovery warning covers it
	}
	d.objStms[start] = stm
	for i, num := range stm.numbers {
		d.define(num, xrefEntry{kind: entryCompressed, offset: start, container: -1, index: i})
		delete(d.cache, num)
		val, _, err := d.lookup(pdfRef{num: num})
		if isFinal(err) {
			return err
		}
		if dict, ok := val.(*pdfDict); ok && err == nil {
			if isEncryptDict(dict) {
				return encryptedError()
			}
			if ty, _ := dict.get("Type").(pdfName); ty == "Catalog" {
				rec.catalog = pdfRef{num: num}
			}
		}
	}
	return nil
}

// objectHeaderBefore reads the "N G " in front of an "obj" keyword at o, and says
// where it begins.
func (d *document) objectHeaderBefore(o int) (num, gen, start int, ok bool) {
	if end := o + len("obj"); end < len(d.data) && isRegular(d.data[end]) {
		return 0, 0, 0, false
	}
	gen, i, ok := d.numberBefore(o)
	if !ok || gen > 65535 {
		return 0, 0, 0, false
	}
	num, start, ok = d.numberBefore(i)
	if !ok || num > maxObjectNumber {
		return 0, 0, 0, false
	}
	return num, gen, start, true
}

// numberBefore reads the digits that end, after some whitespace, just before
// position end, and says where they begin. Whitespace is capped because this runs
// for every "obj" in the file, and a long run in front of each would add up.
func (d *document) numberBefore(end int) (n, start int, ok bool) {
	const maxSpace = 64
	i := end
	for i > 0 && end-i < maxSpace && isSpace(d.data[i-1]) {
		i--
	}
	if i == end {
		return 0, 0, false
	}
	stop := i
	for i > 0 && stop-i <= 10 && isDigit(d.data[i-1]) {
		i--
	}
	n, ok = parseUnsigned(d.data[i:stop])
	return n, i, ok
}

// adoptRecoveredRoot takes the newest trailer's /Root if that leads to a usable
// catalog, and otherwise the last catalog in the file.
func (d *document) adoptRecoveredRoot(lastCatalog pdfRef) error {
	if err := d.adoptTrailers(); err != nil {
		if isFinal(err) {
			return err
		}
		d.root = pdfRef{num: -1}
	}
	if d.root.num >= 0 {
		if val, _, err := d.lookup(d.root); err == nil {
			if dict, ok := val.(*pdfDict); ok && dict.has("Pages") {
				return nil
			}
		}
	}
	if lastCatalog.num < 0 {
		return core.Malformedf("no cross-reference data and no document catalog could be recovered")
	}
	d.root = lastCatalog
	return nil
}
