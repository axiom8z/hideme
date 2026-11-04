package office

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/xmp"
)

var ooxml = ooxmlHandler{limits: defaultLimits}

// rawNames lists what must be gone from the output's ZIP directory, the only part
// of the output that Contract's raw byte search can see through compression.
func rawNames(names ...string) [][]byte {
	out := make([][]byte, len(names))
	for i, n := range names {
		out[i] = []byte(n)
	}
	return out
}

var docPropFields = []string{
	"Author", "Last modified by", "Created", "Modified", "Last printed", "Revision number",
	"Title", "Subject", "Keywords", "Description", "Category", "Content status", "Language", "Version",
	"Application", "Application version", "Company", "Manager", "Total editing time", "Template name",
	"Hyperlink base", "Titles of parts", "Document statistics", "Custom property", "Thumbnail", "ZIP entry times",
}

func TestContract(t *testing.T) {
	coretest.Contract(t, ooxml, []coretest.Case{
		{
			Name: "docx",
			Data: docx(t),
			WantFields: append(slices.Clone(docPropFields),
				"Custom XML data", "Printer settings", "Attached template", "People list", "Comment tracking data",
				"Editing session IDs", "Comment author", "Comment initials", "Comment date",
				"Tracked-change author", "Tracked-change date", "Document ID", "Content control data binding",
				"GPS coordinates"),
			WantRetained:   []string{"External file link"},
			MustNotContain: rawNames("docProps/custom.xml", "docProps/thumbnail", "customXml/", "printerSettings", "people.xml", "commentsIds", "commentsExtensible", "settings.xml.rels"),
			MustContain:    rawNames("word/document.xml", "word/media/image1.jpeg", "word/commentsExtended.xml", "docProps/core.xml", "docProps/app.xml"),
		},
		{
			Name:       "docm",
			Data:       zipOf(t, append(docxParts("application/vnd.ms-word.document.macroEnabled.main+xml"), part{"word/vbaProject.bin", "test macros"})...),
			WantFields: []string{"Author"},
		},
		{
			Name: "xlsx",
			Data: xlsx(t),
			WantFields: append(slices.Clone(docPropFields),
				"Workbook path", "Excel version", "Workbook revision ID", "Revision IDs", "Shared workbook user",
				"Comment author", "Threaded comment person", "Threaded comment date",
				"Pivot cache refreshed by", "Pivot cache refresh date", "Printer settings", "Custom XML data",
				"Shared workbook history"),
			WantRetained:   []string{"External file link"},
			MustNotContain: rawNames("docProps/custom.xml", "printerSettings", "customXml/", "xl/revisions/"),
			MustContain:    rawNames("xl/workbook.xml", "xl/persons/person.xml", "xl/threadedComments/threadedComment1.xml"),
		},
		{
			Name: "pptx",
			Data: pptx(t),
			WantFields: append(slices.Clone(docPropFields),
				"Comment author", "Comment initials", "Comment date", "Comment author account", "Comment time zone",
				"Presentation revision data", "Printer settings", "GPS coordinates"),
			MustNotContain: rawNames("revisionInfo", "changesInfos", "printerSettings", "docProps/thumbnail"),
			MustContain:    rawNames("ppt/commentAuthors.xml", "ppt/authors.xml", "ppt/slides/slide1.xml"),
		},
	})
}

func TestRobustness(t *testing.T) {
	cases := map[string][]byte{"docx": docx(t), "xlsx": xlsx(t), "pptx": pptx(t)}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) { coretest.Robustness(t, ooxml, data) })
	}
}

func TestDocxIsClean(t *testing.T) {
	data := docx(t)
	out, rep := clean(t, ooxml, data)
	parts := unzip(t, out)

	assertAbsent(t, parts, docxSecrets...)
	assertGone(t, parts,
		"docProps/custom.xml", "docProps/thumbnail.jpeg", "customXml/item1.xml", "customXml/itemProps1.xml",
		"customXml/_rels/item1.xml.rels", "word/people.xml", "word/commentsIds.xml", "word/commentsExtensible.xml",
		"word/printerSettings/printerSettings1.bin", "word/_rels/settings.xml.rels")
	assertConsistent(t, parts)
	assertPresent(t, parts, "word/document.xml",
		"Visible body text", "Inserted text", "Deleted text", "Moved text", "Bound text",
		`w:author="Author"`, `<w:pgSz w:w="12240" w:h="15840"/>`, `r:embed="rIdImage"`)
	assertPresent(t, parts, "word/comments.xml", "Comment body text", `w:author="Author"`, `w:initials="A"`)
	assertPresent(t, parts, "word/settings.xml", `<w:zoom w:percent="100"/>`, `<w:defaultTabStop w:val="720"/>`)
	assertPresent(t, parts, "word/styles.xml", `w:styleId="Normal"`)
	assertPresent(t, parts, "word/media/image1.jpeg", fakeImageClean)
	assertPresent(t, parts, "word/media/image2.emf", "EMF test picture")
	assertPresent(t, parts, "docProps/core.xml", "coreProperties")
	assertPresent(t, parts, "docProps/app.xml", "<Properties")

	if !hasValue(rep, "External file link", "linkuser") {
		t.Errorf("the retained link does not show its path: %q", values(rep, "External file link"))
	}
	if !hasValue(rep, "Author", "Test Author") || !hasValue(rep, "Company", "Test Company") {
		t.Errorf("properties are not reported with their values: %+v", rep.Findings)
	}
	if !hasValue(rep, "Custom property", "Test Secret Name = test.user@example.invalid") {
		t.Errorf("custom property value missing: %q", values(rep, "Custom property"))
	}
	if !hasValue(rep, "Printer settings", "Test Printer") {
		t.Errorf("printer name not decoded from DEVMODE: %q", values(rep, "Printer settings"))
	}
	if !hasValue(rep, "Attached template", `C:\Users\testuser`) {
		t.Errorf("template path not reported: %q", values(rep, "Attached template"))
	}
	if !hasValue(rep, "People list", "Test Person") {
		t.Errorf("people not reported: %q", values(rep, "People list"))
	}
	if !hasValue(rep, "Custom XML data", "sharepoint.user@example.invalid") {
		t.Errorf("custom XML not reported: %q", values(rep, "Custom XML data"))
	}
	for _, want := range []string{"image2.emf", "oleObject1.bin", "tracked changes", "comment text remains", "local file path"} {
		if !hasWarning(rep, want) {
			t.Errorf("no warning mentioning %q: %q", want, rep.Warnings)
		}
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "linkuser") {
			t.Errorf("a warning prints a path, and warnings reach clean's logs: %q", w)
		}
	}
}

