package png

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	stdpng "image/png"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/exif"
)

const fakeICCTail = "Test ICC Tail"

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := stdpng.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func basePNG(t *testing.T) []byte {
	t.Helper()
	rng := rand.New(rand.NewPCG(3, 4))
	img := image.NewNRGBA(image.Rect(0, 0, 24, 16))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.UintN(256))
	}
	return encodePNG(t, img)
}

func palettePNG(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.NRGBA{255, 0, 0, 128}, color.NRGBA{0, 255, 0, 255}, color.NRGBA{0, 0, 255, 255}}
	img := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 3)
	}
	return encodePNG(t, img)
}

func rawChunk(typ string, payload []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	out = append(out, typ...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

func compress(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// insertAfterIHDR adds chunks at the place where most writers put metadata.
func insertAfterIHDR(base []byte, chunks ...[]byte) []byte {
	const ihdrEnd = len("\x89PNG\r\n\x1a\n") + 25
	out := bytes.Clone(base[:ihdrEnd])
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, base[ihdrEnd:]...)
}

func tEXt(keyword, value string) []byte {
	return rawChunk("tEXt", []byte(keyword+"\x00"+value))
}

func zTXt(t testing.TB, keyword, value string) []byte {
	return rawChunk("zTXt", append([]byte(keyword+"\x00\x00"), compress(t, []byte(value))...))
}

func iTXt(keyword, value string) []byte {
	return rawChunk("iTXt", []byte(keyword+"\x00\x00\x00en\x00\x00"+value))
}

func compressedITXt(t testing.TB, keyword, value string) []byte {
	return rawChunk("iTXt", append([]byte(keyword+"\x00\x01\x00en\x00\x00"), compress(t, []byte(value))...))
}

func fakeICC(size int) []byte {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(size))
	copy(p[36:], "acsp")
	copy(p[size-len(fakeICCTail):], fakeICCTail)
	return p
}

func iccpChunk(t testing.TB, name string, profile []byte) []byte {
	return rawChunk("iCCP", append([]byte(name+"\x00\x00"), compress(t, profile)...))
}

func tIME() []byte {
	return rawChunk("tIME", []byte{0x07, 0xD1, 2, 3, 4, 5, 6})
}

func chunkTypes(t *testing.T, data []byte) []string {
	t.Helper()
	var types []string
	pos := len(signature)
	for pos < len(data) {
		length := int(binary.BigEndian.Uint32(data[pos:]))
		types = append(types, string(data[pos+4:pos+8]))
		pos += 12 + length
	}
	if pos != len(data) {
		t.Fatalf("chunk walk ended at %d of %d", pos, len(data))
	}
	return types
}

func clean(t *testing.T, data []byte, opts core.Options) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, opts)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func assertSamePixels(t *testing.T, before, after []byte) {
	t.Helper()
	a, err := stdpng.Decode(bytes.NewReader(before))
	if err != nil {
		t.Fatalf("decode input: %v", err)
	}
	b, err := stdpng.Decode(bytes.NewReader(after))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds changed: %v became %v", a.Bounds(), b.Bounds())
	}
	bounds := a.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				t.Fatalf("pixel (%d,%d) changed from %v to %v", x, y, a.At(x, y), b.At(x, y))
			}
		}
	}
}

func hasField(rep *core.Report, field string, action core.Action) bool {
	return slices.ContainsFunc(rep.Findings, func(f core.Finding) bool {
		return f.Field == field && f.Action == action
	})
}

// interlacedPNG hand-builds an Adam7 image, since image/png cannot write one. Each
// pixel's value is derived from its position, so a decode verifies the layout.
func interlacedPNG(t testing.TB, size int) []byte {
	t.Helper()
	passes := []struct{ x0, y0, dx, dy int }{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}
	var raw []byte
	for _, p := range passes {
		for y := p.y0; y < size; y += p.dy {
			if p.x0 >= size {
				break
			}
			raw = append(raw, 0) // filter type None
			for x := p.x0; x < size; x += p.dx {
				raw = append(raw, byte(x*16+y))
			}
		}
	}
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(size))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(size))
	ihdr = append(ihdr, 8, colorGray, 0, 0, 1)
	return bytes.Join([][]byte{signature, rawChunk("IHDR", ihdr), rawChunk("IDAT", compress(t, raw)), rawChunk("IEND", nil)}, nil)
}

