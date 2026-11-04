package pdf

import (
	"context"
	"errors"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// The specification enumerates the keys of a catalog, a page, a resource dictionary,
// an image or form XObject, a font and an action, so these are allowlists: a vendor's
// private key is removed without anyone having heard of it. pdfTeX's /PTEX.FileName
// and /PTEX.InfoDict, which record the path and the metadata of every included figure,
// go this way. The other objects in a PDF, such as annotations and structure elements,
// have too many types and too many optional keys that a renderer depends on, so those
// get the named removal rules in dropEntry, and a warning for a key nobody has heard
// of, which specKeys says.
//
// The two-letter image keys are the abbreviations of inline images. They do not
// belong on an XObject, but viewers accept them there, so a file using them would
// lose its picture if they were treated as private.
var (
	catalogKeys = keySet("Type", "Version", "Extensions", "Pages", "PageLabels", "Names",
		"Dests", "ViewerPreferences", "PageLayout", "PageMode", "Outlines", "Threads",
		"OpenAction", "AA", "URI", "AcroForm", "StructTreeRoot", "MarkInfo", "Lang",
		"OCProperties", "OutputIntents", "Requirements", "NeedsRendering", "DPartRoot")
	pageKeys = keySet("Type", "Parent", "Kids", "Count", "Resources", "MediaBox", "CropBox",
		"BleedBox", "TrimBox", "ArtBox", "BoxColorInfo", "Contents", "Rotate", "Group", "B",
		"Dur", "Trans", "Annots", "AA", "StructParents", "PZ", "SeparationInfo", "Tabs",
		"TemplateInstantiated", "PresSteps", "UserUnit", "VP", "OutputIntents", "DPart")
	xobjectKeys = keySet("Type", "Subtype", "Length", "Filter", "DecodeParms", "F", "FFilter",
		"FDecodeParms", "DL", "Name", "StructParent", "StructParents", "OC", "Measure", "PtData",
		"FormType", "BBox", "Matrix", "Resources", "Group", "Ref",
		"Width", "Height", "ColorSpace", "BitsPerComponent", "Intent", "ImageMask", "Mask",
		"Decode", "Interpolate", "Alternates", "SMask", "SMaskInData", "Matte",
		"W", "H", "BPC", "CS", "D", "DP", "IM", "I")
	// /Encoding is from PDF 1.0, and form dictionaries still carry it in /DR.
	resourceKeys = keySet("ExtGState", "ColorSpace", "Pattern", "Shading", "XObject", "Font", "ProcSet", "Properties", "Encoding")
	fontKeys     = keySet("Type", "Subtype", "Name", "BaseFont", "FirstChar", "LastChar", "Widths", "FontDescriptor",
		"Encoding", "ToUnicode", "FontBBox", "FontMatrix", "CharProcs", "Resources", "DescendantFonts",
		"CIDSystemInfo", "DW", "W", "DW2", "W2", "CIDToGIDMap")
	fontDescriptorKeys = keySet("Type", "FontName", "FontFamily", "FontStretch", "FontWeight", "Flags", "FontBBox",
		"ItalicAngle", "Ascent", "Descent", "Leading", "CapHeight", "XHeight", "StemV", "StemH", "AvgWidth", "MaxWidth",
		"MissingWidth", "FontFile", "FontFile2", "FontFile3", "CharSet", "Style", "Lang", "FD", "CIDSet")
	// actionKeys is every action type's keys together. One action having another's
	// key costs nothing, and an unknown type still loses what no type has.
	actionKeys = keySet("Type", "S", "Next", "D", "SD", "F", "NewWindow", "T", "Win", "Mac", "Unix", "B", "URI", "IsMap",
		"Sound", "Volume", "Synchronous", "Repeat", "Mix", "Annotation", "Operation", "H", "N", "Fields", "Flags", "JS",
		"State", "PreserveRB", "R", "AN", "OP", "Trans", "TA", "V", "TI", "CMD", "Dp")
	actionTypes = keySet("GoTo", "GoToR", "GoToE", "GoToDP", "Launch", "Thread", "URI", "Sound", "Movie", "Hide", "Named",
		"SubmitForm", "ResetForm", "ImportData", "JavaScript", "SetOCGState", "Rendition", "Trans", "GoTo3DView", "RichMediaExecute")
	fontSubtypes = keySet("Type0", "Type1", "MMType1", "Type3", "TrueType", "CIDFontType0", "CIDFontType2")
)

// allowlistedRoles are those that have an allowlist.
const allowlistedRoles = roleCatalog | rolePage | roleXObject | roleFont | roleFontDescriptor | roleAction | roleResources

// isAllowed says whether a key is on the allowlist of any role a dictionary has.
func isAllowed(key string, r role) bool {
	return (r&roleCatalog != 0 && catalogKeys[key]) ||
		(r&rolePage != 0 && pageKeys[key]) ||
		(r&roleXObject != 0 && xobjectKeys[key]) ||
		(r&roleFont != 0 && fontKeys[key]) ||
		(r&roleFontDescriptor != 0 && fontDescriptorKeys[key]) ||
		(r&roleAction != 0 && actionKeys[key]) ||
		(r&roleResources != 0 && resourceKeys[key])
}

func keySet(keys ...string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// cleaner carries one document through the rewrite.
type cleaner struct {
	ctx  context.Context
	doc  *document
	opts core.Options
	rep  *core.Report

	numbers  map[int]int  // old object number to new
	order    []int        // old object numbers in output order
	removed  map[int]bool // decisions already made, so each is reported once
	reported map[core.Finding]bool
	warned   map[string]bool
	scans    map[*pdfStream]streamScan
	dangling int
}

// sanitize returns the document's surviving objects in output order, object 1
// first, and fills in rep.
//
// Numbers are given out in order of first reference while walking the cleaned
// objects breadth first, in dictionary key order. Run on its own output, that walk
// sees the same objects with the same keys in the same order and so gives out the
// same numbers, which is what makes cleaning idempotent.
func sanitize(ctx context.Context, doc *document, opts core.Options, rep *core.Report) ([]object, error) {
	c := &cleaner{
		ctx:      ctx,
		doc:      doc,
		opts:     opts,
		rep:      rep,
		numbers:  make(map[int]int),
		removed:  make(map[int]bool),
		reported: make(map[core.Finding]bool),
		warned:   make(map[string]bool),
		scans:    make(map[*pdfStream]streamScan),
	}
	c.reportDocumentLevel()
	if info, ok := doc.info.(pdfRef); ok && doc.roles[info.num]&(roleCatalog|rolePage) == 0 {
		c.removed[info.num] = true // anything else that points at it must not keep it alive
	}
	c.number(doc.root.num)
	var out []object
	for i := 0; i < len(c.order); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		num := c.order[i]
		val, _, err := doc.lookup(pdfRef{num, doc.entries[num].gen})
		if err != nil {
			return nil, err
		}
		cleaned, err := c.cleanTop(num, val)
		if err != nil {
			return nil, err
		}
		out = append(out, cleaned)
	}
	if c.dangling > 0 {
		rep.Warn("%d references point at objects that do not exist; they read as null and were dropped", c.dangling)
	}
	return out, nil
}

func (c *cleaner) number(old int) int {
	if n, ok := c.numbers[old]; ok {
		return n
	}
	c.order = append(c.order, old)
	n := len(c.order)
	c.numbers[old] = n
	return n
}

func objectWhere(num int) string { return fmt.Sprintf("object %d", num) }

func (c *cleaner) cleanTop(num int, val object) (object, error) {
	r := c.doc.roles[num]
	where := objectWhere(num)
	if r&roleCatalog != 0 {
		where = "Catalog"
	}
	switch t := val.(type) {
	case *pdfStream:
		return c.cleanStream(t, r, where)
	case *pdfDict:
		return c.cleanDict(t, r, where, false)
	}
	cleaned, keep, err := c.cleanValue(val, r, where)
	if err != nil || !keep {
		return nil, err
	}
	return cleaned, nil
}

// cleanValue returns a value's cleaned form, or keep=false if it is gone. A
// dictionary drops such an entry, since absent and null mean the same to a reader.
func (c *cleaner) cleanValue(v object, r role, where string) (cleaned object, keep bool, err error) {
	switch t := v.(type) {
	case pdfRef:
		val, exists, err := c.doc.lookup(t)
		if err != nil {
			return nil, false, err
		}
		if !exists {
			c.dangling++
			return nil, false, nil
		}
		gone, err := c.isRemoved(t.num, val)
		if err != nil || gone {
			return nil, false, err
		}
		return pdfRef{c.number(t.num), 0}, true, nil
	case pdfArray:
		out, err := c.cleanArray(t, r, where)
		return out, err == nil, err
	case *pdfDict:
		if c.removeDict(t, r, where) {
			return nil, false, nil
		}
		out, err := c.cleanDict(t, r, where, false)
		return out, err == nil, err
	}
	return v, true, nil
}

// cleanArray keeps positions by leaving null where an element is gone, because
// arrays are often read by index. /Annots is read as a list, and a null in it
// helps nobody, so a removed annotation just goes.
func (c *cleaner) cleanArray(a pdfArray, r role, where string) (pdfArray, error) {
	if len(a) == 2 {
		if name, _ := c.doc.resolve(a[0]).(pdfName); name == "ICCBased" {
			c.noteICCProfile()
		}
	}
	out := make(pdfArray, 0, len(a))
	for _, e := range a {
		cleaned, keep, err := c.cleanValue(e, r, where)
		if err != nil {
			return nil, err
		}
		if keep {
			out = append(out, cleaned)
		} else if r&roleAnnot == 0 {
			out = append(out, nil)
		}
	}
	return out, nil
}

// isRemoved decides, once per object, whether cleaning drops the object wherever
// it is referenced from, and reports what it held if so.
func (c *cleaner) isRemoved(num int, val object) (bool, error) {
	if gone, ok := c.removed[num]; ok {
		return gone, nil
	}
	c.removed[num] = false
	where := objectWhere(num)
	gone := false
	switch t := c.doc.resolve(val).(type) {
	case *pdfDict:
		gone = c.removeDict(t, c.doc.roles[num], where)
	case *pdfStream:
		var err error
		if gone = c.removeDict(t.dict, c.doc.roles[num], where); !gone {
			gone, err = c.removeStream(t, c.doc.roles[num], where)
			if err != nil {
				return false, err
			}
		}
	}
	c.removed[num] = gone
	return gone, nil
}

// isNameMap reports whether a dictionary maps names its producer chose to things it
// uses, such as a page's fonts or the document's named destinations. A font called
// /Thumb is still a font, so the removal rules, which are about keys, do not apply to
// the names here. What the names lead to is still judged on its own.
//
// Only a dictionary that is nothing else qualifies. A page cannot escape the rules
// for pages by also being named as its own font map.
func (c *cleaner) isNameMap(d *pdfDict, r role) bool {
	return r&roleNameMap != 0 && r&^(roleNameMap|roleResources|roleAppearance) == 0 && !d.has("Type")
}

// removeDict drops dictionaries that are signatures or attachments as a whole.
func (c *cleaner) removeDict(d *pdfDict, r role, where string) bool {
	switch {
	case c.doc.isSignature(d):
		c.reportSignature(d, where)
		return true
	case c.holdsFile(d, r), c.doc.nameOf(d, "Subtype") == "FileAttachment":
		c.reportAttachment(d, where)
		return true
	}
	return false
}

// holdsFile recognises a file specification with data of its own. In a name map /EF
// may only be what a font is called, so there it has to lead to what /EF leads to in
// a file specification. A signature needs no such care, since nothing is ever called
// /ByteRange by accident.
func (c *cleaner) holdsFile(d *pdfDict, r role) bool {
	if !c.isNameMap(d, r) {
		return d.has("EF") || d.has("RF")
	}
	for _, key := range []string{"EF", "RF"} {
		if files, ok := c.doc.resolve(d.get(key)).(*pdfDict); ok && (files.has("F") || files.has("UF")) {
			return true
		}
	}
	return false
}

// removeStream drops streams that are metadata by type, or by what they contain.
//
// Streams that get drawn and font programs are searched for XMP like any other, but
// finding it there is a warning rather than a removal. Text on a page may mention
// those signatures, a font may carry a packet about the font, and removing either
// would damage the page.
func (c *cleaner) removeStream(s *pdfStream, r role, where string) (bool, error) {
	switch c.doc.nameOf(s.dict, "Type") {
	case "Metadata":
		return true, c.reportMetadata(s, where)
	case "EmbeddedFile":
		c.reportAttachment(s.dict, where)
		return true, nil
	case "ObjStm", "XRef":
		// Machinery of the old layout, which the new one does not use. If kept, an
		// old object stream would carry the objects inside it along with it.
		c.add(removal(core.CatHidden, core.RiskMed, "Stale document structure", "", where))
		return true, nil
	}
	if c.doc.nameOf(s.dict, "Subtype") == "XML" {
		return true, c.reportMetadata(s, where)
	}
	steps, err := c.doc.filterChain(s.dict)
	if err != nil {
		return false, err
	}
	if len(steps) > 0 && isJPEGFilter(steps[len(steps)-1].name) {
		return false, nil // cleanImage gives this to the JPEG handler
	}
	isContent := r&roleContent != 0 || c.isDrawn(s.dict)
	isFont := c.isFontProgram(s.dict)
	isImage := c.doc.nameOf(s.dict, "Subtype") == "Image"
	needs := scanNeeds{content: isContent, pixels: -1}
	if isImage {
		needs.pixels = c.doc.imagePixelBytes(s.dict)
	}
	scan, err := c.doc.scanStream(s, needs)
	if err != nil {
		return false, err
	}
	c.scans[s] = scan
	c.reportScan(scan, where)
	if !scan.hasXMP {
		return false, nil
	}
	if isContent || isFont || isImage {
		c.warn("a page, form, font or image stream contains what looks like an XMP packet; it was kept because removing the stream would damage the document")
		c.add(core.Finding{
			Category: core.CatHidden, Risk: core.RiskMed, Field: "XMP packet in a stream the document needs",
			Where: where, Action: core.ActionRetain, Reason: "removing the stream would damage the document",
		})
		return false, nil
	}
	return true, c.reportMetadata(s, where)
}

// reportScan says what a look through a stream turned up that stays in it. What is
// past the end of its data does not stay, and is taken out by cutStream.
func (c *cleaner) reportScan(scan streamScan, where string) {
	if scan.opaque {
		c.warnOpaqueImage()
	}
	if scan.damaged {
		c.warn("a stream could not be fully decoded, so HideMe could not check all of its data; anything hidden in it remains")
	}
	if scan.inline {
		c.warn("a content stream has an inline image that may be a whole JPEG or similar file, or that has keys HideMe does not recognise; any metadata inside it remains")
	}
	if scan.comments {
		c.warn("a content stream holds comments, which no viewer shows; they were kept and can hold names or paths")
	}
	if scan.excess {
		c.warn("an image stream holds more data than its dimensions use; the extra data was kept and can hold anything")
	}
	if scan.hidden {
		c.add(removal(core.CatHidden, core.RiskHigh, "Data after a stream's end", plural(int(scan.cutBytes), "byte"), where))
	}
}

// isDrawn recognises form and PostScript XObjects and tiling patterns from their
// own keys.
func (c *cleaner) isDrawn(d *pdfDict) bool {
	sub := c.doc.nameOf(d, "Subtype")
	return sub == "Form" || sub == "PS" || d.has("BBox") || d.has("PatternType")
}

// isFontProgram recognises /FontFile, /FontFile2 and /FontFile3 streams.
func (c *cleaner) isFontProgram(d *pdfDict) bool {
	switch c.doc.nameOf(d, "Subtype") {
	case "Type1C", "CIDFontType0C", "OpenType":
		return true
	}
	return d.has("Length1")
}

func (c *cleaner) cleanStream(s *pdfStream, r role, where string) (object, error) {
	raw, source, err := c.cleanImage(s, where)
	if err != nil {
		return nil, err
	}
	dict, err := c.cleanDict(source, r, where, true)
	if err != nil {
		return nil, err
	}
	dict.set("Length", int64(len(raw)))
	return &pdfStream{dict: dict, raw: raw}, nil
}

// cleanImage cleans a stream's JPEG data if it has any, returning the data to write
// and the dictionary that describes it.
//
// A JPEG can be wrapped in text or compression filters, which reportlab does by
// default, and can then still carry a location. Such a chain is unwrapped to a plain
// DCTDecode, which decodes to the same pixels. The dictionary changes before it is
// cleaned, so that an indirect filter entry no longer used does not get an object
// number and survive in the output with nothing referring to it.
func (c *cleaner) cleanImage(s *pdfStream, where string) ([]byte, *pdfDict, error) {
	steps, err := c.doc.filterChain(s.dict)
	if err != nil {
		return nil, nil, err
	}
	if len(steps) == 0 {
		return s.raw, s.dict, nil
	}
	source, err := c.normalDict(s.dict, steps)
	if err != nil {
		return nil, nil, err
	}
	last := len(steps) - 1
	if !isJPEGFilter(steps[last].name) {
		return c.cutStream(s, source, steps)
	}
	jpeg := s.raw
	if last > 0 {
		jpeg, err = c.doc.decodeSteps(s.raw, steps[:last])
		if err != nil {
			if isFinal(err) && !errors.Is(err, core.ErrUnsupported) {
				return nil, nil, err
			}
			c.warnOpaqueImage()
			return s.raw, source, nil
		}
	}
	cleaned, recognised, err := c.cleanJPEG(jpeg, where)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case !recognised:
		return s.raw, source, nil
	case last == 0:
		return cleaned, source, nil
	}
	dict, err := c.rewrittenDict(s.dict, steps[last:])
	return cleaned, dict, err
}