// TestBodyTextIsNeverEdited is the reason the editor scans tags instead of
// searching text. Every line of this document spells a real rule's target in a
// place that is not a tag, and all of it must come through byte for byte.
func TestBodyTextIsNeverEdited(t *testing.T) {
	const traps = `<w:p><w:r><w:t>w:author="x" w:date="2001" w:rsidR="00FF0001" r:id="rIdPrinter"</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t><![CDATA[<w:comment w:author="x" w:date="y" w:rsidR="z"/><w:rsids/>]]></w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>&lt;w:ins w:author=&quot;x&quot;&gt;</w:t></w:r></w:p>` +
		`<w:p w:customAttr='w:author="x"' xml:lang="en"><w:r><w:t xml:space="preserve">  a > b  </w:t></w:r></w:p>`
	doc := xmlDecl + `<w:document ` + wordNS + `><w:body>` + traps + `</w:body></w:document>`
	data := zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", doc)...)
	out, _ := clean(t, ooxml, data)
	got := string(unzip(t, out)["word/document.xml"])
	if !strings.Contains(got, traps) {
		t.Errorf("body text changed:\n got %s\nwant %s", got, traps)
	}
}

func TestContentSurvives(t *testing.T) {
	for name, tc := range map[string]struct {
		data  []byte
		parts []string
	}{
		"docx": {docx(t), []string{"word/document.xml", "word/comments.xml"}},
		"xlsx": {xlsx(t), []string{"xl/sharedStrings.xml", "xl/worksheets/sheet1.xml", "xl/threadedComments/threadedComment1.xml"}},
		"pptx": {pptx(t), []string{"ppt/slides/slide1.xml", "ppt/comments/comment1.xml"}},
	} {
		t.Run(name, func(t *testing.T) {
			before := unzip(t, tc.data)
			out, _ := clean(t, ooxml, tc.data)
			after := unzip(t, out)
			for _, part := range tc.parts {
				if !slices.Equal(textOf(t, before[part]), textOf(t, after[part])) {
					t.Errorf("%s text changed:\n got %q\nwant %q", part, textOf(t, after[part]), textOf(t, before[part]))
				}
			}
		})
	}
}

