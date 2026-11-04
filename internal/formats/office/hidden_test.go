package office

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const (
	hiddenSecret = "ZQXHIDDENTEXT"

	warnNotes     = "speaker notes stay"
	warnSlides    = "hidden slides stay"
	warnSheets    = "hidden sheets or tables stay"
	warnVery      = "very hidden sheets stay"
	warnRows      = "hidden rows or columns stay"
	warnNames     = "hidden named ranges stay"
	warnText      = "hidden text stays"
	warnStyle     = "a style marks text as hidden"
	warnObjects   = "hidden drawing objects stay"
	warnPasswords = "a password hash"
)

var allHiddenWarnings = []string{warnNotes, warnSlides, warnSheets, warnVery, warnRows, warnNames, warnText, warnStyle, warnObjects, warnPasswords}

// assertHidden checks that exactly the wanted warning appears, once, and that no
// warning quotes the content it is about.
func assertHidden(t *testing.T, rep *core.Report, want string) {
	t.Helper()
	all := strings.Join(rep.Warnings, "|")
	if strings.Contains(all, hiddenSecret) {
		t.Errorf("a warning quotes the content: %q", rep.Warnings)
	}
	for _, kind := range allHiddenWarnings {
		got, wanted := strings.Count(all, kind), 0
		if kind == want {
			wanted = 1
		}
		if got != wanted {
			t.Errorf("warning %q appears %d times, want %d: %q", kind, got, wanted, rep.Warnings)
		}
	}
}

func TestHiddenWordContentWarns(t *testing.T) {
	run := func(props, text string) string {
		return `<w:p><w:r><w:rPr>` + props + `</w:rPr><w:t>` + text + `</w:t></w:r></w:p>`
	}
	for name, c := range map[string]struct{ body, want string }{
		"hidden run":      {run(`<w:vanish/>`, hiddenSecret), warnText},
		"switched on":     {run(`<w:vanish w:val="true"/>`, hiddenSecret), warnText},
		"switched off":    {run(`<w:vanish w:val="0"/>`, "visible"), ""},
		"also off":        {run(`<w:vanish w:val="false"/>`, "visible"), ""},
		"only a space":    {run(`<w:vanish/>`, " "), ""},
		"web hidden only": {run(`<w:webHidden/>`, "visible"), ""},
		"paragraph mark":  {`<w:p><w:pPr><w:rPr><w:vanish/><w:specVanish/></w:rPr></w:pPr><w:r><w:t>visible</w:t></w:r></w:p>`, ""},
		"old formatting":  {`<w:p><w:r><w:rPr><w:rPrChange w:id="1"><w:rPr><w:vanish/></w:rPr></w:rPrChange></w:rPr><w:t>visible</w:t></w:r></w:p>`, ""},
		"next run":        {`<w:p><w:r><w:rPr><w:vanish/></w:rPr></w:r><w:r><w:t>visible</w:t></w:r></w:p>`, ""},
		"hidden code":     {`<w:p><w:r><w:rPr><w:vanish/></w:rPr><w:instrText> XE "` + hiddenSecret + `" </w:instrText></w:r></w:p>`, warnText},
		"table row":       {`<w:tbl><w:tr><w:trPr><w:hidden/></w:trPr><w:tc><w:p><w:r><w:t>` + hiddenSecret + `</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`, warnRows},
		"row not hidden":  {`<w:tbl><w:tr><w:trPr><w:hidden w:val="0"/></w:trPr></w:tr></w:tbl>`, ""},
		"hidden picture":  {`<w:p><wp:docPr xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" id="1" name="p" hidden="1"/></w:p>`, warnObjects},
		"shown picture":   {`<w:p><wp:docPr xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" id="1" name="p" hidden="0"/></w:p>`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, ooxml, wordDoc(t, c.body))
			assertHidden(t, rep, c.want)
			if strings.Contains(c.body, hiddenSecret) {
				assertPresent(t, unzip(t, out), "word/document.xml", hiddenSecret)
			}
		})
	}
	style := xmlDecl + `<w:styles ` + wordNS + `><w:style w:type="character" w:styleId="H"><w:rPr><w:vanish/></w:rPr></w:style></w:styles>`
	_, rep := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/styles.xml", style)...))
	assertHidden(t, rep, warnStyle)
}

