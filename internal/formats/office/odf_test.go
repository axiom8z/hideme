package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

var odf = odfHandler{limits: defaultLimits}

var odfFields = []string{
	"Initial creator", "Creator", "Creation date", "Modified", "Editing cycles", "Editing duration",
	"Generator", "Print date", "Printed by", "Title", "Description", "Subject", "Keywords",
	"Document statistics", "User-defined field", "Template",
	"Thumbnail", "Printer name", "Printer setup", "Annotation author", "Annotation date", "Annotation initials",
	"Tracked-change author", "Tracked-change date", "Editing session IDs", "Digital signature", "Saved versions",
	"GPS coordinates", "ZIP entry times",
}

func TestODFContract(t *testing.T) {
	cases := []coretest.Case{{
		Name:           "odt",
		Data:           odt(t),
		WantFields:     odfFields,
		WantRetained:   []string{"External file link"},
		MustNotContain: rawNames("Thumbnails", "documentsignatures", "VersionList", "Versions/"),
		MustContain:    rawNames("mimetype"+odtMime, "content.xml", "Pictures/image1.jpg", "Object 1/meta.xml"),
	}}
	for mime, format := range odfFormats {
		if format == "odt" {
			continue
		}
		cases = append(cases, coretest.Case{
			Name:        format,
			Data:        zipOf(t, odfParts(mime)...),
			WantFields:  []string{"Creator"},
			MustContain: rawNames("mimetype" + mime),
		})
	}
	coretest.Contract(t, odf, cases)
}

func TestODFRobustness(t *testing.T) {
	coretest.Robustness(t, odf, odt(t))
}

func TestODFIsClean(t *testing.T) {
	out, rep := clean(t, odf, odt(t))
	parts := unzip(t, out)

	assertAbsent(t, parts, odfSecrets...)
	assertGone(t, parts, "Thumbnails/", "Thumbnails/thumbnail.png", "META-INF/documentsignatures.xml", "VersionList.xml", "Versions/Version1")
	assertPresent(t, parts, "content.xml", "Visible body text", "Annotation body text", `xlink:href="Pictures/image1.jpg"`, "<style:text-properties/>",
		"<dc:creator>"+placeholderAuthor+"</dc:creator><dc:date>"+placeholderODFDate+"</dc:date>")
	assertPresent(t, parts, "styles.xml", "Header text")
	assertPresent(t, parts, "settings.xml", `config:name="ZoomFactor" config:type="short">100<`, `config:name="PrinterName" config:type="string"></config:config-item>`)
	assertPresent(t, parts, "meta.xml", `office:version="1.3"`, "<office:meta/>")
	assertPresent(t, parts, "Pictures/image1.jpg", fakeImageClean)
	assertPresent(t, parts, "Pictures/image2.emf", "EMF test picture")
	assertPresent(t, parts, "Object 1/content.xml", "Nested text")
	assertPresent(t, parts, "Object 1/meta.xml", "<office:meta/>")

	manifest := string(parts["META-INF/manifest.xml"])
	for _, want := range []string{`manifest:full-path="/"`, `manifest:full-path="content.xml"`, `manifest:full-path="Pictures/image1.jpg"`, `manifest:full-path="Object 1/meta.xml"`} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest lost %s", want)
		}
	}
	for name := range parts {
		if strings.HasPrefix(name, "META-INF/manifest") || name == mimetypeName {
			continue
		}
		if !strings.HasSuffix(name, "/") && !strings.Contains(manifest, `manifest:full-path="`+name+`"`) {
			t.Errorf("part %s is not in the manifest", name)
		}
	}
	assertConsistentXML(t, parts)

	if !hasValue(rep, "User-defined field", "Test Secret Name = test.user@example.invalid") {
		t.Errorf("user-defined field: %q", values(rep, "User-defined field"))
	}
	if !hasValue(rep, "Printer name", "Test Printer") {
		t.Errorf("printer name: %q", values(rep, "Printer name"))
	}
	if !hasValue(rep, "Annotation author", "Test Commenter") {
		t.Errorf("annotation author: %q", values(rep, "Annotation author"))
	}
	if !hasValue(rep, "Tracked-change author", "Test Reviser") {
		t.Errorf("tracked-change author: %q", values(rep, "Tracked-change author"))
	}
	if !hasValue(rep, "External file link", "linked.png") {
		t.Errorf("file link: %q", values(rep, "External file link"))
	}
	for _, want := range []string{"image2.emf", "digital signature", "tracked changes", "annotation text remains", "local file path"} {
		if !hasWarning(rep, want) {
			t.Errorf("no warning %q: %q", want, rep.Warnings)
		}
	}
}

