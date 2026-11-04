package isobmff

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// spareSecret is what these tests plant in bytes no reader looks at. It is short
// because many of these places are.
const spareSecret = "Tst!Spar"

func assertGone(t *testing.T, h core.Handler, data []byte, secret string) []byte {
	t.Helper()
	if !bytes.Contains(data, []byte(secret)) {
		t.Fatal("the fixture does not hold the secret")
	}
	out, _ := mustClean(t, h, data, core.Options{})
	if bytes.Contains(out, []byte(secret)) {
		t.Fatalf("%q survived cleaning", secret)
	}
	if len(out) != len(data) {
		t.Fatalf("the file changed size: %d became %d", len(data), len(out))
	}
	again, rep := mustClean(t, h, out, core.Options{})
	if !bytes.Equal(again, out) {
		t.Fatal("cleaning the output changed it")
	}
	if residual := rep.Residual(); len(residual) != 0 {
		t.Fatalf("residual findings: %+v", residual)
	}
	return out
}

// plant overwrites the part of b at off with the secret.
func plant(b []byte, off int, secret string) []byte {
	out := bytes.Clone(b)
	copy(out[off:], secret)
	return out
}

// TestReservedFieldsAreZeroed covers fields the specifications say must be zero, and
// that readers skip. Each holds a few bytes, in every track of every file.
func TestReservedFieldsAreZeroed(t *testing.T) {
	short := spareSecret[:4]
	mvhd := func(off int, secret string) []byte {
		return plant(fullBox("mvhd", 0, 0, be32(0), be32(0), be32(600), be32(100), make([]byte, 80)), 8+off, secret)
	}
	tkhd := func(off int, secret string) []byte {
		return plant(fullBox("tkhd", 0, 7, be32(0), be32(0), be32(1), make([]byte, 68)), 8+off, secret)
	}
	cases := []struct {
		name   string
		old    []byte // an exact box in the fixture
		new    []byte
		secret string
	}{
		{"mvhd reserved", mvhd(0, ""), mvhd(28, spareSecret), spareSecret},
		{"mvhd pre_defined", mvhd(0, ""), mvhd(72, spareSecret), spareSecret},
		{"mvhd flags", mvhd(0, ""), mvhd(1, "Tst"), "Tst"},
		{"tkhd first reserved", tkhd(0, ""), tkhd(16, short), short},
		{"tkhd second reserved", tkhd(0, ""), tkhd(24, spareSecret), spareSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := plainMovie()
			spec.times = 0
			data := spec.build()
			if !bytes.Contains(data, c.old) {
				t.Fatal("the fixture does not hold the box")
			}
			assertGone(t, mp4Handler, bytes.ReplaceAll(data, c.old, c.new), c.secret)
		})
	}
}

func TestHandlerPreDefinedIsZeroed(t *testing.T) {
	spec := plainMovie()
	data := spec.build()
	mhlr := []byte("mhlr")
	t.Run("not a QuickTime component type", func(t *testing.T) {
		assertGone(t, mp4Handler, bytes.ReplaceAll(data, mhlr, []byte(spareSecret[:4])), spareSecret[:4])
	})
	t.Run("a QuickTime component type stays", func(t *testing.T) {
		out, _ := mustClean(t, mp4Handler, data, core.Options{})
		if !bytes.Contains(out, mhlr) {
			t.Fatal("ffmpeg reads this field to tell QuickTime files from ISO ones")
		}
	})
	t.Run("in a HEIF meta box", func(t *testing.T) {
		photo := phonePhoto().build()
		i := bytes.Index(photo, []byte("hdlr"))
		if i < 0 {
			t.Fatal("no hdlr")
		}
		assertGone(t, heifHandler, plant(photo, i+8, spareSecret[:4]), spareSecret[:4])
	})
}

