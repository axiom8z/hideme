package mp3

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

const frameLen128k = 417

// frame builds a 128 kbit/s, 44.1 kHz mono MPEG-1 Layer III frame. The body cycles
// through values that exclude 0xFF, so it holds no accidental sync, and depends on
// the frame's position, so a test can tell frames apart.
func frame(index int) []byte {
	out := make([]byte, frameLen128k)
	copy(out, []byte{0xFF, 0xFB, 0x90, 0xC4})
	for i := headerLen; i < len(out); i++ {
		out[i] = byte((index*7 + i*13) % 251)
	}
	return out
}

func frames(start, n int) []byte {
	var out []byte
	for i := start; i < start+n; i++ {
		out = append(out, frame(i)...)
	}
	return out
}

// xingFrame builds the seek frame an encoder puts first, with its encoder string.
func xingFrame() []byte {
	out := make([]byte, frameLen128k)
	copy(out, []byte{0xFF, 0xFB, 0x90, 0xC4})
	off := headerLen + 17 // mono MPEG-1 side information
	copy(out[off:], "Info\x00\x00\x00\x0F")
	copy(out[off+8+4+4+100+4:], "LAME3.100")
	return out
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

func hasField(rep *core.Report, field string) bool {
	for _, f := range rep.Findings {
		if f.Field == field {
			return true
		}
	}
	return false
}

func TestContract(t *testing.T) {
	audio := frames(0, 8)
	junk := []byte("Test Hidden Bytes")
	allTags := concat(tagtest.ID3v23(), audio, tagtest.APE(), tagtest.Lyrics3v2(), tagtest.Enhanced(), tagtest.ID3v1())
	cases := []coretest.Case{
		{Name: "no metadata", Data: audio, MustContain: [][]byte{audio}},
		{
			Name:           "ID3v2.3 with every frame kind",
			Data:           concat(tagtest.ID3v23(), audio),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "ID3v2.4",
			Data:           concat(tagtest.ID3v24(), audio),
			WantFields:     []string{"ID3v2 tag", "Title", "Artist", "Recording date", "Involved people"},
			MustNotContain: [][]byte{[]byte(tagtest.Fake), []byte("Test Title"), []byte("Test Engineer")},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "ID3v2.2",
			Data:           concat(tagtest.ID3v22(), audio),
			WantFields:     []string{"ID3v2 tag", "Title", "Artist", "Comment", "Embedded picture"},
			MustNotContain: [][]byte{[]byte(tagtest.Fake), []byte("Test Cover")},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "ID3v1",
			Data:           concat(audio, tagtest.ID3v1()),
			WantFields:     []string{"ID3v1 tag", "Title", "Artist", "Album", "Year", "Comment"},
			MustNotContain: [][]byte{[]byte("TAG"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "every kind of tag at once",
			Data:           allTags,
			WantFields:     []string{"ID3v2 tag", "APE tag", "Lyrics3 tag", "Enhanced TAG+ tag", "ID3v1 tag"},
			MustNotContain: concatSecrets(tagtest.ID3Secrets(), []byte(tagtest.Fake), []byte("APETAGEX"), []byte("LYRICS"), []byte("TAG+")),
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "APE footer only",
			Data:           concat(audio, tagtest.APEFooterOnly()),
			WantFields:     []string{"APE tag", "Title", "Artist"},
			MustNotContain: [][]byte{[]byte(tagtest.Fake), []byte("APETAGEX")},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "Lyrics3 v1 with ID3v1",
			Data:           concat(audio, tagtest.Lyrics3v1(), tagtest.ID3v1()),
			WantFields:     []string{"Lyrics3 tag", "Lyrics", "ID3v1 tag"},
			MustNotContain: [][]byte{[]byte("LYRICS"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "appended ID3v2",
			Data:           concat(audio, tagtest.ID3v23()),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "ID3v2 between frames",
			Data:           concat(frames(0, 4), tagtest.ID3v23(), frames(4, 4)),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "junk between frames",
			Data:           concat(frames(0, 4), junk, frames(4, 4)),
			WantFields:     []string{"Non-audio data"},
			MustNotContain: [][]byte{junk},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "junk after the audio",
			Data:           concat(audio, junk),
			WantFields:     []string{"Non-audio data"},
			MustNotContain: [][]byte{junk},
			MustContain:    [][]byte{audio},
		},
		{
			Name:         "encoder string in the seek frame",
			Data:         concat(tagtest.ID3v24(), xingFrame(), audio),
			WantFields:   []string{"ID3v2 tag", "Encoder"},
			WantRetained: []string{"Encoder"},
			MustContain:  [][]byte{xingFrame(), audio},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

func concatSecrets(lists [][]byte, more ...[]byte) [][]byte {
	return append(append([][]byte(nil), lists...), more...)
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, concat(tagtest.ID3v23(), xingFrame(), frames(0, 4), tagtest.APE(), tagtest.ID3v1()))
}

func FuzzClean(f *testing.F) {
	audio := frames(0, 4)
	f.Add(audio)
	f.Add(concat(tagtest.ID3v23(), audio))
	f.Add(concat(tagtest.ID3v24(), audio))
	f.Add(concat(tagtest.ID3v22(), audio))
	f.Add(concat(xingFrame(), audio, tagtest.APE(), tagtest.Lyrics3v2(), tagtest.Enhanced(), tagtest.ID3v1()))
	f.Add(concat(frames(0, 3), []byte("Test Junk"), frames(3, 3), tagtest.APEFooterOnly()))
	f.Add(concat(audio, tagtest.Lyrics3v1(), tagtest.ID3v1()))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}

// The output must be exactly the frames, in order, as the strongest content check
// possible without an MP3 decoder in the standard library.
func TestOutputIsExactlyTheFrames(t *testing.T) {
	audio := concat(xingFrame(), frames(0, 20))
	dirty := concat(tagtest.ID3v23(), audio[:frameLen128k*5], []byte("Test Junk"), audio[frameLen128k*5:], tagtest.APE(), tagtest.ID3v1())
	out, _ := clean(t, dirty)
	if !bytes.Equal(out, audio) {
		t.Errorf("the output differs from the frames: %d bytes, want %d", len(out), len(audio))
	}
}

func TestMatch(t *testing.T) {
	audio := frames(0, 4)
	flac := concat(tagtest.ID3v23(), []byte("fLaC\x00\x00\x00\x22"))
	// ADTS AAC has a sync too, but the layer bits it uses are reserved in MPEG audio.
	adts := bytes.Repeat([]byte{0xFF, 0xF1, 0x50, 0x80, 0x10, 0x1F, 0xFC}, 100)
	free := concat([]byte{0xFF, 0xFB, 0x00, 0xC4}, make([]byte, 100))
	binary := make([]byte, 4096)
	for i := range binary {
		binary[i] = byte(i * 31)
	}
	for _, c := range []struct {
		name string
		data []byte
		want bool
	}{
		{"frames", audio, true},
		{"exactly three frames", frames(0, 3), true},
		{"ID3v2 then frames", concat(tagtest.ID3v23(), audio), true},
		{"two ID3v2 tags then frames", concat(tagtest.ID3v24(), tagtest.ID3v22(), audio), true},
		{"ID3v2, stray zeros, then frames", concat(tagtest.ID3v24(), make([]byte, 512), audio), true},
		{"ID3v2 then a free-format frame", concat(tagtest.ID3v24(), free), true},
		{"two frames", frames(0, 2), false},
		{"one frame with room to spare", concat(frame(0), make([]byte, 2048)), false},
		{"ID3v2 then FLAC", flac, false},
		{"ID3v2 then binary", concat(tagtest.ID3v24(), []byte("not audio at all")), false},
		{"ID3v2 only", tagtest.ID3v24(), false},
		{"ID3v2 then too many zeros", concat(tagtest.ID3v24(), make([]byte, maxLeadingZeros+1), audio), false},
		{"ID3v2 past the end of the file", tagtest.Header(3, 0, 1<<20), false},
		{"ADTS AAC", adts, false},
		{"arbitrary binary", binary, false},
		{"text", []byte("ID3 is the first thing this text says"), false},
		{"empty", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := core.BytesSource(c.data)
			if got := (handler{}).Match(c.data[:min(len(c.data), core.HeadSize)], src); got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

// A very large ID3v2 tag, such as one with high-resolution cover art, pushes the
// audio past the bytes that Match gets in head.
func TestMatchLooksPastHead(t *testing.T) {
	big := tagtest.Tag(3, 0, tagtest.Frame23("APIC", 0, make([]byte, core.HeadSize*2)))
	data := concat(big, frames(0, 4))
	if !(handler{}).Match(data[:core.HeadSize], core.BytesSource(data)) {
		t.Error("the file does not match")
	}
}

func TestFrameLengths(t *testing.T) {
	for _, c := range []struct {
		name   string
		header []byte
		want   int64
	}{
		{"MPEG-1 Layer III", []byte{0xFF, 0xFB, 0x90, 0x00}, 417},
		{"with padding", []byte{0xFF, 0xFB, 0x92, 0x00}, 418},
		{"with CRC", []byte{0xFF, 0xFA, 0x90, 0x00}, 417},
		{"MPEG-1 Layer III 320 kbit/s", []byte{0xFF, 0xFB, 0xE0, 0x00}, 1044},
		{"MPEG-2 Layer III", []byte{0xFF, 0xF3, 0x80, 0x00}, 208},
		{"MPEG-2.5 Layer III", []byte{0xFF, 0xE3, 0x14, 0x00}, 48},
		{"MPEG-1 Layer II", []byte{0xFF, 0xFD, 0x80, 0x00}, 417},
		{"MPEG-1 Layer I", []byte{0xFF, 0xFF, 0x40, 0x00}, 136},
		{"MPEG-1 Layer I with padding", []byte{0xFF, 0xFF, 0x42, 0x00}, 140},
		{"MPEG-2 Layer II", []byte{0xFF, 0xF5, 0x80, 0x00}, 417},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, status := parseHeader(c.header)
			if status != headerOK || h.length != c.want {
				t.Errorf("got length %d and status %d, want %d", h.length, status, c.want)
			}
		})
	}
}

func TestParseHeaderRejects(t *testing.T) {
	for _, c := range []struct {
		name   string
		header []byte
		want   headerStatus
	}{
		{"no sync", []byte{0xFF, 0x7B, 0x90, 0x00}, headerInvalid},
		{"reserved version", []byte{0xFF, 0xEB, 0x90, 0x00}, headerInvalid},
		{"reserved layer", []byte{0xFF, 0xF9, 0x90, 0x00}, headerInvalid},
		{"bad bit rate", []byte{0xFF, 0xFB, 0xF0, 0x00}, headerInvalid},
		{"reserved sample rate", []byte{0xFF, 0xFB, 0x9C, 0x00}, headerInvalid},
		{"too short", []byte{0xFF, 0xFB}, headerInvalid},
		{"free format", []byte{0xFF, 0xFB, 0x00, 0x00}, headerFree},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, got := parseHeader(c.header); got != c.want {
				t.Errorf("status = %d, want %d", got, c.want)
			}
		})
	}
}

func TestEncoderTag(t *testing.T) {
	mono := frameHeader{version: versionMPEG1, layer: 3, mono: true}
	stereo := frameHeader{version: versionMPEG1, layer: 3}
	mpeg2 := frameHeader{version: versionMPEG2, layer: 3}
	mpeg2Mono := frameHeader{version: versionMPEG2, layer: 3, mono: true}

	tagged := func(h frameHeader, magic string, flags byte, encoder string) []byte {
		out := make([]byte, frameLen128k)
		off := headerLen + h.sideInfoLen()
		if h.protected {
			off += 2
		}
		copy(out[off:], magic)
		out[off+7] = flags
		skip := 8
		for _, field := range []struct {
			bit  byte
			size int
		}{{1, 4}, {2, 4}, {4, 100}, {8, 4}} {
			if flags&field.bit != 0 {
				skip += field.size
			}
		}
		copy(out[off+skip:], encoder)
		return out
	}

	for _, c := range []struct {
		name   string
		h      frameHeader
		frame  []byte
		want   string
		wantOK bool
	}{
		{"mono MPEG-1", mono, tagged(mono, "Info", 15, "LAME3.100"), "LAME3.100", true},
		{"stereo MPEG-1", stereo, tagged(stereo, "Xing", 15, "LAME3.100"), "LAME3.100", true},
		{"MPEG-2", mpeg2, tagged(mpeg2, "Info", 15, "Lavc63.1."), "Lavc63.1.", true},
		{"mono MPEG-2", mpeg2Mono, tagged(mpeg2Mono, "Info", 15, "LAME3.100"), "LAME3.100", true},
		{"fewer optional fields", mono, tagged(mono, "Info", 1, "LAME3.100"), "LAME3.100", true},
		{"with CRC", frameHeader{version: versionMPEG1, layer: 3, mono: true, protected: true},
			tagged(frameHeader{version: versionMPEG1, layer: 3, mono: true, protected: true}, "Info", 15, "LAME3.100"), "LAME3.100", true},
		{"trailing spaces", mono, tagged(mono, "Info", 15, "LAME  "), "LAME", true},
		{"no encoder string", mono, tagged(mono, "Info", 15, ""), "", false},
		{"binary where the string would be", mono, tagged(mono, "Info", 15, "\x01\x02\x03"), "", false},
		{"no seek header", mono, make([]byte, frameLen128k), "", false},
		{"frame too short", mono, make([]byte, 10), "", false},
		{"seek header too short", mono, tagged(mono, "Info", 15, "")[:40], "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := encoderTag(c.frame, c.h)
			if got != c.want || ok != c.wantOK {
				t.Errorf("got %q, %v; want %q, %v", got, ok, c.want, c.wantOK)
			}
		})
	}

	vbri := make([]byte, frameLen128k)
	copy(vbri[36:], "VBRI")
	if _, ok := encoderTag(vbri, mono); !ok {
		t.Error("a VBRI header was not noticed")
	}
}

func TestRefusals(t *testing.T) {
	freeFrame := concat([]byte{0xFF, 0xFB, 0x00, 0xC4}, make([]byte, 300))
	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"free format", concat(tagtest.ID3v24(), bytes.Repeat(freeFrame, 5)), core.ErrUnsupported},
		{"free format without tags", bytes.Repeat(freeFrame, 5), core.ErrUnsupported},
		{"ID3v2 past the end of the file", concat(tagtest.Header(3, 0, 1<<20), frames(0, 4)), core.ErrMalformed},
		{"no frames", []byte("Test no audio here at all"), core.ErrMalformed},
		{"empty", nil, core.ErrMalformed},
		{"tags and nothing else", concat(tagtest.ID3v24(), tagtest.ID3v1()), core.ErrMalformed},
		{"two frames are too few", frames(0, 2), core.ErrMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(c.data), &out, core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Clean = %v, want %v", err, c.want)
			}
			if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(c.data), core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Inspect = %v, want %v", err, c.want)
			}
		})
	}
}