// apngChunks returns the animation control chunks of a two-frame APNG, whose
// second frame travels in fdAT.
func apngChunks(t testing.TB) (actl, fctl0, fctl1, fdat []byte) {
	t.Helper()
	frame := func(seq uint32) []byte {
		out := binary.BigEndian.AppendUint32(nil, seq)
		out = binary.BigEndian.AppendUint32(out, 24)
		out = binary.BigEndian.AppendUint32(out, 16)
		out = binary.BigEndian.AppendUint32(out, 0)
		out = binary.BigEndian.AppendUint32(out, 0)
		return append(out, 0, 1, 0, 10, 0, 0)
	}
	actl = rawChunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0})
	fctl0 = rawChunk("fcTL", frame(0))
	fctl1 = rawChunk("fcTL", frame(1))
	frameData := binary.BigEndian.AppendUint32(nil, 2)
	frameData = append(frameData, compress(t, bytes.Repeat([]byte{0, 1, 2, 3, 4}, 200))...)
	fdat = rawChunk("fdAT", frameData)
	return actl, fctl0, fctl1, fdat
}

func apng(t testing.TB, base []byte) []byte {
	t.Helper()
	actl, fctl0, fctl1, fdat := apngChunks(t)
	end := len(base) - 12
	out := insertAfterIHDR(base[:end], actl, fctl0)
	out = append(out, fctl1...)
	out = append(out, fdat...)
	return append(out, base[end:]...)
}

func rawProfile(name string, block []byte) string {
	return "\n" + name + "\n    " + "100\n" + hex.EncodeToString(block) + "\n"
}