// TestSampleEntryFixedFieldsAreZeroed covers the fields of a sample entry that no
// decoder reads. QuickTime's temporal and spatial quality sit there too.
func TestSampleEntryFixedFieldsAreZeroed(t *testing.T) {
	for name, off := range map[string]int{
		"reserved":       8,
		"version":        16,
		"temporal":       24,
		"spatial":        28,
		"data size":      44,
		"horizontal dpi": 36,
		"vertical dpi":   40,
	} {
		t.Run("visual "+name, func(t *testing.T) {
			spec := plainMovie()
			spec.tracks[0].entry = plant(visualEntry("avc1", "", bx("avcC", []byte{1, 2, 3, 4})), off, spareSecret[:4])
			assertGone(t, mp4Handler, spec.build(), spareSecret[:4])
		})
	}
	t.Run("audio reserved", func(t *testing.T) {
		spec := plainMovie()
		spec.tracks[1].entry = plant(audioEntry("mp4a", 0, nil, bx("esds", []byte{0, 0, 0, 0, 3, 0})), 8, spareSecret[:4])
		assertGone(t, mp4Handler, spec.build(), spareSecret[:4])
	})
	t.Run("audio revision", func(t *testing.T) {
		spec := plainMovie()
		spec.tracks[1].entry = plant(audioEntry("mp4a", 0, nil, bx("esds", []byte{0, 0, 0, 0, 3, 0})), 18, spareSecret[:2])
		assertGone(t, mp4Handler, spec.build(), spareSecret[:2])
	})
}

// TestShortUnknownBoxInASampleEntryWarns covers a loophole: the allowance for a short
// box inside QuickTime's wave box also let any number of them sit in the entry itself,
// each holding 8 bytes, with no word to the user.
func TestShortUnknownBoxInASampleEntryWarns(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].entry = visualEntry("avc1", "", bx("avcC", []byte{1, 2, 3, 4}), bx("zzzz", []byte(spareSecret)), bx("zzzy", []byte(spareSecret)))
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "does not recognise") {
		t.Fatalf("no warning about the boxes that stayed: %q", rep.Warnings)
	}

	wave := plainMovie()
	wave.tracks[1].entry = audioEntry("mp4a", 1, make([]byte, 16),
		bx("wave", bx("frma", []byte("mp4a")), bx("zzzz", []byte(spareSecret)), bx("esds", []byte{0, 0, 0, 0, 3, 0})))
	_, rep = mustClean(t, mp4Handler, wave.build(), core.Options{})
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "does not recognise") {
		t.Fatalf("no warning about the box in a wave box: %q", rep.Warnings)
	}
}

// avcConfig is a one-SPS, one-PPS AVC configuration.
func avcConfig() []byte {
	return cat([]byte{1, 66, 0, 30, 0xFF, 0xE1}, be16(4), []byte{0x67, 66, 0, 30}, []byte{1}, be16(2), []byte{0x68, 0xCE})
}

// hevcConfig is an HEVC configuration with one array holding one NAL unit.
func hevcConfig() []byte {
	return cat(make([]byte, 22), []byte{1}, []byte{33}, be16(1), be16(3), []byte{0x42, 1, 1})
}

// av1Config is an AV1 configuration holding one sequence header OBU.
func av1Config() []byte {
	return []byte{0x81, 0, 0, 0, 0x0A, 2, 1, 2}
}

