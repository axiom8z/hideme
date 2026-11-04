package png

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	stdpng "image/png"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const hiddenText = "Test Hidden Text"

// imageChunks returns base's chunks around its single IDAT, whose payload it also
// returns.
func imageChunks(t *testing.T, base []byte) (head, idat, tail []byte) {
	t.Helper()
	const iendSize = 12
	if got := chunkTypes(t, base); !slices.Equal(got, []string{"IHDR", "IDAT", "IEND"}) {
		t.Fatalf("fixture has chunks %q", got)
	}
	return base[:33], base[33+8 : len(base)-iendSize-chunkCRCSize], base[len(base)-iendSize:]
}

func assertNoHiddenText(t *testing.T, out []byte) {
	t.Helper()
	if bytes.Contains(out, []byte(hiddenText)) {
		t.Fatal("the hidden text survived cleaning")
	}
}

func repeatHidden(n int) []byte {
	return []byte(strings.Repeat(hiddenText, n))
}

// Frame chunks mean nothing without an acTL before the image data: every decoder
// skips them, so an fdAT there could carry plain text through cleaning forever.
func TestFrameChunksWithoutAnAnimationAreRemoved(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	image := rawChunk("IDAT", idat)
	fdat := rawChunk("fdAT", append([]byte{0, 0, 0, 1}, hiddenText...))
	fctl := rawChunk("fcTL", []byte(hiddenText + "0123456789")[:26])
	actl := rawChunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0})
	cases := map[string][]byte{
		"fdAT after the image":   slices.Concat(head, image, fdat, tail),
		"fcTL after the image":   slices.Concat(head, image, fctl, tail),
		"fdAT before the image":  slices.Concat(head, fdat, image, tail),
		"acTL after the image":   slices.Concat(head, image, actl, fctl, fdat, tail),
		"acTL of the wrong size": slices.Concat(head, rawChunk("acTL", []byte{0, 0, 0}), image, fctl, fdat, tail),
		"acTL with no frames":    slices.Concat(head, rawChunk("acTL", make([]byte, 8)), image, fctl, fdat, tail),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, core.Options{})
			if !bytes.Equal(out, base) {
				t.Fatalf("the output should be the plain image; it has chunks %q", chunkTypes(t, out))
			}
			if !hasField(rep, "Other chunk", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
		})
	}
}

// Decoders stop reading image data once the zlib stream has ended, so whatever
// follows it, in the same chunk or in later ones, is no part of the picture.
func TestDataAfterTheCompressedImageIsRemoved(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	image := rawChunk("IDAT", idat)
	hidden := rawChunk("IDAT", []byte(hiddenText))
	cases := map[string][]byte{
		"inside the same chunk": slices.Concat(head, rawChunk("IDAT", append(bytes.Clone(idat), hiddenText...)), tail),
		"in a further chunk":    slices.Concat(head, image, hidden, tail),
		"in several chunks":     slices.Concat(head, image, hidden, hidden, tail),
		"after another chunk":   slices.Concat(head, image, rawChunk("gAMA", []byte{0, 0, 0xB1, 0x8F}), hidden, tail),
		"as a second stream":    slices.Concat(head, image, rawChunk("IDAT", compress(t, []byte(hiddenText))), tail),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			assertSamePixels(t, data, data) // a real decoder accepts the hostile file
			out, rep := clean(t, data, core.Options{})
			assertNoHiddenText(t, out)
			assertSamePixels(t, base, out)
			if bytes.Contains(out, compress(t, []byte(hiddenText))) {
				t.Fatal("the second stream survived")
			}
			if !hasField(rep, "Data after the compressed image", core.ActionRemove) && !hasField(rep, "Stray image data", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			if again, _ := clean(t, out, core.Options{}); !bytes.Equal(again, out) {
				t.Fatal("not idempotent")
			}
		})
	}
}

