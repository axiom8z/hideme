package svg

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// binding is one in-scope namespace declaration. An empty prefix is the default
// namespace.
type binding struct {
	prefix string
	uri    string
}

// openElement is an element the scan is inside, how many bindings to drop when it
// closes, and what its character data is for.
type openElement struct {
	name      string
	mark      int
	holdsText bool // something can draw or use characters directly inside it
	isStyle   bool
	// isForeign marks a foreignObject: what it draws is up to the browser, and a file
	// can make sure it never draws at all, so its text cannot be vouched for.
	isForeign bool
}

// warning kinds are issued once each, so a hostile file cannot flood the output.
type warning uint8

const (
	warnDoctypeSubset warning = iota
	warnDoctypeExternal
	warnLocalLink
	warnDataURI
	warnOpaqueImage
	warnUnsupportedImage
	warnForeignMarkup
	warnScript
	warnCSSComment
	warnDataAttributes
	warningKinds
)

// cleaner walks the tokens of one file, copying what stays into out.
type cleaner struct {
	ctx      context.Context
	opts     core.Options
	data     []byte
	s        scanner
	out      []byte
	rep      *core.Report
	bindings []binding
	stack    []openElement
	entities map[string]string
	warned   [warningKinds]bool
	findings int
	further  int
	// editorAttrs counts attributes with no finding of their own. Editors write
	// thousands of them, which would drown the report.
	editorAttrs int
	tokens      int
	// keepDataAttrs says a script or a style sheet may read data- attributes.
	keepDataAttrs bool
}

// newCleaner prepares a walk. keepDataAttrs says whether data- attributes stay, which
// analyse decides.
func newCleaner(ctx context.Context, data []byte, opts core.Options, keepDataAttrs bool) *cleaner {
	return &cleaner{
		ctx:  ctx,
		opts: opts,
		data: data,
		s:    scanner{data: data},
		out:  make([]byte, 0, len(data)),
		rep:  &core.Report{Format: "svg"},

		keepDataAttrs: keepDataAttrs,
	}
}

func (c *cleaner) run() error {
	if err := c.prolog(); err != nil {
		return err
	}
	if err := c.content(); err != nil {
		return err
	}
	c.epilog()
	c.finishFindings()
	return nil
}

// tick honours cancellation without paying for a check on every token.
func (c *cleaner) tick() error {
	c.tokens++
	if c.tokens%contextCheckInterval == 1 {
		return c.ctx.Err()
	}
	return nil
}

func (c *cleaner) copySpace() {
	end := c.s.skipSpace(c.s.pos)
	c.out = append(c.out, c.data[c.s.pos:end]...)
	c.s.pos = end
}

// prolog handles everything before the root element, then the root's start tag.
func (c *cleaner) prolog() error {
	if bytes.HasPrefix(c.data, utf8BOM) {
		c.out = append(c.out, utf8BOM...)
		c.s.pos = len(utf8BOM)
	}
	first, sawDoctype := true, false
	for {
		c.copySpace()
		tok, err := c.s.next()
		if err != nil {
			return err
		}
		switch tok.kind {
		case tokPI:
			if tok.name == "xml" {
				if !first {
					return c.s.errorf(tok.start, "XML declaration is not at the start of the file")
				}
				if err := c.declaration(tok); err != nil {
					return err
				}
				c.keep(tok)
			} else {
				c.instruction(tok)
			}
		case tokComment:
			c.comment(tok)
		case tokDoctype:
			if sawDoctype {
				return c.s.errorf(tok.start, "second DOCTYPE")
			}
			sawDoctype = true
			c.doctype(tok)
		case tokStart:
			return c.startElement(tok)
		default:
			return c.s.errorf(tok.start, "unexpected content before the root element")
		}
		first = false
	}
}

