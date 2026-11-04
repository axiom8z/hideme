package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

var testHandler = handler{limits: defaultLimits}

func contractCases() []coretest.Case {
	visible := asBytes(pageText)
	return []coretest.Case{
		{Name: "no metadata", Data: plainDoc(), MustContain: visible},
		{
			Name: "info dictionary and document ID",
			Data: infoDoc(),
			WantFields: []string{"Author", "Creator", "Producer", "Title", "Subject", "Keywords",
				"Creation date", "Modification date", "Trapped", "Custom metadata", "Document ID"},
			MustNotContain: asBytes("Test Author", "Test Creator App", "Test Producer App", "Test Title",
				"Test Custom Value", "20010203", "/Info", "/ID", "0123456789ABCDEF"),
			MustContain: visible,
		},
		{
			Name: "XMP on the catalog",
			Data: doc{
				catalog: "/Metadata 20 0 R",
				objects: func(b *builder) { b.stream(20, "/Type /Metadata /Subtype /XML", coretest.FakeXMP()) },
			}.build(),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: coretest.FakeXMPSecrets(),
			MustContain:    visible,
		},
		{
			Name: "compressed XMP on an image and a font descriptor",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 21 0 R>>>>",
				objects: func(b *builder) {
					b.stream(20, "/Type /Metadata /Subtype /XML /Filter /FlateDecode", compress(coretest.FakeXMP()))
					b.stream(21, "/Type /XObject /Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 "+
						"/ColorSpace /DeviceGray /Metadata 20 0 R", []byte{0x7F})
				},
			}.build(),
			WantFields:     []string{"Creator", "GPS coordinates"},
			MustNotContain: asBytes("/Metadata", "/Type /Metadata", "/Subtype /XML"),
			MustContain:    visible,
		},
		{
			Name: "metadata stream under a key nobody expects",
			Data: doc{
				page: "/Group <</S /Transparency /Private 20 0 R>>",
				objects: func(b *builder) {
					b.stream(20, "/Type /Metadata", []byte("<Test Stream Metadata/>"))
				},
			}.build(),
			WantFields:     []string{"XMP packet"},
			MustNotContain: asBytes("Test Stream Metadata", "/Private"),
			MustContain:    asBytes(pageText, "/Transparency"),
		},
		{
			Name: "flate stream holding XMP, with nothing saying it is metadata",
			Data: doc{
				page: "/Group <</S /Transparency /Private 20 0 R>>",
				objects: func(b *builder) {
					b.stream(20, "/Filter /FlateDecode", compress(coretest.FakeXMP()))
				},
			}.build(),
			WantFields:     []string{"Creator"},
			MustNotContain: asBytes("/Private"),
			MustContain:    visible,
		},
		{
			Name: "XMP-like text in a page stays, because it is content",
			Data: doc{
				page: "/Contents [4 0 R 20 0 R]",
				objects: func(b *builder) {
					b.stream(20, "/Filter /FlateDecode", compress([]byte("BT (<x:xmpmeta> is XMP) Tj ET")))
				},
			}.build(),
			MustContain: asBytes("/Contents [", "/FlateDecode"),
		},
		{
			Name: "XMP-like text in a form XObject stays, because it is drawn",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Fm0 20 0 R>>>>",
				objects: func(b *builder) {
					b.stream(20, "/Subtype /Form /BBox [0 0 1 1] /Filter /FlateDecode",
						compress([]byte("BT (<x:xmpmeta> is XMP) Tj ET")))
				},
			}.build(),
			MustContain: asBytes("/Fm0", "/BBox"),
		},
		{
			Name: "application private data",
			Data: doc{
				catalog: "/PieceInfo <</TestApp <</Private (Test Private Data)>>>> /LastModified (D:20010203040506Z)",
				page:    "/PieceInfo <</TestApp <</Private (Test Private Data)>>>> /LastModified (D:20010203040506Z)",
			}.build(),
			WantFields:     []string{"Application private data", "Last modified"},
			MustNotContain: asBytes("PieceInfo", "LastModified", "Test Private Data", "20010203"),
			MustContain:    visible,
		},
		{
			Name: "page thumbnail",
			Data: doc{
				page: "/Thumb 20 0 R",
				objects: func(b *builder) {
					b.stream(20, "/Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte("Test Thumbnail Secret"))
				},
			}.build(),
			WantFields:     []string{"Page thumbnail"},
			MustNotContain: asBytes("/Thumb", "Test Thumbnail Secret"),
			MustContain:    visible,
		},
		{
			Name: "attachments by every route",
			Data: doc{
				catalog: "/Names <</EmbeddedFiles 20 0 R /Dests 26 0 R>> /AF [23 0 R] /Collection <</Type /Collection>>",
				page:    "/Annots [24 0 R 25 0 R]",
				objects: func(b *builder) {
					b.obj(20, "<</Kids [21 0 R]>>")
					b.obj(21, "<</Limits [(a) (a)] /Names [(a) 22 0 R]>>")
					b.obj(22, "<</Type /Filespec /F (test-tree.txt) /UF (test-tree-unicode.txt) /EF <</F 27 0 R>>>>")
					b.obj(23, "<</Type /Filespec /F (test-af.txt) /EF <</F 28 0 R>>>>")
					b.obj(24, "<</Type /Annot /Subtype /FileAttachment /Rect [0 0 1 1] "+
						"/FS <</Type /Filespec /F (test-annot.txt) /EF <</F 29 0 R>>>>>>")
					b.obj(25, "<</Type /Annot /Subtype /Link /Rect [0 0 1 1]>>")
					b.obj(26, "<</Names []>>")
					b.stream(27, "/Type /EmbeddedFile", []byte("Test Tree Body"))
					b.stream(28, "/Type /EmbeddedFile", []byte("Test AF Body"))
					b.stream(29, "/Type /EmbeddedFile", []byte("Test Annot Body"))
				},
			}.build(),
			WantFields: []string{"Attachment", "Attachment collection"},
			MustNotContain: asBytes("EmbeddedFile", "/AF", "/Collection", "test-tree", "test-af.txt", "test-annot.txt",
				"Test Tree Body", "Test AF Body", "Test Annot Body", "FileAttachment"),
			MustContain: asBytes(pageText, "/Link", "/Dests"),
		},
		{
			Name: "annotation authors go and their text stays",
			Data: doc{
				page: "/Annots [20 0 R 21 0 R <</Subtype /Square /Rect [0 0 1 1] /T (Test Direct Author)>>]",
				objects: func(b *builder) {
					b.obj(20, "<</Type /Annot /Subtype /Text /Rect [0 0 1 1] /T (Test Author) /M (D:20010203040506Z) "+
						"/CreationDate (D:20010203040507Z) /NM (test-unique-name) /Contents (Test Comment Body) /Popup 21 0 R>>")
					b.obj(21, "<</Subtype /Popup /Rect [0 0 1 1] /T (Test Popup Author) /Parent 20 0 R>>")
				},
			}.build(),
			WantFields:     []string{"Annotation author", "Annotation date", "Annotation ID"},
			WantRetained:   []string{"Comment text"},
			MustNotContain: asBytes("Test Author", "Test Popup Author", "Test Direct Author", "20010203", "test-unique-name", "/T ", "/NM"),
			MustContain:    asBytes(pageText, "Test Comment Body"),
		},
		{
			Name: "form fields keep their names and values",
			Data: doc{
				catalog: "/AcroForm 20 0 R",
				page:    "/Annots [21 0 R 23 0 R]",
				objects: func(b *builder) {
					b.obj(20, "<</Fields [21 0 R 22 0 R] /DA (/Helv 0 Tf 0 g)>>")
					b.obj(21, "<</FT /Tx /Subtype /Widget /Rect [0 0 1 1] /T (test_widget_field) /V (Test Filled Value) /M (D:20010203040506Z)>>")
					b.obj(22, "<</FT /Tx /T (test_parent_field) /V (Test Parent Value) /Kids [23 0 R]>>")
					b.obj(23, "<</Subtype /Widget /Parent 22 0 R /Rect [0 0 1 1]>>")
				},
			}.build(),
			WantFields:     []string{"Annotation date"},
			WantRetained:   []string{"Form field value"},
			MustNotContain: asBytes("20010203"),
			MustContain:    asBytes("/T (test_widget_field)", "/V (Test Filled Value)", "/T (test_parent_field)", "/V (Test Parent Value)", "/DA"),
		},
		{
			Name: "signature with its permissions and validation data",
			Data: doc{
				catalog: "/AcroForm <</Fields [20 0 R] /SigFlags 3>> /Perms <</DocMDP 21 0 R>> /DSS <</Certs [22 0 R]>>",
				page:    "/Annots [20 0 R]",
				objects: func(b *builder) {
					b.obj(20, "<</FT /Sig /Subtype /Widget /Rect [0 0 1 1] /T (test_sig_field) /V 21 0 R>>")
					b.obj(21, "<</Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Name (Test Signer) "+
						"/Reason (Test Reason) /Location (Test Place) /ContactInfo (test@example.com) "+
						"/M (D:20010203040506Z) /ByteRange [0 1 2 3] /Contents <0123456789ABCDEF>>>")
					b.stream(22, "", []byte("Test Certificate"))
				},
			}.build(),
			WantFields: []string{"Signer name", "Signature reason", "Signature location", "Signer contact",
				"Signature date", "Digital signature", "Permission signatures", "Signature validation data"},
			MustNotContain: asBytes("Test Signer", "Test Reason", "Test Place", "test@example.com", "ByteRange", "/Perms",
				"/DSS", "Test Certificate", "/SigFlags", "0123456789ABCDEF"),
			MustContain: asBytes(pageText, "/T (test_sig_field)", "/FT /Sig"),
		},
		{
			Name: "behaviour stays",
			Data: doc{
				catalog: "/OpenAction <</S /JavaScript /JS (app.alert(1))>> /Names <</JavaScript <</Names []>>>> /AA <</WC 20 0 R>>",
				objects: func(b *builder) { b.obj(20, "<</S /JavaScript /JS (app.alert(2))>>") },
			}.build(),
			MustContain: asBytes("/OpenAction", "app.alert\\(1\\)", "/AA", "app.alert\\(2\\)"),
		},
		{
			Name: "embedded JPEG",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 20 0 R /Im1 21 0 R /Im2 22 0 R>>>>",
				objects: func(b *builder) {
					const image = "/Type /XObject /Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceRGB "
					b.stream(20, image+"/Filter /DCTDecode", fakeJPEG(true))
					b.stream(21, image+"/Filter [/DCTDecode]", fakeJPEG(true))
					b.stream(22, image+"/Filter [/FlateDecode /DCTDecode]", compress(fakeJPEG(true)))
				},
			}.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: asBytes("Test Camera GPS", "Exif"),
			MustContain:    asBytes(pageText, "Test Pixel Data"),
		},
		{
			Name: "older revisions",
			Data: incrementalDoc(),
			WantFields: []string{
				"Author", "Earlier revisions",
			},
			MustNotContain: asBytes("Test Old Author", "Test Deleted Redacted Text", "Test New Author"),
			MustContain:    asBytes("Test Replacement Text"),
		},
		{
			Name: "objects nothing refers to",
			Data: doc{objects: func(b *builder) {
				b.obj(20, "<</Secret (Test Orphan Secret)>>")
				b.stream(21, "", []byte("Test Orphan Stream"))
			}}.build(),
			WantFields:     []string{"Orphan objects"},
			MustNotContain: asBytes("Test Orphan"),
			MustContain:    visible,
		},
		{
			Name:           "object and cross-reference streams",
			Data:           xrefStreamDoc(),
			WantFields:     []string{"Author", "Creator", "Document ID"},
			MustNotContain: asBytes("/ObjStm", "/XRef", "x:xmpmeta"),
			MustContain:    asBytes("/Catalog", "/Page", "/FlateDecode"),
		},
		{
			Name:           "hybrid cross-reference",
			Data:           hybridDoc(),
			WantFields:     []string{"Author", "Custom metadata", "City"}, // all of them only in hidden objects
			MustNotContain: asBytes("/ObjStm", "/XRef", "x:xmpmeta", "Test Author", "Test City"),
			MustContain:    asBytes(pageText, "/Catalog", "/StructTreeRoot"),
		},
		{
			Name:           "data after the end of the file",
			Data:           append(plainDoc(), []byte("Test Trailing Secret")...),
			WantFields:     []string{"Data after end of file"},
			MustNotContain: asBytes("Test Trailing Secret"),
			MustContain:    visible,
		},
		{
			Name: "private catalog, page and trailer keys",
			Data: doc{
				catalog: "/TestVendorPrivate (Test Vendor Catalog) /Lang (en-US)",
				page:    "/TestVendorPage (Test Vendor Page) /Rotate 90",
				trailer: "/PTEX.Fullbanner (Test TeX Banner)",
			}.build(),
			WantFields:     []string{"Unknown catalog entry", "Unknown page entry", "Private trailer entry"},
			MustNotContain: asBytes("TestVendor", "Test Vendor", "PTEX", "Test TeX Banner"),
			MustContain:    asBytes(pageText, "/Lang (en-US)", "/Rotate 90"),
		},
		{
			Name: "pdfTeX records of included figures",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Fm0 20 0 R>>>>",
				objects: func(b *builder) {
					b.stream(20, "/Type /XObject /Subtype /Form /BBox [0 0 1 1] /PTEX.FileName (/home/testuser/figure.pdf) "+
						"/PTEX.InfoDict 21 0 R /PTEX.PageNumber 1", []byte("q Q"))
					b.obj(21, "<</Author (Test Figure Author)>>")
				},
			}.build(),
			WantFields:     []string{"Unknown image or form entry"},
			MustNotContain: asBytes("PTEX", "testuser", "Test Figure Author"),
			MustContain:    asBytes(pageText, "/Fm0", "/BBox"),
		},
		{
			Name: "image keys renderers need stay",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 20 0 R>>>>",
				objects: func(b *builder) {
					b.stream(20, "/Type /XObject /Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray "+
						"/Decode [1 0] /Interpolate true /Intent /Perceptual /SMask 21 0 R /OC <</Type /OCG /Name (Test Layer)>> "+
						"/TestVendorImageKey (Test Vendor Image)", []byte{0x7F})
					b.stream(21, "/Type /XObject /Subtype /Image /W 1 /H 1 /BPC 8 /CS /DeviceGray /Matte [0]", []byte{0x7E})
				},
			}.build(),
			WantFields:     []string{"Unknown image or form entry"},
			MustNotContain: asBytes("TestVendorImageKey", "Test Vendor Image"),
			MustContain: asBytes("/Decode [1 0]", "/Interpolate true", "/Intent /Perceptual", "/SMask", "/OC", "Test Layer",
				"/W 1 /H 1 /BPC 8 /CS /DeviceGray /Matte [0]"),
		},
		{
			Name: "article threads and layers name their authors too",
			Data: doc{
				catalog: "/Threads [20 0 R <</I <</Author (Test Direct Thread Author)>>>>] " +
					"/OCProperties <</OCGs [21 0 R] /D <</ON [21 0 R]>>>>",
				objects: func(b *builder) {
					b.obj(20, "<</Type /Thread /I <</Author (Test Thread Author) /Title (Test Thread Title)>>>>")
					b.obj(21, "<</Type /OCG /Name (Kept Layer Name) /Usage <</CreatorInfo <</Creator (Test Layer App) /Subtype /Artwork>>>>>>")
				},
			}.build(),
			WantFields:     []string{"Author", "Title", "Layer creator"},
			MustNotContain: asBytes("Test Thread Author", "Test Direct Thread Author", "Test Thread Title", "Test Layer App", "CreatorInfo"),
			MustContain:    asBytes(pageText, "/Threads", "/Type /Thread", "Kept Layer Name"),
		},
		{
			Name: "the Info dictionary referred to from somewhere that would keep it",
			Data: doc{
				trailer: fmt.Sprintf("/Info %d 0 R", infoObject),
				page:    "/Resources <</Font <</F1 5 0 R /Leak 10 0 R>>>>",
				objects: func(b *builder) { b.obj(infoObject, infoBody) },
			}.build(),
			WantFields:     []string{"Author"},
			MustNotContain: asBytes("Test Author", "/Leak"),
			MustContain:    visible,
		},
		{
			Name: "a font carrying XMP stays, because the page needs it",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /ExtGState <</Private 20 0 R>>>>",
				objects: func(b *builder) {
					b.stream(20, "/Length1 3 /Filter /FlateDecode", compress(coretest.FakeXMP()))
				},
			}.build(),
			MustContain: asBytes("/Length1", "/ExtGState <</Private"),
		},
		{
			Name: "document part metadata",
			Data: doc{
				page:    "/DPart 20 0 R",
				catalog: "/DPartRoot <</DPartRootNode 20 0 R>>",
				objects: func(b *builder) { b.obj(20, "<</DPM <</Recipient (Test Recipient)>>>>") },
			}.build(),
			WantFields:     []string{"Document part metadata"},
			MustNotContain: asBytes("Test Recipient", "/DPM"),
			MustContain:    asBytes(pageText, "/DPart", "/DPartRoot"),
		},
		{
			Name: "image metadata the handler cannot reach is still kept with a warning",
			Data: doc{
				page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 20 0 R>>>>",
				objects: func(b *builder) {
					b.stream(20, "/Subtype /Image /Width 1 /Height 1 /Filter /JPXDecode", []byte("Test JPX Bytes"))
				},
			}.build(),
			MustContain: asBytes("Test JPX Bytes"),
		},
	}
}