func TestContract(t *testing.T) {
	base := basePNG(t)
	profile := fakeICC(3000)
	cases := []coretest.Case{
		{Name: "no metadata", Data: base},
		{
			Name: "text chunks",
			Data: insertAfterIHDR(base,
				tEXt("Title", "Test Title"),
				tEXt("Author", "Test Author"),
				tEXt("Description", "Test Description"),
				tEXt("Copyright", "Test Copyright"),
				tEXt("Creation Time", "2001-02-03"),
				tEXt("Software", "Test Software"),
				tEXt("Source", "Test Source"),
				tEXt("Comment", "Test Comment"),
				tEXt("Custom Key", "Test Custom"),
				zTXt(t, "Disclaimer", "Test Disclaimer"),
				iTXt("Warning", "Test Warning"),
				compressedITXt(t, "Location", "Test Location"),
			),
			WantFields: []string{
				"Title", "Author", "Description", "Copyright", "Creation time", "Software",
				"Source device", "Comment", "Text metadata", "Disclaimer", "Warning", "Location",
			},
			MustNotContain: [][]byte{
				[]byte("Test Title"), []byte("Test Author"), []byte("Test Description"), []byte("Test Copyright"),
				[]byte("2001-02-03"), []byte("Test Software"), []byte("Test Source"), []byte("Test Comment"),
				[]byte("Custom Key"), []byte("Test Custom"), []byte("Disclaimer"), []byte("Test Warning"), []byte("Location"),
			},
		},
		{
			Name:           "XMP in iTXt",
			Data:           insertAfterIHDR(base, iTXt(xmpKeyword, string(coretest.FakeXMP()))),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: append(coretest.FakeXMPSecrets(), []byte(xmpKeyword)),
		},
		{
			Name:           "compressed XMP in iTXt",
			Data:           insertAfterIHDR(base, compressedITXt(t, xmpKeyword, string(coretest.FakeXMP()))),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: [][]byte{[]byte(xmpKeyword)},
		},
		{
			Name:           "XMP under an unexpected keyword",
			Data:           insertAfterIHDR(base, tEXt("Whatever", string(coretest.FakeXMP()))),
			WantFields:     []string{"Creator", "GPS coordinates"},
			MustNotContain: coretest.FakeXMPSecrets(),
		},
		{
			Name:           "EXIF is dropped",
			Data:           insertAfterIHDR(base, rawChunk("eXIf", coretest.FakeEXIF(0))),
			WantFields:     coretest.FakeEXIFFields(),
			MustNotContain: append(coretest.FakeEXIFSecrets(), []byte("eXIf")),
		},
		{
			Name:           "EXIF with a rotated orientation is kept minimal",
			Data:           insertAfterIHDR(base, rawChunk("eXIf", coretest.FakeEXIF(6))),
			WantFields:     coretest.FakeEXIFFields(),
			WantRetained:   []string{"Orientation"},
			MustNotContain: coretest.FakeEXIFSecrets(),
			MustContain:    [][]byte{rawChunk("eXIf", exif.Minimal(6))},
		},
		{
			Name:           "tIME",
			Data:           insertAfterIHDR(base, tIME()),
			WantFields:     []string{"Modification time"},
			MustNotContain: [][]byte{[]byte("tIME")},
		},
		{
			Name:           "C2PA and unknown ancillary chunks",
			Data:           insertAfterIHDR(base, rawChunk("caBX", []byte("Test C2PA")), rawChunk("teSt", []byte("Test Unknown"))),
			WantFields:     []string{"C2PA content credentials", "Other chunk"},
			MustNotContain: [][]byte{[]byte("Test C2PA"), []byte("Test Unknown"), []byte("caBX"), []byte("teSt")},
		},
		{
			Name:           "ICC profile has its name replaced",
			Data:           insertAfterIHDR(base, iccpChunk(t, "Test Author Laptop", profile)),
			WantFields:     []string{"ICC profile name"},
			WantRetained:   []string{"ICC colour profile"},
			MustNotContain: [][]byte{[]byte("Test Author Laptop")},
			MustContain:    [][]byte{[]byte(neutralICCName + "\x00\x00")},
		},
		{
			Name:         "ICC profile with the neutral name",
			Data:         insertAfterIHDR(base, iccpChunk(t, neutralICCName, profile)),
			WantRetained: []string{"ICC colour profile"},
			MustContain:  [][]byte{iccpChunk(t, neutralICCName, profile)},
		},
		{
			Name:           "trailing data",
			Data:           append(bytes.Clone(base), "Test Trailing Data"...),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Data")},
		},
		{
			Name:        "allowlisted ancillary chunks survive",
			Data:        insertAfterIHDR(base, allowlisted(t)...),
			MustContain: allowlisted(t),
		},
		{
			Name:           "APNG frames survive",
			Data:           apng(t, insertAfterIHDR(base, tEXt("Author", "Test Author"))),
			WantFields:     []string{"Author"},
			MustNotContain: [][]byte{[]byte("Test Author")},
		},
		{
			Name:           "metadata after the image data",
			Data:           append(bytes.Clone(base[:len(base)-12]), slices.Concat(tEXt("Author", "Test Author"), rawChunk("eXIf", coretest.FakeEXIF(8)), base[len(base)-12:])...),
			WantFields:     []string{"Author", "GPS coordinates"},
			WantRetained:   []string{"Orientation"},
			MustNotContain: append(coretest.FakeEXIFSecrets(), []byte("Test Author")),
		},
		{
			Name:           "ImageMagick raw EXIF profile",
			Data:           insertAfterIHDR(base, zTXt(t, "Raw profile type exif", rawProfile("exif", append([]byte(exif.Prefix), coretest.FakeEXIF(0)...)))),
			WantFields:     []string{"GPS coordinates", "Artist"},
			MustNotContain: [][]byte{[]byte("Raw profile")},
		},
		{
			Name:           "ImageMagick raw XMP profile",
			Data:           insertAfterIHDR(base, tEXt("Raw profile type xmp", rawProfile("xmp", coretest.FakeXMP()))),
			WantFields:     []string{"Creator", "GPS coordinates"},
			MustNotContain: [][]byte{[]byte("Raw profile")},
		},
		{
			Name:           "ImageMagick raw profile of another kind",
			Data:           insertAfterIHDR(base, tEXt("Raw profile type iptc", rawProfile("iptc", []byte("Test IPTC Data")))),
			WantFields:     []string{"Raw metadata profile"},
			MustNotContain: [][]byte{[]byte("Raw profile")},
		},
		{
			Name:           "ImageMagick raw profile that does not decode",
			Data:           insertAfterIHDR(base, tEXt("Raw profile type exif", "Test Not A Profile")),
			WantFields:     []string{"Raw metadata profile"},
			MustNotContain: [][]byte{[]byte("Raw profile")},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

// allowlisted returns a valid example of each allowlisted ancillary chunk that
// suits an RGBA image.
func allowlisted(t *testing.T) [][]byte {
	t.Helper()
	return [][]byte{
		rawChunk("gAMA", []byte{0, 0, 0xB1, 0x8F}),
		rawChunk("cHRM", make([]byte, 32)),
		rawChunk("sRGB", []byte{0}),
		rawChunk("sBIT", []byte{8, 8, 8, 8}),
		rawChunk("bKGD", []byte{0, 1, 0, 2, 0, 3}),
		rawChunk("pHYs", []byte{0, 0, 0x0B, 0x13, 0, 0, 0x0B, 0x13, 1}),
		rawChunk("cICP", []byte{1, 13, 0, 1}),
		rawChunk("mDCV", make([]byte, 24)),
		rawChunk("cLLI", make([]byte, 8)),
	}
}

func TestPixelsSurvive(t *testing.T) {
	base := basePNG(t)
	withMeta := func(data []byte) []byte {
		return insertAfterIHDR(data, tEXt("Author", "Test Author"), rawChunk("eXIf", coretest.FakeEXIF(6)), iccpChunk(t, "Test", fakeICC(1000)), tIME())
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"RGBA", withMeta(base)},
		{"palette with transparency", withMeta(palettePNG(t))},
		{"interlaced", withMeta(interlacedPNG(t, 19))},
		{"APNG", apng(t, withMeta(base))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, opts := range []core.Options{{}, {Strict: true}} {
				out, _ := clean(t, tc.data, opts)
				assertSamePixels(t, tc.data, out)
			}
		})
	}
}

func TestInterlacedFixtureIsReallyInterlaced(t *testing.T) {
	data := interlacedPNG(t, 19)
	if data[len(signature)+8+12] != 1 {
		t.Fatal("IHDR does not declare Adam7 interlacing")
	}
	img, err := stdpng.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("decoded to %T", img)
	}
	for y := range 19 {
		for x := range 19 {
			if want := byte(x*16 + y); gray.GrayAt(x, y).Y != want {
				t.Fatalf("pixel (%d,%d) is %d, want %d", x, y, gray.GrayAt(x, y).Y, want)
			}
		}
	}
}

