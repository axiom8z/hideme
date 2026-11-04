package office

import (
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
)

// FuzzClean mutates whole archives through both handlers. This exercises the ZIP
// layer. A mutation inside an entry nearly always fails its CRC, which is why the
// two targets below exist as well.
func FuzzClean(f *testing.F) {
	f.Add(docx(f))
	f.Add(xlsx(f))
	f.Add(pptx(f))
	f.Add(odt(f))
	f.Add([]byte{})
	f.Add([]byte("PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, ooxml, data)
		coretest.FuzzTarget(t, odf, data)
	})
}

// FuzzOOXMLParts feeds mutated XML to the rules by building a valid archive around
// it each time, getting past the CRC check. The same bytes are tried as the parts
// with the most logic attached: the main document, a .rels file and the content
// types, each in turn holding the mutated text.
func FuzzOOXMLParts(f *testing.F) {
	for _, parts := range [][]part{docxParts(docxMain), xlsxParts(), pptxParts()} {
		for _, p := range parts {
			if isXMLName(p.name) {
				f.Add(0, p.body)
			}
		}
	}
	f.Add(0, `<a><!-- c --><?p?><![CDATA[x]]></a>`)
	f.Add(1, xmlDecl+relPkgRoot+`<Relationship Id="rId9" Type="`+relType+`printerSettings" Target="../../x.bin" TargetMode="Internal"/></Relationships>`)
	f.Add(2, contentTypes(`<Override PartName="/word/document.xml" ContentType="application/xml"/>`))
	targets := []string{"word/document.xml", "word/_rels/document.xml.rels", "[Content_Types].xml", "_rels/.rels", "word/settings.xml", "docProps/core.xml", "docProps/custom.xml", "word/drawing.vml"}
	f.Fuzz(func(t *testing.T, target int, body string) {
		name := targets[uint(target)%uint(len(targets))]
		coretest.FuzzTarget(t, ooxml, zipOf(t, replacePart(docxParts(docxMain), name, body)...))
	})
}

// FuzzODFParts is the same idea for OpenDocument.
func FuzzODFParts(f *testing.F) {
	for _, p := range odfParts(odtMime) {
		if isXMLName(p.name) {
			f.Add(0, p.body)
		}
	}
	targets := []string{"content.xml", "META-INF/manifest.xml", "meta.xml", "settings.xml", "styles.xml", "manifest.rdf", "Object 1/content.xml"}
	f.Fuzz(func(t *testing.T, target int, body string) {
		name := targets[uint(target)%uint(len(targets))]
		coretest.FuzzTarget(t, odf, zipOf(t, replacePart(odfParts(odtMime), name, body)...))
	})
}
