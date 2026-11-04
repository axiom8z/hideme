package office

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
)

// wordDoc wraps body in a document and swaps it into the shared fixture.
func wordDoc(t *testing.T, body string) []byte {
	t.Helper()
	full := xmlDecl + `<w:document ` + wordNS + `><w:body>` + body + `</w:body></w:document>`
	return zipOf(t, replacePart(docxParts(docxMain), "word/document.xml", full)...)
}

func assertWarns(t *testing.T, rep *core.Report, want string) {
	t.Helper()
	if !hasWarning(rep, want) {
		t.Errorf("no warning mentioning %q: %q", want, rep.Warnings)
	}
}

func assertNoWarning(t *testing.T, rep *core.Report, unwanted string) {
	t.Helper()
	if hasWarning(rep, unwanted) {
		t.Errorf("an unjustified warning mentions %q: %q", unwanted, rep.Warnings)
	}
}

// A part that is neither XML nor in a media folder used to be copied in silence,
// even a whole document with its author, or a photo with its GPS position.
func TestUnrecognisedPartsAreCleanedOrWarned(t *testing.T) {
	nested := zipOf(t, docxParts(docxMain)...)
	parts := append(docxParts(docxMain),
		part{"word/blob.dat", fakeImageDirty},
		part{"word/opaque.dat", "opaque test data"},
		part{"word/empty.dat", ""},
		part{"word/afchunk1.docx", string(nested)},
	)
	parts = replacePart(parts, "word/_rels/document.xml.rels", xmlDecl+relPkgRoot+
		`<Relationship Id="rId1" Type="http://example.invalid/custom" Target="blob.dat"/>`+
		`<Relationship Id="rId2" Type="http://example.invalid/custom" Target="opaque.dat"/>`+
		`<Relationship Id="rId3" Type="http://example.invalid/custom" Target="empty.dat"/>`+
		`<Relationship Id="rId4" Type="`+relType+`aFChunk" Target="afchunk1.docx"/>`+
		`<Relationship Id="rIdImage" Type="`+relType+`image" Target="media/image1.jpeg"/></Relationships>`)
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	if bytes.Contains(got["word/blob.dat"], []byte("GPS:")) {
		t.Errorf("a recognisable file outside the media folder was not cleaned")
	}
	inner := unzip(t, got["word/afchunk1.docx"])
	assertAbsent(t, inner, "Test Author", "Test Commenter")
	assertWarns(t, rep, "this embedded file could not be cleaned")

	const kept = "this embedded file could not be cleaned"
	count := func(extra ...part) int {
		_, rep := clean(t, ooxml, zipOf(t, append(docxParts(docxMain), extra...)...))
		return strings.Count(strings.Join(rep.Warnings, "|"), kept)
	}
	base := count()
	if got := count(part{"word/opaque.dat", "opaque test data"}); got != base+1 {
		t.Errorf("an opaque part gave %d warnings, want %d", got, base+1)
	}
	if got := count(part{"word/empty.dat", ""}); got != base {
		t.Errorf("an empty part holds nothing to warn about: %d warnings, want %d", got, base)
	}
}

func TestODFUnrecognisedPartsAreCleanedOrWarned(t *testing.T) {
	parts := append(odfParts(odtMime),
		part{"images/photo.jpg", fakeImageDirty},
		part{"secret.bin", "opaque test data"},
		part{"Scripts/python/macro.py", "print(1)"},
	)
	out, rep := clean(t, odfHandler{limits: defaultLimits}, zipOf(t, parts...))
	got := unzip(t, out)
	if bytes.Contains(got["images/photo.jpg"], []byte("GPS:")) {
		t.Errorf("a picture outside Pictures/ was not cleaned")
	}
	assertWarns(t, rep, "this embedded file could not be cleaned")
	assertWarns(t, rep, "macros are kept")
}

func TestFormulaPathsWarn(t *testing.T) {
	paths := map[string]string{
		"windows": `'C:\Users\testuser\[Book.xlsx]Sheet1'!A1`,
		"forward": `'D:/work/[Book.xlsx]Sheet1'!A1`,
		"mac":     `'/Users/testuser/[Book.xlsx]Sheet1'!A1`,
		"unc":     `'\\server\testuser\[Book.xlsx]Sheet1'!A1`,
		"file":    `'file:///home/testuser/[Book.xlsx]Sheet1'!A1`,
		"encoded": `'C:&#92;Users&#92;testuser&#92;[Book.xlsx]Sheet1'!A1`,
	}
	places := map[string]string{
		"cell":      `<sheetData><row r="1"><c r="A1"><f>%s</f><v>1</v></c></row></sheetData>`,
		"condition": `<sheetData/><conditionalFormatting sqref="A1"><cfRule type="expression" priority="1"><formula>%s</formula></cfRule></conditionalFormatting>`,
		"list":      `<sheetData/><dataValidations count="1"><dataValidation type="list" sqref="A1"><formula1>%s</formula1></dataValidation></dataValidations>`,
	}
	for pathName, path := range paths {
		for placeName, place := range places {
			t.Run(pathName+"/"+placeName, func(t *testing.T) {
				sheet := xmlDecl + `<worksheet ` + sheetNS + `>` + strings.Replace(place, "%s", path, 1) + `</worksheet>`
				_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/worksheets/sheet1.xml", sheet)...))
				assertWarns(t, rep, "xl/worksheets/sheet1.xml: a formula holds a file path")
				if !hasValue(rep, "File path", "[Book.xlsx]") {
					t.Errorf("inspect could not show the path: %+v", rep.Findings)
				}
				for _, w := range rep.Warnings {
					if strings.Contains(w, "Book.xlsx") {
						t.Errorf("a warning prints a path, and warnings reach clean's logs: %q", w)
					}
				}
			})
		}
	}
	for _, harmless := range []string{`SUM(A1:B2)`, `'Sheet 1'!A1`, `[1]Sheet1!$A$1`, `HYPERLINK("https://example.invalid/page")`, `"a:b"&amp;"c"`, `A1/B1`, `HYPERLINK("https://example.invalid/home/x")`, `HYPERLINK("https://example.invalid/users/x")`} {
		sheet := xmlDecl + `<worksheet ` + sheetNS + `><sheetData><row r="1"><c r="A1"><f>` + harmless + `</f><v>1</v></c></row></sheetData></worksheet>`
		_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/worksheets/sheet1.xml", sheet)...))
		assertNoWarning(t, rep, "holds a file path")
	}
}

func TestDefinedNameAndChartPathsWarn(t *testing.T) {
	book := xmlDecl + `<workbook ` + sheetNS + `><definedNames><definedName name="x">'C:\Users\testuser\[B.xlsx]S'!$A$1</definedName></definedNames><sheets><sheet name="S" sheetId="1" r:id="rIdSheet"/></sheets></workbook>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/workbook.xml", book)...))
	assertWarns(t, rep, "xl/workbook.xml: a formula holds a file path")

	chart := xmlDecl + `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:numRef><c:f>'C:\Users\testuser\[B.xlsx]S'!$A$1</c:f></c:numRef></c:chartSpace>`
	_, rep = clean(t, ooxml, zipOf(t, append(xlsxParts(), part{"xl/charts/chart1.xml", chart})...))
	assertWarns(t, rep, "xl/charts/chart1.xml: a formula holds a file path")
}

// =CELL("filename") stores the workbook's own full path as the cell's cached value.
func TestCachedCellPathWarns(t *testing.T) {
	sheet := xmlDecl + `<worksheet ` + sheetNS + `><sheetData><row r="1"><c r="A1" t="str"><f>CELL("filename")</f><v>C:\Users\testuser\[Book.xlsx]Sheet1</v></c></row></sheetData></worksheet>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/worksheets/sheet1.xml", sheet)...))
	assertWarns(t, rep, "a cached cell value holds a file path")

	plain := xmlDecl + `<worksheet ` + sheetNS + `><sheetData><row r="1"><c r="A1"><f>SUM(B1:B2)</f><v>3</v></c><c r="C1"><v>4</v></c></row></sheetData></worksheet>`
	_, rep = clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/worksheets/sheet1.xml", plain)...))
	assertNoWarning(t, rep, "holds a file path")
}