// incrementalDoc is a document followed by an update that changes the author and
// replaces the page text, which leaves the originals sitting in the file.
func incrementalDoc() []byte {
	b := newBuilder("1.7")
	trailer := fmt.Sprintf("/Root 1 0 R /Info %d 0 R", infoObject)
	doc{
		contents: func(b *builder) { b.stream(4, "", []byte("BT (Test Deleted Redacted Text) Tj ET")) },
		objects:  func(b *builder) { b.obj(infoObject, "<</Author (Test Old Author)>>") },
	}.write(b)
	b.table(trailer)
	b.stream(4, "", []byte("BT (Test Replacement Text) Tj ET"))
	b.obj(infoObject, "<</Author (Test New Author)>>")
	b.table(trailer)
	return b.bytes()
}

func TestContract(t *testing.T) { coretest.Contract(t, testHandler, contractCases()) }

func TestRobustness(t *testing.T) {
	for name, data := range map[string][]byte{
		"kitchen sink": kitchenSink(),
		"xref stream":  xrefStreamDoc(),
		"hybrid":       hybridDoc(),
		"incremental":  incrementalDoc(),
	} {
		t.Run(name, func(t *testing.T) { coretest.Robustness(t, testHandler, data) })
	}
}

func clean(t *testing.T, data []byte, opts core.Options) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := testHandler.Clean(context.Background(), core.BytesSource(data), &out, opts)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func inspect(t *testing.T, data []byte) *core.Report {
	t.Helper()
	rep, err := testHandler.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return rep
}