// A stream spread over many chunks, as every real encoder writes it, must come
// through byte for byte, however its end falls among them.
func TestSplitImageDataIsCopiedExactly(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	var split []byte
	for rest := idat; len(rest) > 0; {
		n := min(len(rest), 7)
		split = append(split, rawChunk("IDAT", rest[:n])...)
		rest = rest[n:]
	}
	data := slices.Concat(head, split, tail)
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, data) || len(rep.Findings) != 0 || len(rep.Warnings) != 0 {
		t.Fatalf("a split stream must pass through untouched: %+v", rep)
	}

	// The tail starts inside the last real chunk and runs on into another.
	last := len(idat) - 7
	tailed := slices.Concat(head, rawChunk("IDAT", idat[:last]), rawChunk("IDAT", append(bytes.Clone(idat[last:]), "Test Hidden"...)), rawChunk("IDAT", []byte(" Text")), tail)
	out, _ = clean(t, tailed, core.Options{})
	if want := slices.Concat(head, rawChunk("IDAT", idat[:last]), rawChunk("IDAT", idat[last:]), tail); !bytes.Equal(out, want) {
		t.Fatalf("the tail was not cut exactly: chunks %q", chunkTypes(t, out))
	}
}

// An empty chunk after the end of the stream holds nothing, so it goes silently.
func TestEmptyChunkAfterTheStreamIsNotReported(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	out, rep := clean(t, slices.Concat(head, rawChunk("IDAT", idat), rawChunk("IDAT", nil), tail), core.Options{})
	if !bytes.Equal(out, base) || len(rep.Findings) != 0 {
		t.Fatalf("%q, %+v", chunkTypes(t, out), rep.Findings)
	}
}

// Data compressed into the stream past the last row cannot be cut without
// re-encoding, so the one thing left is to say that it stays.
func TestExtraImageDataIsAnnounced(t *testing.T) {
	const width, height = 2, 2
	ihdr := binary.BigEndian.AppendUint32(nil, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, colorGray, 0, 0, 0)
	rows := []byte{0, 1, 2, 0, 3, 4}
	make1 := func(raw []byte) []byte {
		return slices.Concat(signature, rawChunk("IHDR", ihdr), rawChunk("IDAT", compress(t, raw)), rawChunk("IEND", nil))
	}
	exact := make1(rows)
	if _, rep := clean(t, exact, core.Options{}); len(rep.Warnings) != 0 || len(rep.Findings) != 0 {
		t.Fatalf("an exact stream must be quiet: %+v", rep)
	}
	extra := make1(append(bytes.Clone(rows), repeatHidden(3)...))
	out, rep := clean(t, extra, core.Options{})
	if !bytes.Equal(out, extra) {
		t.Fatal("the stream must not be re-encoded")
	}
	if !hasField(rep, "Extra data in the compressed image", core.ActionRetain) {
		t.Fatalf("no retained finding: %+v", rep.Findings)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "stay in the output") {
		t.Fatalf("warnings = %q", rep.Warnings)
	}
}

// The interlaced size differs from the plain one, and an exact interlaced stream
// must not read as having extra data.
func TestInterlacedStreamHasNoExtraData(t *testing.T) {
	for _, size := range []int{1, 2, 3, 5, 8, 9, 16, 33} {
		_, rep := clean(t, interlacedPNG(t, size), core.Options{})
		if len(rep.Warnings) != 0 || len(rep.Findings) != 0 {
			t.Fatalf("size %d: %+v", size, rep)
		}
	}
}

