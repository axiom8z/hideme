package tiff

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

const hiddenText = "Test Hidden Text"

func jpegPage(stream []byte) imageSpec {
	return imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{stream}}
}

func refused(t *testing.T, data []byte, want error) {
	t.Helper()
	_, err := handler{}.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// Each strip of a JPEG-compressed page is a JPEG of its own, and the segments a
// decoder needs are kept. A kept segment with room to spare would carry anything at
// all through cleaning, so each must be exactly the shape its marker defines.
func TestJPEGSegmentsWithRoomToSpareAreRefused(t *testing.T) {
	text := []byte(hiddenText)
	for name, seg := range map[string][]byte{
		"restart interval":   jpegSegment(jpegmeta.MarkerDRI, text),
		"number of lines":    jpegSegment(jpegmeta.MarkerDNL, text),
		"arithmetic table":   jpegSegment(jpegmeta.MarkerDAC, text),
		"quantisation table": jpegSegment(jpegmeta.MarkerDQT, text),
		"Huffman table":      jpegSegment(jpegmeta.MarkerDHT, text),
		"frame header":       jpegSegment(0xC1, text),
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, singlePage(littleEndian, jpegPage(withSegments(realJPEG(t, 1), nil, seg))), core.ErrMalformed)
		})
	}
	t.Run("scan header", func(t *testing.T) {
		stream := []byte{0xFF, jpegmeta.MarkerSOI, 0xFF, jpegmeta.MarkerSOS, 0, 18}
		stream = append(stream, text...)
		stream = append(stream, 0xFF, jpegmeta.MarkerEOI)
		refused(t, singlePage(littleEndian, jpegPage(stream)), core.ErrMalformed)
	})
}