// Junk can start with what looks like a header. The chance sync must go with the
// junk, and it must not cost the real frame that comes after it.
func TestFalseSyncInJunk(t *testing.T) {
	falseSync := []byte{0xFF, 0xFB, 0x90, 0xC4, 'T', 'e', 's', 't'}
	audio := frames(0, 8)
	data := concat(frames(0, 4), []byte("x"), falseSync, frames(4, 4))
	out, rep := clean(t, data)
	if !bytes.Equal(out, audio) {
		t.Errorf("got %d bytes, want the %d bytes of audio", len(out), len(audio))
	}
	if !hasField(rep, "Non-audio data") {
		t.Errorf("the junk was not reported: %+v", rep.Findings)
	}
}

// A frame just before junk has no valid successor but is still real audio, because
// the frames before it vouch for it. It must not be dropped along with the junk.
func TestFrameBeforeJunkIsKept(t *testing.T) {
	out, _ := clean(t, concat(frames(0, 4), []byte("x"), frames(4, 4)))
	if !bytes.Equal(out, frames(0, 8)) {
		t.Error("a frame next to the junk was lost")
	}
}

// Streams can change sample rate, for example when files are concatenated.
func TestSampleRateChangeIsKept(t *testing.T) {
	other := make([]byte, 384) // 128 kbit/s at 48 kHz
	copy(other, []byte{0xFF, 0xFB, 0x94, 0xC4})
	for i := headerLen; i < len(other); i++ {
		other[i] = byte(i % 251)
	}
	audio := concat(frames(0, 3), bytes.Repeat(other, 3))
	if h, _ := parseHeader(other); h.length != int64(len(other)) {
		t.Fatalf("the fixture is %d bytes but its header says %d", len(other), h.length)
	}
	out, _ := clean(t, audio)
	if !bytes.Equal(out, audio) {
		t.Errorf("got %d bytes, want %d", len(out), len(audio))
	}
}