// cutStream takes out what is past the end of a stream's data. If that is past the
// end of a filter inside the chain, the filters outside it go too, since the data
// they wrap is no longer what they encoded. That decodes to the same bytes in the
// end, and no original encoding has to be reproduced.
func (c *cleaner) cutStream(s *pdfStream, source *pdfDict, steps []filterStep) ([]byte, *pdfDict, error) {
	scan, ok := c.scans[s]
	if !ok || scan.cutLayer < 0 {
		return s.raw, source, nil
	}
	if scan.cutLayer == 0 {
		return s.raw[:scan.cutAt], source, nil
	}
	data, err := c.doc.decodeSteps(s.raw, steps[:scan.cutLayer])
	if err != nil {
		return nil, nil, err
	}
	if scan.cutAt > int64(len(data)) {
		return nil, nil, core.Malformedf("a stream decoded differently the second time")
	}
	dict, err := c.rewrittenDict(s.dict, steps[scan.cutLayer:])
	return data[:scan.cutAt], dict, err
}

// normalDict gives a stream whose filters go by inline image abbreviations the
// spelling every reader agrees on, since not all of them read /F as a filter.
func (c *cleaner) normalDict(d *pdfDict, steps []filterStep) (*pdfDict, error) {
	short, err := c.doc.abbreviatedFilter(d)
	if err != nil {
		return nil, err
	}
	if short == nil && !d.has("DP") {
		return d, nil
	}
	return c.rewrittenDict(d, steps)
}

