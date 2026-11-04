package gif

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	stdgif "image/gif"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

const hiddenText = "Test Hidden Text"

func assertNoHiddenText(t *testing.T, out []byte) {
	t.Helper()
	if bytes.Contains(out, []byte(hiddenText)) {
		t.Fatal("the hidden text survived cleaning")
	}
}

func refuses(t *testing.T, data []byte) {
	t.Helper()
	var out bytes.Buffer
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Fatalf("got %v, want ErrMalformed", err)
	}
}

func assertIdempotent(t *testing.T, out []byte) {
	t.Helper()
	again, rep := clean(t, out, core.Options{})
	if !bytes.Equal(again, out) {
		t.Fatal("cleaning the output changed it")
	}
	if residual := rep.Residual(); len(residual) != 0 {
		t.Fatalf("residual findings: %+v", residual)
	}
}

func decodeAll(t *testing.T, data []byte) *stdgif.GIF {
	t.Helper()
	g, err := stdgif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return g
}

func assertSameFrames(t *testing.T, before, after []byte) {
	t.Helper()
	a, b := decodeAll(t, before), decodeAll(t, after)
	if len(a.Image) != len(b.Image) {
		t.Fatalf("%d frames became %d", len(a.Image), len(b.Image))
	}
	for i := range a.Image {
		if !bytes.Equal(a.Image[i].Pix, b.Image[i].Pix) {
			t.Fatalf("frame %d changed", i)
		}
	}
}

// lastFrameEnd returns the offset just past the last frame's terminator.
func lastFrameEnd(base []byte) int {
	return len(base) - 1
}

// firstFrameData locates the first frame's image data: the offset of its LZW width
// and the offset of its terminator.
func firstFrameData(t *testing.T, base []byte) (start, terminator int) {
	t.Helper()
	c := &cleaner{data: base, rep: &core.Report{Format: formatID}}
	pos, err := c.header()
	if err != nil {
		t.Fatal(err)
	}
	for base[pos] != introImage {
		if pos, err = c.extension(context.Background(), pos); err != nil {
			t.Fatal(err)
		}
	}
	start, err = c.colorTable(pos+descriptorSize, base[pos+descriptorSize-1])
	if err != nil {
		t.Fatal(err)
	}
	terminator = start + 1
	for base[terminator] != 0 {
		terminator += 1 + int(base[terminator])
	}
	return start, terminator
}

// Plain Text is drawn on the picture by the rare decoder that draws it, so it stays
// as the format's limitations say, but a user must hear that this file has some.
func TestPlainTextIsAnnounced(t *testing.T) {
	base := animation(t)
	data := insertAtStart(base, plainTextBlock(hiddenText))
	for name, opts := range map[string]core.Options{"default": {}, "strict": {Strict: true}} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, opts)
			if !bytes.Contains(out, []byte(hiddenText)) {
				t.Fatal("the text is part of the picture and must stay")
			}
			if !hasField(rep, "Plain Text extension", core.ActionRetain) {
				t.Fatalf("no retained finding: %+v", rep.Findings)
			}
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "Plain Text") {
				t.Fatalf("warnings = %q", rep.Warnings)
			}
			if strings.Contains(rep.Warnings[0], hiddenText) {
				t.Fatal("a warning must not repeat the value, because clean output is meant to be safe to log")
			}
		})
	}
	if _, rep := clean(t, base, core.Options{}); len(rep.Warnings) != 0 {
		t.Fatalf("warnings on a file without any: %q", rep.Warnings)
	}
}