// valueOf returns the value of the first finding with a field, or fails.
func valueOf(t *testing.T, rep *core.Report, field string) string {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Field == field {
			return f.Value
		}
	}
	t.Fatalf("no finding for %q in %+v", field, rep.Findings)
	return ""
}

func valuesOf(rep *core.Report, field string) []string {
	var out []string
	for _, f := range rep.Findings {
		if f.Field == field {
			out = append(out, f.Value)
		}
	}
	return out
}

func wantWarning(t *testing.T, rep *core.Report, fragment string) {
	t.Helper()
	if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, fragment) }) {
		t.Errorf("no warning mentioning %q in %q", fragment, rep.Warnings)
	}
}

// pagesContent returns each page's decoded content, in order. It is the strongest
// content check available without a renderer, since it compares exactly what a
// viewer would execute.
func pagesContent(t *testing.T, data []byte) [][]byte {
	t.Helper()
	d, err := load(context.Background(), data, defaultLimits)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	catalog, _ := d.resolve(d.root).(*pdfDict)
	var out [][]byte
	var visit func(v object)
	visit = func(v object) {
		node, ok := d.resolve(v).(*pdfDict)
		if !ok {
			return
		}
		if kids, ok := d.resolve(node.get("Kids")).(pdfArray); ok {
			for _, k := range kids {
				visit(k)
			}
			return
		}
		streams := []object{node.get("Contents")}
		if a, ok := d.resolve(node.get("Contents")).(pdfArray); ok {
			streams = a
		}
		var page []byte
		for _, v := range streams {
			if s, ok := d.resolve(v).(*pdfStream); ok {
				decoded, err := d.decodeStream(s)
				if err != nil {
					t.Fatalf("decoding a content stream: %v", err)
				}
				page = append(page, decoded...)
			}
		}
		out = append(out, page)
	}
	visit(catalog.get("Pages"))
	return out
}

func TestContentSurvives(t *testing.T) {
	for _, c := range contractCases() {
		t.Run(c.Name, func(t *testing.T) {
			before := pagesContent(t, c.Data)
			after := pagesContent(t, cleanOK(t, c.Data))
			if len(before) != len(after) {
				t.Fatalf("%d pages became %d", len(before), len(after))
			}
			for i := range before {
				if !bytes.Equal(before[i], after[i]) {
					t.Errorf("page %d changed:\n before %q\n after  %q", i, before[i], after[i])
				}
			}
		})
	}
}

func cleanOK(t *testing.T, data []byte) []byte {
	t.Helper()
	out, _ := clean(t, data, core.Options{})
	return out
}

func TestDecodedValues(t *testing.T) {
	utf16Title := "<FEFF0054006500730074002000C90074006500200D9E>"
	data := doc{
		trailer: fmt.Sprintf("/Info %d 0 R", infoObject),
		objects: func(b *builder) {
			b.obj(infoObject, "<</Title "+utf16Title+" /Author (Test\\040\\(Escaped\\) \\200 Author)"+
				" /CreationDate (D:20010203040506+01'00') /ModDate (D:2001)"+
				" /Subject <FEFF001B656E001B0048>>>")
		},
	}.build()
	rep := inspect(t, data)
	if got, want := valueOf(t, rep, "Title"), "Test \u00C9te \u0D9E"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
	if got, want := valueOf(t, rep, "Author"), "Test (Escaped) \u2022 Author"; got != want {
		t.Errorf("Author = %q, want %q", got, want)
	}
	if got, want := valueOf(t, rep, "Creation date"), "2001-02-03 04:05:06 +01:00"; got != want {
		t.Errorf("Creation date = %q, want %q", got, want)
	}
	if got, want := valueOf(t, rep, "Modification date"), "2001"; got != want {
		t.Errorf("Modification date = %q, want %q", got, want)
	}
	if got, want := valueOf(t, rep, "Subject"), "H"; got != want {
		t.Errorf("Subject with a language tag = %q, want %q", got, want)
	}
}

