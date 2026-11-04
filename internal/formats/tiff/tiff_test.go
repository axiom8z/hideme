package tiff

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
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

func findingNamed(rep *core.Report, name string) *core.Finding {
	i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == name })
	if i < 0 {
		return nil
	}
	return &rep.Findings[i]
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
		want bool
	}{
		{"little-endian TIFF", "II*\x00\x08\x00\x00\x00", true},
		{"big-endian TIFF", "MM\x00*\x00\x00\x00\x08", true},
		{"BigTIFF little-endian", "II+\x00\x08\x00\x00\x00", true},
		{"BigTIFF big-endian", "MM\x00+\x00\x08\x00\x00", true},
		{"ORF", "IIRO\x08\x00\x00\x00", true},
		{"ORF big-endian", "MMOR\x00\x00\x00\x08", true},
		{"ORF alternate", "IIRS\x08\x00\x00\x00", true},
		{"RW2", "IIU\x00\x08\x00\x00\x00", true},
		{"too short", "II*", false},
		{"empty", "", false},
		{"JPEG", "\xFF\xD8\xFF\xE0\x00\x10JFIF", false},
		{"wrong magic number", "II\x2B\x01", false},
		{"plain text", "II is two letters", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (handler{}).Match([]byte(tc.head), core.BytesSource([]byte(tc.head))); got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != "tiff" || f.Family != core.FamilyImage || len(f.Extensions) == 0 || len(f.Limitations) == 0 {
		t.Errorf("incomplete format description: %+v", f)
	}
	var registered bool
	for _, h := range core.Handlers() {
		registered = registered || h.Format().ID == "tiff"
	}
	if !registered {
		t.Error("the handler did not register itself")
	}
}

func TestContract(t *testing.T) {
	jpegStrip := withSegments(realJPEG(t, 1), []byte("Test Trailer"),
		jpegSegment(jpegmeta.MarkerAPP0+1, []byte("Exif\x00\x00Test Strip Author")),
		jpegSegment(jpegmeta.MarkerCOM, []byte("Test Strip Comment")),
	)
	jpegPage := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{jpegStrip}}

	tiled := imageSpec{
		width: 6, height: 6, samples: 1, tileWidth: 4, tileHeight: 4,
		parts: [][]byte{pixels(1, 16), pixels(2, 16), pixels(3, 16), pixels(4, 16)},
	}
	planar := imageSpec{
		width: 2, height: 2, samples: 3, planar: 2, rowsPerStrip: 2,
		parts: [][]byte{pixels(5, 4), pixels(6, 4), pixels(7, 4)},
	}
	secondPage := plainSpec(0x70)
	threePages := &coretest.IFD{
		Entries: append(imageTags(plainSpec(0x50)), coretest.Entry{Tag: 0x010F, Value: "Test Make"}),
		Next: &coretest.IFD{
			Entries: append(imageTags(plainSpec(0x60)), coretest.Entry{Tag: 0x010F, Value: "Test Make"}, coretest.Entry{Tag: 0x013B, Value: "Test Author"}),
			Next: &coretest.IFD{
				Entries: append(imageTags(secondPage), coretest.Entry{Tag: tagPageName, Value: "Test Page Three"}),
			},
		},
	}
	preview := imageSpec{width: 2, height: 2, samples: 3, rowsPerStrip: 2, parts: [][]byte{[]byte("Test Preview")}}
	previewPage := &coretest.IFD{
		Entries: imageTags(plainSpec(0x20)),
		Next: &coretest.IFD{
			Entries: append(imageTags(preview), coretest.Entry{Tag: tagNewSubfileType, Value: uint32(1)}),
		},
	}
	sparse := imageSpec{width: 4, height: 4, samples: 3, rowsPerStrip: 2, parts: [][]byte{pixels(8, 24), {}}}

	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name:           "little-endian with every kind of metadata",
			Data:           loaded(littleEndian),
			WantFields:     loadedFields(),
			WantRetained:   []string{"Orientation", "ICC colour profile"},
			MustNotContain: loadedSecrets(),
			MustContain:    [][]byte{pixels(0x40, 24), pixels(0x41, 24), fakeICC()},
		},
		{
			Name:           "big-endian with every kind of metadata",
			Data:           loaded(bigEndian),
			WantFields:     loadedFields(),
			WantRetained:   []string{"Orientation", "ICC colour profile"},
			MustNotContain: loadedSecrets(),
			MustContain:    [][]byte{pixels(0x40, 24), pixels(0x41, 24), fakeICC()},
		},
		{
			Name:           "strict removes orientation and the ICC profile",
			Data:           loaded(littleEndian),
			Opts:           core.Options{Strict: true},
			WantFields:     append(loadedFields(), "Orientation", "ICC colour profile"),
			MustNotContain: append(loadedSecrets(), fakeICC()),
			MustContain:    [][]byte{pixels(0x40, 24)},
		},
		{
			Name:        "no metadata at all",
			Data:        singlePage(littleEndian, plainSpec(1)),
			MustContain: [][]byte{pixels(1, 24), pixels(2, 24)},
		},
		{
			Name:           "tiled",
			Data:           singlePage(bigEndian, tiled, coretest.Entry{Tag: 0x013B, Value: "Test Author"}),
			WantFields:     []string{"Artist"},
			MustNotContain: [][]byte{[]byte("Test Author")},
			MustContain:    [][]byte{pixels(1, 16), pixels(4, 16)},
		},
		{
			Name:        "planar",
			Data:        singlePage(littleEndian, planar),
			MustContain: [][]byte{pixels(5, 4), pixels(6, 4), pixels(7, 4)},
		},
		{
			Name:           "three pages",
			Data:           build(littleEndian, threePages, nil, plainSpec(0x50).parts, plainSpec(0x60).parts, secondPage.parts),
			WantFields:     []string{"Camera make", "Artist", "Page name"},
			MustNotContain: [][]byte{[]byte("Test Make"), []byte("Test Author"), []byte("Test Page Three")},
			MustContain:    [][]byte{pixels(0x50, 24), pixels(0x60, 24), pixels(0x70, 24)},
		},
		{
			Name:           "metadata inside JPEG strips",
			Data:           singlePage(littleEndian, jpegPage),
			WantFields:     []string{"Metadata in JPEG data", "Unreferenced data"},
			MustNotContain: [][]byte{[]byte("Test Strip Author"), []byte("Test Strip Comment"), []byte("Test Trailer")},
		},
		{
			Name:           "a reduced-resolution page",
			Data:           build(littleEndian, previewPage, nil, plainSpec(0x20).parts, preview.parts),
			WantFields:     []string{"Embedded thumbnail"},
			MustNotContain: [][]byte{[]byte("Test Preview")},
			MustContain:    [][]byte{pixels(0x20, 24)},
		},
		{
			Name:           "hidden data between strips",
			Data:           build(littleEndian, &coretest.IFD{Entries: imageTags(plainSpec(9))}, []byte("Test Hidden Gap "), plainSpec(9).parts),
			WantFields:     []string{"Unreferenced data"},
			MustNotContain: [][]byte{[]byte("Test Hidden Gap")},
			MustContain:    [][]byte{pixels(9, 24), pixels(10, 24)},
		},
		{
			Name:        "a sparse strip",
			Data:        singlePage(littleEndian, sparse),
			MustContain: [][]byte{pixels(8, 24)},
		},
		{
			Name:         "odd-length strips and values, which need padding",
			Data:         singlePage(bigEndian, oddSpec(), coretest.Entry{Tag: tagICCProfile, Value: oddICC()}),
			WantRetained: []string{"ICC colour profile"},
			MustContain:  [][]byte{pixels(1, 3), pixels(2, 3), oddICC()},
		},
	})
}

// oddSpec has strips of three bytes, so whatever follows one of them needs padding.
func oddSpec() imageSpec {
	return imageSpec{width: 3, height: 2, rowsPerStrip: 1, parts: [][]byte{pixels(1, 3), pixels(2, 3)}}
}

// TIFF 6.0 requires directories and values to begin on even offsets. Readers often
// forgive a file that breaks the rule, so only a direct check can catch it.
func TestOutputIsWordAligned(t *testing.T) {
	root := &coretest.IFD{
		Entries: append(imageTags(oddSpec()), coretest.Entry{Tag: tagICCProfile, Value: append(fakeICC(), 'x')}),
		Next:    &coretest.IFD{Entries: imageTags(oddSpec())},
	}
	for _, order := range []binary.ByteOrder{littleEndian, bigEndian} {
		out, _ := clean(t, build(order, root, nil, oddSpec().parts, oddSpec().parts), core.Options{})
		pages := 0
		for off := order.Uint32(out[4:]); off != 0; pages++ {
			if off%2 != 0 {
				t.Errorf("directory at odd offset %d", off)
			}
			n := int(order.Uint16(out[off:]))
			for i := range n {
				e := out[int(off)+2+i*entrySize:]
				tag, typ, count := order.Uint16(e), order.Uint16(e[2:]), order.Uint32(e[4:])
				if typeSize(typ)*int64(count) <= 4 {
					continue
				}
				at := order.Uint32(e[8:])
				if at%2 != 0 {
					t.Errorf("tag %d has its value at odd offset %d", tag, at)
				}
				if tag == tagStripOffsets {
					for j := range count {
						if strip := order.Uint32(out[at+4*j:]); strip%2 != 0 {
							t.Errorf("strip at odd offset %d", strip)
						}
					}
				}
			}
			off = order.Uint32(out[int(off)+2+n*entrySize:])
		}
		if pages != 2 {
			t.Errorf("%d pages, want 2", pages)
		}
	}
}

