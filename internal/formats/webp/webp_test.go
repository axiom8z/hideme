package webp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/riff"
)

const fakeICCTail = "Test ICC Tail"

// The standard library has no WebP decoder, so bitstreams here are stand-ins that
// carry the right fixed bytes, and tests check they come through byte for byte.
// Real files were checked against Pillow while developing this handler.
func fakeVP8(mark string) []byte {
	out := []byte{0x10, 0, 0, 0x9D, 0x01, 0x2A, 16, 0, 16, 0}
	return append(out, "TestVP8Bits"+mark...)
}

func fakeVP8L(mark string) []byte {
	return append([]byte{0x2F, 15, 0, 0, 0}, "TestVP8LBits"+mark...)
}

func chunk(id string, payload []byte) []byte {
	out := binary.LittleEndian.AppendUint32([]byte(id), uint32(len(payload)))
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func file(chunks ...[]byte) []byte {
	body := bytes.Join(chunks, nil)
	out := binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body)+4))
	out = append(out, "WEBP"...)
	return append(out, body...)
}

func vp8x(flags byte) []byte {
	return chunk(idVP8X, []byte{flags, 0, 0, 0, 15, 0, 0, 15, 0, 0})
}

func fakeICC(size int) []byte {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(size))
	copy(p[36:], "acsp")
	copy(p[size-len(fakeICCTail):], fakeICCTail)
	return p
}

func frame(flags byte, subChunks ...[]byte) []byte {
	header := make([]byte, frameHeader)
	header[6], header[9] = 15, 15 // frame width and height
	header[12] = 100              // duration
	header[frameFlagsOff] = flags
	return chunk(idANMF, slices.Concat(header, bytes.Join(subChunks, nil)))
}

