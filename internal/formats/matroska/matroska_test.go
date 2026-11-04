package matroska

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"slices"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// Fixture helpers. Everything is written with the shortest legal encodings, unless
// a test is about a longer one.

func idBytes(id uint32) []byte {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], id)
	return bytes.TrimLeft(raw[:], "\x00")
}

func sizeBytes(n int) []byte {
	for width := 1; width <= 8; width++ {
		if int64(n) <= int64(1)<<(7*width)-2 {
			var raw [8]byte
			binary.BigEndian.PutUint64(raw[:], uint64(n)|uint64(1)<<(7*width))
			return raw[8-width:]
		}
	}
	panic("size too large")
}

func el(id uint32, payloads ...[]byte) []byte {
	payload := slices.Concat(payloads...)
	return slices.Concat(idBytes(id), sizeBytes(len(payload)), payload)
}

// unknownEl writes a master with the one-byte unknown size.
func unknownEl(id uint32, payloads ...[]byte) []byte {
	return slices.Concat(idBytes(id), []byte{0xFF}, slices.Concat(payloads...))
}

func text(id uint32, s string) []byte { return el(id, []byte(s)) }

func num(id uint32, v uint64) []byte {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], v)
	trimmed := bytes.TrimLeft(raw[:], "\x00")
	if len(trimmed) == 0 {
		trimmed = []byte{0}
	}
	return el(id, trimmed)
}

func ebmlHeader(docType string) []byte {
	return el(idEBML,
		num(idEBMLVersion, 1), num(idEBMLReadVersion, 1),
		num(idEBMLMaxIDLength, 4), num(idEBMLMaxSizeLength, 8),
		text(idDocType, docType), num(idDocTypeVersion, 4), num(idDocTypeReadVersion, 2))
}

const (
	idTitle         = 0x7BA9
	idMuxingApp     = 0x4D80
	idWritingApp    = 0x5741
	idDateUTC       = 0x4461
	idSegmentFile   = 0x7384
	idSegmentUUID   = 0x73A4
	idTimestampSc   = 0x2AD7B1
	idTrackName     = 0x536E
	idTrackUID      = 0x73C5
	idCodecName     = 0x258688
	idVideo         = 0xE0
	idPixelWidth    = 0xB0
	idPixelHeight   = 0xBA
	idTimestamp     = 0xE7
	idCuePoint      = 0xBB
	idCueTime       = 0xB3
	idCueTrackPos   = 0xB7
	idCueTrack      = 0xF7
	idCueClusterPos = 0xF1
	idVendor        = 0xC9 // no such element in Matroska
	mediaBytes      = "MEDIA-BYTES-MUST-SURVIVE"
)

// plainInfo holds only what a cleaned file keeps, with the mandatory strings blank.
func plainInfo() []byte {
	return el(idInfo, num(idTimestampSc, 1000000), el(idMuxingApp, make([]byte, 4)), el(idWritingApp, make([]byte, 4)))
}

func taggedInfo() []byte {
	return el(idInfo,
		num(idTimestampSc, 1000000),
		text(idMuxingApp, "Test Muxer 1.0"),
		text(idWritingApp, "Test Writer 2.0"),
		text(idTitle, "Test Title"),
		el(idDateUTC, binary.BigEndian.AppendUint64(nil, 86400e9)), // one day after the epoch
		text(idSegmentFile, "C:/Users/Test Author/secret-file.mkv"),
		el(idSegmentUUID, []byte("TESTSEGMENTUID16")))
}

func videoTrack(extra ...[]byte) []byte {
	return el(idTrackEntry, slices.Concat(
		num(idTrackNumber, 1), num(idTrackUID, 77), num(idTrackType, trackVideo),
		text(idCodecID, "V_VP9"),
		el(idVideo, num(idPixelWidth, 16), num(idPixelHeight, 16)),
		slices.Concat(extra...)))
}

func cluster() []byte {
	return el(idCluster, num(idTimestamp, 0), simpleBlock())
}

func tags() []byte {
	simple := func(name, value string) []byte {
		return el(idSimpleTag, text(idTagName, name), text(idTagString, value))
	}
	return el(idTags, el(idTag,
		simple("TITLE", "Test Tag Title"),
		simple("ARTIST", "Test Author"),
		simple("COMMENT", "Test Comment"),
		simple("DATE_RELEASED", "2001-02-03"),
		simple("ENCODER", "Test Encoder"),
		simple("RECORDING_LOCATION", "Eiffel Tower 48.8584, 2.2945")))
}