func TestRobustness(t *testing.T) {
	jpegStrip := withSegments(realJPEG(t, 1), []byte("trailer"), jpegSegment(jpegmeta.MarkerCOM, []byte("comment")))
	jpegPage := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{jpegStrip}}
	tiled := imageSpec{width: 4, height: 4, tileWidth: 4, tileHeight: 4, parts: [][]byte{pixels(1, 16)}}
	for name, data := range map[string][]byte{
		"little-endian": loaded(littleEndian),
		"big-endian":    loaded(bigEndian),
		"JPEG":          singlePage(littleEndian, jpegPage),
		"tiled":         singlePage(bigEndian, tiled),
	} {
		t.Run(name, func(t *testing.T) { coretest.Robustness(t, handler{}, data) })
	}
}

func TestPixelsSurvive(t *testing.T) {
	tiled := imageSpec{
		width: 6, height: 6, samples: 3, tileWidth: 4, tileHeight: 4,
		parts: [][]byte{pixels(1, 48), pixels(2, 48), pixels(3, 48), pixels(4, 48)},
	}
	planar := imageSpec{
		width: 2, height: 2, samples: 3, planar: 2, rowsPerStrip: 1,
		parts: [][]byte{pixels(1, 2), pixels(2, 2), pixels(3, 2), pixels(4, 2), pixels(5, 2), pixels(6, 2)},
	}
	multiPage := &coretest.IFD{
		Entries: imageTags(plainSpec(0x11)),
		Next:    &coretest.IFD{Entries: imageTags(plainSpec(0x22))},
	}
	for name, data := range map[string][]byte{
		"strips little-endian": loaded(littleEndian),
		"strips big-endian":    loaded(bigEndian),
		"tiles":                singlePage(bigEndian, tiled),
		"planar":               singlePage(littleEndian, planar),
		"two pages":            build(littleEndian, multiPage, []byte("gap!"), plainSpec(0x11).parts, plainSpec(0x22).parts),
	} {
		t.Run(name, func(t *testing.T) {
			want, err := readPages(data)
			if err != nil {
				t.Fatalf("decoding the input: %v", err)
			}
			for _, opts := range []core.Options{{}, {Strict: true}} {
				out, _ := clean(t, data, opts)
				got, err := readPages(out)
				if err != nil {
					t.Fatalf("decoding the output: %v", err)
				}
				if len(got) != len(want) {
					t.Fatalf("%d pages became %d", len(want), len(got))
				}
				for i := range want {
					if got[i].width != want[i].width || got[i].height != want[i].height || got[i].samples != want[i].samples {
						t.Errorf("page %d changed shape: %+v became %+v", i, want[i], got[i])
					}
					if !bytes.Equal(got[i].pix, want[i].pix) {
						t.Errorf("page %d lost its pixels", i)
					}
				}
			}
		})
	}
}

// The strip stays a real JPEG that a real decoder reads to the same picture, once
// its comment, EXIF block and trailing data are gone.
func TestJPEGStripPixelsSurvive(t *testing.T) {
	stream := realJPEG(t, 3)
	want, err := jpeg.Decode(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	tampered := withSegments(stream, []byte("Test Trailer"),
		jpegSegment(jpegmeta.MarkerAPP0+1, []byte("Exif\x00\x00Test Strip Author")),
		jpegSegment(jpegmeta.MarkerCOM, []byte("Test Strip Comment")),
	)
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{tampered}}
	out, _ := clean(t, singlePage(bigEndian, page), core.Options{})

	i := bytes.Index(out, stream[:2])
	if i < 0 {
		t.Fatal("no JPEG stream in the output")
	}
	if !bytes.Equal(out[i:], stream) {
		t.Fatalf("the strip is not the original JPEG stream: %d bytes, want %d", len(out[i:]), len(stream))
	}
	got, err := jpeg.Decode(bytes.NewReader(out[i:]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.(*image.Gray).Pix, want.(*image.Gray).Pix) {
		t.Error("the JPEG strip decodes to a different picture")
	}
}

func TestJPEGSegmentsKeptAndCut(t *testing.T) {
	adobe := jpegSegment(jpegmeta.MarkerAPP14, []byte("Adobe\x00\x64\x00\x00\x00\x00\x01"))
	jfif := jpegSegment(jpegmeta.MarkerAPP0, []byte("JFIF\x00\x01\x01\x01\x00\x48\x00\x48\x00\x00"))
	jfifWithThumb := jpegSegment(jpegmeta.MarkerAPP0, []byte("JFIF\x00\x01\x01\x01\x00\x48\x00\x48\x01\x00"))
	realDQT := jpegSegment(jpegmeta.MarkerDQT, make([]byte, 65))
	tables := append(append([]byte{0xFF, jpegmeta.MarkerSOI}, jpegSegment(jpegmeta.MarkerCOM, []byte("Test Table Comment"))...),
		slices.Concat(realDQT, []byte{0xFF, jpegmeta.MarkerEOI})...)
	stream := withSegments(realJPEG(t, 5), nil, adobe, jfif, jfifWithThumb)
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{stream}}
	data := singlePage(littleEndian, page, coretest.Entry{Tag: tagJPEGTables, Value: tables})

	out, rep := clean(t, data, core.Options{})
	if !bytes.Contains(out, adobe) {
		t.Error("the Adobe segment, which says how to convert colours, was removed")
	}
	if !bytes.Contains(out, jfif) {
		t.Error("the plain JFIF header was removed")
	}
	if bytes.Contains(out, jfifWithThumb) {
		t.Error("a JFIF header with a thumbnail was kept")
	}
	if bytes.Contains(out, []byte("Test Table Comment")) {
		t.Error("a comment in the JPEG tables was kept")
	}
	if f := findingNamed(rep, "Metadata in JPEG data"); f == nil {
		t.Errorf("no finding for the removed segments: %+v", rep.Findings)
	}
	if !bytes.Contains(out, slices.Concat(realDQT, []byte{0xFF, jpegmeta.MarkerEOI})) {
		t.Error("the JPEG tables lost a quantisation table")
	}
}

func TestRefused(t *testing.T) {
	withTag := func(tag uint16, value any) []byte {
		return singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tag, Value: value})
	}
	withCompression := func(c uint16) []byte {
		s := plainSpec(1)
		s.compression = c
		return singlePage(littleEndian, s)
	}
	withPhotometric := func(v uint16) []byte {
		s := plainSpec(1)
		s.photometric = v
		return singlePage(littleEndian, s)
	}
	cr2 := singlePage(littleEndian, plainSpec(1))
	copy(cr2[8:], "CR\x02\x00")
	withMakerNote := func(order binary.ByteOrder, note string) []byte {
		s := plainSpec(1)
		root := &coretest.IFD{
			Entries: imageTags(s),
			Exif:    &coretest.IFD{Entries: []coretest.Entry{{Tag: tagMakerNote, Value: []byte(note + "Test Raw Directory")}}},
		}
		return build(order, root, nil, s.parts)
	}
	// A second page marks a file as raw just as well as the first.
	rawSecondPage := build(littleEndian, &coretest.IFD{
		Entries: imageTags(plainSpec(1)),
		Next:    &coretest.IFD{Entries: append(imageTags(plainSpec(2)), coretest.Entry{Tag: 0xC612, Value: []byte{1, 4, 0, 0}})},
	}, nil, plainSpec(1).parts, plainSpec(2).parts)
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"BigTIFF little-endian":  {[]byte("II+\x00\x08\x00\x00\x00\x00\x00\x00\x00\x10\x00\x00\x00"), "BigTIFF"},
		"BigTIFF big-endian":     {[]byte("MM\x00+\x00\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x10"), "BigTIFF"},
		"ORF":                    {[]byte("IIRO\x08\x00\x00\x00\x00\x00\x00\x00"), "ORF"},
		"ORF big-endian":         {[]byte("MMOR\x00\x00\x00\x08\x00\x00\x00\x00"), "ORF"},
		"ORF alternate":          {[]byte("IIRS\x08\x00\x00\x00\x00\x00\x00\x00"), "ORF"},
		"RW2":                    {[]byte("IIU\x00\x08\x00\x00\x00\x00\x00\x00\x00"), "RW2"},
		"CR2":                    {cr2, "CR2"},
		"IIQ":                    {withMakerNote(littleEndian, "IIIICwaR"), "IIQ"},
		"IIQ big-endian":         {withMakerNote(binary.BigEndian, "MMMMRawC"), "IIQ"},
		"DNG":                    {withTag(0xC612, []byte{1, 4, 0, 0}), "DNG"},
		"SubIFDs":                {withTag(0x014A, uint32(100)), "SubIFDs"},
		"colour filter pattern":  {withTag(0x828E, []byte{0, 1, 1, 2}), "colour filter"},
		"TIFF/EP":                {withTag(0x9216, []byte{1, 0, 0, 0}), "TIFF/EP"},
		"raw on a later page":    {rawSecondPage, "DNG"},
		"old-style JPEG":         {withCompression(compressionOldJPEG), "old-style JPEG"},
		"NEF compression":        {withCompression(34713), "compression 34713"},
		"ARW compression":        {withCompression(32767), "compression 32767"},
		"PEF compression":        {withCompression(65535), "compression 65535"},
		"JPEG 2000 compression":  {withCompression(34712), "compression 34712"},
		"WebP compression":       {withCompression(50001), "compression 50001"},
		"colour filter array":    {withPhotometric(32803), "photometric"},
		"linear raw":             {withPhotometric(34892), "photometric"},
		"not TIFF at all":        {[]byte("This is plainly not a TIFF file."), "not a TIFF"},
		"shorter than the magic": {[]byte("II"), "too short"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(tc.data), core.Options{}); !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Inspect: got %v, want ErrUnsupported mentioning %q", err, tc.want)
			}
			var out bytes.Buffer
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(tc.data), &out, core.Options{}); !errors.Is(err, core.ErrUnsupported) {
				t.Errorf("Clean: got %v, want ErrUnsupported", err)
			}
			if out.Len() != 0 {
				t.Errorf("Clean wrote %d bytes of a file it refused", out.Len())
			}
		})
	}
}