func TestAPNGFramesSurvive(t *testing.T) {
	base := basePNG(t)
	data := apng(t, insertAfterIHDR(base, tEXt("Author", "Test Author"), tIME()))
	out, _ := clean(t, data, core.Options{})
	want := apng(t, base)
	if !bytes.Equal(out, want) {
		t.Fatalf("the output is not the same APNG without its metadata: %d bytes, want %d", len(out), len(want))
	}
	if got := chunkTypes(t, out); !slices.Equal(got, []string{"IHDR", "acTL", "fcTL", "IDAT", "fcTL", "fdAT", "IEND"}) {
		t.Fatalf("chunks are %q", got)
	}
}

func TestMinimalEXIFGoesBeforeImageData(t *testing.T) {
	base := basePNG(t)
	data := append(bytes.Clone(base[:len(base)-12]), slices.Concat(rawChunk("eXIf", coretest.FakeEXIF(3)), base[len(base)-12:])...)
	out, _ := clean(t, data, core.Options{})
	types := chunkTypes(t, out)
	if !slices.Equal(types, []string{"IHDR", "eXIf", "IDAT", "IEND"}) {
		t.Fatalf("chunks are %q", types)
	}
	// The go-to decoder for orientation must be able to read what was written.
	d, err := exif.Parse(out[bytes.Index(out, []byte("eXIf"))+4:][:len(exif.Minimal(3))])
	if err != nil || d.Orientation() != 3 {
		t.Fatalf("orientation %d, err %v", d.Orientation(), err)
	}
}

func TestStrictRemovesRetainedData(t *testing.T) {
	base := basePNG(t)
	data := insertAfterIHDR(base, iccpChunk(t, "Test Author Laptop", fakeICC(1000)), rawChunk("eXIf", coretest.FakeEXIF(6)))
	out, rep := clean(t, data, core.Options{Strict: true})
	if !bytes.Equal(out, base) {
		t.Fatalf("strict output differs from the bare image: %d bytes, want %d", len(out), len(base))
	}
	if len(rep.Residual()) != len(rep.Findings) {
		t.Fatalf("strict mode retained something: %+v", rep.Findings)
	}
	for _, field := range []string{"ICC colour profile", "ICC profile name", "Orientation"} {
		if !hasField(rep, field, core.ActionRemove) {
			t.Errorf("field %q not reported as removed", field)
		}
	}
}

func TestOnlyFirstEXIFDecidesOrientation(t *testing.T) {
	base := basePNG(t)
	data := insertAfterIHDR(base, rawChunk("eXIf", coretest.FakeEXIF(0)), rawChunk("eXIf", coretest.FakeEXIF(6)))
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("a second eXIf changed the output")
	}
	if hasField(rep, "Orientation", core.ActionRetain) {
		t.Fatalf("the second block's orientation was described as kept: %+v", rep.Findings)
	}
}

func TestSecondICCIsDropped(t *testing.T) {
	base := basePNG(t)
	first := iccpChunk(t, neutralICCName, fakeICC(1000))
	data := insertAfterIHDR(base, first, iccpChunk(t, neutralICCName, fakeICC(1200)))
	out, rep := clean(t, data, core.Options{})
	if want := insertAfterIHDR(base, first); !bytes.Equal(out, want) {
		t.Fatal("only the first profile should survive")
	}
	if !hasField(rep, "Extra ICC profile", core.ActionRemove) {
		t.Fatalf("no finding for the extra profile: %+v", rep.Findings)
	}
}

