package pdf

import (
	"bytes"
	"maps"
	"slices"

	"github.com/axiom8z/hideme/internal/core"
)

// role is what an object is used for, which its own contents do not always say.
// Annotations need not have a /Type, and a /T means an author on a comment but a
// field name on a form. Roles are worked out before anything is cleaned, because
// which path reaches a shared object first must not change how it is cleaned.
type role uint16

const (
	roleCatalog role = 1 << iota
	rolePage         // a node of the page tree
	roleContent      // a page's content stream
	roleAcroForm
	roleField
	roleAnnot
	roleNames
	roleThread
	roleXObject        // only ever from a stream's own keys
	roleFont           // only ever from a dictionary's own keys
	roleFontDescriptor // only ever from a dictionary's own keys
	roleAction         // only ever from a dictionary's own keys
	roleResources
	roleNameMap // keys are names the producer chose, not keys the format defines
	roleAppearance
)

// spreadRoles are the roles that follow from the key a value sits under wherever
// that happens, so the walk over the whole document hands them out.
const spreadRoles = roleAnnot | roleResources | roleNameMap | roleAppearance

// childRole gives the role a value has because of the key it sits under. It
// serves direct objects while cleaning; analyze works out the same for references.
func childRole(key string, parent role) role {
	switch key {
	case "Annots":
		return roleAnnot
	case "Popup", "IRT":
		return parent & roleAnnot
	case "Names":
		if parent&roleCatalog != 0 {
			return roleNames
		}
	case "Threads":
		if parent&roleCatalog != 0 {
			return roleThread
		}
	case "AcroForm":
		if parent&roleCatalog != 0 {
			return roleAcroForm
		}
	case "Fields":
		if parent&roleAcroForm != 0 {
			return roleField
		}
	case "Kids":
		return parent & roleField
	case "Resources", "DR":
		return roleResources
	case "Font", "XObject", "ExtGState", "ColorSpace", "Pattern", "Shading", "Properties", "Encoding":
		if parent&roleResources != 0 {
			return roleNameMap
		}
	case "Dests":
		if parent&roleCatalog != 0 {
			return roleNameMap
		}
	case "CharProcs", "RoleMap", "ClassMap", "Colorants", "Solidities", "Extensions":
		return roleNameMap
	case "AP":
		return roleAppearance
	case "N", "R", "D":
		if parent&roleAppearance != 0 {
			return roleNameMap
		}
	}
	return 0
}

// analyze validates the document's skeleton and collects what the rewrite needs
// to know up front. Failing here is what sends a file with a damaged
// cross-reference table to the recovery scan.
func (d *document) analyze() error {
	val, _, err := d.lookup(d.root)
	if err != nil {
		return err
	}
	catalog, ok := val.(*pdfDict)
	if !ok {
		return core.Malformedf("/Root is not a dictionary")
	}
	d.roles = make(map[int]role)
	d.roles[d.root.num] |= roleCatalog
	if err := d.markPageTree(catalog); err != nil {
		return err
	}
	if err := d.markForm(catalog); err != nil {
		return err
	}
	if err := d.markChain(catalog.get("Names"), roleNames); err != nil {
		return err
	}
	if err := d.markThreads(catalog); err != nil {
		return err
	}
	reached, err := d.walkReachable()
	if err != nil {
		return err
	}
	return d.countOrphans(reached)
}

// markChain gives a role to a reference and to any further references it leads
// straight to.
func (d *document) markChain(v object, r role) error {
	for range maxRefHops {
		ref, ok := v.(pdfRef)
		if !ok {
			return nil
		}
		d.roles[ref.num] |= r
		val, _, err := d.lookup(ref)
		if err != nil {
			return err
		}
		v = val
	}
	return nil
}