func TestWordPasswordHashesWarn(t *testing.T) {
	for setting, want := range map[string]string{
		`<w:documentProtection w:edit="forms" w:enforcement="1" w:hashValue="QUJD" w:saltValue="QUJD"/>`: warnPasswords,
		`<w:documentProtection w:edit="forms" w:enforcement="1" w:hash="QUJD" w:salt="QUJD"/>`:           warnPasswords,
		`<w:writeProtection w:recommended="1" w:hash="QUJD" w:salt="QUJD"/>`:                             warnPasswords,
		`<w:documentProtection w:edit="forms" w:enforcement="1"/>`:                                       "",
		`<w:writeProtection w:recommended="1"/>`:                                                         "",
		`<w:documentProtection w:edit="forms" w:enforcement="1" w:hash=""/>`:                             "",
	} {
		doc := xmlDecl + `<w:settings ` + wordNS + `>` + setting + `</w:settings>`
		out, rep := clean(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), "word/settings.xml", doc)...))
		assertHidden(t, rep, want)
		assertPresent(t, unzip(t, out), "word/settings.xml", setting)
	}
}

func TestHiddenSpreadsheetContentWarns(t *testing.T) {
	sheets := func(state string) string {
		return xmlDecl + `<workbook ` + sheetNS + `><sheets><sheet name="S" sheetId="1" ` + state + ` r:id="rIdSheet"/></sheets></workbook>`
	}
	book := func(inner string) string {
		return xmlDecl + `<workbook ` + sheetNS + `>` + inner + `<sheets><sheet name="S" sheetId="1" r:id="rIdSheet"/></sheets></workbook>`
	}
	sheet := func(inner string) string {
		return xmlDecl + `<worksheet ` + sheetNS + `>` + inner + `</worksheet>`
	}
	const wb, ws = "xl/workbook.xml", "xl/worksheets/sheet1.xml"
	for name, c := range map[string]struct{ part, body, want string }{
		"hidden sheet":      {wb, sheets(`state="hidden"`), warnSheets},
		"very hidden sheet": {wb, sheets(`state="veryHidden"`), warnVery},
		"visible sheet":     {wb, sheets(`state="visible"`), ""},
		"hidden row":        {ws, sheet(`<sheetData><row r="1" hidden="1"><c r="A1"><v>1</v></c></row></sheetData>`), warnRows},
		"true row":          {ws, sheet(`<sheetData><row r="1" hidden="true"/></sheetData>`), warnRows},
		"zero height row":   {ws, sheet(`<sheetData><row r="1" ht="0" customHeight="1"/></sheetData>`), warnRows},
		"shown row":         {ws, sheet(`<sheetData><row r="1" hidden="0" ht="15"/></sheetData>`), ""},
		"hidden column":     {ws, sheet(`<cols><col min="2" max="3" width="9" hidden="1"/></cols><sheetData/>`), warnRows},
		"zero width column": {ws, sheet(`<cols><col min="2" max="3" width="0" customWidth="1"/></cols><sheetData/>`), warnRows},
		"shown column":      {ws, sheet(`<cols><col min="2" max="3" width="9"/></cols><sheetData/>`), ""},
		"sheet hash":        {ws, sheet(`<sheetData/><sheetProtection algorithmName="SHA-512" hashValue="QUJD" saltValue="QUJD" spinCount="1" sheet="1"/>`), warnPasswords},
		"old sheet hash":    {ws, sheet(`<sheetData/><sheetProtection password="CC1A" sheet="1"/>`), warnPasswords},
		"no sheet password": {ws, sheet(`<sheetData/><sheetProtection sheet="1" objects="1"/>`), ""},
		"range hash":        {ws, sheet(`<sheetData/><protectedRanges><protectedRange name="r" sqref="A1" hashValue="QUJD"/></protectedRanges>`), warnPasswords},
		"book hash":         {wb, book(`<workbookProtection lockStructure="1" workbookHashValue="QUJD"/>`), warnPasswords},
		"old book hash":     {wb, book(`<workbookProtection lockStructure="1" workbookPassword="CC1A"/>`), warnPasswords},
		"revision hash":     {wb, book(`<workbookProtection revisionsHashValue="QUJD"/>`), warnPasswords},
		"no book password":  {wb, book(`<workbookProtection lockStructure="1"/>`), ""},
		"sharing hash":      {wb, book(`<fileSharing reservationPassword="CC1A"/>`), warnPasswords},
		"hidden name":       {wb, book(`<definedNames><definedName name="Secret" hidden="1">"` + hiddenSecret + `"</definedName></definedNames>`), warnNames},
		"built-in name":     {wb, book(`<definedNames><definedName name="_xlnm._FilterDatabase" hidden="1">S!$A$1</definedName></definedNames>`), ""},
		"shown name":        {wb, book(`<definedNames><definedName name="Open">S!$A$1</definedName></definedNames>`), ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, ooxml, zipOf(t, replacePart(xlsxParts(), c.part, c.body)...))
			assertHidden(t, rep, c.want)
		})
	}
}

