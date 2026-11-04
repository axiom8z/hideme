package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

// part is one archive entry of a fixture.
type part struct{ name, body string }

// zipOf builds an archive the way a real tool does, with real timestamps, so that
// the fixtures have something for the rewrite to remove.
func zipOf(t testing.TB, parts ...part) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		method := zip.Deflate
		if p.name == mimetypeName {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     p.name,
			Method:   method,
			Modified: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, p.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unzip returns the decompressed entries of an archive, by name.
func unzip(t testing.TB, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("output is not a ZIP: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = body
	}
	return out
}

func clean(t testing.TB, h core.Handler, data []byte) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := h.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

// values lists every reported value of a field.
func values(rep *core.Report, field string) []string {
	var out []string
	for _, f := range rep.Findings {
		if f.Field == field {
			out = append(out, f.Value)
		}
	}
	return out
}

func hasValue(rep *core.Report, field, want string) bool {
	for _, v := range values(rep, field) {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}

func hasWarning(rep *core.Report, want string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, want) {
			return true
		}
	}
	return false
}

// assertAbsent checks decompressed parts, since Contract only sees deflated bytes.
func assertAbsent(t *testing.T, parts map[string][]byte, secrets ...string) {
	t.Helper()
	for name, body := range parts {
		for _, s := range secrets {
			if bytes.Contains(body, []byte(s)) {
				t.Errorf("%s still contains %q", name, s)
			}
		}
	}
}

func assertPresent(t *testing.T, parts map[string][]byte, name string, wants ...string) {
	t.Helper()
	body, ok := parts[name]
	if !ok {
		t.Errorf("part %s is missing", name)
		return
	}
	for _, s := range wants {
		if !bytes.Contains(body, []byte(s)) {
			t.Errorf("%s lost %q:\n%s", name, s, body)
		}
	}
}

func assertGone(t *testing.T, parts map[string][]byte, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, ok := parts[name]; ok {
			t.Errorf("part %s should have been removed", name)
		}
	}
}

// assertConsistent checks what Office checks when it opens a package and what a
// lenient reader would never notice: every Override names a real part, every
// internal relationship leads to a real part, every r:* reference in a part
// resolves to a relationship of that part, and every XML part is well-formed.
func assertConsistent(t *testing.T, parts map[string][]byte) {
	t.Helper()
	lower := map[string]bool{}
	for name := range parts {
		lower[strings.ToLower(name)] = true
	}
	overrides := regexp.MustCompile(`PartName="/?([^"]+)"`)
	for _, m := range overrides.FindAllSubmatch(parts["[Content_Types].xml"], -1) {
		if !lower[strings.ToLower(string(m[1]))] {
			t.Errorf("[Content_Types].xml has an Override for missing part %s", m[1])
		}
	}
	ids := map[string]map[string]bool{}
	for name, body := range parts {
		source, isRels := relsSource(strings.ToLower(name))
		if !isRels {
			continue
		}
		ids[source] = map[string]bool{}
		dec := xml.NewDecoder(bytes.NewReader(body))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			el, ok := tok.(xml.StartElement)
			if !ok || el.Name.Local != "Relationship" {
				continue
			}
			var id, target, mode string
			for _, a := range el.Attr {
				switch a.Name.Local {
				case "Id":
					id = a.Value
				case "Target":
					target = a.Value
				case "TargetMode":
					mode = a.Value
				}
			}
			ids[source][id] = true
			if mode == "External" {
				continue
			}
			if key := resolveTarget(source, target); !lower[key] {
				t.Errorf("%s: relationship %s points at missing part %s", name, id, key)
			}
		}
	}
	for name, body := range parts {
		if strings.HasSuffix(name, "/") || !isXMLName(name) {
			continue
		}
		dec := xml.NewDecoder(bytes.NewReader(body))
		dec.Strict = true
		dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("%s is not well-formed: %v\n%s", name, err, body)
				break
			}
			el, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			for _, a := range el.Attr {
				if a.Name.Space == nsRels || a.Name.Space == nsRelsStrict {
					if !ids[strings.ToLower(name)][a.Value] {
						t.Errorf("%s: <%s> refers to relationship %s, which does not exist", name, el.Name.Local, a.Value)
					}
				}
			}
		}
	}
}

func isXMLName(name string) bool {
	switch path.Ext(strings.ToLower(name)) {
	case ".xml", ".rels":
		return true
	}
	return false
}

// textOf returns a part's character data, the words a reader of the document sees.
func textOf(t *testing.T, body []byte) []string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("not well-formed: %v", err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := strings.TrimSpace(string(cd)); s != "" {
				out = append(out, s)
			}
		}
	}
}

// Fake embedded file. Tests never import the real image packages, which are
// separate work, so this stands in for one at the core.CleanBytes boundary.
const (
	fakeImageClean = "FAKEIMG pixels"
	fakeImageDirty = fakeImageClean + " GPS:48.8584,2.2945"
	fakeImageBad   = "FAKEIMG BROKEN"
)

type fakeImage struct{}

func init() { core.Register(fakeImage{}) }