// rewrittenDict copies a stream dictionary for data that now has the given filters.
func (c *cleaner) rewrittenDict(d *pdfDict, steps []filterStep) (*pdfDict, error) {
	short, err := c.doc.abbreviatedFilter(d)
	if err != nil {
		return nil, err
	}
	names := make(pdfArray, len(steps))
	parms := make(pdfArray, len(steps))
	hasParms := false
	for i, step := range steps {
		names[i] = pdfName(step.name)
		if full, ok := fullFilterNames[step.name]; ok {
			names[i] = pdfName(full)
		}
		if step.parms != nil {
			parms[i] = step.parms
			hasParms = true
		}
	}
	var filter, decodeParms object = names, parms
	if len(steps) == 1 {
		filter, decodeParms = names[0], parms[0]
	}
	out := &pdfDict{}
	for _, e := range d.entries {
		switch {
		case e.key == "Filter", e.key == "F" && short != nil:
			out.set("Filter", filter)
		case e.key == "DecodeParms", e.key == "DP":
			if hasParms {
				out.set("DecodeParms", decodeParms)
			}
		default:
			out.set(e.key, e.val)
		}
	}
	return out, nil
}

var fullFilterNames = map[string]string{
	"AHx": "ASCIIHexDecode", "A85": "ASCII85Decode", "LZW": "LZWDecode", "Fl": "FlateDecode",
	"RL": "RunLengthDecode", "CCF": "CCITTFaxDecode", "DCT": "DCTDecode",
}

