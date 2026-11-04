package isobmff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// hiddenSecret is planted wherever a file might hide something from a parser.
const hiddenSecret = "Test Hidden Secret"

func secretUserData() []byte {
	return bx("udta", bx("\xa9nam", qtText(hiddenSecret)))
}

func secretTagList() []byte {
	return bx("ilst", ilstItem("\xa9nam", 1, []byte(hiddenSecret)))
}

// TestSampleEntryCannotHideMetadata covers boxes that a real reader finds inside a
// sample entry. ffmpeg parses a sample entry's children with the same table as the
// rest of the file, so it reports an ilst or a Nero chapter list found there, or a
// udta inside a wave box or any other container it knows, as the file's metadata.
func TestSampleEntryCannotHideMetadata(t *testing.T) {
	children := map[string][]byte{
		"ilst": secretTagList(),
		"freeform tag": bx("----",
			fullBox("mean", 0, 0, []byte("com.example")),
			fullBox("name", 0, 0, []byte("tag")),
			bx("data", be32(1), be32(0), []byte(hiddenSecret))),
		"Nero chapters":  fullBox("chpl", 1, 0, be32(0), []byte{1}, be64(0), []byte{byte(len(hiddenSecret))}, []byte(hiddenSecret)),
		"keys":           bx("keys", []byte(hiddenSecret)),
		"XMP":            bx("XMP_", []byte(hiddenSecret)),
		"Windows tags":   bx("Xtra", []byte(hiddenSecret)),
		"3GPP location":  bx("loci", []byte(hiddenSecret)),
		"QuickTime text": bx("\xa9xyz", qtText(hiddenSecret)),
	}
	for _, container := range []string{"moov", "trak", "mdia", "minf", "stbl", "dinf", "edts", "tref", "mvex", "moof", "traf", "stsd", "schi", "mfra"} {
		children[container+" container"] = bx(container, secretUserData())
	}
	var cases []coretest.Case
	for name, child := range children {
		visual := plainMovie()
		visual.tracks[0].entry = visualEntry("avc1", "", bx("avcC", []byte{1, 2, 3, 4}), child)
		cases = append(cases, coretest.Case{
			Name:           "video entry holding " + name,
			Data:           visual.build(),
			MustNotContain: asBytes(hiddenSecret),
			MustContain:    append(mediaBytes(), []byte("avcC")),
		})
		wave := plainMovie()
		wave.tracks[1].entry = audioEntry("mp4a", 1, make([]byte, 16),
			bx("wave", bx("frma", []byte("mp4a")), bx("esds", []byte{0, 0, 0, 0, 3, 0}), child, be32(0)))
		cases = append(cases, coretest.Case{
			Name:           "audio wave box holding " + name,
			Data:           wave.build(),
			MustNotContain: asBytes(hiddenSecret),
			MustContain:    append(mediaBytes(), []byte("frma"), []byte("esds")),
		})
	}
	nested := plainMovie()
	nested.tracks[1].entry = audioEntry("mp4a", 0, nil, bx("wave", bx("wave", secretUserData())))
	cases = append(cases, coretest.Case{
		Name:           "wave inside a wave",
		Data:           nested.build(),
		MustNotContain: asBytes(hiddenSecret),
		MustContain:    mediaBytes(),
	})
	coretest.Contract(t, mp4Handler, cases)
}

// TestUnknownSampleEntryBoxWarns covers what must stay in case a decoder needs it:
// the user has to hear about it.
func TestUnknownSampleEntryBoxWarns(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].entry = visualEntry("avc1", "", bx("avcC", []byte{1, 2, 3, 4}), bx("zzzz", []byte("A Future Codec Config")))
	out, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !bytes.Contains(out, []byte("A Future Codec Config")) {
		t.Error("an unknown box a decoder may need was removed")
	}
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "does not recognise") {
		t.Errorf("no warning about the box that stayed: %q", rep.Warnings)
	}
	var named bool
	for _, f := range rep.Findings {
		named = named || (f.Action == core.ActionRetain && f.Reason != "" && strings.Contains(f.Value, "zzzz"))
	}
	if !named {
		t.Errorf("inspect does not say which box stayed: %s", describeFindings(rep))
	}
}