// markPageTree walks the page tree from the catalog. The roles it gives decide
// what is protected from removal, so they come only from this explicit walk and
// never from what an object happens to say about itself, and every link the walk
// relies on is one that survives cleaning. If the rewrite could remove a node,
// the cleaned document could lose a page tree it needed, so such a file is
// refused.
func (d *document) markPageTree(catalog *pdfDict) error {
	top := catalog.get("Pages")
	if _, ok := top.(pdfRef); !ok {
		return core.Malformedf("the catalog has no page tree")
	}
	if val, err := d.deref(top); err != nil {
		return err
	} else if _, ok := val.(*pdfDict); !ok {
		return core.Malformedf("the catalog has no page tree")
	}
	stack := []object{top}
	seen := make(map[int]bool)
	for len(stack) > 0 {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ref, ok := v.(pdfRef)
		if !ok {
			if v == nil {
				continue
			}
			return core.Malformedf("a page tree node is not an indirect object")
		}
		if seen[ref.num] {
			continue
		}
		seen[ref.num] = true
		if err := d.markChain(ref, rolePage); err != nil {
			return err
		}
		val, err := d.deref(ref)
		if err != nil {
			return err
		}
		if val == nil {
			continue
		}
		node, ok := val.(*pdfDict)
		if !ok || d.isRemovedShape(node) {
			return core.Malformedf("a page tree node is not a page")
		}
		if err := d.markContents(node); err != nil {
			return err
		}
		kids, err := d.deref(node.get("Kids"))
		if err != nil {
			return err
		}
		if a, ok := kids.(pdfArray); ok {
			stack = append(stack, a...)
		}
	}
	return nil
}

func (d *document) markContents(page *pdfDict) error {
	contents := page.get("Contents")
	if err := d.markChain(contents, roleContent); err != nil {
		return err
	}
	val, err := d.deref(contents)
	if err != nil {
		return err
	}
	if a, ok := val.(pdfArray); ok {
		for _, e := range a {
			if err := d.markChain(e, roleContent); err != nil {
				return err
			}
		}
	}
	return nil
}

// markThreads marks article threads, whose /I entry is an Info dictionary in all
// but name.
func (d *document) markThreads(catalog *pdfDict) error {
	threads, err := d.deref(catalog.get("Threads"))
	if err != nil {
		return err
	}
	if a, ok := threads.(pdfArray); ok {
		for _, e := range a {
			if err := d.markChain(e, roleThread); err != nil {
				return err
			}
		}
	}
	return nil
}

// markForm walks the AcroForm field tree, which is what tells a field's /T apart
// from an annotation author's.
func (d *document) markForm(catalog *pdfDict) error {
	form := catalog.get("AcroForm")
	if err := d.markChain(form, roleAcroForm); err != nil {
		return err
	}
	val, err := d.deref(form)
	if err != nil {
		return err
	}
	dict, ok := val.(*pdfDict)
	if !ok {
		return nil
	}
	fields, err := d.deref(dict.get("Fields"))
	if err != nil {
		return err
	}
	top, _ := fields.(pdfArray)
	stack := slices.Clone(top)
	seen := make(map[int]bool)
	for len(stack) > 0 {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ref, ok := v.(pdfRef); ok {
			if seen[ref.num] {
				continue
			}
			seen[ref.num] = true
		}
		if err := d.markChain(v, roleField); err != nil {
			return err
		}
		val, err := d.deref(v)
		if err != nil {
			return err
		}
		node, ok := val.(*pdfDict)
		if !ok {
			continue
		}
		kids, err := d.deref(node.get("Kids"))
		if err != nil {
			return err
		}
		if a, ok := kids.(pdfArray); ok {
			stack = append(stack, a...)
		}
	}
	return nil
}

