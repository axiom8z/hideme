package office

import (
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	foreignSecret = "Tst Foreign Data"
	foreignField  = "Markup the format does not define"
)

// wordBody is a document body with something in it to check is still there.
const wordBody = `<w:p><w:r><w:t>Visible body text</w:t></w:r></w:p>`

func hasField(rep *core.Report, field string) bool {
	for _, f := range rep.Findings {
		if f.Field == field && f.Action == core.ActionRemove {
			return true
		}
	}
	return false
}

// An XML part keeps its tags as written, and libraries and many readers skip what they
// do not know, so a namespace nobody defined holds as much as anyone likes. Before these
// tests, an element, an attribute or a declaration in one went straight through.
func TestForeignMarkupIsRemoved(t *testing.T) {
	const foreignNS = `xmlns:zz="urn:zz"`
	for name, body := range map[string]string{
		"element":               `<zz:a ` + foreignNS + `>` + foreignSecret + `</zz:a>` + wordBody,
		"empty element":         `<zz:a ` + foreignNS + ` v="` + foreignSecret + `"/>` + wordBody,
		"element with children": `<zz:a ` + foreignNS + `><w:p><w:r><w:t>` + foreignSecret + `</w:t></w:r></w:p></zz:a>` + wordBody,
		"attribute":             `<w:p ` + foreignNS + ` zz:a="` + foreignSecret + `"><w:r><w:t>Visible body text</w:t></w:r></w:p>`,
		"namespace declaration": `<w:p xmlns:zz="urn:` + strings.ReplaceAll(foreignSecret, " ", "") + `"><w:r><w:t>Visible body text</w:t></w:r></w:p>`,
		"plain attribute":       `<w:p zzattr="` + foreignSecret + `"><w:r><w:t>Visible body text</w:t></w:r></w:p>`,
		"text beside a child":   `<w:p>` + foreignSecret + `<w:r><w:t>Visible body text</w:t></w:r></w:p>`,
		"text after a child":    `<w:p><w:r><w:t>Visible body text</w:t></w:r>` + foreignSecret + `</w:p>`,
		"CDATA beside a child":  `<w:p><![CDATA[` + foreignSecret + `]]><w:r><w:t>Visible body text</w:t></w:r></w:p>`,
		"text across a comment": `<w:p>` + foreignSecret + `<!-- split -->more<w:r><w:t>Visible body text</w:t></w:r></w:p>`,
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, ooxmlHandler{limits: defaultLimits}, wordDoc(t, body))
			parts := unzip(t, out)
			assertAbsent(t, parts, foreignSecret, "TstForeignData", "urn:zz")
			assertPresent(t, parts, "word/document.xml", "Visible body text")
			if !hasField(rep, foreignField) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			if _, rep := clean(t, ooxmlHandler{limits: defaultLimits}, out); hasField(rep, foreignField) {
				t.Fatal("the cleaned file still has some")
			}
		})
	}
}

// Extensions that the readers know must stay, as must what lies in the same tag.
func TestKnownExtensionsAreKept(t *testing.T) {
	const ext = `xmlns:w14="http://schemas.microsoft.com/office/word/2010/wordml" xmlns:mc="` + nsMarkup + `"`
	body := `<w:p ` + ext + ` mc:Ignorable="w14" w14:textId="77777777"><w:r><w:t>Visible body text</w:t></w:r></w:p>`
	out, rep := clean(t, ooxmlHandler{limits: defaultLimits}, wordDoc(t, body))
	assertPresent(t, unzip(t, out), "word/document.xml", "Visible body text", `mc:Ignorable="w14"`, `xmlns:w14=`)
	if hasField(rep, foreignField) {
		t.Fatalf("a false alarm: %+v", rep.Findings)
	}
}

// A reader that sees mc:Ignorable naming a prefix with no declaration refuses the whole
// document, and so does a Choice requiring one. So they have to go with the namespace.
func TestMarkupCompatibilityStaysConsistent(t *testing.T) {
	const decls = `xmlns:mc="` + nsMarkup + `" xmlns:zz="urn:zz" xmlns:w14="http://schemas.microsoft.com/office/word/2010/wordml"`
	t.Run("Ignorable keeps the prefixes that stay", func(t *testing.T) {
		body := `<w:p ` + decls + ` mc:Ignorable="zz w14"><w:r><w:t>Visible body text</w:t></w:r></w:p>`
		out, _ := clean(t, ooxmlHandler{limits: defaultLimits}, wordDoc(t, body))
		parts := unzip(t, out)
		assertPresent(t, parts, "word/document.xml", `mc:Ignorable="w14"`)
		assertAbsent(t, parts, "urn:zz")
	})
	t.Run("an Ignorable left with nothing goes", func(t *testing.T) {
		body := `<w:p ` + decls + ` mc:Ignorable="zz"><w:r><w:t>Visible body text</w:t></w:r></w:p>`
		out, _ := clean(t, ooxmlHandler{limits: defaultLimits}, wordDoc(t, body))
		assertAbsent(t, unzip(t, out), "Ignorable", "urn:zz")
	})
	t.Run("a Choice that requires it goes, and the Fallback stays", func(t *testing.T) {
		body := `<mc:AlternateContent ` + decls + `><mc:Choice Requires="zz"><w:p><w:r><w:t>` + foreignSecret + `</w:t></w:r></w:p></mc:Choice>` +
			`<mc:Fallback>` + wordBody + `</mc:Fallback></mc:AlternateContent>`
		out, _ := clean(t, ooxmlHandler{limits: defaultLimits}, wordDoc(t, body))
		parts := unzip(t, out)
		assertAbsent(t, parts, foreignSecret, "urn:zz")
		assertPresent(t, parts, "word/document.xml", "Visible body text", "mc:Fallback")
	})
}