func assertConsistentXML(t *testing.T, parts map[string][]byte) {
	t.Helper()
	for name, body := range parts {
		if isXMLName(name) {
			textOf(t, body)
		}
	}
}

func TestODFTextSurvives(t *testing.T) {
	data := odt(t)
	out, _ := clean(t, odf, data)
	before, after := unzip(t, data), unzip(t, out)
	drop := []string{"Test Reviser", "2001-02-03T04:05:09", "Test Commenter", "2001-02-03T04:05:10", "TC", "RkFLRUlNRyBwaXhlbHMgR1BTOjQ4Ljg1ODQsMi4yOTQ1"}
	want := slices.DeleteFunc(textOf(t, before["content.xml"]), func(s string) bool { return slices.Contains(drop, s) })
	got := slices.DeleteFunc(textOf(t, after["content.xml"]), func(s string) bool {
		return s == base64.StdEncoding.EncodeToString([]byte(fakeImageClean)) || s == placeholderAuthor || s == placeholderODFDate
	})
	if !slices.Equal(want, got) {
		t.Errorf("text changed:\n got %q\nwant %q", got, want)
	}
}

func TestODFBinaryDataIsCleaned(t *testing.T) {
	out, rep := clean(t, odf, odt(t))
	want := base64.StdEncoding.EncodeToString([]byte(fakeImageClean))
	if !bytes.Contains(unzip(t, out)["content.xml"], []byte("<office:binary-data>"+want+"</office:binary-data>")) {
		t.Errorf("binary data not cleaned: %s", unzip(t, out)["content.xml"])
	}
	if !hasValue(rep, "GPS coordinates", "48.8584") {
		t.Errorf("no nested finding: %+v", rep.Findings)
	}
}

func TestODFBinaryDataWithLineBreaks(t *testing.T) {
	// Writers wrap base64, so whitespace has to be ignored on the way in.
	encoded := base64.StdEncoding.EncodeToString([]byte(fakeImageDirty))
	wrapped := encoded[:10] + "\n  " + encoded[10:]
	content := strings.Replace(odfContent, "RkFLRUlNRyBwaXhlbHMgR1BTOjQ4Ljg1ODQsMi4yOTQ1", wrapped, 1)
	out, _ := clean(t, odf, zipOf(t, replacePart(odfParts(odtMime), "content.xml", content)...))
	if bytes.Contains(unzip(t, out)["content.xml"], []byte(encoded[10:20])) {
		t.Errorf("the original base64 survives")
	}
}

func TestODFMimetypeComesFirstAndStored(t *testing.T) {
	// Move it last in the input. Readers look at fixed offsets, so the output must
	// put it back however the input was arranged.
	parts := odfParts(odtMime)
	parts = append(parts[1:], parts[0])
	out, _ := clean(t, odf, zipOf(t, parts...))
	if !bytes.HasPrefix(out[30:], []byte("mimetype"+odtMime)) {
		t.Errorf("mimetype is not at the fixed offset: %q", out[:80])
	}
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatal(err)
	}
	first := zr.File[0]
	if first.Name != mimetypeName || first.Method != zip.Store {
		t.Errorf("first entry is %q, method %d", first.Name, first.Method)
	}
	if len(first.Extra) != 0 || first.Flags&0x8 != 0 {
		t.Errorf("first entry has extra %x flags %#x", first.Extra, first.Flags)
	}
	for _, f := range zr.File[1:] {
		if f.Name == mimetypeName {
			t.Errorf("mimetype appears twice")
		}
		if f.ModifiedDate != msdosEpoch || f.ModifiedTime != 0 {
			t.Errorf("%s has a real time", f.Name)
		}
	}
}