func complexField(code, result string) string {
	return `<w:p><w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText xml:space="preserve">` + code + `</w:instrText></w:r>` +
		`<w:r><w:fldChar w:fldCharType="separate"/></w:r><w:r><w:t>` + result + `</w:t></w:r><w:r><w:fldChar w:fldCharType="end"/></w:r></w:p>`
}

func TestFieldsThatRepeatPropertiesWarn(t *testing.T) {
	for name, body := range map[string]string{
		"author":      complexField(` AUTHOR `, "Test Author"),
		"switches":    complexField(` USERNAME \* MERGEFORMAT `, "Test Author"),
		"lower case":  complexField(` lastsavedby `, "Test Author"),
		"file name":   complexField(` FILENAME \p `, "report.docx"),
		"docproperty": complexField(` DOCPROPERTY "Owner" `, "Test Author"),
		"info":        complexField(` INFO Author `, "Test Author"),
		"simple":      `<w:p><w:fldSimple w:instr=" AUTHOR "><w:r><w:t>Test Author</w:t></w:r></w:fldSimple></w:p>`,
		"split runs": `<w:p><w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText> AUT</w:instrText></w:r><w:r><w:instrText>HOR </w:instrText></w:r>` +
			`<w:r><w:fldChar w:fldCharType="separate"/></w:r><w:r><w:t>Test Author</w:t></w:r><w:r><w:fldChar w:fldCharType="end"/></w:r></w:p>`,
		"nested": `<w:p><w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText> IF </w:instrText></w:r>` + complexField(` AUTHOR `, "Test Author") + `<w:r><w:fldChar w:fldCharType="end"/></w:r></w:p>`,
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, ooxml, wordDoc(t, body))
			assertWarns(t, rep, "field")
			assertWarns(t, rep, "cached text")
			// The text is what a reader sees on the page, so it has to stay.
			assertPresent(t, unzip(t, out), "word/document.xml", "<w:t>")
		})
	}
	for _, harmless := range []string{complexField(` PAGE `, "1"), complexField(` TOC \o "1-3" `, "Contents"), complexField(` DATE `, "today")} {
		_, rep := clean(t, ooxml, wordDoc(t, harmless))
		assertNoWarning(t, rep, "cached text")
	}
}

// A localised or unknown field name still gives itself away if its result is a path.
func TestFieldResultHoldingAPathWarns(t *testing.T) {
	_, rep := clean(t, ooxml, wordDoc(t, complexField(` SOMEFIELD `, `C:\Users\testuser\report.docx`)))
	assertWarns(t, rep, "a field result holds a file path")
	_, rep = clean(t, ooxml, wordDoc(t, `<w:p><w:r><w:t>C:\Users\testuser typed in the body</w:t></w:r></w:p>`))
	assertNoWarning(t, rep, "a field result")
}

func TestFieldCodePathVariants(t *testing.T) {
	for name, body := range map[string]string{
		"split": `<w:p><w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText> INCLUDEPICTURE "C:</w:instrText></w:r>` +
			`<w:r><w:instrText>\\Users\\testuser\\a.png" </w:instrText></w:r><w:r><w:fldChar w:fldCharType="end"/></w:r></w:p>`,
		"unc":        `<w:p><w:r><w:instrText> INCLUDETEXT "\\\\server\\testuser\\a.docx" </w:instrText></w:r></w:p>`,
		"mac":        `<w:p><w:r><w:instrText> INCLUDEPICTURE "/Users/testuser/a.png" </w:instrText></w:r></w:p>`,
		"forward":    `<w:p><w:r><w:instrText> LINK Excel.Sheet "D:/work/a.xlsx" </w:instrText></w:r></w:p>`,
		"simple":     `<w:fldSimple w:instr=" INCLUDETEXT &quot;\\\\server\\a.docx&quot; "/>`,
		"encoded":    `<w:p><w:r><w:instrText> INCLUDEPICTURE "C&#58;&#92;&#92;Users" </w:instrText></w:r></w:p>`,
		"at the end": `<w:p><w:r><w:instrText> LINK "C:\\x" </w:instrText></w:r></w:p>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, ooxml, wordDoc(t, body))
			assertWarns(t, rep, "a field code holds a file path")
		})
	}
}

// Binary field data is base64, so nothing inside it can be read or checked here.
func TestOpaqueFieldDataWarns(t *testing.T) {
	_, rep := clean(t, ooxml, wordDoc(t, `<w:p><w:r><w:fldChar w:fldCharType="begin"><w:fldData xml:space="preserve">QUJDRA==</w:fldData></w:fldChar></w:r></w:p>`))
	assertWarns(t, rep, "binary data")
}

func TestBoundContentControlWarns(t *testing.T) {
	out, rep := clean(t, ooxml, wordDoc(t, `<w:sdt><w:sdtPr><w:dataBinding w:xpath="/ns1:coreProperties[1]/ns0:creator[1]" w:storeItemID="{6C3C8BC8-F283-45AE-878A-BAB7291924A1}"/></w:sdtPr><w:sdtContent><w:p><w:r><w:t>Test Author</w:t></w:r></w:p></w:sdtContent></w:sdt>`))
	assertWarns(t, rep, "content control")
	assertPresent(t, unzip(t, out), "word/document.xml", "Test Author")
}

func TestTrackedChangeWithoutAuthorWarns(t *testing.T) {
	_, rep := clean(t, ooxml, wordDoc(t, `<w:p><w:del w:id="2"><w:r><w:delText>Deleted text</w:delText></w:r></w:del></w:p>`))
	assertWarns(t, rep, "tracked changes remain (1 found)")
}

const (
	testParaDoc = `<w:p xmlns:w14="` + nsWord14 + `" w14:paraId="1A2B3C4D" w14:textId="77777777" w:rsidR="00AA0001"><w:r><w:t>Body</w:t></w:r></w:p>` +
		`<w:tbl><w:tr xmlns:w14="` + nsWord14 + `" w14:paraId="1A2B3C4E" w14:textId="5E5E5E5E"/></w:tbl>`
	testCommentsDoc = xmlDecl + `<w:comments ` + wordNS + ` xmlns:w14="` + nsWord14 + `">` +
		`<w:comment w:id="0" w:author="Test A"><w:p w14:paraId="0BADF00D" w14:textId="11111111"><w:r><w:t>first paragraph</w:t></w:r></w:p><w:p w14:paraId="11AA22BB"><w:r><w:t>Parent</w:t></w:r></w:p></w:comment>` +
		`<w:comment w:id="1" w:author="Test B"><w:p w14:paraId="33cc44dd"><w:r><w:t>Reply</w:t></w:r></w:p></w:comment>` +
		`</w:comments>`
	testCommentsExt = xmlDecl + `<w15:commentsEx xmlns:w15="` + nsWord15 + `">` +
		`<w15:commentEx w15:paraId="11AA22BB" w15:done="0"/>` +
		`<w15:commentEx w15:paraId="33CC44DD" w15:paraIdParent="11aa22bb" w15:done="1"/>` +
		`</w15:commentsEx>`
)

var paraIDAttr = regexp.MustCompile(`paraId="([^"]*)"`)

