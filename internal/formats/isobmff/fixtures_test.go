package isobmff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

func bx(typ string, parts ...[]byte) []byte {
	return appendBox(nil, typ, parts...)
}

func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	head := binary.BigEndian.AppendUint32(nil, uint32(version)<<24|flags)
	return bx(typ, append([][]byte{head}, parts...)...)
}

// largeBox writes a box with a 64-bit size field.
func largeBox(typ string, payload []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, 1)
	out = append(out, typ...)
	out = binary.BigEndian.AppendUint64(out, uint64(len(payload))+16)
	return append(out, payload...)
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// qtText writes a QuickTime international text atom's payload.
func qtText(s string) []byte {
	return cat(be16(uint16(len(s))), be16(0x15C7), []byte(s))
}

// ilstItem writes an iTunes-style item holding one data box.
func ilstItem(typ string, dataType uint32, value []byte) []byte {
	return bx(typ, bx("data", be32(dataType), be32(0), value))
}

// fakeTime is 2001-02-03 04:05:06 UTC in the 1904 epoch.
const fakeTime = 3064017906

// testTrack describes one track of a fixture movie. Samples are laid out one
// track after another in the mdat, in samplesPerChunk groups.
type testTrack struct {
	id        uint32
	handler   string
	format    string
	samples   [][]byte
	perChunk  uint32 // defaults to 1
	co64      bool
	stz2Bits  uint8  // use stz2 with this field size instead of stsz
	constSize uint32 // write a constant-size stsz with this size
	entry     []byte // whole sample entry, replacing the default
	refs      []byte // tref payload
	udta      []byte // trak/udta payload
	trakExtra []byte // further whole boxes in the trak
	minfExtra []byte // further whole boxes in the minf
	badOffset func(i int, off uint64) uint64
}

// sizes the stsc, stsz and offset tables; base is where its first sample goes.
func (t testTrack) sampleTable(base uint64) []byte {
	perChunk := max(t.perChunk, 1)
	var offsets [][]byte
	pos := base
	chunks := 0
	for i := 0; i < len(t.samples); i += int(perChunk) {
		off := pos
		if t.badOffset != nil {
			off = t.badOffset(chunks, pos)
		}
		if t.co64 {
			offsets = append(offsets, be64(off))
		} else {
			offsets = append(offsets, be32(uint32(off)))
		}
		for j := i; j < min(i+int(perChunk), len(t.samples)); j++ {
			pos += uint64(len(t.samples[j]))
		}
		chunks++
	}
	var chunkTable []byte
	if t.co64 {
		chunkTable = fullBox("co64", 0, 0, be32(uint32(chunks)), cat(offsets...))
	} else {
		chunkTable = fullBox("stco", 0, 0, be32(uint32(chunks)), cat(offsets...))
	}
	var sizeTable []byte
	switch {
	case t.constSize != 0:
		sizeTable = fullBox("stsz", 0, 0, be32(t.constSize), be32(uint32(len(t.samples))))
	case t.stz2Bits != 0:
		var table []byte
		switch t.stz2Bits {
		case 4:
			for i := 0; i < len(t.samples); i += 2 {
				hi := len(t.samples[i])
				lo := 0
				if i+1 < len(t.samples) {
					lo = len(t.samples[i+1])
				}
				table = append(table, byte(hi<<4|lo))
			}
		case 8:
			for _, s := range t.samples {
				table = append(table, byte(len(s)))
			}
		case 16:
			for _, s := range t.samples {
				table = append(table, be16(uint16(len(s)))...)
			}
		}
		sizeTable = fullBox("stz2", 0, 0, []byte{0, 0, 0, t.stz2Bits}, be32(uint32(len(t.samples))), table)
	default:
		var table []byte
		for _, s := range t.samples {
			table = append(table, be32(uint32(len(s)))...)
		}
		sizeTable = fullBox("stsz", 0, 0, be32(0), be32(uint32(len(t.samples))), table)
	}
	stsc := fullBox("stsc", 0, 0, be32(1), be32(1), be32(perChunk), be32(1))
	stts := fullBox("stts", 0, 0, be32(1), be32(uint32(len(t.samples))), be32(1))
	return cat(t.descriptions(), stts, stsc, sizeTable, chunkTable)
}

func (t testTrack) descriptions() []byte {
	entry := t.entry
	if entry == nil {
		switch t.handler {
		case "vide":
			entry = visualEntry("avc1", "GoPro AVC encoder", bx("avcC", []byte{1, 2, 3, 4}))
		case "soun":
			entry = audioEntry("mp4a", 0, nil, bx("esds", []byte{0, 0, 0, 0, 3, 0}))
		default:
			entry = bx(t.format, make([]byte, 8))
		}
	}
	return fullBox("stsd", 0, 0, be32(1), entry)
}