func TestDamagedICCIsRemoved(t *testing.T) {
	base := basePNG(t)
	badSize := fakeICC(1000)
	badSize[3]++
	cases := map[string][]byte{
		"not a profile":  iccpChunk(t, "x", []byte("Test Not A Profile")),
		"wrong size":     iccpChunk(t, "x", badSize),
		"not zlib":       rawChunk("iCCP", []byte("x\x00\x00Test Not Zlib")),
		"no name":        iccpChunk(t, "", fakeICC(1000)),
		"name too long":  iccpChunk(t, strings.Repeat("x", 80), fakeICC(1000)),
		"no terminator":  rawChunk("iCCP", []byte("Test")),
		"unknown method": rawChunk("iCCP", append([]byte("x\x00\x01"), compress(t, fakeICC(1000))...)),
		"no method byte": rawChunk("iCCP", []byte("x\x00")),
		"trailing bytes": rawChunk("iCCP", append(append([]byte("x\x00\x00"), compress(t, fakeICC(1000))...), "Test Hidden"...)),
		"truncated zlib": rawChunk("iCCP", append([]byte("x\x00\x00"), compress(t, fakeICC(1000))[:20]...)),
	}
	for name, chunk := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, insertAfterIHDR(base, chunk), core.Options{})
			if !bytes.Equal(out, base) {
				t.Fatal("a damaged profile must be dropped")
			}
			if !hasField(rep, "Damaged ICC profile", core.ActionRemove) {
				t.Fatalf("no damage finding: %+v", rep.Findings)
			}
		})
	}
}

func TestDamagedTextIsReportedAndRemoved(t *testing.T) {
	base := basePNG(t)
	cases := map[string][]byte{
		"no keyword terminator": rawChunk("tEXt", []byte("Test Keyword Only")),
		"zTXt unknown method":   rawChunk("zTXt", []byte("Test\x00\x01data")),
		"zTXt no method":        rawChunk("zTXt", []byte("Test\x00")),
		"zTXt not zlib":         rawChunk("zTXt", []byte("Test\x00\x00not zlib")),
		"iTXt too short":        rawChunk("iTXt", []byte("Test\x00\x00")),
		"iTXt no language end":  rawChunk("iTXt", []byte("Test\x00\x00\x00en")),
		"iTXt no translation":   rawChunk("iTXt", []byte("Test\x00\x00\x00en\x00no end")),
		"iTXt unknown method":   rawChunk("iTXt", []byte("Test\x00\x01\x09en\x00\x00data")),
		"iTXt not zlib":         rawChunk("iTXt", []byte("Test\x00\x01\x00en\x00\x00not zlib")),
	}
	for name, chunk := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, insertAfterIHDR(base, chunk), core.Options{})
			if !bytes.Equal(out, base) {
				t.Fatal("the chunk must be dropped whatever it holds")
			}
			if len(rep.Findings) != 1 {
				t.Fatalf("got findings %+v, want one", rep.Findings)
			}
		})
	}
}

func TestLatin1AndUTF8TextAreDecoded(t *testing.T) {
	base := basePNG(t)
	_, rep := clean(t, insertAfterIHDR(base, tEXt("Title", "Caf\xE9"), iTXt("Author", "Caf\xC3\xA9")), core.Options{})
	if got := rep.Findings[0].Value; got != "Café" {
		t.Errorf("tEXt decoded to %q", got)
	}
	if got := rep.Findings[1].Value; got != "Café" {
		t.Errorf("iTXt decoded to %q", got)
	}
}

func TestDecodedValuesAreSpecific(t *testing.T) {
	base := basePNG(t)
	_, rep := clean(t, insertAfterIHDR(base, tIME(), rawChunk("eXIf", coretest.FakeEXIF(0))), core.Options{})
	if got := rep.Findings[0].Value; got != "2001-02-03 04:05:06 UTC" {
		t.Errorf("time decoded to %q", got)
	}
	found := slices.ContainsFunc(rep.Findings, func(f core.Finding) bool {
		return f.Field == "GPS coordinates" && f.Value == coretest.EiffelTower
	})
	if !found {
		t.Errorf("GPS was not decoded: %+v", rep.Findings)
	}
}

func TestBombsAreRefused(t *testing.T) {
	base := basePNG(t)
	bomb := compress(t, make([]byte, maxInflatedChunk+1))
	// Five chunks, each under the per-chunk cap, go over the total cap together.
	medium := rawChunk("zTXt", append([]byte("Medium\x00\x00"), compress(t, make([]byte, maxInflatedChunk-1))...))
	cases := map[string][]byte{
		"zTXt": insertAfterIHDR(base, rawChunk("zTXt", append([]byte("Bomb\x00\x00"), bomb...))),
		"iTXt": insertAfterIHDR(base, rawChunk("iTXt", append([]byte("Bomb\x00\x01\x00en\x00\x00"), bomb...))),
		"iCCP": insertAfterIHDR(base, rawChunk("iCCP", append([]byte("x\x00\x00"), bomb...))),
		"sum":  insertAfterIHDR(base, medium, medium, medium, medium, medium),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, cleanErr := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
			_, inspectErr := handler{}.Inspect(context.Background(), core.BytesSource(data), core.Options{})
			if !errors.Is(cleanErr, core.ErrTooLarge) || !errors.Is(inspectErr, core.ErrTooLarge) {
				t.Fatalf("Clean returned %v and Inspect returned %v, want ErrTooLarge", cleanErr, inspectErr)
			}
		})
	}
}