func anim() []byte {
	return chunk(idANIM, []byte{1, 2, 3, 4, 0, 0})
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

func flagsOf(t *testing.T, data []byte) byte {
	t.Helper()
	if id := string(data[12:16]); id != idVP8X {
		t.Fatalf("first chunk is %q, not VP8X", id)
	}
	return data[20]
}

func chunkIDs(t *testing.T, data []byte) []string {
	t.Helper()
	src := core.BytesSource(data)
	root, form, err := riff.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if form != "WEBP" {
		t.Fatalf("form %q", form)
	}
	var ids []string
	for c, err := range riff.Children(context.Background(), src, root) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	return ids
}

func TestContract(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	vp8l := chunk(idVP8L, fakeVP8L(""))
	alph := chunk(idALPH, []byte("Test Alpha Plane"))
	icc := chunk(idICCP, fakeICC(1001))
	cases := []coretest.Case{
		{Name: "simple lossy", Data: file(vp8), MustContain: [][]byte{vp8}},
		{Name: "simple lossless", Data: file(vp8l), MustContain: [][]byte{vp8l}},
		{
			Name:           "simple file with EXIF and XMP after the bitstream",
			Data:           file(vp8, chunk(idEXIF, coretest.FakeEXIF(6)), chunk(idXMP, coretest.FakeXMP())),
			WantFields:     append(coretest.FakeEXIFFields(), "Orientation", "Creator"),
			MustNotContain: slices.Concat(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), [][]byte{[]byte(idEXIF), []byte(idXMP)}),
			MustContain:    [][]byte{vp8},
		},
		{
			Name:           "EXIF and XMP removed",
			Data:           file(vp8x(flagEXIF|0x04), vp8, chunk(idEXIF, coretest.FakeEXIF(0)), chunk(idXMP, coretest.FakeXMP())),
			WantFields:     slices.Concat(coretest.FakeEXIFFields(), coretest.FakeXMPFields()),
			MustNotContain: slices.Concat(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), [][]byte{[]byte(idEXIF), []byte(idXMP)}),
			MustContain:    [][]byte{vp8},
		},
		{
			Name:           "rotated orientation stays as a minimal block",
			Data:           file(vp8x(flagEXIF), vp8, chunk(idEXIF, coretest.FakeEXIF(6))),
			WantFields:     coretest.FakeEXIFFields(),
			WantRetained:   []string{"Orientation"},
			MustNotContain: coretest.FakeEXIFSecrets(),
			MustContain:    [][]byte{vp8, chunk(idEXIF, exif.Minimal(6))},
		},
		{
			Name:         "ICC profile retained",
			Data:         file(vp8x(flagICC), icc, vp8),
			WantRetained: []string{"ICC colour profile"},
			MustContain:  [][]byte{vp8, icc},
		},
		{
			Name:           "extended image with alpha and everything else",
			Data:           file(vp8x(flagICC|flagAlpha|flagEXIF|0x04), icc, alph, vp8, chunk(idEXIF, coretest.FakeEXIF(8)), chunk(idXMP, coretest.FakeXMP()), chunk("TEST", []byte("Test Unknown Chunk"))),
			WantFields:     []string{"GPS coordinates", "Creator", "Other chunk"},
			WantRetained:   []string{"ICC colour profile", "Orientation"},
			MustNotContain: slices.Concat(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), [][]byte{[]byte("Test Unknown Chunk")}),
			MustContain:    [][]byte{vp8, alph, icc},
		},
		{
			Name: "animation with metadata inside and outside the frames",
			Data: file(
				vp8x(flagAnimation|flagAlpha|flagEXIF|0x04), anim(),
				frame(0x01, alph, vp8, chunk("TEST", []byte("Test Inner Chunk"))),
				frame(0x02, vp8l),
				chunk(idEXIF, coretest.FakeEXIF(0)), chunk(idXMP, coretest.FakeXMP()),
			),
			WantFields:     []string{"GPS coordinates", "Creator", "Other chunk"},
			MustNotContain: slices.Concat(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), [][]byte{[]byte("Test Inner Chunk")}),
			MustContain:    [][]byte{vp8, vp8l, alph, anim()},
		},
		{
			Name:           "trailing data",
			Data:           append(file(vp8), "Test Trailing Data"...),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Data")},
			MustContain:    [][]byte{vp8},
		},
		{
			Name:           "unknown chunk after a simple bitstream",
			Data:           file(vp8, chunk("TEST", []byte("Test Unknown Chunk")), chunk(idICCP, fakeICC(100))),
			WantFields:     []string{"Other chunk", "ICC colour profile"},
			MustNotContain: [][]byte{[]byte("Test Unknown Chunk"), []byte(fakeICCTail), []byte(idICCP)},
			MustContain:    [][]byte{vp8},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

func TestFlagsAreRecomputed(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	icc := chunk(idICCP, fakeICC(100))
	alph := chunk(idALPH, []byte("alpha"))
	exifChunk := chunk(idEXIF, coretest.FakeEXIF(6))
	xmpChunk := chunk(idXMP, coretest.FakeXMP())
	const reservedBits = 0xC1
	cases := []struct {
		name string
		data []byte
		opts core.Options
		want byte
	}{
		{"nothing left", file(vp8x(flagEXIF|0x04), vp8, xmpChunk, chunk(idEXIF, coretest.FakeEXIF(0))), core.Options{}, 0},
		{"orientation kept", file(vp8x(flagEXIF|0x04), vp8, exifChunk, xmpChunk), core.Options{}, flagEXIF},
		{"orientation and profile dropped in strict mode", file(vp8x(flagICC|flagEXIF), icc, vp8, exifChunk), core.Options{Strict: true}, 0},
		{"profile kept", file(vp8x(flagICC|flagEXIF), icc, vp8), core.Options{}, flagICC},
		{"alpha flag copied", file(vp8x(flagAlpha), alph, vp8), core.Options{}, flagAlpha},
		{"animation flag copied", file(vp8x(flagAnimation), anim(), frame(0, vp8)), core.Options{}, flagAnimation},
		{"reserved bits cleared", file(vp8x(flagAlpha|reservedBits), vp8), core.Options{}, flagAlpha},
		{"flags without chunks do not survive", file(vp8x(flagICC|flagEXIF|0x04), vp8), core.Options{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := clean(t, tc.data, tc.opts)
			if got := flagsOf(t, out); got != tc.want {
				t.Fatalf("flags are %#02x, want %#02x", got, tc.want)
			}
		})
	}
}

func TestUnannouncedDataIsNotKept(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	// Without the VP8X flag readers skip these chunks, so keeping them would
	// change how the picture displays.
	data := file(vp8x(0), chunk(idICCP, fakeICC(100)), vp8, chunk(idEXIF, coretest.FakeEXIF(6)))
	out, rep := clean(t, data, core.Options{})
	if want := file(vp8x(0), vp8); !bytes.Equal(out, want) {
		t.Fatalf("got %d bytes, want %d", len(out), len(want))
	}
	if !hasField(rep, "ICC colour profile", core.ActionRemove) || !hasField(rep, "Orientation", core.ActionRemove) {
		t.Fatalf("both must be reported as removed: %+v", rep.Findings)
	}
}

func TestOnlyFirstEXIFDecidesOrientation(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	data := file(vp8x(flagEXIF), vp8, chunk(idEXIF, coretest.FakeEXIF(0)), chunk(idEXIF, coretest.FakeEXIF(6)))
	out, rep := clean(t, data, core.Options{})
	if want := file(vp8x(0), vp8); !bytes.Equal(out, want) {
		t.Fatal("a second EXIF chunk changed the output")
	}
	if hasField(rep, "Orientation", core.ActionRetain) {
		t.Fatalf("the second block's orientation was described as kept: %+v", rep.Findings)
	}
}

func TestPrefixedEXIFIsUnderstood(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	block := append([]byte(exif.Prefix), coretest.FakeEXIF(6)...)
	out, rep := clean(t, file(vp8x(flagEXIF), vp8, chunk(idEXIF, block)), core.Options{})
	if !bytes.Contains(out, chunk(idEXIF, exif.Minimal(6))) {
		t.Fatal("the orientation in a prefixed block was lost")
	}
	if !hasField(rep, "GPS coordinates", core.ActionRemove) {
		t.Fatalf("a prefixed block was not decoded: %+v", rep.Findings)
	}
}

func TestProfileHandling(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	first := chunk(idICCP, fakeICC(100))
	second := chunk(idICCP, fakeICC(120))
	notAProfile := chunk(idICCP, []byte("Test Not A Profile"))
	wrongSize := fakeICC(100)
	wrongSize[3]++
	cases := []struct {
		name  string
		data  []byte
		want  []byte
		field string
	}{
		{"second profile", file(vp8x(flagICC), first, second, vp8), file(vp8x(flagICC), first, vp8), "Extra ICC profile"},
		{"not a profile", file(vp8x(flagICC), notAProfile, vp8), file(vp8x(0), vp8), "Damaged ICC profile"},
		{"wrong size", file(vp8x(flagICC), chunk(idICCP, wrongSize), vp8), file(vp8x(0), vp8), "Damaged ICC profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, rep := clean(t, tc.data, core.Options{})
			if !bytes.Equal(out, tc.want) {
				t.Fatalf("got %d bytes, want %d", len(out), len(tc.want))
			}
			if !hasField(rep, tc.field, core.ActionRemove) {
				t.Fatalf("field %q not reported: %+v", tc.field, rep.Findings)
			}
		})
	}
}

