package audiotag

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/audiotag/tagtest"
	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// describe runs a tag at the start of data through the decoder.
func describe(t *testing.T, kind Kind, data []byte) *core.Report {
	t.Helper()
	sink := NewSink("test")
	win := NewWindow(core.BytesSource(data))
	if err := Describe(context.Background(), win, Region{kind, 0, int64(len(data))}, "", sink); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	return sink.Report()
}

// values collects the reported values of a field.
func values(rep *core.Report, field string) []string {
	var out []string
	for _, f := range rep.Findings {
		if f.Field == field {
			out = append(out, f.Value)
		}
	}
	return out
}

func wantValue(t *testing.T, rep *core.Report, field, want string) {
	t.Helper()
	got := values(rep, field)
	if !slices.Contains(got, want) {
		t.Errorf("field %q has values %q, want %q; report: %+v", field, got, want, rep.Findings)
	}
}

func TestID3v23Frames(t *testing.T) {
	rep := describe(t, KindID3v2, tagtest.ID3v23())
	for _, c := range []struct{ field, want string }{
		{"Title", "Test Title"},
		{"Artist", tagtest.Fake},
		{"Album", "Test Album"},
		{"Album artist", "Test Album Artist"},
		{"Comment", "Test Desc: Test Comment"},
		{"Lyrics", "Test Lyrics"},
		{"User-defined text", "Test Key: Test Value"},
		{"URL", "Test Link: http://example.com/test-wxxx"},
		{"URL", "http://example.com/test-woar"},
		{"Original file name", "C:/Users/Test/secret.wav"},
		{"Other ID3 frame", "TZZZ: Test Unknown Text"},
		{"Other ID3 frame", "MCDI: 17 bytes"},
		{"Private data", "Test Owner: 29 bytes"},
		{"Unique file ID", "http://example.com/test-ufid: 35 bytes"},
		{"Attached file", "test-file.txt: text/plain: Test File Desc: 56 bytes"},
		{"Embedded picture", "image/jpeg, front cover, Test Cover Desc, 55 bytes"},
	} {
		wantValue(t, rep, c.field, c.want)
	}
	for _, field := range tagtest.ID3Fields() {
		if values(rep, field) == nil {
			t.Errorf("field %q is not reported", field)
		}
	}
}

func TestID3v22Frames(t *testing.T) {
	rep := describe(t, KindID3v2, tagtest.ID3v22())
	wantValue(t, rep, "Title", "Test Title")
	wantValue(t, rep, "Artist", tagtest.Fake)
	wantValue(t, rep, "Comment", "Test Desc: Test Comment")
	wantValue(t, rep, "Embedded picture", "JPG, front cover, Test Cover Desc, 47 bytes")
}

func TestID3v24ListValuesAndFooter(t *testing.T) {
	rep := describe(t, KindID3v2, tagtest.ID3v24())
	wantValue(t, rep, "Involved people", "engineer, Test Engineer")

	body := tagtest.Frame24("TIT2", 0, tagtest.UTF8Text("Test Title"))
	tag := append(tagtest.Header(4, id3FlagFooter, len(body)), body...)
	tag = append(tag, []byte("3DI\x04\x00\x10")...)
	tag = append(tag, tagtest.Syncsafe(len(body))...)
	n, ok := ID3v2Length(tag)
	if !ok || n != int64(len(tag)) {
		t.Fatalf("ID3v2Length = %d, %v; want %d", n, ok, len(tag))
	}
	wantValue(t, describe(t, KindID3v2, tag), "Title", "Test Title")
}