func TestKnownSampleEntryBoxesDoNotWarn(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].entry = visualEntry("hvc1", "",
		bx("hvcC", make([]byte, 32)), bx("pasp", make([]byte, 8)), bx("btrt", make([]byte, 12)), coloursNCLX(),
		bx("clli", make([]byte, 4)), bx("mdcv", make([]byte, 24)), bx("st3d", make([]byte, 16)), bx("vexu", make([]byte, 32)))
	spec.tracks[1].entry = audioEntry("mp4a", 1, make([]byte, 16),
		bx("wave", bx("frma", []byte("mp4a")), bx("mp4a", make([]byte, 4)), bx("esds", make([]byte, 32)), bx("chan", make([]byte, 16)), make([]byte, 8)))
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if len(rep.Warnings) != 0 {
		t.Errorf("ordinary decoder configuration raised warnings: %q", rep.Warnings)
	}
}

// TestAppleWaveTerminatorDoesNotWarn covers the end of a wave box as an iPhone writes it:
// a real eight-byte box of type zero with nothing inside. Before this, every iPhone video
// warned about an unrecognised box.
func TestAppleWaveTerminatorDoesNotWarn(t *testing.T) {
	spec := plainMovie()
	spec.tracks[1].entry = audioEntry("mp4a", 1, make([]byte, 16),
		bx("wave", bx("frma", []byte("mp4a")), bx("mp4a", make([]byte, 4)), bx("esds", make([]byte, 32)), bx("\x00\x00\x00\x00")))
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if len(rep.Warnings) != 0 {
		t.Errorf("a wave box ending the way iPhones end it raised warnings: %q", rep.Warnings)
	}
}

// TestZeroTypeBoxesOtherThanTheTerminatorWarn checks the terminator's allowance stays
// narrow: it must be empty, and it only belongs inside a wave box.
func TestZeroTypeBoxesOtherThanTheTerminatorWarn(t *testing.T) {
	const zero = "\x00\x00\x00\x00"
	for name, entry := range map[string][]byte{
		"holding data": audioEntry("mp4a", 1, make([]byte, 16), bx("wave", bx("frma", []byte("mp4a")), bx(zero, []byte(hiddenSecret)))),
		"outside wave": audioEntry("mp4a", 1, make([]byte, 16), bx(zero)),
	} {
		t.Run(name, func(t *testing.T) {
			spec := plainMovie()
			spec.tracks[1].entry = entry
			_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
			if !strings.Contains(strings.Join(rep.Warnings, "\n"), "does not recognise") {
				t.Errorf("no warning: %q", rep.Warnings)
			}
		})
	}
}

// TestKeptContainersAreAllowlists covers boxes that stay but hold children. ffmpeg
// walks into edts and tref and reports a udta there as the track's metadata.
func TestKeptContainersAreAllowlists(t *testing.T) {
	secret := secretUserData()
	elst := fullBox("elst", 0, 0, be32(1), be32(100), be32(0), be32(0x10000))
	aperture := bx("tapt", fullBox("clef", 0, 0, be32(160<<16), be32(120<<16)), secret)
	trak := map[string][]byte{
		"edts":           bx("edts", elst, secret),
		"tapt":           aperture,
		"clip":           bx("clip", bx("crgn", make([]byte, 10)), secret),
		"matt":           bx("matt", bx("kmat", make([]byte, 10)), secret),
		"imap":           bx("imap", secret),
		"long load":      bx("load", make([]byte, 16), []byte(hiddenSecret)),
		"long txas":      bx("txas", []byte(hiddenSecret)),
		"long track grp": bx("trgr", bx("msrc", make([]byte, 8), []byte(hiddenSecret))),
		"elst past end":  bx("edts", fullBox("elst", 0, 0, be32(0), []byte(hiddenSecret))),
	}
	var cases []coretest.Case
	for name, box := range trak {
		spec := plainMovie()
		spec.tracks[0].trakExtra = box
		cases = append(cases, coretest.Case{
			Name:           "trak holding " + name,
			Data:           spec.build(),
			MustNotContain: asBytes(hiddenSecret),
			MustContain:    mediaBytes(),
		})
	}
	spec := plainMovie()
	spec.tracks[0].minfExtra = bx("gmhd", bx("gmin", make([]byte, 16)), secret)
	cases = append(cases, coretest.Case{
		Name:           "gmhd holding a udta",
		Data:           spec.build(),
		MustNotContain: asBytes(hiddenSecret),
		MustContain:    append(mediaBytes(), []byte("gmin")),
	})
	coretest.Contract(t, mp4Handler, cases)

	// The edit list itself still has to survive.
	keep := plainMovie()
	keep.tracks[0].trakExtra = bx("edts", elst, secret)
	out, _ := mustClean(t, mp4Handler, keep.build(), core.Options{})
	if !bytes.Contains(out, elst) {
		t.Error("the edit list was removed")
	}
}