func TestPadBytesAreZeroed(t *testing.T) {
	odd := fakeVP8("")
	if len(odd)%2 == 0 {
		t.Fatal("the fixture must have an odd size")
	}
	dirty := chunk(idVP8, odd)
	dirty[len(dirty)-1] = 'S' // a secret in the pad byte
	out, _ := clean(t, file(dirty), core.Options{})
	if want := file(chunk(idVP8, odd)); !bytes.Equal(out, want) {
		t.Fatalf("output %q, want %q", out, want)
	}

	inner := chunk(idVP8, odd)
	inner[len(inner)-1] = 'S'
	data := file(vp8x(flagAnimation), anim(), frame(0, inner))
	out, _ = clean(t, data, core.Options{})
	if want := file(vp8x(flagAnimation), anim(), frame(0, chunk(idVP8, odd))); !bytes.Equal(out, want) {
		t.Fatalf("the pad inside a frame was not zeroed: %q", out)
	}
}

func TestMissingFinalPadIsAdded(t *testing.T) {
	odd := fakeVP8("")
	raw := binary.LittleEndian.AppendUint32([]byte(idVP8), uint32(len(odd)))
	raw = append(raw, odd...)
	data := file(raw) // no pad byte at the very end, as many writers leave it
	out, rep := clean(t, data, core.Options{})
	if want := file(chunk(idVP8, odd)); !bytes.Equal(out, want) {
		t.Fatalf("output %q, want %q", out, want)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("got findings %+v, want none", rep.Findings)
	}
}

func TestRIFFSizeIsRecomputed(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	out, _ := clean(t, append(file(vp8, chunk("TEST", make([]byte, 100))), make([]byte, 50)...), core.Options{})
	if got, want := binary.LittleEndian.Uint32(out[4:]), uint32(len(out)-8); got != want {
		t.Fatalf("RIFF size %d, want %d", got, want)
	}
}

