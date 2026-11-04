package pdf

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const (
	pageText = "Visible Test Page Text"
	pageBody = "BT /F1 12 Tf 72 720 Td (" + pageText + ") Tj ET"
)

// builder writes a PDF one object at a time and keeps the offsets, so a fixture
// reads like the file it produces.
type builder struct {
	buf     bytes.Buffer
	offsets map[int]int
	lastXR  int
}

func newBuilder(version string) *builder {
	b := &builder{offsets: make(map[int]int)}
	fmt.Fprintf(&b.buf, "%%PDF-%s\n%%\xE2\xE3\xCF\xD3\n", version)
	return b
}

func (b *builder) obj(num int, body string) {
	b.offsets[num] = b.buf.Len()
	fmt.Fprintf(&b.buf, "%d 0 obj\n%s\nendobj\n", num, body)
}

// stream writes a stream object. dict holds the keys other than /Length.
func (b *builder) stream(num int, dict string, data []byte) {
	b.rawStream(num, fmt.Sprintf("/Length %d %s", len(data), dict), data)
}

// rawStream writes a stream with its dictionary exactly as given.
func (b *builder) rawStream(num int, dict string, data []byte) {
	b.offsets[num] = b.buf.Len()
	fmt.Fprintf(&b.buf, "%d 0 obj\n<<%s>>\nstream\n", num, dict)
	b.buf.Write(data)
	b.buf.WriteString("\nendstream\nendobj\n")
}

// table writes a classic table for the objects written since the last one, then
// the trailer, and returns the table's offset. extra goes inside the trailer.
func (b *builder) table(extra string) int {
	nums := make([]int, 0, len(b.offsets))
	size := 1
	for num := range b.offsets {
		nums = append(nums, num)
		size = max(size, num+1)
	}
	slices.Sort(nums)
	at := b.buf.Len()
	b.buf.WriteString("xref\n")
	if b.lastXR == 0 {
		b.buf.WriteString("0 1\n0000000000 65535 f \n")
	}
	for _, num := range nums {
		fmt.Fprintf(&b.buf, "%d 1\n%010d 00000 n \n", num, b.offsets[num])
	}
	if b.lastXR != 0 {
		extra += fmt.Sprintf(" /Prev %d", b.lastXR)
	}
	fmt.Fprintf(&b.buf, "trailer\n<</Size %d %s>>\nstartxref\n%d\n%%%%EOF\n", size, extra, at)
	b.lastXR = at
	b.offsets = make(map[int]int)
	return at
}

func (b *builder) bytes() []byte { return b.buf.Bytes() }