// TestTrackReferencesAreTrackIDs covers a tref, whose children are nothing but
// lists of track IDs. Anything else in one is a place to hide data, and ffmpeg
// reads a udta there.
func TestTrackReferencesAreTrackIDs(t *testing.T) {
	cases := map[string][]byte{
		"udta":            secretUserData(),
		"text":            bx("cdsc", []byte(hiddenSecret)),
		"text as IDs":     bx("cdsc", []byte(hiddenSecret[:16]), []byte(hiddenSecret[:16])),
		"unknown trailer": bx("tmcd", be32(2), []byte(hiddenSecret)),
	}
	var contract []coretest.Case
	for name, ref := range cases {
		spec := plainMovie()
		spec.tracks[0].refs = cat(bx("chap", be32(2)), ref)
		contract = append(contract, coretest.Case{
			Name:           name,
			Data:           spec.build(),
			MustNotContain: asBytes(hiddenSecret),
			MustContain:    append(mediaBytes(), bx("chap", be32(2))),
		})
	}
	coretest.Contract(t, mp4Handler, contract)
}

// growBox returns data with extra appended to the payload of the box at path,
// taking the first box of each name, with every enclosing size corrected. The
// fixtures put the moov after the mdat, so the media does not move.
func growBox(t *testing.T, data []byte, path []string, extra []byte) []byte {
	t.Helper()
	start, end := 0, len(data)
	var sizes []int
	for _, name := range path {
		found := false
		for pos := start; pos+8 <= end; {
			size := int(binary.BigEndian.Uint32(data[pos:]))
			if string(data[pos+4:pos+8]) == name {
				sizes = append(sizes, pos)
				start, end = pos+8, pos+size
				found = true
				break
			}
			pos += size
		}
		if !found {
			t.Fatalf("no %q on the way to %v", name, path)
		}
	}
	out := cat(data[:end], extra, data[end:])
	for _, at := range sizes {
		binary.BigEndian.PutUint32(out[at:], binary.BigEndian.Uint32(out[at:])+uint32(len(extra)))
	}
	return out
}

// TestDataPastDeclaredEnd covers bytes after the part of a kept box its own fields
// account for. No reader looks there, but a raw search finds them.
func TestDataPastDeclaredEnd(t *testing.T) {
	secret := []byte(hiddenSecret)
	minf := []string{"moov", "trak", "mdia", "minf"}
	stbl := append(minf[:4:4], "stbl")
	paths := map[string][]string{
		"mvhd": {"moov", "mvhd"},
		"tkhd": {"moov", "trak", "tkhd"},
		"mdhd": {"moov", "trak", "mdia", "mdhd"},
		"nmhd": append(minf[:4:4], "nmhd"),
		"stts": append(stbl[:5:5], "stts"),
		"stsc": append(stbl[:5:5], "stsc"),
		"stsz": append(stbl[:5:5], "stsz"),
		"stco": append(stbl[:5:5], "stco"),
		"dref": append(minf[:4:4], "dinf", "dref"),
	}
	var cases []coretest.Case
	for name, path := range paths {
		extra := secret
		if name == "dref" {
			// A zero size ends the walk of the entries, which must not end the scrub.
			extra = cat(be32(0), secret)
		}
		cases = append(cases, coretest.Case{
			Name:           name,
			Data:           growBox(t, plainMovie().build(), path, extra),
			MustNotContain: [][]byte{secret},
			MustContain:    mediaBytes(),
		})
	}
	for name, top := range map[string][]byte{
		"sidx": fullBox("sidx", 0, 0, be32(1), be32(1000), be32(0), be32(0), be16(0), be16(0), secret),
		"prft": fullBox("prft", 0, 0, be32(1), be64(0), be32(0), secret),
		"pdin": fullBox("pdin", 0, 0, secret),
		"ssix": fullBox("ssix", 0, 0, be32(0), secret),
	} {
		spec := plainMovie()
		spec.before = top
		cases = append(cases, coretest.Case{
			Name:           name,
			Data:           spec.build(),
			MustNotContain: [][]byte{secret},
			MustContain:    mediaBytes(),
		})
	}
	for name, box := range map[string][]byte{"iods": bx("iods", secret), "ctab": bx("ctab", secret)} {
		spec := plainMovie()
		spec.moovExtra = box
		cases = append(cases, coretest.Case{
			Name:           name + " in moov",
			Data:           spec.build(),
			MustNotContain: [][]byte{secret},
			MustContain:    mediaBytes(),
		})
	}
	coretest.Contract(t, mp4Handler, cases)
}