// replaceTag builds a one-page file with one image tag swapped for entry, with a type and count
// of the caller's choosing.
func replaceTag(order binary.ByteOrder, s imageSpec, tag uint16, entry coretest.Entry) []byte {
	tags := imageTags(s)
	entry.Tag = tag
	i := slices.IndexFunc(tags, func(e coretest.Entry) bool { return e.Tag == tag })
	if i >= 0 {
		tags[i] = entry
	} else {
		tags = append(tags, entry)
	}
	return build(order, &coretest.IFD{Entries: tags}, nil, s.parts)
}

func withoutTag(order binary.ByteOrder, s imageSpec, tag uint16) []byte {
	tags := slices.DeleteFunc(imageTags(s), func(e coretest.Entry) bool { return e.Tag == tag })
	return build(order, &coretest.IFD{Entries: tags}, nil, s.parts)
}

func TestMalformed(t *testing.T) {
	spec := plainSpec(1)
	small := func(mutate func([]byte, binary.ByteOrder)) []byte {
		data := singlePage(littleEndian, spec)
		mutate(data, littleEndian)
		return data
	}
	tagSlot := func(data []byte, order binary.ByteOrder, tag uint16) int {
		off := int(order.Uint32(data[4:]))
		for i := range int(order.Uint16(data[off:])) {
			at := off + 2 + i*entrySize
			if order.Uint16(data[at:]) == tag {
				return at
			}
		}
		t.Fatalf("no tag %d", tag)
		return 0
	}
	tiled := imageSpec{width: 4, height: 4, tileWidth: 4, tileHeight: 4, parts: [][]byte{pixels(1, 16)}}
	for name, data := range map[string][]byte{
		"truncated header":       []byte("II*\x00\x08\x00"),
		"IFD offset into header": small(func(d []byte, o binary.ByteOrder) { o.PutUint32(d[4:], 2) }),
		"IFD offset past the end": small(func(d []byte, o binary.ByteOrder) {
			o.PutUint32(d[4:], 1<<30)
		}),
		"empty directory": func() []byte {
			d := append([]byte("II*\x00\x08\x00\x00\x00"), make([]byte, 6)...)
			return d
		}(),
		"directory runs off the end": small(func(d []byte, o binary.ByteOrder) {
			o.PutUint16(d[o.Uint32(d[4:]):], 2000)
		}),
		"directory cycle": small(func(d []byte, o binary.ByteOrder) {
			off := o.Uint32(d[4:])
			n := o.Uint16(d[off:])
			o.PutUint32(d[int(off)+2+int(n)*entrySize:], off)
		}),
		"next pointer into header": small(func(d []byte, o binary.ByteOrder) {
			off := o.Uint32(d[4:])
			n := o.Uint16(d[off:])
			o.PutUint32(d[int(off)+2+int(n)*entrySize:], 4)
		}),
		"repeated tag": small(func(d []byte, o binary.ByteOrder) {
			at := tagSlot(d, o, tagCompression)
			o.PutUint16(d[at:], tagBitsPerSample)
		}),
		"strip past the end": small(func(d []byte, o binary.ByteOrder) {
			// Point the first of the two strips near the end of the file.
			table := o.Uint32(d[tagSlot(d, o, tagStripOffsets)+8:])
			o.PutUint32(d[table:], uint32(len(d)-1))
		}),
		"strip offset overflow": small(func(d []byte, o binary.ByteOrder) {
			table := o.Uint32(d[tagSlot(d, o, tagStripOffsets)+8:])
			o.PutUint32(d[table:], 0xFFFFFFFF)
		}),
		"counts disagree": small(func(d []byte, o binary.ByteOrder) {
			o.PutUint32(d[tagSlot(d, o, tagStripByteCounts)+4:], 1)
		}),
		"more strips than the image has": replaceTag(littleEndian, spec, tagRowsPerStrip, coretest.Entry{Value: uint32(4)}),
		"fewer strips than the image has": replaceTag(littleEndian, spec, tagRowsPerStrip, coretest.Entry{
			Value: uint32(1),
		}),
		"both strips and tiles": singlePage(littleEndian, tiled, coretest.Entry{Tag: tagStripOffsets, Value: uint32(8)}, coretest.Entry{Tag: tagStripByteCounts, Value: uint32(1)}),
		"no image data":         withoutTag(littleEndian, spec, tagStripOffsets),
		"no byte counts":        withoutTag(littleEndian, spec, tagStripByteCounts),
		"tiles lack a width":    withoutTag(littleEndian, tiled, tagTileWidth),
		"tiles lack a length":   withoutTag(littleEndian, tiled, tagTileLength),
		"no width":              withoutTag(littleEndian, spec, tagImageWidth),
		"no height":             withoutTag(littleEndian, spec, tagImageLength),
		"no photometric":        withoutTag(littleEndian, spec, tagPhotometric),
		"zero width":            replaceTag(littleEndian, spec, tagImageWidth, coretest.Entry{Value: uint32(0)}),
		"zero samples":          replaceTag(littleEndian, spec, tagSamplesPerPixel, coretest.Entry{Value: uint16(0)}),
		"too many samples":      replaceTag(littleEndian, spec, tagSamplesPerPixel, coretest.Entry{Value: uint16(5000)}),
		"zero rows per strip":   replaceTag(littleEndian, spec, tagRowsPerStrip, coretest.Entry{Value: uint32(0)}),
		"bad planar value":      replaceTag(littleEndian, spec, tagPlanarConfig, coretest.Entry{Value: uint16(3)}),
		"zero tile width":       replaceTag(littleEndian, tiled, tagTileWidth, coretest.Entry{Value: uint32(0)}),
		"width is not a number": replaceTag(littleEndian, spec, tagImageWidth, coretest.Entry{Value: "abc"}),
		"bit depth is text":     replaceTag(littleEndian, spec, tagBitsPerSample, coretest.Entry{Value: "abcdefg"}),
		"zero bit depth":        replaceTag(littleEndian, spec, tagBitsPerSample, coretest.Entry{Value: uint16(0)}),
		"bit depth too large":   replaceTag(littleEndian, spec, tagBitsPerSample, coretest.Entry{Value: uint16(65)}),
		"wrong bit depth count": replaceTag(littleEndian, spec, tagBitsPerSample, coretest.Entry{Value: []uint16{8, 8}}),
		// Tags that keep their place in the allowlist only in their proper form,
		// because a free-form value on a kept tag would survive cleaning.
		"resolution as text":        singlePage(littleEndian, spec, coretest.Entry{Tag: tagXResolution, Value: "Test Author"}),
		"fill order holds an array": singlePage(littleEndian, spec, coretest.Entry{Tag: tagFillOrder, Value: []uint16{1, 2, 3, 4, 5}}),
		"white point count":         singlePage(littleEndian, spec, coretest.Entry{Tag: tagWhitePoint, Value: [][2]uint32{{1, 1}}}),
		"extra samples count":       singlePage(littleEndian, spec, coretest.Entry{Tag: tagExtraSamples, Value: []uint16{1, 2, 3, 4}}),
		"dot range count":           singlePage(littleEndian, spec, coretest.Entry{Tag: tagDotRange, Value: []uint16{1, 2, 3}}),
		"kept value past the end": func() []byte {
			data := singlePage(littleEndian, spec)
			o := littleEndian
			off := o.Uint32(data[4:])
			// Claim the Y resolution lies beyond the end of the file.
			for i := range int(o.Uint16(data[off:])) {
				at := int(off) + 2 + i*entrySize
				if o.Uint16(data[at:]) == tagYResolution {
					o.PutUint32(data[at+8:], uint32(len(data)))
				}
			}
			return data
		}(),
		// Well-formed tables, so that it is their being here that gets them refused.
		"JPEG tables on an uncompressed page": singlePage(littleEndian, spec, coretest.Entry{Tag: tagJPEGTables, Value: []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerDQT, 0, 3, 0, 0xFF, jpegmeta.MarkerEOI}}),
		"image overlaps the directory": small(func(d []byte, o binary.ByteOrder) {
			table := o.Uint32(d[tagSlot(d, o, tagStripOffsets)+8:])
			o.PutUint32(d[table:], o.Uint32(d[4:]))
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrMalformed) {
				t.Errorf("Inspect: got %v, want ErrMalformed", err)
			}
		})
	}
}

