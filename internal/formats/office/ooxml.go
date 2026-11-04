package office

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// ooxmlHandler cleans Word, Excel and PowerPoint documents.
type ooxmlHandler struct{ limits limits }

func (ooxmlHandler) Format() core.Format {
	return core.Format{
		ID:     "ooxml",
		Name:   "Microsoft Office document (OOXML)",
		Family: core.FamilyDocument,
		Extensions: []string{
			".docx", ".docm", ".dotx", ".dotm",
			".xlsx", ".xlsm", ".xltx", ".xltm",
			".pptx", ".pptm", ".potx", ".potm", ".ppsx", ".ppsm",
		},
		Limitations: []string{
			"Comment text and tracked changes, including deleted text, stay in the file; only their authors and dates are neutralised.",
			"Text in the document that shows an author or file name, such as a field in a footer, is content and is kept, with a warning for the fields HideMe recognises. Fields named in some other languages are not recognised unless their text is a path.",
			"Formulas and field codes can name other files by their paths; they are content and are kept with a warning.",
			"Speaker notes, hidden slides, sheets, rows, columns, text and drawing objects, and password hashes, are kept with a warning. Text that is only made invisible, such as white on white, is not detected.",
			"Macros (vbaProject.bin) are kept and can hold user names and paths.",
			"Embedded files no handler recognises, such as .emf pictures and OLE .bin objects, are kept unchanged with a warning.",
			"Links to external files are kept with a warning, and their paths can reveal a user name.",
			"Markup in a namespace Office does not use is removed, and so is text beside child elements, comments and processing instructions. Without the format's full schema, an unknown element or attribute in one of its own namespaces cannot be told apart from a real one, and nor can the value of a real attribute, including a relationship type or a namespace name under a known address, or text in an element with no children. Readers skip many of these, so data placed there stays.",
		},
	}
}

func (ooxmlHandler) Match(head []byte, src core.Source) bool {
	return !startsAsOtherFormat(head) && detectPackage(src) == packageOOXML
}

func (h ooxmlHandler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	return h.run(ctx, src, nil, opts)
}

func (h ooxmlHandler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	return h.run(ctx, src, dst, opts)
}

// run is both Inspect and Clean: Inspect is a Clean that writes nothing, so the
// two cannot disagree about what a document holds.
func (h ooxmlHandler) run(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	if detectPackage(src) != packageOOXML {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, core.Unsupportedf("not an Office Open XML package")
	}
	ctx, a, err := openArchive(ctx, src, h.limits)
	if err != nil {
		return nil, err
	}
	p, err := loadPackage(ctx, a)
	if err != nil {
		return nil, err
	}
	p.scanOnly = dst == nil
	rep := &core.Report{Format: p.format}
	if err := a.report(rep); err != nil {
		return nil, err
	}
	a.reportUnusualNames(rep, func(f *zip.File) bool {
		return !strings.HasSuffix(f.Name, "/") && !p.removed[partKey(f.Name)]
	})
	w, err := newArchiveWriter(dst)
	if err != nil {
		return nil, err
	}
	for _, f := range a.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := p.processEntry(ctx, f, w, rep, opts); err != nil {
			return nil, fmt.Errorf("%q: %w", core.SafeText(f.Name), err)
		}
		release(f)
	}
	p.warnUnreferenced(rep)
	p.hidden.warn(rep)
	return rep, w.close()
}

// relationship is one entry of a .rels part.
type relationship struct {
	id, target string
	typeKey    string
	external   bool
	targetKey  string // the part it points at; empty if external or unresolvable
}

type relsPart struct {
	key    string
	source string // key of the part these relationships belong to; "" for the package
	rels   []relationship
}