// TestOrdinaryTablesAreUntouched checks that scrubbing what lies past a table does
// not disturb the table itself, in the fixtures' every form.
func TestOrdinaryTablesAreUntouched(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].co64 = true
	spec.tracks[1].stz2Bits = 16
	data := spec.build()
	out, rep := mustClean(t, mp4Handler, data, core.Options{})
	for _, table := range []string{"stts", "stsc", "stsd", "co64", "stz2"} {
		want := boxBytes(t, data, table)
		if !bytes.Contains(out, want[len(want)-min(len(want), 8):]) {
			t.Errorf("%s changed", table)
		}
	}
	if strings.Contains(describeFindings(rep), "past") {
		t.Errorf("an ordinary table was reported: %s", describeFindings(rep))
	}
}

func boxBytes(t *testing.T, data []byte, typ string) []byte {
	t.Helper()
	at := bytes.Index(data, []byte(typ))
	if at < 4 {
		t.Fatalf("no %q box", typ)
	}
	return data[at-4 : at-4+int(binary.BigEndian.Uint32(data[at-4:]))]
}

func TestLongBrandListRefused(t *testing.T) {
	spec := plainMovie()
	spec.ftyp = bx("ftyp", []byte("isom"), be32(0), bytes.Repeat([]byte("Test"), 100))
	data := spec.build()
	if mp4Handler.Match(data[:64], core.BytesSource(data)) {
		t.Error("a brand list of 400 bytes is somewhere to hide data, but the file was accepted")
	}
}

// palettedEntry is a QuickTime visual sample entry with an embedded colour table,
// which sits between the fixed fields and the child boxes.
func palettedEntry(children ...[]byte) []byte {
	fixed := make([]byte, visualFixed-8)
	binary.BigEndian.PutUint16(fixed[82-8:], 8) // depth, with colour table ID 0 after it
	palette := cat(be32(0), be16(0x8000), be16(1), []byte("PALETTE0"), []byte("PALETTE1"))
	return bx("rle ", append([][]byte{fixed, palette}, children...)...)
}

// TestEmbeddedPaletteSurvives is the zero in a palette's seed field being taken for
// the zero size that ends a list of boxes, which blanked the palette and turned the
// video black.
func TestEmbeddedPaletteSurvives(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].entry = palettedEntry(secretUserData())
	coretest.Contract(t, mp4Handler, []coretest.Case{{
		Name:           "paletted video",
		Data:           spec.build(),
		MustNotContain: asBytes(hiddenSecret),
		MustContain:    append(mediaBytes(), []byte("PALETTE0"), []byte("PALETTE1")),
	}})
}

// TestImageCodedFramesWarn covers data that stays by necessity: each frame is a whole
// image file that can carry EXIF of its own.
func TestImageCodedFramesWarn(t *testing.T) {
	for _, format := range []string{"jpeg", "mjpa", "png "} {
		spec := plainMovie()
		spec.tracks[0].entry = visualEntry(format, "")
		_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if !strings.Contains(strings.Join(rep.Warnings, "\n"), "frame") {
			t.Errorf("%s video: no warning about metadata in its frames: %q", format, rep.Warnings)
		}
	}

	// Motion JPEG in an MP4 file is filed under the generic mp4v.
	spec := plainMovie()
	spec.tracks[0].entry = visualEntry("mp4v", "")
	spec.tracks[0].samples = [][]byte{[]byte("\xFF\xD8\xFF\xE1 Test Frame With Exif")}
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "frame") {
		t.Errorf("JPEG frames in mp4v: no warning: %q", rep.Warnings)
	}

	photo := phonePhoto()
	photo.items[0].typ = "jpeg"
	_, rep = mustClean(t, heifHandler, photo.build(), core.Options{})
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "EXIF") {
		t.Errorf("JPEG item: no warning about metadata inside it: %q", rep.Warnings)
	}
}