// A lone compatible frame at the very end is audio, even after junk.
func TestLastFrameAfterJunkIsKept(t *testing.T) {
	out, _ := clean(t, concat(frames(0, 4), []byte("x"), frame(4)))
	if !bytes.Equal(out, frames(0, 5)) {
		t.Error("the last frame was lost")
	}
}

func TestTruncatedLastFrameIsDropped(t *testing.T) {
	audio := frames(0, 4)
	out, rep := clean(t, concat(audio, frame(4)[:100]))
	if !bytes.Equal(out, audio) {
		t.Errorf("got %d bytes, want %d", len(out), len(audio))
	}
	if !hasField(rep, "Non-audio data") {
		t.Errorf("the partial frame was not reported: %+v", rep.Findings)
	}
}

// A truncated frame followed by a tag must lose to the tag, not swallow it.
func TestTruncatedFrameBeforeTagDoesNotKeepTag(t *testing.T) {
	audio := frames(0, 4)
	out, rep := clean(t, concat(audio, frame(4)[:50], tagtest.ID3v1()))
	if !bytes.Equal(out, audio) {
		t.Errorf("got %d bytes, want %d", len(out), len(audio))
	}
	if !hasField(rep, "ID3v1 tag") {
		t.Errorf("the tag was not reported: %+v", rep.Findings)
	}
}