func (c *cleaner) warnOpaqueImage() {
	c.warn("a stream uses a filter HideMe cannot look inside, such as JPXDecode, JBIG2Decode or CCITTFaxDecode; anything hidden inside that data remains")
}

func isJPEGFilter(name string) bool { return name == "DCTDecode" || name == "DCT" }

// cleanJPEG runs a raw JPEG through its own handler. Strict is right here because
// a PDF viewer ignores a JPEG's EXIF orientation and its ICC profile in favour of
// the PDF's own colour space, so keeping them would preserve nothing visible.
//
// Bytes no handler recognises are kept with a warning, since this may not be a
// JPEG at all, and recognised is false. Any other failure fails the document: the
// data looks like a JPEG, could hold a location, and could not be cleaned.
func (c *cleaner) cleanJPEG(raw []byte, where string) (cleaned []byte, recognised bool, err error) {
	cleaned, nested, err := core.CleanBytes(c.ctx, raw, core.Options{Strict: true, Depth: c.opts.Depth})
	if errors.Is(err, core.ErrUnsupported) {
		c.warn("an image marked as JPEG could not be cleaned because its format was not recognised; any metadata inside it remains")
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("embedded image in %s: %w", where, err)
	}
	c.rep.Merge(where, nested)
	return cleaned, true, nil
}

