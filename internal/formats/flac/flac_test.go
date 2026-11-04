package flac

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/axiom8z/hideme/internal/audiotag/tagtest"
	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// crc8 and crc16 are bit-by-bit, so these fixtures do not share the handler's tables.
func crc8(b []byte) byte {
	var c byte
	for _, v := range b {
		c ^= v
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func crc16(b []byte) uint16 {
	var c uint16
	for _, v := range b {
		c ^= uint16(v) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// buildFrame builds a frame around body. The fields say a 4096-sample block, values
// from STREAMINFO, and mono. Nothing decodes it, so body need not be real subframes.
func buildFrame(index byte, body []byte) []byte {
	out := []byte{0xFF, 0xF8, 0xC0, 0x00, index & 0x7F}
	out = append(out, crc8(out))
	out = append(out, body...)
	return binary.BigEndian.AppendUint16(out, crc16(out))
}

// frame builds a frame with a body that depends on its position and has no 0xFF, so
// it holds no chance sync.
func frame(index int) []byte {
	body := make([]byte, 200)
	for i := range body {
		body[i] = byte((index*5 + i*11) % 251)
	}
	return buildFrame(byte(index), body)
}

func frames(start, n int) []byte {
	var out []byte
	for i := start; i < start+n; i++ {
		out = append(out, frame(i)...)
	}
	return out
}

// streamInfo returns a block with a recognisable MD5 field.
func streamInfo() []byte {
	out := make([]byte, streamInfoLen)
	binary.BigEndian.PutUint16(out, 4096)
	binary.BigEndian.PutUint16(out[2:], 4096)
	out[10], out[11], out[12] = 0x0A, 0xC4, 0x40 // 44.1 kHz, mono, 16 bit
	copy(out[18:], "STREAMINFO MD5 !")
	return out
}

type block struct {
	kind byte
	body []byte
}

func build(blocks []block, audio []byte) []byte {
	out := []byte("fLaC")
	for i, b := range blocks {
		kind := b.kind
		if i == len(blocks)-1 {
			kind |= blockLastFlag
		}
		n := len(b.body)
		out = append(out, kind, byte(n>>16), byte(n>>8), byte(n))
		out = append(out, b.body...)
	}
	return append(out, audio...)
}

func seekPoint(sample, offset uint64, samples uint16) []byte {
	out := binary.BigEndian.AppendUint64(nil, sample)
	out = binary.BigEndian.AppendUint64(out, offset)
	return binary.BigEndian.AppendUint16(out, samples)
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

func fakeComment() []byte {
	return tagtest.VorbisComment("Test Vendor", "TITLE=Test Title", "ARTIST="+tagtest.Fake, "DATE=2001-02-03", "CUSTOM=Test Custom")
}

func TestContract(t *testing.T) {
	audio := frames(0, 6)
	info := block{blockStreamInfo, streamInfo()}
	plain := build([]block{info}, audio)
	secret := [][]byte{[]byte(tagtest.Fake), []byte("Test Title"), []byte("Test Vendor"), []byte("Test Custom")}

	withBlocks := build([]block{
		info,
		{blockVorbis, fakeComment()},
		{blockPicture, tagtest.FLACPicture()},
		{blockApplication, []byte("TEST Test Application Bytes")},
		{blockCueSheet, append([]byte("Test Catalog"), make([]byte, 400)...)},
		{blockPadding, make([]byte, 64)},
		{42, []byte("Test Unknown Block")},
	}, audio)

	cases := []coretest.Case{
		{Name: "no metadata", Data: plain, MustContain: [][]byte{streamInfo(), audio}},
		{
			Name: "every metadata block",
			Data: withBlocks,
			WantFields: []string{
				"Encoder", "Title", "Artist", "Date", "Other tag", "Embedded picture",
				"Application data", "Cue sheet", "Padding", "Unknown chunk",
			},
			MustNotContain: append(secret,
				[]byte("Test Cover"), []byte("Test Application"), []byte("Test Catalog"), []byte("Test Unknown")),
			MustContain: [][]byte{streamInfo(), audio},
		},
		{
			Name:           "hidden data in padding",
			Data:           build([]block{info, {blockPadding, []byte("Test Padding Secret")}}, audio),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{[]byte("Test Padding Secret")},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "prepended ID3v2",
			Data:           concat(tagtest.ID3v23(), plain),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "two prepended ID3v2 tags",
			Data:           concat(tagtest.ID3v24(), tagtest.ID3v22(), plain),
			WantFields:     []string{"ID3v2 tag", "Title", "Artist"},
			MustNotContain: [][]byte{[]byte("ID3"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "trailing ID3v1",
			Data:           concat(plain, tagtest.ID3v1()),
			WantFields:     []string{"ID3v1 tag", "Artist"},
			MustNotContain: [][]byte{[]byte("TAG"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "trailing APE and ID3v1",
			Data:           concat(plain, tagtest.APE(), tagtest.ID3v1()),
			WantFields:     []string{"APE tag", "ID3v1 tag", "Embedded picture"},
			MustNotContain: [][]byte{[]byte("APETAGEX"), []byte("TAG"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "trailing footerless ID3v2",
			Data:           concat(plain, tagtest.ID3v23()),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "trailing junk",
			Data:           concat(plain, []byte("Test Trailing Junk")),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Junk")},
			MustContain:    [][]byte{audio},
		},
		{
			Name:           "seek table kept",
			Data:           build([]block{info, {blockSeekTable, seekPoint(0, 0, 4096)}, {blockVorbis, fakeComment()}}, audio),
			WantFields:     []string{"Title"},
			MustNotContain: secret,
			MustContain:    [][]byte{seekPoint(0, 0, 4096), audio},
		},
		{
			Name:           "invalid seek table dropped",
			Data:           build([]block{info, {blockSeekTable, seekPoint(0, 1<<40, 4096)}}, audio),
			WantFields:     []string{"Invalid seek table"},
			MustNotContain: [][]byte{seekPoint(0, 1<<40, 4096)},
			MustContain:    [][]byte{audio},
		},
		{Name: "no audio frames", Data: build([]block{info}, nil), MustContain: [][]byte{streamInfo()}},
	}
	coretest.Contract(t, handler{}, cases)
}

func TestRobustness(t *testing.T) {
	data := build([]block{
		{blockStreamInfo, streamInfo()},
		{blockSeekTable, seekPoint(0, 0, 4096)},
		{blockVorbis, fakeComment()},
		{blockPicture, tagtest.FLACPicture()},
		{blockPadding, make([]byte, 32)},
	}, frames(0, 4))
	coretest.Robustness(t, handler{}, concat(tagtest.ID3v24(), data, tagtest.APE(), tagtest.ID3v1()))
}

func FuzzClean(f *testing.F) {
	audio := frames(0, 3)
	info := block{blockStreamInfo, streamInfo()}
	f.Add(build([]block{info}, audio))
	f.Add(build([]block{info, {blockSeekTable, seekPoint(0, 0, 4096)}, {blockVorbis, fakeComment()}}, audio))
	f.Add(concat(tagtest.ID3v23(), build([]block{info, {blockPicture, tagtest.FLACPicture()}}, audio), tagtest.ID3v1()))
	f.Add(concat(build([]block{info, {blockPadding, make([]byte, 16)}}, audio), tagtest.APE()))
	f.Add(concat(build([]block{info}, audio), []byte("Test Junk")))
	f.Add(build([]block{info}, nil))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}

// The whole output is predictable: the magic, STREAMINFO flagged as the last block,
// and the frames copied verbatim.
func TestOutputLayout(t *testing.T) {
	audio := frames(0, 10)
	dirty := concat(tagtest.ID3v24(), build([]block{
		{blockStreamInfo, streamInfo()},
		{blockVorbis, fakeComment()},
		{blockPadding, make([]byte, 100)},
	}, audio), tagtest.ID3v1())
	out, _ := clean(t, dirty)
	want := concat([]byte("fLaC"), []byte{blockLastFlag, 0, 0, streamInfoLen}, streamInfo(), audio)
	if !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

// The last-block flag has to move to whichever block now comes last. A reader that
// finds no last block would parse the audio as metadata.
func TestLastBlockFlagWithSeekTable(t *testing.T) {
	audio := frames(0, 3)
	table := seekPoint(0, 0, 4096)
	dirty := build([]block{{blockStreamInfo, streamInfo()}, {blockSeekTable, table}, {blockVorbis, fakeComment()}}, audio)
	out, _ := clean(t, dirty)
	want := concat([]byte("fLaC"),
		[]byte{blockStreamInfo, 0, 0, streamInfoLen}, streamInfo(),
		[]byte{blockSeekTable | blockLastFlag, 0, 0, seekPointLen}, table,
		audio)
	if !bytes.Equal(out, want) {
		t.Errorf("got %x\nwant %x", out[:80], want[:80])
	}
}

func TestSeekTableNormalization(t *testing.T) {
	const audioLen = 1000
	placeholder := func(rest ...byte) []byte {
		p := bytes.Repeat([]byte{0xFF}, 8)
		return append(p, append(rest, make([]byte, 10-len(rest))...)...)
	}
	for _, c := range []struct {
		name  string
		table []byte
		want  []byte
	}{
		{"nil", nil, nil},
		{"valid", concat(seekPoint(0, 0, 10), seekPoint(10, 500, 10)), concat(seekPoint(0, 0, 10), seekPoint(10, 500, 10))},
		{"placeholders kept", concat(seekPoint(0, 0, 10), placeholder()), concat(seekPoint(0, 0, 10), placeholder())},
		{"data in a placeholder is erased", placeholder('T', 'e', 's', 't'), placeholder()},
		{"offset past the audio", seekPoint(0, audioLen, 10), nil},
		{"out of order", concat(seekPoint(10, 0, 10), seekPoint(5, 1, 10)), nil},
		{"duplicate sample", concat(seekPoint(10, 0, 10), seekPoint(10, 1, 10)), nil},
		{"point after a placeholder", concat(placeholder(), seekPoint(10, 0, 10)), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeSeekTable(c.table, audioLen); !bytes.Equal(got, c.want) || (got == nil) != (c.want == nil) {
				t.Errorf("got %x, want %x", got, c.want)
			}
		})
	}
}

// bodyWithInteriorZero searches for a frame body whose running CRC passes through
// zero inside it. In a large frame this happens all the time, about once in 64 KiB.
func bodyWithInteriorZero(t *testing.T) []byte {
	t.Helper()
	prefix := []byte{0xFF, 0xF8, 0xC0, 0x00, 0x09}
	prefix = append(prefix, crc8(prefix))
	body := make([]byte, 400)
	for seed := range 1 << 24 {
		v := uint32(seed)
		for i := range body {
			v = v*1664525 + 1013904223
			body[i] = byte(v>>24) % 250 // no 0xFF, so no chance sync either
		}
		// Look for a zero well inside the body.
		if crc16(append(append([]byte(nil), prefix...), body[:200]...)) == 0 {
			return body
		}
	}
	t.Fatal("no such body found")
	return nil
}

// A CRC that hits zero inside a frame must not be mistaken for the end of the
// frame: that would truncate real audio in a large share of real files. This is the
// bug the last-frame logic exists to avoid.
func TestInteriorCRCZeroDoesNotEndFrame(t *testing.T) {
	last := buildFrame(9, bodyWithInteriorZero(t))
	audio := concat(frames(0, 3), last)
	data := build([]block{{blockStreamInfo, streamInfo()}}, audio)

	out, _ := clean(t, data)
	if !bytes.HasSuffix(out, audio) {
		t.Error("the last frame was cut at the interior zero")
	}
	// The same frame in the middle of the stream, and before a tag.
	for _, c := range []struct {
		name string
		data []byte
		want []byte
	}{
		{"mid-stream", build([]block{{blockStreamInfo, streamInfo()}}, concat(frames(0, 1), last, frames(2, 1))), concat(frames(0, 1), last, frames(2, 1))},
		{"before ID3v1", concat(data, tagtest.ID3v1()), audio},
		{"before APE", concat(data, tagtest.APE()), audio},
		{"before a footerless ID3v2", concat(data, tagtest.ID3v24()), audio},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _ := clean(t, c.data)
			if !bytes.HasSuffix(out, c.want) {
				t.Errorf("the frame was damaged: %d bytes", len(out))
			}
			if bytes.Contains(out, []byte(tagtest.Fake)) {
				t.Error("the tag survived")
			}
		})
	}
	// With junk after it, there are two possible ends and no way to tell them apart.
	t.Run("before junk", func(t *testing.T) {
		_, err := (handler{}).Clean(context.Background(), core.BytesSource(concat(data, []byte("Test Junk"))), &bytes.Buffer{}, core.Options{})
		if !errors.Is(err, core.ErrMalformed) {
			t.Errorf("Clean = %v, want ErrMalformed", err)
		}
	})
}

// A frame whose bytes read as an ID3v1 tag at the end of the file is still audio,
// because its CRC proves those bytes belong to it.
func TestTagLookalikeInsideFrameIsKept(t *testing.T) {
	body := make([]byte, 300)
	for i := range body {
		body[i] = byte(i % 200)
	}
	// 128 bytes before the end of the frame, which ends in a two-byte CRC.
	copy(body[len(body)-126:], "TAG")
	last := buildFrame(3, body)
	audio := concat(frames(0, 3), last)
	data := build([]block{{blockStreamInfo, streamInfo()}}, audio)
	out, rep := clean(t, data)
	if !bytes.HasSuffix(out, audio) {
		t.Errorf("the frame was damaged: %d bytes", len(out))
	}
	if hasField(rep, "ID3v1 tag") {
		t.Errorf("audio was reported as a tag: %+v", rep.Findings)
	}
}

func TestRefusals(t *testing.T) {
	info := block{blockStreamInfo, streamInfo()}
	audio := frames(0, 4)

	corrupt := bytes.Clone(audio)
	corrupt[300] ^= 0x55 // inside the second frame, whose CRC now fails

	hostile := func(blocks ...block) []byte { return build(blocks, audio) }
	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"corrupted frame before more audio", build([]block{info}, corrupt), core.ErrMalformed},
		{"junk between frames", build([]block{info}, concat(frames(0, 2), []byte("Test Junk"), frames(2, 2))), core.ErrMalformed},
		{"junk before the first frame", build([]block{info}, concat([]byte("Test Junk"), audio)), core.ErrMalformed},
		{"first block is not STREAMINFO", hostile(block{blockVorbis, fakeComment()}, info), core.ErrMalformed},
		{"two STREAMINFO blocks", hostile(info, info), core.ErrMalformed},
		{"short STREAMINFO", hostile(block{blockStreamInfo, make([]byte, 33)}), core.ErrMalformed},
		{"long STREAMINFO", hostile(block{blockStreamInfo, make([]byte, 35)}), core.ErrMalformed},
		{"two seek tables", hostile(info, block{blockSeekTable, nil}, block{blockSeekTable, nil}), core.ErrMalformed},
		{"seek table with a partial point", hostile(info, block{blockSeekTable, make([]byte, 17)}), core.ErrMalformed},
		{"invalid block type", hostile(info, block{blockInvalid, nil}), core.ErrMalformed},
		{"block past the end", []byte("fLaC\x00\x00\x00\x22"), core.ErrMalformed},
		{"no last block", concat([]byte("fLaC\x00\x00\x00\x22"), streamInfo()), core.ErrMalformed},
		{"no metadata", []byte("fLaC"), core.ErrMalformed},
		{"not FLAC", []byte("Test not a FLAC file"), core.ErrUnsupported},
		{"ID3v2 then not FLAC", concat(tagtest.ID3v24(), []byte("Test not a FLAC file")), core.ErrUnsupported},
		{"empty", nil, core.ErrUnsupported},
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

func TestTruncatedLastFrameIsDroppedWithWarning(t *testing.T) {
	audio := frames(0, 4)
	data := build([]block{{blockStreamInfo, streamInfo()}}, concat(audio, frame(4)[:100]))
	out, rep := clean(t, data)
	if !bytes.HasSuffix(out, audio) {
		t.Errorf("the good frames were damaged: %d bytes", len(out))
	}
	if !hasField(rep, "Trailing data") {
		t.Errorf("the partial frame was not reported: %+v", rep.Findings)
	}
	if len(rep.Warnings) != 1 {
		t.Errorf("warnings: %q", rep.Warnings)
	}
}

func TestMatch(t *testing.T) {
	plain := build([]block{{blockStreamInfo, streamInfo()}}, frames(0, 2))
	for _, c := range []struct {
		name string
		data []byte
		want bool
	}{
		{"FLAC", plain, true},
		{"ID3v2 then FLAC", concat(tagtest.ID3v23(), plain), true},
		{"two ID3v2 tags then FLAC", concat(tagtest.ID3v24(), tagtest.ID3v22(), plain), true},
		{"ID3v2 then MP3", concat(tagtest.ID3v24(), []byte{0xFF, 0xFB, 0x90, 0xC4, 0, 0, 0, 0}), false},
		{"ID3v2 only", tagtest.ID3v24(), false},
		{"ID3v2 past the end", tagtest.Header(3, 0, 1<<20), false},
		{"other", []byte("Test not a FLAC file"), false},
		{"short", []byte("fL"), false},
		{"empty", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (handler{}).Match(c.data[:min(len(c.data), core.HeadSize)], core.BytesSource(c.data)); got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFrameHeaderLen(t *testing.T) {
	withCRC := func(b ...byte) []byte { return append(b, crc8(b)) }
	for _, c := range []struct {
		name string
		b    []byte
		want int
	}{
		{"plain", withCRC(0xFF, 0xF8, 0xC0, 0x00, 0x05), 6},
		{"variable block size", withCRC(0xFF, 0xF9, 0xC0, 0x00, 0x05), 6},
		{"two-byte coded number", withCRC(0xFF, 0xF8, 0xC0, 0x00, 0xC2, 0x80), 7},
		{"seven-byte coded number", withCRC(0xFF, 0xF8, 0xC0, 0x00, 0xFE, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80), 12},
		{"8-bit block size", withCRC(0xFF, 0xF8, 0x60, 0x00, 0x05, 0xFF), 7},
		{"16-bit block size", withCRC(0xFF, 0xF8, 0x70, 0x00, 0x05, 0x0F, 0xFF), 8},
		{"8-bit sample rate", withCRC(0xFF, 0xF8, 0xCC, 0x00, 0x05, 0x10), 7},
		{"16-bit sample rate", withCRC(0xFF, 0xF8, 0xCD, 0x00, 0x05, 0xAC, 0x44), 8},
		{"bad CRC", []byte{0xFF, 0xF8, 0xC0, 0x00, 0x05, 0x00}, 0},
		{"reserved sync bit", withCRC(0xFF, 0xFA, 0xC0, 0x00, 0x05), 0},
		{"reserved block size", withCRC(0xFF, 0xF8, 0x00, 0x00, 0x05), 0},
		{"invalid sample rate", withCRC(0xFF, 0xF8, 0xCF, 0x00, 0x05), 0},
		{"reserved channels", withCRC(0xFF, 0xF8, 0xC0, 0xB0, 0x05), 0},
		{"reserved sample size", withCRC(0xFF, 0xF8, 0xC0, 0x06, 0x05), 0},
		{"reserved bit", withCRC(0xFF, 0xF8, 0xC0, 0x01, 0x05), 0},
		{"continuation byte as a coded number", withCRC(0xFF, 0xF8, 0xC0, 0x00, 0x80), 0},
		{"0xFF as a coded number", withCRC(0xFF, 0xF8, 0xC0, 0x00, 0xFF), 0},
		{"bad continuation byte", withCRC(0xFF, 0xF8, 0xC0, 0x00, 0xC2, 0x05), 0},
		{"too short", []byte{0xFF, 0xF8}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := frameHeaderLen(c.b); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// Check the CRC tables against the standard's bit-by-bit definition.
func TestCRCTables(t *testing.T) {
	data := []byte("123456789")
	var c8 byte
	for _, v := range data {
		c8 = crc8Table[c8^v]
	}
	var c16 uint16
	for _, v := range data {
		c16 = c16<<8 ^ crc16Table[byte(c16>>8)^v]
	}
	if c8 != crc8(data) || c8 != 0xF4 {
		t.Errorf("CRC-8 = %#x", c8)
	}
	if c16 != crc16(data) || c16 != 0xFEE8 {
		t.Errorf("CRC-16 = %#x", c16)
	}
}

func TestCancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := build([]block{{blockStreamInfo, streamInfo()}, {blockVorbis, fakeComment()}}, frames(0, 2))
	if _, err := (handler{}).Clean(ctx, core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Clean = %v", err)
	}
}

func TestStreamsWithoutLoadingTheFile(t *testing.T) {
	prefix := build([]block{{blockStreamInfo, streamInfo()}, {blockVorbis, fakeComment()}}, nil)
	period := frame(0)
	total := tagtest.WholePeriods(len(prefix), len(period), tagtest.StreamingFileSize)
	src := tagtest.Repeating{Prefix: prefix, Period: period, Total: total}
	var out tagtest.CountingWriter
	allocated := tagtest.Allocated(func() {
		if _, err := (handler{}).Clean(context.Background(), src, &out, core.Options{}); err != nil {
			t.Error(err)
		}
	})
	want := 4 + blockHeaderLen + streamInfoLen + total - int64(len(prefix))
	if out.N != want {
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

// A seek point has no free bits once it has to describe a real frame, so nothing can
// be stashed in a table that passes. These stashes all look fine to a check that goes
// no further than the order of the points and the size of the audio.
func TestSeekTableMustDescribeTheFrames(t *testing.T) {
	audio := frames(0, 6)
	n := uint64(len(frame(0)))
	stash := binary.BigEndian.Uint64([]byte("\x01TestHid"))
	info := block{blockStreamInfo, streamInfo()}
	for _, c := range []struct {
		name  string
		table []byte
		keep  bool
	}{
		{"every field right", concat(seekPoint(0, 0, 4096), seekPoint(4096*3, 3*n, 4096), seekPoint(4096*5, 5*n, 4096)), true},
		{"right, then placeholders", concat(seekPoint(4096, n, 4096), bytes.Repeat([]byte{0xFF}, 8), make([]byte, 10)), true},
		{"text in the sample number", seekPoint(stash, 0, 4096), false},
		{"text in the block size", seekPoint(0, 0, 0x5465), false},
		{"a sample number no frame starts at", seekPoint(100, 0, 4096), false},
		{"a sample number from another frame", seekPoint(4096*2, n, 4096), false},
		{"an offset inside a frame", seekPoint(0, 5, 4096), false},
		{"an offset between frames", concat(seekPoint(0, 0, 4096), seekPoint(4096, n+1, 4096)), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, rep := clean(t, build([]block{info, {blockSeekTable, c.table}}, audio))
			if got := bytes.Contains(out, c.table); got != c.keep {
				t.Errorf("table kept = %v, want %v", got, c.keep)
			}
			if got := hasField(rep, "Invalid seek table"); got == c.keep {
				t.Errorf("reported as invalid = %v, want %v", got, !c.keep)
			}
			if bytes.Contains(out, []byte("TestHid")) || !bytes.Contains(out, audio) {
				t.Errorf("stash kept or audio lost")
			}
		})
	}
}

// Blocks of differing sizes, which variable-blocksize streams use and which every
// coding of the block size can state.
func TestSeekTableWithVariableBlockSizes(t *testing.T) {
	sized := func(code byte, extra []byte, index byte) []byte {
		out := []byte{0xFF, 0xF9, code << 4, 0x00, index & 0x7F}
		out = append(out, extra...)
		out = append(out, crc8(out))
		out = append(out, make([]byte, 40)...)
		return binary.BigEndian.AppendUint16(out, crc16(out))
	}
	parts := [][]byte{
		sized(1, nil, 0),                // 192
		sized(3, nil, 1),                // 1152
		sized(6, []byte{99}, 2),         // 100, stored minus one in a byte
		sized(7, []byte{0x12, 0x33}, 3), // 0x1234, stored minus one in two bytes
		sized(8, nil, 4),                // 256
		sized(15, nil, 5),               // 32768
	}
	sizes := []uint16{192, 1152, 100, 0x1234, 256, 32768}
	var table []byte
	var sample, offset uint64
	for i, p := range parts {
		table = append(table, seekPoint(sample, offset, sizes[i])...)
		sample += uint64(sizes[i])
		offset += uint64(len(p))
	}
	out, rep := clean(t, build([]block{{blockStreamInfo, streamInfo()}, {blockSeekTable, table}}, concat(parts...)))
	if !bytes.Contains(out, table) || hasField(rep, "Invalid seek table") {
		t.Errorf("a valid table was dropped: %+v", rep.Findings)
	}
}