// Decoders stop at the end code, so sub-blocks after it are no part of the picture,
// though they look just like image data to a walk that does not decompress.
func TestDataAfterTheCompressedImageIsRemoved(t *testing.T) {
	base := animation(t)
	_, terminator := firstFrameData(t, base)
	tail := subBlocksOf([]byte(hiddenText))
	data := slices.Concat(base[:terminator], tail, base[terminator+1:])
	out, rep := clean(t, data, core.Options{})
	assertNoHiddenText(t, out)
	if !bytes.Equal(out, base) {
		t.Fatal("the output should be the original animation")
	}
	assertSameFrames(t, base, out)
	if !hasField(rep, "Data after the compressed image", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	assertIdempotent(t, out)
}

// The same when the data starts in the sub-block that holds the end code.
func TestDataAfterTheEndCodeInItsOwnSubBlock(t *testing.T) {
	// A 2x1 image with a minimum code size of 2: clear, 0, 1 and end, of 3 bits each.
	frame := []byte{introImage, 0, 0, 0, 0, 2, 0, 1, 0, 0, 2}
	header := append([]byte("GIF89a\x02\x00\x01\x00\x80\x00\x00"), 0, 0, 0, 255, 255, 255)
	stream := lzwBits(2, 4, 0, 1, 5)
	plain := slices.Concat(header, frame, []byte{byte(len(stream))}, stream, []byte{0, introTrailer})
	hostile := slices.Concat(header, frame, []byte{byte(len(stream) + len(hiddenText))}, stream, []byte(hiddenText), []byte{0, introTrailer})
	out, rep := clean(t, hostile, core.Options{})
	assertNoHiddenText(t, out)
	if !bytes.Equal(out, plain) {
		t.Fatalf("got %x, want %x", out, plain)
	}
	if !hasField(rep, "Data after the compressed image", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	assertSameFrames(t, plain, out)

	if out, rep := clean(t, plain, core.Options{}); !bytes.Equal(out, plain) || len(rep.Findings) != 0 {
		t.Fatalf("a plain stream must not change: %+v", rep.Findings)
	}
}

// lzwBits packs codes of the given starting width, in the order GIF packs them. The
// caller keeps the stream short enough that the width never changes.
func lzwBits(minCode uint, codes ...uint32) []byte {
	var out []byte
	var acc uint32
	var n uint
	width := minCode + 1
	for _, code := range codes {
		acc |= code << n
		n += width
		for n >= 8 {
			out = append(out, byte(acc))
			acc >>= 8
			n -= 8
		}
	}
	if n > 0 {
		out = append(out, byte(acc))
	}
	return out
}

// Once a frame has all its pixels, a decoder is done with it even if the end code
// never comes, and whatever codes follow are not drawn.
func TestCodesPastTheLastPixelAreRemoved(t *testing.T) {
	frame := []byte{introImage, 0, 0, 0, 0, 2, 0, 1, 0, 0, 2}
	header := append([]byte("GIF89a\x02\x00\x01\x00\x80\x00\x00"), 0, 0, 0, 255, 255, 255)
	complete := lzwBits(2, 4, 0, 1)
	build := func(stream []byte) []byte {
		return slices.Concat(header, frame, []byte{byte(len(stream))}, stream, []byte{0, introTrailer})
	}
	plain := build(complete)
	if out, rep := clean(t, plain, core.Options{}); !bytes.Equal(out, plain) || len(rep.Findings) != 0 {
		t.Fatalf("no end code is a known encoder habit, and must not change the file: %+v", rep.Findings)
	}
	hostile := build(append(lzwBits(2, 4, 0, 1, 1, 1, 1, 1), hiddenText...))
	out, _ := clean(t, hostile, core.Options{})
	assertNoHiddenText(t, out)
	if again, _ := clean(t, out, core.Options{}); !bytes.Equal(again, out) {
		t.Fatal("not idempotent")
	}
	if len(out) >= len(hostile) {
		t.Fatal("nothing was cut")
	}
}

// A clear code and an end code after the last pixel are what many encoders write.
func TestClearCodeBeforeTheEndIsNotData(t *testing.T) {
	frame := []byte{introImage, 0, 0, 0, 0, 2, 0, 1, 0, 0, 2}
	header := append([]byte("GIF89a\x02\x00\x01\x00\x80\x00\x00"), 0, 0, 0, 255, 255, 255)
	stream := lzwBits(2, 4, 0, 1, 4, 5)
	data := slices.Concat(header, frame, []byte{byte(len(stream))}, stream, []byte{0, introTrailer})
	if out, rep := clean(t, data, core.Options{}); !bytes.Equal(out, data) || len(rep.Findings) != 0 {
		t.Fatalf("this stream holds nothing extra: %+v", rep.Findings)
	}
}

// A code the table does not hold yet ends the picture in any decoder, with nothing
// to say what the rest of the sub-blocks is.
func TestInvalidImageDataIsRefused(t *testing.T) {
	frame := []byte{introImage, 0, 0, 0, 0, 2, 0, 1, 0, 0, 2}
	header := append([]byte("GIF89a\x02\x00\x01\x00\x80\x00\x00"), 0, 0, 0, 255, 255, 255)
	for name, stream := range map[string][]byte{
		"code past the table": lzwBits(2, 4, 0, 7, 5),
		"first code too high": lzwBits(2, 4, 7, 5),
	} {
		t.Run(name, func(t *testing.T) {
			refuses(t, slices.Concat(header, frame, []byte{byte(len(stream))}, stream, []byte{0, introTrailer}))
		})
	}
}

// Real encoders run the code table up to its limit. That is where the width grows and
// where an off-by-one in the walk would lose its place, so this needs real data.
func TestLargeFramesPassThroughExactly(t *testing.T) {
	data := largeAnimation(t)
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, data) {
		t.Fatalf("output differs, findings %+v", rep.Findings)
	}
}

func TestStackedGraphicControlIsDropped(t *testing.T) {
	base := animation(t)
	control := func(payload string) []byte {
		return slices.Concat([]byte{introExtension, labelGraphicControl, 4}, []byte(payload), []byte{0})
	}
	first := bytes.Index(base, []byte{introExtension, labelGraphicControl})
	data := slices.Concat(base[:first], control("Test"), control("Hide"), base[first:])
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("only the last one before a frame counts")
	}
	if !hasField(rep, "Unused graphic control", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}

	end := lastFrameEnd(base)
	out, rep = clean(t, slices.Concat(base[:end], control("Test"), base[end:]), core.Options{})
	if !bytes.Equal(out, base) || !hasField(rep, "Unused graphic control", core.ActionRemove) {
		t.Fatalf("one with no frame to apply to must go: %+v", rep.Findings)
	}
}