// annotationKind says how an annotation's keys are to be treated.
type annotationKind int

const (
	notAnnotation annotationKind = iota
	widgetAnnotation
	otherAnnotation
)

// classifyAnnotation tells annotations apart, by role where something refers to
// them as annotations, or else by /Type or the /Rect and /Subtype that all of them
// must have. A field in the form tree with no /Subtype is not an annotation even
// if /Annots also lists it: its /T is a field name.
func (c *cleaner) classifyAnnotation(d *pdfDict, r role) annotationKind {
	sub := c.doc.nameOf(d, "Subtype")
	switch {
	case sub == "Widget":
		return widgetAnnotation
	case r&roleField != 0 && sub == "":
		return notAnnotation
	case r&roleAnnot != 0, c.doc.nameOf(d, "Type") == "Annot", sub != "" && d.has("Rect"):
		return otherAnnotation
	}
	return notAnnotation
}

// shapeRole gives the roles a dictionary claims for itself, so a page reached only
// through a destination, or an old catalog, still gets the allowlist.
func (c *cleaner) shapeRole(d *pdfDict, isStream bool) role {
	if isStream {
		switch c.doc.nameOf(d, "Subtype") {
		case "Form", "Image":
			return roleXObject
		}
		return 0
	}
	switch c.doc.nameOf(d, "Type") {
	case "Page", "Pages":
		return rolePage
	case "Catalog":
		return roleCatalog
	case "Thread":
		return roleThread
	case "Font":
		return roleFont
	case "FontDescriptor":
		return roleFontDescriptor
	case "Action":
		return roleAction
	case "":
		return c.untypedRole(d)
	}
	return 0
}

