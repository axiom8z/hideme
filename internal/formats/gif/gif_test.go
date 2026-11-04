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
	"time"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const fakeComment = "Test Comment Secret"

// animation returns a real three-frame GIF with a local colour table on one frame
// and a loop count, as encoded by image/gif.
func animation(t testing.TB) []byte {
	t.Helper()
	global := color.Palette{color.Black, color.White, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}}
	local := color.Palette{color.RGBA{1, 2, 3, 255}, color.RGBA{4, 5, 6, 255}, color.RGBA{7, 8, 9, 255}, color.RGBA{10, 11, 12, 255}}
	rng := rand.New(rand.NewPCG(7, 8))
	frame := func(pal color.Palette) *image.Paletted {
		img := image.NewPaletted(image.Rect(0, 0, 32, 24), pal)
		for i := range img.Pix {
			img.Pix[i] = uint8(rng.UintN(uint(len(pal))))
		}
		return img
	}
	var buf bytes.Buffer
	err := stdgif.EncodeAll(&buf, &stdgif.GIF{
		Image:     []*image.Paletted{frame(global), frame(local), frame(global)},
		Delay:     []int{5, 10, 15},
		Disposal:  []byte{stdgif.DisposalNone, stdgif.DisposalBackground, stdgif.DisposalPrevious},
		LoopCount: 3,
		Config:    image.Config{ColorModel: global, Width: 32, Height: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// firstBlock is where metadata goes in these fixtures: after the header and the
// global colour table of a four-colour palette.
const firstBlock = headerSize + screenSize + 3*4

func insertAtStart(base []byte, blocks ...[]byte) []byte {
	out := bytes.Clone(base[:firstBlock])
	for _, block := range blocks {
		out = append(out, block...)
	}
	return append(out, base[firstBlock:]...)
}

func subBlocksOf(payload []byte) []byte {
	var out []byte
	for len(payload) > 0 {
		n := min(len(payload), 255)
		out = append(out, byte(n))
		out = append(out, payload[:n]...)
		payload = payload[n:]
	}
	return append(out, 0)
}

func commentBlock(text string) []byte {
	return append([]byte{introExtension, labelComment}, subBlocksOf([]byte(text))...)
}

func applicationBlock(id string, payload []byte) []byte {
	if len(id) != applicationID {
		panic("application ID must be 11 bytes")
	}
	out := append([]byte{introExtension, labelApplication, applicationID}, id...)
	return append(out, subBlocksOf(payload)...)
}

func xmpBlock(packet []byte) []byte {
	out := append([]byte{introExtension, labelApplication, applicationID}, "XMP DataXMP"...)
	out = append(out, packet...)
	return append(out, xmpMagic...)
}

func plainTextBlock(text string) []byte {
	out := []byte{introExtension, labelPlainText, plainTextHeader}
	out = append(out, make([]byte, plainTextHeader)...)
	return append(out, subBlocksOf([]byte(text))...)
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

func hasField(rep *core.Report, field string, action core.Action) bool {
	return slices.ContainsFunc(rep.Findings, func(f core.Finding) bool {
		return f.Field == field && f.Action == action
	})
}

func TestContract(t *testing.T) {
	base := animation(t)
	loop := applicationBlock("NETSCAPE2.0", []byte{1, 3, 0})
	cases := []coretest.Case{
		{Name: "no metadata", Data: base, MustContain: [][]byte{base}},
		{
			Name:           "comment",
			Data:           insertAtStart(base, commentBlock(fakeComment)),
			WantFields:     []string{"Comment"},
			MustNotContain: [][]byte{[]byte(fakeComment)},
			MustContain:    [][]byte{base},
		},
		{
			Name:           "long comment across sub-blocks",
			Data:           insertAtStart(base, commentBlock(strings.Repeat("Test Long Comment ", 40))),
			WantFields:     []string{"Comment"},
			MustNotContain: [][]byte{[]byte("Test Long Comment")},
			MustContain:    [][]byte{base},
		},
		{
			Name:           "XMP with the magic trailer",
			Data:           insertAtStart(base, xmpBlock(coretest.FakeXMP())),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: append(coretest.FakeXMPSecrets(), []byte("XMP Data")),
			MustContain:    [][]byte{base},
		},
		{
			Name:           "XMP written as real sub-blocks",
			Data:           insertAtStart(base, applicationBlock("XMP DataXMP", coretest.FakeXMP())),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: [][]byte{[]byte("XMP Data")},
			MustContain:    [][]byte{base},
		},
		{
			Name:           "other application extensions",
			Data:           insertAtStart(base, applicationBlock("TESTAPP 1.0", []byte("Test App Data")), applicationBlock("ICCRGBG1012", []byte("Test Profile"))),
			WantFields:     []string{"Application data"},
			MustNotContain: [][]byte{[]byte("TESTAPP"), []byte("Test App Data"), []byte("ICCRGBG1012"), []byte("Test Profile")},
			MustContain:    [][]byte{base},
		},
		{
			Name:           "unknown extension",
			Data:           insertAtStart(base, append([]byte{introExtension, 0x77}, subBlocksOf([]byte("Test Unknown Extension"))...)),
			WantFields:     []string{"Other extension"},
			MustNotContain: [][]byte{[]byte("Test Unknown Extension")},
			MustContain:    [][]byte{base},
		},
		{
			Name:           "trailing data",
			Data:           append(bytes.Clone(base), "Test Trailing Data"...),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Data")},
			MustContain:    [][]byte{base},
		},
		{
			Name:        "Plain Text extension is part of the picture",
			Data:        insertAtStart(base, plainTextBlock("Visible Text")),
			MustContain: [][]byte{[]byte("Visible Text")},
		},
		{
			Name:        "the loop extensions survive",
			Data:        insertAtStart(base, loop, applicationBlock("ANIMEXTS1.0", []byte{1, 0, 0}), applicationBlock("NETSCAPE2.0", []byte{2, 1, 2, 3, 4})),
			MustContain: [][]byte{loop, applicationBlock("ANIMEXTS1.0", []byte{1, 0, 0}), applicationBlock("NETSCAPE2.0", []byte{2, 1, 2, 3, 4})},
		},
		{
			Name:           "a loop extension hiding data is removed",
			Data:           insertAtStart(base, applicationBlock("NETSCAPE2.0", []byte("Test Hidden Loop Data"))),
			WantFields:     []string{"Application data"},
			MustNotContain: [][]byte{[]byte("Test Hidden Loop Data")},
			MustContain:    [][]byte{base},
		},
		{
			Name:           "metadata between frames",
			Data:           commentsBetweenFrames(t, base),
			WantFields:     []string{"Comment"},
			MustNotContain: [][]byte{[]byte(fakeComment)},
			MustContain:    [][]byte{base},
		},
		{
			Name:        "GIF87a",
			Data:        append([]byte("GIF87a"), base[6:]...),
			MustContain: [][]byte{[]byte("GIF87a")},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

// commentsBetweenFrames adds a comment before each image descriptor.
func commentsBetweenFrames(t testing.TB, base []byte) []byte {
	t.Helper()
	const graphicControlStart = "\x21\xF9\x04"
	var out []byte
	rest := base
	for {
		i := bytes.Index(rest, []byte(graphicControlStart))
		if i < 0 {
			break
		}
		out = append(out, rest[:i]...)
		out = append(out, commentBlock(fakeComment)...)
		out = append(out, graphicControlStart...)
		rest = rest[i+len(graphicControlStart):]
	}
	return append(out, rest...)
}

func TestFramesSurvive(t *testing.T) {
	base := animation(t)
	dirty := insertAtStart(commentsBetweenFrames(t, base),
		commentBlock(fakeComment), xmpBlock(coretest.FakeXMP()), applicationBlock("TESTAPP 1.0", []byte("Test App Data")))
	dirty = append(dirty, "Test Trailing Data"...)
	before, err := stdgif.DecodeAll(bytes.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdgif.DecodeAll(bytes.NewReader(bytes.TrimSuffix(dirty, []byte("Test Trailing Data")))); err != nil {
		t.Fatalf("the dirty fixture must decode as a GIF, or the test proves nothing: %v", err)
	}
	for _, opts := range []core.Options{{}, {Strict: true}} {
		out, _ := clean(t, dirty, opts)
		if !bytes.Equal(out, base) {
			t.Fatalf("the output is not the original image: %d bytes, want %d", len(out), len(base))
		}
		after, err := stdgif.DecodeAll(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		assertSameGIF(t, before, after)
	}
}

func assertSameGIF(t *testing.T, a, b *stdgif.GIF) {
	t.Helper()
	if len(a.Image) != len(b.Image) {
		t.Fatalf("frame count changed from %d to %d", len(a.Image), len(b.Image))
	}
	if a.LoopCount != b.LoopCount || a.BackgroundIndex != b.BackgroundIndex || a.Config.Width != b.Config.Width || a.Config.Height != b.Config.Height {
		t.Fatal("animation settings changed")
	}
	if !slices.Equal(a.Delay, b.Delay) || !slices.Equal(a.Disposal, b.Disposal) {
		t.Fatal("frame timing changed")
	}
	for i := range a.Image {
		x, y := a.Image[i], b.Image[i]
		if x.Rect != y.Rect || !bytes.Equal(x.Pix, y.Pix) {
			t.Fatalf("frame %d's pixels changed", i)
		}
		if len(x.Palette) != len(y.Palette) {
			t.Fatalf("frame %d's palette changed", i)
		}
		for j := range x.Palette {
			if x.Palette[j] != y.Palette[j] {
				t.Fatalf("frame %d's palette entry %d changed", i, j)
			}
		}
	}
}

func TestLoopFixtureIsExercised(t *testing.T) {
	base := animation(t)
	if !bytes.Contains(base, []byte("NETSCAPE2.0")) {
		t.Fatal("the fixture no longer has a loop extension")
	}
	decoded, err := stdgif.DecodeAll(bytes.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.LoopCount != 3 {
		t.Fatalf("loop count %d", decoded.LoopCount)
	}
	if len(decoded.Image[1].Palette) != 4 || decoded.Image[1].Palette[0] == decoded.Image[0].Palette[0] {
		t.Fatal("the second frame should have its own colour table")
	}
}

func TestXMPIsDecodedExactly(t *testing.T) {
	base := animation(t)
	_, rep := clean(t, insertAtStart(base, xmpBlock(coretest.FakeXMP())), core.Options{})
	found := slices.ContainsFunc(rep.Findings, func(f core.Finding) bool {
		return f.Field == "Creator" && f.Value == "Test Author"
	})
	if !found {
		t.Fatalf("XMP was not decoded: %+v", rep.Findings)
	}
	if hasField(rep, "XMP packet", core.ActionRemove) {
		t.Fatalf("the trailer must not make a good packet look damaged: %+v", rep.Findings)
	}
}

func TestXMPLandingOnEitherZeroOfTheTrailer(t *testing.T) {
	base := animation(t)
	for _, length := range []int{100, 101, 300, 301, 302} {
		packet := []byte(strings.Repeat(" ", length))
		out, _ := clean(t, insertAtStart(base, xmpBlock(packet)), core.Options{})
		if !bytes.Equal(out, base) {
			t.Fatalf("a packet of %d bytes threw off the walk", length)
		}
	}
}

func TestStdlibReadsThroughTheMagicTrailer(t *testing.T) {
	// This confirms that the trailer's reason for existing works as the code
	// comment says, so a decoder with no knowledge of XMP skips the extension.
	base := animation(t)
	if _, err := stdgif.DecodeAll(bytes.NewReader(insertAtStart(base, xmpBlock(coretest.FakeXMP())))); err != nil {
		t.Fatal(err)
	}
}

func TestCommentEncodingsAreDecoded(t *testing.T) {
	base := animation(t)
	cases := map[string]string{
		"Caf\xC3\xA9": "Café",
		"Caf\xE9":     "Café",
	}
	for raw, want := range cases {
		_, rep := clean(t, insertAtStart(base, commentBlock(raw)), core.Options{})
		if got := rep.Findings[0].Value; got != want {
			t.Errorf("comment %q decoded to %q, want %q", raw, got, want)
		}
	}
}

func TestValidLoopData(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"loop count", []byte{3, 1, 0, 0, 0}, true},
		{"buffer size", []byte{5, 2, 0, 0, 0, 0, 0}, true},
		{"both", []byte{3, 1, 0, 0, 5, 2, 0, 0, 0, 0, 0}, true},
		{"nothing", []byte{0}, true},
		{"two loop counts", []byte{3, 1, 0, 0, 3, 1, 0, 0, 0}, false},
		{"two buffer sizes", []byte{5, 2, 0, 0, 0, 0, 5, 2, 0, 0, 0, 0, 0}, false},
		{"unknown ID", []byte{3, 9, 0, 0, 0}, false},
		{"long loop count", []byte{4, 1, 0, 0, 0, 0}, false},
		{"short buffer size", []byte{3, 2, 0, 0, 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validLoopData(tc.raw); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnparseableIsRefused(t *testing.T) {
	base := animation(t)
	screen := func(flags byte) []byte {
		return append([]byte("GIF89a\x01\x00\x01\x00"), flags, 0, 0)
	}
	image := func(flags byte, rest ...byte) []byte {
		return append([]byte{introImage, 0, 0, 0, 0, 1, 0, 1, 0, flags}, rest...)
	}
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, core.ErrUnsupported},
		{"not GIF", []byte("not a gif file at all"), core.ErrUnsupported},
		{"wrong version", append([]byte("GIF90a"), base[6:]...), core.ErrUnsupported},
		{"short header", []byte("GIF89a\x01\x00"), core.ErrMalformed},
		{"colour table overruns", screen(0x87), core.ErrMalformed},
		{"no images", slices.Concat(screen(0), []byte{introTrailer}), core.ErrMalformed},
		{"only a comment", slices.Concat(screen(0), commentBlock("x"), []byte{introTrailer}), core.ErrMalformed},
		{"unknown block", insertAtStart(base, []byte{0x55}), core.ErrMalformed},
		{"truncated descriptor", slices.Concat(screen(0), []byte{introImage, 0, 0}), core.ErrMalformed},
		{"local table overruns", slices.Concat(screen(0), image(0x87)), core.ErrMalformed},
		{"no LZW width", slices.Concat(screen(0), image(0)), core.ErrMalformed},
		{"LZW width zero", slices.Concat(screen(0), image(0, 0, 0), []byte{introTrailer}), core.ErrMalformed},
		{"LZW width too large", slices.Concat(screen(0), image(0, 12, 0), []byte{introTrailer}), core.ErrMalformed},
		{"truncated extension", slices.Concat(screen(0), []byte{introExtension}), core.ErrMalformed},
		{"unknown extension overruns", slices.Concat(screen(0), []byte{introExtension, 0x77, 200, 1}), core.ErrMalformed},
		{"short graphic control", slices.Concat(screen(0), []byte{introExtension, labelGraphicControl, 4, 0}), core.ErrMalformed},
		{"graphic control hiding data", slices.Concat(screen(0), []byte{introExtension, labelGraphicControl, 5, 1, 2, 3, 4, 5, 0}), core.ErrMalformed},
		{"graphic control with no terminator", slices.Concat(screen(0), []byte{introExtension, labelGraphicControl, 4, 1, 2, 3, 4, 9}), core.ErrMalformed},
		{"plain text wrong size", slices.Concat(screen(0), []byte{introExtension, labelPlainText, 5, 1, 2, 3, 4, 5, 0}), core.ErrMalformed},
		{"truncated plain text", slices.Concat(screen(0), []byte{introExtension, labelPlainText, plainTextHeader, 1}), core.ErrMalformed},
		{"plain text overruns", slices.Concat(screen(0), []byte{introExtension, labelPlainText, plainTextHeader}, make([]byte, plainTextHeader), []byte{9, 1}), core.ErrMalformed},
		{"comment overruns", slices.Concat(screen(0), []byte{introExtension, labelComment, 9, 1}), core.ErrMalformed},
		{"application overruns", slices.Concat(screen(0), []byte{introExtension, labelApplication, applicationID, 'x'}), core.ErrMalformed},
		{"application data overruns", slices.Concat(screen(0), []byte{introExtension, labelApplication, applicationID}, []byte("TESTAPP 1.0"), []byte{9, 1}), core.ErrMalformed},
		{"XMP with no end", slices.Concat(screen(0), []byte{introExtension, labelApplication, applicationID}, []byte("XMP DataXMP"), []byte{9, 1}), core.ErrMalformed},
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

// Many XMP extensions with no trailer must not each rescan the rest of the file.
// Nothing here fails if the walk is linear; if it is not, the test takes minutes
// and trips the limit.
func TestUnterminatedXMPDoesNotMakeTheWalkQuadratic(t *testing.T) {
	base := animation(t)
	block := []byte{introExtension, labelApplication, applicationID}
	block = append(append(block, "XMP DataXMP"...), 0)
	data := insertAtStart(base, bytes.Repeat(block, 400000))
	start := time.Now()
	out, _ := clean(t, data, core.Options{})
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("the extensions must be dropped")
	}
}

func TestOddApplicationBlockSizeIsRemoved(t *testing.T) {
	base := animation(t)
	block := append([]byte{introExtension, labelApplication}, subBlocksOf([]byte("Test Odd Application Block"))...)
	out, rep := clean(t, insertAtStart(base, block), core.Options{})
	if !bytes.Equal(out, base) {
		t.Fatal("the block must be dropped")
	}
	if !hasField(rep, "Application data", core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
}

func TestTooManyBlocks(t *testing.T) {
	base := animation(t)
	data := append(bytes.Clone(base[:firstBlock]), bytes.Repeat(commentBlock(""), maxBlocks+1)...)
	data = append(data, base[firstBlock:]...)
	var out bytes.Buffer
	_, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestFindingsAreCapped(t *testing.T) {
	base := animation(t)
	data := append(bytes.Clone(base[:firstBlock]), bytes.Repeat(commentBlock("x"), maxFindings+9)...)
	data = append(data, base[firstBlock:]...)
	_, rep := clean(t, data, core.Options{})
	if len(rep.Findings) != maxFindings+1 {
		t.Fatalf("got %d findings, want %d", len(rep.Findings), maxFindings+1)
	}
	if last := rep.Findings[len(rep.Findings)-1]; last.Field != "Further metadata" || last.Value != "9 more items" {
		t.Fatalf("the overflow finding is %+v", last)
	}
}

func TestByteCountWording(t *testing.T) {
	if got := core.ByteCount(1); got != "1 byte" {
		t.Errorf("core.ByteCount(1) = %q", got)
	}
}

func TestRegisteredAndMatching(t *testing.T) {
	h := handler{}
	if !h.Match([]byte("GIF87a"), nil) || !h.Match([]byte("GIF89a"), nil) {
		t.Error("did not match the GIF signatures")
	}
	if h.Match([]byte("GIF8"), nil) || h.Match([]byte("\xFF\xD8\xFF"), nil) {
		t.Error("matched something else")
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
	_, err := handler{}.Clean(context.Background(), core.BytesSource(animation(t)), failingWriter{}, core.Options{})
	if err == nil {
		t.Fatal("a failing writer must fail Clean")
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := handler{}.Inspect(ctx, core.BytesSource(animation(t)), core.Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func fullSample(t testing.TB) []byte {
	base := commentsBetweenFrames(t, animation(t))
	out := insertAtStart(base,
		xmpBlock(coretest.FakeXMP()),
		applicationBlock("TESTAPP 1.0", []byte("Test App Data")),
		applicationBlock("ANIMEXTS1.0", []byte{1, 0, 0}),
		plainTextBlock("Visible Text"),
		append([]byte{introExtension, 0x77}, subBlocksOf([]byte("Test Unknown"))...),
	)
	return append(out, "Test Trailing Data"...)
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, fullSample(t))
}

func FuzzClean(f *testing.F) {
	f.Add(fullSample(f))
	f.Add(animation(f))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, handler{}, data)
	})
}
