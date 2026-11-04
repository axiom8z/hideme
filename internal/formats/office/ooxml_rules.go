package office

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

// Namespaces the rules recognise. Elements are identified by namespace, not by
// prefix, because a document may bind any prefix it likes, and generators such as
// docx4j do. Strict OOXML gives the main vocabularies a second set of names.
const (
	nsWord       = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsWordStrict = "http://purl.oclc.org/ooxml/wordprocessingml/main"
	nsWord14     = "http://schemas.microsoft.com/office/word/2010/wordml"
	nsWord15     = "http://schemas.microsoft.com/office/word/2012/wordml"
	nsWord16DU   = "http://schemas.microsoft.com/office/word/2023/wordml/word16du"
	nsWordCID    = "http://schemas.microsoft.com/office/word/2016/wordml/cid"
	nsWordDraw14 = "http://schemas.microsoft.com/office/word/2010/wordprocessingDrawing"

	nsSheet       = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsSheetStrict = "http://purl.oclc.org/ooxml/spreadsheetml/main"
	nsSheetAC     = "http://schemas.microsoft.com/office/spreadsheetml/2010/11/ac"
	nsSheetRev    = "http://schemas.microsoft.com/office/spreadsheetml/2014/revision"
	nsThreaded    = "http://schemas.microsoft.com/office/spreadsheetml/2018/threadedcomments"

	nsSlide       = "http://schemas.openxmlformats.org/presentationml/2006/main"
	nsSlideStrict = "http://purl.oclc.org/ooxml/presentationml/main"
	nsSlide15     = "http://schemas.microsoft.com/office/powerpoint/2012/main"
	nsSlide188    = "http://schemas.microsoft.com/office/powerpoint/2018/8/main"
	nsSlide14     = "http://schemas.microsoft.com/office/powerpoint/2010/main"

	nsDrawing2014 = "http://schemas.microsoft.com/office/drawing/2014/main"
	nsChart2014   = "http://schemas.microsoft.com/office/drawing/2014/chart"

	nsRels       = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsRelsStrict = "http://purl.oclc.org/ooxml/officeDocument/relationships"
	nsMarkup     = "http://schemas.openxmlformats.org/markup-compatibility/2006"
	nsVMLOffice  = "urn:schemas-microsoft-com:office:office"
	nsXMPMeta    = "adobe:ns:meta/"
	nsRDF        = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
)

// Neutral values for fields the schemas require or that a comment needs in order
// to display. Finding them in a file reports nothing, so a cleaned file stays
// silent when inspected.
const (
	placeholderAuthor   = "Author"
	placeholderInitials = "A"
	placeholderProvider = "None"
	placeholderDate     = "1980-01-01T00:00:00.000"
)