func TestID3v2Length(t *testing.T) {
	for _, c := range []struct {
		name string
		b    []byte
		want int64
		ok   bool
	}{
		{"plain", []byte("ID3\x03\x00\x00\x00\x00\x02\x01"), 10 + 257, true},
		{"future version", []byte("ID3\x05\x00\x00\x00\x00\x00\x20"), 10 + 32, true},
		{"size not syncsafe", []byte("ID3\x03\x00\x00\x00\x00\x80\x00"), 0, false},
		{"version FF", []byte("ID3\xFF\x00\x00\x00\x00\x00\x00"), 0, false},
		{"revision FF", []byte("ID3\x03\xFF\x00\x00\x00\x00\x00"), 0, false},
		{"not a tag", []byte("XY3\x03\x00\x00\x00\x00\x00\x00"), 0, false},
		{"too short", []byte("ID3\x03"), 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ID3v2Length(c.b)
			if got != c.want || ok != c.ok {
				t.Errorf("got %d, %v; want %d, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

// compress returns data as zlib would store it in an ID3 frame.
func compress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestID3Encodings(t *testing.T) {
	text := tagtest.Latin1Text("Test Title")
	stuffed := bytes.ReplaceAll(tagtest.UTF16Text("Test Title"), []byte{0xFF}, []byte{0xFF, 0x00})
	z := compress(t, text)

	t.Run("v2.3 tag-wide unsynchronisation", func(t *testing.T) {
		frames := tagtest.Frame23("TIT2", 0, tagtest.UTF16Text("Test Title"))
		frames = append(frames, tagtest.Frame23("TPE1", 0, tagtest.Latin1Text(tagtest.Fake))...)
		tag := tagtest.Tag(3, id3FlagUnsync, bytes.ReplaceAll(frames, []byte{0xFF}, []byte{0xFF, 0x00}))
		rep := describe(t, KindID3v2, tag)
		wantValue(t, rep, "Title", "Test Title")
		wantValue(t, rep, "Artist", tagtest.Fake) // proves the frame boundary survived the decoding
	})
	t.Run("v2.4 frame unsynchronisation", func(t *testing.T) {
		rep := describe(t, KindID3v2, tagtest.Tag(4, 0, tagtest.Frame24("TIT2", 0x0002, stuffed)))
		wantValue(t, rep, "Title", "Test Title")
	})
	t.Run("v2.3 compressed", func(t *testing.T) {
		body := append([]byte{0, 0, 0, byte(len(text))}, z...)
		rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("TIT2", 0x0080, body)))
		wantValue(t, rep, "Title", "Test Title")
	})
	t.Run("v2.4 compressed with length indicator", func(t *testing.T) {
		body := append(tagtest.Syncsafe(len(text)), z...)
		rep := describe(t, KindID3v2, tagtest.Tag(4, 0, tagtest.Frame24("TIT2", 0x0009, body)))
		wantValue(t, rep, "Title", "Test Title")
	})
	t.Run("grouped", func(t *testing.T) {
		rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("TIT2", 0x0020, append([]byte{7}, text...))))
		wantValue(t, rep, "Title", "Test Title")
		rep = describe(t, KindID3v2, tagtest.Tag(4, 0, tagtest.Frame24("TIT2", 0x0040, append([]byte{7}, text...))))
		wantValue(t, rep, "Title", "Test Title")
	})
	t.Run("encrypted frames are reported, not decoded", func(t *testing.T) {
		rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("TIT2", 0x0040, []byte{1, 2, 3})))
		wantValue(t, rep, "Encrypted ID3 frame", "3 bytes")
		rep = describe(t, KindID3v2, tagtest.Tag(4, 0, tagtest.Frame24("TIT2", 0x0004, []byte{1, 2, 3})))
		wantValue(t, rep, "Encrypted ID3 frame", "3 bytes")
	})
	t.Run("damaged compression", func(t *testing.T) {
		rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("TIT2", 0x0080, []byte("\x00\x00\x00\x09not zlib"))))
		wantValue(t, rep, "Damaged ID3 frames", "frame could not be decoded")
	})
	t.Run("too short for its flags", func(t *testing.T) {
		rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("TIT2", 0x0080, []byte{1})))
		wantValue(t, rep, "Damaged ID3 frames", "frame could not be decoded")
	})
}

// A hostile compressed frame must cost the cap, not what it expands to.
func TestID3CompressionBombIsCapped(t *testing.T) {
	bomb := compress(t, bytes.Repeat([]byte("A"), 64<<20))
	body := append([]byte{4, 0, 0, 0}, bomb...)
	rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("TZZZ", 0x0080, body)))
	if len(values(rep, "Other ID3 frame")) != 1 {
		t.Errorf("the bomb was not reported as one frame: %+v", rep.Findings)
	}
}

// The per-frame cap is not enough on its own: a hostile file can hold any number of
// tiny frames that each expand a thousandfold, so the total has a budget as well.
func TestID3TotalDecompressionIsCapped(t *testing.T) {
	bomb := compress(t, bytes.Repeat([]byte("A"), maxFrameRead))
	body := append([]byte{4, 0, 0, 0}, bomb...)
	frames := make([][]byte, maxInflated/maxFrameRead+50)
	for i := range frames {
		frames[i] = tagtest.Frame23("TZZZ", 0x0080, body)
	}
	rep := describe(t, KindID3v2, tagtest.Tag(3, 0, frames...))
	if got := len(values(rep, "Other ID3 frame")); got != maxInflated/maxFrameRead {
		t.Errorf("%d frames were decoded, want the budget's %d", got, maxInflated/maxFrameRead)
	}
	if got := len(values(rep, "Damaged ID3 frames")); got != 50 {
		t.Errorf("%d frames were reported undecoded, want 50", got)
	}
}

