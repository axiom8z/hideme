package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	stdjpeg "image/jpeg"
	"slices"
	"testing"
	"time"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

func clean(t *testing.T, data []byte, opts core.Options) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, opts)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := stdjpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func assertSamePixels(t *testing.T, a, b image.Image) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds changed: %v became %v", a.Bounds(), b.Bounds())
	}
	bounds := a.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("pixel (%d,%d) changed", x, y)
			}
		}
	}
}

func secrets(extra ...[][]byte) [][]byte {
	return slices.Concat(extra...)
}

func TestContract(t *testing.T) {
	base := noisyJPEG(t)
	scan := lastScan(t, base)
	profile := fakeICC(5000)
	iptc := [][]byte{
		[]byte(fakeByline), []byte(fakeCity), []byte(fakeState), []byte(fakeCountry),
		[]byte(fakeCaption), []byte(fakeCredit), []byte(fakeKeyword), []byte("Test IRB Thumbnail"),
		[]byte("Photoshop 3.0"),
	}
	cases := []coretest.Case{
		{
			Name:       "no metadata",
			Data:       base,
			WantFields: nil,
			MustContain: [][]byte{
				scan,
			},
		},
		{
			Name:           "EXIF",
			Data:           insertAfterSOI(base, exifSegmentFor(coretest.FakeEXIF(0))),
			WantFields:     coretest.FakeEXIFFields(),
			MustNotContain: secrets(coretest.FakeEXIFSecrets(), [][]byte{[]byte("Exif\x00\x00")}),
			MustContain:    [][]byte{scan},
		},
		{
			Name:           "EXIF big-endian with a rotated orientation is kept minimal",
			Data:           insertAfterSOI(base, exifSegmentFor(coretest.FakeIFD(6).Encode(binary.BigEndian))),
			WantFields:     coretest.FakeEXIFFields(),
			WantRetained:   []string{"Orientation"},
			MustNotContain: coretest.FakeEXIFSecrets(),
			MustContain:    [][]byte{scan, []byte("Exif\x00\x00"), exif.Minimal(6)},
		},
		{
			Name:           "XMP",
			Data:           insertAfterSOI(base, xmpSegment()),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: secrets(coretest.FakeXMPSecrets(), [][]byte{[]byte(sigXMP)}),
			MustContain:    [][]byte{scan},
		},
		{
			Name:           "extended XMP across segments",
			Data:           insertAfterSOI(base, extendedXMPSegments(coretest.FakeXMP(), 500)...),
			WantFields:     []string{"Creator", "GPS coordinates", "City"},
			MustNotContain: secrets(coretest.FakeXMPSecrets(), [][]byte{[]byte(sigExtendedXMP)}),
			MustContain:    [][]byte{scan},
		},
		{
			Name:           "comment",
			Data:           insertAfterSOI(base, segment(jpegmeta.MarkerCOM, []byte(fakeComment))),
			WantFields:     []string{"Comment"},
			MustNotContain: [][]byte{[]byte(fakeComment)},
			MustContain:    [][]byte{scan},
		},
		{
			Name:           "Photoshop IPTC and thumbnail",
			Data:           insertAfterSOI(base, photoshopSegment()),
			WantFields:     []string{"By-line", "City", "Province/State", "Country", "Caption", "Credit", "Keywords", "Embedded thumbnail", "Other Photoshop resources"},
			MustNotContain: iptc,
			MustContain:    [][]byte{scan},
		},
		{
			Name:         "ICC profile across segments keeps its chunk order",
			Data:         insertAfterSOI(base, iccSegments(profile, 2000)...),
			WantRetained: []string{"ICC colour profile"},
			MustContain:  append([][]byte{scan}, iccSegments(profile, 2000)...),
		},
		{
			Name:           "JFIF thumbnail is dropped and the header kept",
			Data:           insertAfterSOI(base, jfifSegment(2, 1, []byte("TestJFIFThumb"))),
			WantFields:     []string{"Embedded thumbnail", "Extra JFIF data"},
			MustNotContain: [][]byte{[]byte("TestJFIFThumb")},
			MustContain:    [][]byte{scan, jfifSegment(0, 0, nil)},
		},
		{
			Name:           "JFXX thumbnail",
			Data:           insertAfterSOI(base, jfifSegment(0, 0, nil), segment(jpegmeta.MarkerAPP0, []byte(sigJFXX), []byte("TestJFXXThumb"))),
			WantFields:     []string{"Embedded thumbnail"},
			MustNotContain: [][]byte{[]byte("TestJFXXThumb"), []byte("JFXX")},
			MustContain:    [][]byte{scan, jfifSegment(0, 0, nil)},
		},
		{
			Name: "MPF, FlashPix and unknown APPn",
			Data: insertAfterSOI(base,
				segment(jpegmeta.MarkerAPP0+2, []byte(sigMPF), []byte("TestMPF")),
				segment(jpegmeta.MarkerAPP0+2, []byte(sigFlashPix), []byte("TestFlashPix")),
				segment(jpegmeta.MarkerAPP0+12, []byte("Ducky\x00"), []byte("TestDucky")),
				segment(jpegmeta.MarkerAPP0+14, []byte("NotAdobe"), []byte("TestNotAdobe")),
				segment(jpegmeta.MarkerAPP0+1, []byte("Unknown\x00TestUnknownApp1")),
			),
			WantFields:     []string{"Multi-picture index", "FlashPix data", "APP12 segment", "APP14 segment", "APP1 segment"},
			MustNotContain: [][]byte{[]byte("TestMPF"), []byte("TestFlashPix"), []byte("TestDucky"), []byte("TestNotAdobe"), []byte("TestUnknownApp1")},
			MustContain:    [][]byte{scan},
		},
		{
			Name: "Adobe segment is kept at its defined size",
			Data: insertAfterSOI(base, segment(jpegmeta.MarkerAPP0+14, []byte(sigAdobe), []byte{0, 100, 0, 0, 0, 0, 1}, []byte("TestExtra"))),
			WantFields: []string{
				"Extra Adobe data",
			},
			MustNotContain: [][]byte{[]byte("TestExtra")},
			MustContain:    [][]byte{scan, segment(jpegmeta.MarkerAPP0+14, []byte(sigAdobe), []byte{0, 100, 0, 0, 0, 0, 1})},
		},
		{
			Name:           "trailing motion photo video",
			Data:           append(bytes.Clone(base), "\x00\x00\x00\x18ftypmp42Test Motion Photo"...),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Motion Photo")},
			MustContain:    [][]byte{scan},
		},
		{
			Name:           "trailing second JPEG",
			Data:           append(bytes.Clone(base), base...),
			WantFields:     []string{"Trailing data"},
			MustContain:    [][]byte{scan},
			MustNotContain: [][]byte{append(bytes.Clone(base[len(base)-8:]), base[:8]...)},
		},
		{
			Name: "everything at once",
			Data: append(insertAfterSOI(base, slices.Concat(
				[][]byte{jfifSegment(1, 1, []byte{1, 2, 3})},
				[][]byte{exifSegmentFor(coretest.FakeEXIF(8)), xmpSegment(), photoshopSegment()},
				iccSegments(profile, 3000),
				[][]byte{segment(jpegmeta.MarkerCOM, []byte(fakeComment))},
			)...), "TestTrailer"...),
			WantFields:     append(coretest.FakeEXIFFields(), "Comment", "By-line", "Trailing data"),
			WantRetained:   []string{"Orientation", "ICC colour profile"},
			MustNotContain: secrets(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), iptc, [][]byte{[]byte(fakeComment), []byte("TestTrailer")}),
			MustContain:    [][]byte{scan, exif.Minimal(8), jfifSegment(0, 0, nil)},
		},
		{
			Name:           "progressive with metadata between scans",
			Data:           progressiveJPEG(metadataBetweenScans),
			WantFields:     []string{"Comment", "Creator", "APP5 segment"},
			MustNotContain: secrets(coretest.FakeXMPSecrets(), [][]byte{[]byte("Test Between Scans"), []byte("TestApp5")}),
		},
		{
			Name:        "restart markers",
			Data:        restartJPEG(false),
			MustContain: [][]byte{{0xFF, 0xD0}, {0xFF, 0xD1}, {0xFF, 0xD2}},
		},
		{
			Name:        "fill bytes inside the scan",
			Data:        restartJPEG(true),
			MustContain: [][]byte{{0xFF, 0xFF, 0xFF, 0xD0}},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

func metadataBetweenScans(scan int) []byte {
	if scan == 1 {
		return bytes.Join([][]byte{segment(jpegmeta.MarkerCOM, []byte("Test Between Scans")), xmpSegment()}, nil)
	}
	return segment(jpegmeta.MarkerAPP0+5, []byte("TestApp5"))
}

func noBetween(int) []byte { return nil }

func TestStuffingIsExercised(t *testing.T) {
	if !bytes.Contains(lastScan(t, noisyJPEG(t)), []byte{0xFF, 0x00}) {
		t.Fatal("the fixture's scan holds no stuffed bytes, so it no longer tests them")
	}
}

func TestPixelsSurvive(t *testing.T) {
	base := noisyJPEG(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"baseline", insertAfterSOI(base, exifSegmentFor(coretest.FakeEXIF(0)), xmpSegment(), photoshopSegment())},
		{"baseline with JFIF thumbnail", insertAfterSOI(base, jfifSegment(1, 1, []byte{1, 2, 3}), segment(jpegmeta.MarkerCOM, []byte(fakeComment)))},
		{"baseline with ICC", insertAfterSOI(base, iccSegments(fakeICC(5000), 2000)...)},
		{"progressive", progressiveJPEG(noBetween)},
		{"progressive with metadata between scans", progressiveJPEG(metadataBetweenScans)},
		{"restart markers", restartJPEG(false)},
		{"restart markers with fill bytes", restartJPEG(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := decode(t, tc.data)
			for _, opts := range []core.Options{{}, {Strict: true}} {
				out, _ := clean(t, tc.data, opts)
				assertSamePixels(t, before, decode(t, out))
			}
		})
	}
}

func TestProgressiveFixtureIsReallyProgressive(t *testing.T) {
	data := progressiveJPEG(noBetween)
	if !bytes.Contains(data, []byte{0xFF, 0xC2}) {
		t.Fatal("no SOF2 marker")
	}
	if n := bytes.Count(data, []byte{0xFF, jpegmeta.MarkerSOS}); n != 3 {
		t.Fatalf("got %d scans, want 3", n)
	}
	if img := decode(t, data); img.Bounds().Dx() != 16 {
		t.Fatalf("decoded width %d, want 16", img.Bounds().Dx())
	}
}

func TestProgressiveStructureIsKept(t *testing.T) {
	data := progressiveJPEG(metadataBetweenScans)
	out, _ := clean(t, data, core.Options{})
	if want := progressiveJPEG(noBetween); !bytes.Equal(out, want) {
		t.Fatalf("output differs from the same file with no metadata: %d bytes, want %d", len(out), len(want))
	}
}

func TestOutputLayout(t *testing.T) {
	base := noisyJPEG(t)
	data := insertAfterSOI(base, exifSegmentFor(coretest.FakeEXIF(3)), jfifSegment(0, 0, nil))
	out, _ := clean(t, data, core.Options{})
	want := bytes.Join([][]byte{
		{0xFF, jpegmeta.MarkerSOI},
		jfifSegment(0, 0, nil),
		exifSegmentFor(exif.Minimal(3)),
		base[2:],
	}, nil)
	if !bytes.Equal(out, want) {
		t.Fatalf("JFIF and the minimal EXIF block must directly follow SOI: got %d bytes, want %d", len(out), len(want))
	}
}

func TestStrictRemovesRetainedData(t *testing.T) {
	base := noisyJPEG(t)
	segs := append(iccSegments(fakeICC(5000), 2000), exifSegmentFor(coretest.FakeEXIF(6)))
	out, rep := clean(t, insertAfterSOI(base, segs...), core.Options{Strict: true})
	if !bytes.Equal(out, base) {
		t.Fatalf("strict output differs from the bare image: %d bytes, want %d", len(out), len(base))
	}
	if retained := len(rep.Findings) - len(rep.Residual()); retained != 0 {
		t.Fatalf("strict mode retained %d findings", retained)
	}
	if !hasField(rep, "ICC colour profile", core.ActionRemove) || !hasField(rep, "Orientation", core.ActionRemove) {
		t.Fatalf("strict report does not list the removals: %+v", rep.Findings)
	}
}

func hasField(rep *core.Report, field string, action core.Action) bool {
	return slices.ContainsFunc(rep.Findings, func(f core.Finding) bool {
		return f.Field == field && f.Action == action
	})
}

func TestOrientationOfOneIsDropped(t *testing.T) {
	base := noisyJPEG(t)
	out, rep := clean(t, insertAfterSOI(base, exifSegmentFor(coretest.FakeEXIF(1))), core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("orientation 1 means no rotation and should leave no EXIF block behind")
	}
	if !hasField(rep, "Orientation", core.ActionRemove) {
		t.Fatalf("orientation 1 should be reported as removed: %+v", rep.Findings)
	}
}

// Readers honour the first EXIF block, so a later one must not affect the output
// nor be described as kept.
func TestOnlyFirstExifBlockDecidesOrientation(t *testing.T) {
	base := noisyJPEG(t)
	data := insertAfterSOI(base, exifSegmentFor(coretest.FakeEXIF(0)), exifSegmentFor(coretest.FakeEXIF(6)))
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("a second EXIF block changed the output")
	}
	if !hasField(rep, "Orientation", core.ActionRemove) || hasField(rep, "Orientation", core.ActionRetain) {
		t.Fatalf("the second block's orientation should be reported as removed: %+v", rep.Findings)
	}
}