var (
	fieldSessionIDs   = fieldSpec{"Editing session IDs", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldDocumentID   = fieldSpec{"Document ID", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldDataBinding  = fieldSpec{"Content control data binding", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldCommentName  = fieldSpec{"Comment author", core.CatIdentity, core.RiskHigh, core.ActionAnonymize}
	fieldCommentInit  = fieldSpec{"Comment initials", core.CatIdentity, core.RiskMed, core.ActionAnonymize}
	fieldCommentDate  = fieldSpec{"Comment date", core.CatTime, core.RiskMed, core.ActionRemove}
	fieldCommentStamp = fieldSpec{"Comment date", core.CatTime, core.RiskMed, core.ActionAnonymize}
	fieldChangeName   = fieldSpec{"Tracked-change author", core.CatIdentity, core.RiskHigh, core.ActionAnonymize}
	fieldChangeDate   = fieldSpec{"Tracked-change date", core.CatTime, core.RiskMed, core.ActionRemove}

	fieldWorkbookPath = fieldSpec{"Workbook path", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldExcelVersion = fieldSpec{"Excel version", core.CatSoftware, core.RiskLow, core.ActionRemove}
	fieldWorkbookRev  = fieldSpec{"Workbook revision ID", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldSheetRevID   = fieldSpec{"Revision IDs", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldSharedUser   = fieldSpec{"Shared workbook user", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldPerson       = fieldSpec{"Threaded comment person", core.CatIdentity, core.RiskHigh, core.ActionAnonymize}
	fieldPersonDate   = fieldSpec{"Threaded comment date", core.CatTime, core.RiskMed, core.ActionRemove}
	fieldPivotUser    = fieldSpec{"Pivot cache refreshed by", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldPivotDate    = fieldSpec{"Pivot cache refresh date", core.CatTime, core.RiskMed, core.ActionRemove}

	fieldPicturePath = fieldSpec{"Picture file path", core.CatIdentity, core.RiskHigh, core.ActionAnonymize}

	fieldCommentUser = fieldSpec{"Comment author account", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldCommentZone = fieldSpec{"Comment time zone", core.CatTime, core.RiskMed, core.ActionRemove}

	fieldParagraphIDs = fieldSpec{"Paragraph IDs", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldThreadIDs    = fieldSpec{"Comment thread IDs", core.CatHidden, core.RiskLow, core.ActionAnonymize}
	fieldElementIDs   = fieldSpec{"Drawing element IDs", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldRoundTrip    = fieldSpec{"Drawing round-trip data", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldPermEditor   = fieldSpec{"Permitted editor", core.CatIdentity, core.RiskHigh, core.ActionRemove}
)

// maxWorkbookPaths is far above the one a workbook has. Each costs a walk up the
// element stack, so a hostile part full of them would otherwise take minutes.
const maxWorkbookPaths = 1024

// maxFieldCode bounds what is gathered of one field code across its runs.
const maxFieldCode = 64 << 10

// propertyFields are the Word fields whose result comes from the properties or the
// file's own name and path, in English and the commonest translations. Word stores
// a field under the name it was typed with. Keys are uppercase.
var propertyFields = map[string]bool{
	"AUTHOR": true, "LASTSAVEDBY": true, "USERNAME": true, "USERINITIALS": true, "USERADDRESS": true,
	"FILENAME": true, "TEMPLATE": true, "DOCPROPERTY": true, "INFO": true, "TITLE": true, "SUBJECT": true,
	"KEYWORDS": true, "COMMENTS": true, "CREATEDATE": true, "SAVEDATE": true, "PRINTDATE": true,
	"AUTOR": true, "AUTEUR": true, "DATEINAME": true, "NOMFICHIER": true, "NOMBREARCHIVO": true,
	"BENUTZERNAME": true, "NOMUTILISATEUR": true, "NOMBREUSUARIO": true,
}

var threadPlaceholder = regexp.MustCompile(`^tc=\{[0-9A-Fa-f-]+\}$`)

// partEditor applies the rules to one XML part. The rules run on every part
// rather than on a list of paths, since OPC does not fix where a part lives: a
// comment is known by its namespace, wherever it is stored.
type partEditor struct {
	name       string
	removedIDs map[string]bool
	threadIDs  map[string]string
	dropStarts map[int]bool
	t          *tally
	rep        *core.Report
	paths      pathNotes

	changes, comments int
	warned            map[string]bool

	fieldCode    strings.Builder // the code of the current field, across its runs
	fields       []bool          // open fields, innermost last; true once in its result
	inResults    int             // how many of those are in their result
	afterFormula bool            // the next value element is a formula's cached result

	hidden    *hiddenContent
	inNotes   bool // this part is a slide's notes page
	hiddenRun int  // offset of the run whose properties hide it; no run starts at 0
}

// editPart runs the rules over one XML part.
func (p *ooxmlPackage) editPart(ctx context.Context, name, key string, data []byte, t *tally, rep *core.Report) ([]byte, error) {
	if isBlank(data) {
		return data, nil
	}
	ed := &partEditor{
		name:       name,
		removedIDs: p.removedIDs[key],
		threadIDs:  p.threadIDs,
		dropStarts: map[int]bool{},
		t:          t,
		rep:        rep,
		paths:      pathNotes{rep: rep, where: name},
		warned:     map[string]bool{},
		hidden:     &p.hidden,
	}
	data, err := normalizeXML(data)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(data, []byte("absPath")) {
		if err := ed.findWorkbookPaths(ctx, data); err != nil {
			return nil, err
		}
	}
	if containsAny(data, extensionMarkers) {
		if err := ed.findExtensions(ctx, data, p.elementIDUsers); err != nil {
			return nil, err
		}
	}
	// VML holds HTML, and much of it is not even well-formed, so it has rules of its own.
	policy := ooxmlPolicy
	if isVML(key) {
		policy = nil
	}
	res, err := runXMLPolicy(ctx, data, ed.visit, !p.scanOnly, policy)
	if err != nil {
		return nil, err
	}
	noteMarkup(t, res)
	ed.endFieldCode()
	ed.warnContent()
	return res.out, nil
}

func containsAny(data []byte, markers []string) bool {
	for _, marker := range markers {
		if bytes.Contains(data, []byte(marker)) {
			return true
		}
	}
	return false
}

// findWorkbookPaths is a first pass because the rule cuts backwards. The element
// holding the path sits inside an mc:AlternateContent block, and the whole block
// must go, since it means nothing without its only choice. By the time the path
// is seen, the block's start tag has already gone by.
func (ed *partEditor) findWorkbookPaths(ctx context.Context, data []byte) error {
	found := 0
	_, err := scanXML(ctx, data, func(e *xmlElement) error {
		if !e.is(nsSheetAC, "absPath") {
			return nil
		}
		if found++; found > maxWorkbookPaths {
			return fmt.Errorf("%w: more than %d workbook paths in one part", core.ErrTooLarge, maxWorkbookPaths)
		}
		value, _ := e.attr("", "url")
		ed.t.value(fieldWorkbookPath, value)
		if start, ok := e.enclosing(nsMarkup, "AlternateContent"); ok {
			ed.dropStarts[start] = true
		} else {
			ed.dropStarts[e.offset()] = true
		}
		return nil
	})
	return err
}

var extensionMarkers = []string{"presenceInfo", "creationId", "predDERef", "rowId", "colId", "uniqueId"}

// isElementID recognises the random IDs Office gives drawing objects. They are not
// about anyone, but they survive copying and so tie a file to the ones it came from.
func isElementID(e *xmlElement) bool {
	switch e.space {
	case nsDrawing2014:
		return e.local == "creationId" || e.local == "predDERef" || e.local == "rowId" || e.local == "colId"
	case nsSlide14:
		return e.local == "creationId"
	case nsChart2014:
		return e.local == "uniqueId"
	}
	return false
}

// findExtensions is a first pass for the same reason as findWorkbookPaths. These
// elements live in an extension wrapper whose schema requires its one child, so the
// wrapper has to go with them, and its start tag has gone by when they are seen. A
// list with nothing left goes too: though legal, it would only mark what was there.
func (ed *partEditor) findExtensions(ctx context.Context, data []byte, keepIDs bool) error {
	held, emptied := map[int]int{}, map[int]int{}
	kept := false
	_, err := scanXML(ctx, data, func(e *xmlElement) error {
		if e.local == "ext" {
			if local, list, ok := e.ancestor(0); ok && local == "extLst" {
				held[list]++
			}
			return nil
		}
		switch {
		case e.is(nsSlide15, "presenceInfo"):
			value, _ := e.attr("", "userId")
			ed.t.value(fieldCommentUser, value)
		case !isElementID(e):
			return nil
		case keepIDs:
			kept = true
			return nil
		default:
			ed.t.count(fieldElementIDs)
		}
		local, ext, ok := e.ancestor(0)
		if !ok || local != "ext" {
			ed.dropStarts[e.offset()] = true
			return nil
		}
		if !ed.dropStarts[ext] {
			ed.dropStarts[ext] = true
			if local, list, ok := e.ancestor(1); ok && local == "extLst" {
				emptied[list]++
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for list, n := range emptied {
		if n == held[list] {
			ed.dropStarts[list] = true
		}
	}
	if kept {
		ed.warnOnce("%s: drawing element IDs are kept because comments, slide links or transitions point at them; they can tie this file to earlier versions", ed.name)
	}
	return nil
}

func (ed *partEditor) warnOnce(format string, args ...any) {
	if ed.warned[format] {
		return
	}
	ed.warned[format] = true
	ed.rep.Warn(format, args...)
}

// warnContent says what the rules deliberately leave in place. A user who sees
// only "author removed" should not assume the words they typed are gone as well.
func (ed *partEditor) warnContent() {
	if ed.changes > 0 {
		ed.rep.Warn("%s: tracked changes remain (%d found), including any deleted text; accept or reject them before sharing", ed.name, ed.changes)
	}
	if ed.comments > 0 {
		ed.rep.Warn("%s: comment text remains (%d found), and can itself name people; only authors and dates were neutralised", ed.name, ed.comments)
	}
}

func (ed *partEditor) visit(e *xmlElement) error {
	if ed.dropStarts[e.offset()] {
		e.drop()
		return nil
	}
	switch e.space {
	case nsXMPMeta:
		ed.removeXMP(e)
	case nsRDF:
		if e.local == "RDF" {
			ed.removeXMP(e)
		}
	case nsWord, nsWordStrict:
		ed.wordElement(e)
	case nsWord14, nsWord15:
		ed.word1xElement(e)
	case nsSheet, nsSheetStrict:
		ed.sheetElement(e)
	case nsSheetRev:
		if e.local == "revisionPtr" {
			if id, ok := e.attr("", "documentId"); ok {
				ed.t.value(fieldWorkbookRev, id)
			} else {
				ed.t.count(fieldWorkbookRev)
			}
			e.drop()
		}
	case nsThreaded:
		ed.threadedElement(e)
	case nsSlide, nsSlideStrict:
		ed.slideElement(e)
	case nsSlide15:
		ed.slide15Element(e)
	case nsSlide188:
		ed.modernCommentElement(e)
	case nsVMLOffice:
		if e.local == "signatureline" {
			ed.warnOnce("%s: a signature line is kept and can name the person suggested to sign", ed.name)
		}
	}
	if e.dropped {
		return nil
	}
	if e.local == "cNvPr" || e.local == "docPr" {
		ed.pictureProperties(e)
		if hidden, _ := e.attr("", "hidden"); isOn(hidden) {
			ed.hidden[hiddenObjects] = true
		}
	}
	if ed.inNotes && e.local == "t" {
		ed.notesText(e)
	}
	ed.formulaElement(e)
	for i := range e.attrs {
		a := &e.attrs[i]
		switch a.space {
		case nsXMLNS:
		case "":
			// Word's attributes are always qualified, so one that is not would mean
			// nothing to Word. Its value would still sit in the file, though.
			if e.space == nsWord || e.space == nsWordStrict {
				ed.wordAttr(e, a)
			}
		case nsWord, nsWordStrict:
			ed.wordAttr(e, a)
		case nsWord16DU:
			if a.local == "dateUtc" {
				ed.removeAttr(e, a, dateFieldFor(e))
			}
		case nsRels, nsRelsStrict:
			if err := ed.checkReference(e, a); err != nil {
				return err
			}
		case nsVMLOffice:
			switch a.local {
			case "relid":
				if err := ed.checkReference(e, a); err != nil {
					return err
				}
			case "title":
				if value := e.value(a); looksLikeFilePath(value, false) {
					ed.t.value(fieldPicturePath, value)
					e.setAttr(a, "")
				}
			case "href":
				// Where a picture came from. r:id is what actually holds it.
				if value := e.value(a); looksLikeFilePath(value, false) {
					ed.t.value(fieldPicturePath, value)
					e.dropAttr(a)
				}
			case "gfxdata":
				// A base64 ZIP repeating a DrawingML shape, with any name or path it has.
				ed.t.count(fieldRoundTrip)
				e.dropAttr(a)
			}
		case nsWord14:
			switch a.local {
			case "paraId":
				if !ed.renumberThreadID(e, a) {
					ed.t.count(fieldParagraphIDs)
					e.dropAttr(a)
				}
			case "textId":
				ed.t.count(fieldParagraphIDs)
				e.dropAttr(a)
			}
		case nsWord15:
			if e.local == "commentEx" && (a.local == "paraId" || a.local == "paraIdParent") {
				ed.renumberThreadID(e, a)
			}
		case nsWordDraw14:
			if a.local == "anchorId" || a.local == "editId" {
				ed.t.count(fieldParagraphIDs)
				e.dropAttr(a)
			}
		case nsWordCID:
			if a.local == "durableId" {
				ed.t.count(fieldParagraphIDs)
				e.dropAttr(a)
			}
		default:
			// Every revision of the spreadsheet revision namespace carries the
			// same kind of identifier, one namespace per Office release.
			if a.local == "uid" && strings.Contains(a.space, "/office/spreadsheetml/") && strings.Contains(a.space, "/revision") {
				ed.t.count(fieldSheetRevID)
				e.dropAttr(a)
			}
		}
		ed.paths.checkAttr(e, a)
	}
	return nil
}

// renumberThreadID gives an ID used by a comment thread its new number, and reports
// false for any other ID.
func (ed *partEditor) renumberThreadID(e *xmlElement, a *xmlAttr) bool {
	value := e.value(a)
	id, ok := ed.threadIDs[strings.ToUpper(value)]
	if !ok {
		return false
	}
	if value != id {
		ed.t.count(fieldThreadIDs)
		e.setAttr(a, id)
	}
	return true
}

// formulaElement looks for paths in formulas, by local name, because charts,
// extensions and VML form controls all keep formulas in namespaces of their own.
// Excel stores most external references as [1]Sheet1!A1 and the path in a
// relationship, but other writers and Mac Excel spell the path out.
func (ed *partEditor) formulaElement(e *xmlElement) {
	switch e.local {
	case "f", "formula", "formula1", "formula2", "definedName", "calculatedColumnFormula", "totalsRowFormula",
		"FmlaLink", "FmlaMacro", "FmlaPict", "FmlaRange", "FmlaTxbx":
		ed.afterFormula = true
		if text, leaf := e.leafText(); leaf && hasPathHint(text) {
			ed.paths.note("a formula", text)
		}
	case "v":
		// =CELL("filename") caches the workbook's own full path here.
		if ed.afterFormula {
			ed.afterFormula = false
			if text, leaf := e.leafText(); leaf && hasPathHint(text) {
				ed.paths.note("a cached cell value", text)
			}
		}
	}
}

// pictureProperties blanks a local path left in a drawing's name, description or
// title. Older versions of Word put the source file's full path in the description
// of an inserted picture, where it reads as alt text and usually goes unnoticed. A
// value that is not a path is real alt text and stays.
func (ed *partEditor) pictureProperties(e *xmlElement) {
	for i := range e.attrs {
		a := &e.attrs[i]
		if a.space != "" || (a.local != "name" && a.local != "descr" && a.local != "title") {
			continue
		}
		value := e.value(a)
		if !looksLikeFilePath(value, false) {
			continue
		}
		ed.t.value(fieldPicturePath, value)
		if a.local == "name" {
			e.setAttr(a, "Picture") // required by the schema, and it may not be empty
		} else {
			e.setAttr(a, "")
		}
	}
}

// removeXMP takes out a packet carried inside a part, reporting its decoded values.
func (ed *partEditor) removeXMP(e *xmlElement) {
	for _, f := range xmp.Findings(e.subtreeRaw(), ed.name) {
		ed.rep.Add(f)
	}
	e.drop()
}

// checkReference handles an attribute that names a relationship. If that
// relationship is going away the reference must go with it, or Office reports the
// file as corrupt. Only places where dropping the reference is known to be safe
// are handled; a reference anywhere else fails rather than breaking the document.
func (ed *partEditor) checkReference(e *xmlElement, a *xmlAttr) error {
	if !ed.removedIDs[e.value(a)] {
		return nil
	}
	switch e.local {
	case "pageSetup":
		e.dropAttr(a) // the element also holds the paper size and margins, which stay
		return nil
	case "custData":
		e.drop() // nothing but the reference, in a list that may be empty
		return nil
	}
	return core.Unsupportedf("element %q refers to a part that must be removed", e.local)
}

func (ed *partEditor) anonymizeAttr(e *xmlElement, a *xmlAttr, spec fieldSpec, placeholder string) {
	value := e.value(a)
	if value == "" || value == placeholder {
		return
	}
	ed.t.value(spec, value)
	e.setAttr(a, placeholder)
}

func (ed *partEditor) removeAttr(e *xmlElement, a *xmlAttr, spec fieldSpec) {
	ed.t.value(spec, e.value(a))
	e.dropAttr(a)
}

// isCommentElement separates comments from tracked changes for the report. The
// attributes are the same, but a user reads the two very differently.
func isCommentElement(e *xmlElement) bool {
	return e.local == "comment"
}

func dateFieldFor(e *xmlElement) fieldSpec {
	if isCommentElement(e) {
		return fieldCommentDate
	}
	return fieldChangeDate
}

func (ed *partEditor) wordElement(e *xmlElement) {
	switch e.local {
	case "rsids", "rsid", "rsidRoot":
		// rsid elements sit in w:settings and, less obviously, inside each w:style.
		ed.t.count(fieldSessionIDs)
		e.drop()
	case "attachedTemplate", "printerSettings":
		// Both hold only a reference to a relationship that is removed elsewhere,
		// and the reference is required, so the element cannot stay without it.
		e.drop()
	case "dataBinding":
		// A binding links a content control to custom XML or to the core properties,
		// all of which are now gone. Word would otherwise refill the control from
		// the empty store, changing visible text.
		ed.dropBinding(e)
	case "docVar":
		ed.warnOnce("%s: document variables are kept because fields and macros can depend on them; they can hold personal data", ed.name)
	case "mailMerge":
		ed.warnOnce("%s: mail-merge settings are kept and can hold data source paths and connection strings", ed.name)
	case "comment":
		ed.comments++
	case "ins", "del", "moveFrom", "moveTo":
		// The schema requires an author, and wordAttr counts changes by theirs. Without
		// one the deleted text is still in the file.
		if _, ok := e.attr(e.space, "author"); !ok {
			ed.changes++
		}
	case "vanish":
		ed.wordVanish(e)
	case "hidden":
		if _, parent := e.parent(); parent == "trPr" {
			if value, ok := e.attr(e.space, "val"); !ok || isOn(value) {
				ed.hidden[hiddenRowsColumns] = true
			}
		}
	case "documentProtection", "writeProtection":
		if holdsPassword(e, "hash", "hashValue") {
			ed.hidden[hiddenPasswords] = true
		}
	case "instrText":
		ed.hiddenRunText(e)
		if text, leaf := e.leafText(); leaf {
			ed.fieldCode.WriteString(text)
			if ed.fieldCode.Len() >= maxFieldCode {
				ed.endFieldCode()
			}
		}
	case "fldChar":
		ed.endFieldCode()
		ed.fieldChar(e)
	case "fldData":
		ed.warnOnce("%s: a field holds binary data that HideMe cannot read, and it was kept", ed.name)
	case "fldSimple":
		if instr, ok := e.attr(e.space, "instr"); ok {
			ed.checkFieldCode(instr)
		}
	case "t":
		ed.hiddenRunText(e)
		// A field's name can be in any language, but its result can still be a path.
		if ed.inResults > 0 {
			if text, leaf := e.leafText(); leaf && hasPathHint(text) {
				ed.paths.note("a field result", text)
			}
		}
	}
}

// endFieldCode checks the code gathered so far. Word splits a code across runs where
// it pleases, even inside a word, so no single instrText can be judged alone.
func (ed *partEditor) endFieldCode() {
	if ed.fieldCode.Len() > 0 {
		ed.checkFieldCode(ed.fieldCode.String())
		ed.fieldCode.Reset()
	}
}

// checkFieldCode warns rather than clears a result. The result is text on the page,
// which cleaning must not change, and updating the field would now give a blank.
func (ed *partEditor) checkFieldCode(code string) {
	if hasPathHint(code) {
		ed.paths.note("a field code", code)
	}
	words := strings.Fields(code)
	if len(words) == 0 {
		return
	}
	if name := strings.ToUpper(strings.Trim(words[0], `"`)); propertyFields[name] {
		ed.warnOnce("%s: a field such as %s shows cached text, which can repeat the author or file path removed from the properties; it is part of the content and was kept", ed.name, name)
	}
}

// fieldChar follows the begin, separate and end marks of complex fields, which can
// nest, to know when text is a field's result.
func (ed *partEditor) fieldChar(e *xmlElement) {
	kind, _ := e.attr(e.space, "fldCharType")
	n := len(ed.fields)
	switch kind {
	case "begin":
		ed.fields = append(ed.fields, false)
	case "separate":
		if n > 0 && !ed.fields[n-1] {
			ed.fields[n-1] = true
			ed.inResults++
		}
	case "end":
		if n > 0 {
			if ed.fields[n-1] {
				ed.inResults--
			}
			ed.fields = ed.fields[:n-1]
		}
	}
}

// dropBinding removes a content control's link to the properties or custom XML. What
// the control displays is on the page and stays, though it can repeat that data.
func (ed *partEditor) dropBinding(e *xmlElement) {
	ed.t.count(fieldDataBinding)
	e.drop()
	ed.warnOnce("%s: a content control was bound to the document's properties or custom XML; the binding was removed, but the text it displays stays and can repeat that data", ed.name)
}

func (ed *partEditor) word1xElement(e *xmlElement) {
	switch e.local {
	case "docId":
		if id, ok := e.attr(e.space, "val"); ok {
			ed.t.value(fieldDocumentID, id)
		} else {
			ed.t.count(fieldDocumentID)
		}
		e.drop()
	case "dataBinding":
		ed.dropBinding(e)
	}
}

func (ed *partEditor) wordAttr(e *xmlElement, a *xmlAttr) {
	switch {
	case strings.HasPrefix(a.local, "rsid"):
		ed.t.count(fieldSessionIDs)
		e.dropAttr(a)
	case a.local == "author":
		if !isCommentElement(e) {
			ed.changes++
			ed.anonymizeAttr(e, a, fieldChangeName, placeholderAuthor)
			return
		}
		ed.anonymizeAttr(e, a, fieldCommentName, placeholderAuthor)
	case a.local == "initials":
		ed.anonymizeAttr(e, a, fieldCommentInit, placeholderInitials)
	case a.local == "date":
		ed.removeAttr(e, a, dateFieldFor(e))
	case a.local == "ed":
		// The account allowed to edit a protected range. Optional, as w:edGrp can
		// stand in for it, and the range itself stays.
		ed.removeAttr(e, a, fieldPermEditor)
	}
}

func (ed *partEditor) sheetElement(e *xmlElement) {
	ed.sheetVisibility(e)
	switch e.local {
	case "sheetProtection":
		if holdsPassword(e, "password", "hashValue") {
			ed.hidden[hiddenPasswords] = true
		}
	case "workbookProtection":
		if holdsPassword(e, "workbookPassword", "workbookHashValue", "revisionsPassword", "revisionsHashValue") {
			ed.hidden[hiddenPasswords] = true
		}
	case "fileVersion":
		for i := range e.attrs {
			a := &e.attrs[i]
			switch a.local {
			case "lastEdited", "lowestEdited", "rupBuild", "codeName":
				ed.removeAttr(e, a, fieldExcelVersion)
			}
		}
	case "fileSharing":
		ed.removePlainAttr(e, "userName", fieldSharedUser)
		if holdsPassword(e, "reservationPassword", "hashValue") {
			ed.hidden[hiddenPasswords] = true
		}
	case "author":
		if space, local := e.parent(); (space == nsSheet || space == nsSheetStrict) && local == "authors" {
			// An author of tc={guid} is how Excel ties a legacy comment to its threaded
			// comment. It names nobody, and replacing it would break the thread.
			if text, leaf := e.leafText(); !leaf || !threadPlaceholder.MatchString(text) {
				neutralizeText(ed.t, e, fieldCommentName, placeholderAuthor)
			}
		}
	case "comment":
		ed.comments++
	case "pivotCacheDefinition":
		ed.removePlainAttr(e, "refreshedBy", fieldPivotUser)
		ed.removePlainAttr(e, "refreshedDate", fieldPivotDate)
		ed.removePlainAttr(e, "refreshedDateIso", fieldPivotDate)
	case "customWorkbookView":
		ed.warnOnce("%s: custom view names are kept and Excel often puts the user's name in them", ed.name)
	case "connection":
		ed.warnOnce("%s: data connection strings are kept and can hold paths and user names", ed.name)
	case "protectedRange":
		if _, ok := e.attr("", "securityDescriptor"); ok {
			ed.warnSecurityDescriptor()
		}
		if holdsPassword(e, "password", "hashValue") {
			ed.hidden[hiddenPasswords] = true
		}
	case "securityDescriptor":
		ed.warnSecurityDescriptor()
	case "sheetDataSet":
		ed.warnOnce("%s: a cached copy of values from another workbook is kept and can hold its data", ed.name)
	}
}

func (ed *partEditor) warnSecurityDescriptor() {
	ed.warnOnce("%s: a security descriptor on a protected range is kept, since removing it would change who can edit, and it can name Windows accounts", ed.name)
}

// removePlainAttr and anonymizePlainAttr are for the vocabularies whose attributes
// are unqualified. A copy qualified with the element's own namespace is treated the
// same: the schema would not recognise it, but the value would still sit in the file.
func (ed *partEditor) removePlainAttr(e *xmlElement, local string, spec fieldSpec) {
	for i := range e.attrs {
		if a := &e.attrs[i]; a.local == local && (a.space == "" || a.space == e.space) {
			ed.removeAttr(e, a, spec)
		}
	}
}

func (ed *partEditor) anonymizePlainAttr(e *xmlElement, local string, spec fieldSpec, placeholder string) {
	for i := range e.attrs {
		if a := &e.attrs[i]; a.local == local && (a.space == "" || a.space == e.space) {
			ed.anonymizeAttr(e, a, spec, placeholder)
		}
	}
}

// threadedElement covers both xl/persons and xl/threadedComments, which share a
// namespace. They are kept, so that comment threads still display, with the people
// made anonymous. The person IDs that join them are random GUIDs and stay.
func (ed *partEditor) threadedElement(e *xmlElement) {
	switch e.local {
	case "person":
		ed.anonymizePlainAttr(e, "displayName", fieldPerson, placeholderAuthor)
		ed.anonymizePlainAttr(e, "userId", fieldPerson, placeholderAuthor)
		ed.anonymizePlainAttr(e, "providerId", fieldPerson, placeholderProvider)
	case "threadedComment":
		ed.removePlainAttr(e, "dT", fieldPersonDate)
		ed.comments++
	}
}

func (ed *partEditor) slideElement(e *xmlElement) {
	switch e.local {
	case "notes":
		ed.inNotes = e.depth() == 0
	case "sld":
		if show, ok := e.attr("", "show"); ok && !isOn(show) {
			ed.hidden[hiddenSlides] = true
		}
	case "modifyVerifier":
		if holdsPassword(e, "hashData", "hashValue") {
			ed.hidden[hiddenPasswords] = true
		}
	case "cmAuthor":
		ed.anonymizePlainAttr(e, "name", fieldCommentName, placeholderAuthor)
		ed.anonymizePlainAttr(e, "initials", fieldCommentInit, placeholderInitials)
	case "cm":
		ed.removePlainAttr(e, "dt", fieldCommentDate)
		ed.comments++
	case "tag":
		ed.warnOnce("%s: custom tags are kept and can hold arbitrary data", ed.name)
	}
}

func (ed *partEditor) slide15Element(e *xmlElement) {
	// presenceInfo goes in findExtensions, with the wrapper it needs gone too.
	if e.local == "threadingInfo" {
		ed.removePlainAttr(e, "timeZoneBias", fieldCommentZone)
	}
}

// modernCommentElement is PowerPoint 365's comment format. Here the schema
// requires the date on a comment and every field on an author, so they are
// neutralised instead of removed.
func (ed *partEditor) modernCommentElement(e *xmlElement) {
	switch e.local {
	case "author":
		ed.anonymizePlainAttr(e, "name", fieldCommentName, placeholderAuthor)
		ed.anonymizePlainAttr(e, "initials", fieldCommentInit, placeholderInitials)
		ed.anonymizePlainAttr(e, "userId", fieldCommentName, placeholderAuthor)
		ed.anonymizePlainAttr(e, "providerId", fieldCommentName, placeholderProvider)
	case "cm":
		ed.anonymizePlainAttr(e, "created", fieldCommentStamp, placeholderDate)
		ed.comments++
	case "reply":
		ed.anonymizePlainAttr(e, "created", fieldCommentStamp, placeholderDate)
	}
}