func TestAdobeSegmentInAStripKeepsOnlyTheTransform(t *testing.T) {
	hostile := jpegSegment(jpegmeta.MarkerAPP14, []byte("Adobe"+"Test!!"+"\x01"))
	canonical := jpegSegment(jpegmeta.MarkerAPP14, []byte("Adobe\x00\x64\x00\x00\x00\x00\x01"))
	stream := withSegments(realJPEG(t, 2), nil, hostile, hostile)
	out, rep := clean(t, singlePage(littleEndian, jpegPage(stream)), core.Options{})
	if bytes.Contains(out, []byte("Test!!")) {
		t.Fatal("the free fields of the Adobe segment survived")
	}
	if !bytes.Contains(out, canonical) {
		t.Fatal("the transform was lost")
	}
	if got := bytes.Count(out, []byte("Adobe")); got != 1 {
		t.Fatalf("%d Adobe segments in the output", got)
	}
	if findingNamed(rep, "Adobe header values") == nil {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	again, rep := clean(t, out, core.Options{})
	if !bytes.Equal(again, out) || len(rep.Residual()) != 0 {
		t.Fatalf("not idempotent: %+v", rep.Findings)
	}

	disagree := withSegments(realJPEG(t, 2), nil, canonical, jpegSegment(jpegmeta.MarkerAPP14, []byte("Adobe\x00\x64\x00\x00\x00\x00\x02")))
	refused(t, singlePage(littleEndian, jpegPage(disagree)), core.ErrMalformed)
}

func TestSecondJFIFInAStripIsCut(t *testing.T) {
	jfif := jpegSegment(jpegmeta.MarkerAPP0, []byte("JFIF\x00\x01\x01\x01\x00\x48\x00\x48\x00\x00"))
	second := jpegSegment(jpegmeta.MarkerAPP0, []byte("JFIF\x00Test!!!\x00\x00"))
	out, _ := clean(t, singlePage(littleEndian, jpegPage(withSegments(realJPEG(t, 2), nil, jfif, second))), core.Options{})
	if bytes.Contains(out, []byte("Test!!!")) {
		t.Fatal("the second JFIF header survived")
	}
	if !bytes.Contains(out, jfif) {
		t.Fatal("the first JFIF header was lost")
	}
}

// Every strip can point at the same JPEG, and each one means walking all of it, so a
// file of a megabyte or two could demand terabytes of work. Without the limit this
// runs into the timeout.
func TestRepeatedJPEGStripsDoNotMakeTheWalkQuadratic(t *testing.T) {
	const strips = 1 << 15
	real := realJPEG(t, 1)
	stream := slices.Concat(real[:len(real)-2], make([]byte, 1<<20), real[len(real)-2:]) // a long scan
	parts := make([][]byte, strips)
	for i := range parts {
		parts[i] = nil
	}
	spec := imageSpec{width: 1, height: strips, rowsPerStrip: 1, compression: compressionJPEG, photometric: 1, parts: parts}
	data := singlePage(littleEndian, spec)
	offset := uint32(len(data))
	data = append(data, stream...)
	pointStrips(t, data, offset, uint32(len(stream)))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := (handler{}).Inspect(ctx, core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// pointStrips aims every entry of the strip tables at one range.
func pointStrips(t *testing.T, data []byte, offset, size uint32) {
	t.Helper()
	ifd := littleEndian.Uint32(data[4:])
	for i := range int(littleEndian.Uint16(data[ifd:])) {
		e := data[int(ifd)+2+i*entrySize:]
		value := offset
		switch littleEndian.Uint16(e) {
		case tagStripOffsets:
		case tagStripByteCounts:
			value = size
		default:
			continue
		}
		table := data[littleEndian.Uint32(e[8:]):]
		for j := range int(littleEndian.Uint32(e[4:])) {
			littleEndian.PutUint32(table[4*j:], value)
		}
	}
}

// A colour map on an image that is not a palette image is read by nothing, and it
// holds up to 393 KiB of whatever a writer likes.
func TestColourMapIsKeptOnlyForAPalette(t *testing.T) {
	colorMap := make([]uint16, 3*256)
	for i := range colorMap {
		colorMap[i] = uint16('T')<<8 | uint16('e')
	}
	spec := plainSpec(1)
	out, rep := clean(t, singlePage(littleEndian, spec, coretest.Entry{Tag: tagColorMap, Value: colorMap}), core.Options{})
	if bytes.Contains(out, []byte("eTeTeT")) {
		t.Fatal("the colour map survived")
	}
	if len(rep.Findings) == 0 {
		t.Fatal("nothing was reported")
	}
}

// The same for tile sizes on a page of strips: nothing reads them.
func TestTileSizeIsKeptOnlyForTiles(t *testing.T) {
	out, rep := clean(t, singlePage(littleEndian, plainSpec(1),
		coretest.Entry{Tag: tagTileWidth, Value: uint32(0x54657374)},
		coretest.Entry{Tag: tagTileLength, Value: uint32(0x54657374)},
	), core.Options{})
	if bytes.Contains(out, []byte("tseT")) {
		t.Fatal("the tile size survived")
	}
	if len(rep.Findings) == 0 {
		t.Fatal("nothing was reported")
	}
}

func TestWrongColourMapOnAPaletteIsRefused(t *testing.T) {
	s := imageSpec{width: 4, height: 2, rowsPerStrip: 2, photometric: 3, parts: [][]byte{pixels(1, 8)}}
	refused(t, singlePage(littleEndian, s, coretest.Entry{Tag: tagColorMap, Value: []uint16{1, 2, 3, 4, 5, 6}}), core.ErrMalformed)
}

// A profile states its own size. Bytes past that are no part of it, and a retained
// profile is reported as just a colour profile, so they would pass unseen.
func TestICCProfileMustBeTheSizeItStates(t *testing.T) {
	longer := append(fakeICC(), hiddenText...)
	out, rep := clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagICCProfile, Value: longer}), core.Options{})
	if bytes.Contains(out, []byte(hiddenText)) {
		t.Fatal("the bytes after the profile survived")
	}
	f := findingNamed(rep, "ICC colour profile")
	if f == nil || f.Action != core.ActionRemove {
		t.Fatalf("finding = %+v", f)
	}
}