func threadedDoc(t *testing.T) []byte {
	t.Helper()
	parts := replacePart(docxParts(docxMain), "word/document.xml", xmlDecl+`<w:document `+wordNS+`><w:body>`+testParaDoc+`</w:body></w:document>`)
	parts = replacePart(parts, "word/comments.xml", testCommentsDoc)
	parts = replacePart(parts, "word/commentsExtended.xml", testCommentsExt)
	return zipOf(t, parts...)
}

// Paragraph IDs are random but survive every edit, so they tie versions of a document
// together. Only a comment's ID does anything: commentsExtended uses it to attach a
// reply to its parent, so those are renumbered in step instead of removed.
func TestParagraphIDsGoAndThreadsSurvive(t *testing.T) {
	out, rep := clean(t, ooxml, threadedDoc(t))
	got := unzip(t, out)

	body := got["word/document.xml"]
	for _, secret := range []string{"paraId", "textId", "1A2B3C4D", "5E5E5E5E"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("document.xml keeps %q:\n%s", secret, body)
		}
	}
	comments, ext := got["word/comments.xml"], got["word/commentsExtended.xml"]
	for _, secret := range []string{"11AA22BB", "33CC44DD", "33cc44dd", "0BADF00D", "textId"} {
		if bytes.Contains(comments, []byte(secret)) {
			t.Errorf("comments.xml keeps %q:\n%s", secret, comments)
		}
	}
	for _, secret := range []string{"11AA22BB", "33CC44DD", "11aa22bb"} {
		if bytes.Contains(ext, []byte(secret)) {
			t.Errorf("commentsExtended.xml keeps %q:\n%s", secret, ext)
		}
	}
	// The thread: the reply's parent is the first comment's last paragraph.
	inComments := paraIDAttr.FindAllSubmatch(comments, -1)
	if len(inComments) != 2 {
		t.Fatalf("want an ID on the two referenced paragraphs, got:\n%s", comments)
	}
	parent, reply := string(inComments[0][1]), string(inComments[1][1])
	if parent == reply {
		t.Errorf("renumbering merged two IDs: %s", comments)
	}
	if !bytes.Contains(ext, []byte(`w15:paraId="`+reply+`" w15:paraIdParent="`+parent+`"`)) {
		t.Errorf("the reply no longer points at its parent:\ncomments: %s\next: %s", comments, ext)
	}
	if !bytes.Contains(ext, []byte(`w15:paraId="`+parent+`" w15:done="0"`)) {
		t.Errorf("the parent lost its entry: %s", ext)
	}
	// Every new ID must be valid: non-zero, eight hex digits, below 0x80000000.
	for _, id := range []string{parent, reply} {
		if !regexp.MustCompile(`^0[0-9A-F]{7}$`).MatchString(id) || id == "00000000" {
			t.Errorf("%q is not a valid paragraph ID", id)
		}
	}
	if len(values(rep, "Paragraph IDs")) == 0 {
		t.Errorf("removal not reported: %+v", rep.Findings)
	}

	// Idempotence, and nothing left to report the second time.
	again, rep2 := clean(t, ooxml, out)
	if !bytes.Equal(out, again) {
		t.Errorf("cleaning the output changed it")
	}
	if len(rep2.Residual()) != 0 {
		t.Errorf("findings on a cleaned file: %+v", rep2.Residual())
	}
	assertConsistent(t, got)
}

func TestOtherWordIDsAreRemoved(t *testing.T) {
	const wp14 = "http://schemas.microsoft.com/office/word/2010/wordprocessingDrawing"
	const w16cid = "http://schemas.microsoft.com/office/word/2016/wordml/cid"
	out, _ := clean(t, ooxml, wordDoc(t, `<w:p><w:r><w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:wp14="`+wp14+`" wp14:anchorId="1A2B3C4D" wp14:editId="5E6F7A8B" distT="0"/></w:drawing></w:r></w:p>`+
		`<w:num xmlns:w16cid="`+w16cid+`" w:numId="1" w16cid:durableId="12345678"/>`))
	body := unzip(t, out)["word/document.xml"]
	for _, secret := range []string{"1A2B3C4D", "5E6F7A8B", "12345678"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("document.xml keeps %q:\n%s", secret, body)
		}
	}
	if !bytes.Contains(body, []byte(`distT="0"`)) || !bytes.Contains(body, []byte(`w:numId="1"`)) {
		t.Errorf("real attributes were lost:\n%s", body)
	}
}

const (
	nsA16 = "http://schemas.microsoft.com/office/drawing/2014/main"
	nsP14 = "http://schemas.microsoft.com/office/powerpoint/2010/main"
	nsC16 = "http://schemas.microsoft.com/office/drawing/2014/chart"

	testShapeID = `<a:ext uri="{FF2B5EF4-FFF2-40B4-BE49-F238E27FC236}"><a16:creationId xmlns:a16="` + nsA16 + `" id="{AAAAAAAA-0000-0000-0000-000000000001}"/></a:ext>`
	testKeptExt = `<a:ext uri="{C183D7F6-B498-43B3-948B-1728B52AA6E4}"><adec:decorative xmlns:adec="http://schemas.microsoft.com/office/drawing/2017/decorative" val="1"/></a:ext>`
)

func creationIDSlide() string {
	return xmlDecl + `<p:sld ` + slideNS + `><p:cSld><p:spTree>` +
		`<p:sp><p:nvSpPr><p:cNvPr id="2" name="only"><a:extLst>` + testShapeID + `</a:extLst></p:cNvPr></p:nvSpPr></p:sp>` +
		`<p:sp><p:nvSpPr><p:cNvPr id="3" name="both"><a:extLst>` + testKeptExt + testShapeID + `</a:extLst></p:cNvPr></p:nvSpPr></p:sp>` +
		`<p:sp><p:nvSpPr><p:cNvPr id="4" name="pred"><a:extLst><a:ext uri="{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}"><a16:predDERef xmlns:a16="` + nsA16 + `" pred="{AAAAAAAA-0000-0000-0000-000000000001}"/></a:ext></a:extLst></p:cNvPr></p:nvSpPr></p:sp>` +
		`<p:graphicFrame><a:gridCol w="1"><a:extLst><a:ext uri="{9D8B030D-6E8A-4147-A177-3AD203B41FA5}"><a16:colId xmlns:a16="` + nsA16 + `" val="777001"/></a:ext></a:extLst></a:gridCol></p:graphicFrame>` +
		`</p:spTree><p:extLst><p:ext uri="{BB962C8B-B14F-4D97-AF65-F5344CB8AC3E}"><p14:creationId xmlns:p14="` + nsP14 + `" val="777002"/></p:ext></p:extLst></p:cSld></p:sld>`
}

