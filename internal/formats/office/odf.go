package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	nsOffice   = "urn:oasis:names:tc:opendocument:xmlns:office:1.0"
	nsMeta     = "urn:oasis:names:tc:opendocument:xmlns:meta:1.0"
	nsConfig   = "urn:oasis:names:tc:opendocument:xmlns:config:1.0"
	nsManifest = "urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"
	nsText     = "urn:oasis:names:tc:opendocument:xmlns:text:1.0"
	nsTable    = "urn:oasis:names:tc:opendocument:xmlns:table:1.0"
	nsSVG      = "urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0"

	nsPresentation = "urn:oasis:names:tc:opendocument:xmlns:presentation:1.0"
	nsDraw         = "urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"
	nsDC           = "http://purl.org/dc/elements/1.1/"
	nsXLink        = "http://www.w3.org/1999/xlink"
	nsOOo          = "http://openoffice.org/2009/office"
)

// odfFormats maps a mimetype entry to the format detected. A type missing here is
// refused: .odb, for instance, embeds a whole database that none of this reads.
var odfFormats = map[string]string{
	"application/vnd.oasis.opendocument.text":                  "odt",
	"application/vnd.oasis.opendocument.spreadsheet":           "ods",
	"application/vnd.oasis.opendocument.presentation":          "odp",
	"application/vnd.oasis.opendocument.graphics":              "odg",
	"application/vnd.oasis.opendocument.text-template":         "ott",
	"application/vnd.oasis.opendocument.spreadsheet-template":  "ots",
	"application/vnd.oasis.opendocument.presentation-template": "otp",
	"application/vnd.oasis.opendocument.graphics-template":     "otg",
}

// odfHandler cleans OpenDocument files.
type odfHandler struct{ limits limits }

func (odfHandler) Format() core.Format {
	return core.Format{
		ID:         "odf",
		Name:       "OpenDocument file",
		Family:     core.FamilyDocument,
		Extensions: []string{".odt", ".ods", ".odp", ".odg", ".ott", ".ots", ".otp", ".otg"},
		Limitations: []string{
			"Annotation text and tracked changes, including deleted text, stay in the file; only their authors and dates are removed.",
			"Text in the document that shows an author or file name, such as a field in a footer, is content and is kept, with a warning for fields.",
			"Formulas and other attributes can name other files by their paths; they are content and are kept with a warning.",
			"Speaker notes, hidden slides, sheets, rows, columns, text and drawing objects, and password hashes, are kept with a warning. Text that is only made invisible, such as white on white, is not detected.",
			"Macros in Basic/ and Scripts/ are kept and can hold user names and paths.",
			"Embedded files no handler recognises are kept unchanged with a warning.",
			"Links to external files are kept with a warning, and their paths can reveal a user name.",
			"Encrypted documents cannot be cleaned and are refused.",
			"Markup in a namespace OpenDocument does not use is removed, and so is text outside paragraphs, comments and processing instructions. Without the format's full schema, an unknown element or attribute in one of its own namespaces cannot be told apart from a real one, and nor can the value of a real attribute, or text in an element with no children. Readers skip many of these, so data placed there stays. RDF metadata files take names from anywhere and are only warned about.",
		},
	}
}

func (odfHandler) Match(head []byte, src core.Source) bool {
	return !startsAsOtherFormat(head) && detectPackage(src) == packageODF
}

func (h odfHandler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	return h.run(ctx, src, nil, opts)
}

func (h odfHandler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	return h.run(ctx, src, dst, opts)
}