// Uncompressed pixels have a known size, and anything past it is not pixels. For
// subsampled YCbCr the sum is different, and decoders disagree on it, so a strip that
// has more than the sum allows has no one right place to cut.
func TestUncompressedYCbCrStripsAreChecked(t *testing.T) {
	const width, height = 16, 16
	subsampled := (width / 2) * (height / 2) * (2*2 + 2)
	spec := func(strip []byte) imageSpec {
		return imageSpec{width: width, height: height, samples: 3, rowsPerStrip: height, photometric: photometricYCbCr, parts: [][]byte{strip}}
	}
	sampling := coretest.Entry{Tag: tagYCbCrSubSamp, Value: []uint16{2, 2}}
	exact := pixels(1, subsampled)
	out, _ := clean(t, singlePage(littleEndian, spec(exact), sampling), core.Options{})
	if !bytes.Contains(out, exact) {
		t.Fatal("an exact strip must survive")
	}
	refused(t, singlePage(littleEndian, spec(append(bytes.Clone(exact), hiddenText...)), sampling), core.ErrMalformed)
	full := coretest.Entry{Tag: tagYCbCrSubSamp, Value: []uint16{1, 1}}
	whole := pixels(2, width*height*3)
	if out, _ := clean(t, singlePage(littleEndian, spec(whole), full), core.Options{}); !bytes.Contains(out, whole) {
		t.Fatal("a 4:4:4 strip must survive")
	}
	refused(t, singlePage(littleEndian, spec(exact), coretest.Entry{Tag: tagYCbCrSubSamp, Value: []uint16{3, 3}}), core.ErrMalformed)
	parts := imageSpec{width: width, height: height, samples: 3, rowsPerStrip: height, photometric: photometricYCbCr, planar: 2, parts: [][]byte{exact, exact, exact}}
	refused(t, singlePage(littleEndian, parts), core.ErrUnsupported)
}

// The size of a strip depends on the bit depth of every sample, and adding those up
// for each strip made a file with a thousand samples a thousand times slower. Without
// the sum being kept, this runs into the timeout.
func TestManySamplesDoNotSlowEveryStrip(t *testing.T) {
	coretest.SkipIfRace(t)
	const strips, samples = 1 << 22, maxSamples
	root := &coretest.IFD{Entries: []coretest.Entry{
		{Tag: tagImageWidth, Value: uint32(1)},
		{Tag: tagImageLength, Value: uint32(strips)},
		{Tag: tagBitsPerSample, Value: uint16(8)},
		{Tag: tagCompression, Value: uint16(compressionNone)},
		{Tag: tagPhotometric, Value: uint16(1)},
		{Tag: tagSamplesPerPixel, Value: uint16(samples)},
		{Tag: tagRowsPerStrip, Value: uint32(1)},
		{Tag: tagStripOffsets, Value: make([]uint32, strips)},
		{Tag: tagStripByteCounts, Value: make([]uint32, strips)},
	}}
	data := root.Encode(littleEndian)
	offset := uint32(len(data))
	data = append(data, make([]byte, 4096)...)
	pointStrips(t, data, offset, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := (handler{}).Inspect(ctx, core.BytesSource(data), core.Options{}); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the walk took too long")
	}
}

// The report reads Photoshop and IPTC blocks with the JPEG handler's parser, so a block
// that parser understands gets the same report here. The block goes either way.
func TestPhotoshopAndIPTCVariantsAreReported(t *testing.T) {
	long := []byte{jpegmeta.IPTCTagMarker, 2, 200, 0x80, 0x02, 0, 3, 'a', 'b', 'c'}
	_, rep := clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagIPTC, Type: typeByte, Value: append(long, fakeIPTC()...)}), core.Options{})
	if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "IPTC by-line" }) {
		t.Errorf("a long-form dataset ended the report of those after it: %+v", rep.Findings)
	}
	older := append([]byte("PHUT"), irb(jpegmeta.ResourceIPTC, fakeIPTC())[4:]...)
	_, rep = clean(t, singlePage(littleEndian, plainSpec(1), coretest.Entry{Tag: tagPhotoshop, Type: typeByte, Value: older}), core.Options{})
	if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "IPTC by-line" }) {
		t.Errorf("an older Photoshop signature was not read: %+v", rep.Findings)
	}
}

// A decoder reads only the tables a scan selects, and this handler does not work out
// which those are, so a spare table in a strip stays. The limitations say so.
func TestUnusedJPEGTableInAStripStays(t *testing.T) {
	table := append(append([]byte{0x13}, make([]byte, 15)...), byte(len(hiddenText)))
	table = append(table, hiddenText...)
	out, _ := clean(t, singlePage(littleEndian, jpegPage(withSegments(realJPEG(t, 2), nil, jpegSegment(jpegmeta.MarkerDHT, table)))), core.Options{})
	if !bytes.Contains(out, []byte(hiddenText)) {
		t.Fatal("the table was removed, so the limitations are out of date")
	}
}