// An extension wrapper may not be empty, since the schema requires its one child, so
// the wrapper goes with the ID, and a list left with no extensions goes too.
func TestCreationIDsGoWithTheirWrappers(t *testing.T) {
	parts := replacePart(pptxParts(), "ppt/slides/slide1.xml", creationIDSlide())
	parts = removePart(parts, "ppt/comments/modernComment1.xml")
	out, rep := clean(t, ooxml, zipOf(t, parts...))
	slide := unzip(t, out)["ppt/slides/slide1.xml"]
	for _, secret := range []string{"creationId", "AAAAAAAA", "predDERef", "colId", "777001", "777002", "FF2B5EF4", "BB962C8B"} {
		if bytes.Contains(slide, []byte(secret)) {
			t.Errorf("slide keeps %q:\n%s", secret, slide)
		}
	}
	if !bytes.Contains(slide, []byte(`name="only"/>`)) && !bytes.Contains(slide, []byte(`name="only"></p:cNvPr>`)) {
		t.Errorf("an emptied extension list stays:\n%s", slide)
	}
	if !bytes.Contains(slide, []byte(`<a:extLst>`+testKeptExt+`</a:extLst>`)) {
		t.Errorf("a real extension was lost:\n%s", slide)
	}
	if len(values(rep, "Drawing element IDs")) == 0 {
		t.Errorf("removal not reported: %+v", rep.Findings)
	}
	assertNoWarning(t, rep, "element IDs")
	again, rep2 := clean(t, ooxml, out)
	if !bytes.Equal(out, again) || len(rep2.Residual()) != 0 {
		t.Errorf("not idempotent, or findings remain: %+v", rep2.Residual())
	}
}

// Modern comments and slide zooms anchor to these IDs by value, so removing them
// would detach the anchors. Nobody can check that against PowerPoint here, so they
// stay, and the user hears about it.
func TestCreationIDsStayWhenSomethingPointsAtThem(t *testing.T) {
	for name, user := range map[string]part{
		"comment moniker": {"ppt/comments/modernComment1.xml", xmlDecl + `<p188:cmLst xmlns:p188="` + nsSlide188 + `"><p188:cm id="{T}" authorId="{T}" created="1980-01-01T00:00:00.000"><pc:sldMkLst xmlns:pc="http://schemas.microsoft.com/office/powerpoint/2013/main/command"><pc:docMk/><pc:sldMk cId="777002" sldId="256"/></pc:sldMkLst></p188:cm></p188:cmLst>`},
		"shape moniker":   {"ppt/comments/modernComment1.xml", xmlDecl + `<p188:cmLst xmlns:p188="` + nsSlide188 + `"><p188:cm id="{T}" authorId="{T}" created="1980-01-01T00:00:00.000"><ac:spMk xmlns:ac="http://schemas.microsoft.com/office/drawing/2013/main/command" id="2" creationId="{AAAAAAAA-0000-0000-0000-000000000001}"/></p188:cm></p188:cmLst>`},
		"slide zoom":      {"ppt/slides/slide2.xml", xmlDecl + `<p:sld ` + slideNS + `><pslz:sldZmObj xmlns:pslz="http://schemas.microsoft.com/office/powerpoint/2016/slidezoom" sldId="256" cId="777002"/></p:sld>`},
		"morph":           {"ppt/slides/slide2.xml", xmlDecl + `<p:sld ` + slideNS + `><p:transition><p159:morph xmlns:p159="http://schemas.microsoft.com/office/powerpoint/2015/09/main" option="byObject"/></p:transition></p:sld>`},
	} {
		t.Run(name, func(t *testing.T) {
			parts := replacePart(pptxParts(), "ppt/slides/slide1.xml", creationIDSlide())
			parts = replacePart(parts, user.name, user.body)
			out, rep := clean(t, ooxml, zipOf(t, parts...))
			slide := unzip(t, out)["ppt/slides/slide1.xml"]
			if !bytes.Contains(slide, []byte("AAAAAAAA")) || !bytes.Contains(slide, []byte("777002")) {
				t.Errorf("an ID something points at was removed:\n%s", slide)
			}
			assertWarns(t, rep, "element IDs")
			if len(rep.Residual()) == 0 {
				t.Errorf("the rest of the document should still have been reported")
			}
		})
	}
}

func TestChartAndWordDrawingIDsAreRemoved(t *testing.T) {
	chart := xmlDecl + `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:ser><c:extLst><c:ext uri="{C3380CC4-5D6E-409C-B139-0F0F7B9F44D0}"><c16:uniqueId xmlns:c16="` + nsC16 + `" val="{00000000-AAAA-BBBB-CCCC-000000000001}"/></c:ext></c:extLst></c:ser></c:chartSpace>`
	out, _ := clean(t, ooxml, zipOf(t, append(xlsxParts(), part{"xl/charts/chart1.xml", chart})...))
	if body := unzip(t, out)["xl/charts/chart1.xml"]; bytes.Contains(body, []byte("AAAA-BBBB")) || bytes.Contains(body, []byte("extLst")) {
		t.Errorf("chart keeps its ID:\n%s", body)
	}
	const pic = `<w:p><w:r><w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><wp:docPr id="1" name="Picture 1"><a:extLst>` + testShapeID + `</a:extLst></wp:docPr></wp:inline></w:drawing></w:r></w:p>`
	out, _ = clean(t, ooxml, wordDoc(t, pic))
	if body := unzip(t, out)["word/document.xml"]; bytes.Contains(body, []byte("AAAAAAAA")) || !bytes.Contains(body, []byte(`name="Picture 1"`)) {
		t.Errorf("word drawing wrong:\n%s", body)
	}
}

// The schema requires exactly one child in an extension, so taking only the child
// out, as this rule used to, leaves a part PowerPoint has every right to reject.
func TestPresenceInfoLeavesNoEmptyExtension(t *testing.T) {
	out, rep := clean(t, ooxml, pptx(t))
	authors := unzip(t, out)["ppt/commentAuthors.xml"]
	if bytes.Contains(authors, []byte("presenceInfo")) || bytes.Contains(authors, []byte("<p:ext ")) {
		t.Errorf("wrapper left behind:\n%s", authors)
	}
	if !hasValue(rep, "Comment author account", "test.person@") {
		t.Errorf("not reported: %+v", rep.Findings)
	}
}

func TestExternalLinksThatAreNotHyperlinksWarn(t *testing.T) {
	rels := xmlDecl + relPkgRoot + `<Relationship Id="rId1" Type="` + relType + `externalLinkPath" Target="https://contoso-my.sharepoint.com/personal/testuser/Documents/Budget.xlsx" TargetMode="External"/></Relationships>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/externalLinks/_rels/externalLink1.xml.rels", rels)...))
	assertWarns(t, rep, "a link points at a file outside the document")
	if !hasValue(rep, "External file link", "testuser") {
		t.Errorf("inspect could not show the link: %+v", rep.Findings)
	}
}

func TestHyperlinksWarnOnlyWhenTheyHoldAPath(t *testing.T) {
	for target, want := range map[string]bool{
		"https://example.invalid/home/index.html":               false,
		"https://example.invalid/users/list":                    false,
		"https://example.invalid/page":                          false,
		"mailto:test@example.invalid":                           false,
		"https://contoso-my.sharepoint.com/personal/testuser/a": true,
		"onenote:///C:/Users/testuser/notes.one":                true,
		`mhtml:file://C:\Users\testuser\a.mht`:                  true,
		"file:///C:/Users/testuser/a.docx":                      true,
		`\\server\share\a.docx`:                                 true,
	} {
		parts := replacePart(docxParts(docxMain), "word/_rels/document.xml.rels", xmlDecl+relPkgRoot+
			`<Relationship Id="rIdImage" Type="`+relType+`image" Target="media/image1.jpeg"/>`+
			`<Relationship Id="rIdLink" Type="`+relType+`hyperlink" Target="`+target+`" TargetMode="External"/></Relationships>`)
		_, rep := clean(t, ooxml, zipOf(t, parts...))
		if got := hasWarning(rep, "a link points"); got != want {
			t.Errorf("%s: warning = %v, want %v: %q", target, got, want, rep.Warnings)
		}
	}
}

