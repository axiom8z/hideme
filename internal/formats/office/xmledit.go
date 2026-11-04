package office

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	nsXML   = "http://www.w3.org/XML/1998/namespace"
	nsXMLNS = "xmlns" // marks a namespace declaration; never a real namespace

	// maxXMLDepth bounds the element stack. Real documents stay under a few
	// hundred levels.
	maxXMLDepth = 10000

	// ctxCheckEvery is how many tokens pass between cancellation checks.
	ctxCheckEvery = 1 << 16
)

// xmlElement is what a visitor sees for each start tag, outside any subtree that
// was dropped. It is valid only during the call.
type xmlElement struct {
	space, local string
	attrs        []xmlAttr

	ed      *xmlEditor
	dropped bool
	content *string // replacement for everything between the tags
	edited  bool
}

func (e *xmlElement) is(space, local string) bool { return e.space == space && e.local == local }

// depth is 0 for the root element.
func (e *xmlElement) depth() int { return len(e.ed.stack) }

// offset is the byte position of the tag, a stable identity across passes.
func (e *xmlElement) offset() int { return e.ed.scan.tok.start }

// ancestor returns the local name and offset of the element n levels above this
// one's parent, so 0 is the parent itself.
func (e *xmlElement) ancestor(n int) (local string, start int, ok bool) {
	i := len(e.ed.stack) - 1 - n
	if i < 0 {
		return "", 0, false
	}
	return e.ed.stack[i].local, e.ed.stack[i].start, true
}

// parent returns the enclosing element's name, or nothing for the root.
func (e *xmlElement) parent() (space, local string) {
	if len(e.ed.stack) == 0 {
		return "", ""
	}
	top := e.ed.stack[len(e.ed.stack)-1]
	return top.space, top.local
}

// enclosing returns the offset of the innermost open element with this name.
func (e *xmlElement) enclosing(space, local string) (int, bool) {
	for i := len(e.ed.stack) - 1; i >= 0; i-- {
		if frame := e.ed.stack[i]; frame.space == space && frame.local == local {
			return frame.start, true
		}
	}
	return 0, false
}

// value returns an attribute's decoded value.
func (e *xmlElement) value(a *xmlAttr) string {
	return decodeEntities(e.ed.scan.data[a.valStart:a.valEnd])
}

// attr returns the value of the named attribute.
func (e *xmlElement) attr(space, local string) (string, bool) {
	for i := range e.attrs {
		if a := &e.attrs[i]; a.space == space && a.local == local {
			return e.value(a), true
		}
	}
	return "", false
}

// drop removes the element and its whole subtree.
func (e *xmlElement) drop() { e.dropped = true }

// dropAttr removes one attribute, leaving the rest of the tag byte for byte.
func (e *xmlElement) dropAttr(a *xmlAttr) {
	a.drop = true
	e.edited = true
}

// setAttr replaces one attribute's value.
func (e *xmlElement) setAttr(a *xmlAttr, value string) {
	a.replace = true
	a.newValue = value
	e.edited = true
}

// setContent replaces everything between the tags with text. On a self-closing
// element there is nothing to replace.
func (e *xmlElement) setContent(text string) {
	e.content = &text
}

// leafText returns the element's text and true, or false if it has child elements
// or is damaged. Stopping at the first child keeps this cheap, however deeply a
// hostile file nests elements of a name the visitor cares about.
func (e *xmlElement) leafText() (string, bool) {
	tok := &e.ed.scan.tok
	if tok.selfClosing {
		return "", true
	}
	sc := xmlScanner{data: e.ed.scan.data, pos: tok.end}
	var b strings.Builder
	for {
		more, err := sc.next()
		if err != nil || !more {
			return "", false
		}
		switch sc.tok.kind {
		case tokenText:
			b.WriteString(decodeEntities(sc.data[sc.tok.start:sc.tok.end]))
		case tokenCDATA:
			b.Write(sc.data[sc.tok.bodyStart:sc.tok.bodyEnd])
		case tokenStart, tokenDoctype:
			return "", false
		case tokenEnd:
			return b.String(), true
		}
	}
}