func compress(data []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// doc describes the common shape of a fixture: a one-page document with hooks for
// what a test wants to add.
type doc struct {
	catalog  string
	page     string
	trailer  string
	contents func(b *builder) // writes object 4; the default is pageBody
	objects  func(b *builder)
}

func (f doc) build() []byte {
	b := newBuilder("1.7")
	f.write(b)
	b.table("/Root 1 0 R " + f.trailer)
	return b.bytes()
}

func (f doc) write(b *builder) {
	b.obj(1, fmt.Sprintf("<</Type /Catalog /Pages 2 0 R %s>>", f.catalog))
	b.obj(2, "<</Type /Pages /Kids [3 0 R] /Count 1>>")
	b.obj(3, fmt.Sprintf("<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "+
		"/Resources <</Font <</F1 5 0 R>>>> %s>>", f.page))
	if f.contents != nil {
		f.contents(b)
	} else {
		b.stream(4, "", []byte(pageBody))
	}
	b.obj(5, "<</Type /Font /Subtype /Type1 /BaseFont /Helvetica>>")
	if f.objects != nil {
		f.objects(b)
	}
}

const infoObject = 10

// infoBody is an Info dictionary with every standard key and a custom one.
const infoBody = `<</Author (Test Author) /Creator (Test Creator App) /Producer (Test Producer App)
/Title (Test Title) /Subject (Test Subject) /Keywords (Test Keywords)
/CreationDate (D:20010203040506+01'00') /ModDate (D:20010203040507Z)
/Trapped /False /CustomField (Test Custom Value)>>`

// plainDoc is a document with no metadata at all.
func plainDoc() []byte { return doc{}.build() }

func infoDoc() []byte {
	return doc{
		trailer: fmt.Sprintf("/Info %d 0 R /ID [<0123456789ABCDEF> <FEDCBA9876543210>]", infoObject),
		objects: func(b *builder) { b.obj(infoObject, infoBody) },
	}.build()
}

// kitchenSink carries one of nearly everything, as a seed for robustness and
// fuzz testing, so that mutations reach as many code paths as possible.
func kitchenSink() []byte {
	return doc{
		catalog: "/Metadata 20 0 R /Names <</EmbeddedFiles <</Names [(a.txt) 21 0 R]>>>> /AcroForm 25 0 R " +
			"/OpenAction <</S /JavaScript /JS (app.alert(1))>> /PieceInfo <</Test <</Private 1>>>> /Lang (en)",
		page: "/Thumb 23 0 R /Annots [24 0 R 26 0 R] /LastModified (D:20010203040506Z) " +
			"/Resources <</Font <</F1 5 0 R>> /XObject <</Im0 28 0 R /Im1 29 0 R>>>>",
		trailer: fmt.Sprintf("/Info %d 0 R /ID [<00> <01>]", infoObject),
		objects: func(b *builder) {
			b.obj(infoObject, infoBody)
			b.stream(20, "/Type /Metadata /Subtype /XML", coretest.FakeXMP())
			b.obj(21, "<</Type /Filespec /F (test-attachment.txt) /EF <</F 22 0 R>>>>")
			b.stream(22, "/Type /EmbeddedFile", []byte("Test Attachment Body"))
			b.stream(23, "/Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte{0xAA})
			b.obj(24, "<</Type /Annot /Subtype /Text /Rect [0 0 1 1] /T (Test Annotator) /M (D:20010203040506Z) "+
				"/NM (test-note-id) /Contents (Test Comment Body)>>")
			b.obj(25, "<</Fields [26 0 R 27 0 R]>>")
			b.obj(26, "<</FT /Tx /Subtype /Widget /Rect [0 0 1 1] /T (test_field) /V (Test Field Value)>>")
			b.obj(27, "<</FT /Sig /T (test_signature) /V <</Type /Sig /Filter /Adobe.PPKLite /Name (Test Signer) "+
				"/Location (Test Location) /Reason (Test Reason) /ContactInfo (test@example.com) "+
				"/M (D:20010203040506Z) /ByteRange [0 1 2 3] /Contents <00FF>>>>>")
			b.stream(28, "/Type /XObject /Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 "+
				"/ColorSpace /DeviceRGB /Filter /DCTDecode /Metadata 20 0 R", fakeJPEG(true))
			b.stream(29, "/Type /XObject /Subtype /Image /Width 1 /Height 1 /Filter /JPXDecode", []byte("jpx"))
		},
	}.build()
}

// fakeJPEG returns a minimal JPEG for the fake handler, with a metadata segment
// if asked. The real JPEG handler is another package, which production code must
// reach only through core.CleanBytes, so these tests register their own stand-in
// and check the contract between the two.
func fakeJPEG(withExif bool) []byte {
	out := []byte{0xFF, 0xD8}
	if withExif {
		payload := append([]byte("Exif\x00\x00"), []byte("Test Camera GPS 48.8584")...)
		out = append(out, 0xFF, 0xE1, byte((len(payload)+2)>>8), byte(len(payload)+2))
		out = append(out, payload...)
	}
	out = append(out, 0xFF, 0xDA, 0x00, 0x02)
	out = append(out, []byte("Test Pixel Data")...)
	return append(out, 0xFF, 0xD9)
}

// jpegStub is the stand-in JPEG handler. It removes APP1 segments, and it fails
// the test's expectations loudly by refusing anything but Strict, because the PDF
// handler promises to clean embedded JPEGs in strict mode.
type jpegStub struct{}

func init() { core.Register(jpegStub{}) }

func (jpegStub) Format() core.Format {
	return core.Format{ID: "jpeg-stub", Name: "Test JPEG", Family: core.FamilyImage, Extensions: []string{".jpg"}}
}

func (jpegStub) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte{0xFF, 0xD8})
}

func (s jpegStub) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	return s.Clean(ctx, src, io.Discard, opts)
}

func (jpegStub) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !opts.Strict {
		return nil, core.Unsupportedf("the stub JPEG handler must be called in strict mode")
	}
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(data, []byte{0xFF, 0xD9}) {
		return nil, core.Malformedf("no EOI")
	}
	rep := &core.Report{Format: "jpeg-stub"}
	rest := data[2:]
	for len(rest) >= 4 && rest[0] == 0xFF && rest[1] == 0xE1 {
		size := int(rest[2])<<8 | int(rest[3])
		if 2+size > len(rest)-2 { // the end-of-image marker is not part of a segment
			return nil, core.Malformedf("segment overruns the file")
		}
		rep.Add(core.Finding{
			Category: core.CatLocation, Risk: core.RiskHigh, Field: "GPS coordinates",
			Value: coretest.EiffelTower, Where: "APP1/Exif/GPS", Action: core.ActionRemove,
		})
		rest = rest[2+size:]
	}
	if _, err := dst.Write(slices.Concat(data[:2], rest)); err != nil {
		return nil, err
	}
	return rep, nil
}