// TestBytesPastConfigurationAreZeroed covers decoder configuration, which must stay,
// but which states its own length. Decoders stop there, so what follows is spare.
func TestBytesPastConfigurationAreZeroed(t *testing.T) {
	type entryBox struct {
		name string
		box  []byte
		keep []byte
	}
	secret := []byte(spareSecret)
	visual := []entryBox{
		{"avcC", bx("avcC", avcConfig(), secret), avcConfig()},
		{"avcC high profile with an extension", bx("avcC", []byte{1, 100, 0, 30, 0xFF, 0xE1}, be16(4), []byte{0x67, 100, 0, 30}, []byte{1}, be16(2), []byte{0x68, 0xCE}, []byte{0xFC, 0xF8, 0xF8, 0}, secret), []byte{0xFC, 0xF8, 0xF8, 0}},
		{"hvcC", bx("hvcC", hevcConfig(), secret), hevcConfig()},
		{"av1C", bx("av1C", av1Config(), secret), av1Config()},
		{"vpcC", fullBox("vpcC", 1, 0, []byte{0, 10, 0x80, 1, 1, 1}, be16(2), []byte{7, 7}, secret), []byte{0, 10, 0x80, 1, 1, 1}},
		{"pasp", bx("pasp", be32(1), be32(1), secret), be32(1)},
		{"fiel", bx("fiel", []byte{1, 0}, secret), []byte("fiel")},
	}
	for _, c := range visual {
		t.Run(c.name, func(t *testing.T) {
			spec := plainMovie()
			spec.tracks[0].entry = visualEntry("avc1", "", c.box)
			out := assertGone(t, mp4Handler, spec.build(), spareSecret)
			if !bytes.Contains(out, c.keep) {
				t.Fatal("the configuration itself was changed")
			}
		})
	}
	t.Run("esds", func(t *testing.T) {
		descriptor := []byte{3, 7, 0, 1, 0, 4, 2, 0x40, 0x15}
		spec := plainMovie()
		spec.tracks[1].entry = audioEntry("mp4a", 0, nil, fullBox("esds", 0, 0, descriptor, secret))
		out := assertGone(t, mp4Handler, spec.build(), spareSecret)
		if !bytes.Contains(out, descriptor) {
			t.Fatal("the descriptor was changed")
		}
		// A size too small for what it holds is one to distrust: readers look past it.
		short := []byte{3, 3, 0, 1, 0, 4, 2, 0x40, 0x15}
		spec.tracks[1].entry = audioEntry("mp4a", 0, nil, fullBox("esds", 0, 0, short))
		kept, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if !bytes.Contains(kept, short) {
			t.Fatal("a descriptor with a wrong size lost its configuration")
		}
	})
	t.Run("hvcC in a HEIF property", func(t *testing.T) {
		spec := phonePhoto()
		spec.properties[0] = bx("hvcC", hevcConfig(), secret)
		out := assertGone(t, heifHandler, spec.build(), spareSecret)
		if !bytes.Contains(out, hevcConfig()) {
			t.Fatal("the configuration itself was changed")
		}
	})
	t.Run("av1C in a HEIF property", func(t *testing.T) {
		spec := phonePhoto()
		spec.brand = "avif"
		spec.properties[0] = bx("av1C", av1Config(), secret)
		out := assertGone(t, heifHandler, spec.build(), spareSecret)
		if !bytes.Contains(out, av1Config()) {
			t.Fatal("the configuration itself was changed")
		}
	})
}

// TestBitRateBoxIsZeroed covers btrt: three numbers for a player's buffering, for which
// zero means not stated, so nothing needs what they hold.
func TestBitRateBoxIsZeroed(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].entry = visualEntry("avc1", "", bx("avcC", []byte{1, 2, 3, 4}), bx("btrt", []byte(spareSecret+"abcd")))
	assertGone(t, mp4Handler, spec.build(), spareSecret)
}

// TestBytesPastSampleTablesAreZeroed covers kept tables whose length comes from a count
// of their own, or from the number of samples in the track.
func TestBytesPastSampleTablesAreZeroed(t *testing.T) {
	secret := []byte(spareSecret)
	cases := map[string][]byte{
		"sdtp":                    fullBox("sdtp", 0, 0, []byte{0x10, 0x10, 0x10}, secret),
		"stdp":                    fullBox("stdp", 0, 0, make([]byte, 6), secret),
		"padb":                    fullBox("padb", 0, 0, be32(3), make([]byte, 2), secret),
		"sgpd with default size":  fullBox("sgpd", 1, 0, []byte("roll"), be32(2), be32(1), be16(0xFFFF), secret),
		"sgpd with its own sizes": fullBox("sgpd", 1, 0, []byte("alst"), be32(0), be32(1), be32(2), be16(1), secret),
		"sgpd version 0":          fullBox("sgpd", 0, 0, []byte("roll"), be32(1), be16(0xFFFF), secret),
		"subs":                    fullBox("subs", 0, 0, be32(1), be32(1), be16(1), be16(4), []byte{0, 0}, be32(0), secret),
		"subs version 1":          fullBox("subs", 1, 0, be32(1), be32(1), be16(1), be32(4), []byte{0, 0}, be32(0), secret),
	}
	for name, box := range cases {
		t.Run(name, func(t *testing.T) {
			spec := plainMovie()
			spec.tracks[0].entry = nil
			spec.tracks[0].minfExtra = nil
			spec.moovExtra = nil
			data := withStblChild(t, spec, box)
			assertGone(t, mp4Handler, data, spareSecret)
		})
	}
}