// content walks from the root's start tag to its end tag.
func (c *cleaner) content() error {
	for len(c.stack) > 0 {
		if err := c.tick(); err != nil {
			return err
		}
		tok, err := c.s.next()
		if err != nil {
			return err
		}
		switch tok.kind {
		case tokEOF:
			return c.s.errorf(tok.start, "element %q is never closed", c.stack[len(c.stack)-1].name)
		case tokText, tokCDATA:
			c.characterData(tok)
		case tokComment:
			c.comment(tok)
		case tokPI:
			if tok.name == "xml" {
				return c.s.errorf(tok.start, "XML declaration is not at the start of the file")
			}
			c.instruction(tok)
		case tokDoctype:
			return c.s.errorf(tok.start, "DOCTYPE inside the root element")
		case tokStart:
			if err := c.startElement(tok); err != nil {
				return err
			}
		case tokEnd:
			if err := c.endElement(tok); err != nil {
				return err
			}
		}
	}
	return nil
}

// epilog handles what follows the root element. Whitespace stays, comments and
// instructions go, and anything else has no place in the file, so it and all that
// follows are dropped, like bytes past the end of any other format.
func (c *cleaner) epilog() {
	for {
		c.copySpace()
		start := c.s.pos
		if start >= len(c.data) {
			return
		}
		rest := c.data[start:]
		var tok token
		var err error
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			tok, err = c.s.comment()
		case bytes.HasPrefix(rest, []byte("<?")):
			tok, err = c.s.instruction()
		default:
			err = c.s.errorf(start, "not a comment or instruction")
		}
		if err != nil {
			c.add(finding{"Trailing data", core.CatHidden, core.RiskHigh}, strconv.Itoa(len(rest))+" bytes", "file")
			c.s.pos = len(c.data)
			return
		}
		if tok.kind == tokComment {
			c.comment(tok)
		} else {
			c.instruction(tok)
		}
	}
}

func (c *cleaner) keep(tok token) {
	c.out = append(c.out, c.data[tok.start:tok.end]...)
}

// characterData keeps text where something can use it. Anywhere else nothing draws it,
// so it can only be there for someone reading the file.
func (c *cleaner) characterData(tok token) {
	body := c.data[tok.bodyStart:tok.bodyEnd]
	top := c.stack[len(c.stack)-1]
	if !top.holdsText && c.s.skipSpaceIn(body) < len(body) {
		c.add(finding{"Hidden text", core.CatHidden, core.RiskMed}, clipped(body), "")
		return
	}
	c.noteDataURI(body)
	if top.isStyle {
		c.noteStyleSheet(body)
	}
	if top.isForeign && c.s.skipSpaceIn(body) < len(body) {
		c.warn(warnForeignMarkup, foreignMarkupWarning)
	}
	c.keep(tok)
}

// declaration checks the XML declaration holds only the three pseudo-attributes
// XML defines, in an encoding the scanner can read byte by byte. In a multi-byte
// encoding such as Shift-JIS, a byte equal to '<' might not be a '<' at all.
func (c *cleaner) declaration(tok token) error {
	s := scanner{data: c.data[:tok.end]}
	if _, _, err := s.scanAttributes(tok.bodyStart, "?>", "?>"); err != nil {
		return err
	}
	for _, a := range s.attrs {
		value := string(c.data[a.valueStart:a.valueEnd])
		switch a.name {
		case "version":
			if value != "1.0" {
				return core.Unsupportedf("svg: XML version %q is not supported", value)
			}
		case "encoding":
			if !strings.EqualFold(value, "utf-8") && !strings.EqualFold(value, "us-ascii") {
				return core.Unsupportedf("svg: encoding %q is not supported", value)
			}
		case "standalone":
		default:
			return c.s.errorf(a.start, "attribute %q is not allowed in an XML declaration", a.name)
		}
	}
	return nil
}

// doctype keeps the DOCTYPE and learns its entity declarations. An internal subset
// is kept whole rather than edited, because entities in the document can refer to
// anything in it, and a warning says that it was not cleaned.
func (c *cleaner) doctype(tok token) {
	c.keep(tok)
	if tok.bodyEnd > tok.bodyStart {
		subset := c.data[tok.bodyStart:tok.bodyEnd]
		c.warn(warnDoctypeSubset, "The DOCTYPE has an internal subset, which was kept unchanged and not cleaned.")
		c.entities = parseEntities(subset)
		c.noteDataURI(subset)
	}
	external := c.data[tok.start:tok.end]
	if tok.bodyStart > 0 {
		external = c.data[tok.start:tok.bodyStart]
	}
	if quotesLocalLink(external) {
		c.warn(warnLocalLink, localLinkWarning)
	} else if !quotesOnlyStandardIDs(external) {
		c.warn(warnDoctypeExternal, "The DOCTYPE names something other than a W3C SVG document type. It was kept, and can hold any text.")
	}
}