// untypedRole recognises fonts, whose /Type readers do without, and actions, for
// which it is optional. A structure element also has /S, and its types are the
// producer's to name, but it always has a /P and an action never does.
func (c *cleaner) untypedRole(d *pdfDict) role {
	switch {
	case fontSubtypes[c.doc.nameOf(d, "Subtype")]:
		return roleFont
	case d.has("FontName") && d.has("Flags"):
		return roleFontDescriptor
	case actionTypes[c.doc.nameOf(d, "S")] && !d.has("P") && !d.has("K"):
		return roleAction
	}
	return 0
}

// cleanDict returns a dictionary with its removed keys gone and its references
// renumbered, keeping key order. A stream's /Length is left for the caller to fill
// in. It is never followed, because an indirect length object would otherwise
// survive with nothing left using it.
func (c *cleaner) cleanDict(d *pdfDict, r role, where string, isStream bool) (*pdfDict, error) {
	r |= c.shapeRole(d, isStream)
	kind := c.classifyAnnotation(d, r)
	isMap := !isStream && c.isNameMap(d, r)
	out := &pdfDict{}
	for _, e := range d.entries {
		if isStream && e.key == "Length" {
			out.set("Length", int64(0))
			continue
		}
		if isMap {
			if _, isText := c.doc.resolve(e.val).(pdfString); isText {
				c.noteUnrecognisedKey(e) // nothing a map can name is a string
			}
		} else {
			dropped, err := c.dropEntry(d, e, r, kind, where)
			if err != nil {
				return nil, err
			}
			if dropped {
				continue
			}
			c.noteKept(d, e, r, kind)
		}
		cleaned, keep, err := c.cleanEntry(e, r, where)
		if err != nil {
			return nil, err
		}
		if keep {
			if !isMap && r&allowlistedRoles == 0 && !specKeys[e.key] {
				c.noteUnrecognisedKey(e)
			}
			out.set(e.key, cleaned)
		}
	}
	return out, nil
}

func (c *cleaner) cleanEntry(e dictEntry, r role, where string) (object, bool, error) {
	if e.key == "XFA" && r&roleAcroForm != 0 {
		return c.cleanXFA(e.val, where)
	}
	return c.cleanValue(e.val, childRole(e.key, r), where)
}

// cleanXFA handles the array form of /XFA, a list of packet names and streams. A
// form may carry XMP as one of its packets. Blanking it to null would leave a named
// packet with no data, so the name goes too. The array is written direct, because
// its entries differ from the original's.
func (c *cleaner) cleanXFA(v object, where string) (object, bool, error) {
	a, ok := c.doc.resolve(v).(pdfArray)
	if !ok {
		return c.cleanValue(v, 0, where)
	}
	out := pdfArray{}
	for i := 0; i+1 < len(a); i += 2 {
		packet, keep, err := c.cleanValue(a[i+1], 0, where)
		if err != nil {
			return nil, false, err
		}
		if !keep {
			continue
		}
		label, keep, err := c.cleanValue(a[i], 0, where)
		if err != nil {
			return nil, false, err
		}
		if keep {
			out = append(out, label, packet)
		}
	}
	return out, true, nil
}