func TestFindingsAreCapped(t *testing.T) {
	base := basePNG(t)
	chunk := tEXt("k", "v")
	data := append(bytes.Clone(base[:33]), bytes.Repeat(chunk, maxFindings+50)...)
	data = append(data, base[33:]...)
	_, rep := clean(t, data, core.Options{})
	if len(rep.Findings) != maxFindings+1 {
		t.Fatalf("got %d findings, want %d", len(rep.Findings), maxFindings+1)
	}
	if last := rep.Findings[len(rep.Findings)-1]; last.Field != "Further metadata" || last.Value != "50 more items" {
		t.Fatalf("the overflow finding is %+v", last)
	}
}

func TestTooManyChunks(t *testing.T) {
	base := basePNG(t)
	data := append(bytes.Clone(base[:33]), bytes.Repeat(rawChunk("gAMA", make([]byte, 4)), maxChunks+1)...)
	data = append(data, base[33:]...)
	var out bytes.Buffer
	_, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// tinyPNG builds a 1x1 image of the given colour type holding the given chunks
// before an empty IDAT. Its pixels are not decodable, which is all the same to
// tests of the chunk structure.
func tinyPNG(colorType byte, chunks ...[]byte) []byte {
	p := binary.BigEndian.AppendUint32(nil, 1)
	p = binary.BigEndian.AppendUint32(p, 1)
	p = append(p, 8, colorType, 0, 0, 0)
	out := slices.Concat(signature, rawChunk("IHDR", p))
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(append(out, rawChunk("IDAT", nil)...), rawChunk("IEND", nil)...)
}

func TestUnparseableIsRefused(t *testing.T) {
	base := basePNG(t)
	corrupted := bytes.Clone(base)
	corrupted[len(corrupted)-5]++ // inside IEND's CRC
	idatCorrupted := bytes.Clone(base)
	idatCorrupted[50] ^= 0xFF
	meta := bytes.Clone(insertAfterIHDR(base, tEXt("Author", "Test Author")))
	meta[50] ^= 0xFF
	ihdr := func(width, height uint32, depth, colorType, compression, filter, interlace byte) []byte {
		p := binary.BigEndian.AppendUint32(nil, width)
		p = binary.BigEndian.AppendUint32(p, height)
		p = append(p, depth, colorType, compression, filter, interlace)
		return slices.Concat(signature, rawChunk("IHDR", p), rawChunk("IDAT", nil), rawChunk("IEND", nil))
	}
	palette := rawChunk("PLTE", []byte{1, 2, 3})
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, core.ErrUnsupported},
		{"not PNG", []byte("not a png"), core.ErrUnsupported},
		{"bad CRC in IEND", corrupted, core.ErrMalformed},
		{"bad CRC in IDAT", idatCorrupted, core.ErrMalformed},
		{"bad CRC in a metadata chunk", meta, core.ErrMalformed},
		{"no IEND", base[:len(base)-12], core.ErrMalformed},
		{"signature alone", signature, core.ErrMalformed},
		{"chunk past the end", append(bytes.Clone(signature), 0, 0, 1, 0, 'I', 'H', 'D', 'R', 0, 0, 0, 0), core.ErrMalformed},
		{"length over the limit", append(bytes.Clone(signature), 0xFF, 0xFF, 0xFF, 0xFF, 'I', 'H', 'D', 'R', 0, 0, 0, 0), core.ErrMalformed},
		{"invalid chunk type", slices.Concat(signature, rawChunk("I1DR", make([]byte, 13))), core.ErrMalformed},
		{"first chunk not IHDR", slices.Concat(signature, rawChunk("gAMA", make([]byte, 4)), base[8:]), core.ErrMalformed},
		{"Apple CgBI", slices.Concat(signature, rawChunk("CgBI", make([]byte, 4)), base[8:]), core.ErrUnsupported},
		{"second IHDR", insertAfterIHDR(base, base[8:33]), core.ErrMalformed},
		{"unknown critical chunk", insertAfterIHDR(base, rawChunk("ABCD", nil)), core.ErrUnsupported},
		{"no IDAT", slices.Concat(base[:33], rawChunk("IEND", nil)), core.ErrMalformed},
		{"IEND with a payload", slices.Concat(base[:len(base)-12], rawChunk("IEND", []byte("Test"))), core.ErrMalformed},
		{"short IHDR", slices.Concat(signature, rawChunk("IHDR", make([]byte, 12))), core.ErrMalformed},
		{"zero width", ihdr(0, 1, 8, 6, 0, 0, 0), core.ErrMalformed},
		{"zero height", ihdr(1, 0, 8, 6, 0, 0, 0), core.ErrMalformed},
		{"huge width", ihdr(1<<31, 1, 8, 6, 0, 0, 0), core.ErrMalformed},
		{"bad depth", ihdr(1, 1, 3, 0, 0, 0, 0), core.ErrMalformed},
		{"bad palette depth", ihdr(1, 1, 16, 3, 0, 0, 0), core.ErrMalformed},
		{"bad RGB depth", ihdr(1, 1, 4, 2, 0, 0, 0), core.ErrMalformed},
		{"bad colour type", ihdr(1, 1, 8, 5, 0, 0, 0), core.ErrMalformed},
		{"bad compression", ihdr(1, 1, 8, 6, 1, 0, 0), core.ErrMalformed},
		{"bad filter", ihdr(1, 1, 8, 6, 0, 1, 0), core.ErrMalformed},
		{"bad interlace", ihdr(1, 1, 8, 6, 0, 0, 2), core.ErrMalformed},
		{"palette image without PLTE", tinyPNG(colorPalette), core.ErrMalformed},
		{"PLTE on a gray image", tinyPNG(colorGray, palette), core.ErrMalformed},
		{"PLTE on gray-alpha", tinyPNG(colorGrayAlpha, palette), core.ErrMalformed},
		{"PLTE length not a multiple of 3", tinyPNG(colorPalette, rawChunk("PLTE", []byte{1, 2})), core.ErrMalformed},
		{"PLTE too long", tinyPNG(colorPalette, rawChunk("PLTE", make([]byte, 769))), core.ErrMalformed},
		{"empty PLTE", tinyPNG(colorPalette, rawChunk("PLTE", nil)), core.ErrMalformed},
		{"second PLTE", tinyPNG(colorPalette, palette, palette), core.ErrMalformed},
		{"PLTE after IDAT", slices.Concat(tinyPNG(colorPalette)[:33], rawChunk("IDAT", nil), palette, rawChunk("IEND", nil)), core.ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_, cleanErr := handler{}.Clean(context.Background(), core.BytesSource(tc.data), &out, core.Options{})
			_, inspectErr := handler{}.Inspect(context.Background(), core.BytesSource(tc.data), core.Options{})
			if !errors.Is(cleanErr, tc.want) || !errors.Is(inspectErr, tc.want) {
				t.Fatalf("Clean returned %v and Inspect returned %v, want %v", cleanErr, inspectErr, tc.want)
			}
		})
	}
}