func attachments() []byte {
	return el(idAttachments, el(idAttachedFile,
		text(idFileName, "secret-attachment.txt"),
		text(idFileMediaType, "text/plain"),
		text(idFileDescription, "Test Attachment Description"),
		el(idFileData, []byte("Test Attachment Data"))))
}

func chapters() []byte {
	return el(idChapters, el(idEditionEntry, el(idChapterAtom,
		el(idChapterDisplay, text(idChapString, "Test Chapter")))))
}

func seekEntry(id uint32, position uint64) []byte {
	var pos [8]byte
	binary.BigEndian.PutUint64(pos[:], position)
	return el(idSeek, el(idSeekID, idBytes(id)), el(idSeekPosition, pos[:]))
}

// indexedSegment lays out the children after a SeekHead that points at each of
// them, then wraps them in a Segment. The SeekHead has a fixed size, because its
// positions use eight bytes.
func indexedSegment(children ...[]byte) []byte {
	entries := func(start uint64) [][]byte {
		offset := start
		var out [][]byte
		for _, child := range children {
			id, _, err := parseID(child)
			if err != nil {
				panic(err)
			}
			out = append(out, seekEntry(id, offset))
			offset += uint64(len(child))
		}
		return out
	}
	head := el(idSeekHead, entries(0)...)
	return el(idSegment, el(idSeekHead, entries(uint64(len(head)))...), slices.Concat(children...))
}

func cues() []byte {
	return el(idCues, el(idCuePoint, num(idCueTime, 0),
		el(idCueTrackPos, num(idCueTrack, 1), num(idCueClusterPos, 5))))
}

// fullFile has one of everything the handler removes.
func fullFile() []byte {
	hidden := el(idVoid, []byte("Old Test Author in a Void"))
	vendor := el(idVendor, []byte("Test Vendor Data"))
	track := videoTrack(text(idTrackName, "Test Track Name"), text(idCodecName, "Test Codec Name"))
	return slices.Concat(
		ebmlHeader(docMatroska),
		indexedSegment(hidden, taggedInfo(), el(idTracks, track), cluster(), cues(), attachments(), chapters(), tags(), vendor),
		[]byte("Test Trailing Secret"))
}

var fullFields = []string{
	"Title", "Creation date", "Segment file name", "Segment UID",
	"Muxing application", "Writing application", "Track name",
	"Tag TITLE", "Tag ARTIST", "Tag COMMENT", "Tag DATE_RELEASED", "Tag ENCODER", "Tag RECORDING_LOCATION",
	"Attachment", "Attachment description", "Chapters", "Chapter title",
	"Unknown element", "Hidden data in Void element", "Trailing data",
}

func asBytes(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

var fullSecrets = asBytes(
	"Test Muxer", "Test Writer", "Test Title", "secret-file", "TESTSEGMENTUID16", "Test Track Name",
	"Test Codec Name", "Test Tag Title", "Test Author", "Test Comment", "2001-02-03", "Test Encoder",
	"Eiffel Tower", "secret-attachment", "Test Attachment", "Test Chapter", "Test Vendor Data",
	"Test Trailing Secret", "TITLE", "ARTIST",
)

func TestContract(t *testing.T) {
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name: "every kind of metadata", Data: fullFile(),
			WantFields: fullFields, MustNotContain: fullSecrets, MustContain: asBytes(mediaBytes, "V_VP9"),
		},
		{
			Name: "webm with tags",
			Data: slices.Concat(ebmlHeader(docWebM), indexedSegment(taggedInfo(), el(idTracks, videoTrack()), cluster(), tags())),

			WantFields: []string{"Title", "Tag ARTIST"}, MustNotContain: asBytes("Test Title", "Test Author"), MustContain: asBytes(mediaBytes),
		},
		{
			Name: "nothing to remove",
			Data: slices.Concat(ebmlHeader(docMatroska), indexedSegment(plainInfo(), el(idTracks, videoTrack()), cluster(), cues())),

			MustContain: asBytes(mediaBytes),
		},
		{
			Name: "live stream with unknown sizes",
			Data: slices.Concat(ebmlHeader(docWebM), unknownEl(idSegment,
				taggedInfo(), el(idTracks, videoTrack()),
				unknownEl(idCluster, num(idTimestamp, 0), simpleBlock()),
				tags(),
				unknownEl(idCluster, num(idTimestamp, 1), simpleBlock()),
				cues())),

			WantFields: []string{"Title", "Tag ARTIST"}, MustNotContain: asBytes("Test Title", "Test Author"), MustContain: asBytes(mediaBytes),
		},
	})
}

func clean(t *testing.T, data []byte) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func wantError(t *testing.T, data []byte, want error) {
	t.Helper()
	if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, want) {
		t.Errorf("Inspect: got %v, want %v", err, want)
	}
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, want) {
		t.Errorf("Clean: got %v, want %v", err, want)
	}
}