func TestFrameContentIsPreserved(t *testing.T) {
	vp8, alph := chunk(idVP8, fakeVP8("frame1")), chunk(idALPH, []byte("Test Alpha"))
	vp8l := chunk(idVP8L, fakeVP8L("frame2"))
	first := frame(0x01, alph, vp8)
	second := frame(0x02, vp8l)
	data := file(vp8x(flagAnimation|flagAlpha), anim(), first, second)
	out, _ := clean(t, data, core.Options{})
	if !bytes.Equal(out, data) {
		t.Fatal("a clean animation must pass through unchanged")
	}
	if got := chunkIDs(t, out); !slices.Equal(got, []string{idVP8X, idANIM, idANMF, idANMF}) {
		t.Fatalf("chunks are %q", got)
	}
}

func TestFrameReservedBitsAreCleared(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	out, _ := clean(t, file(vp8x(flagAnimation), anim(), frame(0xFC|0x01, vp8)), core.Options{})
	if want := file(vp8x(flagAnimation), anim(), frame(0x01, vp8)); !bytes.Equal(out, want) {
		t.Fatal("reserved frame bits are a place for data to survive")
	}
}

func TestUnparseableIsRefused(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	vp8l := chunk(idVP8L, fakeVP8L(""))
	alph := chunk(idALPH, []byte("alpha"))
	badVP8 := chunk(idVP8, []byte("Test Not A Bitstream"))
	badVP8L := chunk(idVP8L, []byte("Test Not A Bitstream"))
	wrongForm := append(binary.LittleEndian.AppendUint32([]byte("RIFF"), 4), "WAVE"...)
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, core.ErrMalformed},
		{"not RIFF", []byte("definitely not RIFF"), core.ErrUnsupported},
		{"big-endian RIFX", append([]byte("RIFX"), make([]byte, 8)...), core.ErrUnsupported},
		{"wrong form", wrongForm, core.ErrUnsupported},
		{"size past the end", binary.LittleEndian.AppendUint32([]byte("RIFF"), 1000), core.ErrMalformed},
		{"RIFF size past the file", append(binary.LittleEndian.AppendUint32([]byte("RIFF"), 1000), "WEBP"...), core.ErrMalformed},
		{"no chunks", file(), core.ErrMalformed},
		{"chunk overruns the file", file(binary.LittleEndian.AppendUint32([]byte(idVP8), 1000)), core.ErrMalformed},
		{"stray bytes", file(vp8, []byte("xx")), core.ErrMalformed},
		{"first chunk is metadata", file(chunk(idEXIF, coretest.FakeEXIF(0)), vp8), core.ErrMalformed},
		{"bad lossy bitstream", file(badVP8), core.ErrMalformed},
		{"bad lossless bitstream", file(badVP8L), core.ErrMalformed},
		{"short bitstreams", file(chunk(idVP8, []byte{1})), core.ErrMalformed},
		{"short lossless bitstream", file(chunk(idVP8L, []byte{0x2F})), core.ErrMalformed},
		{"short VP8X", file(chunk(idVP8X, []byte{0}), vp8), core.ErrMalformed},
		{"second VP8X", file(vp8x(0), vp8x(0), vp8), core.ErrMalformed},
		{"no image", file(vp8x(0)), core.ErrMalformed},
		{"two images", file(vp8x(0), vp8, vp8), core.ErrMalformed},
		{"alpha after the image", file(vp8x(0), vp8, alph), core.ErrMalformed},
		{"two alpha planes", file(vp8x(0), alph, alph, vp8), core.ErrMalformed},
		{"alpha with lossless", file(vp8x(0), alph, vp8l), core.ErrMalformed},
		{"bad bitstream in VP8X file", file(vp8x(0), badVP8), core.ErrMalformed},
		{"animation flag with no frames", file(vp8x(flagAnimation), anim()), core.ErrMalformed},
		{"animation flag with no ANIM", file(vp8x(flagAnimation), vp8), core.ErrMalformed},
		{"animation chunks with no flag", file(vp8x(0), anim(), frame(0, vp8)), core.ErrMalformed},
		{"animation and still image", file(vp8x(flagAnimation), anim(), frame(0, vp8), vp8), core.ErrMalformed},
		{"frame before ANIM", file(vp8x(flagAnimation), frame(0, vp8), anim()), core.ErrMalformed},
		{"second ANIM", file(vp8x(flagAnimation), anim(), anim(), frame(0, vp8)), core.ErrMalformed},
		{"ANIM wrong size", file(vp8x(flagAnimation), chunk(idANIM, []byte{1}), frame(0, vp8)), core.ErrMalformed},
		{"frame too short", file(vp8x(flagAnimation), anim(), chunk(idANMF, []byte{1})), core.ErrMalformed},
		{"frame with no image", file(vp8x(flagAnimation), anim(), frame(0)), core.ErrMalformed},
		{"frame with two images", file(vp8x(flagAnimation), anim(), frame(0, vp8, vp8)), core.ErrMalformed},
		{"frame with bad bitstream", file(vp8x(flagAnimation), anim(), frame(0, badVP8)), core.ErrMalformed},
		{"frame with alpha after image", file(vp8x(flagAnimation), anim(), frame(0, vp8, alph)), core.ErrMalformed},
		{"frame with two alpha planes", file(vp8x(flagAnimation), anim(), frame(0, alph, alph, vp8)), core.ErrMalformed},
		{"frame with alpha and lossless", file(vp8x(flagAnimation), anim(), frame(0, alph, vp8l)), core.ErrMalformed},
		{"frame with stray bytes", file(vp8x(flagAnimation), anim(), chunk(idANMF, slices.Concat(make([]byte, frameHeader), vp8, []byte("x")))), core.ErrMalformed},
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