func (c *cleaner) comment(tok token) {
	c.add(finding{"XML comment", core.CatContent, core.RiskMed}, clipped(c.data[tok.bodyStart:tok.bodyEnd]), "")
}

func (c *cleaner) instruction(tok token) {
	body := strings.TrimSpace(clipped(c.data[tok.bodyStart:tok.bodyEnd]))
	if tok.name != "xml-stylesheet" || !c.stylesheetInstruction(tok) {
		c.add(finding{"Processing instruction", core.CatHidden, core.RiskMed}, tok.name+" "+body, "")
	}
}

// stylesheetInstruction keeps a link to a style sheet, as what it links to affects how
// the file looks. An instruction holding anything the standard does not define is not
// a usable link, and goes.
func (c *cleaner) stylesheetInstruction(tok token) bool {
	s := scanner{data: c.data[:tok.end]}
	if _, _, err := s.scanAttributes(tok.bodyStart, "?>", "?>"); err != nil {
		return false
	}
	for _, a := range s.attrs {
		switch a.name {
		case "href", "type", "media", "charset", "alternate", "title":
		default:
			return false
		}
	}
	c.keep(tok)
	for _, a := range s.attrs {
		value := c.data[a.valueStart:a.valueEnd]
		switch a.name {
		case "href":
			if isLocalLink(decodeReferences(value)) {
				c.warn(warnLocalLink, localLinkWarning)
			}
		case "title":
			c.retain(finding{"Style sheet title", core.CatContent, core.RiskLow}, clipped(value), "xml-stylesheet",
				"it names the style sheet that it links to")
		}
	}
	return true
}

// startElement decides the fate of an element: gone with its whole subtree, or kept
// with its attributes filtered. A tag's own declarations apply to its own name and
// attributes, so they are bound first.
func (c *cleaner) startElement(tok token) error {
	mark := len(c.bindings)
	if err := c.bindNamespaces(tok); err != nil {
		return err
	}
	ns, local, err := c.elementNamespace(tok)
	if err != nil {
		return err
	}
	if len(c.stack) == 0 && ((ns != nsSVG && ns != "") || local != "svg") {
		return core.Unsupportedf("svg: the root element is in namespace %q, not SVG", ns)
	}
	switch {
	case !keptNamespaces[ns]:
		name := tok.name
		err := c.removeSubtree(tok, func(raw []byte) { c.describeEditorElement(name, ns, raw) })
		c.bindings = c.bindings[:mark]
		return err
	case isSVG(ns) && local == "metadata":
		err := c.removeSubtree(tok, c.describeMetadata)
		c.bindings = c.bindings[:mark]
		return err
	case !c.elementStays(ns, local):
		err := c.removeSubtree(tok, func(raw []byte) {
			c.add(finding{"Unknown element", core.CatHidden, core.RiskMed}, clipped(raw), "")
		})
		c.bindings = c.bindings[:mark]
		return err
	case isSVG(ns) && (local == "title" || local == "desc"):
		remove, err := c.judgeText(tok, local)
		if err != nil {
			return err
		}
		if remove {
			err := c.removeSubtree(tok, func([]byte) {})
			c.bindings = c.bindings[:mark]
			return err
		}
	case isSVG(ns) && (local == "script" || local == "handler"):
		c.warn(warnScript, scriptWarning)
	}
	if err := c.emitTag(tok, ns, local); err != nil {
		return err
	}
	if tok.selfClosing {
		c.bindings = c.bindings[:mark]
		return nil
	}
	if len(c.stack) >= maxDepth {
		return fmt.Errorf("%w: elements nested more than %d levels deep", core.ErrTooLarge, maxDepth)
	}
	c.stack = append(c.stack, openElement{
		name: tok.name, mark: mark,
		holdsText: c.holdsText(ns, local),
		isStyle:   isSVG(ns) && local == "style",
		isForeign: isSVG(ns) && local == "foreignObject",
	})
	return nil
}