func TestDoublePrefixedExifIsUnderstood(t *testing.T) {
	base := noisyJPEG(t)
	block := append([]byte(exif.Prefix), coretest.FakeEXIF(6)...)
	out, rep := clean(t, insertAfterSOI(base, exifSegmentFor(block)), core.Options{})
	if want := insertAfterSOI(base, exifSegmentFor(exif.Minimal(6))); !bytes.Equal(out, want) {
		t.Fatalf("the orientation in a double-prefixed block was lost: %d bytes, want %d", len(out), len(want))
	}
	if !hasField(rep, "GPS coordinates", core.ActionRemove) {
		t.Fatalf("a double-prefixed block was not decoded: %+v", rep.Findings)
	}
}

func TestDamagedExifIsReportedAndRemoved(t *testing.T) {
	base := noisyJPEG(t)
	out, rep := clean(t, insertAfterSOI(base, segment(jpegmeta.MarkerAPP0+1, []byte("Exif\x00\x00Test Damaged Block"))), core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("a damaged block must be dropped whole")
	}
	if !hasField(rep, "Damaged EXIF block", core.ActionRemove) {
		t.Fatalf("no damage finding: %+v", rep.Findings)
	}
}

func TestDamagedICCIsRemoved(t *testing.T) {
	base := noisyJPEG(t)
	profile := fakeICC(5000)
	good := iccSegments(profile, 2000)
	notAProfile := make([]byte, 100)
	copy(notAProfile, "Test Not A Profile")
	wrongSize := fakeICC(500)
	wrongSize[3]++
	cases := map[string][][]byte{
		"not a profile":     {segment(jpegmeta.MarkerAPP0+2, []byte(sigICC), []byte{1, 1}, notAProfile)},
		"wrong size":        {segment(jpegmeta.MarkerAPP0+2, []byte(sigICC), []byte{1, 1}, wrongSize)},
		"missing chunk":     good[:2],
		"duplicate chunk":   {good[0], good[0], good[1]},
		"sequence 0":        {segment(jpegmeta.MarkerAPP0+2, []byte(sigICC), []byte{0, 1}, profile)},
		"count 0":           {segment(jpegmeta.MarkerAPP0+2, []byte(sigICC), []byte{1, 0}, profile)},
		"first chunk short": {segment(jpegmeta.MarkerAPP0+2, []byte(sigICC), []byte{1, 1}, []byte("short"))},
	}
	for name, segs := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, insertAfterSOI(base, segs...), core.Options{})
			if !bytes.Equal(out, base) {
				t.Fatal("a damaged profile must be dropped")
			}
			if !hasField(rep, "Damaged ICC profile", core.ActionRemove) {
				t.Fatalf("no damage finding: %+v", rep.Findings)
			}
		})
	}
}