// When a frame runs to the exact end of the file across bytes that read as an ID3v1
// tag, there is no telling which it is: keeping it might leak a tag, and dropping
// it might damage audio. The file must be refused, whichever way the odds lie.
func TestAmbiguousTrailingTagIsRefused(t *testing.T) {
	last := frame(4)
	copy(last[len(last)-128:], "TAG")
	data := concat(frames(0, 4), last)
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Clean = %v, want ErrMalformed", err)
	}
}

// A real tag after a frame that reads "TAG" looks like two stacked ID3v1 tags and a
// truncated frame. Taking that reading would silently drop the frame as junk.
func TestStackedTagLookalikeDoesNotDropAudio(t *testing.T) {
	last := frame(3)
	copy(last[len(last)-128:], "TAG")
	data := concat(frames(0, 3), last, tagtest.ID3v1())
	_, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{})
	if !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Clean = %v, want ErrMalformed", err)
	}
}

// Stacked ID3v1 tags themselves are real: some taggers append without looking.
func TestStackedTagsAreRemoved(t *testing.T) {
	audio := frames(0, 4)
	out, _ := clean(t, concat(audio, tagtest.ID3v1(), tagtest.ID3v1()))
	if !bytes.Equal(out, audio) {
		t.Errorf("got %d bytes, want %d", len(out), len(audio))
	}
}