// topChildren lists a known-size Segment's children, failing on anything that does
// not parse, so it also proves the output's structure is intact.
func topChildren(t *testing.T, data []byte) (seg element, children []element) {
	t.Helper()
	r := &reader{ctx: context.Background(), src: core.BytesSource(data)}
	header, _, err := readDocHeader(r)
	if err != nil {
		t.Fatalf("EBML header: %v", err)
	}
	seg, err = r.header(header.end(), int64(len(data)))
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	for e, err := range r.children(seg) {
		if err != nil {
			t.Fatalf("Segment child: %v", err)
		}
		children = append(children, e)
	}
	return seg, children
}

func TestVoidHeaderAtEveryWidthBoundary(t *testing.T) {
	totals := []int64{2, 3, 127, 128, 129, 130, 131, 16384, 16385, 16386, 16387, 16388}
	for width := 3; width <= 7; width++ {
		maxPayload := int64(1)<<(7*width) - 2
		last := 1 + int64(width) + maxPayload
		totals = append(totals, last-1, last, last+1, last+2)
	}
	longest := 1 + int64(8) + int64(1)<<56 - 2
	totals = append(totals, longest-1, longest)
	if _, err := voidHeader(longest + 1); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("voidHeader(%d) gave %v", longest+1, err)
	}
	for _, total := range totals {
		header, err := voidHeader(total)
		if err != nil {
			t.Errorf("voidHeader(%d): %v", total, err)
			continue
		}
		if header[0] != idVoid {
			t.Errorf("voidHeader(%d) starts with %#x", total, header[0])
		}
		size, width, unknown, err := parseSize(header[1:])
		if err != nil || unknown || width != len(header)-1 {
			t.Errorf("voidHeader(%d) = %x: size %d, width %d, unknown %v, err %v", total, header, size, width, unknown, err)
			continue
		}
		if got := 1 + int64(width) + size; got != total {
			t.Errorf("voidHeader(%d) = %x describes %d bytes", total, header, got)
		}
	}
	if header, err := voidHeader(129); err != nil || !bytes.Equal(header, []byte{idVoid, 0x40, 126}) {
		t.Errorf("voidHeader(129) = %x, %v: 129 bytes need a two-byte size holding 126", header, err)
	}
	if header, err := voidHeader(128); err != nil || !bytes.Equal(header, []byte{idVoid, 0x80 | 126}) {
		t.Errorf("voidHeader(128) = %x, %v", header, err)
	}
	for _, total := range []int64{-1, 0, 1} {
		if _, err := voidHeader(total); !errors.Is(err, core.ErrMalformed) {
			t.Errorf("voidHeader(%d) gave %v", total, err)
		}
	}
	if _, err := voidHeader(1 << 62); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("voidHeader(1<<62) gave %v", err)
	}
}

// TestVoidedRegionKeepsItsLength removes an element of each awkward total length
// from a real file and checks that what follows does not move and that the Void
// covers the region exactly.
func TestVoidedRegionKeepsItsLength(t *testing.T) {
	for _, total := range []int{2, 3, 4, 127, 128, 129, 130, 131, 16384, 16385, 16386, 16387, 16388, 2097154, 2097155, 2097156} {
		header, err := voidHeader(int64(total))
		if err != nil {
			t.Fatal(err)
		}
		vendor := bytes.Repeat([]byte{'V'}, total)
		copy(vendor, header)
		vendor[0] = idVendor

		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, plainInfo(), el(idTracks, videoTrack()), vendor, cluster()))
		out, _ := clean(t, data)
		if len(out) != len(data) {
			t.Fatalf("total %d: length changed from %d to %d", total, len(data), len(out))
		}
		_, children := topChildren(t, out)
		if len(children) != 4 {
			t.Fatalf("total %d: the output has %d Segment children, want 4", total, len(children))
		}
		void, next := children[2], children[3]
		if void.id != idVoid || void.end()-void.off != int64(total) {
			t.Errorf("total %d: got element %#x of %d bytes", total, void.id, void.end()-void.off)
		}
		if bytes.IndexByte(out[void.dataOff:void.end()], 'V') >= 0 {
			t.Errorf("total %d: the Void payload was not zeroed", total)
		}
		if next.id != idCluster || next.off != void.end() {
			t.Errorf("total %d: the next element is %#x at %d, want the cluster at %d", total, next.id, next.off, void.end())
		}
	}
}

