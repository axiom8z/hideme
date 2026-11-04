package samples

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/axiom8z/hideme/internal/coretest"
)

// PDFWithText returns a one-page PDF with an Info dictionary, whose page shows the
// given text in an uncompressed content stream.
func PDFWithText(pageText string) []byte {
	return buildPDF(pageText, "")
}

// PDFWithFormField returns a document with a filled-in form, a case where the handler
// keeps what it finds and warns about it.
func PDFWithFormField() []byte {
	return buildPDF("Form", "/AcroForm <</Fields [7 0 R]>>",
		"<</FT /Tx /Subtype /Widget /Rect [0 0 1 1] /T (test_field) /V (Test Field Value)>>")
}

// buildPDF writes objects 1 to 6, then extra as objects from 7 on.
func buildPDF(pageText, catalogExtra string, extra ...string) []byte {
	var b bytes.Buffer
	offsets := map[int]int{}
	object := func(num int, body string) {
		offsets[num] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", num, body)
	}
	b.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	object(1, "<</Type /Catalog /Pages 2 0 R "+catalogExtra+">>")
	object(2, "<</Type /Pages /Kids [3 0 R] /Count 1>>")
	object(3, "<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources <</Font <</F1 5 0 R>>>>>>")
	contents := "BT /F1 12 Tf 72 720 Td (" + pageText + ") Tj ET"
	object(4, fmt.Sprintf("<</Length %d>>\nstream\n%s\nendstream", len(contents), contents))
	object(5, "<</Type /Font /Subtype /Type1 /BaseFont /Helvetica>>")
	object(6, "<</Author ("+Author+") /Title ("+Title+") /Producer (Test Producer App) /CreationDate (D:20010203040506Z)>>")
	for i, body := range extra {
		object(7+i, body)
	}
	size := 7 + len(extra)
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", size)
	for num := 1; num < size; num++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[num])
	}
	fmt.Fprintf(&b, "trailer\n<</Size %d /Root 1 0 R /Info 6 0 R>>\nstartxref\n%d\n%%%%EOF\n", size, xref)
	return b.Bytes()
}

// PDF returns a one-page document with an Info dictionary.
func PDF() Sample {
	const page = "Visible Test Page Text"
	return Sample{
		Name:    "report.pdf",
		Handler: "pdf",
		Data:    PDFWithText(page),
		Secrets: text(Author, Title, "Test Producer App", "D:2001"),
		Content: []byte(page),
	}
}

// archive builds a ZIP with the real timestamps a real tool would leave. The names
// and bodies alternate.
func archive(parts ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < len(parts); i += 2 {
		method := zip.Deflate
		if parts[i] == "mimetype" {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     parts[i],
			Method:   method,
			Modified: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
		})
		if err != nil {
			panic(err)
		}
		if _, err := io.WriteString(w, parts[i+1]); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// DOCX returns a minimal document with core properties. Its secrets are checked in
// the archive's decompressed parts, so they are only useful to a reader that unzips.
func DOCX() Sample {
	const decl = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`
	const body = "Visible Test Body Text"
	return Sample{
		Name:    "letter.docx",
		Handler: "ooxml",
		Data: archive(
			"[Content_Types].xml", decl+`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`+
				`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`+
				`<Default Extension="xml" ContentType="application/xml"/>`+
				`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>`+
				`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`+
				`</Types>`,
			"_rels/.rels", decl+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
				`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>`+
				`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>`+
				`</Relationships>`,
			"word/document.xml", decl+`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>`+body+`</w:t></w:r></w:p></w:body></w:document>`,
			"docProps/core.xml", decl+`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/">`+
				`<dc:creator>`+Author+`</dc:creator><dc:title>`+Title+`</dc:title></cp:coreProperties>`,
		),
	}
}

// ODT returns a minimal OpenDocument text with a meta.xml.
func ODT() Sample {
	const decl = `<?xml version="1.0" encoding="UTF-8"?>`
	const office = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:meta="urn:oasis:names:tc:opendocument:xmlns:meta:1.0" xmlns:dc="http://purl.org/dc/elements/1.1/"`
	return Sample{
		Name:    "letter.odt",
		Handler: "odf",
		Data: archive(
			"mimetype", "application/vnd.oasis.opendocument.text",
			"content.xml", decl+`<office:document-content `+office+`><office:body><office:text><text:p>Visible Test Body Text</text:p></office:text></office:body></office:document-content>`,
			"meta.xml", decl+`<office:document-meta `+office+` office:version="1.3"><office:meta><dc:creator>`+Author+`</dc:creator><dc:title>`+Title+`</dc:title></office:meta></office:document-meta>`,
			"META-INF/manifest.xml", decl+`<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3">`+
				`<manifest:file-entry manifest:full-path="/" manifest:version="1.3" manifest:media-type="application/vnd.oasis.opendocument.text"/>`+
				`<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>`+
				`<manifest:file-entry manifest:full-path="meta.xml" manifest:media-type="text/xml"/>`+
				`</manifest:manifest>`,
		),
	}
}

// SVG returns a drawing with a metadata block, a comment and Inkscape attributes.
func SVG() Sample {
	data := `<?xml version="1.0" encoding="UTF-8"?>
<!-- Created by Test Editor -->
<svg xmlns="http://www.w3.org/2000/svg" xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape" width="10" height="10" inkscape:export-filename="C:\Users\Test Author\drawing.png">
  <metadata>` + string(coretest.FakeXMP()) + `</metadata>
  <rect x="1" y="1" width="8" height="8" fill="red"/>
</svg>
`
	return Sample{
		Name:    "drawing.svg",
		Handler: "svg",
		Data:    []byte(data),
		Secrets: text("Test Editor", Author, "inkscape"),
		Content: []byte(`<rect x="1" y="1" width="8" height="8" fill="red"/>`),
	}
}