// run is both Inspect and Clean, as for OOXML.
func (h odfHandler) run(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	if detectPackage(src) != packageODF {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, core.Unsupportedf("not an OpenDocument package")
	}
	ctx, a, err := openArchive(ctx, src, h.limits)
	if err != nil {
		return nil, err
	}
	mimeEntry, ok := a.byKey[mimetypeName]
	if !ok {
		return nil, core.Malformedf("no mimetype entry")
	}
	mime, err := a.read(ctx, mimeEntry)
	if err != nil {
		return nil, err
	}
	format, ok := odfFormats[string(mime)]
	if !ok {
		return nil, core.Unsupportedf("OpenDocument type %q", mime)
	}
	if err := checkNotEncrypted(ctx, a); err != nil {
		return nil, err
	}
	rep := &core.Report{Format: format}
	if err := a.report(rep); err != nil {
		return nil, err
	}
	a.reportUnusualNames(rep, func(f *zip.File) bool {
		_, removed := removedODFPath(partKey(f.Name))
		return !removed
	})
	w, err := newArchiveWriter(dst)
	if err != nil {
		return nil, err
	}
	// Readers identify an ODF file from fixed offsets into the first entry, so it
	// has to come first and stay uncompressed, wherever the input had it.
	if err := w.add(ctx, mimetypeName, mime, true); err != nil {
		return nil, err
	}
	var hidden hiddenContent
	for _, f := range a.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f == mimeEntry {
			continue
		}
		if err := processODFEntry(ctx, a, f, w, rep, opts, &hidden); err != nil {
			return nil, fmt.Errorf("%q: %w", core.SafeText(f.Name), err)
		}
		release(f)
	}
	hidden.warn(rep)
	if err := warnUnlisted(ctx, a, rep); err != nil {
		return nil, err
	}
	return rep, w.close()
}

// warnUnlisted points out parts the manifest does not list. No reader looks for them,
// which makes them a convenient place to hide data, whatever they are called. Only a
// manifest that can be read says anything about this.
func warnUnlisted(ctx context.Context, a *archive, rep *core.Report) error {
	manifest, ok := a.byKey["meta-inf/manifest.xml"]
	if !ok {
		return nil
	}
	data, err := a.read(ctx, manifest)
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	if _, err := scanXML(ctx, data, func(e *xmlElement) error {
		if e.is(nsManifest, "file-entry") {
			full, _ := e.attr(nsManifest, "full-path")
			listed[partKey(full)] = true
		}
		return nil
	}); err != nil {
		return err
	}
	for _, f := range a.entries {
		key := partKey(f.Name)
		_, removed := removedODFPath(key)
		// An embedded object has a mimetype of its own, which its manifest entry stands for.
		if listed[key] || removed || path.Base(key) == mimetypeName || f == manifest || strings.HasSuffix(f.Name, "/") {
			continue
		}
		rep.Warn("%s: the manifest does not list this part; it was kept", a.label(f))
	}
	return nil
}