// Tags are not worth decoding once the report is full. A tag that is itself over the
// cap has to stop, and later tags must not start, though all are still counted.
func TestDecodingStopsWhenTheReportIsFull(t *testing.T) {
	frames := make([][]byte, maxFindings+10)
	for i := range frames {
		frames[i] = tagtest.Frame23("TIT2", 0, tagtest.Latin1Text("Test Title"))
	}
	tag := tagtest.Tag(3, 0, frames...)
	sink := NewSink("test")
	win := NewWindow(core.BytesSource(tag))
	region := Region{KindID3v2, 0, int64(len(tag))}
	ctx := context.Background()
	for range 3 {
		if err := Describe(ctx, win, region, "", sink); err != nil {
			t.Fatal(err)
		}
	}
	rep := sink.Report()
	if len(rep.Findings) != maxFindings+1 {
		t.Errorf("got %d findings", len(rep.Findings))
	}
	wantValue(t, rep, "Further metadata items", "3 more")
}

func TestID3ExtendedHeaders(t *testing.T) {
	frame23 := tagtest.Frame23("TIT2", 0, tagtest.Latin1Text("Test Title"))
	v23 := append([]byte{0, 0, 0, 6, 0, 0, 0, 0, 0, 0}, frame23...)
	wantValue(t, describe(t, KindID3v2, tagtest.Tag(3, id3FlagExtended, v23)), "Title", "Test Title")

	frame24 := tagtest.Frame24("TIT2", 0, tagtest.Latin1Text("Test Title"))
	v24 := append([]byte{0, 0, 0, 6, 1, 0}, frame24...)
	wantValue(t, describe(t, KindID3v2, tagtest.Tag(4, id3FlagExtended, v24)), "Title", "Test Title")

	for _, c := range []struct {
		name  string
		major byte
		body  []byte
	}{
		{"v2.3 past the end", 3, []byte{0x7F, 0xFF, 0xFF, 0xFF}},
		{"v2.4 not syncsafe", 4, []byte{0xFF, 0, 0, 0}},
		{"v2.4 too small", 4, []byte{0, 0, 0, 1}},
		{"truncated", 3, []byte{0, 0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := describe(t, KindID3v2, tagtest.Tag(c.major, id3FlagExtended, c.body))
			wantValue(t, rep, "Damaged ID3 frames", "extended header")
		})
	}
}

// iTunes wrote plain integers for v2.4 frame sizes. A large frame then needs the
// plain reading or every frame after it is lost.
func TestID3v24PlainFrameSizes(t *testing.T) {
	long := strings.Repeat("x", 200)
	plain := func(id string, body []byte) []byte {
		out := append([]byte(id), 0, 0, byte(len(body)>>8), byte(len(body)))
		out = append(out, 0, 0)
		return append(out, body...)
	}
	tag := tagtest.Tag(4, 0,
		plain("TIT2", tagtest.Latin1Text(long)),
		plain("TPE1", tagtest.Latin1Text(tagtest.Fake)),
	)
	wantValue(t, describe(t, KindID3v2, tag), "Artist", tagtest.Fake)
}

func TestID3Damage(t *testing.T) {
	for _, c := range []struct {
		name  string
		tag   []byte
		field string
	}{
		{"invalid frame ID", tagtest.Tag(3, 0, []byte("t!t2\x00\x00\x00\x01\x00\x00x")), "Damaged ID3 frames"},
		{"frame past the end", tagtest.Tag(3, 0, []byte("TIT2\x00\x00\xFF\xFF\x00\x00\x00Test")), "Damaged ID3 frames"},
		{"v2.2 compressed tag", tagtest.Tag(2, id3v22FlagCompressed, []byte("whatever")), "Damaged ID3 frames"},
		{"unknown version", tagtest.Tag(9, 0, []byte("whatever")), "ID3v2 tag"},
		{"empty", tagtest.Tag(3, 0), "ID3v2 tag"},
		{"only padding", tagtest.Tag(3, 0, make([]byte, 64)), "ID3v2 tag"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := describe(t, KindID3v2, c.tag)
			if values(rep, c.field) == nil {
				t.Errorf("field %q not reported: %+v", c.field, rep.Findings)
			}
		})
	}
}

// XMP stored in a PRIV frame must be reported in detail, since it often holds more
// than the rest of the tag.
func TestID3PrivXMP(t *testing.T) {
	body := append([]byte("XMP\x00"), coretest.FakeXMP()...)
	rep := describe(t, KindID3v2, tagtest.Tag(3, 0, tagtest.Frame23("PRIV", 0, body)))
	wantValue(t, rep, "GPS coordinates", coretest.EiffelTower)
}