func (fakeImage) Format() core.Format {
	return core.Format{ID: "fakeimg", Name: "Fake image", Family: core.FamilyImage, Extensions: []string{".fake"}}
}

func (fakeImage) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte("FAKEIMG"))
}

func (fakeImage) scan(src core.Source) ([]byte, *core.Report, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, nil, err
	}
	if bytes.Contains(data, []byte("BROKEN")) {
		return nil, nil, core.Malformedf("fake image is broken")
	}
	rep := &core.Report{Format: "fakeimg"}
	if i := bytes.Index(data, []byte(" GPS:")); i >= 0 {
		rep.Add(core.Finding{Category: core.CatLocation, Risk: core.RiskHigh, Field: "GPS coordinates", Value: string(data[i+5:]), Action: core.ActionRemove})
		data = data[:i]
	}
	return data, rep, nil
}

func (h fakeImage) Inspect(_ context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	_, rep, err := h.scan(src)
	return rep, err
}

func (h fakeImage) Clean(_ context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	data, rep, err := h.scan(src)
	if err != nil {
		return nil, err
	}
	_, err = dst.Write(data)
	return rep, err
}

const xmlDecl = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

const (
	relType    = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
	relPkgRoot = `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`
	wordNS     = `xmlns:w="` + nsWord + `" xmlns:r="` + nsRels + `"`
)

const coreXML = xmlDecl + `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
	`<dc:title>Test Title</dc:title><dc:subject>Test Subject</dc:subject><dc:creator>Test Author</dc:creator>` +
	`<cp:keywords>Test Keywords</cp:keywords><dc:description>Test Description</dc:description>` +
	`<cp:lastModifiedBy>Test Editor</cp:lastModifiedBy><cp:revision>42</cp:revision>` +
	`<cp:lastPrinted>2001-02-03T04:05:06Z</cp:lastPrinted>` +
	`<dcterms:created xsi:type="dcterms:W3CDTF">2001-02-03T04:05:07Z</dcterms:created>` +
	`<dcterms:modified xsi:type="dcterms:W3CDTF">2001-02-03T04:05:08Z</dcterms:modified>` +
	`<cp:category>Test Category</cp:category><cp:contentStatus>Test Status</cp:contentStatus>` +
	`<dc:language>xx-TEST</dc:language><cp:version>9.9.9</cp:version></cp:coreProperties>`

const appXML = xmlDecl + `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">` +
	`<Template>C:\Users\testuser\Templates\Secret.dotm</Template><TotalTime>123</TotalTime><Pages>7</Pages>` +
	`<Application>Test Application</Application><Company>Test Company</Company><Manager>Test Manager</Manager>` +
	`<HyperlinkBase>file:///C:/Users/testuser/base</HyperlinkBase><AppVersion>16.0000</AppVersion>` +
	`<TitlesOfParts><vt:vector size="1" baseType="lpstr"><vt:lpstr>Test Part Title</vt:lpstr></vt:vector></TitlesOfParts></Properties>`

const customXMLProps = xmlDecl + `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/custom-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">` +
	`<property fmtid="{D5CDD505-2E9C-101B-9397-08002B2CF9AE}" pid="2" name="Test Secret Name"><vt:lpwstr>test.user@example.invalid</vt:lpwstr></property></Properties>`

func devmode() string {
	var b strings.Builder
	for _, r := range "Test Printer" {
		b.WriteRune(r)
		b.WriteByte(0)
	}
	for b.Len() < 64 {
		b.WriteByte(0)
	}
	b.WriteString("private driver data")
	return b.String()
}

// docProps is the metadata every OOXML fixture shares, with its relationships and
// content types.
func docProps() (rels, types string, parts []part) {
	rels = `<Relationship Id="rIdCore" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>` +
		`<Relationship Id="rIdApp" Type="` + relType + `extended-properties" Target="docProps/app.xml"/>` +
		`<Relationship Id="rIdCustom" Type="` + relType + `custom-properties" Target="docProps/custom.xml"/>` +
		`<Relationship Id="rIdThumb" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/thumbnail" Target="docProps/thumbnail.jpeg"/>`
	types = `<Default Extension="jpeg" ContentType="image/jpeg"/><Default Extension="bin" ContentType="application/vnd.openxmlformats-officedocument.oleObject"/>` +
		`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>` +
		`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>` +
		`<Override PartName="/docProps/custom.xml" ContentType="application/vnd.openxmlformats-officedocument.custom-properties+xml"/>`
	parts = []part{
		{"docProps/core.xml", coreXML},
		{"docProps/app.xml", appXML},
		{"docProps/custom.xml", customXMLProps},
		{"docProps/thumbnail.jpeg", "Test Thumbnail Secret"},
	}
	return rels, types, parts
}