func TestRevisionsAndOrphansAreCounted(t *testing.T) {
	rep := inspect(t, incrementalDoc())
	if got, want := valueOf(t, rep, "Earlier revisions"), "1 older revision"; got != want {
		t.Errorf("Earlier revisions = %q, want %q", got, want)
	}
	if got := valueOf(t, rep, "Author"); got != "Test New Author" {
		t.Errorf("the newest definition must win; Author = %q", got)
	}
	got := pagesContent(t, cleanOK(t, incrementalDoc()))
	if len(got) != 1 || !bytes.Contains(got[0], []byte("Test Replacement Text")) {
		t.Errorf("the page must have the updated text; got %q", got)
	}

	orphaned := inspect(t, doc{objects: func(b *builder) {
		b.obj(20, "<</Secret 1>>")
		b.obj(21, "<</Secret 2>>")
	}}.build())
	if got, want := valueOf(t, orphaned, "Orphan objects"), "2 objects nothing refers to"; got != want {
		t.Errorf("Orphan objects = %q, want %q", got, want)
	}
}

// A linearized file has two table sections and a hint stream by design, and none
// of that is a hidden revision or an orphan. Reporting them as such would alarm
// users about every file saved for fast web view.
func TestLinearizedIsNotARevision(t *testing.T) {
	b := newBuilder("1.7")
	doc{}.write(b)
	b.table("/Root 1 0 R")
	b.stream(8, "/S 10", []byte("Test Hint Tables"))
	b.obj(9, fmt.Sprintf("<</Linearized 1 /L 1000 /O 3 /E 500 /N 1 /T 900 /H [%d 10]>>", b.offsets[8]))
	b.table("/Root 1 0 R")
	rep := inspect(t, b.bytes())
	for _, field := range []string{"Earlier revisions", "Orphan objects"} {
		if got := valuesOf(rep, field); len(got) != 0 {
			t.Errorf("%s = %q on a linearized file", field, got)
		}
	}
}

func TestOutputShape(t *testing.T) {
	out := cleanOK(t, kitchenSink())
	if !bytes.HasPrefix(out, []byte("%PDF-1.7\n")) {
		t.Errorf("header = %q", out[:12])
	}
	if !bytes.HasSuffix(out, []byte("%%EOF\n")) {
		t.Errorf("the file must end at its %%%%EOF, got %q", out[len(out)-12:])
	}
	if n := bytes.Count(out, []byte("xref\n")); n != 2 { // "xref" and "startxref"
		t.Errorf("expected a single table; found the keyword %d times", n)
	}
	if n := bytes.Count(out, []byte("trailer")); n != 1 {
		t.Errorf("expected one trailer, found %d", n)
	}
	i := bytes.Index(out, []byte("trailer"))
	trailer := out[i:]
	if !bytes.Contains(trailer, []byte("/Root 1 0 R")) || !bytes.Contains(trailer, []byte("/Size")) {
		t.Errorf("the trailer lacks its required keys: %q", trailer)
	}
	for _, key := range []string{"/Info", "/ID", "/Prev", "/Encrypt", "/Linearized"} {
		if bytes.Contains(out, []byte(key)) {
			t.Errorf("the output contains %s", key)
		}
	}
	if bytes.Contains(out, []byte(" 1 obj")) {
		t.Error("every object must be generation 0")
	}
	if bytes.Contains(out, []byte("HideMe")) {
		t.Error("the output must not credit a producer of ours")
	}
	// Keep the header's version when it is readable, and fall back when it is not.
	// A longer header moves the body, which the recovery scan deals with.
	for header, want := range map[string]string{"1.3": "1.3", "2.0": "2.0", "1.7 Test Comment": "1.7", "bad": "1.7"} {
		data := bytes.Replace(plainDoc(), []byte("%PDF-1.7"), []byte("%PDF-"+header), 1)
		got := cleanOK(t, data)
		if !bytes.HasPrefix(got, []byte("%PDF-"+want+"\n")) {
			t.Errorf("header %q: output starts %q, want version %s", header, got[:12], want)
		}
	}
}

func TestDeterministicRenumbering(t *testing.T) {
	// The same document with its objects written in a different order, and with
	// different numbers, has one cleaned form up to the order of the file's own
	// references.
	b := newBuilder("1.7")
	b.obj(40, "<</Type /Font /Subtype /Type1 /BaseFont /Helvetica>>")
	b.stream(30, "", []byte(pageBody))
	b.obj(20, "<</Type /Page /Parent 10 0 R /MediaBox [0 0 612 792] /Contents 30 0 R /Resources <</Font <</F1 40 0 R>>>>>>")
	b.obj(10, "<</Type /Pages /Kids [20 0 R] /Count 1>>")
	b.obj(5, "<</Type /Catalog /Pages 10 0 R>>")
	b.table("/Root 5 0 R")
	got := cleanOK(t, b.bytes())
	want := cleanOK(t, plainDoc())
	if !bytes.Equal(got, want) {
		t.Errorf("renumbering depends on the input's numbers or layout:\n%s\n---\n%s", got, want)
	}
}

func TestSyntaxRoundTrips(t *testing.T) {
	data := doc{
		catalog: `/Lang (Text with \(parens\), a \\ backslash, \r CR and \n LF) ` +
			`/URI <</Base <0AFF>>> /Strange#20Name#23 (x) /Dests <</Nested [1.5 -2 +3 .5 true false null [] <<>>]>> ` +
			`/PageLabels <</Nums [0 <</S /D>>]>> /ViewerPreferences <</A#42%comment` + "\n" + ` 12345678901234567890>>`,
		page: "/Resources <</Font <</F1 5 0 R>> /Pattern <</P1 << >>>>>>",
	}.build()
	out := cleanOK(t, data)
	for _, want := range []string{
		`/Lang (Text with \(parens\), a \\ backslash, \r CR and \n LF)`,
		"/URI <</Base <0AFF>>>",
		"[1.5 -2 3 .5 true false null [] <<>>]",
		"12345678901234567890",
		"/AB ",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the output lost %q:\n%s", want, out)
		}
	}
	// A catalog allowlist must have removed the invented key, escapes and all.
	if bytes.Contains(out, []byte("Strange")) {
		t.Errorf("an invented catalog key survived:\n%s", out)
	}
	if !bytes.Equal(cleanOK(t, out), out) {
		t.Error("the output does not round trip")
	}
	// Keys keep their order rather than being sorted or shuffled.
	if !bytes.Contains(out, []byte("/Type /Page /Parent 2 0 R /MediaBox")) {
		t.Errorf("page keys were reordered:\n%s", out)
	}
}