// A real JPEG stream reduced to one that breaks a rule of the marker walk.
func TestMalformedJPEGStrips(t *testing.T) {
	valid := realJPEG(t, 1)
	for name, strip := range map[string][]byte{
		"no SOI":             []byte("not a JPEG stream"),
		"one byte":           {0xFF},
		"only SOI":           {0xFF, jpegmeta.MarkerSOI},
		"no EOI":             valid[:len(valid)-2],
		"ends after a FF":    append(append([]byte{}, valid[:len(valid)-2]...), 0xFF),
		"nested SOI":         {0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerEOI},
		"marker zero":        {0xFF, jpegmeta.MarkerSOI, 0xFF, 0x00, 0xFF, jpegmeta.MarkerEOI},
		"not a marker":       {0xFF, jpegmeta.MarkerSOI, 0x12, 0x34, 0xFF, jpegmeta.MarkerEOI},
		"short segment":      {0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerDQT, 0, 1, 0xFF, jpegmeta.MarkerEOI},
		"segment runs out":   {0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerDQT, 0x40, 0, 0xFF, jpegmeta.MarkerEOI},
		"length cut off":     {0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerDQT},
		"half a length":      {0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerDQT, 0},
		"unknown marker":     {0xFF, jpegmeta.MarkerSOI, 0xFF, 0xF0, 0, 2, 0xFF, jpegmeta.MarkerEOI},
		"JPG extension code": {0xFF, jpegmeta.MarkerSOI, 0xFF, 0xC8, 0, 2, 0xFF, jpegmeta.MarkerEOI},
		"fill then nothing":  {0xFF, jpegmeta.MarkerSOI, 0xFF, 0xFF},
	} {
		t.Run(name, func(t *testing.T) {
			page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{strip}}
			if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(singlePage(littleEndian, page)), core.Options{}); !errors.Is(err, core.ErrMalformed) {
				t.Errorf("got %v, want ErrMalformed", err)
			}
		})
	}
}

// Fill bytes, restart markers and stuffed bytes are all legal in places that a
// naive search for "the next marker" would stumble over.
func TestJPEGWalkHandlesEntropyDetails(t *testing.T) {
	strip := slices.Concat(
		[]byte{0xFF, jpegmeta.MarkerSOI},
		[]byte{0xFF}, jpegSegment(jpegmeta.MarkerDQT, make([]byte, 65)), // fill bytes before a marker
		[]byte{0xFF, jpegmeta.MarkerTEM},
		[]byte{0xFF, 0xD3}, // a stray restart marker between segments
		jpegSegment(jpegmeta.MarkerSOS, []byte{1, 1, 0, 0, 63, 0}),
		[]byte{1, 2, 0xFF, 0x00, 3, 0xFF, 0xD0, 4, 0xFF}, // stuffing, a restart, then a fill byte
		jpegSegment(jpegmeta.MarkerDNL, []byte{0, 16}),
		[]byte{0xFF, jpegmeta.MarkerEOI},
	)
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{strip}}
	out, _ := clean(t, singlePage(littleEndian, page), core.Options{})
	if !bytes.Contains(out, strip) {
		t.Error("the strip did not survive unchanged")
	}
}

func TestTooManyJPEGSegments(t *testing.T) {
	var segments []byte
	for range maxJPEGCuts + 1 {
		segments = append(segments, 0xFF, jpegmeta.MarkerCOM, 0, 2)
	}
	strip := append(append([]byte{0xFF, jpegmeta.MarkerSOI}, segments...), 0xFF, jpegmeta.MarkerEOI)
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{strip}}
	if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(singlePage(littleEndian, page)), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestUncompressedStripTailIsDropped(t *testing.T) {
	secret := []byte("Test Strip Tail")
	s := imageSpec{
		width: 4, height: 2, samples: 3, rowsPerStrip: 2,
		parts: [][]byte{append(pixels(1, 24), secret...)},
	}
	data := singlePage(bigEndian, s)
	out, rep := clean(t, data, core.Options{})
	if bytes.Contains(out, secret) {
		t.Error("data after the pixels of an uncompressed strip survived")
	}
	if !bytes.Contains(out, pixels(1, 24)) {
		t.Error("the pixels themselves did not survive")
	}
	if findingNamed(rep, "Unreferenced data") == nil {
		t.Errorf("the tail was not reported: %+v", rep.Findings)
	}
}

// The last strip of a page is shorter than the rest, so a strip size worked out
// from RowsPerStrip alone would keep a tail there. Tiles have no short last row.
func TestUncompressedSizes(t *testing.T) {
	last := imageSpec{
		width: 2, height: 3, rowsPerStrip: 2,
		parts: [][]byte{pixels(1, 4), append(pixels(2, 2), "Test Tail"...)},
	}
	if out, _ := clean(t, singlePage(littleEndian, last), core.Options{}); bytes.Contains(out, []byte("Test Tail")) {
		t.Error("data after the short last strip survived")
	}
	// Short strips stay as they are: losing the tail of a truncated image would be
	// the handler's fault, but keeping what is there is not.
	short := imageSpec{width: 4, height: 2, samples: 3, rowsPerStrip: 2, parts: [][]byte{pixels(1, 10)}}
	if out, _ := clean(t, singlePage(littleEndian, short), core.Options{}); !bytes.Contains(out, pixels(1, 10)) {
		t.Error("a strip shorter than expected lost bytes")
	}
	// Mixed bit depths in a planar file: each plane has its own row size.
	mixed := imageSpec{width: 4, height: 1, samples: 2, planar: 2, rowsPerStrip: 1, parts: [][]byte{pixels(1, 4), append(pixels(2, 8), "Test Tail"...)}}
	tags := imageTags(mixed)
	tags[slices.IndexFunc(tags, func(e coretest.Entry) bool { return e.Tag == tagBitsPerSample })].Value = []uint16{8, 16}
	out, _ := clean(t, build(littleEndian, &coretest.IFD{Entries: tags}, nil, mixed.parts), core.Options{})
	if bytes.Contains(out, []byte("Test Tail")) || !bytes.Contains(out, pixels(2, 8)) {
		t.Error("planes with different depths were trimmed to the wrong size")
	}
	// A strip of subsampled YCbCr that fits is kept whole. TestUncompressedYCbCrStripsAreChecked
	// covers one that does not.
	ycbcr := imageSpec{width: 2, height: 2, samples: 3, photometric: photometricYCbCr, rowsPerStrip: 2, parts: [][]byte{pixels(1, 6)}}
	if out, _ := clean(t, singlePage(littleEndian, ycbcr), core.Options{}); !bytes.Contains(out, pixels(1, 6)) {
		t.Error("YCbCr data was trimmed")
	}
	// Tiled images keep whole tiles, even where the picture ends inside one.
	tile := imageSpec{width: 2, height: 2, tileWidth: 4, tileHeight: 4, parts: [][]byte{append(pixels(3, 16), "Test Tail"...)}}
	out, _ = clean(t, singlePage(littleEndian, tile), core.Options{})
	if bytes.Contains(out, []byte("Test Tail")) || !bytes.Contains(out, pixels(3, 16)) {
		t.Error("a tile was trimmed to the wrong size")
	}
}