func notesSlide(text string) string {
	return xmlDecl + `<p:notes ` + slideNS + `><p:cSld><p:spTree><p:sp><p:nvSpPr><p:nvPr><p:ph type="sldNum"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:fld id="{T}" type="slidenum"><a:t>1</a:t></a:fld></a:p></p:txBody></p:sp>` +
		`<p:sp><p:nvSpPr><p:nvPr><p:ph type="body"/></p:nvPr></p:nvSpPr><p:txBody><a:p>` + text + `</a:p></p:txBody></p:sp></p:spTree></p:cSld></p:notes>`
}

func TestHiddenPresentationContentWarns(t *testing.T) {
	note := `<a:r><a:t>` + hiddenSecret + `</a:t></a:r>`
	for name, c := range map[string]struct {
		parts []part
		want  string
	}{
		"notes": {append(pptxParts(), part{"ppt/notesSlides/notesSlide1.xml", notesSlide(note)}), warnNotes},
		"many notes, one warning": {append(pptxParts(),
			part{"ppt/notesSlides/notesSlide1.xml", notesSlide(note)},
			part{"ppt/notesSlides/notesSlide2.xml", notesSlide(note)}), warnNotes},
		"empty notes":  {append(pptxParts(), part{"ppt/notesSlides/notesSlide1.xml", notesSlide(`<a:endParaRPr/>`)}), ""},
		"hidden slide": {replacePart(pptxParts(), "ppt/slides/slide1.xml", xmlDecl+`<p:sld `+slideNS+` show="0"><p:cSld><p:spTree/></p:cSld></p:sld>`), warnSlides},
		"shown slide":  {replacePart(pptxParts(), "ppt/slides/slide1.xml", xmlDecl+`<p:sld `+slideNS+` show="1"><p:cSld><p:spTree/></p:cSld></p:sld>`), ""},
		"hidden shape": {replacePart(pptxParts(), "ppt/slides/slide1.xml", xmlDecl+`<p:sld `+slideNS+`><p:cSld><p:spTree><p:sp><p:nvSpPr><p:cNvPr id="2" name="s" hidden="1"/></p:nvSpPr></p:sp></p:spTree></p:cSld></p:sld>`), warnObjects},
		"modify hash":  {replacePart(pptxParts(), "ppt/presentation.xml", xmlDecl+`<p:presentation `+slideNS+`><p:sldIdLst><p:sldId id="256" r:id="rIdSlide"/></p:sldIdLst><p:modifyVerifier cryptAlgorithmSid="4" hashData="QUJD" saltData="QUJD" spinValue="1"/></p:presentation>`), warnPasswords},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, ooxml, zipOf(t, c.parts...))
			assertHidden(t, rep, c.want)
			if c.want == warnNotes {
				assertPresent(t, unzip(t, out), "ppt/notesSlides/notesSlide1.xml", hiddenSecret)
			}
			again, rep2 := clean(t, ooxml, out)
			if !bytes.Equal(out, again) {
				t.Errorf("cleaning the output changed it")
			}
			assertHidden(t, rep2, c.want)
		})
	}
}

const (
	testPresentationNS = `xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0"`
	testODPMime        = "application/vnd.oasis.opendocument.presentation"
)