func TestNamesWithEscapes(t *testing.T) {
	data := doc{
		page: "/Resources <</Font <</F#201 5 0 R /A#23B 5 0 R /C#D 5 0 R>>>>",
	}.build()
	out := cleanOK(t, data)
	for _, want := range []string{"/F#201 ", "/A#23B ", "/C#23D "} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

func TestStreamLengths(t *testing.T) {
	for name, build := range map[string]func(b *builder){
		"indirect": func(b *builder) {
			b.rawStream(4, "/Length 20 0 R", []byte(pageBody))
			b.obj(20, fmt.Sprintf("%d", len(pageBody)))
		},
		"too short": func(b *builder) { b.rawStream(4, "/Length 3", []byte(pageBody)) },
		"too long":  func(b *builder) { b.rawStream(4, "/Length 99999999", []byte(pageBody)) },
		"missing":   func(b *builder) { b.rawStream(4, "", []byte(pageBody)) },
		"dangling":  func(b *builder) { b.rawStream(4, "/Length 99 0 R", []byte(pageBody)) },
		"self":      func(b *builder) { b.rawStream(4, "/Length 4 0 R", []byte(pageBody)) },
		"negative":  func(b *builder) { b.rawStream(4, "/Length -1", []byte(pageBody)) },
	} {
		t.Run(name, func(t *testing.T) {
			out := cleanOK(t, doc{contents: build}.build())
			want := fmt.Sprintf("/Length %d", len(pageBody))
			if !bytes.Contains(out, []byte(want)) {
				t.Errorf("the output has no %q:\n%s", want, out)
			}
			got := pagesContent(t, out)
			if len(got) != 1 || !bytes.Equal(got[0], []byte(pageBody)) {
				t.Errorf("the page became %q", got)
			}
			if bytes.Contains(out, []byte("20 0 obj")) {
				t.Error("a length object is no longer used, so it must be gone")
			}
		})
	}
}

func TestRecoveryByScanning(t *testing.T) {
	broken := func(mutate func(data []byte) []byte) []byte { return mutate(infoDoc()) }
	cases := map[string][]byte{
		"no startxref": broken(func(d []byte) []byte {
			return bytes.ReplaceAll(d, []byte("startxref"), []byte("startxxxx"))
		}),
		"startxref points nowhere": broken(func(d []byte) []byte {
			i := bytes.LastIndex(d, []byte("startxref"))
			return append(d[:i:i], []byte("startxref\n3\n%%EOF\n")...)
		}),
		"table entries are wrong": broken(func(d []byte) []byte {
			return bytes.ReplaceAll(d, []byte(" 00000 n "), []byte(" 00007 n "))
		}),
		"truncated before the table": broken(func(d []byte) []byte {
			return d[:bytes.Index(d, []byte("xref\n"))]
		}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, core.Options{})
			wantWarning(t, rep, "recovered by scanning")
			if got := pagesContent(t, out); len(got) != 1 || !bytes.Equal(got[0], []byte(pageBody)) {
				t.Errorf("the page became %q", got)
			}
			if bytes.Contains(out, []byte("Test Author")) {
				t.Error("the Info dictionary survived recovery")
			}
			if residual := inspect(t, out).Residual(); len(residual) != 0 {
				t.Errorf("residual findings: %+v", residual)
			}
			if !bytes.Equal(cleanOK(t, out), out) {
				t.Error("cleaning the recovered output is not idempotent")
			}
		})
	}
}

// In recovery a stream's data is stepped over rather than searched, so an object
// inside one, such as an attached PDF, cannot replace the real thing.
func TestRecoveryIgnoresObjectsInsideStreams(t *testing.T) {
	data := doc{
		catalog: "/Lang (en)",
		objects: func(b *builder) {
			b.stream(20, "", []byte("1 0 obj <</Type /Catalog /Pages 99 0 R /Fake (Test In Stream)>> endobj\n"))
		},
	}.build()
	data = bytes.ReplaceAll(data, []byte("startxref"), []byte("startxxxx"))
	out, _ := clean(t, data, core.Options{})
	if bytes.Contains(out, []byte("Test In Stream")) || !bytes.Contains(out, []byte("/Lang (en)")) {
		t.Errorf("text inside a stream replaced the catalog:\n%s", out)
	}
}

func TestRecoveryCountsSuperseded(t *testing.T) {
	data := incrementalDoc()
	data = bytes.ReplaceAll(data, []byte("startxref"), []byte("startxxxx"))
	out, rep := clean(t, data, core.Options{})
	if !strings.Contains(valueOf(t, rep, "Earlier revisions"), "superseded") {
		t.Errorf("Earlier revisions = %q", valueOf(t, rep, "Earlier revisions"))
	}
	if got := valueOf(t, rep, "Author"); got != "Test New Author" {
		t.Errorf("the last definition must win; Author = %q", got)
	}
	if bytes.Contains(out, []byte("Test Deleted")) || !bytes.Contains(out, []byte("Test Replacement")) {
		t.Errorf("recovery chose the wrong revision:\n%s", out)
	}
}

func TestWarnings(t *testing.T) {
	rep := inspect(t, kitchenSink())
	for _, fragment := range []string{
		"/OpenAction", "JavaScript", "digital signature", "form field values",
		"comment text", "JPXDecode",
	} {
		wantWarning(t, rep, fragment)
	}
	// Clean repeats warnings in every run, and logs must not recreate metadata, so
	// none of them may quote a value.
	for _, w := range rep.Warnings {
		for _, secret := range []string{"Test Comment Body", "Test Field Value", "Test Signer", "test-attachment"} {
			if strings.Contains(w, secret) {
				t.Errorf("warning %q quotes the value %q", w, secret)
			}
		}
	}
	files := inspect(t, doc{
		catalog: "/OpenAction <</S /GoToR /F (C:\\\\Users\\\\Test User\\\\linked.pdf) /D [0 /Fit]>>",
	}.build())
	wantWarning(t, files, "external files")
	if got := valueOf(t, files, "External file reference"); !strings.Contains(got, "Test User") {
		t.Errorf("External file reference = %q", got)
	}
	xfa := inspect(t, doc{catalog: "/AcroForm <</Fields [] /XFA 20 0 R>>", objects: func(b *builder) {
		b.stream(20, "", []byte("<xdp:xdp/>"))
	}}.build())
	wantWarning(t, xfa, "XFA")
}

func TestXFAPacketOfXMP(t *testing.T) {
	data := doc{
		catalog: "/AcroForm <</Fields [] /XFA [(config) 20 0 R (xmpmeta) 21 0 R (template) 22 0 R]>>",
		objects: func(b *builder) {
			b.stream(20, "", []byte("<config/>"))
			b.stream(21, "/Filter /FlateDecode", compress(coretest.FakeXMP()))
			b.stream(22, "", []byte("<template/>"))
		},
	}.build()
	out, rep := clean(t, data, core.Options{})
	if !bytes.Contains(out, []byte("/XFA [(config) ")) || !bytes.Contains(out, []byte("(template) ")) {
		t.Errorf("the rest of the form must stay:\n%s", out)
	}
	if bytes.Contains(out, []byte("xmpmeta")) {
		t.Errorf("the XMP packet's entry must go too:\n%s", out)
	}
	if got := valueOf(t, rep, "Creator"); got != "Test Author" {
		t.Errorf("Creator = %q", got)
	}
}

func TestSharedAnnotationReachedFirstByAnotherRoute(t *testing.T) {
	// A destination in the catalog reaches the annotation before the page does. Its
	// /Subtype and /Rect are missing, so only the /Annots role can tell what it is.
	data := doc{
		catalog: "/Dests <</First 20 0 R>>",
		page:    "/Annots 21 0 R",
		objects: func(b *builder) {
			b.obj(20, "<</T (Test Hidden Author) /Contents (Test Body)>>")
			b.obj(21, "[20 0 R]")
		},
	}.build()
	out, rep := clean(t, data, core.Options{})
	if bytes.Contains(out, []byte("Test Hidden Author")) {
		t.Errorf("the author survived:\n%s", out)
	}
	if got := valueOf(t, rep, "Annotation author"); got != "Test Hidden Author" {
		t.Errorf("Annotation author = %q", got)
	}
}

func TestFieldInAnnotsKeepsItsName(t *testing.T) {
	data := doc{
		catalog: "/AcroForm <</Fields [20 0 R]>>",
		page:    "/Annots [20 0 R]",
		objects: func(b *builder) { b.obj(20, "<</FT /Tx /T (test_kept_name)>>") },
	}.build()
	if out := cleanOK(t, data); !bytes.Contains(out, []byte("/T (test_kept_name)")) {
		t.Errorf("a form field lost its name:\n%s", out)
	}
}