func TestICCChunksOutOfOrderAreKeptInOrder(t *testing.T) {
	base := noisyJPEG(t)
	good := iccSegments(fakeICC(5000), 2000)
	data := insertAfterSOI(base, good[2], good[0], good[1])
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, data) {
		t.Fatal("chunks that readers can reassemble must be kept in their file order")
	}
	if !hasField(rep, "ICC colour profile", core.ActionRetain) {
		t.Fatalf("expected retained profile: %+v", rep.Findings)
	}
}

func TestPhotoshopSpanningSegments(t *testing.T) {
	base := noisyJPEG(t)
	block := resource(jpegmeta.ResourceIPTC, fakeIPTC())
	split := len(block) / 2
	data := insertAfterSOI(base,
		segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop), block[:split]),
		segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop), block[split:]),
	)
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("APP13 segments were not all dropped")
	}
	if !hasField(rep, "By-line", core.ActionRemove) || !hasField(rep, "Keywords", core.ActionRemove) {
		t.Fatalf("the joined block was not decoded: %+v", rep.Findings)
	}
}

func TestPhotoshopNestedBlocksAreDecoded(t *testing.T) {
	base := noisyJPEG(t)
	data := insertAfterSOI(base, segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop),
		resource(jpegmeta.ResourceExif1, coretest.FakeEXIF(6)),
		resource(jpegmeta.ResourceXMP, coretest.FakeXMP()),
	))
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("nested blocks must go with the segment, and orientation in them is never kept")
	}
	for _, field := range []string{"GPS coordinates", "Creator", "Orientation"} {
		if !hasField(rep, field, core.ActionRemove) {
			t.Errorf("field %q not reported as removed", field)
		}
	}
}

