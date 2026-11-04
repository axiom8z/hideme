package wav

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/audiotag/tagtest"
	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const (
	formatMPEG       = 0x0050
	formatMPEGLayer3 = 0x0055
)

func chunk(id string, body []byte) []byte {
	out := binary.LittleEndian.AppendUint32([]byte(id), uint32(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func riffFile(chunks ...[]byte) []byte {
	body := append([]byte(formWAVE), bytes.Join(chunks, nil)...)
	out := binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body)))
	return append(out, body...)
}

// fmtBody describes 16-bit mono PCM at 44.1 kHz.
func fmtBody() []byte {
	return fmtWithTag(1)
}

func fmtWithTag(tag uint16) []byte {
	out := binary.LittleEndian.AppendUint16(nil, tag)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint32(out, 44100)
	out = binary.LittleEndian.AppendUint32(out, 88200)
	out = binary.LittleEndian.AppendUint16(out, 2)
	return binary.LittleEndian.AppendUint16(out, 16)
}

func samples(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i * 7)
	}
	return out
}

func infoList() []byte {
	body := []byte("INFO")
	for _, item := range []struct{ id, value string }{
		{"IART", tagtest.Fake},
		{"ICMT", "Test Comment"},
		{"ICRD", "2001-02-03"},
		{"INAM", "Test Title"},
		{"ISFT", "Test Software"},
		{"ITCH", "Test Technician"},
		{"IZZZ", "Test Other"},
	} {
		body = append(body, chunk(item.id, append([]byte(item.value), 0))...)
	}
	return body
}

// bextBody builds a Broadcast Wave chunk with its fixed layout.
func bextBody() []byte {
	fixed := func(s string, n int) []byte {
		out := make([]byte, n)
		copy(out, s)
		return out
	}
	umid := make([]byte, 64)
	umid[0] = 0x06
	return bytes.Join([][]byte{
		fixed("Test Bext Description", 256),
		fixed("Test Originator", 32),
		fixed("Test Reference", 32),
		fixed("2001-02-03", 10),
		fixed("04:05:06", 8),
		make([]byte, 8), // time reference
		{1, 0},          // version
		umid,
		make([]byte, 10),  // loudness
		make([]byte, 180), // reserved
		[]byte("Test Coding History\r\n"),
	}, nil)
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func clean(t *testing.T, data []byte) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func value(t *testing.T, rep *core.Report, field string) string {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Field == field {
			return f.Value
		}
	}
	t.Errorf("field %q not reported: %+v", field, rep.Findings)
	return ""
}