func TestDecodeUTF16(t *testing.T) {
	for _, c := range []struct {
		name string
		enc  byte
		b    []byte
	}{
		{"LE with mark", 1, tagtest.UTF16LEWithBOM("Test")},
		{"LE without mark", 1, tagtest.UTF16LE("Test")},
		{"BE with mark", 1, []byte{0xFE, 0xFF, 0, 'T', 0, 'e', 0, 's', 0, 't'}},
		{"encoding 2", 2, []byte{0, 'T', 0, 'e', 0, 's', 0, 't'}},
		{"encoding 2 with a wrong mark", 2, tagtest.UTF16LEWithBOM("Test")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeText(c.enc, c.b); got != "Test" {
				t.Errorf("got %q", got)
			}
		})
	}
}

func TestSplitText(t *testing.T) {
	// The first code unit has a zero high byte: a byte-wise NUL search would cut here.
	b := append(tagtest.UTF16LE("Te"), 0, 0, 'x', 0)
	text, rest := splitText(1, b)
	if !bytes.Equal(text, tagtest.UTF16LE("Te")) || !bytes.Equal(rest, []byte{'x', 0}) {
		t.Errorf("got %q and %q", text, rest)
	}
	text, rest = splitText(0, []byte("no terminator"))
	if string(text) != "no terminator" || rest != nil {
		t.Errorf("got %q and %q", text, rest)
	}
}

func TestAPE(t *testing.T) {
	for _, c := range []struct {
		name string
		tag  []byte
	}{
		{"header and footer", tagtest.APE()},
		{"footer only", tagtest.APEFooterOnly()},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := describe(t, KindAPE, c.tag)
			wantValue(t, rep, "APE tag", core.ByteCount(int64(len(c.tag))))
			wantValue(t, rep, "Title", "Test APE Title")
			wantValue(t, rep, "Artist", tagtest.Fake)
			wantValue(t, rep, "Other tag", "Custom Key: Test APE Custom")
			wantValue(t, rep, "Other tag", "Binary Blob: 15 bytes of binary data")
			if values(rep, "Embedded picture") == nil {
				t.Errorf("no picture reported: %+v", rep.Findings)
			}
		})
	}
	t.Run("damaged item", func(t *testing.T) {
		tag := tagtest.APE()
		tag[32] = 0xFF // the first item's size now runs past the tag
		tag[33] = 0xFF
		wantValue(t, describe(t, KindAPE, tag), "Undecoded tag data", core.ByteCount(int64(len(tag)-64)))
	})
}

func TestID3v1AndEnhanced(t *testing.T) {
	rep := describe(t, KindID3v1, tagtest.ID3v1())
	wantValue(t, rep, "Title", "Test V1 Title")
	wantValue(t, rep, "Artist", tagtest.Fake)
	wantValue(t, rep, "Album", "Test V1 Album")
	wantValue(t, rep, "Year", "2001")
	wantValue(t, rep, "Comment", "Test V1 Comment")

	rep = describe(t, KindEnhanced, tagtest.Enhanced())
	wantValue(t, rep, "Title", "Test Plus Title")
	wantValue(t, rep, "Artist", tagtest.Fake)
	wantValue(t, rep, "Genre", "Test Plus Genre")

	wantValue(t, describe(t, KindID3v1, tagtest.ID3v1()), "Genre", "genre number 12")

	withTrack := tagtest.ID3v1()
	withTrack[126] = 7 // ID3v1.1: a zero and then the track, in the comment's last bytes
	wantValue(t, describe(t, KindID3v1, withTrack), "Track number", "7")

	noGenre := tagtest.ID3v1()
	noGenre[127] = 0xFF
	if got := values(describe(t, KindID3v1, noGenre), "Genre"); got != nil {
		t.Errorf("an unset genre was reported: %q", got)
	}

	empty := append([]byte("TAG"), make([]byte, 125)...)
	rep = describe(t, KindID3v1, empty)
	if len(rep.Findings) != 1 || rep.Findings[0].Field != "ID3v1 tag" {
		t.Errorf("an empty tag should report just its presence: %+v", rep.Findings)
	}
}

func TestLyrics3(t *testing.T) {
	rep := describe(t, KindLyrics3v1, tagtest.Lyrics3v1())
	wantValue(t, rep, "Lyrics", "Test Lyrics One by "+tagtest.Fake)

	rep = describe(t, KindLyrics3v2, tagtest.Lyrics3v2())
	wantValue(t, rep, "Lyrics", "Test Lyrics Two")
	wantValue(t, rep, "Author", tagtest.Fake)
	wantValue(t, rep, "Title", "Test Lyrics Title")
}

// kinds lists the kinds of regions, nearest the end first.
func kinds(regions []Region) []Kind {
	out := make([]Kind, len(regions))
	for i, r := range regions {
		out[i] = r.Kind
	}
	return out
}