func TestDamagedImageDataIsRefused(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	badBlock := bytes.Clone(idat)
	badBlock[2] = 0xFF // the reserved deflate block type
	cases := map[string][]byte{
		"bad zlib header":  slices.Concat(head, rawChunk("IDAT", []byte(hiddenText)), tail),
		"bad deflate data": slices.Concat(head, rawChunk("IDAT", badBlock), tail),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); !errors.Is(err, core.ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

// A stream cut short was all read as compressed data, so nothing goes unparsed,
// and a wrong checksum still marks where the stream ends.
func TestShortOrMischeckedImageDataIsKept(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	badSum := bytes.Clone(idat)
	badSum[len(badSum)-1] ^= 0xFF
	for name, payload := range map[string][]byte{
		"cut short":      idat[:len(idat)/2],
		"empty":          nil,
		"wrong checksum": badSum,
	} {
		t.Run(name, func(t *testing.T) {
			data := slices.Concat(head, rawChunk("IDAT", payload), tail)
			if out, _ := clean(t, data, core.Options{}); !bytes.Equal(out, data) {
				t.Fatal("the image data must be copied as it is")
			}
		})
	}
	out, _ := clean(t, slices.Concat(head, rawChunk("IDAT", append(bytes.Clone(badSum), hiddenText...)), tail), core.Options{})
	assertNoHiddenText(t, out)
}

// Decoders use the first of these chunks and skip the rest, so each repeat is a
// free place for 32 bytes of anything, as often as the file likes.
func TestRepeatedAncillaryChunksAreDropped(t *testing.T) {
	base := basePNG(t)
	first := rawChunk("gAMA", []byte{0, 0, 0xB1, 0x8F})
	repeat := rawChunk("gAMA", []byte("Test"))
	out, rep := clean(t, insertAfterIHDR(base, first, repeat, repeat), core.Options{})
	if !bytes.Equal(out, insertAfterIHDR(base, first)) {
		t.Fatalf("chunks %q", chunkTypes(t, out))
	}
	if !hasField(rep, "Other chunk", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
}

// Chromaticities above 1.0 are not colours: decoders skip them, so such a chunk
// can only be 32 bytes of something else.
func TestImpossibleChromaticitiesAreDropped(t *testing.T) {
	base := basePNG(t)
	out, rep := clean(t, insertAfterIHDR(base, rawChunk("cHRM", repeatHidden(2)[:32])), core.Options{})
	if !bytes.Equal(out, base) || !hasField(rep, "Other chunk", core.ActionRemove) {
		t.Fatalf("chunks %q, findings %+v", chunkTypes(t, out), rep.Findings)
	}
	valid := binary.BigEndian.AppendUint32(nil, 100000)
	valid = append(valid, make([]byte, 28)...)
	chunk := rawChunk("cHRM", valid)
	if out, _ := clean(t, insertAfterIHDR(base, chunk), core.Options{}); !bytes.Contains(out, chunk) {
		t.Fatal("a valid cHRM was lost")
	}
}

// These chunks are optional and decoders skip a damaged one, so a file having one
// displays perfectly well and refusing it would help nobody. Dropping it leaves no
// unparsed bytes in the output.
func TestInvalidAncillaryChunksAreDropped(t *testing.T) {
	palette := rawChunk("PLTE", []byte{1, 2, 3})
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	cases := map[string]struct {
		data []byte
		want []string
	}{
		"oversized gAMA":        {insertAfterIHDR(base, rawChunk("gAMA", []byte(hiddenText))), []string{"IHDR", "IDAT", "IEND"}},
		"short pHYs":            {insertAfterIHDR(base, rawChunk("pHYs", make([]byte, 8))), []string{"IHDR", "IDAT", "IEND"}},
		"oversized sRGB":        {insertAfterIHDR(base, rawChunk("sRGB", make([]byte, 2))), []string{"IHDR", "IDAT", "IEND"}},
		"short cHRM":            {insertAfterIHDR(base, rawChunk("cHRM", make([]byte, 31))), []string{"IHDR", "IDAT", "IEND"}},
		"oversized cICP":        {insertAfterIHDR(base, rawChunk("cICP", make([]byte, 5))), []string{"IHDR", "IDAT", "IEND"}},
		"short mDCV":            {insertAfterIHDR(base, rawChunk("mDCV", make([]byte, 23))), []string{"IHDR", "IDAT", "IEND"}},
		"short cLLI":            {insertAfterIHDR(base, rawChunk("cLLI", make([]byte, 7))), []string{"IHDR", "IDAT", "IEND"}},
		"tRNS on RGBA":          {insertAfterIHDR(base, rawChunk("tRNS", []byte{1})), []string{"IHDR", "IDAT", "IEND"}},
		"acTL with no frames":   {insertAfterIHDR(base, rawChunk("acTL", []byte(hiddenText)[:8])), []string{"IHDR", "IDAT", "IEND"}},
		"bKGD wrong size":       {insertAfterIHDR(base, rawChunk("bKGD", []byte{1, 2})), []string{"IHDR", "IDAT", "IEND"}},
		"sBIT wrong size":       {insertAfterIHDR(base, rawChunk("sBIT", []byte{1, 2})), []string{"IHDR", "IDAT", "IEND"}},
		"gAMA after the image":  {slices.Concat(head, rawChunk("IDAT", idat), rawChunk("gAMA", []byte{0, 0, 0xB1, 0x8F}), tail), []string{"IHDR", "IDAT", "IEND"}},
		"identical tRNS":        {tinyPNG(colorGray, rawChunk("tRNS", []byte{0, 1}), rawChunk("tRNS", []byte{0, 1})), []string{"IHDR", "tRNS", "IDAT", "IEND"}},
		"histogram of any size": {tinyPNG(colorPalette, palette, rawChunk("hIST", []byte{1})), []string{"IHDR", "PLTE", "IDAT", "IEND"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, tc.data, core.Options{})
			assertNoHiddenText(t, out)
			if got := chunkTypes(t, out); !slices.Equal(got, tc.want) {
				t.Fatalf("chunks %q, want %q", got, tc.want)
			}
			if !hasField(rep, "Other chunk", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
		})
	}
}

// tRNS changes the pixels, and libpng and Pillow disagree on when it counts, so
// neither keeping nor dropping a doubtful one is safe for both.
func TestDoubtfulTransparencyIsRefused(t *testing.T) {
	palette := rawChunk("PLTE", []byte{1, 2, 3})
	cases := map[string][]byte{
		"on gray, wrong size":  tinyPNG(colorGray, rawChunk("tRNS", []byte{1})),
		"on RGB, wrong size":   tinyPNG(colorRGB, rawChunk("tRNS", []byte{1})),
		"before PLTE":          tinyPNG(colorPalette, rawChunk("tRNS", []byte{1}), palette),
		"longer than the PLTE": tinyPNG(colorPalette, palette, rawChunk("tRNS", []byte{1, 2})),
		"repeats differently":  tinyPNG(colorPalette, palette, rawChunk("tRNS", []byte{1}), rawChunk("tRNS", []byte{2})),
		"after the image":      slices.Concat(tinyPNG(colorGray)[:len(tinyPNG(colorGray))-12], rawChunk("tRNS", []byte{0, 1}), rawChunk("IEND", nil)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); !errors.Is(err, core.ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

// A palette on a true-colour image is only a suggestion for old displays, and no
// decoder needs it to show the picture. It holds up to 768 bytes of anything.
func TestSuggestedPaletteIsDropped(t *testing.T) {
	base := basePNG(t)
	out, rep := clean(t, insertAfterIHDR(base, rawChunk("PLTE", repeatHidden(3)[:48])), core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatalf("chunks %q", chunkTypes(t, out))
	}
	if !hasField(rep, "Suggested palette", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
}

// A one-bit image can reach two palette entries, and decoders ignore the rest.
func TestPaletteEntriesTheImageCannotReachAreCut(t *testing.T) {
	ihdr := binary.BigEndian.AppendUint32(nil, 1)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 1)
	ihdr = append(ihdr, 1, colorPalette, 0, 0, 0)
	palette := append([]byte{1, 2, 3, 4, 5, 6}, repeatHidden(3)[:48]...)
	data := slices.Concat(signature, rawChunk("IHDR", ihdr), rawChunk("PLTE", palette), rawChunk("IDAT", compress(t, []byte{0, 0x80})), rawChunk("IEND", nil))
	out, rep := clean(t, data, core.Options{})
	assertNoHiddenText(t, out)
	if !bytes.Contains(out, rawChunk("PLTE", []byte{1, 2, 3, 4, 5, 6})) {
		t.Fatal("the reachable entries must stay")
	}
	if !hasField(rep, "Unreachable palette entries", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	if _, err := stdpng.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("decode output: %v", err)
	}
}

func frameControl(seq, width, height, x, y uint32, dispose, blend byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, seq)
	out = binary.BigEndian.AppendUint32(out, width)
	out = binary.BigEndian.AppendUint32(out, height)
	out = binary.BigEndian.AppendUint32(out, x)
	out = binary.BigEndian.AppendUint32(out, y)
	out = append(out, 0, 1, 0, 10, dispose, blend)
	return rawChunk("fcTL", out)
}

func frameData(seq uint32, payload []byte) []byte {
	return rawChunk("fdAT", append(binary.BigEndian.AppendUint32(nil, seq), payload...))
}

func TestBrokenAnimationsAreRefused(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	image := rawChunk("IDAT", idat)
	actl := func(frames uint32) []byte {
		return rawChunk("acTL", binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, frames), 0))
	}
	z := compress(t, []byte{0})
	cases := map[string][]byte{
		"fcTL of the wrong size":   slices.Concat(head, actl(2), image, rawChunk("fcTL", make([]byte, 25)), tail),
		"sequence gap":             slices.Concat(head, actl(2), image, frameControl(5, 1, 1, 0, 0, 0, 0), frameData(6, z), tail),
		"fdAT sequence gap":        slices.Concat(head, actl(2), image, frameControl(0, 1, 1, 0, 0, 0, 0), frameData(5, z), tail),
		"fdAT with no frame":       slices.Concat(head, actl(2), image, frameData(0, z), tail),
		"short fdAT":               slices.Concat(head, actl(2), image, frameControl(0, 1, 1, 0, 0, 0, 0), rawChunk("fdAT", []byte{0}), tail),
		"frame outside the canvas": slices.Concat(head, actl(2), image, frameControl(0, 25, 1, 0, 0, 0, 0), frameData(1, z), tail),
		"frame offset too far":     slices.Concat(head, actl(2), image, frameControl(0, 1, 1, 24, 0, 0, 0), frameData(1, z), tail),
		"empty frame":              slices.Concat(head, actl(2), image, frameControl(0, 1, 1, 0, 0, 0, 0), tail),
		"frame with no data":       slices.Concat(head, actl(3), image, frameControl(0, 1, 1, 0, 0, 0, 0), frameControl(1, 1, 1, 0, 0, 0, 0), frameData(2, z), tail),
		"bad dispose":              slices.Concat(head, actl(2), image, frameControl(0, 1, 1, 0, 0, 3, 0), frameData(1, z), tail),
		"bad blend":                slices.Concat(head, actl(2), image, frameControl(0, 1, 1, 0, 0, 0, 2), frameData(1, z), tail),
		"more frames than declared": slices.Concat(head, actl(1), frameControl(0, 24, 16, 0, 0, 0, 0), image,
			frameControl(1, 1, 1, 0, 0, 0, 0), frameData(2, z), tail),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); !errors.Is(err, core.ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

// Frame chunks are numbered, so a tail is cut out of them in a way that keeps
// every number in place.
func TestDataAfterAFrameIsRemovedAndTheSequenceKept(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	actl := rawChunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0})
	fctl := frameControl(0, 24, 16, 0, 0, 0, 0)
	fctl2 := frameControl(1, 1, 1, 0, 0, 0, 0)
	z := compress(t, []byte{0, 1, 2, 3, 4})
	data := slices.Concat(head, actl, fctl, rawChunk("IDAT", idat), fctl2,
		frameData(2, append(bytes.Clone(z), hiddenText...)), frameData(3, []byte(hiddenText)), tail)
	out, rep := clean(t, data, core.Options{})
	assertNoHiddenText(t, out)
	if !hasField(rep, "Data after the compressed image", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	if !bytes.Contains(out, frameData(2, z)) || !bytes.Contains(out, frameData(3, nil)) {
		t.Fatalf("chunks %q", chunkTypes(t, out))
	}
	if again, _ := clean(t, out, core.Options{}); !bytes.Equal(again, out) {
		t.Fatal("not idempotent")
	}
}

func TestTooMuchImageDataIsRefused(t *testing.T) {
	c := &cleaner{rep: &core.Report{Format: formatID}, imageBudget: 8, header: imageHeader{width: 1, height: 1, depth: 8, colorType: colorGray}}
	z := compress(t, make([]byte, 64))
	s := &imageStream{width: 1, height: 1, where: "IDAT", pieces: []streamPiece{{part: 0, data: z}}}
	c.parts = [][]byte{nil}
	if err := c.checkStream(context.Background(), s); !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// clean prints Field and Where and leaves Value out, so that its logs cannot recreate
// the metadata. Text from the file therefore belongs in Value and nowhere else, and
// that includes the names of things, which a writer can set to anything.
func assertLabelsAreOurs(t *testing.T, rep *core.Report, marker string) {
	t.Helper()
	for _, f := range rep.Findings {
		for _, label := range []string{f.Field, f.Where, f.Reason} {
			if strings.Contains(label, marker) {
				t.Errorf("finding %+v repeats text from the file in %q", f, label)
			}
		}
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, marker) {
			t.Errorf("warning %q repeats text from the file", w)
		}
	}
}

func TestFileTextStaysOutOfLabels(t *testing.T) {
	const marker = "Test Marker Text"
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	block := append([]byte("Exif\x00\x00"), coretest.FakeEXIF(0)...)
	for name, chunks := range map[string][]byte{
		"raw profile that does not decode": tEXt("Raw profile type "+marker, "not a profile"),
		"raw profile of unknown contents":  tEXt("Raw profile type "+marker, rawProfile("x", []byte("anything"))),
		"raw EXIF profile":                 tEXt("Raw profile type "+marker, rawProfile("exif", block)),
		"raw XMP profile":                  tEXt("Raw profile type "+marker, rawProfile("xmp", []byte("<x:xmpmeta><dc:creator>x</dc:creator></x:xmpmeta>"))),
		"text keyword":                     tEXt(marker, marker),
		"compressed text keyword":          zTXt(t, marker, marker),
		"international text keyword":       iTXt(marker, marker),
		"profile name":                     iccpChunk(t, marker, fakeICC(300)),
		"damaged profile name":             rawChunk("iCCP", []byte(marker+"\x00\x00garbage")),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, slices.Concat(head, chunks, rawChunk("IDAT", idat), tail), core.Options{})
			assertLabelsAreOurs(t, rep, marker)
		})
	}
	t.Run("chunk type", func(t *testing.T) {
		_, rep := clean(t, insertAfterIHDR(base, rawChunk("teSt", []byte("x"))), core.Options{})
		assertLabelsAreOurs(t, rep, "teSt")
	})
}

// Error messages reach the user too, through embedded files as well as directly.
func TestFileTextStaysOutOfErrors(t *testing.T) {
	base := basePNG(t)
	badCRC := rawChunk("teSt", []byte("x"))
	badCRC[len(badCRC)-1] ^= 0xFF
	badType := rawChunk("te1t", []byte("x"))
	for name, data := range map[string][]byte{
		"unknown critical chunk": insertAfterIHDR(base, rawChunk("TEST", nil)),
		"bad CRC":                insertAfterIHDR(base, badCRC),
		"invalid type":           insertAfterIHDR(base, badType),
		"first chunk":            slices.Concat(signature, rawChunk("teSt", nil), base[8:]),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
			if err == nil {
				t.Fatal("the file should be refused")
			}
			for _, marker := range []string{"teSt", "TEST", "te1t", `\`} {
				if strings.Contains(err.Error(), marker) {
					t.Errorf("the error %q repeats text from the file", err)
				}
			}
		})
	}
}

// A frame costs a few dozen bytes, and each one is a stream to read, so what it takes
// to read one has to be reused. Allocating it anew spent gigabytes on a small file.
func TestManyFramesReuseTheirBuffers(t *testing.T) {
	base := basePNG(t)
	head, idat, tail := imageChunks(t, base)
	z := compress(t, []byte{0})
	data := slices.Concat(head, rawChunk("acTL", []byte{0, 0, 0xFF, 0xFF, 0, 0, 0, 0}), rawChunk("IDAT", idat))
	for seq := uint32(0); seq < 2000; seq += 2 {
		data = append(data, frameControl(seq, 1, 1, 0, 0, 0, 0)...)
		data = append(data, frameData(seq+1, z)...)
	}
	data = append(data, tail...)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	// A reader and a buffer for each of the 1000 frames would be over 100 MB.
	if spent := after.TotalAlloc - before.TotalAlloc; spent > 32<<20 {
		t.Fatalf("spent %d MB on 1000 frames", spent>>20)
	}
}