func TestValidColourVariantsAreAccepted(t *testing.T) {
	palette := rawChunk("PLTE", []byte{1, 2, 3})
	cases := map[string][]byte{
		"gray":       tinyPNG(colorGray, rawChunk("tRNS", []byte{0, 1}), rawChunk("sBIT", []byte{8}), rawChunk("bKGD", []byte{0, 1})),
		"gray alpha": tinyPNG(colorGrayAlpha, rawChunk("sBIT", []byte{8, 8}), rawChunk("bKGD", []byte{0, 1})),
		"RGB":        tinyPNG(colorRGB, rawChunk("tRNS", make([]byte, 6)), rawChunk("sBIT", []byte{8, 8, 8}), rawChunk("bKGD", make([]byte, 6))),
		"palette":    tinyPNG(colorPalette, palette, rawChunk("tRNS", []byte{1}), rawChunk("sBIT", []byte{8, 8, 8}), rawChunk("bKGD", []byte{0})),
		"RGBA":       tinyPNG(colorRGBA, rawChunk("sBIT", []byte{8, 8, 8, 8}), rawChunk("bKGD", make([]byte, 6))),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if out, _ := clean(t, data, core.Options{}); !bytes.Equal(out, data) {
				t.Fatal("a file with no metadata must pass through unchanged")
			}
		})
	}
}

func TestHugeTextIsNotDecodedWhole(t *testing.T) {
	if got := len([]rune(decodeLatin1(bytes.Repeat([]byte{0xE9}, 10*maxTextBytes)))); got != maxTextBytes {
		t.Fatalf("decoded %d characters, want %d", got, maxTextBytes)
	}
}