func TestHiddenODFContentWarns(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	page := func(inner string) string {
		return `<office:presentation><draw:page ` + testPresentationNS + `>` + inner + `</draw:page></office:presentation>`
	}
	for name, c := range map[string]struct{ body, want string }{
		"hidden sheet by style": {`<office:automatic-styles><style:style style:name="ta"><style:table-properties table:display="false"/></style:style></office:automatic-styles>`, warnSheets},
		"hidden sheet":          {`<office:spreadsheet><table:table table:display="false"/></office:spreadsheet>`, warnSheets},
		"shown sheet":           {`<office:spreadsheet><table:table table:display="true"/></office:spreadsheet>`, ""},
		"collapsed row":         {`<office:spreadsheet><table:table><table:table-row table:visibility="collapse"/></table:table></office:spreadsheet>`, warnRows},
		"filtered row":          {`<office:spreadsheet><table:table><table:table-row table:visibility="filter"/></table:table></office:spreadsheet>`, warnRows},
		"collapsed column":      {`<office:spreadsheet><table:table><table:table-column table:visibility="collapse"/></table:table></office:spreadsheet>`, warnRows},
		"shown row":             {`<office:spreadsheet><table:table><table:table-row table:visibility="visible"/></table:table></office:spreadsheet>`, ""},
		"sheet key":             {`<office:spreadsheet><table:table table:protected="true" table:protection-key="QUJD"/></office:spreadsheet>`, warnPasswords},
		"document key":          {`<office:spreadsheet table:structure-protected="true" table:protection-key="QUJD"/>`, warnPasswords},
		"protected, no key":     {`<office:spreadsheet><table:table table:protected="true"/></office:spreadsheet>`, ""},
		"section key":           {`<office:text><text:section text:name="s" text:protected="true" text:protection-key="QUJD"/></office:text>`, warnPasswords},
		"tracked changes key":   {`<office:text><text:tracked-changes text:protection-key="QUJD"/></office:text>`, warnPasswords},
		"hidden text field":     {`<office:text><text:p><text:hidden-text text:condition="x" text:string-value="` + hiddenSecret + `"/></text:p></office:text>`, warnText},
		"hidden paragraph":      {`<office:text><text:p><text:hidden-paragraph text:condition="x" text:is-hidden="true"/>` + hiddenSecret + `</text:p></office:text>`, warnText},
		"hidden section":        {`<office:text><text:section text:name="s" text:display="none"><text:p>` + hiddenSecret + `</text:p></text:section></office:text>`, warnText},
		"conditional section":   {`<office:text><text:section text:name="s" text:display="condition" text:condition="x"/></office:text>`, warnText},
		"shown section":         {`<office:text><text:section text:name="s" text:display="true"/></office:text>`, ""},
		"hidden character":      {`<office:automatic-styles><style:style style:name="T"><style:text-properties text:display="none"/></style:style></office:automatic-styles>`, warnStyle},
		"notes":                 {page(`<presentation:notes><draw:frame><draw:text-box><text:p>` + hiddenSecret + `</text:p></draw:text-box></draw:frame></presentation:notes>`), warnNotes},
		"empty notes":           {page(`<presentation:notes><draw:frame><draw:text-box><text:p/></draw:text-box></draw:frame></presentation:notes>`), ""},
		"master notes":          {`<office:master-styles><style:master-page ` + testPresentationNS + `><presentation:notes><draw:frame><draw:text-box><text:p>number</text:p></draw:text-box></draw:frame></presentation:notes></style:master-page></office:master-styles>`, ""},
		"hidden slide":          {`<office:automatic-styles><style:style ` + testPresentationNS + `><style:drawing-page-properties presentation:visibility="hidden"/></style:style></office:automatic-styles>`, warnSlides},
		"shown slide":           {`<office:automatic-styles><style:style ` + testPresentationNS + `><style:drawing-page-properties presentation:visibility="visible"/></style:style></office:automatic-styles>`, ""},
		"hidden object":         {`<office:text><text:p><draw:frame draw:display="none"/></text:p></office:text>`, warnObjects},
		"shown object":          {`<office:text><text:p><draw:frame draw:display="always"/></text:p></office:text>`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, odf, odfBody(t, testODPMime, c.body))
			assertHidden(t, rep, c.want)
			if strings.Contains(c.body, hiddenSecret) {
				assertPresent(t, unzip(t, out), "content.xml", hiddenSecret)
			}
		})
	}
}