func TestPhotoshopDamageIsReported(t *testing.T) {
	base := noisyJPEG(t)
	truncated := resource(jpegmeta.ResourceIPTC, fakeIPTC())
	truncated = truncated[:len(truncated)-4]
	overlong := append(iptcDataset(80, fakeByline), 0x1C, 2, 90, 0xFF, 0xFF)
	badWidth := append(iptcDataset(80, fakeByline), 0x1C, 2, 90, 0x80, 0x08)
	longValue := append([]byte{0x1C, 2, 90, 0x80, 0x02, 0x00, 0x03}, "Abc"...)
	cases := []struct {
		name    string
		payload []byte
		field   string
	}{
		{"not image resources", []byte("not a resource block"), "Damaged Photoshop data"},
		{"truncated resource", truncated, "Damaged Photoshop data"},
		{"bad IPTC marker", resource(jpegmeta.ResourceIPTC, []byte("Test not IPTC")), "Damaged IPTC data"},
		{"IPTC value overrun", resource(jpegmeta.ResourceIPTC, overlong), "Damaged IPTC data"},
		{"IPTC size field too wide", resource(jpegmeta.ResourceIPTC, badWidth), "Damaged IPTC data"},
		{"long-form IPTC value", resource(jpegmeta.ResourceIPTC, longValue), "City"},
		{"no resources at all", nil, "Photoshop data"},
		{"zero padding only", make([]byte, 8), "Photoshop data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := insertAfterSOI(base, segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop), tc.payload))
			out, rep := clean(t, data, core.Options{})
			if !bytes.Equal(out, base) {
				t.Fatal("the segment must be dropped whatever it holds")
			}
			if !hasField(rep, tc.field, core.ActionRemove) {
				t.Fatalf("field %q not reported: %+v", tc.field, rep.Findings)
			}
		})
	}
}