func TestDecodeRawProfile(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []byte
	}{
		{"usual form", "\nexif\n      4\n01020304\n", []byte{1, 2, 3, 4}},
		{"split across lines", "\nexif\n 4\n0102\n0304\n", []byte{1, 2, 3, 4}},
		{"nothing", "", nil},
		{"name only", "\nexif", nil},
		{"no data", "\nexif\n 4\n", nil},
		{"not hex", "\nexif\n 4\nZZZZ\n", nil},
		{"odd length", "\nexif\n 4\n012\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeRawProfile([]byte(tc.value)); !bytes.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if decodeRawProfile(make([]byte, maxInflatedChunk+1)) != nil {
		t.Fatal("a value over the cap must not be decoded")
	}
}

func TestMalformedTimeIsReported(t *testing.T) {
	base := basePNG(t)
	_, rep := clean(t, insertAfterIHDR(base, rawChunk("tIME", []byte{1, 2})), core.Options{})
	if !strings.Contains(rep.Findings[0].Value, "not a valid time") {
		t.Fatalf("got %+v", rep.Findings[0])
	}
}

func TestRegisteredAndMatching(t *testing.T) {
	h := handler{}
	if h.Match([]byte("\x89PNG"), nil) {
		t.Error("part of the signature must not be enough")
	}
	if !h.Match(basePNG(t), nil) {
		t.Error("did not match a PNG")
	}
	if !slices.ContainsFunc(core.Handlers(), func(r core.Handler) bool { return r.Format().ID == formatID }) {
		t.Error("the handler did not register itself")
	}
	if h.Format().Name == "" || len(h.Format().Extensions) == 0 {
		t.Error("incomplete format description")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteErrorIsReturned(t *testing.T) {
	_, err := handler{}.Clean(context.Background(), core.BytesSource(basePNG(t)), failingWriter{}, core.Options{})
	if err == nil {
		t.Fatal("a failing writer must fail Clean")
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := handler{}.Inspect(ctx, core.BytesSource(basePNG(t)), core.Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// fullSample packs every kind of chunk the handler treats specially into one file.
func fullSample(t testing.TB) []byte {
	return slices.Concat(interlacedPrefix(t),
		iccpChunk(t, "Test", fakeICC(200)),
		tEXt("Author", "Test Author"),
		zTXt(t, "Description", "Test Description"),
		iTXt(xmpKeyword, string(coretest.FakeXMP())),
		compressedITXt(t, "Title", "Test Title"),
		tEXt("Raw profile type exif", rawProfile("exif", coretest.FakeEXIF(6))),
		rawChunk("eXIf", coretest.FakeEXIF(6)),
		tIME(),
		rawChunk("caBX", []byte("Test C2PA")),
		rawChunk("teSt", []byte("Test Unknown")),
		rawChunk("gAMA", []byte{0, 0, 0xB1, 0x8F}),
		rawChunk("pHYs", make([]byte, 9)),
		interlacedSuffix(t),
		[]byte("Test Trailing"),
	)
}

func interlacedPrefix(t testing.TB) []byte {
	return interlacedPNG(t, 19)[:33]
}

func interlacedSuffix(t testing.TB) []byte {
	return interlacedPNG(t, 19)[33:]
}

// fixCRCs rewrites every chunk's CRC. A random mutation nearly always breaks a
// CRC and stops at the first check, so without this the fuzzer would never reach
// the code behind the check.
func fixCRCs(data []byte) []byte {
	if !bytes.HasPrefix(data, signature) {
		return data
	}
	out := bytes.Clone(data)
	pos := len(signature)
	for len(out)-pos >= 12 {
		length := binary.BigEndian.Uint32(out[pos:])
		if uint64(length) > uint64(len(out)-pos-12) {
			break
		}
		end := pos + 8 + int(length)
		binary.BigEndian.PutUint32(out[end:], crc32.ChecksumIEEE(out[pos+4:end]))
		pos = end + 4
	}
	return out
}

func TestRobustness(t *testing.T) {
	sample := fullSample(t)
	coretest.Robustness(t, handler{}, sample)
}

func TestRobustnessBehindCRCs(t *testing.T) {
	sample := fullSample(t)
	rng := rand.New(rand.NewPCG(5, 6))
	mutated := bytes.Clone(sample)
	for range 500 {
		pos := rng.IntN(len(mutated))
		original := mutated[pos]
		mutated[pos] = original ^ byte(1+rng.IntN(255))
		coretest.FuzzTarget(t, handler{}, fixCRCs(mutated))
		mutated[pos] = original
		if t.Failed() {
			t.Fatalf("failed with byte %d changed", pos)
		}
	}
}

func FuzzClean(f *testing.F) {
	f.Add(fullSample(f))
	f.Add(apng(f, interlacedPNG(f, 19)))
	f.Add(interlacedPNG(f, 8))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, handler{}, data)
		if fixed := fixCRCs(data); !bytes.Equal(fixed, data) {
			coretest.FuzzTarget(t, handler{}, fixed)
		}
	})
}