func TestSharedStripsCannotFillTheDisk(t *testing.T) {
	// A tall image in which every strip points at the same megabyte, in a file that
	// is hardly bigger than that megabyte.
	const strips = 1 << 11
	big := pixels(1, 1<<20)
	offsets := make([]uint32, strips)
	sizes := make([]uint32, strips)
	root := &coretest.IFD{Entries: []coretest.Entry{
		{Tag: tagImageWidth, Value: uint32(1 << 20)},
		{Tag: tagImageLength, Value: uint32(strips)},
		{Tag: tagBitsPerSample, Value: uint16(8)},
		{Tag: tagCompression, Value: uint16(compressionNone)},
		{Tag: tagPhotometric, Value: uint16(1)},
		{Tag: tagRowsPerStrip, Value: uint32(1)},
		{Tag: tagStripOffsets, Value: offsets},
		{Tag: tagStripByteCounts, Value: sizes},
	}}
	data := root.Encode(littleEndian)
	at := uint32(len(data))
	data = append(data, big...)
	off := binary.LittleEndian.Uint32(data[4:])
	for i := range int(binary.LittleEndian.Uint16(data[off:])) {
		e := data[int(off)+2+i*entrySize:]
		table := data[binary.LittleEndian.Uint32(e[8:]):]
		switch binary.LittleEndian.Uint16(e) {
		case tagStripOffsets:
			for j := range strips {
				binary.LittleEndian.PutUint32(table[4*j:], at)
			}
		case tagStripByteCounts:
			for j := range strips {
				binary.LittleEndian.PutUint32(table[4*j:], 1<<20)
			}
		}
	}
	if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestCaps(t *testing.T) {
	t.Run("entries in one directory", func(t *testing.T) {
		data := []byte("II*\x00\x08\x00\x00\x00")
		data = binary.LittleEndian.AppendUint16(data, maxEntriesPerIFD+1)
		data = append(data, make([]byte, (maxEntriesPerIFD+1)*entrySize+4)...)
		if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
	// A chain of tiny directories, one after another, each pointing to the next.
	chain := func(pages, entriesEach int) []byte {
		data := []byte("II*\x00\x08\x00\x00\x00")
		for i := range pages {
			data = binary.LittleEndian.AppendUint16(data, uint16(entriesEach))
			for tag := range entriesEach {
				data = binary.LittleEndian.AppendUint16(data, uint16(1000+tag))
				data = binary.LittleEndian.AppendUint16(data, typeShort)
				data = binary.LittleEndian.AppendUint32(data, 1)
				data = binary.LittleEndian.AppendUint32(data, 1)
			}
			next := uint32(len(data) + 4)
			if i == pages-1 {
				next = 0
			}
			data = binary.LittleEndian.AppendUint32(data, next)
		}
		return data
	}
	t.Run("pages", func(t *testing.T) {
		if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(chain(maxPages+1, 1)), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
	t.Run("entries in total", func(t *testing.T) {
		if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(chain(maxTotalEntries/maxEntriesPerIFD+1, maxEntriesPerIFD)), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
	t.Run("strips", func(t *testing.T) {
		p := parser{strips: maxStrips - 1}
		if !p.chargeStrips(1) {
			t.Error("the last permitted strip was refused")
		}
		if p.chargeStrips(1) {
			t.Error("a strip over the cap was accepted")
		}
	})
	t.Run("image dimensions that overflow", func(t *testing.T) {
		g := geometry{width: 0xFFFFFFFF, height: 0xFFFFFFFF, samples: maxSamples, planar: 2, tiled: true, tileWidth: 1, tileHeight: 1, bits: []uint32{64}}
		if got := g.partCount(); got != 1<<64-1 {
			t.Errorf("partCount = %d, want saturation", got)
		}
	})
	t.Run("strips in one file", func(t *testing.T) {
		s := imageSpec{width: 1, height: 2, rowsPerStrip: 1, parts: [][]byte{{1}, {2}}}
		data := singlePage(littleEndian, s)
		p := parser{
			ctx: context.Background(), src: core.BytesSource(data), order: littleEndian,
			report: &core.Report{}, seen: make(map[string]bool), strips: maxStrips - 1,
		}
		dir, _, err := p.readPageDirectory(littleEndian.Uint32(data[4:]))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.buildPage(dir, "page 1"); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
}

// A writer may store strip offsets as SHORT, as this file's two 70000-byte strips
// allow because both of its offsets fit. Duplicated in the output, the second one
// no longer fits, so the table has to widen to LONG for the file to stay readable.
func TestShortOffsetsBecomeLong(t *testing.T) {
	strip := pixels(1, 70000)
	s := imageSpec{width: 70000, height: 2, rowsPerStrip: 1, parts: [][]byte{strip, strip}}
	data := (&coretest.IFD{Entries: imageTags(s)}).Encode(littleEndian)
	at := uint16(len(data))
	data = append(data, strip...)
	off := binary.LittleEndian.Uint32(data[4:])
	for i := range int(binary.LittleEndian.Uint16(data[off:])) {
		e := data[int(off)+2+i*entrySize:]
		if binary.LittleEndian.Uint16(e) == tagStripOffsets {
			binary.LittleEndian.PutUint16(e[2:], typeShort)
			binary.LittleEndian.PutUint16(e[8:], at)
			binary.LittleEndian.PutUint16(e[10:], at)
		}
	}
	out, _ := clean(t, data, core.Options{})
	pages, err := readPages(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || !bytes.Equal(pages[0].pix, append(bytes.Clone(strip), strip...)) {
		t.Error("strips were lost in converting offsets to LONG")
	}
}

func TestRenderingTagsSurvive(t *testing.T) {
	wanted := []coretest.Entry{
		{Tag: tagT4Options, Value: uint32(1)},
		{Tag: tagT6Options, Value: uint32(0)},
		{Tag: tagYCbCrCoeffs, Value: [][2]uint32{{299, 1000}, {587, 1000}, {114, 1000}}},
		{Tag: tagInkSet, Value: uint16(2)},
		{Tag: tagNumberOfInks, Value: uint16(5)},
		{Tag: tagDotRange, Value: []uint16{0, 255}},
		{Tag: tagMaxSampleValue, Value: []uint16{255, 255, 255}},
		{Tag: tagMinSampleValue, Value: uint16(0)},
		{Tag: tagSampleFormat, Value: []uint16{1, 1, 1}},
		{Tag: tagSMinSampleValue, Value: uint16(0)},
		{Tag: tagSMaxSampleValue, Value: uint16(255)},
		{Tag: tagThresholding, Value: uint16(1)},
		{Tag: tagFillOrder, Value: uint16(1)},
		{Tag: tagPredictor, Value: uint16(1)},
		{Tag: tagWhitePoint, Value: [][2]uint32{{3127, 10000}, {3290, 10000}}},
		{Tag: tagPrimaries, Value: [][2]uint32{{1, 2}, {1, 3}, {1, 4}, {1, 5}, {1, 6}, {1, 7}}},
		{Tag: tagYCbCrSubSamp, Value: []uint16{2, 2}},
		{Tag: tagYCbCrPosition, Value: uint16(1)},
		{Tag: tagRefBlackWhite, Value: [][2]uint32{{0, 1}, {255, 1}, {128, 1}, {255, 1}, {128, 1}, {255, 1}}},
		{Tag: tagExtraSamples, Value: []uint16{}},
		{Tag: tagNewSubfileType, Value: uint32(2)},
	}
	data := singlePage(littleEndian, plainSpec(1), wanted...)
	out, rep := clean(t, data, core.Options{})
	if len(rep.Findings) != 0 {
		t.Errorf("rendering tags reported as metadata: %+v", rep.Findings)
	}
	in, err := parse(context.Background(), core.BytesSource(out), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	present := make(map[uint16]bool)
	for _, f := range in.pages[0].fields {
		present[f.tag] = true
	}
	for _, e := range wanted {
		if !present[e.Tag] {
			t.Errorf("tag %d did not survive", e.Tag)
		}
	}
}

func TestColorMapKept(t *testing.T) {
	colorMap := make([]uint16, 3*4)
	for i := range colorMap {
		colorMap[i] = uint16(0x1000 + i)
	}
	s := imageSpec{width: 4, height: 2, rowsPerStrip: 2, photometric: 3, parts: [][]byte{pixels(1, 2)}}
	tags := imageTags(s)
	tags[slices.IndexFunc(tags, func(e coretest.Entry) bool { return e.Tag == tagBitsPerSample })].Value = uint16(2)
	tags = append(tags, coretest.Entry{Tag: tagColorMap, Value: colorMap})
	out, rep := clean(t, build(littleEndian, &coretest.IFD{Entries: tags}, nil, s.parts), core.Options{})
	if len(rep.Findings) != 0 {
		t.Errorf("a palette was reported as metadata: %+v", rep.Findings)
	}
	raw := make([]byte, 0, 2*len(colorMap))
	for _, v := range colorMap {
		raw = binary.LittleEndian.AppendUint16(raw, v)
	}
	if !bytes.Contains(out, raw) {
		t.Error("the palette did not survive")
	}
}

// A two-byte value leaves two bytes of its entry unused, a natural spot to hide
// something. Writing the output from values rather than from the raw entries is
// what clears them.
func TestUnusedValueBytesAreCleared(t *testing.T) {
	data := singlePage(littleEndian, plainSpec(1))
	off := binary.LittleEndian.Uint32(data[4:])
	for i := range int(binary.LittleEndian.Uint16(data[off:])) {
		e := data[int(off)+2+i*entrySize:]
		if binary.LittleEndian.Uint16(e) == tagResolutionUnit {
			copy(e[10:], "HI")
		}
	}
	out, _ := clean(t, data, core.Options{})
	if bytes.Contains(out, []byte("HI")) {
		t.Error("bytes after an inline value survived")
	}
}

func TestOrientation(t *testing.T) {
	report := func(value any, opts core.Options) (*core.Report, []byte) {
		data := singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagOrientation, Value: value})
		out, rep := clean(t, data, opts)
		return rep, out
	}
	t.Run("kept unless strict", func(t *testing.T) {
		rep, out := report(uint16(6), core.Options{})
		f := findingNamed(rep, "Orientation")
		if f == nil || f.Action != core.ActionRetain || !strings.Contains(f.Value, "90") {
			t.Fatalf("got %+v", f)
		}
		kept, _ := clean(t, out, core.Options{Strict: true})
		if bytes.Equal(kept, out) {
			t.Error("strict mode did not remove a retained orientation")
		}
		rep, _ = report(uint16(6), core.Options{Strict: true})
		if f := findingNamed(rep, "Orientation"); f == nil || f.Action != core.ActionRemove {
			t.Errorf("strict: got %+v", f)
		}
	})
	t.Run("a stored LONG is normalised", func(t *testing.T) {
		rep, out := report(uint32(3), core.Options{})
		if f := findingNamed(rep, "Orientation"); f == nil || f.Action != core.ActionRetain {
			t.Fatalf("got %+v", f)
		}
		doc, err := parse(context.Background(), core.BytesSource(out), core.Options{})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(doc.pages[0].fields, func(f field) bool { return f.tag == tagOrientation })
		if i < 0 || doc.pages[0].fields[i].typ != typeShort {
			t.Errorf("orientation stored as %+v", doc.pages[0].fields)
		}
	})
	for name, value := range map[string]any{
		"value too large": uint16(9),
		"value zero":      uint16(0),
		"an array":        []uint16{1, 2, 3, 4},
		"text":            "Test Hidden",
	} {
		t.Run(name, func(t *testing.T) {
			rep, out := report(value, core.Options{})
			if f := findingNamed(rep, "Orientation"); f == nil || f.Action != core.ActionRemove {
				t.Errorf("got %+v", f)
			}
			if bytes.Contains(out, []byte("Test Hidden")) {
				t.Error("a bad orientation's value survived")
			}
		})
	}
}

func TestICCProfile(t *testing.T) {
	t.Run("a bad profile is removed, not retained", func(t *testing.T) {
		for name, entry := range map[string]coretest.Entry{
			"no signature":   {Tag: tagICCProfile, Value: bytes.Repeat([]byte("Test Author "), 20)},
			"too short":      {Tag: tagICCProfile, Value: []byte("Test Author")},
			"wrong type":     {Tag: tagICCProfile, Value: strings.Repeat("Test Author ", 20)},
			"past the end":   {Tag: tagICCProfile, Type: typeByte, Count: 1 << 24, Value: fakeICC()},
			"stored inline":  {Tag: tagICCProfile, Value: []byte("Test")},
			"unknown type":   {Tag: tagICCProfile, Type: 99, Value: fakeICC()},
			"signature cold": {Tag: tagICCProfile, Value: make([]byte, 128)},
		} {
			t.Run(name, func(t *testing.T) {
				out, rep := clean(t, singlePage(littleEndian, plainSpec(1), entry), core.Options{})
				f := findingNamed(rep, "ICC colour profile")
				if f == nil || f.Action != core.ActionRemove {
					t.Fatalf("got %+v", f)
				}
				if bytes.Contains(out, []byte("Test Author")) {
					t.Error("the fake profile's contents survived")
				}
			})
		}
	})
	t.Run("a BYTE profile is still a profile", func(t *testing.T) {
		out, rep := clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagICCProfile, Type: typeByte, Value: fakeICC()}), core.Options{})
		if f := findingNamed(rep, "ICC colour profile"); f == nil || f.Action != core.ActionRetain {
			t.Fatalf("got %+v", f)
		}
		if !bytes.Contains(out, fakeICC()) {
			t.Error("the profile did not survive")
		}
	})
}

func TestThumbnailDirectoryIsDropped(t *testing.T) {
	root := coretest.FakeIFD(0)
	root.Entries = append(root.Entries, imageTags(plainSpec(0x33))...)
	data := build(bigEndian, root, nil, plainSpec(0x33).parts)
	out, rep := clean(t, data, core.Options{})
	if f := findingNamed(rep, "Embedded thumbnail"); f == nil || f.Risk != core.RiskHigh || !strings.Contains(f.Value, "JPEG") {
		t.Errorf("thumbnail finding: %+v", f)
	}
	if bytes.Contains(out, []byte("Test Thumbnail")) {
		t.Error("the thumbnail survived")
	}
	pages, err := readPages(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Errorf("%d pages after dropping the thumbnail directory, want 1", len(pages))
	}
}

func TestPreviewOnlyFileIsKept(t *testing.T) {
	// With no full page to prefer, the only pages there are count as the picture.
	s := plainSpec(1)
	data := singlePage(littleEndian, s, coretest.Entry{Tag: tagNewSubfileType, Value: uint32(1)})
	out, rep := clean(t, data, core.Options{})
	if findingNamed(rep, "Embedded thumbnail") != nil {
		t.Error("the only page was treated as a thumbnail")
	}
	if !bytes.Contains(out, pixels(1, 24)) {
		t.Error("the only page was lost")
	}
}

func TestOldSubfileTypeMarksPreview(t *testing.T) {
	full := plainSpec(1)
	preview := imageSpec{width: 1, height: 1, rowsPerStrip: 1, parts: [][]byte{[]byte("X")}}
	root := &coretest.IFD{
		Entries: imageTags(full),
		Next: &coretest.IFD{
			Entries: append(imageTags(preview), coretest.Entry{Tag: tagSubfileType, Value: uint16(2)}),
		},
	}
	_, rep := clean(t, build(littleEndian, root, nil, full.parts, preview.parts), core.Options{})
	if f := findingNamed(rep, "Embedded thumbnail"); f == nil || !strings.Contains(f.Value, "reduced-resolution") {
		t.Errorf("got %+v", f)
	}
}

func TestMoreHostilePreviews(t *testing.T) {
	// Nothing about a dropped page has to make sense, since none of it is kept.
	full := plainSpec(1)
	for name, next := range map[string]*coretest.IFD{
		"thumbnail without a length": {Entries: []coretest.Entry{{Tag: tagJPEGThumbnail, Value: uint32(8)}}},
		"thumbnail with a bad length": {Entries: []coretest.Entry{
			{Tag: tagJPEGThumbnail, Value: uint32(8)},
			{Tag: tagJPEGThumbLength, Value: "not a number"},
		}},
		"thumbnail pointer is text": {Entries: []coretest.Entry{{Tag: tagJPEGThumbnail, Value: "abcdefgh"}}},
		"tables disagree": {Entries: []coretest.Entry{
			{Tag: tagNewSubfileType, Value: uint32(1)},
			{Tag: tagStripOffsets, Value: []uint32{1, 2}},
			{Tag: tagStripByteCounts, Value: uint32(1)},
		}},
		"offsets are text": {Entries: []coretest.Entry{
			{Tag: tagNewSubfileType, Value: uint32(1)},
			{Tag: tagStripOffsets, Value: "abcdefgh"},
			{Tag: tagStripByteCounts, Value: "abcdefgh"},
		}},
		"counts are text": {Entries: []coretest.Entry{
			{Tag: tagNewSubfileType, Value: uint32(1)},
			{Tag: tagStripOffsets, Value: uint32(8)},
			{Tag: tagStripByteCounts, Value: "a"},
		}},
		"counts run off the end": {Entries: []coretest.Entry{
			{Tag: tagNewSubfileType, Value: uint32(1)},
			{Tag: tagStripOffsets, Value: []uint32{8, 8}},
			{Tag: tagStripByteCounts, Type: typeLong, Value: []uint32{8, 8}},
			{Tag: tagOrientation, Value: uint16(1)},
			{Tag: tagICCProfile, Value: []byte("Test Removed With The Page")},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := &coretest.IFD{Entries: imageTags(full), Next: next}
			out, rep := clean(t, build(littleEndian, root, nil, full.parts), core.Options{})
			if findingNamed(rep, "Embedded thumbnail") == nil {
				t.Errorf("no thumbnail finding: %+v", rep.Findings)
			}
			if !bytes.Contains(out, pixels(1, 24)) {
				t.Error("the full page was damaged")
			}
			if bytes.Contains(out, []byte("Test Removed")) {
				t.Error("a kept tag of the dropped page survived")
			}
		})
	}
}

// exif.Data must read a real block, not the file, or every later page of a
// multi-page scan would be reported as a thumbnail.
func TestPagesAreNotReportedAsThumbnails(t *testing.T) {
	root := &coretest.IFD{
		Entries: append(imageTags(plainSpec(1)), coretest.Entry{Tag: 0x010F, Value: "Test Make"}),
		Next: &coretest.IFD{
			Entries: append(imageTags(plainSpec(2)), coretest.Entry{Tag: 0x010F, Value: "Test Make"}),
		},
	}
	_, rep := clean(t, build(littleEndian, root, nil, plainSpec(1).parts, plainSpec(2).parts), core.Options{})
	if f := findingNamed(rep, "Embedded thumbnail"); f != nil {
		t.Errorf("a real page was reported as a thumbnail: %+v", f)
	}
	count := 0
	for _, f := range rep.Findings {
		if f.Field == "Camera make" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the same make on each page was reported %d times, want once", count)
	}
}

func TestGPSDecimal(t *testing.T) {
	for _, order := range []binary.ByteOrder{littleEndian, bigEndian} {
		_, rep := clean(t, loaded(order), core.Options{})
		f := findingNamed(rep, "GPS coordinates")
		if f == nil || f.Value != coretest.EiffelTower || !strings.Contains(f.Where, "GPS") {
			t.Errorf("%v: GPS finding: %+v", order, f)
		}
	}
}

func TestPhotoshopInsidesAreReported(t *testing.T) {
	_, rep := clean(t, loaded(littleEndian), core.Options{})
	var coordinates, thumbnail, city, writer bool
	for _, f := range rep.Findings {
		switch {
		case f.Field == "GPS coordinates" && strings.HasPrefix(f.Value, "-33.5"):
			coordinates = true
		case f.Field == "Embedded thumbnail" && strings.HasPrefix(f.Value, "Photoshop"):
			thumbnail = true
		case f.Field == "City" && f.Value == "Test IRB City":
			city = true
		case f.Field == "IPTC writer" && strings.HasPrefix(f.Value, "Test IRB Writer"):
			writer = true
		}
	}
	if !coordinates || !thumbnail || !city || !writer {
		t.Errorf("EXIF %v, thumbnail %v, XMP %v, IPTC %v in %+v", coordinates, thumbnail, city, writer, rep.Findings)
	}
}

func TestIPTCStoredAsLong(t *testing.T) {
	stream := fakeIPTC()
	stream = append(stream, make([]byte, (4-len(stream)%4)%4)...)
	var swapped []byte
	for i := 0; i+4 <= len(stream); i += 4 {
		swapped = binary.LittleEndian.AppendUint32(swapped, binary.BigEndian.Uint32(stream[i:]))
	}
	_, rep := clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagIPTC, Type: typeLong, Count: uint32(len(swapped) / 4), Value: swapped}), core.Options{})
	if f := findingNamed(rep, "IPTC by-line"); f == nil || f.Value != "Test IPTC Author" {
		t.Errorf("IPTC stored swapped as LONG: %+v in %+v", f, rep.Findings)
	}
	_, rep = clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagIPTC, Type: typeLong, Count: uint32(len(stream) / 4), Value: stream}), core.Options{})
	if f := findingNamed(rep, "IPTC by-line"); f == nil || f.Value != "Test IPTC Author" {
		t.Errorf("IPTC stored unswapped as LONG: %+v in %+v", f, rep.Findings)
	}
}