// TestOutputPreservesEverythingElse is the content-preservation test: the standard
// library cannot decode video, but the handler should not change a single byte
// outside the regions it reports having changed.
func TestOutputPreservesEverythingElse(t *testing.T) {
	data := fullFile()
	out, _ := clean(t, data)
	trailing := len("Test Trailing Secret")
	if len(out) != len(data)-trailing {
		t.Fatalf("length went from %d to %d, want only the trailing bytes gone", len(data), len(out))
	}
	seg, children := topChildren(t, out)
	if int(seg.end()) != len(out) {
		t.Errorf("the Segment ends at %d in a file of %d bytes", seg.end(), len(out))
	}
	_, original := topChildren(t, data)
	if len(original) != len(children) {
		t.Fatalf("Segment children went from %d to %d", len(original), len(children))
	}
	for i, before := range original {
		after := children[i]
		if before.off != after.off || before.end() != after.end() {
			t.Errorf("child %d moved: %d..%d became %d..%d", i, before.off, before.end(), after.off, after.end())
		}
		switch before.id {
		case idCluster, idCues:
			if !bytes.Equal(data[before.off:before.end()], out[after.off:after.end()]) {
				t.Errorf("element %#x changed", before.id)
			}
		case idTags, idAttachments, idChapters, idVendor:
			if after.id != idVoid {
				t.Errorf("element %#x became %#x, want a Void", before.id, after.id)
			}
		}
	}
}

func TestMandatoryStringsAreBlankedNotVoided(t *testing.T) {
	data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, taggedInfo()))
	out, _ := clean(t, data)
	_, children := topChildren(t, out)
	r := &reader{ctx: context.Background(), src: core.BytesSource(out)}
	var saw []uint32
	for e, err := range r.children(children[0]) {
		if err != nil {
			t.Fatal(err)
		}
		saw = append(saw, e.id)
		if e.id == idMuxingApp || e.id == idWritingApp {
			payload := out[e.dataOff:e.end()]
			if len(payload) == 0 || len(bytes.Trim(payload, "\x00")) != 0 {
				t.Errorf("element %#x holds %q", e.id, payload)
			}
		}
	}
	for _, id := range []uint32{idTimestampSc, idMuxingApp, idWritingApp} {
		if !slices.Contains(saw, id) {
			t.Errorf("element %#x is gone", id)
		}
	}
	if slices.Contains(saw, uint32(idTitle)) {
		t.Errorf("Title survived")
	}
}

func TestSeekEntries(t *testing.T) {
	data := fullFile()
	out, _ := clean(t, data)
	seg, children := topChildren(t, out)
	r := &reader{ctx: context.Background(), src: core.BytesSource(out)}
	if children[0].id != idSeekHead {
		t.Fatalf("first child is %#x", children[0].id)
	}
	var kept []uint32
	for e, err := range r.children(children[0]) {
		if err != nil {
			t.Fatal(err)
		}
		if e.id != idSeek {
			continue
		}
		var target uint32
		var pos uint64
		for child, err := range r.children(e) {
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.readUint(child)
			if err != nil {
				t.Fatal(err)
			}
			if child.id == idSeekID {
				target = uint32(v)
			} else {
				pos = v
			}
		}
		kept = append(kept, target)
		found, err := r.header(seg.dataOff+int64(pos), int64(len(out)))
		if err != nil || found.id != target {
			t.Errorf("the entry for %#x leads to %#x, %v", target, found.id, err)
		}
	}
	// fullFile's SeekHead also lists its Void, Attachments, Chapters, Tags and
	// vendor element, none of which is a target worth keeping.
	want := []uint32{idInfo, idTracks, idCluster, idCues}
	if !slices.Equal(kept, want) {
		t.Errorf("kept Seek entries for %#x, want %#x", kept, want)
	}
}

func TestStaleSeekEntriesGo(t *testing.T) {
	info := plainInfo()
	offset := func(n int) uint64 { return uint64(n) }
	cases := map[string][]byte{
		"wrong position":     seekEntry(idInfo, offset(9999)),
		"wrong element":      seekEntry(idTracks, 0),
		"huge position":      seekEntry(idInfo, 1<<63),
		"no position":        el(idSeek, el(idSeekID, idBytes(idInfo))),
		"no ID":              el(idSeek, num(idSeekPosition, 0)),
		"oversized ID":       el(idSeek, el(idSeekID, []byte{1, 2, 3, 4, 5}), num(idSeekPosition, 0)),
		"oversized position": el(idSeek, el(idSeekID, idBytes(idInfo)), el(idSeekPosition, make([]byte, 9))),
		"two IDs":            el(idSeek, el(idSeekID, idBytes(idInfo)), el(idSeekID, idBytes(idInfo)), num(idSeekPosition, 0)),
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, info, el(idSeekHead, entry)))
			out, _ := clean(t, data)
			_, children := topChildren(t, out)
			head := children[1]
			r := &reader{ctx: context.Background(), src: core.BytesSource(out)}
			for e, err := range r.children(head) {
				if err != nil {
					t.Fatal(err)
				}
				if e.id != idVoid {
					t.Errorf("the SeekHead still holds element %#x", e.id)
				}
			}
		})
	}
}