// subtreeEnd returns the offset just past the element's end tag, or false if the
// document ends first. Callers drop the element afterwards, so a document's total
// work stays linear in its size.
func (e *xmlElement) subtreeEnd() (int, bool) {
	tok := &e.ed.scan.tok
	if tok.selfClosing {
		return tok.end, true
	}
	sc := xmlScanner{data: e.ed.scan.data, pos: tok.end}
	depth := 1
	for {
		more, err := sc.next()
		if err != nil || !more {
			return 0, false
		}
		switch {
		case sc.tok.kind == tokenStart && !sc.tok.selfClosing:
			depth++
		case sc.tok.kind == tokenEnd:
			depth--
			if depth == 0 {
				return sc.tok.end, true
			}
		}
	}
}

// subtreeRaw returns the element's bytes from start tag to end tag, or nil if it
// is damaged.
func (e *xmlElement) subtreeRaw() []byte {
	end, ok := e.subtreeEnd()
	if !ok {
		return nil
	}
	return e.ed.scan.data[e.ed.scan.tok.start:end]
}

// subtreeText returns the text of the element and its descendants, joined with
// spaces. It is for reports, so it stops at limit bytes and ignores damage.
func (e *xmlElement) subtreeText(limit int) string {
	tok := &e.ed.scan.tok
	if tok.selfClosing {
		return ""
	}
	sc := xmlScanner{data: e.ed.scan.data, pos: tok.end}
	depth := 1
	var b strings.Builder
	for b.Len() < limit {
		more, err := sc.next()
		if err != nil || !more {
			break
		}
		switch sc.tok.kind {
		case tokenText, tokenCDATA:
			body := sc.data[sc.tok.start:sc.tok.end]
			if sc.tok.kind == tokenCDATA {
				body = sc.data[sc.tok.bodyStart:sc.tok.bodyEnd]
			}
			if !isBlank(body) {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(strings.TrimSpace(decodeEntities(body)))
			}
		case tokenStart:
			if !sc.tok.selfClosing {
				depth++
			}
		case tokenEnd:
			depth--
			if depth == 0 {
				return b.String()
			}
		}
	}
	return b.String()
}

// xmlEdit is the result of running a visitor over a document.
type xmlEdit struct {
	out          []byte
	comments     int
	firstComment string
	instructions int
	foreign      int // pieces of markup removed under a markupPolicy
}

type xmlFrame struct {
	name         []byte
	space, local string
	nsTop        int
	start        int
	hasChild     bool
}

type nsBinding struct {
	prefix, uri string
	shadows     int // index of the binding of this prefix it hides, or -1
}

type xmlEditor struct {
	ctx   context.Context
	scan  xmlScanner
	out   []byte
	stack []xmlFrame
	ns    []nsBinding
	inner map[string]int // for each prefix in scope, its innermost binding in ns
	names map[string]string
	el    xmlElement
	res   xmlEdit

	// skipDepth is the stack height at which the element being skipped sits, or
	// -1 when nothing is being skipped. keepEnd says its end tag is written,
	// because only its content is being replaced.
	skipDepth int
	keepEnd   bool
	seenRoot  bool
	doctype   bool
	keep      bool // whether anything will read the output

	policy       *markupPolicy // nil, or off once the root turns out to be a vendor's
	textRunEnd   int           // where the run of text strayText last read ended
	textRunStray bool
}

// editXML runs visit over every element and returns the edited document. Text,
// CDATA and untouched tags are copied byte for byte, so the only differences from
// the input are what visit asked for and the removal of comments and processing
// instructions, which no reader needs and which can hold anything.
func editXML(ctx context.Context, data []byte, visit func(*xmlElement) error) (xmlEdit, error) {
	return runXML(ctx, data, visit, true)
}