// Truncated GIFs are common on the web and browsers show what is there. All frames
// that arrived whole stay as they are, the parts of the file that were cut short are
// not copied, and the trailer the file lacks is written.
func TestCutOffFilesAreCleaned(t *testing.T) {
	base := animation(t)
	end := lastFrameEnd(base)
	comment := commentBlock(hiddenText)
	cases := map[string][]byte{
		"no trailer":                base[:end],
		"cut off inside a comment":  slices.Concat(base[:end], comment[:6]),
		"comment without a last 0":  slices.Concat(base[:end], comment[:len(comment)-1]),
		"half an extension":         slices.Concat(base[:end], []byte{introExtension}),
		"half a graphic control":    slices.Concat(base[:end], []byte{introExtension, labelGraphicControl, 4, 1}),
		"half a descriptor":         slices.Concat(base[:end], []byte{introImage, 0, 0}),
		"half a local colour table": slices.Concat(base[:end], []byte{introImage, 0, 0, 0, 0, 1, 0, 1, 0, 0x87, 1, 2}),
		"no image data":             slices.Concat(base[:end], []byte{introImage, 0, 0, 0, 0, 1, 0, 1, 0, 0}),
		"half an application block": slices.Concat(base[:end], applicationBlock("TESTAPP 1.0", []byte(hiddenText))[:20]),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, core.Options{})
			assertNoHiddenText(t, out)
			if !bytes.Equal(out, base) {
				t.Fatalf("got %d bytes, want the %d of the whole animation", len(out), len(base))
			}
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "cut off") {
				t.Fatalf("warnings = %q", rep.Warnings)
			}
			assertIdempotent(t, out)
		})
	}
}