func TestContract(t *testing.T) {
	pcm := samples(1000)
	fmtChunk := chunk(idFmt, fmtBody())
	dataChunk := chunk(idData, pcm)

	cases := []coretest.Case{
		{Name: "no metadata", Data: riffFile(fmtChunk, dataChunk), MustContain: [][]byte{pcm}},
		{
			Name:        "fact chunk kept",
			Data:        riffFile(fmtChunk, chunk(idFact, []byte{1, 2, 3, 4}), dataChunk),
			MustContain: [][]byte{chunk(idFact, []byte{1, 2, 3, 4}), pcm},
		},
		{
			Name:       "LIST INFO",
			Data:       riffFile(fmtChunk, chunk("LIST", infoList()), dataChunk),
			WantFields: []string{"Artist", "Comment", "Creation date", "Title", "Software", "Technician", "Other INFO field"},
			MustNotContain: [][]byte{
				[]byte("LIST"), []byte("INFO"), []byte(tagtest.Fake), []byte("Test Comment"),
				[]byte("2001-02-03"), []byte("Test Software"), []byte("Test Other"),
			},
			MustContain: [][]byte{pcm},
		},
		{
			Name:           "LIST after the audio",
			Data:           riffFile(fmtChunk, dataChunk, chunk("LIST", infoList())),
			WantFields:     []string{"Artist"},
			MustNotContain: [][]byte{[]byte("LIST"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:       "Broadcast WAV",
			Data:       riffFile(chunk("bext", bextBody()), fmtChunk, dataChunk),
			WantFields: []string{"Broadcast WAV chunk", "Description", "Originator", "Originator reference", "Origination date", "UMID", "Coding history"},
			MustNotContain: [][]byte{
				[]byte("bext"), []byte("Test Bext Description"), []byte("Test Originator"), []byte("Test Reference"),
				[]byte("2001-02-03"), []byte("04:05:06"), []byte("Test Coding History"),
			},
			MustContain: [][]byte{pcm},
		},
		{
			Name:           "iXML and AXML",
			Data:           riffFile(fmtChunk, chunk("iXML", []byte("<BWFXML><NOTE>Test iXML Note</NOTE></BWFXML>")), chunk("axml", []byte("<axml>Test AXML</axml>")), dataChunk),
			WantFields:     []string{"iXML metadata", "AXML metadata"},
			MustNotContain: [][]byte{[]byte("iXML"), []byte("Test iXML Note"), []byte("Test AXML")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "XMP chunk",
			Data:           riffFile(fmtChunk, chunk("_PMX", coretest.FakeXMP()), dataChunk),
			WantFields:     coretest.FakeXMPFields(),
			MustNotContain: coretest.FakeXMPSecrets(),
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "ID3 chunk in lower case",
			Data:           riffFile(fmtChunk, dataChunk, chunk("id3 ", tagtest.ID3v23())),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "ID3 chunk in upper case",
			Data:           riffFile(fmtChunk, dataChunk, chunk("ID3 ", tagtest.ID3v24())),
			WantFields:     []string{"ID3v2 tag", "Artist"},
			MustNotContain: [][]byte{[]byte("ID3"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "ID3 chunk that holds no tag",
			Data:           riffFile(fmtChunk, dataChunk, chunk("id3 ", []byte("Test Not A Tag"))),
			WantFields:     []string{"Unknown chunk"},
			MustNotContain: [][]byte{[]byte("Test Not A Tag")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name: "cue, sampler and padding",
			Data: riffFile(chunk("JUNK", make([]byte, 28)), fmtChunk,
				chunk("cue ", make([]byte, 28)), chunk("smpl", make([]byte, 36)),
				chunk("PAD ", make([]byte, 8)), chunk("FLLR", make([]byte, 8)), dataChunk),
			WantFields:     []string{"Padding", "Cue points", "Sampler data"},
			MustNotContain: [][]byte{[]byte("JUNK"), []byte("cue "), []byte("smpl"), []byte("FLLR")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "hidden data in padding",
			Data:           riffFile(fmtChunk, chunk("JUNK", []byte("Test Hidden In Junk")), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{[]byte("Test Hidden In Junk")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "cue labels",
			Data:           riffFile(fmtChunk, chunk("LIST", concat([]byte("adtl"), chunk("labl", []byte("\x01\x00\x00\x00Test Label\x00")))), dataChunk),
			WantFields:     []string{"Cue label"},
			MustNotContain: [][]byte{[]byte("Test Label")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "unknown chunk",
			Data:           riffFile(fmtChunk, chunk("zzzz", []byte("Test Unknown Chunk")), dataChunk),
			WantFields:     []string{"Unknown chunk"},
			MustNotContain: [][]byte{[]byte("Test Unknown Chunk"), []byte("zzzz")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "trailing junk",
			Data:           concat(riffFile(fmtChunk, dataChunk), []byte("Test Trailing Junk")),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Junk")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "trailing tags",
			Data:           concat(riffFile(fmtChunk, dataChunk), tagtest.APE(), tagtest.ID3v1()),
			WantFields:     []string{"APE tag", "ID3v1 tag"},
			MustNotContain: [][]byte{[]byte("APETAGEX"), []byte("TAG"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "damaged LIST",
			Data:           riffFile(fmtChunk, chunk("LIST", []byte("INFOIARTxxxxTest Damaged")), dataChunk),
			WantFields:     []string{"Damaged LIST chunk"},
			MustNotContain: [][]byte{[]byte("LIST"), []byte("Test Damaged")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "too short a LIST",
			Data:           riffFile(fmtChunk, chunk("LIST", []byte("IN")), dataChunk),
			WantFields:     []string{"Damaged LIST chunk"},
			MustNotContain: [][]byte{[]byte("LIST")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "unknown LIST type",
			Data:           riffFile(fmtChunk, chunk("LIST", concat([]byte("zzzz"), chunk("xxxx", []byte("Test Nested")))), dataChunk),
			WantFields:     []string{"LIST chunk"},
			MustNotContain: [][]byte{[]byte("LIST"), []byte("Test Nested")},
			MustContain:    [][]byte{pcm},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, concat(riffFile(
		chunk("bext", bextBody()),
		chunk(idFmt, fmtBody()),
		chunk("LIST", infoList()),
		chunk("id3 ", tagtest.ID3v24()),
		chunk(idData, samples(200)),
		chunk("JUNK", make([]byte, 8)),
	), tagtest.ID3v1()))
}

func FuzzClean(f *testing.F) {
	fmtChunk := chunk(idFmt, fmtBody())
	dataChunk := chunk(idData, samples(64))
	f.Add(riffFile(fmtChunk, dataChunk))
	f.Add(riffFile(fmtChunk, chunk(idFact, []byte{1, 2, 3, 4}), chunk("LIST", infoList()), dataChunk))
	f.Add(riffFile(chunk("bext", bextBody()), fmtChunk, dataChunk, chunk("iXML", []byte("<a>Test</a>"))))
	f.Add(concat(riffFile(fmtChunk, dataChunk, chunk("id3 ", tagtest.ID3v23())), tagtest.ID3v1()))
	f.Add(riffFile(fmtChunk, chunk("LIST", concat([]byte("adtl"), chunk("LIST", []byte("INFO")))), dataChunk))
	f.Add(riffFile(fmtChunk, chunk("_PMX", coretest.FakeXMP()), dataChunk, chunk("JUNK", []byte("Test"))))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}

// The output is exactly the kept chunks: nothing added, nothing out of order, and
// the samples untouched.
func TestOutputLayout(t *testing.T) {
	pcm := samples(1000)
	fact := chunk(idFact, []byte{1, 2, 3, 4})
	fmtChunk := chunk(idFmt, fmtBody())
	dataChunk := chunk(idData, pcm)
	dirty := riffFile(chunk("JUNK", make([]byte, 28)), fmtChunk, chunk("LIST", infoList()), fact, dataChunk, chunk("id3 ", tagtest.ID3v24()))
	out, _ := clean(t, dirty)
	if want := riffFile(fmtChunk, fact, dataChunk); !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

// internal/riff leaves a pad byte's value to the handler, and it can hold anything.
func TestPadBytesAreZeroed(t *testing.T) {
	pcm := samples(101)
	dataChunk := binary.LittleEndian.AppendUint32([]byte(idData), uint32(len(pcm)))
	dataChunk = append(dataChunk, pcm...)
	dataChunk = append(dataChunk, 'S') // a pad byte carrying a value
	dirty := riffFile(chunk(idFmt, fmtBody()), dataChunk, chunk("JUNK", make([]byte, 2)))

	out, _ := clean(t, dirty)
	want := riffFile(chunk(idFmt, fmtBody()), chunk(idData, pcm))
	if !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
	if out[len(out)-1] != 0 {
		t.Errorf("the pad byte is %#x", out[len(out)-1])
	}
}

// Many writers omit the pad on a final odd chunk. The output must be correct whether
// the input had it or not, and its RIFF size must count the pad it adds.
func TestMissingFinalPad(t *testing.T) {
	pcm := samples(101)
	data := binary.LittleEndian.AppendUint32([]byte(idData), uint32(len(pcm)))
	data = append(data, pcm...)
	body := concat([]byte(formWAVE), chunk(idFmt, fmtBody()), data)
	dirty := concat(binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body))), body)

	out, _ := clean(t, dirty)
	want := riffFile(chunk(idFmt, fmtBody()), chunk(idData, pcm))
	if !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

func TestReportedValues(t *testing.T) {
	data := riffFile(
		chunk("bext", bextBody()),
		chunk(idFmt, fmtBody()),
		chunk("LIST", infoList()),
		chunk("iXML", []byte("<BWFXML><NOTE>Test iXML Note</NOTE></BWFXML>")),
		chunk(idData, samples(10)),
	)
	rep, err := handler{}.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ field, want string }{
		{"Artist", tagtest.Fake},
		{"Creation date", "2001-02-03"},
		{"Software", "Test Software"},
		{"Other INFO field", "IZZZ: Test Other"},
		{"Originator", "Test Originator"},
		{"Origination date", "2001-02-03 04:05:06"},
		{"Coding history", "Test Coding History"},
		{"UMID", "06" + strings.Repeat("00", 63)},
	} {
		if got := value(t, rep, c.field); got != c.want {
			t.Errorf("field %q = %q, want %q", c.field, got, c.want)
		}
	}
	if got := value(t, rep, "iXML metadata"); !strings.Contains(got, "Test iXML Note") {
		t.Errorf("iXML value = %q", got)
	}
}

func TestShortBextIsReportedAndRemoved(t *testing.T) {
	data := riffFile(chunk(idFmt, fmtBody()), chunk("bext", []byte("Test Short")), chunk(idData, samples(10)))
	out, rep := clean(t, data)
	if bytes.Contains(out, []byte("Test Short")) {
		t.Error("the chunk survived")
	}
	value(t, rep, "Broadcast WAV chunk")
}

// A hostile file can nest LIST chunks arbitrarily deep.
func TestDeepListNesting(t *testing.T) {
	list := concat([]byte("INFO"), chunk("IART", []byte("Test Deep")))
	for range 2000 {
		list = concat([]byte("zzzz"), chunk("LIST", list))
	}
	data := riffFile(chunk(idFmt, fmtBody()), chunk("LIST", list), chunk(idData, samples(10)))
	out, rep := clean(t, data)
	if bytes.Contains(out, []byte("Test Deep")) {
		t.Error("the chunk survived")
	}
	value(t, rep, "Damaged LIST chunk")
}

func TestRefusals(t *testing.T) {
	fmtChunk := chunk(idFmt, fmtBody())
	dataChunk := chunk(idData, samples(10))
	good := riffFile(fmtChunk, dataChunk)

	variant := func(id string) []byte {
		out := bytes.Clone(good)
		copy(out, id)
		return out
	}
	streamingRIFF := bytes.Clone(good)
	binary.LittleEndian.PutUint32(streamingRIFF[4:], streamingSize)

	streamingData := concat([]byte(idData), []byte{0xFF, 0xFF, 0xFF, 0xFF}, samples(10))
	streamingFile := riffFile(fmtChunk, streamingData)

	extensibleMP3 := fmtWithTag(formatExtensible)
	extensibleMP3 = append(extensibleMP3, make([]byte, 8)...)
	extensibleMP3 = binary.LittleEndian.AppendUint16(extensibleMP3, formatMPEGLayer3)
	extensibleMP3 = append(extensibleMP3, make([]byte, 14)...)

	shortRIFF := bytes.Clone(good)
	binary.LittleEndian.PutUint32(shortRIFF[4:], 2)
	overrun := bytes.Clone(good)
	binary.LittleEndian.PutUint32(overrun[4:], uint32(len(good)))

	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"RF64", variant("RF64"), core.ErrUnsupported},
		{"BW64", variant("BW64"), core.ErrUnsupported},
		{"RIFX", variant("RIFX"), core.ErrUnsupported},
		{"streaming RIFF size", streamingRIFF, core.ErrUnsupported},
		{"streaming data size", streamingFile, core.ErrUnsupported},
		{"not WAVE", bytes.Replace(good, []byte(formWAVE), []byte("WEBP"), 1), core.ErrUnsupported},
		{"MPEG Layer III", riffFile(chunk(idFmt, fmtWithTag(formatMPEGLayer3)), dataChunk), core.ErrUnsupported},
		{"MPEG", riffFile(chunk(idFmt, fmtWithTag(formatMPEG)), dataChunk), core.ErrUnsupported},
		{"MPEG in extensible", riffFile(chunk(idFmt, extensibleMP3), dataChunk), core.ErrUnsupported},
		{"not RIFF", []byte("Test not a WAV file, not at all"), core.ErrUnsupported},

		{"no fmt", riffFile(dataChunk), core.ErrMalformed},
		{"no data", riffFile(fmtChunk), core.ErrMalformed},
		{"data before fmt", riffFile(dataChunk, fmtChunk), core.ErrMalformed},
		{"two fmt chunks", riffFile(fmtChunk, fmtChunk, dataChunk), core.ErrMalformed},
		{"two data chunks", riffFile(fmtChunk, dataChunk, dataChunk), core.ErrMalformed},
		{"two fact chunks", riffFile(fmtChunk, chunk(idFact, make([]byte, 4)), chunk(idFact, make([]byte, 4)), dataChunk), core.ErrMalformed},
		{"short fmt", riffFile(chunk(idFmt, make([]byte, 15)), dataChunk), core.ErrMalformed},
		{"short fact", riffFile(fmtChunk, chunk(idFact, make([]byte, 3)), dataChunk), core.ErrMalformed},
		{"chunk past the end of RIFF", riffFile(fmtChunk, concat([]byte(idData), []byte{0xF0, 0xFF, 0, 0}, samples(10))), core.ErrMalformed},
		{"stray bytes in RIFF", riffFile(fmtChunk, dataChunk, []byte("xx")), core.ErrMalformed},
		{"RIFF too small for its form", shortRIFF, core.ErrMalformed},
		{"RIFF past the end of the file", overrun, core.ErrMalformed},
		{"too short", []byte("RIFF"), core.ErrMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(c.data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Clean = %v, want %v", err, c.want)
			}
			if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(c.data), core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Inspect = %v, want %v", err, c.want)
			}
		})
	}
}

// Extensible PCM and the other common codecs must not trip the MPEG check.
func TestOtherFormatsAreAccepted(t *testing.T) {
	extensible := fmtWithTag(formatExtensible)
	extensible = append(extensible, make([]byte, 8)...)
	extensible = binary.LittleEndian.AppendUint16(extensible, 1)
	extensible = append(extensible, make([]byte, 14)...)
	for name, body := range map[string][]byte{
		"extensible PCM": extensible,
		"float":          fmtWithTag(3),
		"mu-law":         fmtWithTag(7),
		"ADPCM":          append(fmtWithTag(0x11), 2, 0, 0xF9, 0x07),
	} {
		t.Run(name, func(t *testing.T) {
			pcm := samples(16)
			out, _ := clean(t, riffFile(chunk(idFmt, body), chunk(idData, pcm)))
			if !bytes.Contains(out, body) || !bytes.Contains(out, pcm) {
				t.Error("the file was damaged")
			}
		})
	}
}

func TestMatch(t *testing.T) {
	good := riffFile(chunk(idFmt, fmtBody()), chunk(idData, samples(10)))
	for _, c := range []struct {
		name string
		data []byte
		want bool
	}{
		{"WAV", good, true},
		{"RF64 for a clear refusal", bytes.Replace(good, []byte("RIFF"), []byte("RF64"), 1), true},
		{"BW64 for a clear refusal", bytes.Replace(good, []byte("RIFF"), []byte("BW64"), 1), true},
		{"RIFX for a clear refusal", bytes.Replace(good, []byte("RIFF"), []byte("RIFX"), 1), true},
		{"WebP", bytes.Replace(good, []byte(formWAVE), []byte("WEBP"), 1), false},
		{"AVI", bytes.Replace(good, []byte(formWAVE), []byte("AVI "), 1), false},
		{"other", []byte("Test not a WAV file, not at all"), false},
		{"short", []byte("RIFF"), false},
		{"empty", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (handler{}).Match(c.data[:min(len(c.data), core.HeadSize)], core.BytesSource(c.data)); got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := riffFile(chunk(idFmt, fmtBody()), chunk("LIST", infoList()), chunk(idData, samples(10)))
	if _, err := (handler{}).Clean(ctx, core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Clean = %v", err)
	}
}

func TestStreamsWithoutLoadingTheFile(t *testing.T) {
	const samplesLen = tagtest.StreamingFileSize
	head := []byte("RIFF\x00\x00\x00\x00WAVE")
	fmtChunk := chunk(idFmt, fmtBody())
	list := chunk("LIST", infoList())
	dataHeader := binary.LittleEndian.AppendUint32([]byte(idData), samplesLen)
	prefix := concat(head, fmtChunk, list, dataHeader)
	binary.LittleEndian.PutUint32(prefix[4:], uint32(len(prefix)-8+samplesLen))

	src := tagtest.Repeating{Prefix: prefix, Period: samples(4096), Total: int64(len(prefix)) + samplesLen}
	var out tagtest.CountingWriter
	allocated := tagtest.Allocated(func() {
		if _, err := (handler{}).Clean(context.Background(), src, &out, core.Options{}); err != nil {
			t.Error(err)
		}
	})
	want := int64(len(head)+len(fmtChunk)+len(dataHeader)) + samplesLen
	if out.N != want {
		t.Errorf("wrote %d bytes, want %d", out.N, want)
	}
	if allocated > tagtest.StreamingBudget {
		t.Errorf("allocated %d bytes to clean a %d-byte file", allocated, src.Total)
	}
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != formatID || f.Family != core.FamilyAudio || len(f.Extensions) == 0 || len(f.Limitations) == 0 {
		t.Errorf("incomplete format: %+v", f)
	}
}

// A writer that never went back to fill in its sizes leaves a data chunk of zero
// bytes with the samples after it. ffmpeg plays those samples, so taking the sizes at
// their word would write out a silent file and report success.
func TestEmptyDataChunkWithBytesAfterItIsRefused(t *testing.T) {
	fmtChunk := chunk(idFmt, fmtBody())
	empty := chunk(idData, nil)
	for name, data := range map[string][]byte{
		"samples past the RIFF size":   concat(riffFile(fmtChunk, empty), samples(1000)),
		"silence read as empty chunks": riffFile(fmtChunk, empty, make([]byte, 1000)),
		"a chunk after it":             riffFile(fmtChunk, empty, chunk("LIST", infoList())),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrUnsupported) {
				t.Errorf("Clean = %v, want %v", err, core.ErrUnsupported)
			}
		})
	}
	if out, _ := clean(t, riffFile(fmtChunk, empty)); !bytes.Equal(out, riffFile(fmtChunk, empty)) {
		t.Error("a file that really is empty was changed")
	}
}

// Bytes past what the format fields define are read by nothing. They are known to be
// spare, so they cannot be left in place as part of a chunk that has to stay.
func TestSpareBytesInKeptChunks(t *testing.T) {
	pcm := samples(1000)
	dataChunk := chunk(idData, pcm)
	secret := []byte("Test Hidden Format")
	le16 := func(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
	extensible := concat(fmtWithTag(formatExtensible), le16(22), le16(16), []byte{4, 0, 0, 0}, le16(1), make([]byte, 14))
	adpcm := concat(fmtWithTag(0x11), le16(2), []byte{0xF9, 0x07})

	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name:           "after the sample count in fact",
			Data:           riffFile(chunk(idFmt, fmtBody()), chunk(idFact, concat([]byte{1, 2, 3, 4}, secret)), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunk(idFact, []byte{1, 2, 3, 4}), pcm},
		},
		{
			Name:           "after the 16 bytes of PCM fmt",
			Data:           riffFile(chunk(idFmt, concat(fmtBody(), secret)), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunk(idFmt, fmtBody()), pcm},
		},
		{
			Name:           "in a PCM extra area that cbSize declares",
			Data:           riffFile(chunk(idFmt, concat(fmtBody(), le16(uint16(len(secret))), secret)), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunk(idFmt, fmtBody()), pcm},
		},
		{
			Name:           "past an empty extra area",
			Data:           riffFile(chunk(idFmt, concat(fmtBody(), le16(0), secret)), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunk(idFmt, concat(fmtBody(), le16(0))), pcm},
		},
		{
			Name:           "after the extensible fields",
			Data:           riffFile(chunk(idFmt, concat(extensible, secret)), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunk(idFmt, extensible), pcm},
		},
		{
			Name:           "past a codec's extra area",
			Data:           riffFile(chunk(idFmt, concat(adpcm, secret)), dataChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunk(idFmt, adpcm), pcm},
		},
		{
			Name:        "a plain 18-byte fmt stays as it is",
			Data:        riffFile(chunk(idFmt, concat(fmtBody(), le16(0))), dataChunk),
			MustContain: [][]byte{chunk(idFmt, concat(fmtBody(), le16(0))), pcm},
		},
	})
}

// The data chunk is copied unread, which is only safe for a codec with nowhere to put
// metadata. FLAC, Vorbis and AAC streams can all carry their own tags in there.
func TestCodecsThatCanHoldMetadataAreRefused(t *testing.T) {
	dataChunk := chunk(idData, samples(16))
	extensible := func(sub uint16) []byte {
		out := binary.LittleEndian.AppendUint16(fmtWithTag(formatExtensible), 22)
		out = append(out, make([]byte, 6)...)
		out = binary.LittleEndian.AppendUint16(out, sub)
		return append(out, make([]byte, 14)...)
	}
	for _, c := range []struct {
		name string
		body []byte
		want error
	}{
		{"FLAC", fmtWithTag(0xF1AC), core.ErrUnsupported},
		{"Vorbis", fmtWithTag(0x566F), core.ErrUnsupported},
		{"AAC", fmtWithTag(0x00FF), core.ErrUnsupported},
		{"an unknown codec", fmtWithTag(0x7777), core.ErrUnsupported},
		{"FLAC in extensible", extensible(0xF1AC), core.ErrUnsupported},
		{"extensible with no room for a sub-format", fmtWithTag(formatExtensible), core.ErrMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := riffFile(chunk(idFmt, c.body), dataChunk)
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Clean = %v, want %v", err, c.want)
			}
		})
	}
}

// clean prints every finding's Where, so chunk names from the file cannot go in it.
func TestChunkNamesStayOutOfWhere(t *testing.T) {
	const name = "ZQMK"
	fmtChunk := chunk(idFmt, fmtBody())
	dataChunk := chunk(idData, samples(16))
	damaged := concat([]byte(name), []byte("abcdxxxxTest"))
	for label, list := range map[string][]byte{
		"a damaged LIST of an unknown type": damaged,
		"a damaged LIST inside another":     concat([]byte("INFO"), chunk("LIST", damaged)),
		"an unknown item in a label list":   concat([]byte("adtl"), chunk(name, []byte("Test\x00"))),
		"an unknown item in an INFO list":   concat([]byte("INFO"), chunk(name, []byte("Test\x00"))),
	} {
		t.Run(label, func(t *testing.T) {
			_, rep := clean(t, riffFile(fmtChunk, chunk("LIST", list), dataChunk))
			if len(rep.Findings) == 0 {
				t.Fatal("nothing was reported")
			}
			for _, f := range rep.Findings {
				if strings.Contains(f.Field, name) || strings.Contains(f.Where, name) {
					t.Errorf("the name is in Field %q or Where %q", f.Field, f.Where)
				}
			}
		})
	}
}