func TestExternalWorkbookCacheWarns(t *testing.T) {
	link := xmlDecl + `<externalLink ` + sheetNS + `><externalBook r:id="rId1"><sheetDataSet><sheetData sheetId="0"><row r="1"><cell r="A1"><v>Test Cached Value</v></cell></row></sheetData></sheetDataSet></externalBook></externalLink>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/externalLinks/externalLink1.xml", link)...))
	assertWarns(t, rep, "cached copy of values")
}

func TestPathsInAttributesWarn(t *testing.T) {
	for name, body := range map[string]string{
		"tooltip":     `<w:p><w:hyperlink w:anchor="a" w:tooltip="C:\Users\testuser\x"/></w:p>`,
		"xml base":    `<w:p xml:base="file:///C:/Users/testuser/"/>`,
		"vml src":     `<w:p><v:imagedata xmlns:v="urn:schemas-microsoft-com:vml" src="file:///C:\Users\testuser\clip.png"/></w:p>`,
		"vml alt":     `<w:p><v:shape xmlns:v="urn:schemas-microsoft-com:vml" alt="C:\Users\testuser\a.png"/></w:p>`,
		"in alt text": `<w:p><wp:docPr xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" id="1" name="p" descr="Picture from C:\Users\testuser\a.png"/></w:p>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, ooxml, wordDoc(t, body))
			assertWarns(t, rep, "holds a file path")
			if !hasValue(rep, "File path", "testuser") {
				t.Errorf("inspect could not show the path: %+v", rep.Findings)
			}
		})
	}
	book := xmlDecl + `<workbook ` + sheetNS + `><sheets><sheet name="S" sheetId="1" r:id="rIdSheet"/></sheets><webPublishObjects count="1"><webPublishObject id="1" divId="d" destinationFile="C:\Users\testuser\x.htm"/></webPublishObjects></workbook>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/workbook.xml", book)...))
	assertWarns(t, rep, "holds a file path")
	dde := xmlDecl + `<externalLink ` + sheetNS + `><ddeLink ddeService="Excel" ddeTopic="C:\Users\testuser\x.xls"/></externalLink>`
	_, rep = clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/externalLinks/externalLink1.xml", dde)...))
	assertWarns(t, rep, "holds a file path")

	// These are everywhere in real files, and none of them is a path.
	_, rep = clean(t, ooxml, wordDoc(t, `<w:p><w:r w:val="profile:x"/><a:ext xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" uri="http://schemas.microsoft.com/office/word/2010/wordprocessingGroup"/><w:hyperlink w:tooltip="https://example.invalid/a"/></w:p>`))
	assertNoWarning(t, rep, "holds a file path")
}

func TestDataURIInAttributeWarns(t *testing.T) {
	_, rep := clean(t, ooxml, wordDoc(t, `<w:p><v:imagedata xmlns:v="urn:schemas-microsoft-com:vml" src="data:image/jpeg;base64,QUJD"/></w:p>`))
	assertWarns(t, rep, "data: URI")
}

// gfxdata is a base64 ZIP that repeats a DrawingML shape for round trips, with any
// name or path the shape has. The picture itself never needs it.
func TestVMLRoundTripDataAndSourcePathAreRemoved(t *testing.T) {
	out, rep := clean(t, ooxml, wordDoc(t, `<w:p><w:pict><v:shape xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office" o:gfxdata="UEsDBBQABgAI"><v:imagedata r:id="rIdImage" o:href="file:///C:\Users\testuser\clip.png"/></v:shape></w:pict></w:p>`))
	body := unzip(t, out)["word/document.xml"]
	for _, secret := range []string{"gfxdata", "UEsDBBQ", "testuser", "o:href"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("keeps %q:\n%s", secret, body)
		}
	}
	if !bytes.Contains(body, []byte(`r:id="rIdImage"`)) {
		t.Errorf("the picture itself was lost:\n%s", body)
	}
	if len(values(rep, "Drawing round-trip data")) == 0 || !hasValue(rep, "Picture file path", "testuser") {
		t.Errorf("not reported: %+v", rep.Findings)
	}
}

func TestSignatureLineWarns(t *testing.T) {
	_, rep := clean(t, ooxml, wordDoc(t, `<w:p><w:pict><v:shape xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office"><o:signatureline o:suggestedsigner="Test Signer" o:suggestedsigneremail="test@example.invalid"/></v:shape></w:pict></w:p>`))
	assertWarns(t, rep, "signature line")
}

func TestPermissionEditorIsRemoved(t *testing.T) {
	out, rep := clean(t, ooxml, wordDoc(t, `<w:p><w:permStart w:id="1" w:ed="CONTOSO\testuser" w:colFirst="1"/><w:r><w:t>x</w:t></w:r><w:permEnd w:id="1"/></w:p>`))
	body := unzip(t, out)["word/document.xml"]
	if bytes.Contains(body, []byte("testuser")) {
		t.Errorf("the editor stays:\n%s", body)
	}
	if !bytes.Contains(body, []byte(`w:id="1" w:colFirst="1"`)) {
		t.Errorf("the rest of the range was lost:\n%s", body)
	}
	if !hasValue(rep, "Permitted editor", "testuser") {
		t.Errorf("not reported: %+v", rep.Findings)
	}
}