func TestAuxiliaryVideoTrackReported(t *testing.T) {
	spec := plainMovie()
	spec.tracks = append(spec.tracks, testTrack{id: 3, handler: "auxv", entry: visualEntry("hvc1", ""), samples: [][]byte{[]byte("DEPTH SAMPLE")}})
	out, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !bytes.Contains(out, []byte("DEPTH SAMPLE")) {
		t.Error("the auxiliary track's samples were removed")
	}
	var found bool
	for _, f := range rep.Findings {
		found = found || (f.Field == "Auxiliary video track" && f.Action == core.ActionRetain && f.Reason != "")
	}
	if !found {
		t.Errorf("not reported: %s", describeFindings(rep))
	}
}

// TestOpenChunkExcessIsNotOffset checks the warning about unchecked bytes next to
// legacy audio chunks. One chunk claiming more than it holds must not hide another
// that holds more than it claims.
func TestOpenChunkExcessIsNotOffset(t *testing.T) {
	audio := testTrack{
		id: 1, handler: "soun", constSize: 8, perChunk: 2,
		samples: asBytes("PCMAPCMA", "PCMBPCMB", "HIDDEN!!", "HIDDEN!!"),
		entry:   audioEntry("sowt", 0, nil),
		// Chunk 1 claims 16 bytes at +0 but only has 8 before chunk 2 begins. Chunk 2
		// claims 16 at +8 and has 24. The totals agree; the second chunk does not.
		badOffset: func(i int, off uint64) uint64 {
			if i == 1 {
				return off - 8
			}
			return off
		},
	}
	spec := movieSpec{tracks: []testTrack{audio}, times: fakeTime}
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "8 bytes") {
		t.Errorf("want a warning about the 8 unchecked bytes, got %q", rep.Warnings)
	}
}

// countingSource counts the bytes read from a file.
type countingSource struct {
	core.Source
	read *int64
}

func (s countingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.Source.ReadAt(p, off)
	*s.read += int64(n)
	return n, err
}

var _ io.ReaderAt = countingSource{}

// manyItemsSharingBytes declares many Exif items at the same long extent, so that a
// small file asks for those bytes to be read and decoded once per item.
func manyItemsSharingBytes(items int, shared []byte) []byte {
	const primary = 1
	ftyp := bx("ftyp", []byte("heic"), be32(0), []byte("mif1"), []byte("heic"))
	meta := func(mdat uint32) []byte {
		infes := [][]byte{fullBox("infe", 2, 0, be16(primary), be16(0), []byte("hvc1"), []byte{0})}
		locations := [][]byte{be16(primary), be16(0), be16(1), be32(mdat + uint32(len(shared))), be32(uint32(len(primaryData)))}
		for i := range items {
			id := be16(uint16(i + 2))
			infes = append(infes, fullBox("infe", 2, 1, id, be16(0), []byte("Exif"), []byte{0}))
			locations = append(locations, id, be16(0), be16(1), be32(mdat), be32(uint32(len(shared))))
		}
		count := be16(uint16(items + 1))
		return fullBox("meta", 0, 0,
			fullBox("hdlr", 0, 0, be32(0), []byte("pict"), make([]byte, 12), []byte{0}),
			pitmBox(primary, false, nil),
			fullBox("iloc", 0, 0, be16(0x4400), count, cat(locations...)),
			fullBox("iinf", 0, 0, count, cat(infes...)),
			bx("iprp", bx("ipco", bx("hvcC", []byte{1})), fullBox("ipma", 0, 0, be32(1), be16(primary), []byte{1}, []byte{0x81})),
		)
	}
	start := uint32(len(ftyp) + len(meta(0)) + 8)
	return cat(ftyp, meta(start), bx("mdat", shared, primaryData))
}