// elementStays holds kept namespaces to the elements they define. Markup that is not
// SVG stays where a foreignObject may be drawing it, but nothing here can vouch for it.
func (c *cleaner) elementStays(ns, local string) bool {
	switch ns {
	case nsNone, nsSVG:
		return svgElements[local]
	case nsXHTML, nsMathML:
		c.warn(warnForeignMarkup, foreignMarkupWarning)
		return true
	case nsEvents:
		return local == "listener"
	}
	return false
}

// holdsText reports whether characters directly inside an element can be of any use.
func (c *cleaner) holdsText(ns, local string) bool {
	switch {
	case !isSVG(ns):
		return true
	case local == "a" || local == "switch":
		return len(c.stack) > 0 && c.stack[len(c.stack)-1].holdsText
	}
	return textElements[local]
}

func (c *cleaner) endElement(tok token) error {
	if len(c.stack) == 0 {
		return c.s.errorf(tok.start, "end tag %q has no start tag", tok.name)
	}
	top := c.stack[len(c.stack)-1]
	if top.name != tok.name {
		return c.s.errorf(tok.start, "end tag %q does not match start tag %q", tok.name, top.name)
	}
	c.stack = c.stack[:len(c.stack)-1]
	c.bindings = c.bindings[:top.mark]
	c.keep(tok)
	return nil
}

// bindNamespaces records a start tag's xmlns declarations. An entity-valued one, as
// Illustrator writes, is read through the DOCTYPE's declaration. That only decides
// how to treat the prefix: the entity itself is never expanded in the output.
func (c *cleaner) bindNamespaces(tok token) error {
	for _, a := range tok.attrs {
		prefix, local := splitName(a.name)
		var bound string
		switch {
		case prefix == "xmlns":
			bound = local
		case prefix == "" && local == "xmlns":
		default:
			continue
		}
		uri := c.namespaceValue(a)
		if bound != "" && uri == "" {
			return c.s.errorf(a.start, "prefix %q is bound to an empty namespace", bound)
		}
		if len(c.bindings) >= maxBindings {
			return fmt.Errorf("%w: more than %d namespace declarations in scope", core.ErrTooLarge, maxBindings)
		}
		c.bindings = append(c.bindings, binding{prefix: bound, uri: uri})
	}
	return nil
}

// namespaceValue returns the URI an xmlns attribute declares. A value that is not a
// plain URI or a known entity gets a name no allowlist holds, which makes everything
// in that namespace removable. Failing to understand a prefix must not protect it.
func (c *cleaner) namespaceValue(a attr) string {
	value := string(c.data[a.valueStart:a.valueEnd])
	if !strings.Contains(value, "&") {
		return value
	}
	if name, ok := strings.CutPrefix(value, "&"); ok {
		if name, ok := strings.CutSuffix(name, ";"); ok {
			if uri, ok := c.entities[name]; ok && !strings.ContainsAny(uri, "&%") {
				return uri
			}
		}
	}
	return unresolvedNamespace
}

func (c *cleaner) lookup(prefix string) (string, bool) {
	if prefix == "xml" {
		return nsXML, true
	}
	for i := len(c.bindings) - 1; i >= 0; i-- {
		if c.bindings[i].prefix == prefix {
			return c.bindings[i].uri, true
		}
	}
	return "", false
}

// elementNamespace resolves a start tag's name. Without any default namespace, an
// unprefixed element has no namespace, as in files written without an xmlns, and
// is treated as SVG. An undeclared prefix means the file is not namespace-well-formed,
// and browsers refuse such a file too.
func (c *cleaner) elementNamespace(tok token) (ns, local string, err error) {
	prefix, local := splitName(tok.name)
	if prefix == "xmlns" {
		return "", "", c.s.errorf(tok.start, "element %q uses the reserved xmlns prefix", tok.name)
	}
	ns, ok := c.lookup(prefix)
	if !ok && prefix != "" {
		return "", "", c.s.errorf(tok.start, "element %q uses an undeclared prefix", tok.name)
	}
	return ns, local, nil
}