// A permission's security descriptor holds Windows account SIDs.
func TestProtectedRangeSecurityDescriptorWarns(t *testing.T) {
	sheet := xmlDecl + `<worksheet ` + sheetNS + `><sheetData/><protectedRanges><protectedRange name="r" sqref="A1" securityDescriptor="O:WDG:WDD:(A;;CC;;;S-1-5-21-1-2-3-1001)"/></protectedRanges></worksheet>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), "xl/worksheets/sheet1.xml", sheet)...))
	assertWarns(t, rep, "security descriptor")
}

func TestReferenceFromCustomerDataIsDropped(t *testing.T) {
	slide := xmlDecl + `<p:sld ` + slideNS + `><p:cSld><p:spTree/></p:cSld><p:custDataLst><p:custData r:id="rIdData"/><p:tags r:id="rIdTags"/></p:custDataLst></p:sld>`
	parts := replacePart(pptxParts(), "ppt/slides/slide1.xml", slide)
	parts = append(parts,
		part{"ppt/slides/_rels/slide1.xml.rels", xmlDecl + relPkgRoot +
			`<Relationship Id="rIdData" Type="` + relType + `customXml" Target="../../customXml/item1.xml"/>` +
			`<Relationship Id="rIdTags" Type="` + relType + `tags" Target="../tags/tag1.xml"/></Relationships>`},
		part{"ppt/tags/tag1.xml", xmlDecl + `<p:tagLst ` + slideNS + `/>`},
		part{"customXml/item1.xml", xmlDecl + `<root><email>sharepoint.user@example.invalid</email></root>`})
	parts = dedupeLast(parts)
	out, _ := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	assertAbsent(t, got, "sharepoint.user@")
	body := got["ppt/slides/slide1.xml"]
	if bytes.Contains(body, []byte("custData ")) || !bytes.Contains(body, []byte(`<p:tags r:id="rIdTags"/>`)) {
		t.Errorf("wrong result:\n%s", body)
	}
	assertConsistent(t, got)
}

// Before this limit, half a megabyte could occupy a CPU for minutes: each element
// searched every namespace declaration in scope.
func TestManyNamespaceDeclarationsStayFast(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`<w:document xmlns:w="` + nsWord + `"`)
	for i := range 50000 {
		b.WriteString(` xmlns:p`)
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString(itoa(i))
		b.WriteString(`="u"`)
	}
	b.WriteString(`>`)
	for range 1000000 {
		b.WriteString(`<a/>`)
	}
	b.WriteString(`</w:document>`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := editXML(ctx, b.Bytes(), func(*xmlElement) error { return nil }); err != nil {
		t.Fatalf("editXML: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func TestNamespaceScopesStillRestore(t *testing.T) {
	doc := `<r xmlns:p="one"><p:a/><m xmlns:p="two"><p:b/><n xmlns:p="three"><p:c/></n><p:d/></m><p:e/></r>`
	want := map[string]string{"a": "one", "b": "two", "c": "three", "d": "two", "e": "one"}
	_, err := editXML(context.Background(), []byte(doc), func(e *xmlElement) error {
		if uri, ok := want[e.local]; ok && e.space != uri {
			t.Errorf("%s is in %q, want %q", e.local, e.space, uri)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManyWorkbookPathsAreRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString(xmlDecl + `<workbook ` + sheetNS + ` xmlns:x15ac="` + nsSheetAC + `"><sheets><sheet name="S" sheetId="1" r:id="rIdSheet"/></sheets>`)
	b.WriteString(strings.Repeat(`<a>`, 9000))
	b.WriteString(strings.Repeat(`<x15ac:absPath url="x"/>`, 300000))
	b.WriteString(strings.Repeat(`</a>`, 9000))
	b.WriteString(`</workbook>`)
	data := zipOf(t, replacePart(xlsxParts(), "xl/workbook.xml", b.String())...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := ooxml.Inspect(ctx, core.BytesSource(data), core.Options{})
	if !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("err = %v, want a safety limit", err)
	}
}

const (
	nsTestSVG = "urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0"
	odsMime   = "application/vnd.oasis.opendocument.spreadsheet"
)

func odfBody(t *testing.T, mime, body string) []byte {
	t.Helper()
	content := xmlDecl + `<office:document-content ` + odfNS + ` xmlns:table="` + nsTable + `" xmlns:svg="` + nsTestSVG + `" office:version="1.3"><office:body>` + body + `</office:body></office:document-content>`
	return zipOf(t, replacePart(odfParts(mime), "content.xml", content)...)
}

func TestODFPathsInFormulasWarn(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	for name, body := range map[string]string{
		"formula":  `<office:spreadsheet><table:table><table:table-row><table:table-cell table:formula="of:=['file:///home/testuser/b.ods'#$S.A1]"/></table:table-row></table:table></office:spreadsheet>`,
		"windows":  `<office:spreadsheet><table:table><table:table-row><table:table-cell table:formula="of:=['file:///C:/Users/testuser/b.ods'#$S.A1]"/></table:table-row></table:table></office:spreadsheet>`,
		"name":     `<office:spreadsheet><table:named-expressions><table:named-expression table:name="x" table:expression="['file:///home/testuser/b.ods'#$S.A1]"/></table:named-expressions></office:spreadsheet>`,
		"dde":      `<office:spreadsheet><table:dde-links><table:dde-link><office:dde-source office:dde-topic="file:///home/testuser/b.ods"/></table:dde-link></table:dde-links></office:spreadsheet>`,
		"dde text": `<office:text><text:dde-connection-decls><text:dde-connection-decl office:dde-topic="/home/testuser/b.odt"/></text:dde-connection-decls></office:text>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, odf, odfBody(t, odsMime, body))
			assertWarns(t, rep, "holds a file path")
			if !hasValue(rep, "File path", "testuser") {
				t.Errorf("inspect could not show the path: %+v", rep.Findings)
			}
		})
	}
	_, rep := clean(t, odf, odfBody(t, odsMime, `<office:spreadsheet><table:table><table:table-row><table:table-cell table:formula="of:=SUM([.A1:.B2])"/></table:table-row></table:table></office:spreadsheet>`))
	assertNoWarning(t, rep, "holds a file path")
}

func TestODFFieldsThatRepeatPropertiesWarn(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	for _, field := range []string{"author-name", "author-initials", "initial-creator", "creator", "file-name", "template-name", "sender-email", "sender-lastname", "printed-by", "user-defined", "title"} {
		t.Run(field, func(t *testing.T) {
			out, rep := clean(t, odf, odfBody(t, odtMime, `<office:text><text:p><text:`+field+`>Test Author</text:`+field+`></text:p></office:text>`))
			assertWarns(t, rep, "cached text")
			assertPresent(t, unzip(t, out), "content.xml", "Test Author")
		})
	}
	_, rep := clean(t, odf, odfBody(t, odtMime, `<office:text><text:p><text:page-number>1</text:page-number><text:span>Test</text:span></text:p></office:text>`))
	assertNoWarning(t, rep, "cached text")
}

func TestODFSheetTrackedChangesWarn(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	_, rep := clean(t, odf, odfBody(t, odsMime, `<office:spreadsheet><table:tracked-changes><table:deletion table:id="c1"><office:change-info><dc:creator>Author</dc:creator></office:change-info></table:deletion><table:insertion table:id="c2"/></table:tracked-changes></office:spreadsheet>`))
	assertWarns(t, rep, "tracked changes remain (2 found)")
}

func TestODFConfigItemWithAPathWarns(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	settings := xmlDecl + `<office:document-settings ` + odfNS + `><office:settings><config:config-item-set config:name="x">` +
		`<config:config-item config:name="SomePath" config:type="string">file:///home/testuser/x</config:config-item>` +
		`<config:config-item config:name="ZoomFactor" config:type="short">100</config:config-item></config:config-item-set></office:settings></office:document-settings>`
	_, rep := clean(t, odf, zipOf(t, replacePart(odfParts(odtMime), "settings.xml", settings)...))
	assertWarns(t, rep, "holds a file path")
	if !hasValue(rep, "File path", "testuser") {
		t.Errorf("inspect could not show the path: %+v", rep.Findings)
	}
}

func TestODFPicturePathInAltTextIsBlanked(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	out, rep := clean(t, odf, odfBody(t, odtMime, `<office:text><text:p><draw:frame><svg:title>C:\Users\testuser\a.png</svg:title><svg:desc>/home/testuser/a.png</svg:desc></draw:frame><draw:frame><svg:desc>A real description</svg:desc></draw:frame></text:p></office:text>`))
	body := unzip(t, out)["content.xml"]
	if bytes.Contains(body, []byte("testuser")) {
		t.Errorf("the path stays:\n%s", body)
	}
	if !bytes.Contains(body, []byte("A real description")) {
		t.Errorf("real alt text was lost:\n%s", body)
	}
	if !hasValue(rep, "Picture file path", "testuser") {
		t.Errorf("not reported: %+v", rep.Findings)
	}
}