func TestIPTCFindingsAreCapped(t *testing.T) {
	base := noisyJPEG(t)
	iptc := bytes.Repeat(iptcDataset(25, "k"), 3*maxIPTCFindings)
	_, rep := clean(t, insertAfterSOI(base, segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop), resource(jpegmeta.ResourceIPTC, iptc))), core.Options{})
	if n := len(rep.Findings); n > maxIPTCFindings+1 {
		t.Fatalf("got %d findings from one block", n)
	}
	if !hasField(rep, "Other IPTC datasets", core.ActionRemove) {
		t.Fatal("datasets over the cap must still be counted")
	}
}

// A long run of zeros after many resources must not be rescanned for each one.
// Nothing here fails if the walk is linear; if it is not, the test takes minutes
// and trips the limit.
func TestPaddingDoesNotMakeThePhotoshopWalkQuadratic(t *testing.T) {
	coretest.SkipIfRace(t)
	base := noisyJPEG(t)
	const chunk = 60000
	joined := append(bytes.Repeat(resource(0x03ED, nil), 400000), make([]byte, 32<<20)...)
	var segs [][]byte
	for off := 0; off < len(joined); off += chunk {
		segs = append(segs, segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop), joined[off:min(off+chunk, len(joined))]))
	}
	start := time.Now()
	out, rep := clean(t, insertAfterSOI(base, segs...), core.Options{})
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("the segments must be dropped")
	}
	if !hasField(rep, "Other Photoshop resources", core.ActionRemove) || hasField(rep, "Damaged Photoshop data", core.ActionRemove) {
		t.Fatalf("zero padding must not read as damage: %+v", rep.Findings)
	}
}