func TestTrailingTags(t *testing.T) {
	audio := []byte("audio bytes")
	concat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	footer := []byte("3DI\x04\x00\x10\x00\x00\x00\x00")
	id3WithFooter := append(tagtest.Header(4, 0x10, 0), footer...)

	for _, c := range []struct {
		name string
		data []byte
		want []Kind
	}{
		{"nothing", audio, []Kind{}},
		{"ID3v1", concat(audio, tagtest.ID3v1()), []Kind{KindID3v1}},
		{"enhanced", concat(audio, tagtest.Enhanced(), tagtest.ID3v1()), []Kind{KindID3v1, KindEnhanced}},
		{"APE with header", concat(audio, tagtest.APE()), []Kind{KindAPE}},
		{"APE footer only", concat(audio, tagtest.APEFooterOnly()), []Kind{KindAPE}},
		{"Lyrics3 v1", concat(audio, tagtest.Lyrics3v1()), []Kind{KindLyrics3v1}},
		{"Lyrics3 v2", concat(audio, tagtest.Lyrics3v2()), []Kind{KindLyrics3v2}},
		{"ID3v2 with footer", concat(audio, id3WithFooter), []Kind{KindID3v2}},
		{"all of them", concat(audio, tagtest.APE(), tagtest.Lyrics3v2(), tagtest.ID3v1()), []Kind{KindID3v1, KindLyrics3v2, KindAPE}},
		{"APE footer with an impossible size", concat(audio, apeFooter(2000, 1<<20, 0)), []Kind{}},
		{"APE footer with an unknown version", concat(audio, apeFooter(3000, 32, 0)), []Kind{}},
		{"APE block that says it is a header", concat(audio, apeFooter(2000, 32, 1<<29)), []Kind{}},
		{"Lyrics3 v2 without its start", concat(audio, []byte("000010LYRICS200")), []Kind{}},
		{"Lyrics3 v2 with a bad size", concat(audio, []byte("abcdefLYRICS200")), []Kind{}},
		{"Lyrics3 v1 without its start", concat(audio, []byte("LYRICSEND")), []Kind{}},
		{"ID3v2 footer without a tag", concat(audio, footer), []Kind{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			win := NewWindow(core.BytesSource(c.data))
			got := kinds(TrailingTags(win, int64(len(audio)), int64(len(c.data))))
			if !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The walk must not run past the floor even if a tag claims to start before it.
func TestTrailingTagsRespectFloor(t *testing.T) {
	data := append([]byte("audio bytes"), tagtest.APE()...)
	win := NewWindow(core.BytesSource(data))
	if got := TrailingTags(win, int64(len(data)-40), int64(len(data))); len(got) != 0 {
		t.Errorf("found a tag reaching below the floor: %+v", got)
	}
}

func TestTrailingTagsAreCapped(t *testing.T) {
	data := bytes.Repeat(tagtest.ID3v1(), maxTrailers*2)
	win := NewWindow(core.BytesSource(data))
	if got := TrailingTags(win, 0, int64(len(data))); len(got) != maxTrailers {
		t.Errorf("got %d tags, want the cap of %d", len(got), maxTrailers)
	}
}

func apeFooter(version, size, flags uint32) []byte {
	out := []byte("APETAGEX")
	for _, v := range []uint32{version, size, 0, flags} {
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return append(out, make([]byte, 8)...)
}

func TestTagAt(t *testing.T) {
	data := append([]byte("xx"), tagtest.ID3v23()...)
	win := NewWindow(core.BytesSource(data))
	r, ok := TagAt(win, 2, int64(len(data)))
	if !ok || r.Kind != KindID3v2 || r.Len != int64(len(data)-2) {
		t.Errorf("got %+v, %v", r, ok)
	}
	if _, ok := TagAt(win, 2, int64(len(data)-1)); ok {
		t.Error("accepted a tag running past the limit")
	}
	if _, ok := TagAt(win, 0, int64(len(data))); ok {
		t.Error("accepted bytes that are not a tag")
	}
	// A footer is not a header, so it cannot start a tag.
	ape := tagtest.APEFooterOnly()
	win = NewWindow(core.BytesSource(ape))
	if _, ok := TagAt(win, int64(len(ape)-32), int64(len(ape))); ok {
		t.Error("accepted an APE footer as a header")
	}
}

func TestLooksLikeTrailer(t *testing.T) {
	block := func(sig string, at int) []byte {
		out := make([]byte, TailSize)
		copy(out[TailSize-at:], sig)
		return out
	}
	for _, c := range []struct {
		name string
		tail []byte
		want bool
	}{
		{"nothing", make([]byte, TailSize), false},
		{"short", []byte("short"), false},
		{"empty", nil, false},
		{"ID3v1", block("TAG", 128), true},
		{"APE footer", block("APETAGEX", 32), true},
		{"Lyrics3 v2", block("LYRICS200", 9), true},
		{"Lyrics3 v1", block("LYRICSEND", 9), true},
		{"ID3v2 footer", block("3DI", 10), true},
		{"signature in the wrong place", block("TAG", 127), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksLikeTrailer(c.tail); got != c.want {
				t.Errorf("got %v", got)
			}
		})
	}
}

func TestVorbisComments(t *testing.T) {
	picture := base64.StdEncoding.EncodeToString(tagtest.FLACPicture())
	data := tagtest.VorbisComment("Test Vendor",
		"TITLE=Test Title", "artist="+tagtest.Fake, "DATE=2001", "COMMENT=Test Comment",
		"LOCATION=Eiffel Tower", "REPLAYGAIN_TRACK_GAIN=-1 dB", "MUSICBRAINZ_TRACKID=abc",
		"CUSTOM=Test Custom", "no separator", "METADATA_BLOCK_PICTURE="+picture, "COVERART=aGVsbG8=")
	sink := NewSink("test")
	rest := VorbisComments(append(data, 1), "c", sink)
	rep := sink.Report()
	if !bytes.Equal(rest, []byte{1}) {
		t.Errorf("rest = %v, want the framing byte", rest)
	}
	for _, c := range []struct{ field, want string }{
		{"Encoder", "Test Vendor"},
		{"Title", "Test Title"},
		{"Artist", tagtest.Fake},
		{"Date", "2001"},
		{"Comment", "Test Comment"},
		{"Location", "Eiffel Tower"},
		{"Loudness tag", "REPLAYGAIN_TRACK_GAIN: -1 dB"},
		{"Music database ID", "MUSICBRAINZ_TRACKID: abc"},
		{"Other tag", "CUSTOM: Test Custom"},
		{"Other tag", "no separator"},
		{"Embedded picture", "image/jpeg, front cover, Test Cover Desc, " + core.ByteCount(int64(len(picture))/4*3)},
		{"Embedded picture", "8 bytes of COVERART"},
	} {
		wantValue(t, rep, c.field, c.want)
	}
}

// Cleaned output holds an empty comment block, which must report nothing, or the
// engine would discard every cleaned file as still dirty.
func TestVorbisCommentsEmptyReportsNothing(t *testing.T) {
	sink := NewSink("test")
	if rest := VorbisComments(tagtest.VorbisComment(""), "c", sink); len(rest) != 0 {
		t.Errorf("rest = %v", rest)
	}
	if rep := sink.Report(); len(rep.Findings) != 0 {
		t.Errorf("findings: %+v", rep.Findings)
	}
}

func TestVorbisCommentsDamage(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"vendor past the end", []byte{0xFF, 0xFF, 0xFF, 0xFF}},
		{"no count", []byte{0, 0, 0, 0}},
		{"comment past the end", []byte{0, 0, 0, 0, 1, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"count larger than the data", []byte{0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := NewSink("test")
			VorbisComments(c.data, "c", sink)
			if values(sink.Report(), "Undecoded tag data") == nil {
				t.Error("the damage was not reported")
			}
		})
	}
}

func TestFLACPicture(t *testing.T) {
	full := tagtest.FLACPicture()
	if got, want := FLACPicture(full, 100), "image/jpeg, front cover, Test Cover Desc, 100 bytes"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, c := range []struct {
		name string
		b    []byte
		want string
	}{
		{"too short", []byte{1, 2}, "100 bytes"},
		{"mime past the end", []byte{0, 0, 0, 3, 0xFF, 0xFF, 0xFF, 0xFF}, "front cover, 100 bytes"},
		{"unknown type", []byte{0, 0, 0xFF, 0xFF, 0, 0, 0, 0}, "100 bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := FLACPicture(c.b, 100); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestScanTrailing(t *testing.T) {
	audio := []byte("audio bytes")
	junk := []byte("Test Trailing Junk")
	data := bytes.Join([][]byte{audio, tagtest.ID3v23(), junk, tagtest.APEFooterOnly(), tagtest.ID3v1()}, nil)
	sink := NewSink("test")
	win := NewWindow(core.BytesSource(data))
	off, n, err := ScanTrailing(context.Background(), win, int64(len(audio)), "after", sink)
	if err != nil {
		t.Fatal(err)
	}
	rep := sink.Report()
	wantValue(t, rep, "Trailing data", core.ByteCount(int64(len(junk))))
	if want := int64(len(audio) + len(tagtest.ID3v23())); off != want || n != int64(len(junk)) {
		t.Errorf("unknown region is %d+%d, want %d+%d", off, n, want, len(junk))
	}
	for _, field := range []string{"ID3v2 tag", "APE tag", "ID3v1 tag"} {
		if values(rep, field) == nil {
			t.Errorf("%q not reported", field)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ScanTrailing(ctx, NewWindow(core.BytesSource(data)), int64(len(audio)), "", NewSink("test")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled scan returned %v", err)
	}
}

func TestSinkCapsFindings(t *testing.T) {
	sink := NewSink("test")
	for i := range maxFindings + 10 {
		sink.Add(TitleField.Remove(fmt.Sprint(i), ""))
	}
	rep := sink.Report()
	if len(rep.Findings) != maxFindings+1 {
		t.Errorf("got %d findings", len(rep.Findings))
	}
	wantValue(t, rep, "Further metadata items", "10 more")
}

func TestAllZero(t *testing.T) {
	data := append(make([]byte, windowSize+10), 1)
	win := NewWindow(core.BytesSource(data))
	ctx := context.Background()
	if ok, err := AllZero(ctx, win, 0, windowSize+10); !ok || err != nil {
		t.Errorf("got %v, %v", ok, err)
	}
	if ok, err := AllZero(ctx, win, 0, windowSize+11); ok || err != nil {
		t.Errorf("got %v, %v", ok, err)
	}
	zeros := NewWindow(core.BytesSource(make([]byte, 16)))
	if _, err := AllZero(ctx, zeros, 0, 17); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("a range past the end returned %v", err)
	}
}

func TestWindow(t *testing.T) {
	data := make([]byte, windowSize*2+100)
	for i := range data {
		data[i] = byte(i % 251)
	}
	win := NewWindow(core.BytesSource(data))
	size := int64(len(data))

	for _, off := range []int64{0, 10, windowSize - 2, windowSize, 5, size - 4} {
		if got := win.At(off, 4); !bytes.Equal(got, data[off:off+4]) {
			t.Errorf("At(%d) = %v", off, got)
		}
	}
	if got := win.At(0, len(data)); !bytes.Equal(got, data) {
		t.Error("a read larger than the window failed")
	}
	for _, c := range []struct {
		off int64
		n   int
	}{{-1, 1}, {size, 1}, {size - 1, 2}, {size + 1, 0}, {0, -1}} {
		if got := win.At(c.off, c.n); got != nil {
			t.Errorf("At(%d, %d) = %v, want nil", c.off, c.n, got)
		}
	}
	if got := win.At(size, 0); got == nil || len(got) != 0 {
		t.Errorf("an empty read at the end = %v", got)
	}
	if got := win.AtMost(size-2, 10); !bytes.Equal(got, data[size-2:]) {
		t.Errorf("AtMost = %v", got)
	}
	if got := win.From(size - 3); !bytes.Equal(got, data[size-3:]) {
		t.Errorf("From = %v", got)
	}
	if got := win.From(size); got != nil {
		t.Errorf("From at the end = %v", got)
	}
	if win.Err() != nil {
		t.Errorf("Err = %v", win.Err())
	}
}

// failingSource fails every read, as a dying disk would.
type failingSource struct{ size int64 }

var errDisk = errors.New("disk failure")

func (s failingSource) Size() int64                           { return s.size }
func (s failingSource) ReadAt(_ []byte, _ int64) (int, error) { return 0, errDisk }

// A failed read must not pass for the end of the file, which would silently
// shorten the output of every walk built on this.
func TestWindowFailedRead(t *testing.T) {
	win := NewWindow(failingSource{1000})
	if got := win.At(0, 4); got != nil {
		t.Errorf("At = %v", got)
	}
	if !errors.Is(win.Err(), errDisk) {
		t.Errorf("Err = %v", win.Err())
	}
	sink := NewSink("test")
	if _, _, err := ScanTrailing(context.Background(), NewWindow(failingSource{1000}), 0, "", sink); !errors.Is(err, errDisk) {
		t.Errorf("ScanTrailing = %v", err)
	}
}

func TestFixedTextAndLatin1(t *testing.T) {
	if got := FixedText([]byte("Test\x00junk")); got != "Test" {
		t.Errorf("got %q", got)
	}
	if got := FixedText([]byte("  Test  ")); got != "Test" {
		t.Errorf("got %q", got)
	}
	if got := Latin1([]byte{0xE9}); got != "é" {
		t.Errorf("got %q", got)
	}
	if got := Latin1OrUTF8([]byte("é")); got != "é" {
		t.Errorf("got %q", got)
	}
	if got := Latin1OrUTF8([]byte{0xE9}); got != "é" {
		t.Errorf("got %q", got)
	}
}

func TestBytesAndJoin(t *testing.T) {
	if got := core.ByteCount(1); got != "1 byte" {
		t.Errorf("got %q", got)
	}
	if got := core.ByteCount(2); got != "2 bytes" {
		t.Errorf("got %q", got)
	}
	if got := Join("", "b"); got != "b" {
		t.Errorf("got %q", got)
	}
	if got := Join("a", "b"); got != "a/b" {
		t.Errorf("got %q", got)
	}
}

// clean prints every finding's Field and Where and hides only its Value, so that logs
// do not recreate what was removed. A name chosen by whoever wrote the file is as much
// metadata as its value, and has to go in Value too.
func TestNamesFromTheFileStayOutOfFieldAndWhere(t *testing.T) {
	const marker = "ZQ"
	apeItem := func(key string) []byte {
		out := append(binary.LittleEndian.AppendUint32(nil, 1), 0, 0, 0, 0)
		out = append(out, key...)
		return append(out, 0, 'v')
	}
	ape := func(key string) []byte {
		items := apeItem(key)
		return append(items, apeFooter(2000, uint32(len(items)+32), 0)...)
	}
	vorbis := func(key string) []byte { return tagtest.VorbisComment("", key+"=v") }
	check := func(t *testing.T, rep *core.Report, wantValue string) {
		t.Helper()
		sawValue := wantValue == ""
		for _, f := range rep.Findings {
			if strings.Contains(f.Field, marker) || strings.Contains(f.Where, marker) {
				t.Errorf("the name is in Field %q or Where %q", f.Field, f.Where)
			}
			sawValue = sawValue || strings.Contains(f.Value, wantValue)
		}
		if !sawValue {
			t.Errorf("the name is not in any Value, so inspect cannot show it: %+v", rep.Findings)
		}
	}

	for _, c := range []struct {
		name, id string
		major    byte
		frame    []byte
		value    string
	}{
		{"v2.3 frame", "ZQMK", 3, tagtest.Frame23("ZQMK", 0, []byte("\x00v")), "ZQMK"},
		{"v2.4 frame", "ZQMK", 4, tagtest.Frame24("ZQMK", 0, []byte("\x00v")), "ZQMK"},
		{"v2.2 frame", "ZQM", 2, tagtest.Frame22("ZQM", []byte("\x00v")), "ZQM"},
		{"text frame", "TZQM", 3, tagtest.Frame23("TZQM", 0, []byte("\x00v")), "TZQM"},
		{"URL frame", "WZQM", 3, tagtest.Frame23("WZQM", 0, []byte("http://example.com/")), "WZQM"},
		{"encrypted frame", "ZQMK", 3, tagtest.Frame23("ZQMK", 0x0040, []byte("xv")), ""},
		{"damaged frame", "ZQMK", 3, tagtest.Frame23("ZQMK", 0x0080, []byte("x")), ""},
		{"user text description", "TXXX", 3, tagtest.Frame23("TXXX", 0, []byte("\x00ZQMK\x00v")), "ZQMK"},
		{"user URL description", "WXXX", 3, tagtest.Frame23("WXXX", 0, []byte("\x00ZQMK\x00v")), "ZQMK"},
	} {
		t.Run(c.name, func(t *testing.T) {
			check(t, describe(t, KindID3v2, tagtest.Tag(c.major, 0, c.frame)), c.value)
		})
	}
	t.Run("frame running past the tag", func(t *testing.T) {
		frame := tagtest.Frame23("ZQMK", 0, []byte("\x00v"))
		frame[7] = 200
		check(t, describe(t, KindID3v2, tagtest.Tag(3, 0, frame)), "")
	})
	t.Run("version", func(t *testing.T) {
		for _, f := range describe(t, KindID3v2, tagtest.Header(200, 0, 0)).Findings {
			if strings.Contains(f.Where, "200") {
				t.Errorf("the version byte is in Where %q", f.Where)
			}
		}
	})

	for _, key := range []string{
		"ZQMK", "REPLAYGAIN_ZQMK", "R128_ZQMK", "MUSICBRAINZ_ZQMK", "ACOUSTID_ZQMK", "DISCOGS_ZQMK",
	} {
		t.Run("Vorbis key "+key, func(t *testing.T) {
			sink := NewSink("test")
			VorbisComments(vorbis(key), "where", sink)
			check(t, sink.Report(), key)
		})
		t.Run("APE key "+key, func(t *testing.T) {
			check(t, describe(t, KindAPE, ape(key)), key)
		})
	}
	t.Run("APE cover art key", func(t *testing.T) {
		check(t, describe(t, KindAPE, ape("Cover Art (ZQMK)")), "ZQMK")
	})

	// Even the case of a name it knows is the writer's choice.
	t.Run("case of a known key", func(t *testing.T) {
		sink := NewSink("test")
		VorbisComments(vorbis("aRtIsT"), "where", sink)
		rep := sink.Report()
		wantValue(t, rep, "Artist", "v")
		if got := rep.Findings[len(rep.Findings)-1].Where; got != "where/ARTIST" {
			t.Errorf("Where = %q", got)
		}
	})
}