func TestXlsxIsClean(t *testing.T) {
	out, rep := clean(t, ooxml, xlsx(t))
	parts := unzip(t, out)

	assertAbsent(t, parts,
		"Test Author", "Test Company", "testuser\\", "absPath", "AlternateContent><mc:Choice Requires=\"x15ac\"",
		"lastEdited", "lowestEdited", "rupBuild", "TEST-CODE-GUID", "revisionPtr", "TEST-REV-GUID", "TEST-SHEET-UID",
		"Test Sharer", "Test Commenter", "Second Commenter", "Test Person", "test.person@", "providerId=\"AD\"",
		"2001-02-03", "Test Refresher", "refreshedDate", "Test Revision User", "printerSettings", "rIdPrinter", "sharepoint.user@")
	assertGone(t, parts, "xl/printerSettings/printerSettings1.bin", "xl/revisions/userNames.xml", "customXml/item1.xml")
	assertConsistent(t, parts)
	assertPresent(t, parts, "xl/workbook.xml", `appName="xl"`, `<sheet name="Sheet1" sheetId="1" r:id="rIdSheet"/>`)
	assertPresent(t, parts, "xl/comments1.xml", "<author>Author</author><author>Author</author>", "Cell comment text")
	assertPresent(t, parts, "xl/persons/person.xml", `displayName="Author"`, `userId="Author"`, `providerId="None"`, `id="{00000000-0000-0000-0000-000000000001}"`)
	assertPresent(t, parts, "xl/threadedComments/threadedComment1.xml", "Threaded comment text", `personId="{00000000-0000-0000-0000-000000000001}"`)
	assertPresent(t, parts, "xl/pivotCache/pivotCacheDefinition1.xml", `recordCount="1"`)
	if !hasValue(rep, "Workbook path", `C:\Users\testuser\Desktop\`) {
		t.Errorf("workbook path not reported: %q", values(rep, "Workbook path"))
	}
}

// TestPageSetupKeepsEverythingButTheReference covers the classic trap. Removing the
// printer settings part while pageSetup still names it makes Excel show a repair
// error, and removing the whole element would lose the page layout.
func TestPageSetupKeepsEverythingButTheReference(t *testing.T) {
	out, _ := clean(t, ooxml, xlsx(t))
	sheet := string(unzip(t, out)["xl/worksheets/sheet1.xml"])
	for _, want := range []string{`<pageSetup orientation="landscape"/>`, `<legacyDrawing r:id="rIdKeep"/>`} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet lacks %s:\n%s", want, sheet)
		}
	}
	// An AlternateContent block that holds no path is real content.
	if !strings.Contains(sheet, `<mc:AlternateContent><mc:Choice Requires="x14"><controls/></mc:Choice></mc:AlternateContent>`) {
		t.Errorf("an unrelated AlternateContent block was removed:\n%s", sheet)
	}
}

func TestPptxIsClean(t *testing.T) {
	out, _ := clean(t, ooxml, pptx(t))
	parts := unzip(t, out)
	assertAbsent(t, parts,
		"Test Author", "Test Commenter", "test.person@", "presenceInfo", "timeZoneBias", "2001-02-03",
		"Test Modern Author", "test.modern@", "Test Changer", "TEST-CLIENT", "printerSettings", "rIdPrinter", "GPS:")
	assertGone(t, parts, "ppt/revisionInfo.xml", "ppt/changesInfos/changesInfo1.xml", "ppt/printerSettings/printerSettings1.bin", "docProps/thumbnail.jpeg")
	assertConsistent(t, parts)
	assertPresent(t, parts, "ppt/slides/slide1.xml", "Visible slide text")
	assertPresent(t, parts, "ppt/commentAuthors.xml", `name="Author"`, `initials="A"`, `id="1"`, `lastIdx="1"`)
	assertPresent(t, parts, "ppt/comments/comment1.xml", "Slide comment text", `authorId="1"`)
	assertPresent(t, parts, "ppt/authors.xml", `name="Author"`, `userId="Author"`, `providerId="None"`)
	assertPresent(t, parts, "ppt/comments/modernComment1.xml", `created="`+placeholderDate+`"`)
}

// TestPrefixesDoNotMatter: rules follow namespaces, because any writer may choose
// other prefixes, and a hostile one will.
func TestPrefixesDoNotMatter(t *testing.T) {
	doc := xmlDecl + `<ns0:document xmlns:ns0="` + nsWord + `" xmlns:ns1="` + nsRels + `"><ns0:body>` +
		`<ns0:p ns0:rsidR="00DD0001"><ns0:ins ns0:author="Test Reviser" ns0:date="2001-02-03T04:05:09Z"/></ns0:p>` +
		`<ns0:sectPr><ns0:printerSettings ns1:id="rIdPrinter"/></ns0:sectPr>` +
		`</ns0:body></ns0:document>`
	data := zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", doc)...)
	out, rep := clean(t, ooxml, data)
	parts := unzip(t, out)
	assertAbsent(t, parts, "00DD0001", "Test Reviser", "2001-02-03", "printerSettings")
	assertConsistent(t, parts)
	if !hasValue(rep, "Tracked-change author", "Test Reviser") {
		t.Errorf("author under a renamed prefix not reported: %+v", rep.Findings)
	}
}

// TestStrictNamespaces covers ISO Strict documents, which use different namespaces
// and relationship types for the same things.
func TestStrictNamespaces(t *testing.T) {
	sheet := xmlDecl + `<worksheet xmlns="` + nsSheetStrict + `" xmlns:r="` + nsRelsStrict + `"><sheetData/><pageSetup orientation="landscape" r:id="rId1"/></worksheet>`
	workbook := xmlDecl + `<workbook xmlns="` + nsSheetStrict + `" xmlns:r="` + nsRelsStrict + `"><fileVersion appName="xl" rupBuild="24312"/><sheets><sheet name="S" sheetId="1" r:id="rId1"/></sheets></workbook>`
	const strictRel = "http://purl.oclc.org/ooxml/officeDocument/relationships/"
	data := zipOf(t,
		part{"[Content_Types].xml", contentTypes(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)},
		part{"_rels/.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rId1" Type="` + strictRel + `officeDocument" Target="xl/workbook.xml"/>` +
			`<Relationship Id="rId2" Type="` + strictRel + `extendedProperties" Target="docProps/app.xml"/></Relationships>`},
		part{"docProps/app.xml", xmlDecl + `<Properties xmlns="http://purl.oclc.org/ooxml/officeDocument/extendedProperties"><Company>Test Company</Company></Properties>`},
		part{"xl/workbook.xml", workbook},
		part{"xl/_rels/workbook.xml.rels", xmlDecl + relPkgRoot + `<Relationship Id="rId1" Type="` + strictRel + `worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
		part{"xl/worksheets/sheet1.xml", sheet},
		part{"xl/worksheets/_rels/sheet1.xml.rels", xmlDecl + relPkgRoot + `<Relationship Id="rId1" Type="` + strictRel + `printerSettings" Target="../printerSettings/p.bin"/></Relationships>`},
		part{"xl/printerSettings/p.bin", devmode()},
	)
	out, _ := clean(t, ooxml, data)
	parts := unzip(t, out)
	assertAbsent(t, parts, "Test Company", "rupBuild", "printerSettings", "Test Printer")
	assertPresent(t, parts, "xl/worksheets/sheet1.xml", `<pageSetup orientation="landscape"/>`)
}

// TestPartsAreKnownByRoleNotName: OPC allows any part name, and .NET's packaging
// library really does put core properties in a guid-named .psmdcp file.
func TestPartsAreKnownByRoleNotName(t *testing.T) {
	const guidName = "package/services/metadata/core-properties/1234abcd.psmdcp"
	base := docxParts(docxMain)
	parts := make([]part, 0, len(base))
	for _, p := range base {
		switch p.name {
		case "docProps/core.xml":
			p.name = guidName
		case "[Content_Types].xml":
			p.body = strings.Replace(p.body, `<Override PartName="/docProps/core.xml"`, `<Override PartName="/`+guidName+`"`, 1)
		case "_rels/.rels":
			p.body = strings.Replace(p.body, `Target="docProps/core.xml"`, `Target="/`+guidName+`"`, 1)
		case "docProps/thumbnail.jpeg":
			p.name = "preview/snapshot.jpeg"
		}
		parts = append(parts, p)
	}
	parts = replacePart(parts, "_rels/.rels", strings.Replace(partBody(parts, "_rels/.rels"), `Target="docProps/thumbnail.jpeg"`, `Target="preview/snapshot.jpeg"`, 1))
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	assertAbsent(t, got, "Test Author", "Test Thumbnail")
	assertConsistent(t, got)
	assertPresent(t, got, guidName, "coreProperties")
	assertGone(t, got, "preview/snapshot.jpeg")
	if !hasValue(rep, "Author", "Test Author") {
		t.Errorf("core properties under another name were not recognised: %+v", rep.Findings)
	}
}

func partBody(parts []part, name string) string {
	for _, p := range parts {
		if p.name == name {
			return p.body
		}
	}
	return ""
}

// TestAttributesQualifiedTheWrongWay: neither spelling below is what the schema says,
// so Office would ignore them, but the values would still be in the file.
func TestAttributesQualifiedTheWrongWay(t *testing.T) {
	doc := xmlDecl + `<w:document ` + wordNS + `><w:body><w:ins author="Test Reviser" date="2001-02-03T04:05:09Z" rsidR="00DD0001"/></w:body></w:document>`
	out, _ := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", doc)...))
	assertAbsent(t, unzip(t, out), "Test Reviser", "2001-02-03", "00DD0001")

	pivot := xmlDecl + `<x:pivotCacheDefinition xmlns:x="` + nsSheet + `" x:refreshedBy="Test Refresher"/>`
	out, _ = clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/pivotCache/pivotCacheDefinition1.xml", pivot)...))
	assertAbsent(t, unzip(t, out), "Test Refresher")
}

// TestSpellingsOfPartNames: OPC part names are case-insensitive and percent-encoded,
// so each of these names the same part as far as Office is concerned.
func TestSpellingsOfPartNames(t *testing.T) {
	for _, name := range []string{"DOCPROPS/CUSTOM.XML", "docProps/custom%2Exml"} {
		t.Run(name, func(t *testing.T) {
			base := docxParts(docxMain)
			parts := make([]part, 0, len(base))
			for _, p := range base {
				switch p.name {
				case "docProps/custom.xml":
					p.name = name
				case "[Content_Types].xml":
					p.body = strings.Replace(p.body, `/docProps/custom.xml"`, `/`+name+`"`, 1)
				case "_rels/.rels":
					p.body = strings.Replace(p.body, `Target="docProps/custom.xml"`, `Target="`+name+`"`, 1)
				}
				parts = append(parts, p)
			}
			out, _ := clean(t, ooxml, zipOf(t, parts...))
			got := unzip(t, out)
			assertAbsent(t, got, "Test Secret Name", "custom")
		})
	}
}

func TestMediaFoundByRoleNotFolder(t *testing.T) {
	base := docxParts(docxMain)
	parts := make([]part, 0, len(base))
	for _, p := range base {
		switch p.name {
		case "word/media/image1.jpeg":
			p.name = "word/pics/a.jpeg"
		case "word/_rels/document.xml.rels":
			p.body = strings.Replace(p.body, `Target="media/image1.jpeg"`, `Target="pics/a.jpeg"`, 1)
		}
		parts = append(parts, p)
	}
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	assertAbsent(t, unzip(t, out), "GPS:")
	if !hasValue(rep, "GPS coordinates", "48.8584") {
		t.Errorf("nested finding missing: %+v", rep.Findings)
	}
}

func TestReferenceToRemovedPartFromUnknownPlaceFails(t *testing.T) {
	// The thumbnail is also used as a picture in the body. Removing the part would
	// break the picture, and keeping it would keep the thumbnail, so the file is
	// refused rather than damaged or left half clean.
	parts := replacePart(docxParts(docxMain), "word/_rels/document.xml.rels", xmlDecl+relPkgRoot+
		`<Relationship Id="rIdImage" Type="`+relType+`image" Target="../docProps/thumbnail.jpeg"/>`+
		`<Relationship Id="rIdPrinter" Type="`+relType+`printerSettings" Target="printerSettings/printerSettings1.bin"/></Relationships>`)
	_, err := ooxml.Clean(context.Background(), core.BytesSource(zipOf(t, parts...)), &bytes.Buffer{}, core.Options{})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestEmbeddedDocumentsRecurse(t *testing.T) {
	inner := zipOf(t, replacePart(docxParts(docxMain), "docProps/core.xml", strings.ReplaceAll(coreXML, "Test Author", "Test Inner Author"))...)
	parts := append(docxParts(docxMain), part{"word/embeddings/Document1.docx", string(inner)})
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	embedded := unzip(t, unzip(t, out)["word/embeddings/Document1.docx"])
	assertAbsent(t, embedded, "Test Inner Author", "GPS:")
	if !hasValue(rep, "Author", "Test Inner Author") {
		t.Errorf("nested author not reported: %q", values(rep, "Author"))
	}
	found := false
	for _, f := range rep.Findings {
		if f.Value == "Test Inner Author" && strings.HasPrefix(f.Where, "word/embeddings/Document1.docx/") {
			found = true
		}
	}
	if !found {
		t.Errorf("nested finding does not say where it came from: %+v", rep.Findings)
	}
}

func TestDocumentNestedTooDeepFails(t *testing.T) {
	data := docx(t)
	for range core.MaxNestedDepth + 1 {
		data = zipOf(t, append(docxParts(docxMain), part{"word/embeddings/Inner.docx", string(data)})...)
	}
	_, err := ooxml.Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{})
	if !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestNestedErrorFailsWholeFile(t *testing.T) {
	data := zipOf(t, replacePart(docxParts(docxMain), "word/media/image1.jpeg", fakeImageBad)...)
	for _, h := range []func() error{
		func() error {
			_, err := ooxml.Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{})
			return err
		},
		func() error {
			_, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{})
			return err
		},
	} {
		if err := h(); !errors.Is(err, core.ErrMalformed) {
			t.Errorf("got %v, want ErrMalformed", err)
		}
	}
}

func TestMacrosAreKeptWithAWarning(t *testing.T) {
	parts := append(docxParts("application/vnd.ms-word.document.macroEnabled.main+xml"), part{"word/vbaProject.bin", "test macros"})
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	assertPresent(t, unzip(t, out), "word/vbaProject.bin", "test macros")
	if !hasWarning(rep, "macros") {
		t.Errorf("no macro warning: %q", rep.Warnings)
	}
	if rep.Format != "docm" {
		t.Errorf("Format = %q, want docm", rep.Format)
	}
}

func TestFormatsDetected(t *testing.T) {
	for contentType, want := range ooxmlFormats {
		data := zipOf(t, docxParts(contentType)...)
		rep, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{})
		if err != nil {
			t.Errorf("%s: %v", want, err)
			continue
		}
		if rep.Format != want {
			t.Errorf("Format = %q, want %q", rep.Format, want)
		}
		ext := "." + want
		if !slices.Contains(ooxml.Format().Extensions, ext) {
			t.Errorf("%s is detected but not listed in Extensions", ext)
		}
	}
}

func TestUnknownPackageTypesAreRefused(t *testing.T) {
	// Visio and XPS are OPC packages too, but their parts mean nothing to the rules.
	data := zipOf(t, docxParts("application/vnd.ms-visio.drawing.main+xml")...)
	_, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestStrayParts(t *testing.T) {
	parts := append(docxParts(docxMain),
		part{"word/userdata_history.xml", xmlDecl + `<root/>`},
		part{"hidden/secret.dat", "unreferenced data"},
		part{"[trash]/0000.dat", "Test Trash"},
		part{"_xmlsignatures/sig1.xml", xmlDecl + `<Signature>Test Signer</Signature>`},
	)
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	assertGone(t, got, "[trash]/0000.dat", "_xmlsignatures/sig1.xml")
	if _, ok := got["word/userdata_history.xml"]; !ok {
		t.Errorf("unknown part was not copied")
	}
	for _, want := range []string{"this part is not recognised", "no relationship points", "digital signature"} {
		if !hasWarning(rep, want) {
			t.Errorf("no warning %q: %q", want, rep.Warnings)
		}
	}
	if len(values(rep, "Digital signature")) == 0 || len(values(rep, "Discarded package data")) == 0 {
		t.Errorf("removed parts not reported: %+v", rep.Findings)
	}
}

// TestXMPInAKeptPartIsRemoved uses the shared XMP reader's fixture.
func TestXMPInAKeptPartIsRemoved(t *testing.T) {
	packet := string(coretest.FakeXMP())
	packet = packet[strings.Index(packet, "<x:xmpmeta") : strings.Index(packet, "</x:xmpmeta>")+len("</x:xmpmeta>")]
	doc := xmlDecl + `<w:document ` + wordNS + `><w:body><w:p/>` + packet + `</w:body></w:document>`
	data := zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", doc)...)
	out, rep := clean(t, ooxml, data)
	got := unzip(t, out)["word/document.xml"]
	if xmp.Contains(got) {
		t.Errorf("XMP survives: %s", got)
	}
	if !hasValue(rep, "GPS coordinates", coretest.EiffelTower) {
		t.Errorf("XMP was not decoded: %+v", rep.Findings)
	}
}

func TestPicturePathsAreBlanked(t *testing.T) {
	const a = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`
	doc := xmlDecl + `<w:document ` + wordNS + ` ` + a + ` xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"><w:body><w:p><w:r><w:drawing>` +
		`<wp:docPr id="1" name="Picture 1" descr="C:\\Users\\testuser\\Pictures\\holiday.jpg"/>` +
		`<a:cNvPr id="2" name="file:///C:/Users/testuser/b.png" title="/home/testuser/c.png"/>` +
		`<a:cNvPr id="3" name="Sunset" descr="A sunset over the sea: red and gold"/>` +
		`</w:drawing></w:r></w:p></w:body></w:document>`
	out, rep := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", doc)...))
	got := unzip(t, out)["word/document.xml"]
	if bytes.Contains(got, []byte("testuser")) {
		t.Errorf("a path survives: %s", got)
	}
	for _, want := range []string{`name="Picture 1" descr=""`, `name="Picture" title=""`, `descr="A sunset over the sea: red and gold"`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if !hasValue(rep, "Picture file path", "holiday.jpg") {
		t.Errorf("not reported: %+v", rep.Findings)
	}
}

func TestFieldCodesWithPathsWarn(t *testing.T) {
	for name, doc := range map[string]string{
		"complex": `<w:p><w:r><w:instrText>INCLUDEPICTURE "C:\\\\Users\\\\testuser\\\\a.png"</w:instrText></w:r></w:p>`,
		"simple":  `<w:fldSimple w:instr=" LINK Excel.Sheet file:///C:/x.xlsx "/>`,
	} {
		t.Run(name, func(t *testing.T) {
			full := xmlDecl + `<w:document ` + wordNS + `><w:body>` + doc + `</w:body></w:document>`
			_, rep := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", full)...))
			if !hasWarning(rep, "field code") {
				t.Errorf("no warning: %q", rep.Warnings)
			}
		})
	}
	plain := xmlDecl + `<w:document ` + wordNS + `><w:body><w:p><w:r><w:instrText> PAGE </w:instrText></w:r></w:p></w:body></w:document>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", plain)...))
	if hasWarning(rep, "field code") {
		t.Errorf("a harmless field warned: %q", rep.Warnings)
	}
}

func TestSensitivityLabelIsRemoved(t *testing.T) {
	base := docxParts(docxMain)
	parts := replacePart(base, "_rels/.rels", strings.Replace(partBody(base, "_rels/.rels"), "</Relationships>",
		`<Relationship Id="rIdLabel" Type="http://schemas.microsoft.com/office/2020/02/relationships/classificationlabels" Target="docMetadata/LabelInfo.xml"/></Relationships>`, 1))
	parts = replacePart(parts, "[Content_Types].xml", strings.Replace(partBody(parts, "[Content_Types].xml"), "</Types>",
		`<Override PartName="/docMetadata/LabelInfo.xml" ContentType="application/vnd.ms-office.classificationlabels+xml"/></Types>`, 1))
	parts = append(parts, part{"docMetadata/LabelInfo.xml", xmlDecl + `<clbl:labelList xmlns:clbl="http://schemas.microsoft.com/office/2020/mipLabelMetadata"><clbl:label id="{TEST}" siteId="{TEST-TENANT}"/></clbl:labelList>`})
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	assertGone(t, got, "docMetadata/LabelInfo.xml")
	assertAbsent(t, got, "TEST-TENANT", "LabelInfo", "classificationlabels")
	assertConsistent(t, got)
	if len(values(rep, "Sensitivity label")) == 0 {
		t.Errorf("not reported: %+v", rep.Findings)
	}
}

func TestVMLThatIsNotXMLIsKept(t *testing.T) {
	// Office writes unclosed <br> tags in VML, so refusing this would refuse real
	// workbooks. It has to be kept, and the report has to admit it was not read.
	const vml = `<xml xmlns:v="urn:schemas-microsoft-com:vml"><v:textbox><br></v:textbox></xml>`
	out, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/drawings/vmlDrawing1.vml", vml)...))
	if got := string(unzip(t, out)["xl/drawings/vmlDrawing1.vml"]); got != vml {
		t.Errorf("VML changed: %s", got)
	}
	if !hasWarning(rep, "VML") {
		t.Errorf("no VML warning: %q", rep.Warnings)
	}
}

// TestBrokenVMLReportsNoEditItDidNotMake: the path comes before the damage, so a
// report built as the scan goes along would claim to have blanked it.
func TestBrokenVMLReportsNoEditItDidNotMake(t *testing.T) {
	const vml = `<xml xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="` + nsVMLOffice + `"><v:imagedata o:title="C:\\Users\\testuser\\a.png"/><v:textbox><br></v:textbox></xml>`
	data := zipOf(t, replacePart(xlsxParts(), "xl/drawings/vmlDrawing1.vml", vml)...)
	out, rep := clean(t, ooxml, data)
	if len(values(rep, "Picture file path")) != 0 {
		t.Errorf("reports an edit it could not make: %+v", rep.Findings)
	}
	verify, err := ooxml.Inspect(context.Background(), core.BytesSource(out), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r := verify.Residual(); len(r) != 0 {
		t.Errorf("residual findings: %+v", r)
	}
	const wellFormed = `<xml xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="` + nsVMLOffice + `"><v:imagedata o:title="C:\\Users\\testuser\\a.png"/></xml>`
	cleaned, rep2 := cleanVML(t, wellFormed)
	if strings.Contains(cleaned, "testuser") || len(values(rep2, "Picture file path")) != 1 {
		t.Errorf("well-formed VML: %s, %+v", cleaned, rep2.Findings)
	}
}

func cleanVML(t *testing.T, vml string) (string, *core.Report) {
	t.Helper()
	out, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/drawings/vmlDrawing1.vml", vml)...))
	return string(unzip(t, out)["xl/drawings/vmlDrawing1.vml"]), rep
}

func TestUTF16PartsAreCleaned(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-16"?><w:document ` + wordNS + `><w:body><w:p w:rsidR="00EE0001"><w:ins w:author="Test Wide Author"/><w:r><w:t>Wide text é</w:t></w:r></w:p></w:body></w:document>`
	wide := []byte{0xFF, 0xFE}
	for _, unit := range utf16.Encode([]rune(doc)) {
		wide = append(wide, byte(unit), byte(unit>>8))
	}
	out, rep := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", string(wide))...))
	got := unzip(t, out)["word/document.xml"]
	for _, secret := range []string{"00EE0001", "Test Wide Author"} {
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("%q survives in %s", secret, got)
		}
	}
	if !bytes.Contains(got, []byte(`encoding="UTF-8"`)) || !bytes.Contains(got, []byte("Wide text é")) {
		t.Errorf("not converted to UTF-8 properly: %s", got)
	}
	if !hasValue(rep, "Tracked-change author", "Test Wide Author") {
		t.Errorf("author in UTF-16 not reported: %+v", rep.Findings)
	}
}

func TestPolyglotBelongsToNeitherHandler(t *testing.T) {
	data := zipOf(t, append(docxParts(docxMain), part{mimetypeName, odtMime})...)
	for _, h := range []core.Handler{ooxml, odfHandler{limits: defaultLimits}} {
		if h.Match(data, core.BytesSource(data)) {
			t.Errorf("%s claims a file that is both OOXML and ODF", h.Format().ID)
		}
		if _, err := h.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("%s: got %v, want ErrUnsupported", h.Format().ID, err)
		}
	}
}