func TestStraySegments(t *testing.T) {
	base := noisyJPEG(t)
	profile := fakeICC(300)
	eoi := len(base) - 2
	data := slices.Concat(base[:eoi],
		iccSegments(profile, 1000)[0],
		jfifSegment(0, 0, nil),
		base[eoi:])
	_, rep := clean(t, data, core.Options{})
	if hasField(rep, "ICC colour profile", core.ActionRetain) {
		t.Fatal("a profile after the image data cannot be applied, so it must not be kept")
	}
	for _, field := range []string{"Stray ICC profile data", "Stray JFIF header"} {
		if !hasField(rep, field, core.ActionRemove) {
			t.Errorf("field %q not reported: %+v", field, rep.Findings)
		}
	}
}

func TestSecondJFIFIsDropped(t *testing.T) {
	base := noisyJPEG(t)
	out, rep := clean(t, insertAfterSOI(base, jfifSegment(0, 0, nil), jfifSegment(0, 0, nil)), core.Options{})
	if want := insertAfterSOI(base, jfifSegment(0, 0, nil)); !bytes.Equal(out, want) {
		t.Fatal("only the first JFIF header should survive")
	}
	if !hasField(rep, "Stray JFIF header", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
}

func TestShortOrUnknownSignaturesAreStillReported(t *testing.T) {
	base := noisyJPEG(t)
	cases := []struct {
		name  string
		seg   []byte
		field string
	}{
		{"ICC signature with no header", segment(jpegmeta.MarkerAPP0+2, []byte(sigICC)), "APP2 segment"},
		{"unknown APP0", segment(jpegmeta.MarkerAPP0, []byte("AVI1\x00Test")), "APP0 segment"},
		{"non-Photoshop APP13", segment(jpegmeta.MarkerAPP0+13, []byte("Adobe_CM\x00Test")), "APP13 segment"},
		{"short Adobe", segment(jpegmeta.MarkerAPP0+14, []byte(sigAdobe)), "APP14 segment"},
		{"XMP under an unknown signature", segment(jpegmeta.MarkerAPP0+1, coretest.FakeXMP()), "Creator"},
		{"bare Exif signature", segment(jpegmeta.MarkerAPP0+1, []byte(sigExif)), "Damaged EXIF block"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, rep := clean(t, insertAfterSOI(base, tc.seg), core.Options{})
			if !bytes.Equal(out, base) {
				t.Fatal("the segment must be dropped")
			}
			if !hasField(rep, tc.field, core.ActionRemove) {
				t.Fatalf("field %q not reported: %+v", tc.field, rep.Findings)
			}
		})
	}
}

func TestByteCountWording(t *testing.T) {
	if got := core.ByteCount(1); got != "1 byte" {
		t.Errorf("core.ByteCount(1) = %q", got)
	}
	if got := countOf(1, "dataset"); got != "1 dataset" {
		t.Errorf("countOf(1) = %q", got)
	}
}