func contentTypes(extra string) string {
	return xmlDecl + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` + extra + `</Types>`
}

const docBody = `<w:body>` +
	`<w:p w:rsidR="00AA0001" w:rsidRDefault="00AA0002" w:rsidP="00AA0003"><w:r w:rsidRPr="00AA0004"><w:t>Visible body text</w:t></w:r></w:p>` +
	`<w:p><w:ins w:id="1" w:author="Test Reviser" w:date="2001-02-03T04:05:09Z"><w:r><w:t>Inserted text</w:t></w:r></w:ins>` +
	`<w:del w:id="2" w:author="Test Deleter" w:date="2001-02-03T04:05:10Z"><w:r><w:delText>Deleted text</w:delText></w:r></w:del></w:p>` +
	`<w:p><w:pPr><w:pPrChange w:id="3" w:author="Test Formatter" w:date="2001-02-03T04:05:11Z"><w:pPr/></w:pPrChange></w:pPr>` +
	`<w:moveFrom w:id="4" w:author="Test Mover" w:date="2001-02-03T04:05:12Z"><w:r><w:t>Moved text</w:t></w:r></w:moveFrom></w:p>` +
	`<w:sdt><w:sdtPr><w:dataBinding w:prefixMappings="" w:xpath="/secret[1]" w:storeItemID="{TEST-STORE}"/></w:sdtPr><w:sdtContent><w:p><w:r><w:t>Bound text</w:t></w:r></w:p></w:sdtContent></w:sdt>` +
	`<w:p><w:r><w:drawing><a:blip xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" r:embed="rIdImage"/></w:drawing></w:r></w:p>` +
	`<w:sectPr w:rsidR="00AA0005"><w:printerSettings r:id="rIdPrinter"/><w:pgSz w:w="12240" w:h="15840"/></w:sectPr></w:body>`

// docxParts builds a document with something planted in every place the rules
// cover. It is returned as parts so tests can add or change one.
func docxParts(mainType string) []part {
	rootRels, types, props := docProps()
	parts := []part{
		{"[Content_Types].xml", contentTypes(types +
			`<Default Extension="emf" ContentType="image/x-emf"/>` +
			`<Override PartName="/word/document.xml" ContentType="` + mainType + `"/>` +
			`<Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/>` +
			`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
			`<Override PartName="/word/comments.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.comments+xml"/>` +
			`<Override PartName="/word/commentsExtended.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.commentsExtended+xml"/>` +
			`<Override PartName="/word/commentsIds.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.commentsIds+xml"/>` +
			`<Override PartName="/word/commentsExtensible.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.commentsExtensible+xml"/>` +
			`<Override PartName="/word/people.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.people+xml"/>` +
			`<Override PartName="/customXml/itemProps1.xml" ContentType="application/vnd.openxmlformats-officedocument.customXmlProperties+xml"/>` +
			`<Override PartName="/word/printerSettings/printerSettings1.bin" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.printerSettings"/>`)},
		{"_rels/.rels", xmlDecl + relPkgRoot + `<Relationship Id="rIdDoc" Type="` + relType + `officeDocument" Target="word/document.xml"/>` + rootRels + `</Relationships>`},
		{"word/document.xml", xmlDecl + `<w:document ` + wordNS + `>` + docBody + `</w:document>`},
		{"word/_rels/document.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdSettings" Type="` + relType + `settings" Target="settings.xml"/>` +
			`<Relationship Id="rIdStyles" Type="` + relType + `styles" Target="styles.xml"/>` +
			`<Relationship Id="rIdComments" Type="` + relType + `comments" Target="comments.xml"/>` +
			`<Relationship Id="rIdExt" Type="http://schemas.microsoft.com/office/2011/relationships/commentsExtended" Target="commentsExtended.xml"/>` +
			`<Relationship Id="rIdIds" Type="http://schemas.microsoft.com/office/2016/09/relationships/commentsIds" Target="commentsIds.xml"/>` +
			`<Relationship Id="rIdExtensible" Type="http://schemas.microsoft.com/office/2018/08/relationships/commentsExtensible" Target="commentsExtensible.xml"/>` +
			`<Relationship Id="rIdPeople" Type="http://schemas.microsoft.com/office/2011/relationships/people" Target="people.xml"/>` +
			`<Relationship Id="rIdXML" Type="` + relType + `customXml" Target="../customXml/item1.xml"/>` +
			`<Relationship Id="rIdPrinter" Type="` + relType + `printerSettings" Target="printerSettings/printerSettings1.bin"/>` +
			`<Relationship Id="rIdImage" Type="` + relType + `image" Target="media/image1.jpeg"/>` +
			`<Relationship Id="rIdEmf" Type="` + relType + `image" Target="media/image2.emf"/>` +
			`<Relationship Id="rIdOle" Type="` + relType + `oleObject" Target="embeddings/oleObject1.bin"/>` +
			`<Relationship Id="rIdWeb" Type="` + relType + `hyperlink" Target="https://example.invalid/page" TargetMode="External"/>` +
			`<Relationship Id="rIdLocal" Type="` + relType + `hyperlink" Target="file:///C:/Users/linkuser/Secret.docx" TargetMode="External"/>` +
			`</Relationships>`},
		{"word/settings.xml", xmlDecl + `<w:settings ` + wordNS + ` xmlns:w14="` + nsWord14 + `" xmlns:w15="` + nsWord15 + `">` +
			`<w:zoom w:percent="100"/><w:attachedTemplate r:id="rIdTemplate"/><w:defaultTabStop w:val="720"/>` +
			`<w:rsids><w:rsidRoot w:val="00BB0001"/><w:rsid w:val="00BB0002"/></w:rsids>` +
			`<w14:docId w14:val="1234ABCD"/><w15:docId w15:val="{TEST-DOC-GUID}"/></w:settings>`},
		{"word/_rels/settings.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdTemplate" Type="` + relType + `attachedTemplate" Target="file:///C:\Users\testuser\Templates\Normal.dotm" TargetMode="External"/></Relationships>`},
		{"word/styles.xml", xmlDecl + `<w:styles ` + wordNS + `><w:style w:type="paragraph" w:styleId="Normal"><w:name w:val="Normal"/><w:rsid w:val="00BB0003"/></w:style></w:styles>`},
		{"word/comments.xml", xmlDecl + `<w:comments ` + wordNS + `><w:comment w:id="0" w:author="Test Commenter" w:initials="TC" w:date="2001-02-03T04:05:13Z"><w:p w:rsidR="00CC0001"><w:r><w:t>Comment body text</w:t></w:r></w:p></w:comment></w:comments>`},
		{"word/commentsExtended.xml", xmlDecl + `<w15:commentsEx xmlns:w15="` + nsWord15 + `"><w15:commentEx w15:paraId="00000001" w15:done="0"/></w15:commentsEx>`},
		{"word/commentsIds.xml", xmlDecl + `<w16cid:commentsIds xmlns:w16cid="http://schemas.microsoft.com/office/word/2016/wordml/cid"><w16cid:commentId w16cid:paraId="00000001" w16cid:durableId="TESTDURABLE"/></w16cid:commentsIds>`},
		{"word/commentsExtensible.xml", xmlDecl + `<w16cex:commentsExtensible xmlns:w16cex="http://schemas.microsoft.com/office/word/2018/wordml/cex"><w16cex:commentExtensible w16cex:durableId="TESTDURABLE" w16cex:dateUtc="2001-02-03T04:05:14Z"/></w16cex:commentsExtensible>`},
		{"word/people.xml", xmlDecl + `<w15:people xmlns:w15="` + nsWord15 + `"><w15:person w15:author="Test Person"><w15:presenceInfo w15:providerId="AD" w15:userId="test.person@example.invalid"/></w15:person></w15:people>`},
		{"word/printerSettings/printerSettings1.bin", devmode()},
		{"word/media/image1.jpeg", fakeImageDirty},
		{"word/media/image2.emf", "EMF test picture"},
		{"word/embeddings/oleObject1.bin", "OLE test object"},
		{"customXml/item1.xml", xmlDecl + `<root><email>sharepoint.user@example.invalid</email></root>`},
		{"customXml/itemProps1.xml", xmlDecl + `<ds:datastoreItem xmlns:ds="http://schemas.openxmlformats.org/officeDocument/2006/customXml" ds:itemID="{TEST-STORE}"/>`},
		{"customXml/_rels/item1.xml.rels", xmlDecl + relPkgRoot + `<Relationship Id="rId1" Type="` + relType + `customXmlProps" Target="itemProps1.xml"/></Relationships>`},
	}
	return append(parts, props...)
}

const docxMain = "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"

func docx(t testing.TB) []byte {
	return zipOf(t, docxParts(docxMain)...)
}

var docxSecrets = []string{
	"Test Author", "Test Editor", "Test Title", "Test Subject", "Test Keywords", "Test Description",
	"Test Category", "Test Status", "Test Company", "Test Manager", "Test Application", "testuser",
	"Test Secret Name", "test.user@example.invalid", "Test Thumbnail", "Test Reviser", "Test Deleter",
	"Test Formatter", "Test Mover", "Test Commenter", "Test Person", "test.person@", "2001-02-03",
	"sharepoint.user@", "TEST-STORE", "TEST-DOC-GUID", "1234ABCD", "TESTDURABLE", "00AA000", "00BB000", "00CC000",
	"GPS:", "Test Printer", "private driver", "w:rsid", "attachedTemplate", "printerSettings", "people.xml", "customXml",
}

const sheetNS = `xmlns="` + nsSheet + `" xmlns:r="` + nsRels + `"`

func xlsxParts() []part {
	rootRels, types, props := docProps()
	parts := []part{
		{"[Content_Types].xml", contentTypes(types +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>` +
			`<Override PartName="/xl/comments1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.comments+xml"/>` +
			`<Override PartName="/xl/persons/person.xml" ContentType="application/vnd.ms-excel.person+xml"/>` +
			`<Override PartName="/xl/threadedComments/threadedComment1.xml" ContentType="application/vnd.ms-excel.threadedcomments+xml"/>` +
			`<Override PartName="/xl/pivotCache/pivotCacheDefinition1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.pivotCacheDefinition+xml"/>` +
			`<Override PartName="/xl/printerSettings/printerSettings1.bin" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.printerSettings"/>` +
			`<Override PartName="/xl/externalLinks/externalLink1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.externalLink+xml"/>` +
			`<Override PartName="/xl/revisions/userNames.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.userNames+xml"/>`)},
		{"_rels/.rels", xmlDecl + relPkgRoot + `<Relationship Id="rIdDoc" Type="` + relType + `officeDocument" Target="xl/workbook.xml"/>` + rootRels + `</Relationships>`},
		{"xl/workbook.xml", xmlDecl + `<workbook ` + sheetNS + ` xmlns:mc="` + nsMarkup + `" mc:Ignorable="x15ac xr" xmlns:x15ac="` + nsSheetAC + `" xmlns:xr="` + nsSheetRev + `">` +
			`<fileVersion appName="xl" lastEdited="7" lowestEdited="6" rupBuild="24312" codeName="{TEST-CODE-GUID}"/>` +
			`<fileSharing userName="Test Sharer"/><workbookPr defaultThemeVersion="166925"/>` +
			`<mc:AlternateContent><mc:Choice Requires="x15ac"><x15ac:absPath url="C:\Users\testuser\Desktop\"/></mc:Choice></mc:AlternateContent>` +
			`<xr:revisionPtr revIDLastSave="0" documentId="8_{TEST-REV-GUID}"/>` +
			`<sheets><sheet name="Sheet1" sheetId="1" r:id="rIdSheet"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdSheet" Type="` + relType + `worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rIdStrings" Type="` + relType + `sharedStrings" Target="sharedStrings.xml"/>` +
			`<Relationship Id="rIdPersons" Type="http://schemas.microsoft.com/office/2017/10/relationships/person" Target="persons/person.xml"/>` +
			`<Relationship Id="rIdPivot" Type="` + relType + `pivotCacheDefinition" Target="pivotCache/pivotCacheDefinition1.xml"/>` +
			`<Relationship Id="rIdLink" Type="` + relType + `externalLink" Target="externalLinks/externalLink1.xml"/>` +
			`<Relationship Id="rIdUsers" Type="` + relType + `usernames" Target="revisions/userNames.xml"/>` +
			`<Relationship Id="rIdXML" Type="` + relType + `customXml" Target="../customXml/item1.xml"/></Relationships>`},
		{"xl/worksheets/sheet1.xml", xmlDecl + `<worksheet ` + sheetNS + ` xmlns:x14="http://schemas.microsoft.com/office/spreadsheetml/2009/9/main" xmlns:xr="` + nsSheetRev + `" xr:uid="{TEST-SHEET-UID}" xmlns:mc="` + nsMarkup + `">` +
			`<sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><v>12345</v></c></row></sheetData>` +
			`<mc:AlternateContent><mc:Choice Requires="x14"><controls/></mc:Choice></mc:AlternateContent>` +
			`<pageSetup orientation="landscape" r:id="rIdPrinter"/><legacyDrawing r:id="rIdKeep"/></worksheet>`},
		{"xl/worksheets/_rels/sheet1.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdPrinter" Type="` + relType + `printerSettings" Target="../printerSettings/printerSettings1.bin"/>` +
			`<Relationship Id="rIdKeep" Type="` + relType + `vmlDrawing" Target="../drawings/vmlDrawing1.vml"/>` +
			`<Relationship Id="rIdComments" Type="` + relType + `comments" Target="../comments1.xml"/>` +
			`<Relationship Id="rIdThread" Type="http://schemas.microsoft.com/office/2017/10/relationships/threadedComment" Target="../threadedComments/threadedComment1.xml"/>` +
			`</Relationships>`},
		{"xl/sharedStrings.xml", xmlDecl + `<sst xmlns="` + nsSheet + `" count="1" uniqueCount="1"><si><t>Visible cell text</t></si></sst>`},
		{"xl/comments1.xml", xmlDecl + `<comments xmlns="` + nsSheet + `"><authors><author>Test Commenter</author><author>Second Commenter</author></authors>` +
			`<commentList><comment ref="A1" authorId="0"><text><r><t>Cell comment text</t></r></text></comment></commentList></comments>`},
		{"xl/persons/person.xml", xmlDecl + `<personList xmlns="` + nsThreaded + `"><person displayName="Test Person" id="{00000000-0000-0000-0000-000000000001}" userId="test.person@example.invalid" providerId="AD"/></personList>`},
		{"xl/threadedComments/threadedComment1.xml", xmlDecl + `<ThreadedComments xmlns="` + nsThreaded + `"><threadedComment ref="A1" dT="2001-02-03T04:05:15.00" personId="{00000000-0000-0000-0000-000000000001}" id="{00000000-0000-0000-0000-000000000002}"><text>Threaded comment text</text></threadedComment></ThreadedComments>`},
		{"xl/pivotCache/pivotCacheDefinition1.xml", xmlDecl + `<pivotCacheDefinition ` + sheetNS + ` refreshedBy="Test Refresher" refreshedDate="2001.5" refreshedDateIso="2001-02-03T04:05:16" recordCount="1"/>`},
		{"xl/printerSettings/printerSettings1.bin", devmode()},
		{"xl/externalLinks/externalLink1.xml", xmlDecl + `<externalLink ` + sheetNS + `><externalBook r:id="rId1"/></externalLink>`},
		{"xl/externalLinks/_rels/externalLink1.xml.rels", xmlDecl + relPkgRoot + `<Relationship Id="rId1" Type="` + relType + `externalLinkPath" Target="file:///C:/Users/testuser/Budget.xlsx" TargetMode="External"/></Relationships>`},
		{"xl/drawings/vmlDrawing1.vml", `<xml xmlns:v="urn:schemas-microsoft-com:vml"><v:shape id="s1"/></xml>`},
		{"xl/revisions/userNames.xml", xmlDecl + `<users xmlns="` + nsSheet + `"><userInfo guid="{TEST-GUID}" name="Test Revision User"/></users>`},
		{"customXml/item1.xml", xmlDecl + `<root><email>sharepoint.user@example.invalid</email></root>`},
	}
	return append(parts, props...)
}

func xlsx(t testing.TB) []byte { return zipOf(t, xlsxParts()...) }

const slideNS = `xmlns:p="` + nsSlide + `" xmlns:r="` + nsRels + `" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`

func pptxParts() []part {
	rootRels, types, props := docProps()
	parts := []part{
		{"[Content_Types].xml", contentTypes(types +
			`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>` +
			`<Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>` +
			`<Override PartName="/ppt/commentAuthors.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.commentAuthors+xml"/>` +
			`<Override PartName="/ppt/comments/comment1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.comments+xml"/>` +
			`<Override PartName="/ppt/authors.xml" ContentType="application/vnd.ms-powerpoint.authors+xml"/>` +
			`<Override PartName="/ppt/comments/modernComment1.xml" ContentType="application/vnd.ms-powerpoint.comments+xml"/>` +
			`<Override PartName="/ppt/revisionInfo.xml" ContentType="application/vnd.ms-powerpoint.revisioninfo+xml"/>` +
			`<Override PartName="/ppt/changesInfos/changesInfo1.xml" ContentType="application/vnd.ms-powerpoint.changesinfo+xml"/>` +
			`<Override PartName="/ppt/printerSettings/printerSettings1.bin" ContentType="application/vnd.openxmlformats-officedocument.presentationml.printerSettings"/>`)},
		{"_rels/.rels", xmlDecl + relPkgRoot + `<Relationship Id="rIdDoc" Type="` + relType + `officeDocument" Target="ppt/presentation.xml"/>` + rootRels + `</Relationships>`},
		{"ppt/presentation.xml", xmlDecl + `<p:presentation ` + slideNS + `><p:sldIdLst><p:sldId id="256" r:id="rIdSlide"/></p:sldIdLst></p:presentation>`},
		{"ppt/_rels/presentation.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdSlide" Type="` + relType + `slide" Target="slides/slide1.xml"/>` +
			`<Relationship Id="rIdAuthors" Type="` + relType + `commentAuthors" Target="commentAuthors.xml"/>` +
			`<Relationship Id="rIdModern" Type="http://schemas.microsoft.com/office/2018/10/relationships/authors" Target="authors.xml"/>` +
			`<Relationship Id="rIdRevision" Type="http://schemas.microsoft.com/office/2015/10/relationships/revisionInfo" Target="revisionInfo.xml"/>` +
			`<Relationship Id="rIdChanges" Type="http://schemas.microsoft.com/office/2016/11/relationships/changesInfo" Target="changesInfos/changesInfo1.xml"/>` +
			`<Relationship Id="rIdPrinter" Type="` + relType + `printerSettings" Target="printerSettings/printerSettings1.bin"/></Relationships>`},
		{"ppt/slides/slide1.xml", xmlDecl + `<p:sld ` + slideNS + `><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Visible slide text</a:t></a:r></a:p></p:txBody></p:sp>` +
			`<p:pic><a:blip r:embed="rIdImage"/></p:pic></p:spTree></p:cSld></p:sld>`},
		{"ppt/slides/_rels/slide1.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdImage" Type="` + relType + `image" Target="../media/image1.jpeg"/>` +
			`<Relationship Id="rIdComments" Type="` + relType + `comments" Target="../comments/comment1.xml"/>` +
			`<Relationship Id="rIdModern" Type="http://schemas.microsoft.com/office/2018/10/relationships/comments" Target="../comments/modernComment1.xml"/></Relationships>`},
		{"ppt/media/image1.jpeg", fakeImageDirty},
		{"ppt/commentAuthors.xml", xmlDecl + `<p:cmAuthorLst ` + slideNS + ` xmlns:p15="` + nsSlide15 + `"><p:cmAuthor id="1" name="Test Commenter" initials="TC" lastIdx="1" clrIdx="0"><p:extLst><p:ext uri="{TEST}"><p15:presenceInfo userId="test.person@example.invalid" providerId="AD"/></p:ext></p:extLst></p:cmAuthor></p:cmAuthorLst>`},
		{"ppt/comments/comment1.xml", xmlDecl + `<p:cmLst ` + slideNS + ` xmlns:p15="` + nsSlide15 + `"><p:cm authorId="1" dt="2001-02-03T04:05:17.000" idx="1"><p:text>Slide comment text</p:text><p:extLst><p:ext uri="{TEST}"><p15:threadingInfo timeZoneBias="300"/></p:ext></p:extLst></p:cm></p:cmLst>`},
		{"ppt/authors.xml", xmlDecl + `<p188:authorLst xmlns:p188="` + nsSlide188 + `"><p188:author id="{TEST}" name="Test Modern Author" initials="TM" userId="S::test.modern@example.invalid" providerId="AD"/></p188:authorLst>`},
		{"ppt/comments/modernComment1.xml", xmlDecl + `<p188:cmLst xmlns:p188="` + nsSlide188 + `"><p188:cm id="{TEST}" authorId="{TEST}" created="2001-02-03T04:05:18.000"><p188:replyLst><p188:reply id="{R}" authorId="{TEST}" created="2001-02-03T04:05:19.000"/></p188:replyLst></p188:cm></p188:cmLst>`},
		{"ppt/revisionInfo.xml", xmlDecl + `<p1510:revInfo xmlns:p1510="http://schemas.microsoft.com/office/powerpoint/2015/10/main"><p1510:client id="{TEST-CLIENT}" v="1" dt="2001-02-03T04:05:20.000"/></p1510:revInfo>`},
		{"ppt/changesInfos/changesInfo1.xml", xmlDecl + `<pc:chgInfo xmlns:pc="http://schemas.microsoft.com/office/powerpoint/2013/main/command"><pc:chgData name="Test Changer" userId="test.changer@example.invalid"/></pc:chgInfo>`},
		{"ppt/printerSettings/printerSettings1.bin", devmode()},
	}
	return append(parts, props...)
}

func pptx(t testing.TB) []byte { return zipOf(t, pptxParts()...) }

const (
	odfNS = `xmlns:office="` + nsOffice + `" xmlns:text="` + nsText + `" xmlns:dc="` + nsDC + `" xmlns:meta="` + nsMeta +
		`" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" xmlns:officeooo="` + nsOOo + `" xmlns:xlink="` + nsXLink +
		`" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:config="` + nsConfig + `"`

	odfMeta = xmlDecl + `<office:document-meta ` + odfNS + ` office:version="1.3"><office:meta>` +
		`<meta:initial-creator>Test Author</meta:initial-creator><dc:creator>Test Editor</dc:creator>` +
		`<meta:creation-date>2001-02-03T04:05:06</meta:creation-date><dc:date>2001-02-03T04:05:07</dc:date>` +
		`<meta:editing-cycles>42</meta:editing-cycles><meta:editing-duration>PT1H</meta:editing-duration>` +
		`<meta:generator>Test Generator/1.0</meta:generator><meta:print-date>2001-02-03T04:05:08</meta:print-date>` +
		`<meta:printed-by>Test Printer Person</meta:printed-by><dc:title>Test Title</dc:title>` +
		`<dc:description>Test Description</dc:description><dc:subject>Test Subject</dc:subject>` +
		`<meta:keyword>Test Keyword</meta:keyword><meta:document-statistic meta:page-count="7"/>` +
		`<meta:user-defined meta:name="Test Secret Name" meta:value-type="string">test.user@example.invalid</meta:user-defined>` +
		`<meta:template xlink:href="file:///C:/Users/testuser/Template.ott" xlink:type="simple"/>` +
		`</office:meta></office:document-meta>`

	odfContent = xmlDecl + `<office:document-content ` + odfNS + ` office:version="1.3">` +
		`<office:automatic-styles><style:style style:name="T1" style:family="text"><style:text-properties officeooo:rsid="00AA0001"/></style:style>` +
		`<style:style style:name="P1" style:family="paragraph"><style:text-properties officeooo:paragraph-rsid="00AA0002"/></style:style></office:automatic-styles>` +
		`<office:body><office:text>` +
		`<text:tracked-changes><text:changed-region text:id="ct1"><text:insertion><office:change-info><dc:creator>Test Reviser</dc:creator><dc:date>2001-02-03T04:05:09</dc:date></office:change-info></text:insertion></text:changed-region></text:tracked-changes>` +
		`<text:p>Visible body text</text:p>` +
		`<text:p><office:annotation><dc:creator>Test Commenter</dc:creator><dc:date>2001-02-03T04:05:10</dc:date><meta:creator-initials>TC</meta:creator-initials><text:p>Annotation body text</text:p></office:annotation></text:p>` +
		`<text:p><draw:frame><draw:image xlink:href="Pictures/image1.jpg" xlink:type="simple"/></draw:frame></text:p>` +
		`<text:p><draw:frame><draw:image xlink:href="file:///C:/Users/testuser/linked.png" xlink:type="simple"/></draw:frame></text:p>` +
		`<text:p><draw:frame><draw:image><office:binary-data>RkFLRUlNRyBwaXhlbHMgR1BTOjQ4Ljg1ODQsMi4yOTQ1</office:binary-data></draw:image></draw:frame></text:p>` +
		`</office:text></office:body></office:document-content>`

	odfStyles = xmlDecl + `<office:document-styles ` + odfNS + ` office:version="1.3"><office:master-styles><text:p><office:annotation><dc:creator>Test Style Author</dc:creator></office:annotation>Header text</text:p></office:master-styles></office:document-styles>`

	odfSettings = xmlDecl + `<office:document-settings ` + odfNS + ` office:version="1.3"><office:settings><config:config-item-set config:name="ooo:configuration-settings">` +
		`<config:config-item config:name="PrinterName" config:type="string">Test Printer</config:config-item>` +
		`<config:config-item config:name="PrinterSetup" config:type="base64Binary">VGVzdCBQcml2YXRlIFNldHVw</config:config-item>` +
		`<config:config-item config:name="Rsid" config:type="int">11223344</config:config-item>` +
		`<config:config-item config:name="ZoomFactor" config:type="short">100</config:config-item>` +
		`</config:config-item-set></office:settings></office:document-settings>`

	odfManifest = xmlDecl + `<manifest:manifest xmlns:manifest="` + nsManifest + `" manifest:version="1.3">` +
		`<manifest:file-entry manifest:full-path="/" manifest:version="1.3" manifest:media-type="application/vnd.oasis.opendocument.text"/>` +
		`<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="styles.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="meta.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="settings.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="Thumbnails/thumbnail.png" manifest:media-type="image/png"/>` +
		`<manifest:file-entry manifest:full-path="Thumbnails/" manifest:media-type=""/>` +
		`<manifest:file-entry manifest:full-path="Pictures/image1.jpg" manifest:media-type="image/jpeg"/>` +
		`<manifest:file-entry manifest:full-path="Pictures/image2.emf" manifest:media-type="image/x-emf"/>` +
		`<manifest:file-entry manifest:full-path="Object 1/content.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="Object 1/meta.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="Object 1/settings.xml" manifest:media-type="text/xml"/>` +
		`<manifest:file-entry manifest:full-path="Object 1/" manifest:media-type="application/vnd.oasis.opendocument.spreadsheet"/>` +
		`<manifest:file-entry manifest:full-path="META-INF/documentsignatures.xml" manifest:media-type=""/>` +
		`<manifest:file-entry manifest:full-path="VersionList.xml" manifest:media-type=""/>` +
		`<manifest:file-entry manifest:full-path="Versions/Version1" manifest:media-type=""/>` +
		`</manifest:manifest>`
)

func odfParts(mime string) []part {
	return []part{
		{mimetypeName, mime},
		{"content.xml", odfContent},
		{"styles.xml", odfStyles},
		{"meta.xml", odfMeta},
		{"settings.xml", odfSettings},
		{"Thumbnails/", ""},
		{"Thumbnails/thumbnail.png", "Test Thumbnail Secret"},
		{"Pictures/image1.jpg", fakeImageDirty},
		{"Pictures/image2.emf", "EMF test picture"},
		{"Object 1/content.xml", xmlDecl + `<office:document-content ` + odfNS + `><office:body><text:p><office:annotation><dc:creator>Test Nested Author</dc:creator></office:annotation>Nested text</text:p></office:body></office:document-content>`},
		{"Object 1/meta.xml", xmlDecl + `<office:document-meta ` + odfNS + ` office:version="1.3"><office:meta><dc:creator>Test Nested Meta</dc:creator></office:meta></office:document-meta>`},
		{"Object 1/settings.xml", xmlDecl + `<office:document-settings ` + odfNS + `><office:settings><config:config-item config:name="PrinterName" config:type="string">Test Nested Printer</config:config-item></office:settings></office:document-settings>`},
		{"META-INF/documentsignatures.xml", xmlDecl + `<document-signatures><signature>Test Signer Certificate</signature></document-signatures>`},
		{"VersionList.xml", xmlDecl + `<VL:version-list xmlns:VL="http://openoffice.org/2001/versions-list"><VL:version-entry VL:title="Version1" VL:creator="Test Version Author"/></VL:version-list>`},
		{"Versions/Version1", "Test Old Version Content"},
		{"META-INF/manifest.xml", odfManifest},
	}
}

const odtMime = "application/vnd.oasis.opendocument.text"

func odt(t testing.TB) []byte { return zipOf(t, odfParts(odtMime)...) }

var odfSecrets = []string{
	"Test Author", "Test Editor", "Test Title", "Test Description", "Test Subject", "Test Keyword",
	"Test Generator", "Test Printer", "Test Secret Name", "test.user@example.invalid", "2001-02-03",
	"Test Reviser", "Test Commenter", "Test Style Author", "Test Nested", "VGVzdCBQcml2YXRlIFNldHVw", "11223344",
	"Test Thumbnail", "Test Signer", "Test Version", "Test Old Version", "00AA000", "rsid=", "Thumbnails",
	"documentsignatures", "VersionList", "GPS:", "UEdTOjQ4", "R1BTOjQ4",
}

// replacePart swaps a part's body in a fixture.
func replacePart(parts []part, name, body string) []part {
	out := append([]part(nil), parts...)
	for i := range out {
		if out[i].name == name {
			out[i].body = body
			return out
		}
	}
	return append(out, part{name, body})
}