func TestMatch(t *testing.T) {
	plain := zipOf(t, part{"hello.txt", "hello"})
	epub := zipOf(t, part{mimetypeName, "application/epub+zip"}, part{"content.xml", "<a/>"})
	for name, tc := range map[string]struct {
		data []byte
		want bool
	}{
		"docx":      {docx(t), true},
		"xlsx":      {xlsx(t), true},
		"pptx":      {pptx(t), true},
		"odt":       {odt(t), false},
		"plain zip": {plain, false},
		"epub":      {epub, false},
		"not zip":   {[]byte("<?xml version='1.0'?><a/>"), false},
		"empty":     {nil, false},
	} {
		if got := ooxml.Match(tc.data, core.BytesSource(tc.data)); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", name, got, tc.want)
		}
	}
}

// TestDocumentGluedToAnotherFormat: JPEG, PNG and PDF readers ignore what follows
// the file, so a real picture can carry a whole document after it. That file is a
// picture, and its handler removes the trailer.
func TestDocumentGluedToAnotherFormat(t *testing.T) {
	doc := docx(t)
	for name, prefix := range map[string]string{
		"jpeg": "\xFF\xD8\xFF\xE0fake jpeg",
		"png":  "\x89PNG\r\n\x1a\nfake png",
		"pdf":  "%PDF-1.7 fake pdf",
		"mp3":  "\xFF\xFBfake audio",
		"gif":  "GIF89a fake gif",
		"mp4":  "\x00\x00\x00\x18ftypisom fake video",
		"svg":  "<svg xmlns='x'/>",
	} {
		data := append([]byte(prefix), doc...)
		head := data[:min(len(data), core.HeadSize)]
		if ooxml.Match(head, core.BytesSource(data)) {
			t.Errorf("%s: the document handler claims a %s with a document on its end", name, name)
		}
	}
}