// visualEntry writes a visual sample entry. A vendor code and compressor name go
// in the fixed fields, where QuickTime writers really put them.
func visualEntry(format, compressor string, children ...[]byte) []byte {
	fixed := make([]byte, visualFixed-8)
	copy(fixed[entryVendor-8:], "Tven")
	fixed[visualCompName-8] = byte(len(compressor))
	copy(fixed[visualCompName-8+1:], compressor)
	return bx(format, append([][]byte{fixed}, children...)...)
}

// audioEntry writes an audio sample entry of the given QuickTime version.
func audioEntry(format string, version uint16, extra []byte, children ...[]byte) []byte {
	fixed := make([]byte, audioFixed-8)
	binary.BigEndian.PutUint16(fixed[entryVersion-8:], version)
	copy(fixed[entryVendor-8:], "Tven")
	parts := [][]byte{fixed, extra}
	return bx(format, append(parts, children...)...)
}

func (t testTrack) trak(base uint64, times uint32) []byte {
	var parts [][]byte
	parts = append(parts, fullBox("tkhd", 0, 7, be32(times), be32(times), be32(t.id), make([]byte, 68)))
	if t.refs != nil {
		parts = append(parts, bx("tref", t.refs))
	}
	if t.udta != nil {
		parts = append(parts, bx("udta", t.udta))
	}
	if t.trakExtra != nil {
		parts = append(parts, t.trakExtra)
	}
	hdlr := fullBox("hdlr", 0, 0, []byte("mhlr"), []byte(t.handler), []byte("appl"), make([]byte, 8), []byte("Test Handler Name\x00"))
	mdhd := fullBox("mdhd", 0, 0, be32(times), be32(times), be32(600), be32(100), be16(0x55C4), be16(0))
	minf := bx("minf",
		fullBox("nmhd", 0, 0),
		t.minfExtra,
		bx("dinf", fullBox("dref", 0, 0, be32(1), fullBox("url ", 0, 1))),
		bx("stbl", t.sampleTable(base)),
	)
	parts = append(parts, bx("mdia", mdhd, hdlr, minf))
	return bx("trak", parts...)
}

// movieSpec describes a fixture movie.
type movieSpec struct {
	ftyp       []byte // nil for the default; empty for none
	before     []byte // top-level boxes between ftyp and the first big box
	moovFirst  bool
	largeMdat  bool
	tracks     []testTrack
	udta       []byte // moov/udta payload
	moovMeta   []byte // moov/meta payload
	moovExtra  []byte // further boxes in moov
	mdatPrefix []byte // orphaned bytes before the first sample
	mdatGap    []byte // orphaned bytes between tracks
	mdatSuffix []byte // orphaned bytes after the last sample
	after      []byte // appended verbatim after the last box
	times      uint32
}

var defaultFtyp = bx("ftyp", []byte("isom"), be32(512), []byte("isomiso2avc1mp41"))

// build lays the movie out. The moov's size does not depend on the offsets in
// it, so building twice gives the real offsets.
func (s movieSpec) build() []byte {
	out, _ := s.layout(0)
	_, mdatStart := s.layout(0)
	out, _ = s.layout(mdatStart)
	return out
}

func (s movieSpec) layout(mdatPayload uint64) ([]byte, uint64) {
	ftyp := s.ftyp
	if ftyp == nil {
		ftyp = defaultFtyp
	}
	var media []byte
	media = append(media, s.mdatPrefix...)
	var traks [][]byte
	for i, t := range s.tracks {
		traks = append(traks, t.trak(mdatPayload+uint64(len(media)), s.times))
		for _, sample := range t.samples {
			media = append(media, sample...)
		}
		if i+1 < len(s.tracks) {
			media = append(media, s.mdatGap...)
		}
	}
	media = append(media, s.mdatSuffix...)

	moovParts := [][]byte{fullBox("mvhd", 0, 0, be32(s.times), be32(s.times), be32(600), be32(100), make([]byte, 80))}
	moovParts = append(moovParts, traks...)
	if s.udta != nil {
		moovParts = append(moovParts, bx("udta", s.udta))
	}
	if s.moovMeta != nil {
		moovParts = append(moovParts, bx("meta", s.moovMeta))
	}
	if s.moovExtra != nil {
		moovParts = append(moovParts, s.moovExtra)
	}
	moov := bx("moov", moovParts...)

	var mdat []byte
	var mdatHeader int
	if s.largeMdat {
		mdat, mdatHeader = largeBox("mdat", media), 16
	} else {
		mdat, mdatHeader = bx("mdat", media), 8
	}
	if s.moovFirst {
		start := len(ftyp) + len(s.before) + len(moov) + mdatHeader
		return cat(ftyp, s.before, moov, mdat, s.after), uint64(start)
	}
	start := len(ftyp) + len(s.before) + mdatHeader
	return cat(ftyp, s.before, mdat, moov, s.after), uint64(start)
}