// Damaged metadata must be removed and reported, not refused: nothing in it stays,
// and refusing would leave the user with no clean file at all.
func TestDamagedMetadataIsStillRemoved(t *testing.T) {
	longText := strings.Repeat("Test Long ", 8000)
	for name, tc := range map[string]struct {
		entries []coretest.Entry
		want    string
	}{
		"Exif pointer past the end":   {[]coretest.Entry{{Tag: tagExifIFD, Value: uint32(1 << 30)}}, "Damaged EXIF block"},
		"Exif pointer is text":        {[]coretest.Entry{{Tag: tagExifIFD, Value: "abc"}}, "Damaged EXIF block"},
		"GPS pointer past the end":    {[]coretest.Entry{{Tag: tagGPSIFD, Value: uint32(1 << 30)}}, "Damaged EXIF block"},
		"zero Exif pointer":           {[]coretest.Entry{{Tag: tagExifIFD, Value: uint32(0)}}, "EXIF block"},
		"Artist runs off the end":     {[]coretest.Entry{{Tag: 0x013B, Type: typeASCII, Count: 1 << 30, Value: "Test Author"}}, "Damaged EXIF block"},
		"XMP runs off the end":        {[]coretest.Entry{{Tag: tagXMP, Type: typeByte, Count: 1 << 30, Value: []byte("<x:xmpmeta>")}}, "Embedded XMP"},
		"XMP is garbage":              {[]coretest.Entry{{Tag: tagXMP, Type: typeByte, Value: []byte("not a packet")}}, "XMP packet"},
		"IPTC runs off the end":       {[]coretest.Entry{{Tag: tagIPTC, Type: typeByte, Count: 1 << 30, Value: []byte("xxxxxxxx")}}, "Embedded IPTC"},
		"IPTC is garbage":             {[]coretest.Entry{{Tag: tagIPTC, Type: typeByte, Value: []byte("not a record")}}, "Embedded IPTC"},
		"IPTC length overruns":        {[]coretest.Entry{{Tag: tagIPTC, Type: typeByte, Value: []byte{jpegmeta.IPTCTagMarker, 2, 80, 0xFF, 0xFF, 'x', 'y', 'z'}}}, "Embedded IPTC"},
		"IPTC truncated record":       {[]coretest.Entry{{Tag: tagIPTC, Type: typeByte, Value: []byte{jpegmeta.IPTCTagMarker, 2, 80, 0, 200, 'x', 'y', 'z'}}}, "Embedded IPTC"},
		"Photoshop runs off the end":  {[]coretest.Entry{{Tag: tagPhotoshop, Type: typeByte, Count: 1 << 30, Value: []byte("xxxxxxxx")}}, "Embedded Photoshop data"},
		"Photoshop name overruns":     {[]coretest.Entry{{Tag: tagPhotoshop, Type: typeByte, Value: []byte("8BIM\x04\x04\xFFabcdef")}}, "Embedded Photoshop data"},
		"Photoshop size overruns":     {[]coretest.Entry{{Tag: tagPhotoshop, Type: typeByte, Value: append([]byte("8BIM\x04\x04\x00\x00\xFF\xFF\xFF\xFF"), "abc"...)}}, "Embedded Photoshop data"},
		"text tag runs off the end":   {[]coretest.Entry{{Tag: tagPageName, Type: typeASCII, Count: 1 << 30, Value: "Test Page"}}, "Page name"},
		"oversized text tag":          {[]coretest.Entry{{Tag: tagPageName, Value: longText}}, "Page name"},
		"oversized Artist":            {[]coretest.Entry{{Tag: 0x013B, Value: longText}}, "Other EXIF tags"},
		"oversized MakerNote":         {[]coretest.Entry{{Tag: 0x927C, Value: bytes.Repeat([]byte("Test Note "), 8000)}}, "MakerNote"},
		"short NUL-terminated string": {[]coretest.Entry{{Tag: tagPageName, Value: "ab"}}, "Page name"},
		"a tag of unknown type":       {[]coretest.Entry{{Tag: 0x8298, Type: 99, Value: "Test Author"}}, "Other EXIF tags"},
	} {
		t.Run(name, func(t *testing.T) {
			data := singlePage(littleEndian, plainSpec(1), tc.entries...)
			out, rep := clean(t, data, core.Options{})
			if findingNamed(rep, tc.want) == nil {
				t.Errorf("no %q finding in %+v", tc.want, rep.Findings)
			}
			for _, secret := range []string{"Test Author", "Test Long", "Test Note", "Test Page"} {
				if bytes.Contains(out, []byte(secret)) {
					t.Errorf("%q survived", secret)
				}
			}
		})
	}
}