// scanXML runs visit over every element and keeps no output, for a pass that only reads
// a part or for Inspect. Building an edited copy of a large sheet costs a second sheet.
func scanXML(ctx context.Context, data []byte, visit func(*xmlElement) error) (xmlEdit, error) {
	return runXML(ctx, data, visit, false)
}

func runXML(ctx context.Context, data []byte, visit func(*xmlElement) error, keep bool) (xmlEdit, error) {
	return runXMLPolicy(ctx, data, visit, keep, nil)
}

func runXMLPolicy(ctx context.Context, data []byte, visit func(*xmlElement) error, keep bool, policy *markupPolicy) (xmlEdit, error) {
	data, err := normalizeXML(data)
	if err != nil {
		return xmlEdit{}, err
	}
	var out []byte
	if keep {
		out = make([]byte, 0, len(data))
	}
	ed := &xmlEditor{
		ctx:       ctx,
		scan:      xmlScanner{data: data},
		out:       out,
		keep:      keep,
		inner:     map[string]int{},
		names:     map[string]string{},
		skipDepth: -1,
		policy:    policy,
	}
	ed.el.ed = ed
	if bytes.HasPrefix(data, []byte("\xEF\xBB\xBF")) {
		ed.emit(data[:3])
		ed.scan.pos = 3
	}
	prologEnd := ed.scan.pos
	for tokens := 0; ; tokens++ {
		if tokens%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return xmlEdit{}, err
			}
		}
		more, err := ed.scan.next()
		if err != nil {
			return xmlEdit{}, err
		}
		if !more {
			break
		}
		if err := ed.step(prologEnd, visit); err != nil {
			return xmlEdit{}, err
		}
	}
	if len(ed.stack) > 0 {
		return xmlEdit{}, scanError(len(data), fmt.Sprintf("element %q is never closed", ed.stack[len(ed.stack)-1].name))
	}
	if !ed.seenRoot {
		return xmlEdit{}, core.Malformedf("XML has no root element")
	}
	ed.res.out = ed.out
	return ed.res, nil
}

func (ed *xmlEditor) emit(b []byte) {
	if ed.keep {
		ed.out = append(ed.out, b...)
	}
}

func (ed *xmlEditor) step(prologEnd int, visit func(*xmlElement) error) error {
	tok := &ed.scan.tok
	raw := ed.scan.data[tok.start:tok.end]
	if ed.doctype && ed.skipDepth < 0 && (tok.kind == tokenText || tok.kind == tokenStart) && hasUnknownEntity(raw) {
		return core.Unsupportedf("XML at byte %d: an entity only the DTD could define", tok.start)
	}
	switch tok.kind {
	case tokenText, tokenCDATA:
		if len(ed.stack) == 0 && (tok.kind == tokenCDATA || !isBlank(raw)) {
			return scanError(tok.start, "text outside the root element")
		}
		if ed.skipDepth < 0 {
			if ed.policy != nil && len(ed.stack) > 0 {
				body := raw
				if tok.kind == tokenCDATA {
					body = ed.scan.data[tok.bodyStart:tok.bodyEnd]
				}
				if ed.strayText(body) {
					ed.res.foreign++
					return nil
				}
			}
			ed.emit(raw)
		}
	case tokenComment:
		if ed.skipDepth < 0 {
			if ed.res.comments == 0 {
				ed.res.firstComment = string(ed.scan.data[tok.bodyStart:tok.bodyEnd])
			}
			ed.res.comments++
		}
	case tokenInstruction:
		return ed.instruction(prologEnd, raw)
	case tokenDoctype:
		if ed.seenRoot {
			return scanError(tok.start, "DOCTYPE after the root element")
		}
		ed.doctype = true
		ed.res.instructions++
	case tokenStart:
		return ed.start(visit)
	case tokenEnd:
		return ed.end(raw)
	}
	return nil
}