func TestStructTreeTitleIsNotAnAuthor(t *testing.T) {
	data := doc{
		catalog: "/StructTreeRoot <</Type /StructTreeRoot /K [<</S /P /T (Test Element Title)>>]>>",
	}.build()
	if out := cleanOK(t, data); !bytes.Contains(out, []byte("/T (Test Element Title)")) {
		t.Errorf("a /T outside any annotation was removed:\n%s", out)
	}
}

func TestEmbeddedJPEG(t *testing.T) {
	image := fakeJPEG(true)
	data := doc{
		page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 20 0 R>>>>",
		objects: func(b *builder) {
			b.stream(20, "/Subtype /Image /Filter /DCTDecode", image)
		},
	}.build()
	out, rep := clean(t, data, core.Options{})
	want := fakeJPEG(false)
	if !bytes.Contains(out, fmt.Appendf(nil, "/Length %d", len(want))) {
		t.Errorf("/Length was not updated to %d:\n%s", len(want), out)
	}
	if !bytes.Contains(out, want) {
		t.Error("the cleaned image is not in the output")
	}
	i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "GPS coordinates" })
	if i < 0 || !strings.HasSuffix(rep.Findings[i].Where, "APP1/Exif/GPS") || !strings.HasPrefix(rep.Findings[i].Where, "object 20") {
		t.Errorf("the nested report was not merged with a location: %+v", rep.Findings)
	}
}

func TestEmbeddedJPEGFailures(t *testing.T) {
	jpegDoc := func(dict string, image []byte) []byte {
		return doc{
			page:    "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 20 0 R>>>>",
			objects: func(b *builder) { b.stream(20, "/Subtype /Image "+dict, image) },
		}.build()
	}

	t.Run("not a recognised format keeps the bytes with a warning", func(t *testing.T) {
		out, rep := clean(t, jpegDoc("/Filter /DCTDecode", []byte("Test Not A JPEG")), core.Options{})
		wantWarning(t, rep, "not recognised")
		if !bytes.Contains(out, []byte("Test Not A JPEG")) {
			t.Error("the unrecognised bytes must be kept")
		}
	})
	t.Run("a damaged JPEG fails the document", func(t *testing.T) {
		damaged := fakeJPEG(true)
		damaged = damaged[:len(damaged)-2]
		_, err := testHandler.Clean(context.Background(), core.BytesSource(jpegDoc("/Filter /DCTDecode", damaged)), &bytes.Buffer{}, core.Options{})
		if !errors.Is(err, core.ErrMalformed) {
			t.Errorf("err = %v, want ErrMalformed", err)
		}
	})
	t.Run("a filter chain around a JPEG is unwrapped", func(t *testing.T) {
		out, rep := clean(t, jpegDoc("/Filter [/ASCIIHexDecode /DCTDecode] /DecodeParms [null <</ColorTransform 1>>]",
			hexEncode(fakeJPEG(true))), core.Options{})
		if got := valueOf(t, rep, "GPS coordinates"); got != coretest.EiffelTower {
			t.Errorf("GPS coordinates = %q", got)
		}
		if !bytes.Contains(out, fakeJPEG(false)) {
			t.Errorf("the output lacks the cleaned, unwrapped image:\n%s", out)
		}
		if !bytes.Contains(out, []byte("/Filter /DCTDecode /DecodeParms <</ColorTransform 1>>")) {
			t.Errorf("the dictionary does not describe the new data:\n%s", out)
		}
	})
	t.Run("an indirect filter chain leaves no object behind", func(t *testing.T) {
		data := doc{
			page: "/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 20 0 R>>>>",
			objects: func(b *builder) {
				b.stream(20, "/Subtype /Image /Filter 21 0 R", hexEncode(fakeJPEG(true)))
				b.obj(21, "[/ASCIIHexDecode /DCTDecode]")
			},
		}.build()
		out := cleanOK(t, data)
		if bytes.Contains(out, []byte("ASCIIHex")) {
			t.Errorf("the old filter chain survived:\n%s", out)
		}
		if !bytes.Equal(cleanOK(t, out), out) {
			t.Errorf("not idempotent:\n%s", out)
		}
	})
	t.Run("an undecodable wrapper keeps the bytes with a warning", func(t *testing.T) {
		out, rep := clean(t, jpegDoc("/Filter [/VendorDecode /DCTDecode]", []byte("Test Vendor Bytes")), core.Options{})
		wantWarning(t, rep, "JPXDecode")
		if !bytes.Contains(out, []byte("Test Vendor Bytes")) {
			t.Error("the image must be kept as it is")
		}
	})
	t.Run("a wrapped stream with no JPEG inside stays as it was", func(t *testing.T) {
		out, rep := clean(t, jpegDoc("/Filter [/ASCIIHexDecode /DCTDecode]", hexEncode([]byte("Not A JPEG"))), core.Options{})
		wantWarning(t, rep, "not recognised")
		if !bytes.Contains(out, []byte("[/ASCIIHexDecode /DCTDecode]")) {
			t.Errorf("the chain must stay when nothing was cleaned:\n%s", out)
		}
	})
	t.Run("nesting depth is enforced", func(t *testing.T) {
		_, err := testHandler.Clean(context.Background(), core.BytesSource(jpegDoc("/Filter /DCTDecode", fakeJPEG(false))),
			&bytes.Buffer{}, core.Options{Depth: core.MaxNestedDepth})
		if !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", err)
		}
	})
}

func TestEncrypted(t *testing.T) {
	cases := map[string][]byte{
		"trailer": doc{trailer: "/Encrypt 20 0 R", objects: func(b *builder) {
			b.obj(20, "<</Filter /Standard /V 1 /R 2 /O <00> /U <00> /P -4>>")
		}}.build(),
		"direct in the trailer": doc{trailer: "/Encrypt <</Filter /Standard>>"}.build(),
		"trailer lost": bytes.ReplaceAll(doc{objects: func(b *builder) {
			b.obj(20, "<</Filter /Standard /V 1 /R 2 /O <00> /U <00> /P -4>>")
		}}.build(), []byte("trailer"), []byte("xxxxxxx")),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := testHandler.Inspect(context.Background(), core.BytesSource(data), core.Options{})
			if !errors.Is(err, core.ErrEncrypted) {
				t.Errorf("Inspect: err = %v, want ErrEncrypted", err)
			}
			_, err = testHandler.Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{})
			if !errors.Is(err, core.ErrEncrypted) {
				t.Errorf("Clean: err = %v, want ErrEncrypted", err)
			}
		})
	}
}

func TestDecompressionLimits(t *testing.T) {
	bomb := compress(make([]byte, 1<<20))
	small := defaultLimits
	small.maxDecodedStream = 1 << 16
	small.maxScanned = 1 << 16
	h := handler{limits: small}
	cases := map[string][]byte{
		"metadata stream": doc{catalog: "/Metadata 20 0 R", objects: func(b *builder) {
			b.stream(20, "/Type /Metadata /Filter /FlateDecode", bomb)
		}}.build(),
		"stream scanned for XMP": doc{page: "/Group <</Private 20 0 R>>", objects: func(b *builder) {
			b.stream(20, "/Filter /FlateDecode", bomb)
		}}.build(),
	}
	// An object stream bomb, hidden by a trailer that sends the reader to recovery.
	objStmBomb := newBuilder("1.5")
	objStmBomb.stream(6, "/Type /ObjStm /N 1 /First 4 /Filter /FlateDecode", bomb)
	objStmBomb.buf.WriteString("trailer <</Root 1 0 R>>\n")
	cases["object stream"] = objStmBomb.bytes()
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
				t.Errorf("Inspect: err = %v, want ErrTooLarge", err)
			}
			if _, err := h.Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrTooLarge) {
				t.Errorf("Clean: err = %v, want ErrTooLarge", err)
			}
		})
	}

	// The same streams are fine within the limits, and a stream that has to be
	// scanned does not have to fit the much smaller limit on streams held in memory.
	generous := defaultLimits
	generous.maxDecodedStream = 1 << 16
	data := cases["stream scanned for XMP"]
	if _, err := (handler{limits: generous}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); err != nil {
		t.Errorf("a large image-sized stream was refused: %v", err)
	}

	tiny := defaultLimits
	tiny.maxValues = 10
	if _, err := (handler{limits: tiny}).Inspect(context.Background(), core.BytesSource(infoDoc()), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("value limit: err = %v, want ErrTooLarge", err)
	}
}