// withStblChild builds a movie and adds a child to the first track's stbl, fixing the
// sizes around it. The movie's mdat comes first, so no sample offset moves.
func withStblChild(t *testing.T, spec movieSpec, children ...[]byte) []byte {
	t.Helper()
	child := cat(children...)
	data := spec.build()
	stbl := bytes.Index(data, []byte("stbl"))
	if stbl < 4 {
		t.Fatal("no stbl")
	}
	end := stbl - 4 + int(binary.BigEndian.Uint32(data[stbl-4:]))
	out := append(bytes.Clone(data[:end]), child...)
	out = append(out, data[end:]...)
	for _, name := range []string{"stbl", "minf", "mdia", "trak", "moov"} {
		i := bytes.LastIndex(out[:stbl+4], []byte(name))
		if i < 4 {
			t.Fatalf("no %s", name)
		}
		size := binary.BigEndian.Uint32(out[i-4:])
		binary.BigEndian.PutUint32(out[i-4:], size+uint32(len(child)))
	}
	return out
}

// TestBytesPastFragmentIndexAreZeroed covers tfra, whose entries have sizes of their own.
func TestBytesPastFragmentIndexAreZeroed(t *testing.T) {
	secret := []byte(spareSecret)
	entry := cat(be32(0), be32(0), []byte{1, 1, 1})
	cases := map[string][]byte{
		"tfra":           fullBox("tfra", 0, 0, be32(1), be32(0), be32(1), entry, secret),
		"tfra version 1": fullBox("tfra", 1, 0, be32(1), be32(0), be32(1), be64(0), be64(0), []byte{1, 1, 1}, secret),
		"tfra big sizes": fullBox("tfra", 0, 0, be32(1), be32(0x15), be32(1), be32(0), be32(0), []byte{0, 1}, []byte{1}, []byte{0, 1}, secret),
	}
	for name, box := range cases {
		t.Run(name, func(t *testing.T) {
			spec := plainMovie()
			spec.moovExtra = bx("mvex", fullBox("trex", 0, 0, be32(1), be32(1), be32(0), be32(0), be32(0)))
			spec.after = bx("mfra", box, fullBox("mfro", 0, 0, be32(16)))
			out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
			if bytes.Contains(out, secret) {
				t.Fatalf("%q survived cleaning", secret)
			}
		})
	}
}