// instruction keeps the XML declaration, the only instruction that means anything
// to a reader of these parts. Elsewhere the name xml is reserved and an
// instruction spelled that way is damage.
func (ed *xmlEditor) instruction(prologEnd int, raw []byte) error {
	tok := &ed.scan.tok
	target := ed.scan.data[tok.bodyStart:tok.bodyEnd]
	if i := bytes.IndexFunc(target, func(r rune) bool { return r < 0x80 && isSpace(byte(r)) }); i >= 0 {
		target = target[:i]
	}
	if bytes.EqualFold(target, []byte("xml")) {
		if tok.start != prologEnd {
			return scanError(tok.start, "XML declaration is not at the start of the document")
		}
		ed.emit(raw)
		return nil
	}
	if ed.skipDepth < 0 {
		ed.res.instructions++
	}
	return nil
}

func (ed *xmlEditor) start(visit func(*xmlElement) error) error {
	tok := &ed.scan.tok
	if len(ed.stack) == 0 {
		if ed.seenRoot {
			return scanError(tok.start, "more than one root element")
		}
		ed.seenRoot = true
	}
	if len(ed.stack) >= maxXMLDepth {
		return fmt.Errorf("%w: XML nested more than %d levels deep", core.ErrTooLarge, maxXMLDepth)
	}
	if ed.skipDepth >= 0 {
		if !tok.selfClosing {
			ed.stack = append(ed.stack, xmlFrame{name: tok.name, nsTop: len(ed.ns)})
		}
		return nil
	}
	nsTop := len(ed.ns)
	if err := ed.resolve(); err != nil {
		return err
	}
	el := &ed.el
	*el = xmlElement{space: el.space, local: el.local, attrs: tok.attrs, ed: ed}
	if len(ed.stack) == 0 && ed.policy != nil && !ed.policy.known(el.space) {
		ed.policy = nil
	}
	if len(ed.stack) > 0 {
		ed.stack[len(ed.stack)-1].hasChild = true
	}
	if err := visit(el); err != nil {
		return err
	}
	if !el.dropped && ed.policy != nil && ed.applyPolicy(el) {
		el.drop()
	}
	if el.dropped {
		if tok.selfClosing {
			ed.popNamespaces(nsTop)
			return nil
		}
		ed.skipDepth, ed.keepEnd = len(ed.stack), false
	} else {
		ed.writeTag(el)
		if !tok.selfClosing && el.content != nil {
			ed.emit([]byte(escapeXML(*el.content)))
			ed.skipDepth, ed.keepEnd = len(ed.stack), true
		}
	}
	if tok.selfClosing {
		ed.popNamespaces(nsTop)
		return nil
	}
	ed.stack = append(ed.stack, xmlFrame{name: tok.name, space: el.space, local: el.local, nsTop: nsTop, start: tok.start})
	return nil
}

// resolve pushes this tag's namespace declarations and resolves its names. An
// undeclared prefix is an error: a renamed or missing binding must not be a way to
// slip an attribute past rules that identify attributes by namespace.
func (ed *xmlEditor) resolve() error {
	tok := &ed.scan.tok
	data := ed.scan.data
	for i := range tok.attrs {
		a := &tok.attrs[i]
		prefix, local, hasPrefix := bytes.Cut(a.name, []byte(":"))
		switch {
		case !hasPrefix && string(prefix) == "xmlns":
			ed.pushNamespace("", decodeEntities(data[a.valStart:a.valEnd]))
			a.space = nsXMLNS
		case hasPrefix && string(prefix) == "xmlns":
			ed.pushNamespace(ed.intern(local), decodeEntities(data[a.valStart:a.valEnd]))
			a.space = nsXMLNS
		}
	}
	for i := range tok.attrs {
		a := &tok.attrs[i]
		if a.space == nsXMLNS {
			continue
		}
		prefix, local, hasPrefix := bytes.Cut(a.name, []byte(":"))
		if !hasPrefix {
			a.local = ed.intern(prefix)
			continue
		}
		uri, ok := ed.lookup(prefix)
		if !ok {
			return scanError(a.start, fmt.Sprintf("undeclared namespace prefix in attribute %q", a.name))
		}
		a.space, a.local = uri, ed.intern(local)
	}
	prefix, local, hasPrefix := bytes.Cut(tok.name, []byte(":"))
	if !hasPrefix {
		uri, _ := ed.lookup(nil)
		ed.el.space, ed.el.local = uri, ed.intern(prefix)
		return nil
	}
	uri, ok := ed.lookup(prefix)
	if !ok {
		return scanError(tok.start, fmt.Sprintf("undeclared namespace prefix in element %q", tok.name))
	}
	ed.el.space, ed.el.local = uri, ed.intern(local)
	return nil
}