// dropEntry reports and takes out the keys that are metadata, and says whether it
// did. These are the removal rules for everything that has no allowlist.
func (c *cleaner) dropEntry(d *pdfDict, e dictEntry, r role, kind annotationKind, where string) (bool, error) {
	switch e.key {
	case "Metadata":
		return true, c.reportMetadata(e.val, where+"/Metadata")
	case "PieceInfo":
		c.add(removal(core.CatHidden, core.RiskMed, "Application private data", c.pieceInfoNames(e.val), where))
		return true, nil
	case "LastModified":
		c.add(removal(core.CatTime, core.RiskMed, "Last modified", formatDate(c.doc.text(e.val)), where))
		return true, nil
	case "Thumb":
		c.add(removal(core.CatThumbnail, core.RiskHigh, "Page thumbnail", c.streamSize(e.val), where))
		return true, nil
	case "AF":
		c.reportAssociatedFiles(e.val, where)
		return true, nil
	case "OPI":
		c.add(removal(core.CatIdentity, core.RiskMed, "Prepress image link", "", where))
		return true, nil
	case "CreatorInfo":
		// Optional content records the application that made a layer here.
		c.add(removal(core.CatSoftware, core.RiskLow, "Layer creator", c.creatorInfoName(e.val), where))
		return true, nil
	case "DPM":
		// Document part metadata, free-form, and in variable printing often names
		// the person each part of the document is for.
		c.add(removal(core.CatHidden, core.RiskMed, "Document part metadata", "", where))
		return true, nil
	case "User":
		// Optional content records who a layer is meant for here, and it may be a person.
		c.add(removal(core.CatIdentity, core.RiskHigh, "Layer user", c.layerUserName(e.val), where))
		return true, nil
	}
	if r&allowlistedRoles != 0 && !isAllowed(e.key, r) {
		c.reportOffListKey(e, r, where)
		return true, nil
	}
	if r&roleThread != 0 && e.key == "I" {
		c.reportInfoDict(e.val, "Thread")
		return true, nil
	}
	if r&roleNames != 0 && e.key == "EmbeddedFiles" {
		c.reportNameTree(e.val)
		return true, nil
	}
	if r&roleNames != 0 && (e.key == "URLS" || e.key == "IDS") {
		// Web capture records where its pages were fetched from, and the content
		// sets those trees lead to go with /SpiderInfo, which is off the catalog's list.
		c.add(removal(core.CatHidden, core.RiskMed, "Web capture data", "", where))
		return true, nil
	}
	if r&roleAcroForm != 0 && e.key == "SigFlags" && c.doc.hasSignature {
		return true, nil // says signatures exist, and they no longer will
	}
	if kind == otherAnnotation || kind == widgetAnnotation {
		if dropped := c.dropAnnotationKey(e, kind); dropped {
			return true, nil
		}
	}
	if r&roleField != 0 || kind == widgetAnnotation {
		switch {
		case e.key == "SV":
			c.add(removal(core.CatIdentity, core.RiskMed, "Signature seed value", "", "AcroForm"))
			return true, nil
		case e.key == "V" && c.fieldType(d) == "Sig":
			if sig, ok := c.doc.resolve(e.val).(*pdfDict); ok {
				c.reportSignature(sig, "AcroForm")
			}
			return true, nil
		}
	}
	return false, nil
}

// dropAnnotationKey applies the annotation rules. On a widget, /T is the form field
// name, and forms break without it, but its dates are still dates.
func (c *cleaner) dropAnnotationKey(e dictEntry, kind annotationKind) bool {
	const where = "Annotation"
	switch e.key {
	case "M", "CreationDate":
		c.add(removal(core.CatTime, core.RiskMed, "Annotation date", formatDate(c.doc.text(e.val)), where))
		return true
	case "T":
		if kind == otherAnnotation {
			c.add(removal(core.CatIdentity, core.RiskHigh, "Annotation author", c.doc.text(e.val), where))
			return true
		}
	case "NM":
		if kind == otherAnnotation {
			c.add(removal(core.CatHidden, core.RiskLow, "Annotation ID", c.doc.text(e.val), where))
			return true
		}
	}
	return false
}

func (c *cleaner) noteUnrecognisedKey(e dictEntry) {
	c.add(core.Finding{
		Category: core.CatHidden, Risk: core.RiskMed, Field: "Unrecognised key", Value: c.keyAndText(e),
		Where: "Dictionary", Action: core.ActionRetain,
		Reason: "objects of this kind have too many optional keys to remove the ones not known",
	})
	c.warn("a dictionary holds a key HideMe does not recognise; it was kept and can hold private data")
}