// TestUnrecognisedFileTypeBrands covers a list of four-byte names that nothing checks.
// One that means anything to a reader stays. The rest are free room.
func TestUnrecognisedFileTypeBrands(t *testing.T) {
	t.Run("compatible brand", func(t *testing.T) {
		spec := plainMovie()
		spec.ftyp = bx("ftyp", []byte("isom"), be32(0), []byte("isomTst!mp41"))
		out, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if bytes.Contains(out, []byte("Tst!")) {
			t.Fatal("an unrecognised brand survived")
		}
		if !bytes.Contains(out, []byte("mp41")) {
			t.Fatal("a brand readers know was removed")
		}
		if !hasFinding(rep, "File-type brand", core.ActionRemove) {
			t.Fatalf("no finding: %s", describeFindings(rep))
		}
	})
	t.Run("minor version", func(t *testing.T) {
		spec := plainMovie()
		spec.ftyp = bx("ftyp", []byte("isom"), []byte("Tst!"), []byte("isom"))
		out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if bytes.Contains(out, []byte("Tst!")) {
			t.Fatal("the minor version survived")
		}
	})
	t.Run("bytes that are no whole brand", func(t *testing.T) {
		spec := plainMovie()
		spec.ftyp = bx("ftyp", []byte("isom"), be32(0), []byte("isomTst"))
		out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if bytes.Contains(out, []byte("Tst")) {
			t.Fatal("the bytes past the last whole brand survived")
		}
	})
	t.Run("major brand warns", func(t *testing.T) {
		spec := plainMovie()
		spec.ftyp = bx("ftyp", []byte("XAVC"), be32(0), []byte("XAVCmp42iso2"))
		out, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if !bytes.Contains(out, []byte("XAVC")) {
			t.Fatal("the major brand was removed")
		}
		if !strings.Contains(strings.Join(rep.Warnings, "\n"), "file-type brand") {
			t.Fatalf("no warning: %q", rep.Warnings)
		}
		if !hasFinding(rep, "File-type brand", core.ActionRetain) {
			t.Fatalf("no finding: %s", describeFindings(rep))
		}
	})
	t.Run("nothing to say about known brands", func(t *testing.T) {
		out, rep := mustClean(t, mp4Handler, plainMovie().build(), core.Options{})
		if !bytes.Contains(out, []byte("isomiso2avc1mp41")) {
			t.Fatal("known brands changed")
		}
		if hasFinding(rep, "File-type brand", core.ActionRemove) || hasFinding(rep, "File-type brand", core.ActionRetain) || len(rep.Warnings) != 0 {
			t.Fatalf("a false alarm: %s %q", describeFindings(rep), rep.Warnings)
		}
	})
	t.Run("styp", func(t *testing.T) {
		spec := plainMovie()
		spec.before = bx("styp", []byte("msdh"), []byte("Tst!"), []byte("msdhTst?"))
		out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if bytes.Contains(out, []byte("Tst")) {
			t.Fatal("styp bytes survived")
		}
	})
}

func hasFinding(rep *core.Report, field string, action core.Action) bool {
	for _, f := range rep.Findings {
		if f.Field == field && f.Action == action {
			return true
		}
	}
	return false
}

// TestReferenceToUndeclaredItemIsDropped covers iref entries that name items the file
// never declares. Nothing can follow them, so their IDs are only somewhere to put bytes.
func TestReferenceToUndeclaredItemIsDropped(t *testing.T) {
	const id = 0x5473 // "Ts"
	for name, ref := range map[string]itemRef{
		"from":      {typ: "cdsc", from: id, to: []uint32{1}},
		"to":        {typ: "cdsc", from: 1, to: []uint32{id}},
		"thumbnail": {typ: "thmb", from: id, to: []uint32{1}},
	} {
		t.Run(name, func(t *testing.T) {
			spec := phonePhoto()
			spec.refs = append(spec.refs, ref)
			data := spec.build()
			out, _ := mustClean(t, heifHandler, data, core.Options{})
			if bytes.Contains(out, []byte{0x54, 0x73}) {
				t.Fatal("the undeclared item's ID survived")
			}
		})
	}
}