// ooxmlPackage is what a document says about its own parts.
type ooxmlPackage struct {
	a         *archive
	format    string
	overrides map[string]string // part key to content type
	defaults  map[string]string // lowercase extension to content type
	rels      []*relsPart

	kinds      map[string]partKind        // for every non-rels part
	removed    map[string]bool            // parts that will not reach the output
	removedIDs map[string]map[string]bool // for each source part, relationship IDs going away
	referenced map[string]bool            // parts a surviving relationship points at
	embedded   map[string]bool            // parts a relationship calls a file in its own right
	remaining  map[string]bool            // extensions some surviving part still has
	gone       map[string]bool            // extensions some removed part had

	threadParts    map[string]bool   // parts that hold comment threading
	threadIDs      map[string]string // for each paragraph ID a thread uses, its new number
	elementIDUsers bool              // something points at drawing element IDs by value

	hidden   hiddenContent
	scanOnly bool // nothing will be written, so edited parts need not be built
}

// loadPackage reads the structure every later decision depends on. The rewrite
// happens entry by entry in archive order, and [Content_Types].xml or a .rels
// part can come first, so what is being removed has to be known in advance.
func loadPackage(ctx context.Context, a *archive) (*ooxmlPackage, error) {
	p := &ooxmlPackage{
		a:          a,
		overrides:  map[string]string{},
		defaults:   map[string]string{},
		kinds:      map[string]partKind{},
		removed:    map[string]bool{},
		removedIDs: map[string]map[string]bool{},
		referenced: map[string]bool{},
		embedded:   map[string]bool{},
		remaining:  map[string]bool{},
		gone:       map[string]bool{},

		threadParts: map[string]bool{},
		threadIDs:   map[string]string{},
	}
	if err := p.loadContentTypes(ctx); err != nil {
		return nil, err
	}
	if err := p.loadRelationships(ctx); err != nil {
		return nil, err
	}
	main, err := p.findMainPart()
	if err != nil {
		return nil, err
	}
	p.classify(main)
	if err := p.loadCommentThreads(ctx); err != nil {
		return nil, err
	}
	if err := p.findElementIDUsers(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// loadCommentThreads numbers the paragraph IDs that comment threads use. A reply
// finds its parent through them, across two parts, so they cannot simply go. The new
// numbers come from their order in the file, which makes them stable under a second
// clean and leaves nothing that links the output to another version of the document.
func (p *ooxmlPackage) loadCommentThreads(ctx context.Context) error {
	for _, f := range p.a.entries {
		key := partKey(f.Name)
		if p.removed[key] || !p.isThreadPart(key) {
			continue
		}
		data, err := p.a.read(ctx, f)
		if err != nil {
			return err
		}
		_, err = scanXML(ctx, data, func(e *xmlElement) error {
			if !e.is(nsWord15, "commentEx") {
				return nil
			}
			for i := range e.attrs {
				a := &e.attrs[i]
				if a.space != nsWord15 || (a.local != "paraId" && a.local != "paraIdParent") {
					continue
				}
				id := strings.ToUpper(e.value(a))
				if _, seen := p.threadIDs[id]; !seen {
					p.threadIDs[id] = fmt.Sprintf("%08X", len(p.threadIDs)+1)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("%q: %w", core.SafeText(f.Name), err)
		}
	}
	return nil
}

func (p *ooxmlPackage) isThreadPart(key string) bool {
	return p.threadParts[key] || path.Base(key) == "commentsextended.xml" ||
		strings.HasSuffix(p.contentType(key), "commentsextended+xml")
}

// findElementIDUsers looks for what refers to a drawing element's ID by value. Only
// PowerPoint does: modern comments anchor to slides and shapes by these IDs, slide
// zooms name their target by them, and Morph may pair shapes with them. Reading every
// part twice is affordable here because presentations keep their bulk in media, which
// this skips. A false match only keeps some IDs, with a warning.
func (p *ooxmlPackage) findElementIDUsers(ctx context.Context) error {
	if !strings.HasPrefix(p.format, "p") {
		return nil
	}
	for _, f := range p.a.entries {
		key := partKey(f.Name)
		_, isRels := relsSource(key)
		kind := p.kinds[key]
		if isRels || strings.HasSuffix(f.Name, "/") || key == contentTypesKey || p.removed[key] ||
			kind == partCoreProps || kind == partAppProps || p.isEmbedded(key) || !p.isXML(key) {
			continue
		}
		data, err := p.a.read(ctx, f)
		if err != nil {
			return err
		}
		if data, err = normalizeXML(data); err != nil {
			continue // the rewrite will meet this part again and decide
		}
		if containsAny(data, elementIDUseMarkers) {
			p.elementIDUsers = true
			return nil
		}
	}
	return nil
}

var elementIDUseMarkers = []string{"creationId=", " cId=", ":morph", "<morph"}

func (p *ooxmlPackage) loadContentTypes(ctx context.Context) error {
	f, ok := p.a.byKey[contentTypesKey]
	if !ok {
		return core.Malformedf("no [Content_Types].xml")
	}
	data, err := p.a.read(ctx, f)
	if err != nil {
		return err
	}
	_, err = scanXML(ctx, data, func(e *xmlElement) error {
		if e.space != nsContentTypes {
			return nil
		}
		switch e.local {
		case "Override":
			name, _ := e.attr("", "PartName")
			contentType, _ := e.attr("", "ContentType")
			p.overrides[partKey(strings.TrimPrefix(name, "/"))] = strings.ToLower(contentType)
		case "Default":
			ext, _ := e.attr("", "Extension")
			contentType, _ := e.attr("", "ContentType")
			p.defaults[strings.ToLower(ext)] = strings.ToLower(contentType)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("[Content_Types].xml: %w", err)
	}
	return nil
}

func (p *ooxmlPackage) loadRelationships(ctx context.Context) error {
	for _, f := range p.a.entries {
		key := partKey(f.Name)
		source, isRels := relsSource(key)
		if !isRels {
			continue
		}
		data, err := p.a.read(ctx, f)
		if err != nil {
			return err
		}
		part := &relsPart{key: key, source: source}
		_, err = scanXML(ctx, data, func(e *xmlElement) error {
			if e.is(nsPackageRels, "Relationship") {
				part.rels = append(part.rels, parseRelationship(e, source))
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("%q: %w", core.SafeText(f.Name), err)
		}
		p.rels = append(p.rels, part)
	}
	return nil
}

func parseRelationship(e *xmlElement, source string) relationship {
	id, _ := e.attr("", "Id")
	relType, _ := e.attr("", "Type")
	target, _ := e.attr("", "Target")
	mode, _ := e.attr("", "TargetMode")
	r := relationship{id: id, target: target, typeKey: relTypeKey(relType), external: strings.EqualFold(mode, "External")}
	if !r.external {
		r.targetKey = resolveTarget(source, target)
	}
	return r
}

// relsSource returns the key of the part a .rels part belongs to. The name is the
// only link between them: word/_rels/document.xml.rels is about word/document.xml,
// and _rels/.rels is about the package itself.
func relsSource(key string) (string, bool) {
	dir, base := path.Split(key)
	dir = strings.TrimSuffix(dir, "/")
	if !strings.HasSuffix(base, ".rels") || path.Base(dir) != "_rels" {
		return "", false
	}
	parent := strings.TrimSuffix(strings.TrimSuffix(dir, "_rels"), "/")
	return path.Join(parent, strings.TrimSuffix(base, ".rels")), true
}

// relTypeKey reduces a relationship type to its last segment. Transitional and
// Strict documents and various Office versions spell the same idea in different
// namespaces and sometimes with different hyphenation.
func relTypeKey(relType string) string {
	last := relType[strings.LastIndexByte(relType, '/')+1:]
	return strings.ToLower(strings.ReplaceAll(last, "-", ""))
}

// resolveTarget turns a relationship target into a part key. Targets are URIs, so
// they may be percent-encoded, and some writers use backslashes.
func resolveTarget(source, target string) string {
	target = strings.ReplaceAll(target, `\`, "/")
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return ""
	}
	key := partKey(target)
	if !strings.HasPrefix(key, "/") {
		key = path.Join("/", path.Dir(source), key)
	}
	return strings.TrimPrefix(path.Clean(key), "/")
}

func (p *ooxmlPackage) findMainPart() (string, error) {
	for _, part := range p.rels {
		if part.source != "" {
			continue
		}
		for _, r := range part.rels {
			if r.typeKey != "officedocument" || r.external {
				continue
			}
			if _, ok := p.a.byKey[r.targetKey]; !ok {
				return "", core.Malformedf("main document part %q is missing", r.target)
			}
			format, ok := ooxmlFormats[p.contentType(r.targetKey)]
			if !ok {
				return "", core.Unsupportedf("package type %q", p.contentType(r.targetKey))
			}
			p.format = format
			return r.targetKey, nil
		}
	}
	return "", core.Malformedf("package has no main document relationship")
}

func (p *ooxmlPackage) contentType(key string) string {
	if ct, ok := p.overrides[key]; ok {
		return ct
	}
	return p.defaults[strings.TrimPrefix(path.Ext(key), ".")]
}

// classify decides the fate of every part. The precedence runs from what Office
// acts on to what is merely conventional. The name comes last because OPC lets a
// part have any name: .NET packaging, for one, writes core properties to
// package/services/metadata/core-properties/<guid>.psmdcp.
func (p *ooxmlPackage) classify(main string) {
	fromRels := map[string]partKind{}
	for _, part := range p.rels {
		for _, r := range part.rels {
			if k, ok := relKinds[r.typeKey]; ok && r.targetKey != "" {
				fromRels[r.targetKey] = k
			}
			if embeddedRelTypes[r.typeKey] {
				p.embedded[r.targetKey] = true
			}
			if r.typeKey == "commentsextended" {
				p.threadParts[r.targetKey] = true
			}
		}
	}
	for _, f := range p.a.entries {
		key := partKey(f.Name)
		_, isRels := relsSource(key)
		if isRels || strings.HasSuffix(f.Name, "/") || key == contentTypesKey || key == main {
			continue
		}
		kind, ok := fromRels[key]
		if !ok {
			kind, ok = contentTypeKinds[p.overrides[key]]
		}
		if !ok {
			kind = kindFromName(key)
		}
		p.kinds[key] = kind
		if kind.removed() {
			p.removed[key] = true
		}
	}
	for _, part := range p.rels {
		p.classifyRels(part)
	}
	for _, f := range p.a.entries {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		key := partKey(f.Name)
		ext := path.Ext(key)
		if p.removed[key] {
			p.gone[ext] = true
		} else {
			p.remaining[ext] = true
		}
	}
}

func (p *ooxmlPackage) classifyRels(part *relsPart) {
	if p.sourceRemoved(part.source) {
		p.removed[part.key] = true
		return
	}
	kept, dropped := 0, 0
	for _, r := range part.rels {
		if !p.relGone(r) {
			kept++
			p.referenced[r.targetKey] = true
			continue
		}
		dropped++
		if p.removedIDs[part.source] == nil {
			p.removedIDs[part.source] = map[string]bool{}
		}
		p.removedIDs[part.source][r.id] = true
	}
	// The package's own relationships always stay, since the main document needs
	// them. Elsewhere an emptied file is noise that also shows what was removed.
	if dropped > 0 && kept == 0 && part.source != "" {
		p.removed[part.key] = true
	}
}

// sourceRemoved reports whether the part a .rels file belongs to is going away. For
// a source that does not exist, only its name is left to go on.
func (p *ooxmlPackage) sourceRemoved(source string) bool {
	if _, exists := p.a.byKey[source]; exists {
		return p.removed[source]
	}
	return kindFromName(source).removed()
}

// relGone is true for a relationship that does not survive: its type marks
// metadata, as with an external attached template, or its target is being removed.
func (p *ooxmlPackage) relGone(r relationship) bool {
	return relKinds[r.typeKey].removed() || (r.targetKey != "" && p.removed[r.targetKey])
}

func (p *ooxmlPackage) processEntry(ctx context.Context, f *zip.File, w *archiveWriter, rep *core.Report, opts core.Options) error {
	// ZIP directory entries are not parts in the OPC model, and their names are one
	// more place for data to hide.
	if strings.HasSuffix(f.Name, "/") {
		return nil
	}
	key := partKey(f.Name)
	source, isRels := relsSource(key)
	// A removed part's relationships go with it. Reading them would add nothing.
	if isRels && p.sourceRemoved(source) {
		return nil
	}
	data, err := p.a.read(ctx, f)
	if err != nil {
		return err
	}
	label := p.a.label(f)
	t := newTally(label)
	defer t.flush(rep)

	kind := p.kinds[key]
	switch {
	case key == contentTypesKey:
		data, err = p.editContentTypes(ctx, data, t)
	case isRels:
		data, err = p.editRels(ctx, data, source, t, rep, label)
	case kind.removed():
		return p.reportRemoved(ctx, kind, data, t, rep, label)
	case kind == partCoreProps:
		if err := reportProperties(ctx, data, coreFields, fieldCoreOther, t); err != nil {
			return err
		}
		data = []byte(emptyCoreProps)
	case kind == partAppProps:
		if err := reportProperties(ctx, data, appFields, fieldAppOther, t); err != nil {
			return err
		}
		data = []byte(emptyAppProps)
	case isVBA(key, p.contentType(key)):
		rep.Warn("%s: macros are kept and can hold user names and file paths", label)
	case p.isEmbedded(key):
		data, err = cleanEmbedded(ctx, label, data, opts, rep)
	case isVML(key):
		data, err = p.editVML(ctx, label, key, data, rep)
	case p.isXML(key):
		warnIfMetadata(rep, f.Name, label)
		data, err = p.editPart(ctx, label, key, data, t, rep)
	default:
		// Not XML and not called a file by the package. It could still be one, and
		// only its content can say so: an altChunk is a whole document, say.
		warnIfMetadata(rep, f.Name, label)
		data, err = cleanEmbedded(ctx, label, data, opts, rep)
	}
	if err != nil {
		return err
	}
	if p.removed[key] {
		return nil // a .rels part left with nothing in it
	}
	return w.add(ctx, f.Name, data, false)
}

// isEmbedded recognises media by what the package says about it, not only by the
// conventional folder, so an image stored at word/pics/a.jpg is still cleaned.
func (p *ooxmlPackage) isEmbedded(key string) bool {
	if strings.Contains("/"+key, "/media/") || strings.Contains("/"+key, "/embeddings/") {
		return true
	}
	ct := p.contentType(key)
	return p.embedded[key] || strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "video/")
}

func (p *ooxmlPackage) isXML(key string) bool {
	switch path.Ext(key) {
	case ".xml", ".rels":
		return true
	}
	ct := p.contentType(key)
	return strings.HasSuffix(ct, "+xml") || strings.HasSuffix(ct, "/xml")
}

func isVML(key string) bool { return path.Ext(key) == ".vml" }

func isVBA(key, contentType string) bool {
	return path.Base(key) == "vbaproject.bin" || contentType == "application/vnd.ms-office.vbaproject"
}

// cleanEmbedded sends an embedded file back through the registry. Formats nothing
// recognises are kept, because removing them would damage the document, and the
// warning names them so the claim of "clean" stays honest.
func cleanEmbedded(ctx context.Context, name string, data []byte, opts core.Options, rep *core.Report) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	cleaned, nested, err := core.CleanBytes(ctx, data, opts)
	switch {
	case err == nil:
		rep.Merge(name, nested)
		return cleaned, nil
	case errors.Is(err, core.ErrUnsupported):
		rep.Warn("%s: this embedded file could not be cleaned and was kept as it is", name)
		return data, nil
	}
	return nil, err
}

// warnIfMetadata flags a part the handler does not understand. It is copied,
// since documents rely on vendor parts, but the name can at least be pointed out.
func warnIfMetadata(rep *core.Report, name, label string) {
	if !looksLikeMetadata(name) {
		return
	}
	rep.Warn("%s: this part is not recognised but looks like metadata; it was kept", label)
}

var metadataHints = []string{"metadata", "properties", "author", "person", "people", "history", "revision", "userdata", "audit", "tracking"}

func looksLikeMetadata(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "xl/metadata.xml": // dynamic-array cell metadata, not about anyone
		return false
	case "ppt/commentauthors.xml", "ppt/authors.xml", "xl/persons/person.xml": // handled by the rules
		return false
	}
	for _, hint := range metadataHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// warnUnreferenced points out parts nothing leads to. No reader of the document
// would ever see them, which makes them a convenient place to hide data.
func (p *ooxmlPackage) warnUnreferenced(rep *core.Report) {
	for _, f := range p.a.entries {
		key := partKey(f.Name)
		_, isRels := relsSource(key)
		if isRels || key == contentTypesKey || strings.HasSuffix(f.Name, "/") || p.removed[key] || p.referenced[key] {
			continue
		}
		rep.Warn("%s: no relationship points at this part; it was kept", p.a.label(f))
	}
}

// editContentTypes drops the entries for removed parts, so that the package does
// not advertise parts it no longer has.
func (p *ooxmlPackage) editContentTypes(ctx context.Context, data []byte, t *tally) ([]byte, error) {
	res, err := runXMLPolicy(ctx, data, func(e *xmlElement) error {
		if e.space != nsContentTypes {
			return nil
		}
		switch e.local {
		case "Override":
			name, _ := e.attr("", "PartName")
			if p.removed[partKey(strings.TrimPrefix(name, "/"))] {
				e.drop()
			}
		case "Default":
			ext, _ := e.attr("", "Extension")
			dotted := "." + strings.ToLower(ext)
			if p.gone[dotted] && !p.remaining[dotted] {
				e.drop()
			}
		}
		return nil
	}, true, ooxmlPolicy)
	if err != nil {
		return nil, err
	}
	noteMarkup(t, res)
	return res.out, nil
}

// editRels removes relationships that do not survive and reports those it keeps
// that point at someone's file system.
func (p *ooxmlPackage) editRels(ctx context.Context, data []byte, source string, t *tally, rep *core.Report, name string) ([]byte, error) {
	links, dangling := 0, 0
	res, err := runXMLPolicy(ctx, data, func(e *xmlElement) error {
		if !e.is(nsPackageRels, "Relationship") {
			return nil
		}
		r := parseRelationship(e, source)
		if p.relGone(r) {
			if relKinds[r.typeKey] == partTemplate {
				t.value(fieldTemplate, r.target)
			}
			e.drop()
			return nil
		}
		_, exists := p.a.byKey[r.targetKey]
		if !r.external && !exists && !looksLikeFilePath(r.target, false) {
			dangling++
			e.drop()
			return nil
		}
		// A hyperlink is usually just a web page, but anything else external, such as
		// a linked workbook, is another of the user's files wherever it is stored.
		var isLink bool
		switch {
		case r.external && r.typeKey == "hyperlink":
			isLink = looksLikeFilePath(r.target, true) || hasPathHint(r.target)
		case r.external:
			isLink = strings.TrimSpace(r.target) != ""
		default:
			isLink = !exists && looksLikeFilePath(r.target, false)
		}
		if isLink {
			if links < maxValuesPerField {
				reportFileLink(rep, name, r.target)
			}
			links++
		}
		return nil
	}, true, ooxmlPolicy)
	if err != nil {
		return nil, err
	}
	if dangling > 0 {
		t.count(fieldDanglingRel)
	}
	noteMarkup(t, res)
	return res.out, nil
}

// reportFileLink retains the link and says so twice over, with a purpose: the
// finding carries the path, which only `inspect` prints, and the warning carries
// no value, because warnings also appear in the logs of a `clean` run.
func reportFileLink(rep *core.Report, where, target string) {
	rep.Add(core.Finding{
		Category: core.CatIdentity,
		Risk:     core.RiskHigh,
		Field:    "External file link",
		Value:    target,
		Where:    where,
		Action:   core.ActionRetain,
		Reason:   "the document needs the link; a local path in it can reveal a user name, so review it before sharing",
	})
	if !looksLikeFilePath(target, true) {
		rep.Warn("%s: a link points at a file outside the document, and its address can reveal a user name", where)
		return
	}
	rep.Warn("%s: a link points at a local file path, which can reveal a user name", where)
}

// pathNotes reports file paths a part has to keep because they are its content. As
// with reportFileLink, the finding carries the value, which only `inspect` prints,
// and the warning does not. The bounds stop a hostile part growing the report.
type pathNotes struct {
	rep      *core.Report
	where    string
	warned   map[string]bool
	findings int

	sawDataURI bool
}

func (n *pathNotes) note(what, value string) {
	if !n.warned[what] {
		if n.warned == nil {
			n.warned = map[string]bool{}
		}
		n.warned[what] = true
		n.rep.Warn("%s: %s holds a file path, which can reveal a user name; it is part of the content and was kept", n.where, what)
	}
	if n.findings < maxValuesPerField {
		n.findings++
		n.rep.Add(core.Finding{
			Category: core.CatIdentity,
			Risk:     core.RiskHigh,
			Field:    "File path",
			Value:    value,
			Where:    n.where,
			Action:   core.ActionRetain,
			Reason:   "it is part of the document's content; a path can reveal a user name, so review it before sharing",
		})
	}
}

// checkAttr is the net under the specific rules: no list of attributes could name
// every place a path turns up, from xml:base to destinationFile. Most attributes in
// a big sheet are short and hold none of these bytes, so they cost next to nothing.
func (n *pathNotes) checkAttr(e *xmlElement, a *xmlAttr) {
	if a.space == nsXMLNS || a.drop || a.replace || !bytes.ContainsAny(e.ed.scan.data[a.valStart:a.valEnd], `:\/&`) {
		return
	}
	// The attribute's name is from the file too, so it goes with the value.
	value := e.value(a)
	switch {
	case hasPathHint(value):
		n.note("an attribute", string(a.name)+"="+value)
	case len(value) >= 5 && strings.EqualFold(value[:5], "data:") && !n.sawDataURI:
		n.sawDataURI = true
		n.rep.Warn("%s: an attribute holds a file as a data: URI, which was not cleaned", n.where)
	}
}

// pathHints are spellings of a path anywhere in a longer string, lowercase. A OneDrive
// address counts, since its folder is named after the account.
var pathHints = []string{`\\`, `..\`, `\users\`, "sharepoint.com/personal/"}

// homeRoots name someone's folder only at the root of a path. In the middle of a web
// address, as in example.com/home/index.html, they are just another folder.
var homeRoots = []string{"/users/", "/home/"}

// hasPathHint finds a file path inside a string that is more than the path, such as
// a formula or a field code. looksLikeFilePath is for a value that is wholly a target.
func hasPathHint(s string) bool {
	if !strings.ContainsAny(s, `:\/`) {
		return false
	}
	lower := strings.ToLower(s)
	for _, hint := range pathHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	for _, root := range homeRoots {
		for at := 0; ; at += len(root) {
			i := strings.Index(lower[at:], root)
			if i < 0 {
				break
			}
			at += i
			if at == 0 || !isHostByte(lower[at-1]) {
				return true
			}
		}
	}
	for i := 1; i+1 < len(s); i++ {
		// A drive letter stands alone. In http:// the colon follows a longer word.
		if s[i] == ':' && (s[i+1] == '\\' || s[i+1] == '/') && isASCIILetter(s[i-1]) && (i == 1 || !isWordByte(s[i-2])) {
			return true
		}
	}
	for at := 0; ; {
		i := strings.Index(lower[at:], "file:")
		if i < 0 {
			return false
		}
		at += i
		if at == 0 || !isWordByte(lower[at-1]) {
			return true
		}
		at += len("file:")
	}
}

func isWordByte(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9')
}

func isHostByte(c byte) bool {
	return isWordByte(c) || c == '.' || c == '-' || c == '_' || c == '%'
}

// looksLikeFilePath recognises references into a file system. A bare relative
// name counts only where the caller knows it leads outside the package, since in
// ODF a relative href normally points at a part inside it.
func looksLikeFilePath(target string, bareIsPath bool) bool {
	t := strings.TrimSpace(target)
	lower := strings.ToLower(t)
	switch {
	case t == "", strings.HasPrefix(t, "#"):
		return false
	case strings.HasPrefix(lower, "file:"),
		strings.HasPrefix(t, `\\`), strings.HasPrefix(t, "//"), strings.HasPrefix(t, "/"),
		strings.HasPrefix(t, "../"), strings.HasPrefix(t, `..\`):
		return true
	case len(t) >= 3 && isASCIILetter(t[0]) && t[1] == ':' && (t[2] == '\\' || t[2] == '/'):
		return true
	}
	return bareIsPath && !hasURIScheme(t)
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func hasURIScheme(t string) bool {
	i := strings.IndexByte(t, ':')
	if i <= 0 || !isASCIILetter(t[0]) {
		return false
	}
	for j := 1; j < i; j++ {
		c := t[j]
		if !isASCIILetter(c) && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// neutralizeText replaces an element's text unless it already holds the
// placeholder. A value that is not plain text is replaced too, since it could be
// hiding the name.
func neutralizeText(t *tally, e *xmlElement, spec fieldSpec, placeholder string) {
	text, leaf := e.leafText()
	if leaf && (strings.TrimSpace(text) == "" || text == placeholder) {
		return
	}
	if !leaf {
		text = e.subtreeText(describeLimit)
	}
	t.value(spec, text)
	e.setContent(placeholder)
}

// noteMarkup reports the comments and instructions an edit removed.
func noteMarkup(t *tally, res xmlEdit) {
	if res.comments > 0 {
		t.value(fieldXMLComment, res.firstComment)
	}
	if res.instructions > 0 {
		t.count(fieldXMLInstruction)
	}
	if res.foreign > 0 {
		t.count(fieldXMLForeign)
	}
}

// editVML treats VML more kindly than XML. Office itself writes VML that is not
// well-formed, with unclosed <br> tags among its shapes, so refusing such a part
// would refuse genuine Excel files. It is kept unread, and the warning says so.
//
// The attempt reports into a scratch report. If it fails part-way, the output keeps
// the original bytes, so nothing it found so far was really removed and the report
// must not say otherwise.
func (p *ooxmlPackage) editVML(ctx context.Context, name, key string, data []byte, rep *core.Report) ([]byte, error) {
	var attempt core.Report
	t := newTally(name)
	out, err := p.editPart(ctx, name, key, data, t, &attempt)
	switch {
	case err == nil:
		t.flush(&attempt)
		rep.Findings = append(rep.Findings, attempt.Findings...)
		rep.Warnings = append(rep.Warnings, attempt.Warnings...)
		return out, nil
	case errors.Is(err, core.ErrMalformed):
		rep.Warn("%s: this VML drawing is not well-formed XML, so it was kept without being inspected", name)
		return data, nil
	}
	return nil, err
}