func TestODFPasswordSettingsWarn(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	for item, want := range map[string]string{
		`<config:config-item config:name="RedlineProtectionKey" config:type="base64Binary">QUJD</config:config-item>`:                                                                                                                                                warnPasswords,
		`<config:config-item config:name="RedlineProtectionKey" config:type="base64Binary"></config:config-item>`:                                                                                                                                                    "",
		`<config:config-item-map-indexed config:name="ModifyPasswordInfo"><config:config-item-map-entry><config:config-item config:name="hash" config:type="base64Binary">QUJD</config:config-item></config:config-item-map-entry></config:config-item-map-indexed>`: warnPasswords,
	} {
		settings := xmlDecl + `<office:document-settings ` + odfNS + `><office:settings><config:config-item-set config:name="x">` + item + `</config:config-item-set></office:settings></office:document-settings>`
		_, rep := clean(t, odf, zipOf(t, replacePart(odfParts(odtMime), "settings.xml", settings)...))
		assertHidden(t, rep, want)
	}
}

// An embedded object is its own part. The warning is still about one file.
func TestHiddenContentWarnsOncePerFile(t *testing.T) {
	odf := odfHandler{limits: defaultLimits}
	const hidden = `<office:spreadsheet><table:table table:display="false"/></office:spreadsheet>`
	nested := xmlDecl + `<office:document-content ` + odfNS + ` xmlns:table="` + nsTable + `"><office:body>` + hidden + `</office:body></office:document-content>`
	parts := replacePart(odfParts(odsMime), "Object 1/content.xml", nested)
	parts = replacePart(parts, "content.xml", nested)
	_, rep := clean(t, odf, zipOf(t, parts...))
	assertHidden(t, rep, warnSheets)
}

func hugeRepeat(n int) []part {
	return replacePart(docxParts(docxMain), "word/document.xml", xmlDecl+`<w:document `+wordNS+`><w:body><w:p><w:r><w:t>`+strings.Repeat("a", n)+`</w:t></w:r></w:p></w:body></w:document>`)
}

// A file of a few kilobytes that claims tens of megabytes is a bomb. Only perfectly
// repeating content compresses this far, and real documents do not reach it.
func TestImplausibleExpansionIsRefused(t *testing.T) {
	data := zipOf(t, hugeRepeat(40<<20)...)
	for _, run := range []func() error{
		func() error {
			_, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{})
			return err
		},
		func() error {
			_, err := ooxml.Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{})
			return err
		},
	} {
		if err := run(); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("%d bytes expanding to 40 MiB: got %v, want ErrTooLarge", len(data), err)
		}
	}
	odf := odfHandler{limits: defaultLimits}
	odt := zipOf(t, replacePart(odfParts(odtMime), "content.xml", xmlDecl+`<office:document-content `+odfNS+`><office:body><text:p>`+strings.Repeat("a", 40<<20)+`</text:p></office:body></office:document-content>`)...)
	if _, err := odf.Inspect(context.Background(), core.BytesSource(odt), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("OpenDocument: got %v, want ErrTooLarge", err)
	}
}

// The most repetitive real data measured, the same value in every cell of a numbered
// row, compresses about 18 times. It must pass well above the floor.
func TestRepetitiveRealSheetIsNotRefused(t *testing.T) {
	var sheet strings.Builder
	sheet.WriteString(xmlDecl + `<worksheet ` + sheetNS + `><sheetData>`)
	for r := 1; sheet.Len() < 48<<20; r++ {
		fmt.Fprintf(&sheet, `<row r="%d"><c r="A%d" s="1"><f t="shared" si="0"/><v>0</v></c><c r="B%d" s="1"><v>1</v></c></row>`, r, r, r)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	data := zipOf(t, replacePart(xlsxParts(), "xl/worksheets/sheet1.xml", sheet.String())...)
	if ratio := sheet.Len() / len(data); ratio > 64 {
		t.Logf("this sheet compressed %d times", ratio)
	}
	if _, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{}); err != nil {
		t.Errorf("a %d byte sheet expanding to %d: %v", len(data), sheet.Len(), err)
	}
}

func bytesAllocated(f func()) int {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return int(after.TotalAlloc - before.TotalAlloc)
}

// Inspect has no output to build and should need one copy of a part. A buffer left to
// double its way up allocates three, and each editing pass that keeps its output one
// more.
func TestInspectHoldsOneCopyOfAPart(t *testing.T) {
	coretest.SkipIfRace(t)
	const size = 16 << 20
	data := zipOf(t, hugeRepeat(size)...)
	got := bytesAllocated(func() {
		if _, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{}); err != nil {
			t.Error(err)
		}
	})
	if got > size*3/2 {
		t.Errorf("Inspect allocated %d MiB for a %d MiB part", got>>20, size>>20)
	}
}