var (
	videoSamples = [][]byte{[]byte("VIDEO SAMPLE ONE"), []byte("VIDEO SAMPLE TWO!"), []byte("VIDEO SAMPLE THREE")}
	audioSamples = [][]byte{[]byte("AUDIO SAMPLE ONE"), []byte("AUDIO SAMPLE TWO")}
	gpsSamples   = [][]byte{[]byte("GPS LOG 48.8584 2.2945"), []byte("GPS LOG 48.8585 2.2946")}
)

func videoTrack() testTrack {
	return testTrack{id: 1, handler: "vide", samples: videoSamples}
}

func audioTrack() testTrack {
	return testTrack{id: 2, handler: "soun", samples: audioSamples}
}

func gpsTrack() testTrack {
	return testTrack{id: 3, handler: "meta", format: "mebx", samples: gpsSamples}
}

func plainMovie() movieSpec {
	return movieSpec{tracks: []testTrack{videoTrack(), audioTrack()}, times: fakeTime}
}

// richUserData is a udta in the style of ffmpeg, cameras and GoPro.
func richUserData() []byte {
	return cat(
		bx("\xa9xyz", qtText("+48.8584+002.2945+035.000/")),
		bx("\xa9mak", qtText("Test Make")),
		bx("\xa9mod", qtText("Test Model")),
		bx("\xa9swr", qtText("Test Software")),
		bx("\xa9day", qtText("2001-02-03")),
		bx("\xa9nam", qtText("Test Title")),
		bx("\xa9ART", qtText("Test Author")),
		bx("\xa9too", qtText("Test Encoder")),
		bx("GPMF", []byte("Test GoPro Settings")),
		bx("FIRM", []byte("Test Firmware")),
		bx("VEND", []byte("Test Vendor Box")),
	)
}

// phoneMeta is a moov/meta in the iPhone style, with a keys table.
func phoneMeta() []byte {
	key := func(s string) []byte { return cat(be32(uint32(len(s)+8)), []byte("mdta"), []byte(s)) }
	keys := fullBox("keys", 0, 0, be32(5),
		key("com.apple.quicktime.location.ISO6709"),
		key("com.apple.quicktime.make"),
		key("com.apple.quicktime.model"),
		key("com.apple.quicktime.software"),
		key("com.apple.quicktime.creationdate"),
	)
	item := func(index uint32, value string) []byte {
		return bx(string(be32(index)), bx("data", be32(1), be32(0), []byte(value)))
	}
	ilst := bx("ilst",
		item(1, "+48.8584+002.2945+035.000/"),
		item(2, "Test Make"),
		item(3, "Test Model"),
		item(4, "Test Software"),
		item(5, "2001-02-03T04:05:06+0100"),
	)
	// QuickTime writes meta as a plain box, with hdlr straight after the header.
	hdlr := fullBox("hdlr", 0, 0, be32(0), []byte("mdta"), make([]byte, 12), []byte{0})
	return cat(hdlr, keys, ilst)
}

// iTunesUserData is a udta holding an iTunes tag list, the M4A layout.
func iTunesUserData() []byte {
	meta := fullBox("meta", 0, 0,
		fullBox("hdlr", 0, 0, be32(0), []byte("mdir"), []byte("appl"), make([]byte, 8), []byte{0}),
		bx("ilst",
			ilstItem("\xa9nam", 1, []byte("Test Title")),
			ilstItem("\xa9ART", 1, []byte("Test Author")),
			ilstItem("\xa9alb", 1, []byte("Test Album")),
			ilstItem("\xa9day", 1, []byte("2001")),
			ilstItem("apID", 1, []byte("test@example.com")),
			ilstItem("covr", 13, []byte("\xFF\xD8Test Cover Art\xFF\xD9")),
			bx("----",
				fullBox("mean", 0, 0, []byte("com.apple.iTunes")),
				fullBox("name", 0, 0, []byte("Test Custom Tag")),
				bx("data", be32(1), be32(0), []byte("Test Custom Value")),
			),
		),
	)
	return meta
}