func TestODFDataURIWarns(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	_, rep := clean(t, odf, odfBody(t, odtMime, `<office:text><text:p><draw:frame><draw:image xlink:href="data:image/jpeg;base64,QUJD"/></draw:frame></text:p></office:text>`))
	assertWarns(t, rep, "data: URI")
}

func TestODFVersionsOfAnEmbeddedObjectAreRemoved(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	parts := append(odfParts(odtMime),
		part{"Object 1/VersionList.xml", xmlDecl + `<VL:version-list xmlns:VL="http://openoffice.org/2001/versions-list"><VL:version-entry VL:creator="Test Nested Version"/></VL:version-list>`},
		part{"Object 1/Versions/Version1", "Test Nested Old"})
	out, _ := clean(t, odf, zipOf(t, parts...))
	got := unzip(t, out)
	assertGone(t, got, "Object 1/VersionList.xml", "Object 1/Versions/Version1")
}

// Once the DOCTYPE goes, an entity only the external DTD could define would make the
// output ill-formed, though the input was not.
func TestDoctypeEntityIsRefusedNotBroken(t *testing.T) {
	for _, doc := range []string{
		`<!DOCTYPE a SYSTEM "a.dtd"><a>&who;</a>`,
		`<!DOCTYPE a SYSTEM "a.dtd"><a b="&who;"/>`,
	} {
		_, err := editXML(context.Background(), []byte(doc), func(*xmlElement) error { return nil })
		if !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("%s: err = %v, want unsupported", doc, err)
		}
	}
	for _, doc := range []string{
		`<!DOCTYPE a SYSTEM "a.dtd"><a b="&amp;&#10;">&lt;&gt;&quot;&apos;&#xA;</a>`,
		`<a>&who;</a>`, // nothing changes about this one, so it stays the reader's problem
	} {
		if _, err := editXML(context.Background(), []byte(doc), func(*xmlElement) error { return nil }); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

func removePart(parts []part, name string) []part {
	var out []part
	for _, p := range parts {
		if p.name != name {
			out = append(out, p)
		}
	}
	return out
}

// dedupeLast keeps the last of parts sharing a name, for fixtures built with append.
func dedupeLast(parts []part) []part {
	seen := map[string]int{}
	var out []part
	for _, p := range parts {
		if i, ok := seen[p.name]; ok {
			out[i] = p
			continue
		}
		seen[p.name] = len(out)
		out = append(out, p)
	}
	return out
}

// A byte-order mark must not hide the declaration behind it. UTF-7 spells < as +ADw-,
// so a reader honouring the declaration would see tags that the scanner cannot.
func TestDeclaredEncodingBehindAByteOrderMark(t *testing.T) {
	doc := "\xEF\xBB\xBF<?xml version=\"1.0\" encoding=\"UTF-7\"?><a>+ADw-dc:creator+AD4-Test+ADw-/dc:creator+AD4-</a>"
	if _, err := editXML(context.Background(), []byte(doc), func(*xmlElement) error { return nil }); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("err = %v, want unsupported", err)
	}
	for _, encoding := range []string{"UTF-8", "utf-8", "ISO-8859-1"} {
		doc := "\xEF\xBB\xBF<?xml version=\"1.0\" encoding=\"" + encoding + "\"?><a/>"
		if _, err := editXML(context.Background(), []byte(doc), func(*xmlElement) error { return nil }); err != nil {
			t.Errorf("%s behind a BOM: %v", encoding, err)
		}
	}
}

const fileMarker = "ZQXMARKER"

// quotedInReason mirrors what cmd/hideme hides of a failure's reason on clean.
var quotedInReason = regexp.MustCompile(`"(?:[^"\\]|\\.)*(?:"|$)`)

// assertNothingFromFile checks everything clean prints: warnings, and every part of a
// finding but its value.
func assertNothingFromFile(t *testing.T, rep *core.Report) {
	t.Helper()
	for _, w := range rep.Warnings {
		if strings.Contains(w, fileMarker) {
			t.Errorf("a warning prints text from the file: %q", w)
		}
	}
	for _, f := range rep.Findings {
		if strings.Contains(f.Field+f.Where+f.Reason, fileMarker) {
			t.Errorf("a finding shows text from the file outside its value: %+v", f)
		}
	}
}

func TestNothingFromTheFileReachesWarningsOrLocations(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	m := fileMarker
	nested := zipOf(t, append(docxParts(docxMain), part{"word/" + m + "nested.dat", "opaque"})...)
	comments := xmlDecl + `<w:comments ` + wordNS + `><w:comment w:id="0" w:author="Test"><w:p><w:r><w:t>x</w:t></w:r></w:p></w:comment></w:comments>`
	ooxmlCases := map[string][]part{
		"custom property name":  replacePart(docxParts(docxMain), "docProps/custom.xml", xmlDecl+`<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/custom-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"><property fmtid="{D5CDD505-2E9C-101B-9397-08002B2CF9AE}" pid="2" name="`+m+`"><vt:lpwstr>x</vt:lpwstr></property></Properties>`),
		"unknown property name": replacePart(docxParts(docxMain), "docProps/core.xml", xmlDecl+`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"><cp:`+m+`>x</cp:`+m+`></cp:coreProperties>`),
		"opaque part":           append(docxParts(docxMain), part{"word/" + m + ".dat", "opaque"}),
		"media part":            append(docxParts(docxMain), part{"word/media/" + m + ".jpeg", fakeImageDirty}),
		"media it cannot read":  append(docxParts(docxMain), part{"word/media/" + m + ".emf", "opaque"}),
		"unreferenced XML":      append(docxParts(docxMain), part{m + "/a.xml", xmlDecl + `<a/>`}),
		"looks like metadata":   append(docxParts(docxMain), part{"word/" + m + "_history.xml", xmlDecl + `<a/>`}),
		"removed by folder":     append(docxParts(docxMain), part{"_xmlsignatures/" + m + ".xml", xmlDecl + `<a/>`}),
		"comments in a part":    append(docxParts(docxMain), part{"word/" + m + ".xml", comments}),
		"macros":                append(docxParts(docxMain), part{"word/" + m + "/vbaproject.bin", "x"}),
		"broken VML":            append(docxParts(docxMain), part{"word/" + m + ".vml", "<xml><br></xml>"}),
		"customXML with XMP":    append(docxParts(docxMain), part{"customXml/" + m + ".xml", xmlDecl + `<x:xmpmeta xmlns:x="adobe:ns:meta/"/>`}),
		"nested part":           append(docxParts(docxMain), part{"word/embeddings/oleObject2.docx", string(nested)}),
		"relationship ID": replacePart(docxParts(docxMain), "word/_rels/settings.xml.rels", xmlDecl+relPkgRoot+
			`<Relationship Id="`+m+`" Type="`+relType+`oleObject" Target="C:\Users\testuser\a.xlsx" TargetMode="External"/></Relationships>`),
		"attribute name": replacePart(docxParts(docxMain), "word/document.xml", xmlDecl+`<w:document `+wordNS+`><w:body><w:p a`+m+`="C:\Users\testuser\x"/><`+m+` b="data:image/png;base64,QUJD"/></w:body></w:document>`),
		"element name":   replacePart(docxParts(docxMain), "word/document.xml", xmlDecl+`<w:document `+wordNS+`><w:body><`+m+` a="C:\Users\testuser\x"/></w:body></w:document>`),
	}
	for name, parts := range ooxmlCases {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, ooxml, zipOf(t, parts...))
			assertNothingFromFile(t, rep)
		})
	}
	odfCases := map[string][]part{
		"user-defined name": replacePart(odfParts(odtMime), "meta.xml", xmlDecl+`<office:document-meta `+odfNS+`><office:meta><meta:user-defined meta:name="`+m+`">x</meta:user-defined></office:meta></office:document-meta>`),
		"unknown meta":      replacePart(odfParts(odtMime), "meta.xml", xmlDecl+`<office:document-meta `+odfNS+`><office:meta><meta:`+m+`>x</meta:`+m+`></office:meta></office:document-meta>`),
		"opaque part":       append(odfParts(odtMime), part{m + ".bin", "opaque"}),
		"macro":             append(odfParts(odtMime), part{"Scripts/python/" + m + ".py", "x"}),
		"XML part":          append(odfParts(odtMime), part{m + ".xml", xmlDecl + `<office:document-content ` + odfNS + `><office:body><text:p><office:annotation/></text:p></office:body></office:document-content>`}),
		"removed part":      append(odfParts(odtMime), part{"Thumbnails/" + m + ".png", "x"}),
		"sender field":      replacePart(odfParts(odtMime), "content.xml", xmlDecl+`<office:document-content `+odfNS+`><office:body><text:sender-`+m+`>x</text:sender-`+m+`></office:body></office:document-content>`),
		"attribute name":    replacePart(odfParts(odtMime), "content.xml", xmlDecl+`<office:document-content `+odfNS+`><office:body><text:p a`+m+`="file:///home/testuser/x"/></office:body></office:document-content>`),
		"config name":       replacePart(odfParts(odtMime), "settings.xml", xmlDecl+`<office:document-settings `+odfNS+`><office:settings><config:config-item config:name="`+m+`" config:type="string">file:///home/testuser/x</config:config-item></office:settings></office:document-settings>`),
	}
	for name, parts := range odfCases {
		t.Run("odf "+name, func(t *testing.T) {
			_, rep := clean(t, odf, zipOf(t, parts...))
			assertNothingFromFile(t, rep)
		})
	}
}