// checkNotEncrypted matters because ODF encryption does not set the ZIP encrypted
// flag. Only the manifest shows it, and the parts themselves are opaque, so
// carrying on would pass every secret straight through.
func checkNotEncrypted(ctx context.Context, a *archive) error {
	f, ok := a.byKey["meta-inf/manifest.xml"]
	if !ok {
		return nil
	}
	data, err := a.read(ctx, f)
	if err != nil {
		return err
	}
	_, err = scanXML(ctx, data, func(e *xmlElement) error {
		if e.is(nsManifest, "encryption-data") {
			return fmt.Errorf("%w: the manifest lists encrypted parts", core.ErrEncrypted)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", core.SafeText(f.Name), err)
	}
	return nil
}

var (
	fieldSavedVersions = fieldSpec{"Saved versions", core.CatHidden, core.RiskHigh, core.ActionRemove}
	fieldMetaOther     = fieldSpec{"Other metadata", core.CatContent, core.RiskLow, core.ActionRemove}
	fieldInlineMeta    = fieldSpec{"Embedded document metadata", core.CatHidden, core.RiskHigh, core.ActionRemove}

	fieldPrinterName  = fieldSpec{"Printer name", core.CatDevice, core.RiskMed, core.ActionRemove}
	fieldPrinterSetup = fieldSpec{"Printer setup", core.CatDevice, core.RiskMed, core.ActionRemove}

	fieldNoteAuthor   = fieldSpec{"Annotation author", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldNoteDate     = fieldSpec{"Annotation date", core.CatTime, core.RiskMed, core.ActionRemove}
	fieldNoteInitials = fieldSpec{"Annotation initials", core.CatIdentity, core.RiskMed, core.ActionRemove}
	fieldEditAuthor   = fieldSpec{"Tracked-change author", core.CatIdentity, core.RiskHigh, core.ActionAnonymize}
	fieldEditDate     = fieldSpec{"Tracked-change date", core.CatTime, core.RiskMed, core.ActionAnonymize}
	fieldCredit       = fieldSpec{"Author in document body", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldCreditDate   = fieldSpec{"Date in document body", core.CatTime, core.RiskMed, core.ActionRemove}

	// odfPropertyFields are the text: elements that show a property, the file's name
	// or path, or LibreOffice's own record of the user, as cached text.
	odfPropertyFields = map[string]bool{
		"author-name": true, "author-initials": true, "creator": true, "initial-creator": true, "printed-by": true,
		"file-name": true, "template-name": true, "user-defined": true, "title": true, "subject": true,
		"description": true, "keywords": true, "creation-date": true, "creation-time": true,
		"modification-date": true, "modification-time": true, "print-date": true, "print-time": true,
	}

	metaFields = map[string]fieldSpec{
		"initial-creator":    {"Initial creator", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"creator":            {"Creator", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"printed-by":         {"Printed by", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"creation-date":      {"Creation date", core.CatTime, core.RiskMed, core.ActionRemove},
		"date":               {"Modified", core.CatTime, core.RiskMed, core.ActionRemove},
		"print-date":         {"Print date", core.CatTime, core.RiskMed, core.ActionRemove},
		"editing-cycles":     {"Editing cycles", core.CatHidden, core.RiskLow, core.ActionRemove},
		"editing-duration":   {"Editing duration", core.CatTime, core.RiskLow, core.ActionRemove},
		"generator":          {"Generator", core.CatSoftware, core.RiskLow, core.ActionRemove},
		"title":              {"Title", core.CatContent, core.RiskLow, core.ActionRemove},
		"description":        {"Description", core.CatContent, core.RiskLow, core.ActionRemove},
		"subject":            {"Subject", core.CatContent, core.RiskLow, core.ActionRemove},
		"keyword":            {"Keywords", core.CatContent, core.RiskLow, core.ActionRemove},
		"language":           {"Language", core.CatContent, core.RiskLow, core.ActionRemove},
		"document-statistic": {"Document statistics", core.CatTechnical, core.RiskLow, core.ActionRemove},
		"user-defined":       {"User-defined field", core.CatContent, core.RiskMed, core.ActionRemove},
		"template":           {"Template", core.CatIdentity, core.RiskMed, core.ActionRemove},
	}

	// blankedConfigItems lists the settings that name a device or fingerprint a
	// session. LibreOffice itself writes them blank when it has nothing to say.
	blankedConfigItems = map[string]fieldSpec{
		"PrinterName":  fieldPrinterName,
		"PrintFaxName": fieldPrinterName,
		"PrinterSetup": fieldPrinterSetup,
		"Rsid":         fieldSessionIDs,
		"RsidRoot":     fieldSessionIDs,
	}
)

// removedODFPath recognises parts that go whole. Keys are lowercase.
func removedODFPath(key string) (fieldSpec, bool) {
	in := "/" + strings.TrimSuffix(key, "/") + "/"
	switch {
	case strings.Contains(in, "/thumbnails/"):
		return fieldThumbnail, true
	case strings.HasPrefix(key, "meta-inf/") && strings.Contains(path.Base(key), "signatures"):
		return fieldSignature, true
	case path.Base(key) == "versionlist.xml", strings.Contains(in, "/versions/"):
		return fieldSavedVersions, true
	}
	return fieldSpec{}, false
}

func processODFEntry(ctx context.Context, a *archive, f *zip.File, w *archiveWriter, rep *core.Report, opts core.Options, hidden *hiddenContent) error {
	key := partKey(f.Name)
	spec, removed := removedODFPath(key)
	if strings.HasSuffix(f.Name, "/") {
		// Unlike OPC, ODF lists directories in its manifest, so they are parts.
		if removed {
			return nil
		}
		return w.add(ctx, f.Name, nil, true)
	}
	data, err := a.read(ctx, f)
	if err != nil {
		return err
	}
	label := a.label(f)
	t := newTally(label)
	defer t.flush(rep)
	if removed {
		t.value(spec, byteCount(int64(len(data))))
		if spec == fieldSignature {
			rep.Warn("%s: a digital signature was removed; cleaning changes the document, so it could not stay valid", label)
		}
		return nil
	}
	switch ext := path.Ext(key); {
	case ext == ".xml" || ext == ".rdf":
		warnIfMetadata(rep, f.Name, label)
		data, err = editODFPart(ctx, label, key, data, t, rep, opts, hidden, w == nil)
	case isMacroPath(key):
		warnIfMetadata(rep, f.Name, label)
	case path.Base(key) == mimetypeName && odfMediaType.Match(data):
		// An embedded object has its own. Anything else with this name is not one.
	default:
		// ODF lets a picture live anywhere the content names it, and an OLE object is
		// a bare file, so only the content can say what this is.
		warnIfMetadata(rep, f.Name, label)
		data, err = cleanEmbedded(ctx, label, data, opts, rep)
	}
	if err != nil {
		return err
	}
	if isMacroPath(key) {
		rep.Warn("%s: macros are kept and can hold user names and file paths", label)
	}
	return w.add(ctx, f.Name, data, false)
}

func isMacroPath(key string) bool {
	top, _, _ := strings.Cut(key, "/")
	return top == "basic" || top == "scripts"
}

// odfEditor applies the ODF rules to one XML part. As with OOXML, the rules go by
// namespace and run on every part, because embedded objects repeat the same files
// one directory down.
type odfEditor struct {
	ctx      context.Context
	name     string
	isRDF    bool
	metaPart bool
	version  string
	opts     core.Options
	t        *tally
	rep      *core.Report
	paths    pathNotes

	changes, annotations, fileLinks int
	warned                          map[string]bool

	hidden   *hiddenContent
	notesEnd int // where the notes page last looked at ends
}

// placeholderODFDate is the neutral value for a date the schema requires.
const placeholderODFDate = "1980-01-01T00:00:00"

var (
	odfVersion   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	odfMediaType = regexp.MustCompile(`^application/[a-z0-9.+-]+$`)
)

func editODFPart(ctx context.Context, name, key string, data []byte, t *tally, rep *core.Report, opts core.Options, hidden *hiddenContent, scanOnly bool) ([]byte, error) {
	// LibreOffice really does write an empty Configurations2/accelerator/current.xml.
	if isBlank(data) {
		return data, nil
	}
	ed := &odfEditor{
		ctx:      ctx,
		name:     name,
		isRDF:    path.Ext(key) == ".rdf",
		metaPart: path.Base(key) == "meta.xml",
		opts:     opts,
		t:        t,
		rep:      rep,
		paths:    pathNotes{rep: rep, where: name},
		warned:   map[string]bool{},
		hidden:   hidden,
	}
	// RDF takes its predicates from anywhere, and has a warning of its own.
	policy := odfPolicy
	if ed.isRDF {
		policy = nil
	}
	res, err := runXMLPolicy(ctx, data, ed.visit, !scanOnly, policy)
	if err != nil {
		return nil, err
	}
	noteMarkup(t, res)
	ed.warnContent()
	if ed.metaPart {
		return emptyMetaDocument(ed.version), nil
	}
	return res.out, nil
}

// emptyMetaDocument keeps the part rather than deleting it, since readers expect
// it to exist, and keeps the document's ODF version, which says nothing about
// anyone but does tell readers how to take the rest of the package.
func emptyMetaDocument(version string) []byte {
	versionAttr := ""
	if odfVersion.MatchString(version) {
		versionAttr = ` office:version="` + version + `"`
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<office:document-meta xmlns:office="` + nsOffice + `"` + versionAttr + `><office:meta/></office:document-meta>`)
}

func (ed *odfEditor) warnOnce(format string, args ...any) {
	if ed.warned[format] {
		return
	}
	ed.warned[format] = true
	ed.rep.Warn(format, args...)
}

func (ed *odfEditor) warnContent() {
	if ed.changes > 0 {
		ed.rep.Warn("%s: tracked changes remain (%d found), including any deleted text; accept or reject them before sharing", ed.name, ed.changes)
	}
	if ed.annotations > 0 {
		ed.rep.Warn("%s: annotation text remains (%d found), and can itself name people; only authors and dates were removed", ed.name, ed.annotations)
	}
}

func (ed *odfEditor) visit(e *xmlElement) error {
	if e.depth() == 0 && e.is(nsOffice, "document-meta") {
		ed.metaPart = true
	}
	if ed.metaPart {
		ed.metaElement(e)
		return nil
	}
	switch e.space {
	case nsXMPMeta:
		for _, f := range xmp.Findings(e.subtreeRaw(), ed.name) {
			ed.rep.Add(f)
		}
		e.drop()
		return nil
	case nsManifest:
		ed.manifestElement(e)
	case nsConfig:
		ed.configElement(e)
	case nsDC, nsMeta:
		ed.creditElement(e)
	case nsOffice:
		if err := ed.officeElement(e); err != nil {
			return err
		}
	case nsText, nsTable:
		// A spreadsheet's changes come in several kinds, with deleted cells inside.
		if _, parent := e.parent(); parent == "tracked-changes" || e.local == "changed-region" || e.local == "cell-content-change" {
			ed.changes++
		}
		if e.space == nsText && (odfPropertyFields[e.local] || strings.HasPrefix(e.local, "sender-")) {
			ed.warnOnce("%s: a field shows cached text, which can repeat the author or file path removed from the properties; it is part of the content and was kept", ed.name)
		}
		if e.space == nsText && (e.local == "hidden-text" || e.local == "hidden-paragraph") {
			ed.hidden[hiddenText] = true
		}
	case nsSVG:
		ed.altText(e)
	case nsPresentation:
		if e.local == "notes" {
			ed.odfNotes(e)
		}
	}
	if e.dropped {
		return nil
	}
	if ed.isRDF && e.depth() > 0 {
		if text, leaf := e.leafText(); leaf && strings.TrimSpace(text) != "" {
			ed.warnOnce("%s: this RDF metadata holds literal values and was kept", ed.name)
		}
	}
	for i := range e.attrs {
		a := &e.attrs[i]
		ed.odfAttr(e, a)
		switch a.space {
		case nsOOo:
			if a.local == "rsid" || a.local == "paragraph-rsid" {
				ed.t.count(fieldSessionIDs)
				e.dropAttr(a)
			}
		case nsXLink:
			if a.local == "href" {
				if value := e.value(a); looksLikeFilePath(value, false) {
					if ed.fileLinks < maxValuesPerField {
						reportFileLink(ed.rep, ed.name, value)
					}
					ed.fileLinks++
					continue
				}
			}
		}
		ed.paths.checkAttr(e, a)
	}
	return nil
}

// altText blanks a local path left in a drawing's title or description, as for OOXML:
// converters carry over the full path older versions of Word put there. Real alt text
// stays, and one that merely mentions a path is pointed out.
func (ed *odfEditor) altText(e *xmlElement) {
	if e.local != "title" && e.local != "desc" {
		return
	}
	text, leaf := e.leafText()
	switch {
	case !leaf:
	case looksLikeFilePath(text, false):
		ed.t.value(fieldPicturePath, text)
		e.setContent("")
	case hasPathHint(text):
		ed.paths.note("a drawing's description", text)
	}
}

func (ed *odfEditor) metaElement(e *xmlElement) {
	if e.depth() == 0 {
		if v, ok := e.attr(nsOffice, "version"); ok {
			ed.version = v
		}
		return
	}
	if space, local := e.parent(); space != nsOffice || local != "meta" {
		return
	}
	value := e.subtreeText(describeLimit)
	spec, known := metaFields[e.local]
	switch e.local {
	case "user-defined":
		name, _ := e.attr(nsMeta, "name")
		value = name + " = " + value
	case "document-statistic":
		value = ""
		for i := range e.attrs {
			a := &e.attrs[i]
			if a.space != nsXMLNS {
				value += a.local + "=" + e.value(a) + " "
			}
		}
	case "template":
		href, _ := e.attr(nsXLink, "href")
		value = href
	case "auto-reload", "hyperlink-behaviour":
		href, _ := e.attr(nsXLink, "href")
		value = href
	}
	e.drop()
	if strings.TrimSpace(value) == "" {
		return
	}
	if known {
		ed.t.value(spec, value)
		return
	}
	ed.t.value(fieldMetaOther, e.local+": "+value)
}

// manifestElement keeps the manifest in step with the entries that were removed. A
// listed part that does not exist makes LibreOffice call the file corrupt.
func (ed *odfEditor) manifestElement(e *xmlElement) {
	if e.local != "file-entry" {
		return
	}
	full, _ := e.attr(nsManifest, "full-path")
	if _, removed := removedODFPath(partKey(strings.TrimPrefix(full, "./"))); removed {
		e.drop()
	}
}

func (ed *odfEditor) configElement(e *xmlElement) {
	item, _ := e.attr(nsConfig, "name")
	if item == "ModifyPasswordInfo" {
		ed.hidden[hiddenPasswords] = true
	}
	if e.local != "config-item" {
		return
	}
	spec, ok := blankedConfigItems[item]
	text, leaf := e.leafText()
	if item == "RedlineProtectionKey" && leaf && strings.TrimSpace(text) != "" {
		ed.hidden[hiddenPasswords] = true
	}
	if !ok {
		if leaf && hasPathHint(text) {
			ed.paths.note("a setting", text)
		}
		return
	}
	if leaf && strings.TrimSpace(text) == "" {
		return
	}
	switch spec {
	case fieldPrinterSetup:
		ed.t.value(spec, byteCount(int64(len(text))))
	case fieldSessionIDs:
		ed.t.count(spec)
	default:
		ed.t.value(spec, text)
	}
	e.setContent("")
}

// creditElement deals with names and dates in the body of a document. In an
// annotation they are optional, and LibreOffice shows a comment without them as
// being by an unknown author, so they go. In a tracked change the schema requires
// both, so they are replaced with neutral values instead.
func (ed *odfEditor) creditElement(e *xmlElement) {
	parentSpace, parent := e.parent()
	if parentSpace == nsOffice && parent == "change-info" && e.space == nsDC {
		switch e.local {
		case "creator":
			neutralizeText(ed.t, e, fieldEditAuthor, placeholderAuthor)
		case "date":
			neutralizeText(ed.t, e, fieldEditDate, placeholderODFDate)
		}
		return
	}
	inNote := parentSpace == nsOffice && parent == "annotation"
	var spec fieldSpec
	switch {
	case e.space == nsDC && e.local == "creator" && inNote:
		spec = fieldNoteAuthor
	case e.space == nsDC && e.local == "date" && inNote:
		spec = fieldNoteDate
	case e.space == nsDC && e.local == "creator":
		spec = fieldCredit
	case e.space == nsDC && e.local == "date":
		spec = fieldCreditDate
	case e.space == nsMeta && e.local == "creator-initials":
		spec = fieldNoteInitials
	case e.space == nsMeta && e.local == "date-string":
		spec = fieldNoteDate
	default:
		return
	}
	ed.t.value(spec, e.subtreeText(describeLimit))
	e.drop()
}

func (ed *odfEditor) officeElement(e *xmlElement) error {
	switch e.local {
	case "annotation":
		ed.annotations++
	case "meta":
		// Metadata belongs in meta.xml; a copy in any other part is just as personal.
		ed.t.value(fieldInlineMeta, e.subtreeText(describeLimit))
		e.drop()
	case "binary-data":
		return ed.cleanBinaryData(e)
	}
	return nil
}

// cleanBinaryData handles a picture stored as base64 inside the XML. It is the same
// picture as one in Pictures/, GPS and all, so it gets the same treatment.
func (ed *odfEditor) cleanBinaryData(e *xmlElement) error {
	text, leaf := e.leafText()
	if !leaf {
		return core.Malformedf("office:binary-data holds child elements")
	}
	raw, err := base64.StdEncoding.DecodeString(stripSpace(text))
	if err != nil {
		ed.rep.Warn("%s: an embedded object is not valid base64 and was kept as it is", ed.name)
		return nil
	}
	cleaned, nested, err := core.CleanBytes(ed.ctx, raw, ed.opts)
	switch {
	case err == nil:
		ed.rep.Merge(ed.name, nested)
		e.setContent(base64.StdEncoding.EncodeToString(cleaned))
		return nil
	case errors.Is(err, core.ErrUnsupported):
		ed.rep.Warn("%s: an embedded object could not be cleaned and was kept as it is", ed.name)
		return nil
	}
	return err
}

func stripSpace(s string) string {
	return string(bytes.Map(func(r rune) rune {
		if r < 0x80 && isSpace(byte(r)) {
			return -1
		}
		return r
	}, []byte(s)))
}