func TestODFEncryptedIsRefused(t *testing.T) {
	manifest := xmlDecl + `<manifest:manifest xmlns:manifest="` + nsManifest + `"><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"><manifest:encryption-data manifest:checksum="x"/></manifest:file-entry></manifest:manifest>`
	data := zipOf(t, replacePart(odfParts(odtMime), "META-INF/manifest.xml", manifest)...)
	var out bytes.Buffer
	if _, err := odf.Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); !errors.Is(err, core.ErrEncrypted) {
		t.Errorf("Clean: got %v, want ErrEncrypted", err)
	}
	if _, err := odf.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrEncrypted) {
		t.Errorf("Inspect: got %v, want ErrEncrypted", err)
	}
}

func TestODFUnknownTypesAreRefused(t *testing.T) {
	for _, mime := range []string{"application/vnd.oasis.opendocument.base", odtMime + "\n", odtMime + "-new"} {
		data := zipOf(t, odfParts(mime)...)
		if !odf.Match(data, core.BytesSource(data)) {
			t.Errorf("%q: Match should claim it, so the refusal is specific", mime)
		}
		if _, err := odf.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("%q: got %v, want ErrUnsupported", mime, err)
		}
	}
}

// TestODFEmptyPartPassesThrough uses what every LibreOffice document of a certain
// age contains: a zero-byte XML file and a set of bare directory entries.
func TestODFEmptyPartPassesThrough(t *testing.T) {
	parts := append(odfParts(odtMime),
		part{"Configurations2/accelerator/current.xml", ""},
		part{"Configurations2/statusbar/", ""},
		part{"layout-cache", "\x00\x01binary"})
	out, _ := clean(t, odf, zipOf(t, parts...))
	got := unzip(t, out)
	if body, ok := got["Configurations2/accelerator/current.xml"]; !ok || len(body) != 0 {
		t.Errorf("empty part: present=%v, %q", ok, body)
	}
	if _, ok := got["Configurations2/statusbar/"]; !ok {
		t.Errorf("a directory entry listed in the manifest was dropped")
	}
	assertPresent(t, got, "layout-cache", "binary")
}

func TestODFEmbeddedObjectFiles(t *testing.T) {
	parts := append(odfParts(odtMime),
		part{"Object 1/Pictures/deep.jpg", fakeImageDirty},
		part{"Object 1/mimetype", "application/vnd.oasis.opendocument.chart"},
		part{"Object 2", "OLE test object"},
		part{"ObjectReplacements/Object 1", "SVM test picture"})
	out, rep := clean(t, odf, zipOf(t, parts...))
	got := unzip(t, out)
	assertPresent(t, got, "Object 1/Pictures/deep.jpg", fakeImageClean)
	if bytes.Contains(got["Object 1/Pictures/deep.jpg"], []byte("GPS:")) {
		t.Errorf("a picture inside an embedded object kept its GPS")
	}
	for _, want := range []string{"Object 2: this embedded file", "ObjectReplacements/Object 1: this embedded file"} {
		if !hasWarning(rep, want) {
			t.Errorf("no warning %q: %q", want, rep.Warnings)
		}
	}
	if hasWarning(rep, "Object 1/mimetype") {
		t.Errorf("an embedded object's mimetype is not worth a warning: %q", rep.Warnings)
	}
}