// itemSpec is one item in a fixture HEIF file.
type itemSpec struct {
	id      uint32
	typ     string
	name    string
	content string // for mime items
	data    []byte // stored in the mdat or, with inIdat, in idat
	inIdat  bool
	hidden  bool
	props   []uint16 // 1-based property indexes
}

// heifSpec describes a fixture HEIF file.
type heifSpec struct {
	brand       string
	ilocVersion byte
	offsetSize  int
	lengthSize  int
	baseSize    int
	indexSize   int
	items       []itemSpec
	primary     uint32
	refs        []itemRef
	properties  [][]byte
	mdatSuffix  []byte // orphaned bytes
	idatSuffix  []byte // orphaned bytes
	metaExtra   []byte // further children of meta
	before      []byte
	after       []byte
	moov        []byte
	largeMeta   bool
	wideIDs     bool     // the box versions with 32-bit item IDs and 16-bit property indexes
	iinfVersion byte     // when not zero, this iinf version with its 32-bit item count
	extraInfes  [][]byte // further whole infe boxes
	pitmTail    []byte   // bytes after the primary item ID
}

func (s heifSpec) build() []byte {
	out, start := s.layout(0)
	out, _ = s.layout(start)
	return out
}

// layout returns the file and where the mdat's payload begins.
func (s heifSpec) layout(mdatPayload uint64) ([]byte, uint64) {
	brand := s.brand
	if brand == "" {
		brand = "heic"
	}
	ftyp := bx("ftyp", []byte(brand), be32(0), []byte("mif1"), []byte(brand))

	var mdat, idat []byte
	var ilocEntries []byte
	count := 0
	for _, it := range s.items {
		if it.data == nil {
			continue
		}
		count++
		if s.ilocVersion == 2 {
			ilocEntries = append(ilocEntries, be32(it.id)...)
		} else {
			ilocEntries = append(ilocEntries, be16(uint16(it.id))...)
		}
		if s.ilocVersion >= 1 {
			method := uint16(0)
			if it.inIdat {
				method = 1
			}
			ilocEntries = append(ilocEntries, be16(method)...)
		}
		ilocEntries = append(ilocEntries, be16(0)...) // data reference index

		var offset uint64
		if it.inIdat {
			offset = uint64(len(idat))
			idat = append(idat, it.data...)
		} else {
			offset = mdatPayload + uint64(len(mdat))
			mdat = append(mdat, it.data...)
		}
		// With no offset field, the base carries the whole position.
		base := uint64(0)
		if s.offsetSize == 0 {
			base, offset = offset, 0
		}
		ilocEntries = appendSized(ilocEntries, base, s.baseSize)
		ilocEntries = append(ilocEntries, be16(1)...)
		ilocEntries = appendSized(ilocEntries, 1, s.indexSize)
		ilocEntries = appendSized(ilocEntries, offset, s.offsetSize)
		ilocEntries = appendSized(ilocEntries, uint64(len(it.data)), s.lengthSize)
	}
	mdat = append(mdat, s.mdatSuffix...)
	idat = append(idat, s.idatSuffix...)

	sizes := be16(uint16(s.offsetSize)<<12 | uint16(s.lengthSize)<<8 | uint16(s.baseSize)<<4 | uint16(s.indexSize))
	var ilocCount []byte
	if s.ilocVersion == 2 {
		ilocCount = be32(uint32(count))
	} else {
		ilocCount = be16(uint16(count))
	}
	iloc := fullBox("iloc", s.ilocVersion, 0, sizes, ilocCount, ilocEntries)

	var infes []byte
	for _, it := range s.items {
		var flags uint32
		if it.hidden {
			flags = 1
		}
		id, version := be16(uint16(it.id)), byte(2)
		if s.wideIDs {
			id, version = be32(it.id), 3
		}
		body := cat(id, be16(0), []byte(it.typ), []byte(it.name), []byte{0})
		if it.typ == "mime" {
			body = cat(body, []byte(it.content), []byte{0})
		}
		infes = append(infes, fullBox("infe", version, flags, body)...)
	}
	infes = append(infes, cat(s.extraInfes...)...)
	infeCount := len(s.items) + len(s.extraInfes)
	iinf := fullBox("iinf", 0, 0, be16(uint16(infeCount)), infes)
	if s.wideIDs {
		iinf = fullBox("iinf", 1, 0, be32(uint32(infeCount)), infes)
	}
	if s.iinfVersion != 0 {
		iinf = fullBox("iinf", s.iinfVersion, 0, be32(uint32(infeCount)), infes)
	}

	itemID := func(id uint32) []byte {
		if s.wideIDs {
			return be32(id)
		}
		return be16(uint16(id))
	}
	var refs []byte
	for _, r := range s.refs {
		to := be16(uint16(len(r.to)))
		for _, id := range r.to {
			to = append(to, itemID(id)...)
		}
		refs = append(refs, bx(r.typ, itemID(r.from), to)...)
	}

	var ipma []byte
	entries := 0
	for _, it := range s.items {
		if len(it.props) == 0 {
			continue
		}
		entries++
		ipma = append(ipma, itemID(it.id)...)
		ipma = append(ipma, byte(len(it.props)))
		for _, p := range it.props {
			if s.wideIDs {
				ipma = append(ipma, be16(p)...)
			} else {
				ipma = append(ipma, byte(p))
			}
		}
	}
	ipmaBox := fullBox("ipma", 0, 0, be32(uint32(entries)), ipma)
	if s.wideIDs {
		ipmaBox = fullBox("ipma", 1, 1, be32(uint32(entries)), ipma)
	}
	iprp := bx("iprp", bx("ipco", s.properties...), ipmaBox)

	children := [][]byte{
		fullBox("hdlr", 0, 0, be32(0), []byte("pict"), make([]byte, 12), []byte("Test Handler Name\x00")),
		pitmBox(s.primary, s.wideIDs, s.pitmTail),
		iloc,
		iinf,
	}
	if refs != nil {
		version := byte(0)
		if s.wideIDs {
			version = 1
		}
		children = append(children, fullBox("iref", version, 0, refs))
	}
	children = append(children, iprp)
	if idat != nil {
		children = append(children, bx("idat", idat))
	}
	if s.metaExtra != nil {
		children = append(children, s.metaExtra)
	}
	metaBody := append(be32(0), cat(children...)...)
	var meta []byte
	var mdatStart int
	if s.largeMeta {
		meta = largeBox("meta", metaBody)
	} else {
		meta = bx("meta", metaBody)
	}
	mdatStart = len(ftyp) + len(s.before) + len(meta) + 8
	return cat(ftyp, s.before, meta, bx("mdat", mdat), s.moov, s.after), uint64(mdatStart)
}