// emitTag copies a start tag, leaving out attributes that go and replacing the
// value of any embedded image.
func (c *cleaner) emitTag(tok token, ns, local string) error {
	image := isSVG(ns) && (local == "image" || local == "feImage")
	cursor := tok.start
	for _, a := range tok.attrs {
		action, err := c.judgeAttribute(a, ns, local, image)
		if err != nil {
			return err
		}
		switch {
		case action.drop:
			c.out = append(c.out, c.data[cursor:a.start]...)
			cursor = a.end
		case action.replace != nil:
			c.out = append(c.out, c.data[cursor:a.valueStart]...)
			c.out = append(c.out, action.replace...)
			cursor = a.valueEnd
		}
	}
	c.out = append(c.out, c.data[cursor:tok.end]...)
	return nil
}

// attrAction is what happens to one attribute. The zero value keeps it.
type attrAction struct {
	drop    bool
	replace []byte
}

func (c *cleaner) judgeAttribute(a attr, elementNS, element string, image bool) (attrAction, error) {
	prefix, local := splitName(a.name)
	value := c.data[a.valueStart:a.valueEnd]
	if prefix == "xmlns" || (prefix == "" && local == "xmlns") {
		uri := c.namespaceValue(a)
		if uri == "" || keptNamespaces[uri] {
			return attrAction{}, nil
		}
		c.add(finding{"Editor namespace", core.CatSoftware, core.RiskLow}, displayNamespace(uri), "")
		return attrAction{drop: true}, nil
	}
	ns := nsNone
	if prefix != "" {
		var ok bool
		if ns, ok = c.lookup(prefix); !ok {
			return attrAction{}, c.s.errorf(a.start, "attribute %q uses an undeclared prefix", a.name)
		}
		if !keptNamespaces[ns] {
			c.describeEditorAttribute(ns, local, value)
			return attrAction{drop: true}, nil
		}
	}
	if !c.attributeStays(a, ns, local, elementNS, element, value) {
		return attrAction{drop: true}, nil
	}
	return c.judgeKeptAttribute(a, ns, local, value, image)
}

// attributeStays holds an attribute to what its namespace defines. On elements that are
// not SVG, all attributes in no namespace stay, like the elements themselves.
func (c *cleaner) attributeStays(a attr, ns, local, elementNS, element string, value []byte) bool {
	known := false
	switch ns {
	case nsNone, nsSVG:
		known = !isSVG(elementNS) && ns == nsNone || svgAttributes[local]
		if owners, limited := attributeOwners[local]; limited && isSVG(elementNS) {
			known = owners[element]
		}
		if strings.HasPrefix(local, "on") && !known {
			c.warn(warnScript, scriptWarning)
			known = true
		}
		if strings.HasPrefix(local, "data-") && !known && c.keepDataAttrs {
			c.warn(warnDataAttributes, "The file has data- attributes that a script or style sheet may read. They were kept and not inspected.")
			known = true
		}
	case nsXLink:
		known = xlinkAttributes[local]
	case nsXML:
		known = xmlAttributes[local]
	case nsEvents:
		known = eventsAttributes[local]
	case nsXHTML, nsMathML:
		known = !isSVG(elementNS)
	}
	if !known {
		c.add(finding{"Unknown attribute", core.CatHidden, core.RiskMed}, a.name+"="+clipped(value), "")
		return false
	}
	if isSVG(elementNS) && (textAttributes[local] || local == "title") {
		c.retain(finding{"Attribute text", core.CatContent, core.RiskLow}, clipped(value), "", attributeTextReason)
	}
	return true
}

const attributeTextReason = "a tooltip or a screen reader can present it, like a title"

// linkAttributes are the attributes that hold a URL, in SVG and in the HTML that a
// foreignObject can carry.
var linkAttributes = setOf("href", "src", "data", "poster", "action", "cite", "longdesc", "background")