// walkReachable visits everything the original document can reach, parsing it.
// Any object that fails to parse here fails the document, because a rewrite that
// silently lost it could damage the content. It also spreads spreadRoles, such as
// roleAnnot along /Annots, /Popup and /IRT.
//
// An object is revisited when it gains a role, which bounds the walk at a few
// visits per object however the graph is arranged.
func (d *document) walkReachable() (map[int]bool, error) {
	reached := make(map[int]bool)
	queue := []int{}
	mark := func(r pdfRef, given role) {
		if e, ok := d.entries[r.num]; !ok || e.kind == entryFree || e.gen != r.gen {
			return // a dangling reference reads as null and reaches nothing
		}
		gained := given&^d.roles[r.num] != 0
		d.roles[r.num] |= given
		if !reached[r.num] || gained {
			reached[r.num] = true
			queue = append(queue, r.num)
		}
	}
	var walk func(v object, given role)
	walk = func(v object, given role) {
		switch t := v.(type) {
		case pdfRef:
			mark(t, given)
		case pdfArray:
			for _, e := range t {
				walk(e, given)
			}
		case *pdfStream:
			walk(t.dict, given)
		case *pdfDict:
			if d.isSignature(t) {
				d.hasSignature = true
			}
			for _, e := range t.entries {
				walk(e.val, childRole(e.key, given)&spreadRoles)
			}
		}
	}
	mark(d.root, 0)
	if r, ok := d.info.(pdfRef); ok {
		mark(r, 0)
	} else {
		walk(d.info, 0)
	}
	walk(d.id, 0)
	for len(queue) > 0 {
		if err := d.ctx.Err(); err != nil {
			return nil, err
		}
		num := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		e := d.entries[num]
		val, exists, err := d.lookup(pdfRef{num, e.gen})
		if err != nil {
			return nil, err
		}
		if exists {
			walk(val, d.roles[num]&(spreadRoles|roleCatalog))
		}
	}
	return reached, nil
}

// countOrphans counts in-use objects the document can no longer reach, which are
// where deleted content tends to survive. Machinery that is unreachable by design
// is left out: object streams, cross-reference streams, and the parameter
// dictionary and hint streams of a linearized file.
func (d *document) countOrphans(reached map[int]bool) error {
	var orphans []int
	for _, num := range slices.Sorted(maps.Keys(d.entries)) {
		if d.entries[num].kind != entryFree && !reached[num] {
			orphans = append(orphans, num)
		}
	}
	hintOffsets := make(map[int]bool)
	linearized := false
	for _, num := range orphans {
		e := d.entries[num]
		val, _, err := d.lookup(pdfRef{num, e.gen})
		if isFinal(err) {
			return err
		}
		if dict, ok := val.(*pdfDict); ok && err == nil && dict.has("Linearized") {
			linearized = true
			if h, ok := d.resolve(dict.get("H")).(pdfArray); ok {
				for i := 0; i < len(h); i += 2 {
					if off, ok := d.resolve(h[i]).(int64); ok && off >= 0 && off <= int64(len(d.data)) {
						hintOffsets[int(off)] = true
					}
				}
			}
		}
	}
	for _, num := range orphans {
		e := d.entries[num]
		val, _, err := d.lookup(pdfRef{num, e.gen})
		if isFinal(err) {
			return err
		}
		if hintOffsets[e.offset] && e.kind == entryInUse {
			continue
		}
		switch t := val.(type) {
		case *pdfStream:
			if ty, _ := t.dict.get("Type").(pdfName); ty == "ObjStm" || ty == "XRef" {
				continue
			}
		case *pdfDict:
			if t.has("Linearized") {
				continue
			}
		}
		d.orphans++
	}
	if linearized {
		// A linearized file's own structure is two sections, not two revisions.
		d.sections--
	}
	return nil
}

// isSignature recognises a signature dictionary by its shape. /Type is optional
// on these, so the other keys that only signatures have must count too.
func (d *document) isSignature(x *pdfDict) bool {
	t := d.nameOf(x, "Type")
	return t == "Sig" || t == "DocTimeStamp" || x.has("ByteRange") ||
		(x.has("SubFilter") && x.has("Contents"))
}

// isRemovedShape reports whether cleaning always drops a dictionary, because of
// what it is, wherever it is found: a signature or an attachment.
func (d *document) isRemovedShape(x *pdfDict) bool {
	return d.isSignature(x) || x.has("EF") || x.has("RF") || d.nameOf(x, "Subtype") == "FileAttachment"
}

// trailingBytes returns how much non-whitespace follows the final %%EOF.
func (d *document) trailingBytes() int {
	i := bytes.LastIndex(d.data, []byte("%%EOF"))
	if i < 0 {
		return 0
	}
	return len(bytes.Trim(d.data[i+len("%%EOF"):], "\x00\t\n\f\r "))
}