// TestBytesPastGridDescriptionAreZeroed covers a derived image's description, which
// states its own length. It stays, but a decoder stops reading it there.
func TestBytesPastGridDescriptionAreZeroed(t *testing.T) {
	grid := []byte{0, 0, 0, 0, 0, 16, 0, 16} // version, flags, rows, columns, width, height
	wide := []byte{0, 1, 0, 0, 0, 0, 0, 16, 0, 0, 0, 16}
	overlay := cat([]byte{0, 0}, make([]byte, 8), be16(16), be16(16), be16(0), be16(0))
	for name, c := range map[string]struct {
		typ    string
		data   []byte
		inIdat bool
	}{
		"grid in idat": {"grid", grid, true},
		"grid in mdat": {"grid", grid, false},
		"wide grid":    {"grid", wide, true},
		"overlay":      {"iovl", overlay, true},
	} {
		t.Run(name, func(t *testing.T) {
			spec := phonePhoto()
			spec.ilocVersion = 1
			spec.items = []itemSpec{
				{id: 1, typ: c.typ, data: append(bytes.Clone(c.data), spareSecret...), inIdat: c.inIdat, props: []uint16{2, 3}},
				{id: 7, typ: "hvc1", data: primaryData, hidden: true, props: []uint16{1}},
			}
			spec.primary = 1
			spec.refs = []itemRef{{typ: "dimg", from: 1, to: []uint32{7}}}
			out := assertGone(t, heifHandler, spec.build(), spareSecret)
			if !bytes.Contains(out, primaryData) || !bytes.Contains(out, c.data) {
				t.Fatal("the picture or its description was changed")
			}
		})
	}
}

// TestCompleteBoxesAreLeftAlone holds the other half: a real file's boxes, which fill
// exactly what they state, must come through unchanged.
func TestCompleteBoxesAreLeftAlone(t *testing.T) {
	spec := plainMovie()
	spec.times = 0
	spec.tracks[0].entry = visualEntry("avc1", "", bx("avcC", avcConfig()), bx("pasp", be32(1), be32(1)))
	esds := fullBox("esds", 0, 0, []byte{3, 7, 0, 1, 0, 4, 2, 0x40, 0x15})
	spec.tracks[1].entry = audioEntry("mp4a", 0, nil, esds)
	out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
	for _, want := range [][]byte{bx("avcC", avcConfig()), bx("pasp", be32(1), be32(1)), esds} {
		if !bytes.Contains(out, want) {
			t.Fatalf("% x changed", want)
		}
	}
}

// TestTableClaimingMoreThanItHoldsIsRefused covers a count past the end of the box.
// Readers stop at the end, which leaves them reading a table of nothing, and the count
// field itself is then four more bytes nothing checks.
func TestTableClaimingMoreThanItHoldsIsRefused(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].trakExtra = bx("edts", fullBox("elst", 0, 0, []byte("Tst!")))
	var out bytes.Buffer
	if _, err := mp4Handler.Clean(testContext(), core.BytesSource(spec.build()), &out, core.Options{}); err == nil {
		t.Fatal("a table that overruns its box was accepted")
	}
}

// TestDataEntryTypeIsNormalised covers the type of a self-contained data reference,
// which says nothing, since the media is in this file whatever the type is.
func TestDataEntryTypeIsNormalised(t *testing.T) {
	spec := plainMovie()
	spec.times = 0
	data := bytes.ReplaceAll(spec.build(), []byte("url "), []byte(spareSecret[:4]))
	assertGone(t, mp4Handler, data, spareSecret[:4])
}

// TestChunkRunDescriptionIndexIsChecked covers a field readers ignore if it is out of
// range, in every run of chunks.
func TestChunkRunDescriptionIndexIsChecked(t *testing.T) {
	spec := plainMovie()
	old := fullBox("stsc", 0, 0, be32(1), be32(1), be32(1), be32(1))
	data := bytes.ReplaceAll(spec.build(), old, fullBox("stsc", 0, 0, be32(1), be32(1), be32(1), []byte(spareSecret[:4])))
	out := assertGone(t, mp4Handler, data, spareSecret[:4])
	if !bytes.Contains(out, old) {
		t.Fatal("the run should now use the track's one description")
	}
}

// TestUnknownSampleGroupIsRemoved covers groups of a kind no specification defines, which
// are a whole table of anything that no reader will ever look at.
func TestUnknownSampleGroupIsRemoved(t *testing.T) {
	secret := []byte(spareSecret + spareSecret)
	sgpd := fullBox("sgpd", 1, 0, []byte("Tst?"), be32(uint32(len(secret))), be32(1), secret)
	sbgp := fullBox("sbgp", 0, 0, []byte("Tst?"), be32(1), be32(3), be32(1))
	assertGone(t, mp4Handler, withStblChild(t, plainMovie(), sgpd, sbgp), "Tst?")
}