// A SeekHead may follow what it points at, and may point at an element after it.
func TestSeekHeadAfterIndexedElements(t *testing.T) {
	info := plainInfo()
	head := el(idSeekHead, seekEntry(idInfo, 0), seekEntry(idTags, uint64(len(info))+uint64(len(el(idSeekHead, seekEntry(idInfo, 0), seekEntry(idTags, 0))))))
	data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, info, head, tags()))
	out, _ := clean(t, data)
	_, children := topChildren(t, out)
	r := &reader{ctx: context.Background(), src: core.BytesSource(out)}
	var ids []uint32
	for e, err := range r.children(children[1]) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.id)
	}
	if !slices.Equal(ids, []uint32{idSeek, idVoid}) {
		t.Errorf("SeekHead children are %#x, want one Seek and one Void", ids)
	}
}

func TestCRC(t *testing.T) {
	crc := el(idCRC32, []byte{1, 2, 3, 4})
	t.Run("in a modified master it goes", func(t *testing.T) {
		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment,
			el(idInfo, crc, text(idTitle, "Test Title")),
			el(idTracks, crc, videoTrack(text(idTrackName, "Test Track Name")))))
		out, _ := clean(t, data)
		if bytes.Contains(out, crc) {
			t.Error("a CRC-32 over changed bytes survived")
		}
	})
	t.Run("in an untouched master it stays", func(t *testing.T) {
		covered := num(idTimestampSc, 1000000)
		valid := el(idCRC32, binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(covered)))
		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idInfo, valid, covered)))
		out, _ := clean(t, data)
		if !bytes.Equal(out, data) {
			t.Error("a file with nothing to remove changed")
		}
	})
	t.Run("at segment level it goes when any child changes", func(t *testing.T) {
		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, crc, plainInfo(), tags()))
		out, _ := clean(t, data)
		if bytes.Contains(out, crc) {
			t.Error("the Segment's CRC-32 survived")
		}
	})
	t.Run("in the EBML header", func(t *testing.T) {
		header := el(idEBML, crc, text(idDocType, docMatroska), text(idVendor, "Test Vendor Data"))
		out, _ := clean(t, slices.Concat(header, el(idSegment, plainInfo())))
		if bytes.Contains(out, crc) || bytes.Contains(out, []byte("Test Vendor Data")) {
			t.Error("the header kept a stale CRC-32 or an unknown child")
		}
		if !(handler{}).Match(out[:min(len(out), core.HeadSize)], core.BytesSource(out)) {
			t.Error("the output no longer matches")
		}
	})
	t.Run("a second CRC or one of the wrong size is not kept", func(t *testing.T) {
		for _, data := range [][]byte{
			slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idInfo, crc, crc))),
			slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idInfo, el(idCRC32, []byte("Test Long CRC"))))),
		} {
			out, _ := clean(t, data)
			if bytes.Contains(out, crc) || bytes.Contains(out, []byte("Test Long CRC")) {
				t.Error("an unexplainable CRC-32 survived")
			}
		}
	})
}

func TestExistingVoid(t *testing.T) {
	zeroed := el(idVoid, make([]byte, 10))
	data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, zeroed, plainInfo()))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Error("a zeroed Void changed")
	}
	if len(rep.Findings) != 0 {
		t.Errorf("a zeroed Void gave findings: %+v", rep.Findings)
	}
	// Also legal between the header and the Segment.
	data = slices.Concat(ebmlHeader(docMatroska), el(idVoid, []byte("Test Stale Data")), el(idSegment, plainInfo()))
	out, rep = clean(t, data)
	if bytes.Contains(out, []byte("Test Stale Data")) || len(rep.Findings) != 1 {
		t.Errorf("a Void before the Segment: %+v", rep.Findings)
	}
}

func TestEncrypted(t *testing.T) {
	const (
		idContentEncoding     = 0x6240
		idContentEncodingType = 0x5033
		idContentEncryption   = 0x5035
		idContentCompression  = 0x5034
		idContentCompAlgo     = 0x4254
	)
	file := func(encoding []byte) []byte {
		return slices.Concat(ebmlHeader(docMatroska), el(idSegment,
			el(idTracks, videoTrack(el(idContentEncodings, el(idContentEncoding, encoding))))))
	}
	wantError(t, file(num(idContentEncodingType, 1)), core.ErrEncrypted)
	wantError(t, file(el(idContentEncryption)), core.ErrEncrypted)

	compressed := file(slices.Concat(num(idContentEncodingType, 0), el(idContentCompression, num(idContentCompAlgo, 3))))
	if out, _ := clean(t, compressed); !bytes.Equal(out, compressed) {
		t.Error("a compressed track changed")
	}
}