// judgeKeptAttribute handles the attributes that stay but may need attention.
func (c *cleaner) judgeKeptAttribute(a attr, ns, local string, value []byte, image bool) (attrAction, error) {
	isLink := linkAttributes[local] && (ns == nsNone || ns == nsXLink)
	if isLink && image && local == "href" {
		if replacement, handled, err := c.embeddedImage(value); handled || err != nil {
			return attrAction{replace: replacement}, err
		}
	}
	if isLink || (local == "base" && ns == nsXML) {
		if isLocalLink(decodeReferences(value)) {
			c.warn(warnLocalLink, localLinkWarning)
			c.retain(finding{"Linked file path", core.CatIdentity, core.RiskHigh}, clipped(value), local,
				"removing the link would break what it points to")
		}
	}
	c.noteDataURI(value)
	if local == "style" && ns == nsNone {
		c.noteStyleSheet(value)
	} else if !isLink {
		c.noteCSSLinks(value)
	}
	return attrAction{}, nil
}

const (
	localLinkWarning = "The file links to a local file path, which was kept and can reveal a user name."
	scriptWarning    = "The file has scripts, which were kept and not inspected."

	foreignMarkupWarning = "The file has HTML, MathML or other foreignObject content, which was kept and not inspected."
)

// noteDataURI warns about embedded data in a place this handler does not clean, such
// as an image, or a font with its own metadata, in a CSS url().
func (c *cleaner) noteDataURI(text []byte) {
	if !c.warned[warnDataURI] && holdsDataURI(text) {
		c.warn(warnDataURI, "The file embeds data in a data: URI outside an image element, such as in CSS or a link. It was kept and not cleaned.")
	}
}

// noteStyleSheet warns about what stays in a style sheet: comments, which tools sign
// with their names, and links to local files.
func (c *cleaner) noteStyleSheet(text []byte) {
	if bytes.Contains(text, []byte("/*")) {
		c.warn(warnCSSComment, "A style sheet holds a comment, which was kept.")
	}
	c.noteCSSLinks(text)
}

func (c *cleaner) noteCSSLinks(text []byte) {
	if !c.warned[warnLocalLink] && linksToLocalFile(text) {
		c.warn(warnLocalLink, localLinkWarning)
	}
}

// removeSubtree skips from a start tag past its matching end tag, handing the raw
// bytes to describe. Everything in between still goes through the scanner, because
// only proper tokens can show where the subtree really ends.
func (c *cleaner) removeSubtree(tok token, describe func(raw []byte)) error {
	end, err := skipSubtree(c.ctx, &c.s, tok)
	if err != nil {
		return err
	}
	describe(c.data[tok.start:end])
	return nil
}

// skipSubtree advances s past the end of the element whose start tag is tok, which
// s has already read.
func skipSubtree(ctx context.Context, s *scanner, tok token) (int, error) {
	if tok.selfClosing {
		return tok.end, nil
	}
	names := []string{tok.name}
	for n := 0; len(names) > 0; n++ {
		if n%contextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		t, err := s.next()
		if err != nil {
			return 0, err
		}
		switch t.kind {
		case tokEOF:
			return 0, s.errorf(t.start, "element %q is never closed", tok.name)
		case tokDoctype:
			return 0, s.errorf(t.start, "DOCTYPE inside an element")
		case tokStart:
			if !t.selfClosing {
				if len(names) >= maxDepth {
					return 0, fmt.Errorf("%w: elements nested more than %d levels deep", core.ErrTooLarge, maxDepth)
				}
				names = append(names, t.name)
			}
		case tokEnd:
			if names[len(names)-1] != t.name {
				return 0, s.errorf(t.start, "end tag %q does not match start tag %q", t.name, names[len(names)-1])
			}
			names = names[:len(names)-1]
		}
	}
	return s.pos, nil
}

// judgeText decides whether a title or desc goes. It reads ahead with its own scanner:
// if the element stays, its attributes and children still need the normal walk.
func (c *cleaner) judgeText(tok token, local string) (remove bool, err error) {
	text, err := c.elementText(tok)
	if err != nil {
		return false, err
	}
	f := finding{"Title", core.CatContent, core.RiskLow}
	if local == "desc" {
		f.field = "Description"
	}
	switch {
	case c.opts.Strict || isToolBanner(text) || isFileName(text):
		c.add(f, text, "svg")
		return true, nil
	case text != "":
		c.retain(f, text, "svg", "it can be accessibility text that describes the image")
	}
	return false, nil
}