func TestTooManyChunks(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	junk := chunk("TEST", nil)
	data := file(vp8, bytes.Repeat(junk, maxChunks+1))
	var out bytes.Buffer
	_, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestFindingsAreCapped(t *testing.T) {
	vp8 := chunk(idVP8, fakeVP8(""))
	data := file(vp8, bytes.Repeat(chunk("TEST", nil), maxFindings+7))
	_, rep := clean(t, data, core.Options{})
	if len(rep.Findings) != maxFindings+1 {
		t.Fatalf("got %d findings, want %d", len(rep.Findings), maxFindings+1)
	}
	if last := rep.Findings[len(rep.Findings)-1]; last.Field != "Further metadata" || last.Value != "7 more items" {
		t.Fatalf("the overflow finding is %+v", last)
	}
}

func TestByteCountWording(t *testing.T) {
	if got := core.ByteCount(1); got != "1 byte" {
		t.Errorf("core.ByteCount(1) = %q", got)
	}
	if got := core.ByteCount(int64(2)); got != "2 bytes" {
		t.Errorf("core.ByteCount(2) = %q", got)
	}
}

func TestRegisteredAndMatching(t *testing.T) {
	h := handler{}
	if !h.Match(file(chunk(idVP8, fakeVP8(""))), nil) {
		t.Error("did not match WebP")
	}
	if h.Match([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), nil) {
		t.Error("matched a WAV file")
	}
	if h.Match([]byte("RIFF"), nil) {
		t.Error("matched a truncated header")
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
	_, err := handler{}.Clean(context.Background(), core.BytesSource(file(chunk(idVP8, fakeVP8("")))), failingWriter{}, core.Options{})
	if err == nil {
		t.Fatal("a failing writer must fail Clean")
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := handler{}.Inspect(ctx, core.BytesSource(file(chunk(idVP8, fakeVP8("")))), core.Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func extendedSample() []byte {
	vp8 := chunk(idVP8, fakeVP8(""))
	return file(vp8x(flagICC|flagAlpha|flagEXIF|0x04), chunk(idICCP, fakeICC(100)), chunk(idALPH, []byte("alpha")), vp8,
		chunk(idEXIF, coretest.FakeEXIF(6)), chunk(idXMP, coretest.FakeXMP()), chunk("TEST", []byte("Test Unknown")))
}

func animatedSample() []byte {
	vp8 := chunk(idVP8, fakeVP8(""))
	return file(vp8x(flagAnimation|flagAlpha), anim(),
		frame(0x01, chunk(idALPH, []byte("alpha")), vp8, chunk("TEST", []byte("Test Inner"))),
		frame(0x02, chunk(idVP8L, fakeVP8L(""))),
		chunk(idEXIF, coretest.FakeEXIF(0)))
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, extendedSample())
	coretest.Robustness(t, handler{}, animatedSample())
}

func FuzzClean(f *testing.F) {
	f.Add(extendedSample())
	f.Add(animatedSample())
	f.Add(file(chunk(idVP8, fakeVP8("x")), chunk(idEXIF, coretest.FakeEXIF(6))))
	f.Add(append(file(chunk(idVP8L, fakeVP8L(""))), "Test Trailing"...))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, handler{}, data)
	})
}