// TestThreadedCommentPlaceholderSurvives: Excel gives a threaded comment's legacy
// copy the author tc={guid}, which is the link between the two.
func TestThreadedCommentPlaceholderSurvives(t *testing.T) {
	const tc = "tc={00000000-0000-0000-0000-000000000002}"
	comments := xmlDecl + `<comments xmlns="` + nsSheet + `"><authors><author>` + tc + `</author><author>Test Commenter</author></authors><commentList/></comments>`
	out, _ := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/comments1.xml", comments)...))
	got := string(unzip(t, out)["xl/comments1.xml"])
	if want := `<author>` + tc + `</author><author>Author</author>`; !strings.Contains(got, want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEmptyXMLPartsPassThrough(t *testing.T) {
	out, _ := clean(t, ooxml, zipOf(t, append(docxParts(docxMain), part{"word/empty.xml", ""})...))
	if got, ok := unzip(t, out)["word/empty.xml"]; !ok || len(got) != 0 {
		t.Errorf("empty part: present=%v, %q", ok, got)
	}
}

func TestMainPartUnderAReservedNameKeepsItsRelationships(t *testing.T) {
	// A hostile or eccentric main part name must not cost the document its parts.
	base := docxParts(docxMain)
	var parts []part
	for _, p := range base {
		switch p.name {
		case "word/document.xml":
			p.name = "customXml/main.xml"
		case "word/_rels/document.xml.rels":
			p.name = "customXml/_rels/main.xml.rels"
			p.body = strings.NewReplacer(`Target="`, `Target="../word/`, `Target="../word/../customXml/`, `Target="../customXml/`, `Target="../word/https:`, `Target="https:`, `Target="../word/file:`, `Target="file:`).Replace(p.body)
		case "_rels/.rels":
			p.body = strings.Replace(p.body, `Target="word/document.xml"`, `Target="customXml/main.xml"`, 1)
		case "[Content_Types].xml":
			p.body = strings.Replace(p.body, `/word/document.xml`, `/customXml/main.xml`, 1)
		}
		parts = append(parts, p)
	}
	out, _ := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	if _, ok := got["customXml/main.xml"]; !ok {
		t.Fatalf("the main part was removed")
	}
	if _, ok := got["customXml/_rels/main.xml.rels"]; !ok {
		t.Errorf("the main part's relationships were removed")
	}
}

func TestEmptyPropertiesReportNothing(t *testing.T) {
	data := zipOf(t,
		part{"[Content_Types].xml", contentTypes(`<Override PartName="/word/document.xml" ContentType="` + docxMain + `"/>`)},
		part{"_rels/.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rId1" Type="` + relType + `officeDocument" Target="word/document.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/></Relationships>`},
		part{"word/document.xml", xmlDecl + `<w:document ` + wordNS + `><w:body/></w:document>`},
		part{"docProps/core.xml", emptyCoreProps},
	)
	out, _ := clean(t, ooxml, data)
	rep, err := ooxml.Inspect(context.Background(), core.BytesSource(out), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("a clean document reports %+v", rep.Findings)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("a clean document warns %q", rep.Warnings)
	}
}

// TestDamagedPropertiesAreStillReplaced: the part goes regardless, so its damage
// can only cost detail, and a broken properties part must not stop the rest.
func TestDamagedPropertiesAreStillReplaced(t *testing.T) {
	data := zipOf(t, replacePart(docxParts(docxMain), "docProps/core.xml", "<broken Test Author")...)
	out, rep := clean(t, ooxml, data)
	assertAbsent(t, unzip(t, out), "Test Author")
	if len(values(rep, "Core property")) == 0 {
		t.Errorf("damaged core properties not reported: %+v", rep.Findings)
	}
}

func TestDamagedPartsFailClosed(t *testing.T) {
	for name, body := range map[string]string{
		"unclosed":        `<w:document ` + wordNS + `><w:body>`,
		"undeclared":      `<w:document><w:p w:author="Test Author"/></w:document>`,
		"not xml":         `Test Author`,
		"entity subset":   `<!DOCTYPE w [<!ENTITY who "Test Author">]><w:document ` + wordNS + `><w:ins w:author="&who;"/></w:document>`,
		"utf-7":           `<?xml version="1.0" encoding="UTF-7"?>+ADw-w:document+AD4-`,
		"two roots":       `<a/><b/>`,
		"text after root": `<a/>Test Author`,
		"nul":             "<a>\x00</a>",
		"invalid utf-8":   "<a>\xff</a>",
	} {
		t.Run(name, func(t *testing.T) {
			data := zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", body)...)
			var out bytes.Buffer
			if _, err := ooxml.Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); err == nil {
				t.Errorf("Clean accepted a damaged part")
			}
			if _, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{}); err == nil {
				t.Errorf("Inspect accepted a damaged part")
			}
		})
	}
}

func TestZipEntriesAreCanonical(t *testing.T) {
	out, _ := clean(t, ooxml, docx(t))
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.ModifiedDate != msdosEpoch || f.ModifiedTime != 0 {
			t.Errorf("%s has time %#x %#x", f.Name, f.ModifiedDate, f.ModifiedTime)
		}
		if len(f.Extra) != 0 {
			t.Errorf("%s has extra field % x", f.Name, f.Extra)
		}
		if f.Comment != "" {
			t.Errorf("%s has comment %q", f.Name, f.Comment)
		}
		if f.Flags&0x8 != 0 {
			t.Errorf("%s uses a data descriptor", f.Name)
		}
	}
	if zr.Comment != "" {
		t.Errorf("archive has comment %q", zr.Comment)
	}
	if !bytes.HasPrefix(out, []byte("PK\x03\x04")) {
		t.Errorf("output does not start with a local file header")
	}
	if !bytes.HasSuffix(out[:len(out)-18], []byte("PK\x05\x06")) {
		t.Errorf("output does not end with the end-of-central-directory record")
	}
}

func TestEntryOrderIsKept(t *testing.T) {
	parts := docxParts(docxMain)
	data := zipOf(t, parts...)
	out, _ := clean(t, ooxml, data)
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatal(err)
	}
	removed := func(name string) bool {
		return !slices.ContainsFunc(zr.File, func(f *zip.File) bool { return f.Name == name })
	}
	var want, got []string
	for _, p := range parts {
		if !removed(p.name) {
			want = append(want, p.name)
		}
	}
	for _, f := range zr.File {
		got = append(got, f.Name)
	}
	if !slices.Equal(want, got) {
		t.Errorf("order changed:\n got %q\nwant %q", got, want)
	}
}