func TestUnparseableIsRefused(t *testing.T) {
	base := noisyJPEG(t)
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, core.ErrUnsupported},
		{"not JPEG", []byte("not a jpeg at all"), core.ErrUnsupported},
		{"no scan", []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerEOI}, core.ErrMalformed},
		{"only tables", bytes.Join([][]byte{{0xFF, jpegmeta.MarkerSOI}, segment(jpegmeta.MarkerDQT, make([]byte, 65)), {0xFF, jpegmeta.MarkerEOI}}, nil), core.ErrMalformed},
		{"ends in fill bytes", []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, 0xFF}, core.ErrMalformed},
		{"length of zero", insertAfterSOI(base, []byte{0xFF, jpegmeta.MarkerCOM, 0, 0}), core.ErrMalformed},
		{"length past the end", []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerCOM, 0xFF, 0xFF, 0}, core.ErrMalformed},
		{"no room for a length", []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerCOM, 0}, core.ErrMalformed},
		{"scan before frame", []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerSOS, 0, 8, 1, 1, 0, 0, 63, 0, 1, 0xFF, jpegmeta.MarkerEOI}, core.ErrMalformed},
		{"second SOI", insertAfterSOI(base, []byte{0xFF, jpegmeta.MarkerSOI}), core.ErrMalformed},
		{"standalone restart marker", insertAfterSOI(base, []byte{0xFF, 0xD0}), core.ErrMalformed},
		{"second frame", insertAfterSOI(base, segment(0xC0, []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0})), core.ErrMalformed},
		{"short JFIF", insertAfterSOI(base, segment(jpegmeta.MarkerAPP0, []byte(sigJFIF))), core.ErrMalformed},
		{"DQT with spare bytes", insertAfterSOI(base, segment(jpegmeta.MarkerDQT, make([]byte, 65), []byte("Test Spare"))), core.ErrMalformed},
		{"empty DQT", insertAfterSOI(base, segment(jpegmeta.MarkerDQT)), core.ErrMalformed},
		{"DQT bad precision", insertAfterSOI(base, segment(jpegmeta.MarkerDQT, append([]byte{0x20}, make([]byte, 64)...))), core.ErrMalformed},
		{"DHT with spare bytes", insertAfterSOI(base, segment(jpegmeta.MarkerDHT, make([]byte, 17), []byte("Test Spare"))), core.ErrMalformed},
		{"empty DHT", insertAfterSOI(base, segment(jpegmeta.MarkerDHT)), core.ErrMalformed},
		{"DHT bad class", insertAfterSOI(base, segment(jpegmeta.MarkerDHT, append([]byte{0x20}, make([]byte, 16)...))), core.ErrMalformed},
		{"DHT too many symbols", insertAfterSOI(base, segment(jpegmeta.MarkerDHT, append([]byte{0x00}, bytes.Repeat([]byte{0xFF}, 16)...))), core.ErrMalformed},
		{"DRI with spare bytes", insertAfterSOI(base, segment(jpegmeta.MarkerDRI, []byte{0, 1, 2})), core.ErrMalformed},
		{"odd DAC", insertAfterSOI(base, segment(jpegmeta.MarkerDAC, []byte{0, 1, 2})), core.ErrMalformed},
		{"DNL before a scan", insertAfterSOI(base, segment(jpegmeta.MarkerDNL, []byte{0, 1})), core.ErrMalformed},
		{"hierarchical", insertAfterSOI(base, segment(jpegmeta.MarkerDHP, []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0})), core.ErrUnsupported},
		{"expand reference", insertAfterSOI(base, segment(jpegmeta.MarkerEXP, []byte{0x11})), core.ErrUnsupported},
		{"JPEG-LS", insertAfterSOI(base, segment(0xF7, []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0})), core.ErrUnsupported},
		{"reserved JPG marker", insertAfterSOI(base, segment(jpegmeta.MarkerJPG, []byte{0})), core.ErrUnsupported},
		{"reserved low marker", insertAfterSOI(base, segment(0x02, []byte{0})), core.ErrMalformed},
		{"bad scan header", bytes.Replace(base, []byte{0xFF, jpegmeta.MarkerSOS, 0, 12, 3}, []byte{0xFF, jpegmeta.MarkerSOS, 0, 12, 4}, 1), core.ErrMalformed},
		{"bad frame header", bytes.Replace(restartJPEG(false), []byte{0xFF, 0xC0, 0, 11, 8}, []byte{0xFF, 0xC0, 0, 11, 9, 0}, 1), core.ErrMalformed},
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