// TestItemsCannotMultiplyReads is a small file asking for the same long extent to
// be read and decoded once per item, which took minutes before reads were budgeted.
func TestItemsCannotMultiplyReads(t *testing.T) {
	shared := bytes.Repeat([]byte{0xAB}, 1<<20)
	data := manyItemsSharingBytes(2000, shared)
	var read int64
	src := countingSource{core.BytesSource(data), &read}
	rep, err := heifHandler.Inspect(testContext(), src, core.Options{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// The 2 GiB this asks for, against a budget of a small multiple of the file.
	if limit := int64(256 << 20); read > limit {
		t.Errorf("read %d bytes from a file of %d, want at most %d", read, len(data), limit)
	}
	if !strings.Contains(describeFindings(rep), "Exif") {
		t.Errorf("items past the budget went unreported: %s", describeFindings(rep))
	}
	out, _ := mustClean(t, heifHandler, data, core.Options{})
	if bytes.Contains(out, shared[:64]) {
		t.Error("the items' bytes survived")
	}
}

// TestOrientationIsRetainedAndStrictRemovesIt is what docs/DESIGN.md promises: the
// rotation stays, is reported with a reason, and goes in strict mode.
func TestOrientationIsRetainedAndStrictRemovesIt(t *testing.T) {
	spec := phonePhoto()
	spec.properties = append(spec.properties, bx("imir", []byte{1}))
	spec.items[0].props = append(spec.items[0].props, 8)
	data := spec.build()

	out, rep := mustClean(t, heifHandler, data, core.Options{})
	if !bytes.Contains(out, []byte("irot")) || !bytes.Contains(out, []byte("imir")) {
		t.Error("the default mode removed the orientation")
	}
	retained := 0
	for _, f := range rep.Findings {
		if f.Field == "Orientation" && f.Action == core.ActionRetain && f.Reason != "" {
			retained++
		}
	}
	if retained != 2 {
		t.Errorf("want rotation and mirroring both reported as retained: %s", describeFindings(rep))
	}

	out, rep = mustClean(t, heifHandler, data, core.Options{Strict: true})
	if bytes.Contains(out, []byte("irot")) || bytes.Contains(out, []byte("imir")) {
		t.Error("strict mode kept the orientation")
	}
	for _, f := range rep.Findings {
		if f.Field == "Orientation" && f.Action == core.ActionRetain {
			t.Errorf("strict mode reports orientation as kept: %s", describeFindings(rep))
		}
	}
	again, _ := mustClean(t, heifHandler, out, core.Options{Strict: true})
	if !bytes.Equal(out, again) {
		t.Error("strict mode is not idempotent")
	}
}

// TestHEIFKeptBoxesHoldOnlyTheirFields covers the same hiding places in a meta box.
func TestHEIFKeptBoxesHoldOnlyTheirFields(t *testing.T) {
	secret := []byte(hiddenSecret)
	mutate := func(fn func(*heifSpec)) []byte {
		s := phonePhoto()
		fn(&s)
		return s.build()
	}
	group := func(typ string, ids []uint32, tail []byte) []byte {
		body := cat(be32(1), be32(uint32(len(ids))))
		for _, id := range ids {
			body = cat(body, be32(id))
		}
		return fullBox(typ, 0, 0, body, tail)
	}
	mustGo := [][]byte{secret[:16]}
	cases := []coretest.Case{
		{Name: "pitm", Data: mutate(func(s *heifSpec) { s.pitmTail = secret })},
		{Name: "ispe", Data: mutate(func(s *heifSpec) { s.properties[1] = fullBox("ispe", 0, 0, be32(4032), be32(3024), secret) })},
		{Name: "irot", Data: mutate(func(s *heifSpec) { s.properties[2] = bx("irot", []byte{1}, secret) })},
		{Name: "pixi", Data: mutate(func(s *heifSpec) { s.properties[1] = fullBox("pixi", 0, 0, []byte{3, 8, 8, 8}, secret) })},
		{Name: "nclx", Data: mutate(func(s *heifSpec) {
			s.properties[3] = bx("colr", []byte("nclx"), []byte{0, 1, 0, 13, 0, 1, 0x80}, secret)
		})},
		{Name: "unknown group", Data: mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", bx("zzzz", secret)) })},
		{Name: "group past its end", Data: mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", group("altr", []uint32{1, 2}, secret)) })},
		{Name: "group of text", Data: mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", fullBox("altr", 0, 0, be32(1), be32(4), secret[:16])) })},
		{Name: "group naming a removed item", Data: mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", group("altr", []uint32{2, 1, 5}, nil)) })},
		{Name: "udta in the group list", Data: mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", secretUserData()) })},
	}
	for i := range cases {
		cases[i].MustNotContain = mustGo
		cases[i].MustContain = [][]byte{primaryData}
	}
	coretest.Contract(t, heifHandler, cases)

	// A real group stays, so an alternative or a stereo pair still works.
	kept := mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", group("altr", []uint32{1, 5}, nil)) })
	out, _ := mustClean(t, heifHandler, kept, core.Options{})
	if !bytes.Contains(out, group("altr", []uint32{1, 5}, nil)) {
		t.Error("an ordinary entity group was removed")
	}
	// Item 2 is the thumbnail, which goes. The group must stop naming it.
	shrunk := mutate(func(s *heifSpec) { s.metaExtra = bx("grpl", group("altr", []uint32{2, 1, 5}, nil)) })
	out, _ = mustClean(t, heifHandler, shrunk, core.Options{})
	if !bytes.Contains(out, cat([]byte("altr"), be32(0), be32(1), be32(2), be32(1), be32(5), be32(0))) {
		t.Error("the group still names the removed item")
	}
}

// TestItemAddressedThroughItemRefused closes a way to leave bytes behind: an Exif
// item stored by reference inside a kept image's extent cannot be cut out of it, and
// the Limitations say such items are refused.
func TestItemAddressedThroughItemRefused(t *testing.T) {
	spec := phonePhoto()
	spec.ilocVersion = 1
	data := spec.build()
	h := parseFixtureIloc(data)
	out := bytes.Clone(data)
	for _, e := range h.entries {
		if e.id == 3 { // the Exif item
			copy(out[bytes.Index(out, e.raw)+2:], be16(2))
		}
	}
	_, err := heifHandler.Inspect(testContext(), core.BytesSource(out), core.Options{})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("gave %v, want ErrUnsupported", err)
	}
}

// The engine prints Finding.Field, Finding.Where and warnings on clean, so none of
// them may hold text from the file. Text in Value is hidden there, and text a refusal
// quotes is blanked by the CLI, so that is where such text has to go.
const (
	leakType = "QZQZ"         // all that a box type, handler or item type can hold
	leakText = "QZMARKERTEXT" // anything longer
	leakID   = 0x515A515A     // a track ID, which the CLI would print in decimal
)

// quotedInRefusal matches what the CLI blanks in a refusal's reason on clean.
var quotedInRefusal = regexp.MustCompile(`"(?:[^"\\]|\\.)*(?:"|$)`)

func checkNoLeak(t *testing.T, name string, h core.Handler, data []byte) {
	t.Helper()
	markers := []string{leakType, leakText, strconv.Itoa(leakID)}
	var out bytes.Buffer
	rep, err := h.Clean(testContext(), core.BytesSource(data), &out, core.Options{})
	if err != nil {
		text := quotedInRefusal.ReplaceAllString(err.Error(), "")
		for _, marker := range markers {
			if strings.Contains(text, marker) {
				t.Errorf("%s: refusal holds %q outside quotes: %v", name, marker, err)
			}
		}
		return
	}
	for _, marker := range markers {
		for _, f := range rep.Findings {
			if strings.Contains(f.Field, marker) || strings.Contains(f.Where, marker) || strings.Contains(f.Reason, marker) {
				t.Errorf("%s: finding holds %q: field %q, where %q, reason %q", name, marker, f.Field, f.Where, f.Reason)
			}
		}
		for _, w := range rep.Warnings {
			if strings.Contains(w, marker) {
				t.Errorf("%s: warning holds %q: %s", name, marker, w)
			}
		}
	}
}

func TestReportHoldsNoFileTextOutsideValues(t *testing.T) {
	text := []byte(leakText)
	item := func(typ string) []byte { return bx(typ, bx("data", be32(1), be32(0), text)) }
	key := cat(be32(uint32(len(text)+8)), []byte("mdta"), text)
	mdta := bx("meta", fullBox("hdlr", 0, 0, be32(0), []byte("mdta"), make([]byte, 12), []byte{0}),
		fullBox("keys", 0, 0, be32(1), key), bx("ilst", item(string(be32(1)))))
	freeform := bx("----", fullBox("mean", 0, 0, text), fullBox("name", 0, 0, text), bx("data", be32(1), be32(0), text))
	tags := fullBox("meta", 0, 0, bx("ilst", freeform, item(leakType)))
	xmpNames := []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:` + leakText + `="http://example.com/" ` + leakText + `:` + leakText + `="` + leakText + `"/></rdf:RDF></x:xmpmeta>`)

	movie := func(fn func(*movieSpec)) []byte {
		s := plainMovie()
		fn(&s)
		return s.build()
	}
	for name, data := range map[string][]byte{
		"udta box type":   movie(func(s *movieSpec) { s.udta = bx(leakType, text) }),
		"moov child type": movie(func(s *movieSpec) { s.moovExtra = bx(leakType, text) }),
		"top-level type":  movie(func(s *movieSpec) { s.before = bx(leakType, text) }),
		"mdta key":        movie(func(s *movieSpec) { s.moovExtra = mdta }),
		"freeform name":   movie(func(s *movieSpec) { s.udta = tags }),
		"uuid":            movie(func(s *movieSpec) { s.moovExtra = bx("uuid", text, text) }),
		"XMP names":       movie(func(s *movieSpec) { s.before = bx("uuid", xmpUUID, xmpNames) }),
		"entry box":       movie(func(s *movieSpec) { s.tracks[0].entry = visualEntry("avc1", "", bx(leakType, text)) }),
		"wave box": movie(func(s *movieSpec) {
			s.tracks[1].entry = audioEntry("mp4a", 0, nil, bx("wave", bx(leakType, text)))
		}),
		"entry format":     movie(func(s *movieSpec) { s.tracks[0].entry = visualEntry(leakType, "") }),
		"reference type":   movie(func(s *movieSpec) { s.tracks[0].refs = bx(leakType, text) }),
		"trak child type":  movie(func(s *movieSpec) { s.tracks[0].trakExtra = bx(leakType, text) }),
		"track group type": movie(func(s *movieSpec) { s.tracks[0].trakExtra = bx("trgr", bx(leakType, text)) }),
		"kept track ID":    movie(func(s *movieSpec) { s.tracks[0].id = leakID }),
		"removed track": movie(func(s *movieSpec) {
			s.tracks = append(s.tracks, testTrack{id: leakID, handler: leakType, format: leakType, samples: [][]byte{text}})
		}),
		"subtitle track ID": movie(func(s *movieSpec) {
			s.tracks = append(s.tracks, testTrack{id: leakID, handler: "sbtl", format: "tx3g", samples: [][]byte{text}})
		}),
		"auxiliary track": movie(func(s *movieSpec) {
			s.tracks = append(s.tracks, testTrack{id: leakID, handler: "auxv", entry: visualEntry(leakType, ""), samples: [][]byte{text}})
		}),
		"fragmented refusal": cat(defaultFtyp, bx("moov",
			fullBox("mvhd", 0, 0, make([]byte, 96)),
			testTrack{id: 2, handler: leakType, format: leakType}.trak(0, 0),
			bx("mvex", fullBox("trex", 0, 0, be32(1), make([]byte, 16)))), bx("mdat", text)),
	} {
		checkNoLeak(t, name, mp4Handler, data)
	}

	photo := func(fn func(*heifSpec)) []byte {
		s := phonePhoto()
		fn(&s)
		return s.build()
	}
	for name, data := range map[string][]byte{
		"item name":      photo(func(s *heifSpec) { s.items[0].name = leakText }),
		"item type":      photo(func(s *heifSpec) { s.items = append(s.items, itemSpec{id: 9, typ: leakType, data: text}) }),
		"content type":   photo(func(s *heifSpec) { s.items[3].content = leakText }),
		"mime item":      photo(func(s *heifSpec) { s.items[3].data = text }),
		"meta child":     photo(func(s *heifSpec) { s.metaExtra = bx(leakType, text) }),
		"group type":     photo(func(s *heifSpec) { s.metaExtra = bx("grpl", bx(leakType, text)) }),
		"after the mdat": photo(func(s *heifSpec) { s.after = bx(leakType, text) }),
		"reference type": photo(func(s *heifSpec) { s.refs = append(s.refs, itemRef{typ: leakType, from: 1, to: []uint32{5}}) }),
		"property": photo(func(s *heifSpec) {
			s.properties = append(s.properties, bx(leakType, text))
			s.items[0].props = append(s.items[0].props, 8)
		}),
		"essential property refusal": photo(func(s *heifSpec) {
			s.properties = append(s.properties, bx(leakType, text))
			s.items[0].props = append(s.items[0].props, 0x88)
		}),
	} {
		checkNoLeak(t, name, heifHandler, data)
	}
}