func TestHostileStructures(t *testing.T) {
	deep := strings.Repeat("[", maxNesting+50) + strings.Repeat("]", maxNesting+50)
	chain := func() []byte {
		// Each stream's /Length is a reference to the next stream.
		b := newBuilder("1.7")
		doc{page: "/Group <</A 100 0 R>>"}.write(b)
		for i := 100; i < 20100; i++ {
			b.rawStream(i, fmt.Sprintf("/Length %d 0 R", i+1), []byte("x"))
		}
		b.table("/Root 1 0 R")
		return b.bytes()
	}
	filterChain := func() []byte {
		// Reading each object stream needs an object from the next one.
		b := newBuilder("1.5")
		doc{page: "/Group <</A 100 0 R>>"}.write(b)
		rows := map[int][2]int{}
		for i := range 2000 {
			container, inner := 1000+i, 100+i
			rows[inner] = [2]int{2, container}
			body := fmt.Sprintf("%d 0 null", inner)
			b.rawStream(container, fmt.Sprintf("/Type /ObjStm /N 1 /First 5 /Filter %d 0 R /Length %d", 101+i, len(body)), []byte(body))
			rows[container] = [2]int{1, b.offsets[container]}
		}
		for num, off := range b.offsets {
			if _, ok := rows[num]; !ok {
				rows[num] = [2]int{1, off}
			}
		}
		size := 3001
		var plain []byte
		for num := range size {
			e := rows[num]
			plain = append(plain, byte(e[0]), byte(e[1]>>16), byte(e[1]>>8), byte(e[1]), 0)
		}
		at := b.buf.Len()
		fmt.Fprintf(&b.buf, "3000 0 obj\n<</Type /XRef /Size %d /W [1 3 1] /Root 1 0 R /Length %d>>\nstream\n", size, len(plain))
		b.buf.Write(plain)
		fmt.Fprintf(&b.buf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", at)
		return b.bytes()
	}
	cases := map[string][]byte{
		"empty":             {},
		"header only":       []byte("%PDF-"),
		"prev loop":         append(plainDoc(), []byte("xref\n0 0\ntrailer\n<</Root 1 0 R /Prev 9999>>\nstartxref\n0\n%%EOF\n")...),
		"deep nesting":      doc{catalog: "/Dests " + deep}.build(),
		"page tree loop":    doc{objects: func(b *builder) { b.obj(2, "<</Type /Pages /Kids [2 0 R 3 0 R] /Count 1>>") }}.build(),
		"catalog is number": doc{trailer: "/Root 5 0 R"}.build(),
		"reference loop": doc{catalog: "/Dests 20 0 R", objects: func(b *builder) {
			b.obj(20, "21 0 R")
			b.obj(21, "20 0 R")
		}}.build(),
		"annotation ring": doc{page: "/Annots [20 0 R]", objects: func(b *builder) {
			b.obj(20, "<</Subtype /Text /Rect [0 0 1 1] /Popup 21 0 R /IRT 21 0 R>>")
			b.obj(21, "<</Subtype /Popup /Rect [0 0 1 1] /Parent 20 0 R /IRT 20 0 R>>")
		}}.build(),
		"name tree loop": doc{catalog: "/Names <</EmbeddedFiles 20 0 R>>", objects: func(b *builder) {
			b.obj(20, "<</Kids [20 0 R 20 0 R]>>")
		}}.build(),
		"info is the page tree": doc{trailer: "/Info 2 0 R"}.build(),
		"info is the catalog":   doc{trailer: "/Info 1 0 R"}.build(),
		"pages is an attachment": doc{objects: func(b *builder) {
			b.obj(2, "<</Type /Pages /Kids [3 0 R] /Count 1 /EF <<>>>>")
		}}.build(),
		"length chain":              chain(),
		"object streams each other": filterChain(),
		"many unterminated streams": append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("1 0 obj<<>>stream\n"), 5000)...),
		"many unterminated strings": append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("1 0 obj(\n"), 5000)...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) { coretest.FuzzTarget(t, testHandler, data) })
	}
}

func TestPrevLoopFallsBack(t *testing.T) {
	data := plainDoc()
	at := bytes.LastIndex(data, []byte("xref\n0"))
	data = bytes.Replace(data, []byte("trailer\n<<"), fmt.Appendf(nil, "trailer\n<</Prev %d ", at), 1)
	_, rep := clean(t, data, core.Options{})
	wantWarning(t, rep, "recovered by scanning")
}