// TestKnownSampleGroupsStay is the other half: what a player uses to seek must not go.
func TestKnownSampleGroupsStay(t *testing.T) {
	sgpd := fullBox("sgpd", 1, 0, []byte("roll"), be32(2), be32(1), be16(0xFFFF))
	sbgp := fullBox("sbgp", 0, 0, []byte("roll"), be32(1), be32(3), be32(1))
	out, _ := mustClean(t, mp4Handler, withStblChild(t, plainMovie(), sgpd, sbgp), core.Options{})
	if !bytes.Contains(out, sgpd) || !bytes.Contains(out, sbgp) {
		t.Fatal("a sample group a player uses was removed")
	}
}

// TestSyncTableEntriesAreChecked covers a table of sample numbers that players only
// consult to seek. A number that is out of order or past the last sample is free room.
func TestSyncTableEntriesAreChecked(t *testing.T) {
	const secret = 0x54737421 // "Tst!"
	spec := plainMovie()
	data := withStblChild(t, spec, fullBox("stss", 0, 0, be32(3), be32(1), be32(secret), be32(3)))
	out := assertGone(t, mp4Handler, data, "Tst!")
	if !bytes.Contains(out, fullBox("stss", 0, 0, be32(1), be32(1), be32(0), be32(0))) {
		t.Fatal("the valid part of the table should stay")
	}
	good := fullBox("stss", 0, 0, be32(2), be32(1), be32(3))
	out, _ = mustClean(t, mp4Handler, withStblChild(t, spec, good), core.Options{})
	if !bytes.Contains(out, good) {
		t.Fatal("a valid table changed")
	}
}

// TestOldQuickTimeRegionsWarn covers a clipping region and a matte. Only old QuickTime
// players use them, and they can hold anything, so the user has to hear they stayed.
func TestOldQuickTimeRegionsWarn(t *testing.T) {
	for name, box := range map[string][]byte{
		"clip": bx("clip", bx("crgn", []byte(spareSecret))),
		"matt": bx("matt", fullBox("kmat", 0, 0, []byte(spareSecret))),
	} {
		t.Run(name, func(t *testing.T) {
			spec := plainMovie()
			spec.tracks[0].trakExtra = box
			_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
			if !strings.Contains(strings.Join(rep.Warnings, "\n"), "clipping region or matte") {
				t.Fatalf("no warning: %q", rep.Warnings)
			}
		})
	}
	spec := plainMovie()
	if _, rep := mustClean(t, mp4Handler, spec.build(), core.Options{}); len(rep.Warnings) != 0 {
		t.Fatalf("a false alarm: %q", rep.Warnings)
	}
}

// TestBytesPastOldHeadersAreZeroed covers the fixed layouts inside a generic media header.
func TestBytesPastOldHeadersAreZeroed(t *testing.T) {
	secret := []byte(spareSecret)
	for name, box := range map[string][]byte{
		"gmin": bx("gmhd", bx("gmin", make([]byte, 16), secret)),
		"text": bx("gmhd", bx("text", make([]byte, 36), secret)),
	} {
		t.Run(name, func(t *testing.T) {
			spec := plainMovie()
			spec.tracks[0].minfExtra = box
			assertGone(t, mp4Handler, spec.build(), spareSecret)
		})
	}
	t.Run("leva", func(t *testing.T) {
		level := cat(be32(1), []byte{2}) // a track, and an assignment with nothing further
		spec := plainMovie()
		spec.moovExtra = bx("mvex", fullBox("leva", 0, 0, []byte{1}, level, secret), fullBox("trex", 0, 0, be32(1), be32(1), be32(0), be32(0), be32(0)))
		out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})
		if bytes.Contains(out, secret) {
			t.Fatal("the bytes past the last level survived")
		}
	})
}