// A failure's reason may name what it could not read, but only in quotes, which the
// CLI hides on clean. Text outside quotes reaches the log whatever the file says.
func TestNothingFromTheFileIsUnquotedInErrors(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	m := fileMarker
	const broken = `<a><b></a>`
	for name, c := range map[string]struct {
		h     core.Handler
		parts []part
	}{
		"damaged part":     {ooxml, append(docxParts(docxMain), part{"word/" + m + ".xml", broken})},
		"damaged rels":     {ooxml, append(docxParts(docxMain), part{"word/_rels/" + m + ".xml.rels", broken})},
		"damaged thread":   {ooxml, append(docxParts(docxMain), part{m + "/commentsExtended.xml", broken})},
		"wide declaration": {ooxml, append(docxParts(docxMain), part{"word/a.xml", `<?xml version="1.0" encoding="utf-16` + m + `"?><a/>`})},
		"other encoding":   {ooxml, append(docxParts(docxMain), part{"word/a.xml", `<?xml version="1.0" encoding="` + m + `"?><a/>`})},
		"element name":     {ooxml, replacePart(docxParts(docxMain), "word/document.xml", `<`+m+`:a/>`)},
		"reference":        {ooxml, replacePart(docxParts(docxMain), "word/document.xml", xmlDecl+`<w:document `+wordNS+`><w:`+m+` r:id="rIdPrinter"/></w:document>`)},
		"odf damaged part": {odf, append(odfParts(odtMime), part{m + ".xml", broken})},
		"odf type":         {odf, replacePart(odfParts(odtMime), mimetypeName, odfMimePrefix+m)},
		"odf manifest": {odf, append(removePart(odfParts(odtMime), "META-INF/manifest.xml"),
			part{"META-INF/manifest.xml", `<?xml version="1.0" encoding="` + m + `"?><a/>`})},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := c.h.Clean(context.Background(), core.BytesSource(zipOf(t, c.parts...)), &out, core.Options{})
			if err == nil {
				t.Fatal("the fixture should fail")
			}
			if shown := quotedInReason.ReplaceAllString(err.Error(), `"..."`); strings.Contains(shown, m) {
				t.Errorf("clean would print text from the file: %s", shown)
			}
		})
	}
}

func TestUnusualPartNames(t *testing.T) {
	for name, want := range map[string]bool{
		"word/document.xml":                               true,
		"WORD/DOCUMENT.XML":                               true,
		"[Content_Types].xml":                             true,
		"_rels/.rels":                                     true,
		"word/_rels/document.xml.rels":                    true,
		"xl/worksheets/sheet12.xml":                       true,
		"word/embeddings/Microsoft_Excel_Worksheet1.xlsx": true,
		"ppt/comments/modernComment_100_A1B2C3D4.xml":     true,
		"Pictures/1000000000000320000002581A2B3C4D.jpg":   true,
		"package/services/metadata/core-properties/0123456789abcdef0123456789abcdef.psmdcp": true,
		"Object 1/content.xml":         true,
		"Object 1/":                    true,
		"ObjectReplacements/Object 12": true,
		"mimetype":                     true,
		"word/media/Jane Doe.jpg":      false,
		"word/media/image1.secret":     false,
		"word/janedoe/document.xml":    false,
		"janedoe.xml":                  false,
		"word/document%20x.xml":        false,
		"word/_rels/janedoe.xml.rels":  false,
		"Basic/MyLibrary/module1.xml":  false,
		"Janedoe/":                     false,
	} {
		if got := conventionalName(name); got != want {
			t.Errorf("conventionalName(%q) = %v, want %v", name, got, want)
		}
	}
	parts := append(docxParts(docxMain), part{"word/media/Jane Doe.jpeg", fakeImageClean})
	_, rep := clean(t, ooxml, zipOf(t, parts...))
	assertWarns(t, rep, "part names that office software does not generate")
	if !hasValue(rep, "Unusual part name", "Jane Doe") {
		t.Errorf("inspect could not show the name: %+v", rep.Findings)
	}
	// One that goes anyway is no leftover.
	_, rep = clean(t, ooxml, zipOf(t, append(docxParts(docxMain), part{"[trash]/Jane Doe.dat", "x"})...))
	assertNoWarning(t, rep, "part names that office software does not generate")
}

func TestThreadsInUTF16StayConsistent(t *testing.T) {
	wide := func(doc string) string {
		out := []byte{0xFF, 0xFE}
		for _, unit := range utf16.Encode([]rune(strings.Replace(doc, "UTF-8", "UTF-16", 1))) {
			out = append(out, byte(unit), byte(unit>>8))
		}
		return string(out)
	}
	parts := replacePart(docxParts(docxMain), "word/comments.xml", wide(testCommentsDoc))
	parts = replacePart(parts, "word/commentsExtended.xml", wide(testCommentsExt))
	out, _ := clean(t, ooxml, zipOf(t, parts...))
	got := unzip(t, out)
	comments, ext := got["word/comments.xml"], got["word/commentsExtended.xml"]
	ids := paraIDAttr.FindAllSubmatch(comments, -1)
	if len(ids) != 2 {
		t.Fatalf("want two IDs in the comments:\n%s", comments)
	}
	if !bytes.Contains(ext, []byte(`w15:paraId="`+string(ids[1][1])+`" w15:paraIdParent="`+string(ids[0][1])+`"`)) {
		t.Errorf("the thread broke:\n%s\n%s", comments, ext)
	}
}