func TestUnsupportedFilterInObjectStreamIsNotPassedThrough(t *testing.T) {
	data := bytes.Clone(xrefStreamDoc())
	i := bytes.Index(data, []byte("/Type /ObjStm"))
	j := i + bytes.Index(data[i:], []byte("/FlateDecode"))
	copy(data[j:], "/JBIG2Decode") // the same length, so no offset moves
	_, err := testHandler.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestFilters(t *testing.T) {
	hexData := []byte("48 65\n6C6C 6F>")
	cases := []struct {
		name string
		dict string
		raw  []byte
		want string
	}{
		{"none", "", []byte("Hello"), "Hello"},
		{"flate", "/Filter /FlateDecode", compress([]byte("Hello")), "Hello"},
		{"flate in an array with an abbreviation", "/Filter [/Fl]", compress([]byte("Hello")), "Hello"},
		{"hex", "/Filter /ASCIIHexDecode", hexData, "Hello"},
		{"ascii85", "/Filter /ASCII85Decode", []byte("87cUR~>"), "Hell"},
		{"ascii85 over flate", "/Filter [/ASCIIHexDecode /FlateDecode]", hexEncode(compress([]byte("Hello"))), "Hello"},
		{"png up", "/Filter /FlateDecode /DecodeParms <</Predictor 12 /Columns 2>>", compress([]byte{2, 'H', 'e', 2, 36, 7}), "Hel\x6c"},
		{"png sub", "/Filter /FlateDecode /DecodeParms [<</Predictor 11 /Columns 2>>]", compress([]byte{1, 'H', 29}), "He"},
		{"png average", "/Filter /FlateDecode /DecodeParms <</Predictor 13 /Columns 1>>", compress([]byte{0, 10, 3, 5}), "\x0a\x0a"},
		{"png paeth", "/Filter /FlateDecode /DecodeParms <</Predictor 14 /Columns 1>>", compress([]byte{0, 10, 4, 1}), "\x0a\x0b"},
		{"tiff", "/Filter /FlateDecode /DecodeParms <</Predictor 2 /Columns 3>>", compress([]byte{1, 1, 1}), "\x01\x02\x03"},
		{"truncated flate keeps what decoded", "/Filter /FlateDecode", compress([]byte("Hello Hello Hello"))[:10], ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var dict *pdfDict
			p := parser{data: []byte("<<" + c.dict + ">>"), budget: &valueBudget{limit: 100}}
			v, err := p.value(0)
			if err != nil {
				t.Fatal(err)
			}
			dict = v.(*pdfDict)
			d := newDocument(context.Background(), nil, defaultLimits)
			got, err := d.decodeStream(&pdfStream{dict: dict, raw: c.raw})
			if err != nil {
				if c.want == "" {
					return // no bytes survived the damage, which is also fine
				}
				t.Fatal(err)
			}
			if c.want != "" && string(got) != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	bad := []string{
		"/Filter /FlateDecode /DecodeParms <</Predictor 3 /Columns 1>>",
		"/Filter /FlateDecode /DecodeParms <</Predictor 12 /Columns 0>>",
		"/Filter /FlateDecode /DecodeParms <</Predictor 12 /Colors 99>>",
		"/Filter /FlateDecode /DecodeParms <</Predictor 2 /BitsPerComponent 4>>",
		"/Filter 5",
		"/Filter [5]",
	}
	for _, dictText := range bad {
		p := parser{data: []byte("<<" + dictText + ">>"), budget: &valueBudget{limit: 100}}
		v, err := p.value(0)
		if err != nil {
			t.Fatal(err)
		}
		d := newDocument(context.Background(), nil, defaultLimits)
		if _, err := d.decodeStream(&pdfStream{dict: v.(*pdfDict), raw: compress([]byte{99, 1, 2})}); err == nil {
			t.Errorf("%s: expected an error", dictText)
		}
	}
	// An invalid PNG tag.
	p := parser{data: []byte("<</Filter /FlateDecode /DecodeParms <</Predictor 12 /Columns 1>>>>"), budget: &valueBudget{limit: 100}}
	v, err := p.value(0)
	if err != nil {
		t.Fatal(err)
	}
	d := newDocument(context.Background(), nil, defaultLimits)
	if _, err := d.decodeStream(&pdfStream{dict: v.(*pdfDict), raw: compress([]byte{9, 1})}); err == nil {
		t.Error("expected an error for an unknown PNG tag")
	}
}

func hexEncode(b []byte) []byte {
	return fmt.Appendf(nil, "%X>", b)
}

// A signature that straddles two read chunks must still be seen.
func TestXMPScanAcrossChunks(t *testing.T) {
	for _, gap := range []int{0, 1, 100, 64<<10 - 40, 64<<10 - 5, 64 << 10, 128<<10 + 3} {
		padded := append(make([]byte, gap), []byte("<x:xmpmeta xmlns:x=\"adobe:ns:meta/\">")...)
		d := newDocument(context.Background(), nil, defaultLimits)
		dict := &pdfDict{}
		dict.set("Filter", pdfName("FlateDecode"))
		scan, err := d.scanStream(&pdfStream{dict: dict, raw: compress(padded)}, scanNeeds{pixels: -1})
		if err != nil || !scan.hasXMP {
			t.Errorf("gap %d: found = %v, err = %v", gap, scan.hasXMP, err)
		}
	}
}

func TestParserValues(t *testing.T) {
	parse := func(text string) (object, error) {
		p := parser{data: []byte(text), budget: &valueBudget{limit: 1000}}
		return p.value(0)
	}
	good := []struct {
		text string
		want string // through appendObject, so this also pins the writer's spelling
	}{
		{"12", "12"},
		{"+12", "12"},
		{"-12", "-12"},
		{"1.50", "1.50"},
		{"-.5", "-.5"},
		{"5.", "5."},
		{"99999999999999999999", "99999999999999999999"},
		{"true", "true"},
		{"false", "false"},
		{"null", "null"},
		{"/Name", "/Name"},
		{"/", "/"},
		{"/A#20B", "/A#20B"},
		{"/A#", "/A#23"},
		{"/A#G1", "/A#23G1"},
		{"(a(b)c)", `(a\(b\)c)`},
		{`(a\053\53\8)`, "(a++8)"},
		{`(\n\r\t\b\f\(\)\\)`, "(\\n\\r\t\b\f\\(\\)\\\\)"},
		{"(a\\\nb)", "(ab)"},
		{"(a\\\r\nb)", "(ab)"},
		{"(a\r\nb\rc)", `(a\nb\nc)`},
		{`(\777)`, "(\xFF)"},
		{"<48 65 6>", "<486560>"},
		{"<>", "<>"},
		{"[1 2 R 3]", "[1 2 R 3]"},
		{"[1 2 3 R]", "[1 2 3 R]"},
		{"[1 0 R]", "[1 0 R]"},
		{"[]", "[]"},
		{"<<>>", "<<>>"},
		{"<</B 1/A 2>>", "<</B 1 /A 2>>"},
		{"<</A 1/A 2>>", "<</A 2>>"},
		{"<</A%comment\n1>>", "<</A 1>>"},
		{"% comment\n7", "7"},
	}
	for _, c := range good {
		v, err := parse(c.text)
		if err != nil {
			t.Errorf("%q: %v", c.text, err)
			continue
		}
		if got := string(appendObject(nil, v)); got != c.want {
			t.Errorf("%q wrote as %q, want %q", c.text, got, c.want)
		}
		again, err := parse(c.want)
		if err != nil {
			t.Errorf("%q wrote %q, which does not read back: %v", c.text, c.want, err)
			continue
		}
		if got := string(appendObject(nil, again)); got != c.want {
			t.Errorf("%q is not stable: %q then %q", c.text, c.want, got)
		}
	}
	for _, text := range []string{
		"", ")", "]", ">", "{", "}", "(open", "<4", "<xy>", "[1", "<</A", "<</A>>", "<<1 2>>", "abc", "1e5",
		"--1", "1.2.3", "-", ".", "<<",
	} {
		if v, err := parse(text); err == nil {
			t.Errorf("%q parsed as %v, expected an error", text, v)
		} else if !errors.Is(err, core.ErrMalformed) {
			t.Errorf("%q: %v does not wrap ErrMalformed", text, err)
		}
	}
}

func TestLargeDictionary(t *testing.T) {
	var text strings.Builder
	text.WriteString("<<")
	const n = 20000
	for i := range n {
		fmt.Fprintf(&text, "/K%d %d ", i, i)
	}
	text.WriteString("/K5 -1 >>")
	p := parser{data: []byte(text.String()), budget: &valueBudget{limit: 1 << 20}}
	v, err := p.value(0)
	if err != nil {
		t.Fatal(err)
	}
	d := v.(*pdfDict)
	if len(d.entries) != n {
		t.Errorf("%d entries, want %d", len(d.entries), n)
	}
	if got := d.get("K5"); got != int64(-1) {
		t.Errorf("a repeated key must take the last value in place; K5 = %v", got)
	}
	if got := d.entries[5].key; got != "K5" {
		t.Errorf("order changed: entry 5 is %q", got)
	}
	if d.has("K99999") {
		t.Error("found a key that is not there")
	}
}

func TestFormat(t *testing.T) {
	f := testHandler.Format()
	if f.ID != "pdf" || f.Family != core.FamilyDocument || !slices.Equal(f.Extensions, []string{".pdf"}) || len(f.Limitations) == 0 {
		t.Errorf("Format = %+v", f)
	}
	if testHandler.Match([]byte("%PDF"), nil) || testHandler.Match([]byte("x%PDF-1.7"), nil) {
		t.Error("Match must require %PDF- at offset 0")
	}
	if !testHandler.Match([]byte("%PDF-1.7\n"), nil) {
		t.Error("Match must accept a PDF header")
	}
}

func TestIsRegistered(t *testing.T) {
	if !slices.ContainsFunc(core.Handlers(), func(h core.Handler) bool { return h.Format().ID == "pdf" }) {
		t.Error("the handler is not registered")
	}
}