func TestTrackTypes(t *testing.T) {
	track := func(kind uint64) []byte {
		return slices.Concat(ebmlHeader(docMatroska), el(idSegment,
			el(idTracks, el(idTrackEntry, num(idTrackNumber, 3), num(idTrackType, kind)))))
	}
	for _, kind := range []uint64{0, 3, 0x10, 0x12, 0x20, 0x21} {
		wantError(t, track(kind), core.ErrUnsupported)
	}
	if _, rep := clean(t, track(trackAudio)); len(rep.Warnings) != 0 {
		t.Errorf("an audio track gave warnings: %q", rep.Warnings)
	}
	if _, rep := clean(t, track(trackSubtitle)); len(rep.Warnings) != 1 {
		t.Errorf("a subtitle track gave warnings %q, want one", rep.Warnings)
	}
	noType := slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTracks, el(idTrackEntry, num(idTrackNumber, 3)))))
	wantError(t, noType, core.ErrUnsupported)
	twoTypes := slices.Concat(ebmlHeader(docMatroska), el(idSegment,
		el(idTracks, el(idTrackEntry, num(idTrackType, 1), num(idTrackType, 0x21)))))
	wantError(t, twoTypes, core.ErrMalformed)
}

func TestUnknownSizes(t *testing.T) {
	blocks := func(n uint64) []byte {
		return slices.Concat(num(idTimestamp, n), simpleBlock())
	}
	t.Run("a cluster ends at the next top-level element", func(t *testing.T) {
		data := slices.Concat(ebmlHeader(docWebM), unknownEl(idSegment, el(idTracks, videoTrack()),
			unknownEl(idCluster, blocks(0)), text(idVendor, "Test Vendor Data"),
			unknownEl(idCluster, blocks(1)), tags(), unknownEl(idCluster, blocks(2))))
		out, rep := clean(t, data)
		if len(out) != len(data) {
			t.Errorf("length changed from %d to %d", len(data), len(out))
		}
		if bytes.Contains(out, []byte("Test Vendor Data")) || bytes.Contains(out, []byte("Test Author")) {
			t.Error("an element between clusters survived")
		}
		if n := bytes.Count(out, []byte(mediaBytes)); n != 3 {
			t.Errorf("%d of 3 blocks survived", n)
		}
		if len(rep.Findings) == 0 {
			t.Error("nothing was reported")
		}
	})
	t.Run("a Void in a cluster is scrubbed", func(t *testing.T) {
		data := slices.Concat(ebmlHeader(docWebM), unknownEl(idSegment, el(idTracks, videoTrack()),
			unknownEl(idCluster, blocks(0), el(idVoid, []byte("Test Stale Data")), blocks(1))))
		out, _ := clean(t, data)
		if bytes.Contains(out, []byte("Test Stale Data")) {
			t.Error("the Void survived")
		}
		if n := bytes.Count(out, []byte(mediaBytes)); n != 2 {
			t.Errorf("%d of 2 blocks survived", n)
		}
	})
	t.Run("a cluster CRC goes when its Void changed", func(t *testing.T) {
		crc := el(idCRC32, []byte{1, 2, 3, 4})
		data := slices.Concat(ebmlHeader(docWebM), unknownEl(idSegment, el(idTracks, videoTrack()),
			unknownEl(idCluster, crc, blocks(0), el(idVoid, []byte("Test Stale Data")))))
		out, _ := clean(t, data)
		if bytes.Contains(out, crc) {
			t.Error("a stale CRC-32 survived")
		}
	})
	t.Run("an unknown-size cluster in a known-size segment stops at its end", func(t *testing.T) {
		data := slices.Concat(ebmlHeader(docWebM), el(idSegment, el(idTracks, videoTrack()), unknownEl(idCluster, blocks(0))), []byte("Test Trailing Secret"))
		out, _ := clean(t, data)
		if bytes.Contains(out, []byte("Test Trailing Secret")) {
			t.Error("the trailing data survived")
		}
	})
	for name, data := range map[string][]byte{
		"Info":          unknownEl(idSegment, unknownEl(idInfo)),
		"Tags":          unknownEl(idSegment, unknownEl(idTags)),
		"cluster child": unknownEl(idSegment, unknownEl(idCluster, unknownEl(0xA0))),
		"Track":         unknownEl(idSegment, el(idTracks, unknownEl(idTrackEntry))),
	} {
		t.Run("an unknown-size "+name+" is refused", func(t *testing.T) {
			wantError(t, slices.Concat(ebmlHeader(docWebM), data), core.ErrMalformed)
		})
	}
}