// pushNamespace and popNamespaces keep lookup constant-time. Searching the bindings
// in scope would let a small file declare a million prefixes and then charge every
// one of its millions of tags for reading past them.
func (ed *xmlEditor) pushNamespace(prefix, uri string) {
	shadows, ok := ed.inner[prefix]
	if !ok {
		shadows = -1
	}
	ed.inner[prefix] = len(ed.ns)
	ed.ns = append(ed.ns, nsBinding{prefix, uri, shadows})
}

func (ed *xmlEditor) popNamespaces(top int) {
	for i := len(ed.ns) - 1; i >= top; i-- {
		if b := ed.ns[i]; b.shadows < 0 {
			delete(ed.inner, b.prefix)
		} else {
			ed.inner[b.prefix] = b.shadows
		}
	}
	ed.ns = ed.ns[:top]
}

func (ed *xmlEditor) lookup(prefix []byte) (string, bool) {
	if i, ok := ed.inner[string(prefix)]; ok {
		return ed.ns[i].uri, true
	}
	if string(prefix) == "xml" {
		return nsXML, true
	}
	return "", len(prefix) == 0
}

// intern avoids allocating a string for every tag of a sheet with millions of
// cells, whose names repeat endlessly.
func (ed *xmlEditor) intern(name []byte) string {
	if s, ok := ed.names[string(name)]; ok {
		return s
	}
	s := string(name)
	ed.names[s] = s
	return s
}

// writeTag copies the start tag, applying attribute edits.
func (ed *xmlEditor) writeTag(el *xmlElement) {
	tok := &ed.scan.tok
	data := ed.scan.data
	if !el.edited {
		ed.emit(data[tok.start:tok.end])
		return
	}
	pos := tok.start
	for i := range tok.attrs {
		a := &tok.attrs[i]
		switch {
		case a.drop:
			ed.emit(data[pos:a.start])
			pos = a.end
		case a.replace:
			ed.emit(data[pos:a.valStart])
			ed.emit([]byte(escapeXML(a.newValue)))
			pos = a.valEnd
		}
	}
	ed.emit(data[pos:tok.end])
}

func (ed *xmlEditor) end(raw []byte) error {
	tok := &ed.scan.tok
	if len(ed.stack) == 0 {
		return scanError(tok.start, fmt.Sprintf("end tag %q with nothing open", tok.name))
	}
	frame := ed.stack[len(ed.stack)-1]
	if !bytes.Equal(frame.name, tok.name) {
		return scanError(tok.start, fmt.Sprintf("end tag %q closes element %q", tok.name, frame.name))
	}
	ed.stack = ed.stack[:len(ed.stack)-1]
	ed.popNamespaces(frame.nsTop)
	if ed.skipDepth >= 0 {
		if len(ed.stack) != ed.skipDepth {
			return nil
		}
		ed.skipDepth = -1
		if !ed.keepEnd {
			return nil
		}
	}
	ed.emit(raw)
	return nil
}