// TestOrdinaryMakerNotesAreNotTakenForRaw keeps the IIQ refusal from catching a TIFF that
// a camera wrote with a maker note of its own, including one that only starts like IIQ's.
func TestOrdinaryMakerNotesAreNotTakenForRaw(t *testing.T) {
	for _, note := range []string{"Nikon\x00\x02\x10", "IIIIabcd", "MMMMabcd", "IIII"} {
		s := plainSpec(1)
		root := &coretest.IFD{
			Entries: imageTags(s),
			Exif:    &coretest.IFD{Entries: []coretest.Entry{{Tag: tagMakerNote, Value: []byte(note)}}},
		}
		_, rep := clean(t, build(littleEndian, root, nil, s.parts), core.Options{})
		if findingNamed(rep, "MakerNote") == nil {
			t.Errorf("%q: no MakerNote finding", note)
		}
	}
}

func TestOversizedMakerNoteSizeIsReal(t *testing.T) {
	note := bytes.Repeat([]byte("n"), maxBlockValue+10)
	_, rep := clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: 0x927C, Value: note}), core.Options{})
	if f := findingNamed(rep, "MakerNote"); f == nil || !strings.Contains(f.Value, "65546") {
		t.Errorf("got %+v", f)
	}
}

func TestSubIFDCycleIsContained(t *testing.T) {
	// The Exif IFD's Interop pointer leads back to the Exif IFD.
	data := singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagExifIFD, Value: uint32(0)})
	subAt := uint32(len(data))
	sub := binary.LittleEndian.AppendUint16(nil, 1)
	sub = binary.LittleEndian.AppendUint16(sub, tagInteropIFD)
	sub = binary.LittleEndian.AppendUint16(sub, typeLong)
	sub = binary.LittleEndian.AppendUint32(sub, 1)
	sub = binary.LittleEndian.AppendUint32(sub, subAt)
	sub = binary.LittleEndian.AppendUint32(sub, 0)
	data = append(data, sub...)
	off := binary.LittleEndian.Uint32(data[4:])
	for i := range int(binary.LittleEndian.Uint16(data[off:])) {
		e := data[int(off)+2+i*entrySize:]
		if binary.LittleEndian.Uint16(e) == tagExifIFD {
			binary.LittleEndian.PutUint32(e[8:], subAt)
		}
	}
	_, rep := clean(t, data, core.Options{})
	if findingNamed(rep, "Damaged EXIF block") == nil {
		t.Errorf("no damage reported: %+v", rep.Findings)
	}
}

func TestReportBudget(t *testing.T) {
	// Past the allowance, reports get less specific and nothing else changes.
	full := loaded(littleEndian)
	p := &parser{
		ctx: context.Background(), src: core.BytesSource(full), order: littleEndian,
		report: &core.Report{Format: formatID}, seen: make(map[string]bool),
	}
	dir, _, err := p.readPageDirectory(littleEndian.Uint32(full[4:]))
	if err != nil {
		t.Fatal(err)
	}
	var removed []entry
	for _, e := range dir.entries {
		if _, kept := allowed[e.tag]; !kept && e.tag != tagOrientation && e.tag != tagICCProfile {
			removed = append(removed, e)
		}
	}
	p.reportRemoved(removed, "page 1")
	if findingNamed(p.report, "GPS coordinates") != nil {
		t.Errorf("GPS was decoded with no allowance left: %+v", p.report.Findings)
	}
	for _, name := range []string{"Embedded XMP", "Embedded IPTC", "Embedded Photoshop data", "Page name"} {
		if findingNamed(p.report, name) == nil {
			t.Errorf("no %q finding with no allowance left: %+v", name, p.report.Findings)
		}
	}
}

func TestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if _, err := (handler{}).Clean(ctx, core.BytesSource(loaded(littleEndian)), &out, core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Clean: got %v, want context.Canceled", err)
	}
	if _, err := (handler{}).Inspect(ctx, core.BytesSource(loaded(littleEndian)), core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Inspect: got %v, want context.Canceled", err)
	}
}

// failAfter is a writer that fails once it has taken n bytes.
type failAfter struct{ n int }

func (w *failAfter) Write(b []byte) (int, error) {
	if len(b) > w.n {
		return 0, errors.New("disk full")
	}
	w.n -= len(b)
	return len(b), nil
}

func TestWriteErrorsSurface(t *testing.T) {
	data := loaded(littleEndian)
	total := func() int {
		out, _ := clean(t, data, core.Options{})
		return len(out)
	}()
	for n := range total {
		if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &failAfter{n: n}, core.Options{}); err == nil {
			t.Fatalf("a writer failing after %d bytes went unnoticed", n)
		}
	}
}