func TestChainedDocumentsAreRefused(t *testing.T) {
	doc := slices.Concat(ebmlHeader(docMatroska), el(idSegment, plainInfo()))
	wantError(t, slices.Concat(doc, doc), core.ErrUnsupported)
	live := slices.Concat(ebmlHeader(docMatroska), unknownEl(idSegment, plainInfo()))
	wantError(t, slices.Concat(live, doc), core.ErrUnsupported)
}

func TestMalformed(t *testing.T) {
	header := ebmlHeader(docMatroska)
	for name, data := range map[string][]byte{
		"no segment":           header,
		"junk before segment":  slices.Concat(header, text(idVendor, "x"), el(idSegment)),
		"segment overruns":     slices.Concat(header, idBytes(idSegment), sizeBytes(100), plainInfo()),
		"child overruns":       slices.Concat(header, el(idSegment, idBytes(idInfo), sizeBytes(100))),
		"zero byte as an ID":   slices.Concat(header, el(idSegment, []byte{0, 0, 0})),
		"wide ID":              slices.Concat(header, el(idSegment, []byte{0x08, 1, 2, 3, 4, 0x80})),
		"reserved ID":          slices.Concat(header, el(idSegment, []byte{0xFF, 0x80})),
		"oversized scalar":     slices.Concat(header, el(idSegment, el(idInfo, el(idTimestampSc, make([]byte, 9))))),
		"oversized codec name": slices.Concat(header, el(idSegment, el(idTracks, videoTrack(el(idCodecID, make([]byte, 65)))))),
		"truncated header":     slices.Concat(header, idBytes(idSegment)),
		"child header cut":     slices.Concat(header, el(idSegment, []byte{0x15, 0x49})),
	} {
		t.Run(name, func(t *testing.T) { wantError(t, data, core.ErrMalformed) })
	}
}

func TestDocumentChecks(t *testing.T) {
	segment := el(idSegment, plainInfo())
	for name, header := range map[string][]byte{
		"newer EBML":     el(idEBML, text(idDocType, docMatroska), num(idEBMLReadVersion, 2)),
		"newer Matroska": el(idEBML, text(idDocType, docMatroska), num(idDocTypeReadVersion, 5)),
		"wide IDs":       el(idEBML, text(idDocType, docMatroska), num(idEBMLMaxIDLength, 5)),
		"wide sizes":     el(idEBML, text(idDocType, docMatroska), num(idEBMLMaxSizeLength, 9)),
		"other doc type": el(idEBML, text(idDocType, "other")),
		"no doc type":    el(idEBML),
	} {
		t.Run(name, func(t *testing.T) { wantError(t, slices.Concat(header, segment), core.ErrUnsupported) })
	}
}

func TestMatch(t *testing.T) {
	match := func(data []byte) bool {
		return handler{}.Match(data[:min(len(data), core.HeadSize)], core.BytesSource(data))
	}
	segment := el(idSegment, plainInfo())
	for _, docType := range []string{docMatroska, docWebM, docWebM + "\x00\x00"} {
		if !match(slices.Concat(ebmlHeader(docType), segment)) {
			t.Errorf("doc type %q does not match", docType)
		}
	}
	for name, data := range map[string][]byte{
		"empty":          nil,
		"other magic":    []byte("RIFF....AVI "),
		"other doc type": slices.Concat(ebmlHeader("mka-not"), segment),
		"magic only":     ebmlMagic,
		"two doc types":  el(idEBML, text(idDocType, docWebM), text(idDocType, "other")),
		"long doc type":  el(idEBML, text(idDocType, string(make([]byte, maxShortStringSize+1)))),
		"unknown size":   slices.Concat(idBytes(idEBML), []byte{0xFF}, text(idDocType, docWebM)),
	} {
		if match(data) {
			t.Errorf("%s matches", name)
		}
	}
	// A header padded past HeadSize still belongs to this handler.
	padded := slices.Concat(el(idEBML, el(idVoid, make([]byte, core.HeadSize)), text(idDocType, docWebM)), segment)
	if !match(padded) {
		t.Error("a header longer than HeadSize does not match")
	}
}

func TestReportFormat(t *testing.T) {
	data := slices.Concat(ebmlHeader(docWebM), el(idSegment, plainInfo()))
	rep, err := handler{}.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Format != docWebM {
		t.Errorf("Format is %q", rep.Format)
	}
}