// elementText returns an element's trimmed text, without moving the main scanner. It
// gives up after maxTextTokens, which real text never comes near: a hostile file can
// nest these elements, and each would otherwise read on past all the others. The text
// only decides what the element is called and whether it is a tool's banner.
func (c *cleaner) elementText(tok token) (string, error) {
	if tok.selfClosing {
		return "", nil
	}
	s := scanner{data: c.data, pos: tok.end}
	var text strings.Builder
	for depth, n := 1, 0; depth > 0 && n < maxTextTokens; n++ {
		t, err := s.next()
		if err != nil {
			return "", err
		}
		switch t.kind {
		case tokEOF:
			return "", s.errorf(t.start, "element %q is never closed", tok.name)
		case tokText, tokCDATA:
			// Only its own text: a child may be removed, and then a second clean must
			// reach the same verdict.
			if depth == 1 && text.Len() < maxTextBytes {
				text.Write(c.data[t.bodyStart:min(t.bodyEnd, t.bodyStart+maxTextBytes)])
			}
		case tokStart:
			if !t.selfClosing {
				depth++
			}
		case tokEnd:
			depth--
		}
	}
	return strings.TrimSpace(unescapeBasic(text.String())), nil
}

// finding describes how something appears in the report. The field and the place given
// with it are printed even by clean, which promises to print nothing from the file, so
// neither may hold a name from the file. Only the value is hidden there.
type finding struct {
	field    string
	category core.Category
	risk     core.Risk
}

// add records a removal, or counts it once the report has reached its limit.
func (c *cleaner) add(f finding, value, where string) {
	if c.findings >= maxFindings {
		c.further++
		return
	}
	c.findings++
	c.rep.Add(core.Finding{Category: f.category, Risk: f.risk, Field: f.field, Value: value, Where: where, Action: core.ActionRemove})
}

// retain records something deliberately kept. Past the report's limit it goes
// unmentioned, since it needs no action and is the same on every run.
func (c *cleaner) retain(f finding, value, where, reason string) {
	if c.findings >= maxFindings {
		return
	}
	c.findings++
	c.rep.Add(core.Finding{Category: f.category, Risk: f.risk, Field: f.field, Value: value, Where: where, Action: core.ActionRetain, Reason: reason})
}

// mergeNested folds in an embedded image's report, keeping to the same limit.
func (c *cleaner) mergeNested(nested *core.Report) {
	if c.findings >= maxFindings {
		c.further += len(nested.Residual())
		return
	}
	c.findings += len(nested.Findings)
	c.rep.Merge("embedded image", nested)
}

func (c *cleaner) warn(kind warning, message string) {
	if !c.warned[kind] {
		c.warned[kind] = true
		c.rep.Warn("%s", message)
	}
}

func (c *cleaner) finishFindings() {
	if c.editorAttrs > 0 {
		c.rep.Add(core.Finding{
			Category: core.CatSoftware, Risk: core.RiskLow, Field: "Editor attributes",
			Value: strconv.Itoa(c.editorAttrs) + " attributes", Action: core.ActionRemove,
		})
	}
	if c.further > 0 {
		c.rep.Add(core.Finding{
			Category: core.CatHidden, Risk: core.RiskMed, Field: "Further metadata",
			Value: strconv.Itoa(c.further) + " more items", Action: core.ActionRemove,
		})
	}
}

// inScopeDeclarations writes the bindings in force as attributes, innermost first
// and each prefix once, so that a metadata subtree can be parsed away from the tags
// that declared its prefixes.
func (c *cleaner) inScopeDeclarations(b *bytes.Buffer) {
	var seen []string
	for i := len(c.bindings) - 1; i >= 0; i-- {
		bound := c.bindings[i]
		if slices.Contains(seen, bound.prefix) || bound.uri == unresolvedNamespace {
			continue
		}
		seen = append(seen, bound.prefix)
		b.WriteString(" xmlns")
		if bound.prefix != "" {
			b.WriteString(":" + bound.prefix)
		}
		b.WriteString(`="`)
		b.WriteString(escapeAttr(bound.uri))
		b.WriteString(`"`)
	}
}