func TestODFOldManifestWithDoctype(t *testing.T) {
	manifest := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<!DOCTYPE manifest:manifest PUBLIC "-//OpenOffice.org//DTD Manifest 1.0//EN" "Manifest.dtd">` + "\n" +
		`<manifest:manifest xmlns:manifest="` + nsManifest + `"><manifest:file-entry manifest:media-type="text/xml" manifest:full-path="content.xml"/></manifest:manifest>`
	out, _ := clean(t, odf, zipOf(t, replacePart(odfParts(odtMime), "META-INF/manifest.xml", manifest)...))
	got := unzip(t, out)["META-INF/manifest.xml"]
	if bytes.Contains(got, []byte("DOCTYPE")) || !bytes.Contains(got, []byte("content.xml")) {
		t.Errorf("manifest = %s", got)
	}
}

func TestODFMatch(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		want bool
	}{
		"odt":  {odt(t), true},
		"docx": {docx(t), false},
		"epub": {zipOf(t, part{mimetypeName, "application/epub+zip"}), false},
		"zip":  {zipOf(t, part{"a.txt", "x"}), false},
		"none": {nil, false},
	} {
		if got := odf.Match(tc.data, core.BytesSource(tc.data)); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", name, got, tc.want)
		}
	}
}

func TestODFMacrosAreKeptWithAWarning(t *testing.T) {
	parts := append(odfParts(odtMime), part{"Basic/Standard/Module1.xml", xmlDecl + `<script>test macro</script>`})
	out, rep := clean(t, odf, zipOf(t, parts...))
	assertPresent(t, unzip(t, out), "Basic/Standard/Module1.xml", "test macro")
	if !hasWarning(rep, "macros") {
		t.Errorf("no macro warning: %q", rep.Warnings)
	}
}

func TestODFInlineMetaIsRemoved(t *testing.T) {
	content := xmlDecl + `<office:document ` + odfNS + `><office:meta><dc:creator>Test Author</dc:creator></office:meta><office:body><text:p>Text</text:p></office:body></office:document>`
	out, rep := clean(t, odf, zipOf(t, replacePart(odfParts(odtMime), "content.xml", content)...))
	assertAbsent(t, unzip(t, out), "Test Author")
	if !hasValue(rep, "Embedded document metadata", "Test Author") {
		t.Errorf("inline metadata not reported: %+v", rep.Findings)
	}
}

func TestODFRDFWithLiteralsIsPointedOut(t *testing.T) {
	rdf := xmlDecl + `<rdf:RDF xmlns:rdf="` + nsRDF + `" xmlns:x="urn:x"><rdf:Description rdf:about=""><x:owner>Test Owner</x:owner></rdf:Description></rdf:RDF>`
	_, rep := clean(t, odf, zipOf(t, append(odfParts(odtMime), part{"manifest.rdf", rdf})...))
	if !hasWarning(rep, "RDF") {
		t.Errorf("no RDF warning: %q", rep.Warnings)
	}
	plain := xmlDecl + `<rdf:RDF xmlns:rdf="` + nsRDF + `"><rdf:Description rdf:about="content.xml"/></rdf:RDF>`
	_, rep = clean(t, odf, zipOf(t, append(odfParts(odtMime), part{"manifest.rdf", plain})...))
	if hasWarning(rep, "RDF") {
		t.Errorf("the standard manifest.rdf should pass quietly: %q", rep.Warnings)
	}
}

func TestODFDamageFailsClosed(t *testing.T) {
	data := zipOf(t, replacePart(odfParts(odtMime), "content.xml", "<office:text Test Author")...)
	if _, err := odf.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("got %v, want ErrMalformed", err)
	}
}

func TestODFManifestPathVariants(t *testing.T) {
	manifest := xmlDecl + `<manifest:manifest xmlns:manifest="` + nsManifest + `"><manifest:file-entry manifest:full-path="./Thumbnails/thumbnail.png" manifest:media-type="image/png"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/></manifest:manifest>`
	out, _ := clean(t, odf, zipOf(t, replacePart(odfParts(odtMime), "META-INF/manifest.xml", manifest)...))
	if bytes.Contains(unzip(t, out)["META-INF/manifest.xml"], []byte("Thumbnails")) {
		t.Errorf("manifest still lists the thumbnail")
	}
}