func TestDecodedValues(t *testing.T) {
	rep, err := handler{}.Inspect(context.Background(), core.BytesSource(fullFile()), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Title":              "Test Title",
		"Muxing application": "Test Muxer 1.0",
		"Tag ARTIST":         "Test Author",
		"Attachment":         "secret-attachment.txt (text/plain, 20 bytes)",
		"Chapter title":      "Test Chapter",
		"Segment UID":        hex.EncodeToString([]byte("TESTSEGMENTUID16")),
		"Creation date":      "2001-01-02T00:00:00Z",
	}
	for field, value := range want {
		i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == field })
		if i < 0 {
			t.Errorf("no finding for %q", field)
		} else if rep.Findings[i].Value != value {
			t.Errorf("%s = %q, want %q", field, rep.Findings[i].Value, value)
		}
	}
	location := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "Tag RECORDING_LOCATION" })
	if location < 0 || rep.Findings[location].Category != core.CatLocation || rep.Findings[location].Risk != core.RiskHigh {
		t.Errorf("the location tag is not rated as a location: %+v", rep.Findings)
	}
}

func TestDamagedRemovedElementsStillGo(t *testing.T) {
	broken := []byte{0xFF, 0xFF, 0xFF}
	for name, element := range map[string][]byte{
		"tags":        el(idTags, text(idTitle, "Test Title"), broken),
		"attachments": el(idAttachments, broken),
		"chapters":    el(idChapters, broken),
	} {
		t.Run(name, func(t *testing.T) {
			data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, element))
			out, rep := clean(t, data)
			if bytes.Contains(out, broken) || bytes.Contains(out, []byte("Test Title")) {
				t.Error("damaged contents survived")
			}
			if len(rep.Findings) == 0 {
				t.Error("nothing was reported")
			}
		})
	}
}

// Hostile nesting must neither overflow the stack nor be mistaken for clean data.
func TestDeepNesting(t *testing.T) {
	nested := func(id uint32) []byte {
		payload := text(idTagName, "DEEP")
		for range 20000 {
			payload = el(id, payload)
		}
		return payload
	}
	for name, data := range map[string][]byte{
		"tags":     el(idTags, el(idTag, nested(idSimpleTag))),
		"chapters": el(idChapters, nested(idChapterAtom)),
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := clean(t, slices.Concat(ebmlHeader(docMatroska), el(idSegment, data)))
			if bytes.Contains(out, []byte("DEEP")) {
				t.Error("a nested value survived")
			}
		})
	}
}

func TestLimits(t *testing.T) {
	t.Run("the report stops growing", func(t *testing.T) {
		tag := el(idSimpleTag, text(idTagName, "NAME"), text(idTagString, "Test"))
		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTags, el(idTag, bytes.Repeat(tag, maxFindings*3)))))
		_, rep := clean(t, data)
		if len(rep.Findings) != maxFindings+1 {
			t.Errorf("%d findings", len(rep.Findings))
		}
		if last := rep.Findings[len(rep.Findings)-1]; last.Field != "Further metadata" {
			t.Errorf("the last finding is %+v", last)
		}
	})
	t.Run("too many rewrites", func(t *testing.T) {
		vendor := []byte{idVendor, 0x80}
		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, bytes.Repeat(vendor, maxPatches+1)))
		wantError(t, data, core.ErrTooLarge)
	})
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != docMatroska || f.Family != core.FamilyVideo || !slices.Contains(f.Extensions, ".webm") || len(f.Limitations) == 0 || f.ResidueWarnOnly {
		t.Errorf("Format is %+v", f)
	}
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, fullFile())
}

func TestRobustnessLive(t *testing.T) {
	coretest.Robustness(t, handler{}, slices.Concat(ebmlHeader(docWebM), unknownEl(idSegment,
		taggedInfo(), el(idTracks, videoTrack(text(idTrackName, "Test Track Name"))),
		unknownEl(idCluster, num(idTimestamp, 0), simpleBlock()),
		tags(), unknownEl(idCluster, num(idTimestamp, 1)), cues())))
}

func FuzzClean(f *testing.F) {
	f.Add(fullFile())
	f.Add(slices.Concat(ebmlHeader(docWebM), unknownEl(idSegment,
		taggedInfo(), unknownEl(idCluster, num(idTimestamp, 0), el(idVoid, []byte("stale"))), tags())))
	f.Add(slices.Concat(ebmlHeader(docWebM), indexedSegment(el(idCRC32, []byte{1, 2, 3, 4}), plainInfo(), el(idTracks, videoTrack()))))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}