func TestTooManySegments(t *testing.T) {
	base := noisyJPEG(t)
	comment := segment(jpegmeta.MarkerCOM, []byte("x"))
	data := append(bytes.Clone(base[:2]), bytes.Repeat(comment, maxSegments+1)...)
	data = append(data, base[2:]...)
	var out bytes.Buffer
	_, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestCommentEncodingsAreDecoded(t *testing.T) {
	base := noisyJPEG(t)
	cases := map[string]string{
		"Caf\xC3\xA9": "Café", // UTF-8
		"Caf\xE9":     "Café", // Latin-1
	}
	for raw, want := range cases {
		_, rep := clean(t, insertAfterSOI(base, segment(jpegmeta.MarkerCOM, []byte(raw))), core.Options{})
		if got := rep.Findings[0].Value; got != want {
			t.Errorf("comment %q decoded to %q, want %q", raw, got, want)
		}
	}
}

func TestSegmentLabelNamesKnownWriters(t *testing.T) {
	cases := map[string]string{
		"Ducky\x00data": "Ducky",
		"\x01binary":    "",
		"no terminator": "",
		"":              "",
	}
	for payload, want := range cases {
		if got := segmentLabel([]byte(payload)); got != want {
			t.Errorf("segmentLabel(%q) = %q, want %q", payload, got, want)
		}
	}
}

func TestRegisteredAndMatching(t *testing.T) {
	h := handler{}
	if got := h.Format().ID; got != formatID {
		t.Fatalf("format ID %q", got)
	}
	if h.Match([]byte{0xFF, 0xD8}, nil) {
		t.Error("two bytes must not be enough to claim a file")
	}
	if h.Match([]byte("GIF89a"), nil) {
		t.Error("matched a GIF")
	}
	if !h.Match(noisyJPEG(t), nil) {
		t.Error("did not match a JPEG")
	}
	if !slices.ContainsFunc(core.Handlers(), func(r core.Handler) bool { return r.Format().ID == formatID }) {
		t.Error("the handler did not register itself")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteErrorIsReturned(t *testing.T) {
	_, err := handler{}.Clean(context.Background(), core.BytesSource(noisyJPEG(t)), failingWriter{}, core.Options{})
	if err == nil {
		t.Fatal("a failing writer must fail Clean")
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := handler{}.Inspect(ctx, core.BytesSource(noisyJPEG(t)), core.Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// fullSample packs every kind of segment the handler treats specially into one
// small file, so mutations of it reach all of the parsing code.
func fullSample() []byte {
	return insertAfterSOI(progressiveJPEG(metadataBetweenScans), slices.Concat(
		[][]byte{jfifSegment(1, 1, []byte{1, 2, 3})},
		[][]byte{exifSegmentFor(coretest.FakeEXIF(6))},
		[][]byte{xmpSegment()},
		extendedXMPSegments([]byte("<x:xmpmeta>short</x:xmpmeta>"), 10),
		[][]byte{photoshopSegment()},
		iccSegments(fakeICC(300), 200),
		[][]byte{segment(jpegmeta.MarkerAPP0+14, []byte(sigAdobe), []byte{0, 100, 0, 0, 0, 0, 1})},
		[][]byte{segment(jpegmeta.MarkerCOM, []byte(fakeComment))},
	)...)
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, fullSample())
	coretest.Robustness(t, handler{}, restartJPEG(true))
}

func FuzzClean(f *testing.F) {
	f.Add(fullSample())
	f.Add(restartJPEG(true))
	f.Add(progressiveJPEG(noBetween))
	f.Add(append(restartJPEG(false), "TestTrailer"...))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, handler{}, data)
	})
}