// Unknown attributes in a relationship or content type have no reader at all.
func TestPackagePartsKeepOnlyTheirAttributes(t *testing.T) {
	rels := xmlDecl + `<Relationships xmlns="` + nsPackageRels + `" zzattr="` + foreignSecret + `">` +
		`<Relationship Id="rId1" Type="` + nsRels + `/officeDocument" Target="word/document.xml" zzattr="` + foreignSecret + `"/>` +
		foreignSecret + `</Relationships>`
	parts := replacePart(docxParts(docxMain), "_rels/.rels", rels)
	out, rep := clean(t, ooxmlHandler{limits: defaultLimits}, zipOf(t, parts...))
	got := unzip(t, out)
	assertAbsent(t, got, foreignSecret)
	assertPresent(t, got, "_rels/.rels", `Target="word/document.xml"`)
	if !hasField(rep, foreignField) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	for _, p := range docxParts(docxMain) {
		if p.name != "[Content_Types].xml" {
			continue
		}
		withAttr := strings.Replace(p.body, `<Default `, `<Default zzattr="`+foreignSecret+`" `, 1)
		out, _ := clean(t, ooxmlHandler{limits: defaultLimits}, zipOf(t, replacePart(docxParts(docxMain), p.name, withAttr)...))
		assertAbsent(t, unzip(t, out), foreignSecret)
	}
}

// A vendor's own part has a root the handler does not know. It is kept, and warned about
// when it looks like metadata, so its contents are not this rule's business.
func TestVendorPartIsLeftAlone(t *testing.T) {
	vendor := xmlDecl + `<vendor xmlns="urn:vendor"><item>vendor data</item></vendor>`
	parts := append(docxParts(docxMain), part{"word/vendor.xml", vendor})
	out, _ := clean(t, ooxmlHandler{limits: defaultLimits}, zipOf(t, parts...))
	assertPresent(t, unzip(t, out), "word/vendor.xml", "vendor data")
}

func TestODFForeignMarkupIsRemoved(t *testing.T) {
	content := func(inner string) string {
		return xmlDecl + `<office:document-content ` + odfNS + `><office:body><office:text>` + inner + `</office:text></office:body></office:document-content>`
	}
	const visible = `<text:p>Visible body text</text:p>`
	for name, inner := range map[string]string{
		"element":               `<zz:a xmlns:zz="urn:zz">` + foreignSecret + `</zz:a>` + visible,
		"attribute":             `<text:p xmlns:zz="urn:zz" zz:a="` + foreignSecret + `">Visible body text</text:p>`,
		"namespace declaration": `<text:p xmlns:zz="urn:` + strings.ReplaceAll(foreignSecret, " ", "") + `">Visible body text</text:p>`,
		"plain attribute":       `<text:p zzattr="` + foreignSecret + `">Visible body text</text:p>`,
		"text in element-only":  foreignSecret + visible,
		"text after a child":    visible + foreignSecret,
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, odfHandler{limits: defaultLimits}, zipOf(t, replacePart(odfParts(odtMime), "content.xml", content(inner))...))
			parts := unzip(t, out)
			assertAbsent(t, parts, foreignSecret, "TstForeignData", "urn:zz")
			assertPresent(t, parts, "content.xml", "Visible body text")
			if !hasField(rep, foreignField) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
		})
	}
	t.Run("mixed text in paragraphs stays", func(t *testing.T) {
		inner := `<text:p>Visible <text:span>body</text:span> text</text:p>`
		out, rep := clean(t, odfHandler{limits: defaultLimits}, zipOf(t, replacePart(odfParts(odtMime), "content.xml", content(inner))...))
		assertPresent(t, unzip(t, out), "content.xml", "Visible ", " text")
		if hasField(rep, foreignField) {
			t.Fatalf("a false alarm: %+v", rep.Findings)
		}
	})
}

// A relationship to a part that does not exist leads nowhere, and readers skip it, so
// its type is free room.
func TestDanglingRelationshipIsRemoved(t *testing.T) {
	rels := xmlDecl + `<Relationships xmlns="` + nsPackageRels + `">` +
		`<Relationship Id="rId1" Type="` + nsRels + `/officeDocument" Target="word/document.xml"/>` +
		`<Relationship Id="rIdZ" Type="http://example.com/` + strings.ReplaceAll(foreignSecret, " ", "") + `" Target="nothing.xml"/>` +
		`</Relationships>`
	out, rep := clean(t, ooxmlHandler{limits: defaultLimits}, zipOf(t, replacePart(docxParts(docxMain), "_rels/.rels", rels)...))
	got := unzip(t, out)
	assertAbsent(t, got, "TstForeignData", "nothing.xml")
	assertPresent(t, got, "_rels/.rels", `Target="word/document.xml"`)
	if !hasField(rep, "Relationship to a missing part") {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
}

// OpenDocument lists its parts in its manifest. No reader looks for one that is not
// listed, so it can hold anything, whatever its name.
func TestPartMissingFromTheManifestWarns(t *testing.T) {
	const body = xmlDecl + `<a>` + foreignSecret + `</a>`
	_, rep := clean(t, odfHandler{limits: defaultLimits}, zipOf(t, append(odfParts(odtMime), part{"customXml/item9.xml", body})...))
	if !hasWarning(rep, "manifest does not list") {
		t.Fatalf("no warning: %q", rep.Warnings)
	}
	_, rep = clean(t, odfHandler{limits: defaultLimits}, zipOf(t, odfParts(odtMime)...))
	if hasWarning(rep, "manifest does not list") {
		t.Fatalf("a false alarm: %q", rep.Warnings)
	}
}