// A frame cut short keeps what arrived, since that is what a browser would draw, and
// the output gets the terminator and the trailer that make it a valid file.
func TestFrameCutOffInsideItsDataIsKept(t *testing.T) {
	base := animation(t)
	_, terminator := firstFrameData(t, base)
	for _, cut := range []int{terminator, terminator - 1, terminator - 10, terminator - 100} {
		out, rep := clean(t, base[:cut], core.Options{})
		if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "cut off") {
			t.Fatalf("cut at %d: warnings = %q", cut, rep.Warnings)
		}
		if !bytes.HasSuffix(out, []byte{0, introTrailer}) {
			t.Fatalf("cut at %d: the output is not terminated", cut)
		}
		assertIdempotent(t, out)
		if got := bytes.Count(out, []byte{introImage}); got == 0 {
			t.Fatalf("cut at %d: the partial frame was lost", cut)
		}
	}
}

func TestStrayBytesBetweenBlocks(t *testing.T) {
	base := animation(t)
	end := lastFrameEnd(base)
	t.Run("zero bytes between frames are stepped over", func(t *testing.T) {
		_, terminator := firstFrameData(t, base)
		out, rep := clean(t, slices.Concat(base[:terminator+1], []byte{0, 0}, base[terminator+1:]), core.Options{})
		if !bytes.Equal(out, base) {
			t.Fatal("the output should be the original animation")
		}
		if !hasField(rep, "Stray bytes", core.ActionRemove) {
			t.Fatalf("no finding: %+v", rep.Findings)
		}
	})
	t.Run("anything else ends the file", func(t *testing.T) {
		out, rep := clean(t, slices.Concat(base[:end], []byte(hiddenText)), core.Options{})
		assertNoHiddenText(t, out)
		if !bytes.Equal(out, base) {
			t.Fatal("the output should be the original animation")
		}
		if !hasField(rep, "Trailing data", core.ActionRemove) || len(rep.Warnings) != 1 {
			t.Fatalf("findings %+v, warnings %q", rep.Findings, rep.Warnings)
		}
	})
	t.Run("a frame past them could be lost, so the original is protected", func(t *testing.T) {
		_, terminator := firstFrameData(t, base)
		_, rep := clean(t, slices.Concat(base[:terminator+1], []byte("x"), base[terminator+1:]), core.Options{})
		if !rep.MayLoseContent || len(rep.Warnings) != 1 {
			t.Fatalf("MayLoseContent = %v, warnings %q", rep.MayLoseContent, rep.Warnings)
		}
	})
	t.Run("before any frame there is no picture to save", func(t *testing.T) {
		refuses(t, insertAtStart(base, []byte(hiddenText)))
	})
}

// largeAnimation encodes noise big enough to fill the LZW table several times over.
func largeAnimation(t *testing.T) []byte {
	t.Helper()
	pal := make(color.Palette, 256)
	for i := range pal {
		pal[i] = color.RGBA{uint8(i), uint8(255 - i), uint8(i * 3), 255}
	}
	rng := rand.New(rand.NewPCG(11, 12))
	var g stdgif.GIF
	for range 3 {
		img := image.NewPaletted(image.Rect(0, 0, 300, 200), pal)
		for i := range img.Pix {
			img.Pix[i] = uint8(rng.UintN(256))
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 1)
	}
	var buf bytes.Buffer
	if err := stdgif.EncodeAll(&buf, &g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// clean prints Field and Where and leaves Value out, so an application's name, which
// any file can set, belongs in Value alone.
func TestFileTextStaysOutOfLabels(t *testing.T) {
	const marker = "TESTMARKER1"
	base := animation(t)
	for name, block := range map[string][]byte{
		"application name": applicationBlock(marker, []byte("x")),
		"comment":          commentBlock(marker),
		"plain text":       plainTextBlock(marker),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, insertAtStart(base, block), core.Options{})
			for _, f := range rep.Findings {
				if strings.Contains(f.Field, marker) || strings.Contains(f.Where, marker) || strings.Contains(f.Reason, marker) {
					t.Errorf("finding %+v repeats text from the file outside Value", f)
				}
			}
			for _, w := range rep.Warnings {
				if strings.Contains(w, marker) {
					t.Errorf("warning %q repeats text from the file", w)
				}
			}
		})
	}
}
