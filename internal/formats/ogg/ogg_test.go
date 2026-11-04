package ogg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/audiotag/tagtest"
	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// crc is bit-by-bit, so these fixtures do not share the handler's table.
func crc(b []byte) uint32 {
	var c uint32
	for _, v := range b {
		c ^= uint32(v) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// rawPage builds a page from lacing values and a body.
func rawPage(flags byte, granule uint64, serial, seq uint32, lacing, body []byte) []byte {
	out := []byte("OggS\x00")
	out = append(out, flags)
	out = binary.LittleEndian.AppendUint64(out, granule)
	out = binary.LittleEndian.AppendUint32(out, serial)
	out = binary.LittleEndian.AppendUint32(out, seq)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = append(out, byte(len(lacing)))
	out = append(out, lacing...)
	out = append(out, body...)
	binary.LittleEndian.PutUint32(out[22:], crc(out))
	return out
}

// lace returns the lacing values for whole packets.
func lace(packets ...[]byte) []byte {
	var out []byte
	for _, p := range packets {
		n := len(p)
		for ; n >= 255; n -= 255 {
			out = append(out, 255)
		}
		out = append(out, byte(n))
	}
	return out
}

// onePage puts whole packets on one page.
func onePage(flags byte, granule uint64, serial, seq uint32, packets ...[]byte) []byte {
	return rawPage(flags, granule, serial, seq, lace(packets...), bytes.Join(packets, nil))
}

func vorbisID() []byte {
	out := append([]byte(nil), "\x01vorbis"...)
	out = append(out, 0, 0, 0, 0) // version
	out = append(out, 1)          // channels
	out = binary.LittleEndian.AppendUint32(out, 44100)
	out = append(out, make([]byte, 12)...) // bit rates
	return append(out, 0xB8, 1)            // block sizes, framing
}

func opusHead() []byte {
	out := append([]byte(nil), "OpusHead\x01\x01"...)
	out = binary.LittleEndian.AppendUint16(out, 312) // pre-skip
	out = binary.LittleEndian.AppendUint32(out, 48000)
	return append(out, 0, 0, 0) // gain, mapping family
}

func vorbisComment(body []byte) []byte {
	return append(append([]byte("\x03vorbis"), body...), 1)
}

func opusTags(body []byte) []byte {
	return append([]byte("OpusTags"), body...)
}

func fakeTags() []byte {
	return tagtest.VorbisComment("Test Vendor", "TITLE=Test Title", "ARTIST="+tagtest.Fake, "DATE=2001-02-03", "CUSTOM=Test Custom")
}

// vorbisSetup returns a setup header. It is padded to length with values that give
// no chance of reading as anything else.
func vorbisSetup(n int) []byte {
	out := make([]byte, n)
	copy(out, "\x05vorbis")
	for i := 7; i < n; i++ {
		out[i] = byte(i % 251)
	}
	return out
}

// vorbisAudio returns an audio packet, whose low bit is clear.
func vorbisAudio(i, n int) []byte {
	out := make([]byte, n)
	for j := range out {
		out[j] = byte((i*3 + j*7) % 250 &^ 1)
	}
	return out
}

func opusAudio(i, n int) []byte {
	out := make([]byte, n)
	for j := range out {
		out[j] = byte((i*3 + j*7) % 250)
	}
	return out
}

const serialA, serialB = 0x11111111, 0x22222222

// vorbisFile builds a typical file: the identification header alone on page 0, then
// comment and setup sharing page 1, then audio.
func vorbisFile(serial uint32, comment []byte) []byte {
	var out []byte
	out = append(out, onePage(flagBOS, 0, serial, 0, vorbisID())...)
	out = append(out, onePage(0, 0, serial, 1, vorbisComment(comment), vorbisSetup(600))...)
	out = append(out, onePage(0, 1024, serial, 2, vorbisAudio(0, 300), vorbisAudio(1, 300))...)
	out = append(out, onePage(flagEOS, 2048, serial, 3, vorbisAudio(2, 300))...)
	return out
}

func opusFile(serial uint32, comment []byte) []byte {
	var out []byte
	out = append(out, onePage(flagBOS, 0, serial, 0, opusHead())...)
	out = append(out, onePage(0, 0, serial, 1, opusTags(comment))...)
	out = append(out, onePage(0, 960, serial, 2, opusAudio(0, 100), opusAudio(1, 100))...)
	out = append(out, onePage(flagEOS, 1920, serial, 3, opusAudio(2, 100))...)
	return out
}

// spanPages splits a packet across pages of at most segs segments each, in the way
// a muxer lays out a comment too large for one page.
func spanPages(serial, firstSeq uint32, packet []byte, segs int) (pages []byte, next uint32) {
	seq := firstSeq
	var flags byte
	for len(packet) >= segs*255 {
		pages = append(pages, rawPage(flags, granuleNone, serial, seq, bytes.Repeat([]byte{255}, segs), packet[:segs*255])...)
		packet = packet[segs*255:]
		flags = flagContinued
		seq++
	}
	pages = append(pages, rawPage(flags, 0, serial, seq, lace(packet), packet)...)
	return pages, seq + 1
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

// parsedPage is a page read back independently of the handler's parser.
type parsedPage struct {
	flags   byte
	granule uint64
	serial  uint32
	seq     uint32
	lacing  []byte
	body    []byte
}

func parsePages(t *testing.T, data []byte) []parsedPage {
	t.Helper()
	var pages []parsedPage
	for len(data) > 0 {
		if len(data) < 27 || string(data[:4]) != "OggS" {
			t.Fatalf("no page where one should be, %d bytes from the end", len(data))
		}
		segs := int(data[26])
		lacing := data[27 : 27+segs]
		n := 0
		for _, l := range lacing {
			n += int(l)
		}
		total := 27 + segs + n
		raw := bytes.Clone(data[:total])
		want := binary.LittleEndian.Uint32(raw[22:])
		binary.LittleEndian.PutUint32(raw[22:], 0)
		if got := crc(raw); got != want {
			t.Errorf("page %d has a bad CRC", len(pages))
		}
		pages = append(pages, parsedPage{
			flags:   data[5],
			granule: binary.LittleEndian.Uint64(data[6:]),
			serial:  binary.LittleEndian.Uint32(data[14:]),
			seq:     binary.LittleEndian.Uint32(data[18:]),
			lacing:  lacing,
			body:    data[27+segs : total],
		})
		data = data[total:]
	}
	return pages
}

// packetsOf reassembles the packets of one logical stream.
func packetsOf(pages []parsedPage, serial uint32) [][]byte {
	var packets [][]byte
	var cur []byte
	for _, p := range pages {
		if p.serial != serial {
			continue
		}
		off := 0
		for _, l := range p.lacing {
			cur = append(cur, p.body[off:off+int(l)]...)
			off += int(l)
			if l < 255 {
				packets = append(packets, cur)
				cur = nil
			}
		}
	}
	return packets
}

func TestContract(t *testing.T) {
	secrets := [][]byte{[]byte(tagtest.Fake), []byte("Test Title"), []byte("Test Vendor"), []byte("Test Custom"), []byte("2001-02-03")}
	fields := []string{"Encoder", "Title", "Artist", "Date", "Other tag"}
	big := tagtest.VorbisComment("Test Vendor", "ARTIST="+tagtest.Fake, "COMMENT="+strings.Repeat("Test Comment ", 40000))
	spanned, next := spanPages(serialA, 1, opusTags(big), 255)
	bigOpus := concat(
		onePage(flagBOS, 0, serialA, 0, opusHead()), spanned,
		onePage(flagEOS, 960, serialA, next, opusAudio(0, 100)),
	)

	cases := []coretest.Case{
		{
			Name:           "Vorbis",
			Data:           vorbisFile(serialA, fakeTags()),
			WantFields:     fields,
			MustNotContain: secrets,
			MustContain:    [][]byte{vorbisSetup(600), vorbisAudio(0, 300)},
		},
		{
			Name:           "Opus",
			Data:           opusFile(serialA, fakeTags()),
			WantFields:     fields,
			MustNotContain: secrets,
			MustContain:    [][]byte{opusHead(), opusAudio(0, 100)},
		},
		{
			Name:        "already clean Vorbis",
			Data:        vorbisFile(serialA, tagtest.VorbisComment("")),
			MustContain: [][]byte{vorbisSetup(600)},
		},
		{
			Name:        "already clean Opus",
			Data:        opusFile(serialA, tagtest.VorbisComment("")),
			MustContain: [][]byte{opusHead()},
		},
		{
			Name:           "Opus comment spanning many pages",
			Data:           bigOpus,
			WantFields:     []string{"Encoder", "Artist", "Comment"},
			MustNotContain: [][]byte{[]byte(tagtest.Fake), []byte("Test Comment")},
			MustContain:    [][]byte{opusAudio(0, 100)},
		},
		{
			Name:           "padding in OpusTags",
			Data:           opusFile(serialA, append(tagtest.VorbisComment(""), "Test Padding"...)),
			WantFields:     []string{"Padding"},
			MustNotContain: [][]byte{[]byte("Test Padding")},
		},
		{
			Name:           "chained streams",
			Data:           concat(vorbisFile(serialA, fakeTags()), opusFile(serialB, fakeTags())),
			WantFields:     fields,
			MustNotContain: secrets,
			MustContain:    [][]byte{vorbisSetup(600), opusHead()},
		},
		{
			Name:           "chained streams sharing a serial number",
			Data:           concat(vorbisFile(serialA, fakeTags()), vorbisFile(serialA, fakeTags())),
			WantFields:     fields,
			MustNotContain: secrets,
		},
		{
			Name: "multiplexed streams",
			Data: concat(
				onePage(flagBOS, 0, serialA, 0, vorbisID()),
				onePage(flagBOS, 0, serialB, 0, opusHead()),
				onePage(0, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(600)),
				onePage(0, 0, serialB, 1, opusTags(fakeTags())),
				onePage(flagEOS, 1024, serialA, 2, vorbisAudio(0, 300)),
				onePage(flagEOS, 960, serialB, 2, opusAudio(0, 100)),
			),
			WantFields:     fields,
			MustNotContain: secrets,
			MustContain:    [][]byte{vorbisAudio(0, 300), opusAudio(0, 100)},
		},
		{
			Name:           "trailing tags",
			Data:           concat(vorbisFile(serialA, fakeTags()), tagtest.APE(), tagtest.ID3v1()),
			WantFields:     []string{"APE tag", "ID3v1 tag"},
			MustNotContain: [][]byte{[]byte("APETAGEX"), []byte("TAG"), []byte(tagtest.Fake)},
		},
		{
			Name:           "trailing junk",
			Data:           concat(vorbisFile(serialA, fakeTags()), []byte("Test Trailing Junk")),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Junk")},
		},
		{
			Name: "junk and a tag between chained streams",
			Data: concat(vorbisFile(serialA, fakeTags()), []byte("Test Gap Junk"), vorbisFile(serialB, fakeTags()),
				tagtest.ID3v23(), opusFile(0x33333333, fakeTags())),
			WantFields:     []string{"Non-page data", "ID3v2 tag"},
			MustNotContain: [][]byte{[]byte("Test Gap Junk"), []byte(tagtest.Fake), []byte("ID3")},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, concat(vorbisFile(serialA, fakeTags()), opusFile(serialB, fakeTags()), tagtest.ID3v1()))
}

func FuzzClean(f *testing.F) {
	f.Add(vorbisFile(serialA, fakeTags()))
	f.Add(opusFile(serialA, fakeTags()))
	f.Add(vorbisFile(serialA, tagtest.VorbisComment("")))
	f.Add(concat(vorbisFile(serialA, fakeTags()), opusFile(serialB, fakeTags())))
	f.Add(concat(vorbisFile(serialA, fakeTags()), []byte("Test Junk"), tagtest.APE()))
	f.Add(concat(
		onePage(flagBOS, 0, serialA, 0, vorbisID()),
		onePage(flagBOS, 0, serialB, 0, opusHead()),
		onePage(0, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(64)),
		onePage(0, 0, serialB, 1, opusTags(fakeTags())),
		onePage(flagEOS, 960, serialB, 2, opusAudio(0, 16)),
	))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}

// The strongest content check the standard library allows: read the output back
// with an independent parser, then compare packets, granules and numbering.
func TestVorbisPacketsSurvive(t *testing.T) {
	data := vorbisFile(serialA, fakeTags())
	out, _ := clean(t, data)

	before := parsePages(t, data)
	after := parsePages(t, out)
	in, got := packetsOf(before, serialA), packetsOf(after, serialA)
	if len(in) != len(got) {
		t.Fatalf("%d packets became %d", len(in), len(got))
	}
	for i := range in {
		if i == 1 {
			continue // the comment, checked below
		}
		if !bytes.Equal(in[i], got[i]) {
			t.Errorf("packet %d changed", i)
		}
	}
	wantComment := []byte("\x03vorbis\x00\x00\x00\x00\x00\x00\x00\x00\x01")
	if !bytes.Equal(got[1], wantComment) {
		t.Errorf("the comment packet is %q", got[1])
	}
	for i, p := range after {
		if p.serial != serialA {
			t.Errorf("page %d has serial %#x", i, p.serial)
		}
		if p.seq != uint32(i) {
			t.Errorf("page %d has sequence number %d", i, p.seq)
		}
	}
	if len(before) != len(after) {
		t.Fatalf("%d pages became %d", len(before), len(after))
	}
	for i := range before {
		if before[i].flags != after[i].flags || before[i].granule != after[i].granule {
			t.Errorf("page %d: flags %#x granule %d became flags %#x granule %d", i, before[i].flags, before[i].granule, after[i].flags, after[i].granule)
		}
	}
}

// A comment on many pages becomes one page, so everything after it moves down and
// needs a new sequence number and checksum, while granules stay as they were.
func TestLaterPagesAreRenumbered(t *testing.T) {
	big := tagtest.VorbisComment("Test Vendor", "COMMENT="+strings.Repeat("x", 300000))
	spanned, next := spanPages(serialA, 1, opusTags(big), 255)
	audio := [][]byte{opusAudio(0, 100), opusAudio(1, 100), opusAudio(2, 100)}
	data := concat(
		onePage(flagBOS, 0, serialA, 0, opusHead()), spanned,
		onePage(0, 960, serialA, next, audio[0]),
		onePage(0, 1920, serialA, next+1, audio[1]),
		onePage(flagEOS, 2880, serialA, next+2, audio[2]),
	)
	if next < 4 {
		t.Fatalf("the fixture only spans %d pages", next-1)
	}
	out, _ := clean(t, data)
	pages := parsePages(t, out)
	if len(pages) != 5 {
		t.Fatalf("got %d pages, want 5", len(pages))
	}
	for i, p := range pages {
		if p.seq != uint32(i) {
			t.Errorf("page %d has sequence number %d", i, p.seq)
		}
	}
	for i, wantGranule := range []uint64{960, 1920, 2880} {
		p := pages[i+2]
		if p.granule != wantGranule || !bytes.Equal(p.body, audio[i]) {
			t.Errorf("audio page %d changed: granule %d", i, p.granule)
		}
	}
	if pages[4].flags != flagEOS {
		t.Errorf("the end-of-stream flag moved: %#x", pages[4].flags)
	}
}

// Gaps in the original numbering mean pages were lost. A decoder uses them to notice
// that, so they have to survive renumbering.
func TestSequenceGapsSurvive(t *testing.T) {
	big := tagtest.VorbisComment("Test Vendor", "COMMENT="+strings.Repeat("x", 300000))
	spanned, next := spanPages(serialA, 1, opusTags(big), 255)
	data := concat(
		onePage(flagBOS, 0, serialA, 0, opusHead()), spanned,
		onePage(0, 960, serialA, next, opusAudio(0, 100)),
		onePage(flagEOS, 2880, serialA, next+5, opusAudio(1, 100)),
	)
	out, _ := clean(t, data)
	pages := parsePages(t, out)
	if got := pages[3].seq - pages[2].seq; got != 5 {
		t.Errorf("the gap became %d", got)
	}
}

// Setup headers of awkward sizes are where pagination bugs live: exact multiples of
// 255 need a zero lacing value, and more than 255 segments need another page.
func TestSetupSizesRepaginate(t *testing.T) {
	for _, n := range []int{
		64, 255, 254, 256, 510, 255 * 254, 255*254 + 239, 255 * 255, 255*255 - 16, 255*255 + 1, 255 * 256, 255 * 510, 200000,
	} {
		setup := vorbisSetup(n)
		lacing := lace(vorbisComment(fakeTags()), setup)
		body := concat(vorbisComment(fakeTags()), setup)
		// Lay the original out on full pages, as a muxer would.
		var mid []byte
		var flags byte
		seq := uint32(1)
		for len(lacing) > 0 {
			segs := lacing[:min(255, len(lacing))]
			bodyLen := 0
			for _, l := range segs {
				bodyLen += int(l)
			}
			mid = append(mid, rawPage(flags, 0, serialA, seq, segs, body[:bodyLen])...)
			body = body[bodyLen:]
			lacing = lacing[len(segs):]
			seq++
			flags = 0
			if segs[len(segs)-1] == 255 {
				flags = flagContinued
			}
		}
		data := concat(
			onePage(flagBOS, 0, serialA, 0, vorbisID()), mid,
			onePage(flagEOS, 1024, serialA, seq, vorbisAudio(0, 300)),
		)

		out, _ := clean(t, data)
		packets := packetsOf(parsePages(t, out), serialA)
		if len(packets) != 4 {
			t.Errorf("setup of %d bytes: got %d packets", n, len(packets))
			continue
		}
		if !bytes.Equal(packets[2], setup) {
			t.Errorf("setup of %d bytes did not survive", n)
		}
		if !bytes.Equal(packets[3], vorbisAudio(0, 300)) {
			t.Errorf("setup of %d bytes: the audio did not survive", n)
		}
		// Idempotence on exactly these layouts, which the contract cases do not reach.
		if again, _ := clean(t, out); !bytes.Equal(again, out) {
			t.Errorf("setup of %d bytes: cleaning twice changed the output", n)
		}
		for i, p := range parsePages(t, out) {
			if p.seq != uint32(i) {
				t.Errorf("setup of %d bytes: page %d has sequence number %d", n, i, p.seq)
			}
		}
	}
}

// Everything nobody can verify is refused, loudly, instead of passed through.
func TestRefusals(t *testing.T) {
	good := vorbisFile(serialA, fakeTags())

	badCRC := bytes.Clone(good)
	badCRC[len(badCRC)-1] ^= 0x55

	badCommentCRC := bytes.Clone(good)
	badCommentCRC[len(onePage(flagBOS, 0, serialA, 0, vorbisID()))+40] ^= 0x55

	version := bytes.Clone(good)
	version[4] = 1
	binary.LittleEndian.PutUint32(version[22:], 0)
	binary.LittleEndian.PutUint32(version[22:], crc(version[:len(onePage(flagBOS, 0, serialA, 0, vorbisID()))]))

	reserved := concat(
		onePage(flagBOS, 0, serialA, 0, vorbisID()),
		onePage(0x08, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(64)),
	)

	bos := func(packet []byte) []byte { return onePage(flagBOS, 0, serialA, 0, packet) }
	vorbisHead := concat(bos(vorbisID()), onePage(0, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(64)))
	opusHeaders := concat(bos(opusHead()), onePage(0, 0, serialA, 1, opusTags(fakeTags())))

	oddVorbis := vorbisID()
	binary.LittleEndian.PutUint32(oddVorbis[7:], 1)
	oddOpus := opusHead()
	oddOpus[8] = 0x10
	unknownFamily := opusHead()
	unknownFamily[18] = 9

	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"Theora", bos([]byte("\x80theora-test-header")), core.ErrUnsupported},
		{"Speex", bos([]byte("Speex   test-header")), core.ErrUnsupported},
		{"FLAC in Ogg", bos([]byte("\x7fFLAC-test-header")), core.ErrUnsupported},
		{"Skeleton", bos([]byte("fishead\x00test-header")), core.ErrUnsupported},
		{"unknown codec", bos([]byte("Test Unknown Codec")), core.ErrUnsupported},
		{"Vorbis multiplexed with Theora", concat(onePage(flagBOS, 0, serialB, 0, vorbisID()), bos([]byte("\x80theora-test-header"))), core.ErrUnsupported},
		{"Vorbis chained to Speex", concat(good, onePage(flagBOS, 0, serialB, 0, []byte("Speex   test-header"))), core.ErrUnsupported},
		{"later Vorbis version", bos(oddVorbis), core.ErrUnsupported},
		{"later Opus version", bos(oddOpus), core.ErrUnsupported},
		{"unknown Opus mapping family", bos(unknownFamily), core.ErrUnsupported},

		{"bad CRC on an audio page", badCRC, core.ErrMalformed},
		{"bad CRC on the comment page", badCommentCRC, core.ErrMalformed},
		{"page version", version, core.ErrMalformed},
		{"reserved flag", reserved, core.ErrMalformed},
		{"no pages", []byte("Test no pages here"), core.ErrMalformed},
		{"empty", nil, core.ErrMalformed},
		{"page from an unknown stream", onePage(0, 0, serialA, 0, vorbisID()), core.ErrMalformed},
		{"stream begins twice", concat(bos(vorbisID()), bos(vorbisID())), core.ErrMalformed},
		{"page after end of stream", concat(good, onePage(0, 0, serialA, 9, vorbisAudio(9, 10))), core.ErrMalformed},
		{"identification only", bos(vorbisID()), core.ErrMalformed},
		{"no setup header", concat(bos(vorbisID()), onePage(0, 0, serialA, 1, vorbisComment(fakeTags()))), core.ErrMalformed},
		{"no OpusTags", bos(opusHead()), core.ErrMalformed},
		{"comment packet is something else", concat(bos(vorbisID()), onePage(0, 0, serialA, 1, []byte("\x05vorbis"), vorbisSetup(64))), core.ErrMalformed},
		{"setup packet is something else", concat(bos(vorbisID()), onePage(0, 0, serialA, 1, vorbisComment(fakeTags()), []byte("\x03vorbis"))), core.ErrMalformed},
		{"OpusTags missing its magic", concat(bos(opusHead()), onePage(0, 0, serialA, 1, []byte("NotOpusTags"))), core.ErrMalformed},
		{"audio on the page that ends the headers", concat(bos(vorbisID()), onePage(0, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(64), vorbisAudio(0, 16))), core.ErrMalformed},
		{"audio after OpusTags on its page", concat(bos(opusHead()), onePage(0, 0, serialA, 1, opusTags(fakeTags()), opusAudio(0, 16))), core.ErrMalformed},
		{"two packets on the first page", onePage(flagBOS, 0, serialA, 0, vorbisID(), vorbisID()), core.ErrMalformed},
		{"first page ends mid-packet", rawPage(flagBOS, 0, serialA, 0, []byte{255}, bytes.Repeat([]byte("\x01vorbis"), 255)[:255]), core.ErrMalformed},
		{"first page is also the last", onePage(flagBOS|flagEOS, 0, serialA, 0, vorbisID()), core.ErrMalformed},
		{"first page claims continuation", onePage(flagBOS|flagContinued, 0, serialA, 0, vorbisID()), core.ErrMalformed},
		{"continuation flag out of place", concat(bos(vorbisID()), onePage(flagContinued, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(64))), core.ErrMalformed},
		{"missing continuation flag", concat(bos(opusHead()), rawPage(0, 0, serialA, 1, []byte{255}, opusTags(make([]byte, 247))), onePage(0, 0, serialA, 2, []byte{0})), core.ErrMalformed},
		{"short Vorbis identification", bos(vorbisID()[:29]), core.ErrMalformed},
		{"long Vorbis identification", bos(append(vorbisID(), 'x')), core.ErrMalformed},
		{"short OpusHead", bos(opusHead()[:18]), core.ErrMalformed},
		{"long OpusHead", bos(append(opusHead(), "Test"...)), core.ErrMalformed},
		{"hidden comment among Vorbis audio", concat(vorbisHead, onePage(0, 1024, serialA, 2, vorbisAudio(0, 16), vorbisComment(fakeTags()))), core.ErrMalformed},
		{"hidden comment among Opus audio", concat(opusHeaders, onePage(0, 960, serialA, 2, opusAudio(0, 16), opusTags(fakeTags()))), core.ErrMalformed},
		{"second OpusHead among the audio", concat(opusHeaders, onePage(0, 960, serialA, 2, opusHead())), core.ErrMalformed},
		{"end of stream before the headers finish", concat(bos(vorbisID()), onePage(flagEOS, 0, serialA, 1, vorbisComment(fakeTags()))), core.ErrMalformed},
		{"partial header packet after the end of the headers", concat(bos(opusHead()), rawPage(0, 0, serialA, 1, []byte{32, 255}, append(opusTags(tagtest.VorbisComment("Test")), make([]byte, 255+32-len(opusTags(tagtest.VorbisComment("Test"))))...))), core.ErrMalformed},
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

// Files cut off in the audio are common, from interrupted recordings or downloads.
func TestPartialLastPageIsDropped(t *testing.T) {
	good := vorbisFile(serialA, fakeTags())
	lastLen := len(onePage(flagEOS, 2048, serialA, 3, vorbisAudio(2, 300)))
	cut := good[:len(good)-lastLen/2]
	out, rep := clean(t, cut)

	pages := parsePages(t, out)
	if len(pages) != 3 {
		t.Errorf("got %d pages, want 3", len(pages))
	}
	if len(rep.Warnings) != 1 {
		t.Errorf("warnings: %q", rep.Warnings)
	}
	if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "Trailing data" }) {
		t.Errorf("the partial page was not reported: %+v", rep.Findings)
	}
}

func TestCutOffInTheHeadersIsRefused(t *testing.T) {
	good := vorbisFile(serialA, fakeTags())
	cut := good[:len(onePage(flagBOS, 0, serialA, 0, vorbisID()))+40]
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(cut), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Clean = %v, want ErrMalformed", err)
	}
}

// Opus channel mappings other than 0 have a table that sets the expected length.
func TestOpusHeadLengths(t *testing.T) {
	head := func(family, channels byte, extra int) []byte {
		out := opusHead()
		out[9], out[18] = channels, family
		return append(out, make([]byte, extra)...)
	}
	family3 := head(3, 2, 2+2*(1+1)*2)
	family3[19], family3[20] = 1, 1

	for _, c := range []struct {
		name string
		head []byte
		ok   bool
	}{
		{"family 0", head(0, 2, 0), true},
		{"family 1", head(1, 6, 2+6), true},
		{"family 2", head(2, 4, 2+4), true},
		{"family 255", head(255, 2, 2+2), true},
		{"family 3", family3, true},
		{"family 1 too short", head(1, 6, 2+5), false},
		{"family 1 too long", head(1, 6, 2+7), false},
		{"family 3 too short for its fields", head(3, 2, 0), false},
		{"family 3 too long", append(bytes.Clone(family3), 0), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := checkIdentification(codecOpus, c.head); (err == nil) != c.ok {
				t.Errorf("checkIdentification = %v", err)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want bool
	}{
		{"Vorbis", vorbisFile(serialA, fakeTags()), true},
		{"Opus", opusFile(serialA, fakeTags()), true},
		{"a codec Clean refuses", onePage(flagBOS, 0, serialA, 0, []byte("\x80theora-test-header")), true},
		{"first page is not BOS", onePage(0, 0, serialA, 0, vorbisID()), false},
		{"wrong version", append([]byte("OggS\x01\x02"), make([]byte, 30)...), false},
		{"too short", []byte("OggS"), false},
		{"other", []byte("Test not an Ogg file, but long enough to check"), false},
		{"empty", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (handler{}).Match(c.data[:min(len(c.data), core.HeadSize)], core.BytesSource(c.data)); got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

// A capture pattern straddling the read buffer's boundary must still be found.
func TestCaptureSearchAcrossBuffers(t *testing.T) {
	const windowSize = 256 << 10
	for _, gap := range []int{windowSize - 3, windowSize - 2, windowSize - 1, windowSize, windowSize + 1} {
		first := vorbisFile(serialA, tagtest.VorbisComment(""))
		data := concat(first, make([]byte, gap-len(first)), vorbisFile(serialB, tagtest.VorbisComment("")))
		out, rep := clean(t, data)
		want := concat(first, vorbisFile(serialB, tagtest.VorbisComment("")))
		if !bytes.Equal(out, want) {
			t.Errorf("gap %d: got %d bytes, want %d; findings %+v", gap, len(out), len(want), rep.Findings)
		}
	}
}

func TestCRCMatchesReference(t *testing.T) {
	data := []byte("123456789")
	raw := append(make([]byte, 26), data...)
	if got, want := pageCRC(raw), crc(raw); got != want {
		t.Errorf("pageCRC = %#x, want %#x", got, want)
	}
	if got := crc(data); got != 0x89A1897F {
		t.Errorf("the reference CRC itself is wrong: %#x", got)
	}
}

func TestCancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := vorbisFile(serialA, fakeTags())
	if _, err := (handler{}).Clean(ctx, core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Clean = %v", err)
	}
}

func TestTooManyStreams(t *testing.T) {
	var data []byte
	for i := range maxActiveStreams + 1 {
		data = append(data, onePage(flagBOS, 0, uint32(i), 0, vorbisID())...)
	}
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("Clean = %v, want ErrTooLarge", err)
	}
}

// The setup header has to be held whole to re-paginate it, so it has a hard limit.
func TestOversizedSetupIsRefused(t *testing.T) {
	setupPages, _ := spanPages(serialA, 2, vorbisSetup(maxSetupLen+1), 255)
	data := concat(onePage(flagBOS, 0, serialA, 0, vorbisID()), onePage(0, 0, serialA, 1, vorbisComment(fakeTags())), setupPages)
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("Clean = %v, want ErrTooLarge", err)
	}
}

// A comment is only read for the report, so one too long to read in full must still
// be removed, with the report admitting what it did not decode.
func TestOversizedCommentIsStillRemoved(t *testing.T) {
	big := tagtest.VorbisComment("Test Vendor", "ARTIST="+tagtest.Fake, "COMMENT="+strings.Repeat("x", maxCommentRead+1))
	spanned, next := spanPages(serialA, 1, opusTags(big), 255)
	audio := opusAudio(0, 100)
	data := concat(onePage(flagBOS, 0, serialA, 0, opusHead()), spanned, onePage(flagEOS, 960, serialA, next, audio))

	out, rep := clean(t, data)
	if bytes.Contains(out, []byte(tagtest.Fake)) || bytes.Contains(out, []byte("xxxx")) {
		t.Error("the comment survived")
	}
	if !bytes.Contains(out, audio) {
		t.Error("the audio did not survive")
	}
	if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "Undecoded tag data" }) {
		t.Errorf("the report does not admit to the undecoded part: %+v", rep.Findings)
	}
	if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Value == tagtest.Fake }) {
		t.Errorf("what came before the cap was not decoded: %+v", rep.Findings)
	}
}