// Removing junk can leave frames whose own bytes read as a tag at the new end of
// the file. The next pass would misread them, so the first must refuse.
func TestOutputThatWouldReadAsATagIsRefused(t *testing.T) {
	last := frame(3)
	copy(last[len(last)-128:], "TAG")
	data := concat(frames(0, 3), last, []byte("Test Junk"))
	_, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{})
	if !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Clean = %v, want ErrMalformed", err)
	}
}

func TestCancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := concat(tagtest.ID3v23(), frames(0, 8))
	if _, err := (handler{}).Clean(ctx, core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Clean = %v", err)
	}
	if _, err := (handler{}).Inspect(ctx, core.BytesSource(data), core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Inspect = %v", err)
	}
}

// failAfter fails reads once they reach a given offset, as a disk failing mid-file
// would. The walk must fail, not treat the unread rest as the end of the audio.
type failAfter struct {
	core.Source
	limit int64
}

var errDisk = errors.New("disk failure")

func (s failAfter) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > s.limit {
		return 0, errDisk
	}
	return s.Source.ReadAt(p, off)
}

func TestReadFailureIsNotTheEndOfTheFile(t *testing.T) {
	data := frames(0, 2000)
	src := failAfter{core.BytesSource(data), int64(len(data)) / 2}
	var out bytes.Buffer
	if _, err := (handler{}).Clean(context.Background(), src, &out, core.Options{}); !errors.Is(err, errDisk) {
		t.Errorf("Clean = %v, with %d bytes written", err, out.Len())
	}
}

// cancelAfter is a context that becomes cancelled after a number of checks, to prove
// that a long scan notices by itself rather than only between steps of the walk.
type cancelAfter struct {
	context.Context
	checks *int
}

func (c cancelAfter) Err() error {
	if *c.checks == 0 {
		return context.Canceled
	}
	*c.checks--
	return nil
}

func TestCancelDuringJunkScan(t *testing.T) {
	// Well over one read buffer of junk with nothing in it that could start a frame.
	data := concat(frames(0, 4), bytes.Repeat([]byte{'x'}, 1<<20), frames(4, 4))
	checks := 1 // the walk's own check at the start, and then the scan's first
	ctx := cancelAfter{context.Background(), &checks}
	if _, err := (handler{}).Inspect(ctx, core.BytesSource(data), core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Inspect = %v, want context.Canceled", err)
	}
}

