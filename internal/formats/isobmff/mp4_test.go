package isobmff

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

var (
	heifHandler = &handler{kind: kindHEIF, format: core.Format{ID: "heif"}}
	mp4Handler  = &handler{kind: kindMP4, format: core.Format{ID: "mp4"}}
)

func testContext() context.Context { return context.Background() }

func asBytes(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

// mediaBytes is what every fixture movie must still contain after cleaning.
func mediaBytes() [][]byte {
	return slices.Concat(videoSamples, audioSamples)
}

func TestMovieContract(t *testing.T) {
	timeFields := []string{"Capture time", "Modification time"}
	withGPS := plainMovie()
	withGPS.tracks = append(withGPS.tracks, gpsTrack())
	withGPS.tracks[0].refs = bx("cdsc", be32(3))

	withTimecode := plainMovie()
	withTimecode.tracks = append(withTimecode.tracks,
		testTrack{id: 3, handler: "tmcd", format: "tmcd", samples: [][]byte{[]byte("TIMECODE 12:34:56")}},
		testTrack{id: 4, handler: "meta", format: "gpmd", samples: [][]byte{[]byte("GPMF DEVC GPS5 TELEMETRY")}},
		testTrack{id: 5, handler: "camm", format: "camm", samples: [][]byte{[]byte("CAMM MOTION DATA")}},
		testTrack{id: 6, handler: "hint", format: "rtp ", samples: [][]byte{[]byte("RTP HINT DATA")}},
	)

	faststart := plainMovie()
	faststart.moovFirst = true
	faststart.udta = richUserData()

	moovLast := plainMovie()
	moovLast.udta = richUserData()

	legacy := plainMovie()
	legacy.ftyp = []byte{}
	legacy.before = bx("wide")
	legacy.udta = richUserData()

	large := plainMovie()
	large.largeMdat = true
	large.tracks[0].co64 = true
	large.tracks[1].co64 = true
	large.udta = richUserData()

	stz2 := plainMovie()
	stz2.tracks[0].samples = [][]byte{[]byte("VIDEO A"), []byte("VIDEO BB")}
	stz2.tracks[0].stz2Bits = 4
	stz2.tracks[1].stz2Bits = 16

	phone := plainMovie()
	phone.moovMeta = phoneMeta()

	m4a := movieSpec{
		ftyp:   bx("ftyp", []byte("M4A "), be32(512), []byte("isomiso2")),
		tracks: []testTrack{audioTrack()},
		udta:   iTunesUserData(),
		times:  fakeTime,
	}

	threeGP := plainMovie()
	threeGP.ftyp = bx("ftyp", []byte("3gp4"), be32(0), []byte("isom3gp4"))
	threeGP.udta = cat(
		fullBox("titl", 0, 0, be16(0x15C7), []byte("Test Title\x00")),
		fullBox("auth", 0, 0, be16(0x15C7), []byte("Test Author\x00")),
		fullBox("loci", 0, 0, be16(0x15C7), []byte("Test Place\x00"), []byte{0},
			be32(150372), be32(3201984), be32(0), []byte("Earth\x00Test Note\x00")),
	)

	topLevel := plainMovie()
	topLevel.before = cat(
		bx("uuid", xmpUUID, coretest.FakeXMP()),
		bx("Xtra", []byte("Test Windows Tag")),
		fullBox("meta", 0, 0, bx("ilst", ilstItem("\xa9nam", 1, []byte("Test Title")))),
		bx("udta", bx("\xa9xyz", qtText("+48.8584+002.2945/"))),
		bx("VEND", []byte("Test Vendor Box")),
	)

	trailing := plainMovie()
	trailing.after = append([]byte{0x12, 0x34, 0x56, 0x78, 0xFF}, []byte("SEFHTest Samsung Trailer SEFT")...)

	shortTrailing := plainMovie()
	shortTrailing.after = []byte("SEFT")

	stale := plainMovie()
	stale.before = bx("free", []byte("Test Stale Free Box"))
	stale.moovExtra = cat(bx("skip", []byte("Test Stale Skip Box")), bx("wide", []byte("Test Stale Wide Box")))
	stale.mdatPrefix = []byte("Test Orphan Before")
	stale.mdatGap = []byte("Test Orphan Between")
	stale.mdatSuffix = []byte("Test Orphan After")

	subtitles := plainMovie()
	subtitles.tracks = append(subtitles.tracks, testTrack{id: 3, handler: "sbtl", format: "tx3g", samples: [][]byte{[]byte("SUBTITLE TEXT")}})

	legacyAudio := plainMovie()
	legacyAudio.tracks[1] = testTrack{
		id: 2, handler: "soun", format: "sowt", samples: [][]byte{[]byte("PCMDPCMD")}, constSize: 1,
		entry: audioEntry("sowt", 0, nil),
	}

	qtV1 := plainMovie()
	qtV1.tracks[1].entry = audioEntry("mp4a", 1, make([]byte, 16), bx("wave", bx("frma", []byte("mp4a"))), bx("uuid", []byte("Test Audio Vendor")))

	icc := plainMovie()
	icc.tracks[0].entry = visualEntry("hvc1", "", bx("hvcC", []byte{1}), coloursICC(), bx("udta", []byte("Test Entry Metadata")))

	trakData := plainMovie()
	trakData.tracks[0].udta = bx("\xa9mod", qtText("Test Model"))

	timeOnly := plainMovie()

	cases := []coretest.Case{
		{
			Name:           "moov after mdat",
			Data:           moovLast.build(),
			WantFields:     slices.Concat(timeFields, []string{"GPS coordinates", "GPS altitude", "Camera make", "Camera model", "Software", "Date", "Title", "Artist", "Encoding tool", "GoPro metadata", "Camera firmware", "Other metadata tag", "Handler name", "Handler vendor code", "Compressor name", "Encoder vendor code"}),
			MustNotContain: asBytes("+48.8584", "Test Make", "Test Model", "Test Software", "Test Title", "Test Author", "Test Encoder", "Test GoPro Settings", "Test Firmware", "Test Vendor Box", "Test Handler Name", "GoPro AVC", "Tven"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "faststart",
			Data:           faststart.build(),
			WantFields:     []string{"GPS coordinates", "Camera make"},
			MustNotContain: asBytes("+48.8584", "Test Make"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "QuickTime without ftyp",
			Data:           legacy.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: asBytes("+48.8584"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "64-bit mdat and co64",
			Data:           large.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: asBytes("+48.8584"),
			MustContain:    mediaBytes(),
		},
		{
			Name:        "stz2 sample sizes",
			Data:        stz2.build(),
			MustContain: slices.Concat(stz2.tracks[0].samples, audioSamples),
		},
		{
			Name:           "timed metadata track",
			Data:           withGPS.build(),
			WantFields:     []string{"Timed metadata track"},
			MustNotContain: asBytes("GPS LOG", "mebx", "cdsc"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "timecode, telemetry, motion and hint tracks",
			Data:           withTimecode.build(),
			WantFields:     []string{"Timecode track", "GoPro telemetry track", "Camera motion track", "Hint track"},
			MustNotContain: asBytes("TIMECODE", "GPMF DEVC", "CAMM MOTION", "RTP HINT"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "phone keys and ISO 6709 location",
			Data:           phone.build(),
			WantFields:     []string{"GPS coordinates", "GPS altitude", "Camera make", "Camera model", "Software", "Date"},
			MustNotContain: asBytes("+48.8584", "com.apple.quicktime", "Test Make", "Test Model", "2001-02-03T04"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "M4A with iTunes tags and cover art",
			Data:           m4a.build(),
			WantFields:     []string{"Title", "Artist", "Album", "Date", "Purchase account", "Cover art", "Other metadata tag"},
			MustNotContain: asBytes("Test Title", "Test Author", "Test Album", "test@example.com", "Test Cover Art", "Test Custom"),
			MustContain:    audioSamples,
		},
		{
			Name:           "3GP asset boxes",
			Data:           threeGP.build(),
			WantFields:     []string{"Title", "Author", "GPS coordinates", "Location name"},
			MustNotContain: asBytes("Test Title", "Test Author", "Test Place", "Test Note"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "top-level metadata boxes",
			Data:           topLevel.build(),
			WantFields:     slices.Concat(coretest.FakeXMPFields(), []string{"Windows metadata", "Unknown box"}),
			MustNotContain: slices.Concat(coretest.FakeXMPSecrets(), asBytes("Test Windows Tag", "Test Vendor Box", "+48.8584")),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "trailer after the last box",
			Data:           trailing.build(),
			WantFields:     []string{"Trailing data"},
			MustNotContain: asBytes("SEFH", "SEFT"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "trailer too short to be a box",
			Data:           shortTrailing.build(),
			WantFields:     []string{"Trailing data"},
			MustNotContain: asBytes("SEFT"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "stale free space and orphaned media",
			Data:           stale.build(),
			WantFields:     []string{"Data in free space", "Unreferenced media data"},
			MustNotContain: asBytes("Test Stale", "Test Orphan"),
			MustContain:    mediaBytes(),
		},
		{
			Name:        "subtitle track stays",
			Data:        subtitles.build(),
			MustContain: slices.Concat(mediaBytes(), asBytes("SUBTITLE TEXT")),
		},
		{
			Name:        "legacy QuickTime audio with a placeholder sample size",
			Data:        legacyAudio.build(),
			MustContain: slices.Concat(videoSamples, asBytes("PCMDPCMD")),
		},
		{
			Name:           "QuickTime version 1 audio entry",
			Data:           qtV1.build(),
			MustNotContain: asBytes("Test Audio Vendor"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "ICC profile and metadata in a sample entry",
			Data:           icc.build(),
			WantFields:     []string{"ICC colour profile", "User data"},
			WantRetained:   []string{"ICC colour profile"},
			MustNotContain: asBytes("Test Entry Metadata"),
			MustContain:    mediaBytes(),
		},
		{
			Name:           "metadata in a track",
			Data:           trakData.build(),
			WantFields:     []string{"Camera model"},
			MustNotContain: asBytes("Test Model"),
			MustContain:    mediaBytes(),
		},
		{
			Name:        "nothing but times and names",
			Data:        timeOnly.build(),
			WantFields:  timeFields,
			MustContain: mediaBytes(),
		},
	}
	coretest.Contract(t, mp4Handler, cases)
}

// TestRemovedTrackSamplesZeroedInPlace checks the serious bug this code exists to
// prevent: a removed track's samples, the actual GPS log, surviving in the mdat.
func TestRemovedTrackSamplesZeroedInPlace(t *testing.T) {
	spec := plainMovie()
	spec.tracks = append(spec.tracks, gpsTrack())
	data := spec.build()
	out, rep := mustClean(t, mp4Handler, data, core.Options{})

	if len(out) != len(data) {
		t.Fatalf("size changed from %d to %d", len(data), len(out))
	}
	// Every media byte of a kept track stays at the same offset.
	for _, sample := range mediaBytes() {
		at := bytes.Index(data, sample)
		if at < 0 || !bytes.Equal(out[at:at+len(sample)], sample) {
			t.Errorf("kept sample %q moved or changed", sample)
		}
	}
	for _, sample := range gpsSamples {
		at := bytes.Index(data, sample)
		if !core.AllZero(out[at : at+len(sample)]) {
			t.Errorf("removed sample %q was not zeroed", sample)
		}
	}
	if bytes.Contains(out, []byte("trak\x00\x00\x00\x00mebx")) {
		t.Error("the removed trak box is still there")
	}
	if n := bytes.Count(out, []byte("trak")); n != 2 {
		t.Errorf("found %d trak boxes, want 2", n)
	}
	if !strings.Contains(describeFindings(rep), "Timed metadata track") {
		t.Errorf("no finding for the removed track: %s", describeFindings(rep))
	}
}

// TestOffsetsStayValid checks that the cleaned moov still locates every sample.
func TestOffsetsStayValid(t *testing.T) {
	for _, moovFirst := range []bool{false, true} {
		spec := plainMovie()
		spec.moovFirst = moovFirst
		spec.tracks = append(spec.tracks, gpsTrack())
		spec.udta = richUserData()
		out, _ := mustClean(t, mp4Handler, spec.build(), core.Options{})

		a, err := analyze(testContext(), core.BytesSource(out), core.Options{}, kindMP4)
		if err != nil {
			t.Fatalf("analyze the output: %v", err)
		}
		// The video comes first in the mdat, so its table entry is the lowest kept
		// range, and that must be exactly where the sample's bytes are.
		first := mergeSpans(a.keep)[0].start
		if want := int64(bytes.Index(out, videoSamples[0])); first != want {
			t.Errorf("moovFirst=%v: the table locates the video at %d, but it is at %d", moovFirst, first, want)
		}
	}
}

func TestHeaderFieldsZeroed(t *testing.T) {
	spec := plainMovie()
	data := spec.build()
	out, rep := mustClean(t, mp4Handler, data, core.Options{})
	if !strings.Contains(describeFindings(rep), `Capture time="2001-02-03 04:05:06 UTC"`) {
		t.Errorf("capture time not decoded: %s", describeFindings(rep))
	}
	if bytes.Contains(out, be32(fakeTime)) {
		t.Error("a timestamp survived")
	}
	// A vendor code in hdlr is stored in the first reserved word.
	if bytes.Contains(out, []byte("appl")) {
		t.Error("the handler vendor code survived")
	}
	for _, kept := range asBytes("mhlr", "vide", "soun", "avc1", "avcC", "mp4a", "esds") {
		if !bytes.Contains(out, kept) {
			t.Errorf("lost %q", kept)
		}
	}
}

func TestVersionOneTimes(t *testing.T) {
	const seconds = uint64(fakeTime)
	mvhd := fullBox("mvhd", 1, 0, be64(seconds), be64(seconds), be32(600), be64(100), make([]byte, 80))
	data := cat(defaultFtyp, bx("moov", mvhd), bx("mdat"))
	out, rep := mustClean(t, mp4Handler, data, core.Options{})
	if bytes.Contains(out, be64(seconds)) {
		t.Error("a 64-bit timestamp survived")
	}
	if !strings.Contains(describeFindings(rep), "2001-02-03 04:05:06 UTC") {
		t.Errorf("not decoded: %s", describeFindings(rep))
	}
}

func TestFragmentedFile(t *testing.T) {
	moofBox := bx("moof",
		fullBox("mfhd", 0, 0, be32(1)),
		bx("uuid", []byte("Test Fragment UUID 0123456789")),
		bx("traf",
			fullBox("tfhd", 0, 0, be32(1)),
			fullBox("tfdt", 0, 0, be32(0)),
			fullBox("trun", 0, 0, be32(0)),
			bx("uuid", []byte("Test Traf UUID 0123456789")),
			bx("meta", []byte("Test Traf Meta")),
			bx("udta", bx("\xa9mod", qtText("Test Traf Model"))),
		),
	)
	mvex := bx("mvex", fullBox("trex", 0, 0, be32(1), make([]byte, 16)))
	moov := func(extra ...[]byte) []byte {
		v := testTrack{id: 1, handler: "vide"}
		parts := [][]byte{fullBox("mvhd", 0, 0, make([]byte, 96)), v.trak(0, fakeTime), mvex}
		return bx("moov", append(parts, extra...)...)
	}
	mdatData := []byte("Test Fragment Media Stays")
	mfra := bx("mfra", fullBox("tfra", 0, 0, make([]byte, 12)), bx("uuid", []byte("Test Mfra UUID 0123456789")), fullBox("mfro", 0, 0, be32(16)))
	data := cat(defaultFtyp, moov(), moofBox, bx("mdat", mdatData), mfra)

	coretest.Contract(t, mp4Handler, []coretest.Case{{
		Name:           "fragments",
		Data:           data,
		WantFields:     []string{"Vendor UUID box", "Metadata box", "Camera model"},
		MustNotContain: asBytes("Test Fragment UUID", "Test Traf UUID", "Test Traf Meta", "Test Traf Model", "Test Mfra UUID"),
		MustContain:    [][]byte{mdatData, []byte("moof"), []byte("tfhd"), []byte("trun")},
	}})

	// A track that must go cannot be removed from a fragmented file. An empty_moov
	// file is the dangerous case: the moov's tables are empty, so they locate none
	// of the samples, which are all described by the fragments.
	telemetry := testTrack{id: 2, handler: "meta", format: "gpmd"}
	for name, withTrack := range map[string][]byte{
		"empty sample table":    cat(defaultFtyp, moov(telemetry.trak(0, fakeTime)), moofBox, bx("mdat", mdatData)),
		"mvex without any moof": cat(defaultFtyp, moov(telemetry.trak(0, fakeTime)), bx("mdat", mdatData)),
	} {
		if _, err := mp4Handler.Inspect(testContext(), core.BytesSource(withTrack), core.Options{}); !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("%s: removing a track from a fragmented file gave %v, want ErrUnsupported", name, err)
		}
	}
}

func TestProducerReferenceTimeZeroed(t *testing.T) {
	spec := plainMovie()
	spec.before = fullBox("prft", 0, 0, be32(1), be64(0xE1E2E3E4E5E6E7E8), be32(0))
	data := spec.build()
	out, rep := mustClean(t, mp4Handler, data, core.Options{})
	if bytes.Contains(out, be64(0xE1E2E3E4E5E6E7E8)) {
		t.Error("the wall-clock timestamp survived")
	}
	if !bytes.Contains(out, []byte("prft")) {
		t.Error("the prft box was removed, but it is on the allowlist")
	}
	if !strings.Contains(describeFindings(rep), "Producer reference time") {
		t.Errorf("not reported: %s", describeFindings(rep))
	}
}

// TestRefused lists structures that must fail closed, with the sentinel each must
// wrap, because the engine uses it to tell the user why a file was not cleaned.
func TestRefused(t *testing.T) {
	overlap := plainMovie()
	shared := []byte("SHARED BYTES OF BOTH")
	overlap.tracks = []testTrack{
		{id: 1, handler: "vide", samples: [][]byte{shared}},
		{id: 3, handler: "meta", format: "mebx", samples: [][]byte{shared},
			badOffset: func(_ int, off uint64) uint64 { return off - uint64(len(shared)) }},
	}

	pastEnd := plainMovie()
	pastEnd.tracks[0].badOffset = func(int, uint64) uint64 { return 0x7FFFFFF0 }

	wrap := plainMovie()
	wrap.tracks[0].co64 = true
	wrap.tracks[0].badOffset = func(int, uint64) uint64 { return 0xFFFFFFFFFFFFFFF8 }

	gpsOutside := plainMovie()
	gpsOutside.tracks = append(gpsOutside.tracks, gpsTrack())
	gpsOutside.tracks[2].badOffset = func(int, uint64) uint64 { return 0 } // inside the ftyp

	videoInFree := plainMovie()
	videoInFree.before = bx("free", make([]byte, 64))
	videoInFree.tracks[0].badOffset = func(int, uint64) uint64 { return uint64(len(defaultFtyp) + 8) }

	encrypted := plainMovie()
	encrypted.tracks[0].entry = visualEntry("encv", "")

	protected := plainMovie()
	protected.tracks[1].entry = audioEntry("drms", 0, nil)

	sinf := plainMovie()
	sinf.tracks[0].entry = visualEntry("avc1", "", bx("sinf", []byte{1}))

	pssh := plainMovie()
	pssh.moovExtra = fullBox("pssh", 0, 0, make([]byte, 20))

	compressed := plainMovie()
	compressed.moovExtra = bx("cmov", make([]byte, 8))

	reference := plainMovie()
	reference.moovExtra = bx("rmra", make([]byte, 8))

	badTimes := plainMovie()
	badTimes.moovExtra = fullBox("mvhd", 7, 0, make([]byte, 96))

	twoMoov := plainMovie()
	twoMoov.after = bx("moov", fullBox("mvhd", 0, 0, make([]byte, 96)))

	twoFtyp := plainMovie()
	twoFtyp.after = defaultFtyp

	external := plainMovie()
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"removed track overlapping a kept one", overlap.build(), core.ErrMalformed},
		{"chunk past the end of the file", pastEnd.build(), core.ErrMalformed},
		{"chunk offset near 2^64", wrap.build(), core.ErrMalformed},
		{"removed track outside the mdat", gpsOutside.build(), core.ErrMalformed},
		{"kept track inside a free box", videoInFree.build(), core.ErrMalformed},
		{"encv sample entry", encrypted.build(), core.ErrEncrypted},
		{"drms sample entry", protected.build(), core.ErrEncrypted},
		{"sinf in a sample entry", sinf.build(), core.ErrEncrypted},
		{"pssh in moov", pssh.build(), core.ErrEncrypted},
		{"compressed moov", compressed.build(), core.ErrUnsupported},
		{"reference movie", reference.build(), core.ErrUnsupported},
		{"unknown mvhd version", badTimes.build(), core.ErrUnsupported},
		{"two moov boxes", twoMoov.build(), core.ErrMalformed},
		{"two ftyp boxes", twoFtyp.build(), core.ErrMalformed},
		{"no moov", cat(defaultFtyp, bx("mdat", []byte("x"))), core.ErrMalformed},
		{"Canon CR3", cat(bx("ftyp", []byte("crx "), be32(1), []byte("crx isom")), bx("moov")), core.ErrUnsupported},
		{"media in another file", externalData(external), core.ErrUnsupported},
		{"child overrunning its parent", cat(defaultFtyp, bx("moov", []byte{0, 0, 1, 0, 't', 'r', 'a', 'k'}), bx("mdat")), core.ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := mp4Handler.Inspect(testContext(), core.BytesSource(c.data), core.Options{})
			if !errors.Is(err, c.want) {
				t.Errorf("Inspect gave %v, want %v", err, c.want)
			}
			_, cleanErr := mp4Handler.Clean(testContext(), core.BytesSource(c.data), &bytes.Buffer{}, core.Options{})
			if !errors.Is(cleanErr, c.want) {
				t.Errorf("Clean gave %v, want %v", cleanErr, c.want)
			}
		})
	}
}

// externalData rewrites a fixture so its first track has an alias data reference,
// as a QuickTime reference movie would.
func externalData(spec movieSpec) []byte {
	data := spec.build()
	entry := bytes.Index(data, fullBox("url ", 0, 1))
	external := fullBox("url ", 0, 0, []byte("file:///Users/test/source.mov\x00"))
	grow := len(external) - 12
	out := cat(data[:entry], external, data[entry+12:])
	// Grow each enclosing box by updating the ancestors' size fields.
	for _, name := range []string{"dref", "dinf", "minf", "mdia", "trak", "moov"} {
		at := bytes.LastIndex(out[:entry], []byte(name)) - 4
		size := uint32(out[at])<<24 | uint32(out[at+1])<<16 | uint32(out[at+2])<<8 | uint32(out[at+3])
		size += uint32(grow)
		copy(out[at:], be32(size))
	}
	return out
}

func TestSampleTableBoundaries(t *testing.T) {
	// A constant size that claims more than the file holds, on a video track.
	huge := plainMovie()
	huge.tracks[0].constSize = 0xFFFFFFFF
	if _, err := mp4Handler.Inspect(testContext(), core.BytesSource(huge.build()), core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("huge constant size gave %v", err)
	}
}

func TestOpenChunksStopAtNextReference(t *testing.T) {
	// Real QuickTime PCM: the table says size 1, the true frame is 4 bytes.
	audio := testTrack{
		id: 2, handler: "soun", format: "sowt", constSize: 1,
		samples: [][]byte{[]byte("PCM1PCM1PCM1"), []byte("PCM2PCM2PCM2")},
		entry:   audioEntry("sowt", 0, nil),
	}
	spec := movieSpec{tracks: []testTrack{audio, gpsTrack()}, times: fakeTime}
	data := spec.build()
	out, rep := mustClean(t, mp4Handler, data, core.Options{})
	if !bytes.Contains(out, []byte("PCM1PCM1PCM1")) || !bytes.Contains(out, []byte("PCM2PCM2PCM2")) {
		t.Error("real PCM audio beyond the placeholder size was zeroed")
	}
	if bytes.Contains(out, []byte("GPS LOG")) {
		t.Error("the removed track next to open-ended audio survived")
	}
	if len(rep.Warnings) == 0 {
		t.Error("no warning that bytes next to the audio were kept unchecked")
	}
}

func TestMovieRobustness(t *testing.T) {
	spec := plainMovie()
	spec.tracks = append(spec.tracks, gpsTrack())
	spec.tracks[0].refs = bx("cdsc", be32(3))
	spec.udta = richUserData()
	spec.moovMeta = phoneMeta()
	spec.moovFirst = true
	coretest.Robustness(t, mp4Handler, spec.build())

	fragmented := cat(defaultFtyp,
		bx("moov", fullBox("mvhd", 0, 0, make([]byte, 96)), bx("mvex", fullBox("trex", 0, 0, be32(1), make([]byte, 16)))),
		bx("moof", fullBox("mfhd", 0, 0, be32(1)), bx("traf", fullBox("tfhd", 0, 0, be32(1)))),
		bx("mdat", []byte("media")))
	coretest.Robustness(t, mp4Handler, fragmented)
}

func TestISO6709(t *testing.T) {
	cases := []struct {
		in       string
		lat, lon string
		ok       bool
		hasAlt   bool
	}{
		{"+48.8584+002.2945/", "48.858400", "2.294500", true, false},
		{"+48.8584+002.2945+035.000/", "48.858400", "2.294500", true, true},
		{"-33.8688+151.2093-010.5/", "-33.868800", "151.209300", true, true},
		{"+4851.504+00217.670/", "48.858400", "2.294500", true, false},
		{"+485130.24+0021740.20/", "48.858400", "2.294500", true, false},
		{"+48.8584+002.2945CRSWGS_84/", "48.858400", "2.294500", true, false},
		{"", "", "", false, false},
		{"48.8584+002.2945/", "", "", false, false},
		{"+48.8584/", "", "", false, false},
		{"+99.0000+002.2945/", "", "", false, false},
		{"+48.8584+999.0000/", "", "", false, false},
		{"+abc+def/", "", "", false, false},
		{"+1+2+3+4/", "", "", false, false},
		{"+/", "", "", false, false},
	}
	for _, c := range cases {
		lat, lon, _, hasAlt, ok := parseISO6709(c.in)
		if ok != c.ok || hasAlt != c.hasAlt {
			t.Errorf("%q: ok=%v hasAlt=%v, want %v %v", c.in, ok, hasAlt, c.ok, c.hasAlt)
			continue
		}
		if !ok {
			continue
		}
		if got := formatCoordinates(lat, lon); got != c.lat+", "+c.lon {
			t.Errorf("%q gave %s", c.in, got)
		}
	}
}

// TestUnparseableLocationStillReported holds for a value in no known form: the
// raw text must reach the user rather than the finding going missing.
func TestUnparseableLocationStillReported(t *testing.T) {
	spec := plainMovie()
	spec.udta = bx("\xa9xyz", qtText("near the Eiffel Tower"))
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !strings.Contains(describeFindings(rep), `GPS coordinates="near the Eiffel Tower"`) {
		t.Errorf("got %s", describeFindings(rep))
	}
}

func TestSubtitleWarning(t *testing.T) {
	spec := plainMovie()
	spec.tracks = append(spec.tracks, testTrack{id: 3, handler: "sbtl", format: "tx3g", samples: [][]byte{[]byte("SUBTITLE")}})
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, "subtitle") }) {
		t.Errorf("no subtitle warning: %q", rep.Warnings)
	}
}

func TestGaplessInfoWarning(t *testing.T) {
	spec := plainMovie()
	spec.udta = fullBox("meta", 0, 0, bx("ilst", bx("----",
		fullBox("mean", 0, 0, []byte("com.apple.iTunes")),
		fullBox("name", 0, 0, []byte("iTunSMPB")),
		bx("data", be32(1), be32(0), []byte(" 00000000 00000840")),
	)))
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, "gapless") }) {
		t.Errorf("no gapless warning: %q", rep.Warnings)
	}
}

// TestFindingsAreBounded holds even for a moov stuffed with distinct tags.
func TestFindingsAreBounded(t *testing.T) {
	var items []byte
	for i := range maxFindings + 50 {
		items = append(items, bx("\xa9nam", qtText("Test Title "+strings.Repeat("x", i%3)+string(rune('a'+i%26))+strconv.Itoa(i)))...)
	}
	spec := plainMovie()
	spec.udta = items
	_, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	if len(rep.Findings) > maxFindings+1 {
		t.Errorf("%d findings is over the cap", len(rep.Findings))
	}
	if !strings.Contains(describeFindings(rep), "Further metadata items") {
		t.Error("the overflow was not reported")
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := plainMovie().build()
	if _, err := mp4Handler.Inspect(ctx, core.BytesSource(data), core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

func TestLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates hundreds of megabytes")
	}
	// A moov over the cap. The bytes must exist for the header to be believed, so
	// this uses a source that pretends to hold them.
	src := &zeroSource{
		head: cat(defaultFtyp, be32(maxLoadedBox+16), []byte("moov")),
		size: int64(len(defaultFtyp)) + maxLoadedBox + 16,
	}
	if _, err := mp4Handler.Inspect(testContext(), src, core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("oversized moov gave %v", err)
	}
}

// zeroSource is a file of the given size that is zero past its head, so a test can
// present a huge file without allocating it.
type zeroSource struct {
	head []byte
	size int64
}

func (z *zeroSource) Size() int64 { return z.size }

func (z *zeroSource) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	if off < int64(len(z.head)) {
		copy(p, z.head[off:])
	}
	return len(p), nil
}

// TestHugeFileStreams proves the promise in the package comment: a file bigger
// than any memory limit, with samples past the 32-bit range, is cleaned without
// the media ever being loaded.
func TestHugeFileStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 5 GiB of zeros")
	}
	const (
		media = int64(5) << 30
		at    = uint64(9) << 29 // 4.5 GiB, which only a co64 can reach
	)
	spec := movieSpec{
		moovFirst: true,
		largeMdat: true,
		times:     fakeTime,
		udta:      richUserData(),
		tracks: []testTrack{{
			id: 1, handler: "vide", co64: true, samples: [][]byte{make([]byte, 1024)},
			badOffset: func(int, uint64) uint64 { return at },
		}},
	}
	built := spec.build()
	end := bytes.Index(built, []byte("mdat")) + 12 // up to the 64-bit size's end
	head := cat(built[:end-8], be64(uint64(media)+16))
	src := &zeroSource{head: head, size: int64(len(head)) + media}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	written := &countingWriter{}
	rep, err := mp4Handler.Clean(testContext(), src, written, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if written.n != src.size {
		t.Errorf("wrote %d bytes of %d", written.n, src.size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Errorf("allocated %d MiB in total for a 5 GiB file", allocated>>20)
	}
	if !strings.Contains(describeFindings(rep), "GPS coordinates") {
		t.Errorf("metadata not found: %s", describeFindings(rep))
	}
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// TestShortLastChunk covers a last chunk shorter than its stsc run says, which
// players accept: what the size table covers stays, the rest is unreferenced.
func TestShortLastChunk(t *testing.T) {
	spec := plainMovie()
	spec.tracks[0].perChunk = 2 // three samples, so the second chunk comes up short
	spec.mdatGap = []byte("Test Orphan Between")
	out, rep := mustClean(t, mp4Handler, spec.build(), core.Options{})
	for _, s := range mediaBytes() {
		if !bytes.Contains(out, s) {
			t.Errorf("lost %q", s)
		}
	}
	if bytes.Contains(out, []byte("Test Orphan")) {
		t.Error("the orphaned bytes survived")
	}
	if !strings.Contains(describeFindings(rep), "Unreferenced media data") {
		t.Errorf("not reported: %s", describeFindings(rep))
	}
}