func asBytes(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

// inner is an object destined for an object stream.
type inner struct {
	num  int
	body string
}

// objectStream writes an object stream holding objects and returns their rows for
// a cross-reference stream.
func (b *builder) objectStream(num int, objects []inner, rows map[int][3]int) {
	var header, body strings.Builder
	for i, o := range objects {
		fmt.Fprintf(&header, "%d %d ", o.num, body.Len())
		body.WriteString(o.body + "\n")
		rows[o.num] = [3]int{2, num, i}
	}
	data := compress([]byte(header.String() + body.String()))
	b.stream(num, fmt.Sprintf("/Type /ObjStm /N %d /First %d /Filter /FlateDecode", len(objects), header.Len()), data)
	rows[num] = [3]int{1, b.offsets[num], 0}
}

// xrefStream writes a cross-reference stream over rows, using a PNG predictor as
// real writers do, and returns its offset. It covers object numbers below size.
func (b *builder) xrefStream(num, size int, rows map[int][3]int, trailerKeys string) int {
	at := b.buf.Len()
	rows[num] = [3]int{1, at, 0}
	const columns = 5 // field widths 1, 3 and 1
	var plain []byte
	prev := make([]byte, columns)
	for n := range size {
		e := rows[n]
		row := []byte{byte(e[0]), byte(e[1] >> 16), byte(e[1] >> 8), byte(e[1]), byte(e[2])}
		plain = append(plain, 2) // PNG "up"
		for i := range row {
			plain = append(plain, row[i]-prev[i])
		}
		prev = row
	}
	b.stream(num, fmt.Sprintf("/Type /XRef /Size %d /W [1 3 1] /Filter /FlateDecode "+
		"/DecodeParms <</Predictor 12 /Columns %d>> %s", size, columns, trailerKeys), compress(plain))
	return at
}

// xrefStreamDoc is laid out the way modern writers do it: everything that can be
// is in an object stream, with a cross-reference stream at the end.
func xrefStreamDoc() []byte {
	b := newBuilder("1.5")
	rows := make(map[int][3]int)
	b.stream(4, "/Filter /FlateDecode", compress([]byte(pageBody)))
	rows[4] = [3]int{1, b.offsets[4], 0}
	b.stream(7, "/Type /Metadata /Subtype /XML", coretest.FakeXMP())
	rows[7] = [3]int{1, b.offsets[7], 0}
	b.objectStream(6, []inner{
		{1, "<</Type /Catalog /Pages 2 0 R /Metadata 7 0 R>>"},
		{2, "<</Type /Pages /Kids [3 0 R] /Count 1>>"},
		{3, "<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources <</Font <</F1 5 0 R>>>>>>"},
		{5, "<</Type /Font /Subtype /Type1 /BaseFont /Helvetica>>"},
		{infoObject, infoBody},
	}, rows)
	at := b.xrefStream(8, infoObject+1, rows, fmt.Sprintf("/Root 1 0 R /Info %d 0 R /ID [<AA> <BB>]", infoObject))
	fmt.Fprintf(&b.buf, "startxref\n%d\n%%%%EOF\n", at)
	return b.bytes()
}

// hybridDoc is a hybrid-reference file. Its classic table is complete enough for an
// old reader, and it hides the objects only newer readers should see by marking them
// free. Its cross-reference stream defines those. Here the hidden objects are the
// Info dictionary and an XMP-bearing structure tree, so missing them would be a leak.
func hybridDoc() []byte {
	b := newBuilder("1.5")
	doc{catalog: "/StructTreeRoot 7 0 R"}.write(b)
	classic := make(map[int]int)
	for num, off := range b.offsets {
		classic[num] = off
	}
	rows := make(map[int][3]int)
	b.stream(9, "/Type /Metadata /Subtype /XML", coretest.FakeXMP())
	classic[9] = b.offsets[9]
	b.objectStream(6, []inner{
		{7, "<</Type /StructTreeRoot /Metadata 9 0 R>>"},
		{infoObject, infoBody},
	}, rows)
	classic[6] = rows[6][1]
	const xrefNum, size = 8, infoObject + 1
	at := b.xrefStream(xrefNum, size, rows, "")
	classic[xrefNum] = at

	tableAt := b.buf.Len()
	fmt.Fprintf(&b.buf, "xref\n0 %d\n0000000000 65535 f \n", size)
	for num := 1; num < size; num++ {
		if off, ok := classic[num]; ok {
			fmt.Fprintf(&b.buf, "%010d 00000 n \n", off)
		} else {
			b.buf.WriteString("0000000000 00000 f \n")
		}
	}
	fmt.Fprintf(&b.buf, "trailer\n<</Size %d /Root 1 0 R /Info %d 0 R /XRefStm %d>>\nstartxref\n%d\n%%%%EOF\n",
		size, infoObject, at, tableAt)
	return b.bytes()
}