func TestStreamsWithoutLoadingTheFile(t *testing.T) {
	prefix := tagtest.ID3v24()
	period := frame(0)
	total := tagtest.WholePeriods(len(prefix), len(period), tagtest.StreamingFileSize)
	src := tagtest.Repeating{Prefix: prefix, Period: period, Total: total}
	var out tagtest.CountingWriter
	allocated := tagtest.Allocated(func() {
		if _, err := (handler{}).Clean(context.Background(), src, &out, core.Options{}); err != nil {
			t.Error(err)
		}
	})
	if want := total - int64(len(prefix)); out.N != want {
		t.Errorf("wrote %d bytes, want %d", out.N, want)
	}
	if allocated > tagtest.StreamingBudget {
		t.Errorf("allocated %d bytes to clean a %d-byte file", allocated, total)
	}
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != formatID || f.Family != core.FamilyAudio || len(f.Extensions) == 0 || len(f.Limitations) == 0 {
		t.Errorf("incomplete format: %+v", f)
	}
}

// The emphasis bits say nothing about a frame's length. ffmpeg, mpg123 and mutagen
// all play frames that use the reserved value, so dropping them would lose audio.
func TestReservedEmphasisIsAudio(t *testing.T) {
	some := frames(0, 8)
	some[2*frameLen128k+3] |= 2
	some[3*frameLen128k+3] |= 2
	if out, _ := clean(t, some); !bytes.Equal(out, some) {
		t.Errorf("frames were dropped: %d bytes, want %d", len(out), len(some))
	}

	all := frames(0, 8)
	for i := range 8 {
		all[i*frameLen128k+3] |= 2
	}
	if !(handler{}).Match(all, core.BytesSource(all)) {
		t.Error("a file whose every frame uses the reserved emphasis does not match")
	}
	if out, _ := clean(t, all); !bytes.Equal(out, all) {
		t.Errorf("frames were dropped: %d bytes, want %d", len(out), len(all))
	}
}

// fakeOwner stands in for a handler that registers after this one, as those for WAV
// and Office documents do.
type fakeOwner struct{ core.Handler }

func (fakeOwner) Format() core.Format { return core.Format{ID: "fake-owner"} }
func (fakeOwner) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte("FAKEOWNER"))
}

func init() { core.Register(fakeOwner{}) }

// Audio does not always start where it should. Each of these reads as MP3 to
// independent tools, and everything ahead of the first frame must go.
func TestLeadingDataBeforeTheFirstFrame(t *testing.T) {
	audio := frames(1, 12)
	secret := []byte("Test Leading Data")
	cut := concat(frame(0)[100:], audio) // a stream cut mid-frame
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"zeros", concat(make([]byte, 1000), audio)},
		{"zeros then ID3v2", concat(make([]byte, 64), tagtest.ID3v24(), audio)},
		{"APE tag", concat(tagtest.APE(), audio)},
		{"ID3v2 then APE tag", concat(tagtest.ID3v24(), tagtest.APE(), audio)},
		{"ID3v2 then junk", concat(tagtest.ID3v24(), secret, audio)},
		{"ID3v2 understating its size", concat(tagtest.Header(3, 0, 1), secret, audio)},
		{"a header from some other tool", concat(secret, []byte(" HTTP/1.1 200 OK\r\n\r\n"), audio)},
		{"a stream cut mid-frame", cut},
		{"a stream cut at both ends", concat(cut, frame(13)[:200])},
	} {
		t.Run(c.name, func(t *testing.T) {
			coretest.Contract(t, handler{}, []coretest.Case{{
				Name:           "clean",
				Data:           c.data,
				MustNotContain: [][]byte{secret, []byte(tagtest.Fake)},
				MustContain:    [][]byte{audio},
			}})
			if out, _ := clean(t, c.data); !bytes.Equal(out, audio) {
				t.Errorf("the output is %d bytes, want exactly the %d bytes of frames", len(out), len(audio))
			}
		})
	}
}