func TestStacksStoredOutsideTheStructure(t *testing.T) {
	refused := func(t *testing.T, data []byte, want string) {
		t.Helper()
		if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want ErrUnsupported mentioning %q", err, want)
		}
	}
	description := func(text string) coretest.Entry {
		return coretest.Entry{Tag: tagImageDescription, Value: text}
	}
	t.Run("ImageJ stack with one directory", func(t *testing.T) {
		refused(t, singlePage(littleEndian, plainSpec(1), description("ImageJ=1.53t\nimages=40\nslices=40\n")), "ImageJ")
	})
	t.Run("MetaMorph stack", func(t *testing.T) {
		refused(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: 0x835D, Value: []byte{1, 2, 3, 4, 5, 6, 7, 8}}), "MetaMorph")
	})
	for name, text := range map[string]string{
		"ImageJ with every frame in a directory": "ImageJ=1.53t\nimages=1\n",
		"ImageJ with no frame count":             "ImageJ=1.53t\nunit=um\n",
		"ImageJ with an unreadable frame count":  "ImageJ=1.53t\nimages=lots\n",
		"not ImageJ at all":                      "images=40\nTest Description",
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, singlePage(littleEndian, plainSpec(1), description(text)), core.Options{})
			if findingNamed(rep, "Image description") == nil {
				t.Errorf("the description was not reported: %+v", rep.Findings)
			}
		})
	}
	t.Run("a description too long to be ImageJ's", func(t *testing.T) {
		text := "ImageJ=1.53t\nimages=40\n" + strings.Repeat("x", maxTextValue)
		clean(t, singlePage(littleEndian, plainSpec(1), description(text)), core.Options{})
	})
	t.Run("a description past the end of the file", func(t *testing.T) {
		clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagImageDescription, Type: typeASCII, Count: 1000, Value: "abcdef"}), core.Options{})
	})
}

func TestMostlyHiddenFileWarns(t *testing.T) {
	data := singlePage(littleEndian, plainSpec(1))
	_, rep := clean(t, data, core.Options{})
	if len(rep.Warnings) != 0 {
		t.Errorf("a normal file drew a warning: %q", rep.Warnings)
	}
	_, rep = clean(t, append(data, bytes.Repeat([]byte("frame"), 1000)...), core.Options{})
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "keep the original") {
		t.Errorf("got %q", rep.Warnings)
	}
}

func TestJPEGTableEdges(t *testing.T) {
	stream := realJPEG(t, 2)
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{stream}}
	present := func(t *testing.T, out []byte) bool {
		t.Helper()
		doc, err := parse(context.Background(), core.BytesSource(out), core.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(doc.pages[0].fields, func(f field) bool { return f.tag == tagJPEGTables })
	}
	t.Run("too short to hold a table", func(t *testing.T) {
		out, _ := clean(t, singlePage(littleEndian, page, coretest.Entry{Tag: tagJPEGTables, Value: []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerEOI}}), core.Options{})
		if present(t, out) {
			t.Error("tables with no room for a table were kept")
		}
	})
	t.Run("nothing left after filtering", func(t *testing.T) {
		tables := []byte{0xFF, jpegmeta.MarkerSOI}
		tables = append(tables, jpegSegment(jpegmeta.MarkerCOM, []byte("Test Table Comment"))...)
		tables = append(tables, 0xFF, jpegmeta.MarkerEOI)
		out, rep := clean(t, singlePage(littleEndian, page, coretest.Entry{Tag: tagJPEGTables, Value: tables}), core.Options{})
		if present(t, out) {
			t.Error("tables holding only a comment were kept")
		}
		if bytes.Contains(out, []byte("Test Table Comment")) {
			t.Error("the comment survived")
		}
		if findingNamed(rep, "Metadata in JPEG data") == nil {
			t.Errorf("the comment was not reported: %+v", rep.Findings)
		}
	})
	t.Run("past the end of the file", func(t *testing.T) {
		data := singlePage(littleEndian, page, coretest.Entry{Tag: tagJPEGTables, Type: typeUndefined, Count: 1 << 24, Value: []byte("abcdefgh")})
		if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrMalformed) {
			t.Errorf("got %v, want ErrMalformed", err)
		}
	})
	t.Run("not JPEG at all", func(t *testing.T) {
		data := singlePage(littleEndian, page, coretest.Entry{Tag: tagJPEGTables, Value: []byte("Test Author here")})
		if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrMalformed) {
			t.Errorf("got %v, want ErrMalformed", err)
		}
	})
}

// faultySource fails every read after the first n, as a dying disk does.
type faultySource struct {
	core.Source
	n *int
}

func (s faultySource) ReadAt(p []byte, off int64) (int, error) {
	if *s.n <= 0 {
		return 0, errors.New("disk failure")
	}
	*s.n--
	return s.Source.ReadAt(p, off)
}

// A failed read must surface as an error at whichever point it strikes. Passing it
// over could mean metadata that was never seen, reported as absent.
func TestReadErrorsSurface(t *testing.T) {
	stream := withSegments(realJPEG(t, 1), []byte("trailer"), jpegSegment(jpegmeta.MarkerCOM, []byte("comment")))
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{stream}}
	for name, data := range map[string][]byte{
		"loaded": loaded(littleEndian),
		"JPEG":   singlePage(littleEndian, page),
		"hidden": append(singlePage(littleEndian, plainSpec(1)), "Test Hidden"...),
	} {
		t.Run(name, func(t *testing.T) {
			sawFailure := false
			for n := 0; n < 4096; n++ {
				remaining := n
				var out bytes.Buffer
				_, err := handler{}.Clean(context.Background(), faultySource{core.BytesSource(data), &remaining}, &out, core.Options{})
				if err == nil {
					if !sawFailure {
						t.Fatal("Clean succeeded with no failing read at all")
					}
					return
				}
				sawFailure = true
			}
			t.Fatal("Clean never finished, however many reads it was allowed")
		})
	}
}

// cancellingSource cancels the context once it has served n reads.
type cancellingSource struct {
	core.Source
	n      *int
	cancel context.CancelFunc
}

func (s cancellingSource) ReadAt(p []byte, off int64) (int, error) {
	if *s.n <= 0 {
		s.cancel()
	}
	*s.n--
	return s.Source.ReadAt(p, off)
}

// A cancellation partway through must stop the work with the context's own error,
// or finish cleanly if it arrived after the last check. Nothing else is allowed.
func TestCancellationPartway(t *testing.T) {
	stream := withSegments(realJPEG(t, 1), []byte("trailer"), jpegSegment(jpegmeta.MarkerCOM, []byte("comment")))
	page := imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{stream}}
	gap := append(singlePage(littleEndian, plainSpec(1)), make([]byte, 1<<18)...)
	multi := build(littleEndian, &coretest.IFD{
		Entries: imageTags(plainSpec(1)),
		Next:    &coretest.IFD{Entries: imageTags(plainSpec(2))},
	}, nil, plainSpec(1).parts, plainSpec(2).parts)
	for name, data := range map[string][]byte{
		"loaded": loaded(littleEndian),
		"JPEG":   singlePage(littleEndian, page),
		"gap":    gap,
		"multi":  multi,
	} {
		t.Run(name, func(t *testing.T) {
			for n := 0; n < 4096; n++ {
				ctx, cancel := context.WithCancel(context.Background())
				remaining := n
				var out bytes.Buffer
				_, err := handler{}.Clean(ctx, cancellingSource{core.BytesSource(data), &remaining, cancel}, &out, core.Options{})
				cancel()
				if err == nil {
					return
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("after %d reads: %v", n, err)
				}
			}
			t.Fatal("Clean never finished, however many reads it was allowed")
		})
	}
}

// Some writers declare a sub-IFD pointer with TIFF's IFD type rather than LONG.
func TestPointerOfTypeIFD(t *testing.T) {
	root := &coretest.IFD{
		Entries: imageTags(plainSpec(1)),
		GPS:     coretest.FakeIFD(0).GPS,
	}
	data := build(littleEndian, root, nil, plainSpec(1).parts)
	off := binary.LittleEndian.Uint32(data[4:])
	for i := range int(binary.LittleEndian.Uint16(data[off:])) {
		e := data[int(off)+2+i*entrySize:]
		if binary.LittleEndian.Uint16(e) == tagGPSIFD {
			binary.LittleEndian.PutUint16(e[2:], typeIFD)
		}
	}
	_, rep := clean(t, data, core.Options{})
	if f := findingNamed(rep, "GPS coordinates"); f == nil || f.Value != coretest.EiffelTower {
		t.Errorf("got %+v in %+v", f, rep.Findings)
	}
}

// A dropped preview may share a value with the page that stays, and sharing is no
// reason to refuse the file. Strip data shared with metadata, by contrast, would be.
func TestDroppedPageMayShareValues(t *testing.T) {
	preview := imageSpec{width: 2, height: 2, samples: 3, rowsPerStrip: 2, parts: [][]byte{pixels(3, 12)}}
	root := &coretest.IFD{
		Entries: imageTags(plainSpec(1)),
		Next: &coretest.IFD{
			Entries: append(imageTags(preview), coretest.Entry{Tag: tagNewSubfileType, Value: uint32(1)}),
		},
	}
	data := build(littleEndian, root, nil, plainSpec(1).parts, preview.parts)

	// Find the first page's Y resolution, then point the preview's at it too.
	o := littleEndian
	slot := func(off uint32, tag uint16) []byte {
		for i := range int(o.Uint16(data[off:])) {
			if e := data[int(off)+2+i*entrySize:]; o.Uint16(e) == tag {
				return e
			}
		}
		t.Fatalf("no tag %d", tag)
		return nil
	}
	first := o.Uint32(data[4:])
	second := o.Uint32(data[int(first)+2+int(o.Uint16(data[first:]))*entrySize:])
	copy(slot(second, tagYResolution)[8:12], slot(first, tagYResolution)[8:12])

	out, rep := clean(t, data, core.Options{})
	if findingNamed(rep, "Embedded thumbnail") == nil {
		t.Errorf("the preview was not dropped: %+v", rep.Findings)
	}
	if !bytes.Contains(out, pixels(1, 24)) {
		t.Error("the page that stays lost its pixels")
	}
}