// fieldType finds a field's /FT, which a field may inherit from its parents.
func (c *cleaner) fieldType(d *pdfDict) string {
	for range maxRefHops {
		if name := c.doc.nameOf(d, "FT"); name != "" {
			return name
		}
		parent, ok := c.doc.resolve(d.get("Parent")).(*pdfDict)
		if !ok {
			break
		}
		d = parent
	}
	return ""
}

// noteKept reports what stays because deleting it would change the document or its
// behaviour. A retained finding names the value, for inspect. A warning does not,
// because warnings are shown on every clean and logs must not recreate metadata.
func (c *cleaner) noteKept(d *pdfDict, e dictEntry, r role, kind annotationKind) {
	switch e.key {
	case "OpenAction":
		if r&roleCatalog != 0 {
			c.warn("the document runs an action when opened (/OpenAction); this is behaviour rather than metadata and was kept")
		}
	case "AA":
		c.warn("the document has automatic actions (/AA); this is behaviour rather than metadata and was kept")
	case "JS", "JavaScript":
		c.warn("the document contains JavaScript; this is behaviour rather than metadata and was kept, and scripts can hold names or paths")
	case "XFA":
		if r&roleAcroForm != 0 {
			c.warn("the form uses XFA; its XML was not inspected and can hold names and field values")
		}
	case "Contents", "RC", "Subj":
		if kind == otherAnnotation {
			if text := c.doc.text(e.val); text != "" {
				c.add(core.Finding{
					Category: core.CatContent, Risk: core.RiskMed, Field: "Comment text", Value: text,
					Where: "Annotation", Action: core.ActionRetain,
					Reason: "visible document content, not metadata",
				})
				c.warn("comment text remains in the document; only its author, dates and IDs were removed")
			}
		}
	case "V", "DV", "RV":
		if r&roleField != 0 || kind == widgetAnnotation {
			c.noteFieldValue(d, e)
		}
	case "F", "UF", "DOS", "Mac", "Unix":
		c.noteFileReference(e)
	case "3DD", "Sound", "Movie", "RichMediaContent":
		c.warn("the document embeds sound, video or 3D data; it was kept, and any metadata inside it remains")
	case "FontFile", "FontFile2", "FontFile3":
		c.add(core.Finding{
			Category: core.CatTechnical, Risk: core.RiskLow, Field: "Embedded font program",
			Where: "Font", Action: core.ActionRetain, Reason: "the page needs it to render; its own name table stays",
		})
	case "DestOutputProfile":
		c.noteICCProfile()
	}
}

func (c *cleaner) noteICCProfile() {
	c.add(core.Finding{
		Category: core.CatTechnical, Risk: core.RiskLow, Field: "ICC colour profile",
		Where: "Color space", Action: core.ActionRetain, Reason: "the page's colours need it, so it stays even in strict mode",
	})
}

// noteFileReference reports a path. These keys hold one if they hold text or a file
// specification at all, whatever they are in: /F is a number on an annotation, but a
// string on a stream or an action means a file, and a dictionary with no name in it
// gives no path.
func (c *cleaner) noteFileReference(e dictEntry) {
	var path string
	switch t := c.doc.resolve(e.val).(type) {
	case pdfString:
		path = decodeText(t.data)
	case *pdfDict:
		path = fileName(c.doc, t)
	}
	if path == "" {
		return
	}
	c.add(core.Finding{
		Category: core.CatIdentity, Risk: core.RiskMed, Field: "External file reference", Value: path,
		Where: "File specification", Action: core.ActionRetain,
		Reason: "links and actions in the document depend on it",
	})
	c.warn("the document refers to external files by path; those paths can hold user names and were kept")
}

func (c *cleaner) noteFieldValue(d *pdfDict, e dictEntry) {
	var text string
	switch t := c.doc.resolve(e.val).(type) {
	case *pdfStream:
		text = "(rich text value)"
	case pdfArray:
		for i, item := range t {
			if i > 0 {
				text += ", "
			}
			text += c.doc.text(item)
			if len(text) > 200 {
				break
			}
		}
	default:
		text = c.doc.text(e.val)
	}
	if text == "" {
		return
	}
	if fieldName := c.doc.text(d.get("T")); fieldName != "" {
		text = fieldName + ": " + text
	}
	c.add(core.Finding{
		Category: core.CatContent, Risk: core.RiskMed, Field: "Form field value", Value: text,
		Where: "AcroForm", Action: core.ActionRetain,
		Reason: "form content, not metadata; deleting it would change the document",
	})
	c.warn("form field values remain in the document; they are content and can hold personal details")
}