// Matching on a run of frames after unknown data must not make this handler the
// owner of a file that merely stores an MP3. With an unknown prefix, anything else
// unaccounted for means the frames are one part of something larger.
func TestStoredMP3IsNotTakenForAnMP3(t *testing.T) {
	audio := frames(0, 12)
	local := []byte("PK\x03\x04 a stored file's local header")
	central := []byte("PK\x01\x02 the central directory of an archive")
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"one stored file", concat(local, audio, central)},
		{"two stored files", concat(local, audio, local, audio, central)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(c.data), &out, core.Options{}); !errors.Is(err, core.ErrUnsupported) {
				t.Errorf("Clean = %v, want %v", err, core.ErrUnsupported)
			}
		})
	}

	// A wrapper a registered handler claims is that handler's file, not this one's.
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"owned by another handler", concat([]byte("FAKEOWNER"), audio)},
		{"too few frames after unknown data", concat(local, frames(0, 4))},
		{"too few frames after zeros", concat(make([]byte, 16), frames(0, 4))},
		{"a prefix too long to be a partial frame", concat(bytes.Repeat([]byte{'x'}, maxUnknownPrefix+1), audio)},
		{"ID3v2 then too much junk", concat(tagtest.ID3v24(), bytes.Repeat([]byte{'x'}, maxLeadingZeros+1), audio)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if (handler{}).Match(c.data[:min(len(c.data), core.HeadSize)], core.BytesSource(c.data)) {
				t.Error("Match = true")
			}
		})
	}
}

// xingBytes returns a seek frame that states the given audio size.
func xingBytes(n int) []byte {
	out := xingFrame()
	binary.BigEndian.PutUint32(out[headerLen+17+12:], uint32(n))
	return out
}

func vbriBytes(n int) []byte {
	out := make([]byte, frameLen128k)
	copy(out, []byte{0xFF, 0xFB, 0x90, 0xC4})
	copy(out[36:], "VBRI")
	binary.BigEndian.PutUint32(out[46:], uint32(n))
	return out
}

// A seek header goes stale only when it counted bytes that are no longer there. Junk
// added after encoding was never counted, so removing it makes the header right.
func TestStaleSeekHeaderWarns(t *testing.T) {
	audio := frames(0, 100)
	total := len(audio) + frameLen128k
	withJunk := func(seek []byte) []byte {
		return concat(seek, audio[:frameLen128k*50], []byte("Test Junk"), audio[frameLen128k*50:])
	}
	noBytes := xingFrame()
	noBytes[headerLen+17+7] = 0x0D
	for _, c := range []struct {
		name string
		data []byte
		warn bool
	}{
		{"accurate", concat(xingBytes(total), audio), false},
		{"junk the header did not count", withJunk(xingBytes(total)), false},
		{"counts everything but its own frame", concat(xingBytes(total-frameLen128k), audio), false},
		{"off by less than the table can resolve", concat(xingBytes(total+100), audio), false},
		{"no size stated", concat(noBytes, audio), false},
		{"counted the junk", withJunk(xingBytes(total + 30000)), true},
		{"counted far too little", concat(xingBytes(total/2), audio), true},
		{"VBRI accurate", concat(vbriBytes(total), audio), false},
		{"VBRI counted the junk", withJunk(vbriBytes(total + 30000)), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, rep := clean(t, c.data)
			inspected, err := handler{}.Inspect(context.Background(), core.BytesSource(c.data), core.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(rep.Warnings) > 0; got != c.warn {
				t.Errorf("warnings = %q, want a warning: %v", rep.Warnings, c.warn)
			}
			if got := len(inspected.Warnings) > 0; got != c.warn {
				t.Errorf("Inspect warnings = %q, want a warning: %v", inspected.Warnings, c.warn)
			}
		})
	}
}

// A seek frame holds no audio, so bytes past the fields of its header are known to
// be unused, and they cannot be dismissed as data inside the audio stream.
func TestHiddenBytesAfterSeekHeaderAreZeroed(t *testing.T) {
	audio := frames(0, 8)
	secret := []byte("Test Hidden Seek Frame")
	hide := func(frame []byte) []byte {
		out := bytes.Clone(frame)
		copy(out[300:], secret)
		return out
	}
	noEncoder := xingFrame()
	clear(noEncoder[headerLen+17+120:])
	vbri := vbriBytes(0)
	binary.BigEndian.PutUint16(vbri[54:], 2) // two table entries
	binary.BigEndian.PutUint16(vbri[58:], 2) // of two bytes each
	binary.BigEndian.PutUint16(vbri[62:], 0x1234)
	for _, c := range []struct {
		name  string
		frame []byte
	}{
		{"Xing with an encoder", xingFrame()},
		{"Xing without an encoder", noEncoder},
		{"VBRI", vbri},
	} {
		t.Run(c.name, func(t *testing.T) {
			coretest.Contract(t, handler{}, []coretest.Case{{
				Name:           "clean",
				Data:           concat(hide(c.frame), audio),
				WantFields:     []string{"Data hidden in padding"},
				MustNotContain: [][]byte{secret},
				MustContain:    [][]byte{c.frame, audio},
			}})
		})
	}

	t.Run("an ordinary first frame is left alone", func(t *testing.T) {
		if out, rep := clean(t, audio); !bytes.Equal(out, audio) || hasField(rep, "Data hidden in padding") {
			t.Error("an audio frame was treated as a seek frame")
		}
	})
}