// cancelAfter is a context that becomes cancelled after a number of checks, to prove
// that a long scan notices by itself rather than only between pages.
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

func TestCancelDuringPageSearch(t *testing.T) {
	file := vorbisFile(serialA, fakeTags())
	data := concat(file, bytes.Repeat([]byte{'x'}, 1<<20), vorbisFile(serialB, fakeTags()))
	// One check per page, one for the loop reaching the junk, and then the search's first.
	checks := 4 + 1
	ctx := cancelAfter{context.Background(), &checks}
	if _, err := (handler{}).Inspect(ctx, core.BytesSource(data), core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Inspect = %v, want context.Canceled", err)
	}
	if checks != 0 {
		t.Errorf("%d checks were left over, so the search itself was not what noticed", checks)
	}
}

// Every page here is renumbered, because the comment shrinks from two pages to one,
// so this also proves that renumbering millions of pages costs no memory.
func TestStreamsWithoutLoadingTheFile(t *testing.T) {
	comment := tagtest.VorbisComment("Test Vendor", "COMMENT="+strings.Repeat("x", 600))
	spanned, next := spanPages(serialA, 1, opusTags(comment), 2)
	prefix := concat(onePage(flagBOS, 0, serialA, 0, opusHead()), spanned)
	if next != 3 {
		t.Fatalf("the comment spans %d pages, want 2", next-1)
	}
	period := onePage(0, 960, serialA, next, opusAudio(0, 1000))
	total := tagtest.WholePeriods(len(prefix), len(period), tagtest.StreamingFileSize)
	src := tagtest.Repeating{Prefix: prefix, Period: period, Total: total}
	var out tagtest.CountingWriter
	allocated := tagtest.Allocated(func() {
		if _, err := (handler{}).Clean(context.Background(), src, &out, core.Options{}); err != nil {
			t.Error(err)
		}
	})
	if out.N < total-int64(len(prefix)) {
		t.Errorf("wrote only %d bytes of a %d-byte file", out.N, total)
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

// A page saying it continues a packet when none is open starts nothing a player will
// read, which makes it a place to hide a comment packet from the check for them.
func TestContinuationOfNothingIsRefused(t *testing.T) {
	second := vorbisComment(fakeTags())
	head := concat(onePage(flagBOS, 0, serialA, 0, vorbisID()), onePage(0, 0, serialA, 1, vorbisComment(fakeTags()), vorbisSetup(64)))
	orphan := rawPage(flagContinued, 0, serialA, 2, lace(second), second)
	for name, data := range map[string][]byte{
		"on the first audio page": concat(head, orphan),
		"after a complete packet": concat(head, onePage(0, 1024, serialA, 2, vorbisAudio(0, 300)), rawPage(flagContinued, 0, serialA, 3, lace(second), second)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrMalformed) {
				t.Errorf("Clean = %v, want %v", err, core.ErrMalformed)
			}
		})
	}

	// An audio packet really can span pages, so continuing an open one must still work.
	long := vorbisAudio(0, 600)
	first := rawPage(0, granuleNone, serialA, 2, bytes.Repeat([]byte{255}, 2), long[:510])
	rest := rawPage(flagContinued|flagEOS, 1024, serialA, 3, []byte{90}, long[510:])
	if out, _ := clean(t, concat(head, first, rest)); !bytes.Contains(out, long[510:]) {
		t.Error("a packet spanning two pages was lost")
	}
}

// A tagger can put an ID3v2 tag in front of any file, and players skip it.
func TestLeadingID3v2(t *testing.T) {
	file := vorbisFile(serialA, fakeTags())
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name:           "one tag",
			Data:           concat(tagtest.ID3v23(), file),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{vorbisSetup(600)},
		},
		{
			Name:           "two tags",
			Data:           concat(tagtest.ID3v24(), tagtest.ID3v22(), file),
			WantFields:     []string{"ID3v2 tag", "Artist"},
			MustNotContain: [][]byte{[]byte("ID3"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{vorbisSetup(600)},
		},
	})
	for name, data := range map[string][]byte{
		"tag and nothing else": tagtest.ID3v23(),
		"tag then not Ogg":     concat(tagtest.ID3v23(), []byte("Test not an Ogg file, but long enough to check")),
	} {
		t.Run(name, func(t *testing.T) {
			if (handler{}).Match(data, core.BytesSource(data)) {
				t.Error("Match = true")
			}
		})
	}
}