func TestCleanHoldsTwoCopiesOfAPart(t *testing.T) {
	coretest.SkipIfRace(t)
	const size = 16 << 20
	data := zipOf(t, hugeRepeat(size)...)
	got := bytesAllocated(func() {
		var out bytes.Buffer
		out.Grow(1 << 20)
		if _, err := ooxml.Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); err != nil {
			t.Error(err)
		}
	})
	if got > size*5/2 {
		t.Errorf("Clean allocated %d MiB for a %d MiB part", got>>20, size>>20)
	}
}

func TestWideTextAllocatesOneNarrowCopy(t *testing.T) {
	const units = 8 << 20
	wide := append([]byte{0xFF, 0xFE}, bytes.Repeat([]byte{'a', 0}, units)...)
	wide = append(wide, '<', 0, 'a', 0, '/', 0, '>', 0)
	got := bytesAllocated(func() {
		if _, err := normalizeXML(wide); err != nil {
			t.Error(err)
		}
	})
	if got > units*3/2 {
		t.Errorf("decoding %d MiB of wide text allocated %d MiB", len(wide)>>20, got>>20)
	}
}

func TestWideTextDecodesLikeTheStandardLibrary(t *testing.T) {
	for _, units := range [][]uint16{
		{'<', 'a', '/', '>'},
		{'<', 'a', '>', 0xE9, 0x4E2D, '<', '/', 'a', '>'},
		{'<', 'a', '>', 0xD83D, 0xDE00, '<', '/', 'a', '>'},
		{'<', 'a', '>', 0xD83D, 'x', '<', '/', 'a', '>'},
		{'<', 'a', '>', 0xDE00, 'x', '<', '/', 'a', '>'},
		{'<', 'a', '>', 0xDE00, 0xD83D, '<', '/', 'a', '>'},
		{'<', 'a', '>', 0xD83D, 0xD83D, 0xDE00, '<', '/', 'a', '>'},
		{'<', 'a', '/', '>', 0xD83D},
	} {
		little := []byte{0xFF, 0xFE}
		big := []byte{0xFE, 0xFF}
		for _, u := range units {
			little = append(little, byte(u), byte(u>>8))
			big = append(big, byte(u>>8), byte(u))
		}
		want := string(utf16.Decode(units))
		for _, input := range [][]byte{little, big} {
			got, err := normalizeXML(input)
			if err != nil {
				t.Errorf("%x: %v", input, err)
				continue
			}
			if string(got) != want {
				t.Errorf("%x: got %q, want %q", input, got, want)
			}
		}
	}
}

// Refusing has to be cheap too. Reading past what is reserved would make the buffer
// double just before the limit stopped it, which is the most memory it could ever use.
func TestRefusalHoldsNoMoreThanTheLimit(t *testing.T) {
	coretest.SkipIfRace(t)
	data := zipOf(t, hugeRepeat(40<<20)...)
	got := bytesAllocated(func() {
		if _, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
	if limit := int(defaultLimits.minTotal) * 5 / 4; got > limit {
		t.Errorf("refusing allocated %d MiB, more than %d MiB", got>>20, limit>>20)
	}
	const mib = 1 << 20
	small := ooxmlHandler{limits: limits{maxEntry: 8 * mib, maxTotal: 64 * mib}}
	big := bomb(t, 40*mib)
	got = bytesAllocated(func() {
		if _, err := small.Inspect(context.Background(), core.BytesSource(big), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
	if got > 10*mib {
		t.Errorf("the entry limit allowed %d MiB", got>>20)
	}
}

func TestBudgetFollowsTheFileSize(t *testing.T) {
	const mib = 1 << 20
	for size, want := range map[int64]int64{
		0:         32 * mib,
		64 * 1024: 32 * mib,
		mib:       128 * mib,
		8 * mib:   1024 * mib,
		32 * mib:  4096 * mib,
		64 * mib:  4096 * mib,
		1 << 62:   4096 * mib,
	} {
		if got := defaultLimits.totalFor(size); got != want {
			t.Errorf("a file of %d bytes gets %d, want %d", size, got, want)
		}
	}
	// Limits without an expansion rule are just their total.
	if got := (limits{maxTotal: 5}).totalFor(1); got != 5 {
		t.Errorf("got %d", got)
	}
}