// Joined files keep the seek frame of each part. That is one more encoder string in
// the file and one more frame with room to hide things in, wherever it comes.
func TestSeekFrameInTheMiddleOfTheAudio(t *testing.T) {
	audio := frames(0, 8)
	second := xingFrame()
	copy(second[headerLen+17+120:], "Test Tool")
	hidden := bytes.Clone(second)
	copy(hidden[300:], "Test Hidden Seek Frame")
	coretest.Contract(t, handler{}, []coretest.Case{{
		Name:           "joined files",
		Data:           concat(xingFrame(), audio, hidden, audio),
		WantFields:     []string{"Encoder", "Data hidden in padding"},
		WantRetained:   []string{"Encoder"},
		MustNotContain: [][]byte{[]byte("Test Hidden Seek Frame")},
		MustContain:    [][]byte{concat(xingFrame(), audio, second, audio)},
	}})
	_, rep := clean(t, concat(xingFrame(), audio, second, audio))
	found := false
	for _, f := range rep.Findings {
		found = found || f.Field == "Encoder" && f.Value == "Test Tool"
	}
	if !found {
		t.Errorf("the second encoder string went unreported: %+v", rep.Findings)
	}
}

// A seek frame has empty side information. An audio frame can read "Info" in its main
// data by chance, about once in 2^31 frames, and must not lose bytes for it.
func TestAudioFrameThatReadsLikeASeekFrame(t *testing.T) {
	lookalike := frame(3)
	copy(lookalike[headerLen+17:], "Info\x00\x00\x00\x0F")
	for name, data := range map[string][]byte{
		"first":      concat(lookalike, frames(0, 8)),
		"in between": concat(frames(0, 4), lookalike, frames(4, 4)),
	} {
		t.Run(name, func(t *testing.T) {
			if out, rep := clean(t, data); !bytes.Equal(out, data) || hasField(rep, "Data hidden in padding") {
				t.Errorf("an audio frame was changed: %+v", rep.Findings)
			}
		})
	}
}

// Junk right after a frame can start with what reads as a compatible header, whose
// length then runs into the real frame behind it. Taking it for a frame keeps the junk
// and loses that real frame, so frames found inside it have to outrank it.
func TestSyncAtTheStartOfJunkDoesNotCostAFrame(t *testing.T) {
	audio := frames(0, 8)
	secret := []byte("Test Junk After A Sync")
	long := concat([]byte{0xFF, 0xFB, 0x90, 0xC4}, secret) // the same length as a real frame
	short := concat([]byte{0xFF, 0xFB, 0x50, 0xC4}, secret)
	cut := frameLen128k * 4
	for name, data := range map[string][]byte{
		"as long as a real frame":  concat(audio[:cut], long, audio[cut:]),
		"shorter than a real one":  concat(audio[:cut], short, audio[cut:]),
		"with few frames after it": concat(audio[:frameLen128k*7], short, audio[frameLen128k*7:]),
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := clean(t, data)
			if !bytes.Equal(out, audio) {
				t.Errorf("the output is %d bytes, want exactly the %d bytes of frames", len(out), len(audio))
			}
		})
	}

	// A frame before junk that holds no frames itself is still a frame.
	withJunk := concat(audio[:cut], secret, audio[cut:])
	if out, _ := clean(t, withJunk); !bytes.Equal(out, audio) {
		t.Errorf("the output is %d bytes, want %d", len(out), len(audio))
	}
}

// Readers look for a seek header at one offset and skip the frame that has one, whatever
// comes before it, so an "audio" frame that reads like one is 32 bytes no reader decodes,
// and more after the header. It may really be audio, so it stays, with a warning.
func TestFirstFrameThatReadsLikeASeekFrameWarns(t *testing.T) {
	lookalike := xingFrame()
	copy(lookalike[headerLen:], "Tst!")
	data := concat(lookalike, frames(0, 8))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Fatal("what may be audio was changed")
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "seek header") {
		t.Fatalf("warnings = %q", rep.Warnings)
	}
	if _, rep := clean(t, concat(xingFrame(), frames(0, 8))); len(rep.Warnings) != 0 {
		t.Fatalf("a false alarm: %q", rep.Warnings)
	}
}