func pitmBox(primary uint32, wide bool, tail []byte) []byte {
	if wide {
		return fullBox("pitm", 1, 0, be32(primary), tail)
	}
	return fullBox("pitm", 0, 0, be16(uint16(primary)), tail)
}

func appendSized(dst []byte, v uint64, size int) []byte {
	switch size {
	case 4:
		return append(dst, be32(uint32(v))...)
	case 8:
		return append(dst, be64(v)...)
	}
	return dst
}

// exifItem wraps a TIFF block the way Apple does: a 4-byte offset of 6, then the
// "Exif\0\0" signature, then the block.
func exifItem(tiff []byte) []byte {
	return cat(be32(6), []byte("Exif\x00\x00"), tiff)
}

var (
	primaryData   = []byte("PRIMARY IMAGE CODED DATA")
	thumbnailData = []byte("THUMBNAIL IMAGE SECRET CROP")
	auxData       = []byte("AUXILIARY DEPTH MAP DATA")
)

func coloursNCLX() []byte {
	return bx("colr", []byte("nclx"), []byte{0, 1, 0, 13, 0, 1, 0x80})
}

func coloursICC() []byte {
	return bx("colr", []byte("prof"), []byte("Test ICC Profile Bytes"))
}

// standardProperties are the ipco children most fixtures use: 1 hvcC, 2 ispe,
// 3 irot, 4 nclx colour, 5 ICC colour, 6 thumbnail hvcC, 7 auxC.
func standardProperties() [][]byte {
	return [][]byte{
		bx("hvcC", []byte("PRIMARY DECODER CONFIG")),
		fullBox("ispe", 0, 0, be32(4032), be32(3024)),
		bx("irot", []byte{1}),
		coloursNCLX(),
		coloursICC(),
		bx("hvcC", []byte("THUMB DECODER CONFIG")),
		fullBox("auxC", 0, 0, []byte("urn:mpeg:hevc:2015:auxid:2\x00")),
	}
}

// mustClean is for fixtures that are expected to clean.
func mustClean(t *testing.T, h core.Handler, data []byte, opts core.Options) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := h.Clean(testContext(), core.BytesSource(data), &out, opts)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func describeFindings(rep *core.Report) string {
	var b bytes.Buffer
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "%s=%q(%s) ", f.Field, f.Value, f.Action)
	}
	return b.String()
}
