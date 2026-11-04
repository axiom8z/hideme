package isobmff

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// countedTables are the kept boxes that are a table with a count. Unlike a fixed
// layout, one that claims more than it holds is not a quirk of an old writer: readers
// stop at the end of the box, so the count is four bytes nothing checks.
var countedTables = typeSet("elst", "stts", "ctts", "stsh", "co64", "stsc", "stss", "stps", "stco", "sbgp",
	"stsz", "stz2", "trun", "subs", "tfra", "sgpd")

// fieldSpan is a field in a box's payload.
type fieldSpan struct{ off, n int64 }

// reservedFields lists what the specifications say must be zero in the kept boxes,
// and that no reader looks at: flags with no defined bits, version bytes of boxes that
// only have one version, and reserved or pre-defined fields.
func reservedFields(typ string, p []byte) []fieldSpan {
	wide := len(p) > 0 && p[0] == 1
	switch typ {
	case "mvhd":
		if wide {
			return []fieldSpan{{1, 3}, {38, 10}, {84, 24}}
		}
		return []fieldSpan{{1, 3}, {26, 10}, {72, 24}}
	case "tkhd":
		// The lowest byte of the flags says whether the track is enabled, and more.
		if wide {
			return []fieldSpan{{1, 2}, {24, 4}, {36, 8}, {50, 2}}
		}
		return []fieldSpan{{1, 2}, {16, 4}, {24, 8}, {38, 2}}
	case "mdhd":
		if wide {
			return []fieldSpan{{1, 3}, {34, 2}}
		}
		return []fieldSpan{{1, 3}, {22, 2}}
	case "smhd":
		return []fieldSpan{{0, 4}, {6, 2}}
	case "hmhd":
		return []fieldSpan{{0, 4}, {16, 4}}
	case "vmhd":
		return []fieldSpan{{0, 3}}
	case "nmhd", "sthd", "stts", "stss", "stsc", "stsz", "stco", "co64", "stsh", "stdp", "padb", "stps", "sdtp", "trex", "mfhd", "mfro":
		return []fieldSpan{{0, 4}}
	case "stz2":
		return []fieldSpan{{0, 7}} // the version, the flags and 24 reserved bits
	case "ctts", "elst", "cslg", "sbgp", "sgpd", "mehd", "tfdt", "clef", "prof", "enof":
		return []fieldSpan{{1, 3}}
	case "tfra":
		return []fieldSpan{{1, 3}, {8, 3}}
	}
	return nil
}

// zeroReserved clears a box's reserved fields. A real file holds zeros there, so
// anything else is worth telling the user about.
func (a *analysis) zeroReserved(m *image, b box, where string) {
	p := m.data[b.body:b.end]
	for _, f := range reservedFields(b.typ, p) {
		if f.off+f.n > int64(len(p)) {
			continue
		}
		if m.zero(b.body+f.off, b.body+f.off+f.n) {
			a.reportReserved(b.typ, where)
		}
	}
	// A colour is only used by the graphics modes that mix with it, and QuickTime
	// writes one anyway, so this is an ordinary value to clear, with nothing to report.
	const graphicsMode, opcolor, ditherCopy = 4, 6, 0x40
	if b.typ == "vmhd" && len(p) >= opcolor+6 && binary.BigEndian.Uint16(p[graphicsMode:]) <= ditherCopy {
		m.zero(b.body+opcolor, b.body+opcolor+6)
	}
}

func (a *analysis) reportReserved(typ, where string) {
	a.remove(core.CatHidden, core.RiskLow, "Data in a reserved field", fmt.Sprintf("%q", typ), where)
}

// scrubVisualFixed clears what no decoder reads in a visual sample entry: the reserved
// bytes, QuickTime's version, revision and quality figures, and its data size. A
// resolution is rewritten to the 72 dpi every writer puts there, rather than cleared,
// because QuickTime says those fields are not optional.
func (a *analysis) scrubVisualFixed(m *image, e box, where string) {
	const (
		reserved, reservedEnd = 8, 14
		version, versionEnd   = 16, 20
		quality, qualityEnd   = 24, 32
		resolution            = 36
		dataSize, dataSizeEnd = 44, 48
		dpi72                 = 0x00480000
	)
	if m.zero(e.start+reserved, e.start+reservedEnd) {
		a.reportReserved(e.typ, where)
	}
	m.zero(e.start+version, e.start+versionEnd)
	m.zero(e.start+quality, e.start+qualityEnd)
	m.zero(e.start+dataSize, e.start+dataSizeEnd)
	for i := range int64(2) {
		field := m.data[e.start+resolution+i*4:][:4]
		if v := binary.BigEndian.Uint32(field); v != 0 && v != dpi72 {
			binary.BigEndian.PutUint32(field, dpi72)
			m.dirty = true
		}
	}
}

// scrubAudioFixed clears what no decoder reads in an audio sample entry. The packet
// size only means something with the compression ID that says so.
func (a *analysis) scrubAudioFixed(m *image, e box, where string) {
	const (
		reserved, reservedEnd = 8, 14
		revision, revisionEnd = 18, 20
		compressionID         = 28
		packetSize, sizeEnd   = 30, 32
	)
	if m.zero(e.start+reserved, e.start+reservedEnd) {
		a.reportReserved(e.typ, where)
	}
	m.zero(e.start+revision, e.start+revisionEnd)
	if binary.BigEndian.Uint16(m.data[e.start+entryVersion:]) == 0 && binary.BigEndian.Uint16(m.data[e.start+compressionID:]) == 0 {
		m.zero(e.start+packetSize, e.start+sizeEnd)
	}
}

// knownBrands are the ftyp and styp brands that say which specification a file follows.
// Readers check for some of them and ignore the rest. Any other is four bytes of
// whatever a writer likes, or the name of a camera maker's format.
var knownBrands = typeSet(
	"isom", "iso2", "iso3", "iso4", "iso5", "iso6", "iso7", "iso8", "iso9", "isoa", "isob", "isoc",
	"mp41", "mp42", "mp71", "avc1", "avc2", "avc3", "avc4", "hvc1", "hev1", "hvc2", "hev2", "vvc1", "vvi1", "av01", "vp08", "vp09",
	"dby1", "dby2", "dts1", "dtsc", "dtse", "dtsh", "dtsl", "dtsx", "opus", "fLaC", "mp4a", "ac-3", "ec-3",
	"M4A ", "M4B ", "M4P ", "M4V ", "M4VH", "M4VP", "mmp4", "mp21", "F4V ", "F4P ", "F4A ", "F4B ", "f4v ", "f4a ",
	"qt  ", "3gp1", "3gp2", "3gp3", "3gp4", "3gp5", "3gp6", "3gp7", "3gp8", "3gp9", "3g2a", "3g2b", "3g2c",
	"3ge6", "3ge7", "3ge9", "3gf9", "3gg6", "3gg9", "3gh9", "3gm9", "3gr6", "3gr9", "3gs6", "3gs7", "3gs9", "3gt8", "3gt9", "3gv6",
	"dash", "dsms", "msdh", "msix", "risx", "sidx", "sims", "lmsg", "ssss", "cmfc", "cmf2", "cmfs", "cmff", "cmfl", "ccff", "ccea",
	"isml", "piff", "ismv", "emsg", "tmf1", "cdm1", "cdm4", "nvr1", "mqt ", "uvvu", "uvvm", "cenc", "cbc1", "cbcs", "cens",
	"heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "heif", "mif1", "mif2", "mif3", "msf1", "miaf",
	"MiHE", "MiHA", "MiHB", "MiPr", "MiAC", "MiCm", "MiCC", "avif", "avis", "MA1A", "MA1B", "avio", "vvic", "vvis",
	"jpeg", "jpgs", "j2ki", "j2is", "jpls", "jxs ", "unif", "uncv",
)

// fileType clears what is spare in an ftyp or styp box: the minor version, which only
// says something to a reader of the brand's own specification and which none consults,
// any brand no specification defines, and bytes that make no whole brand. Size and
// layout stay. A brand is replaced with the major brand, since a list of the same name
// twice means the same as once. An unrecognised major brand has to stay, because it says
// what the file is, and it gets a warning.
func (a *analysis) fileType(b box) error {
	const major, minor, firstCompatible = 4, 8, 8
	size := b.end - b.body
	if size < firstCompatible {
		return a.trimTop(b, 0)
	}
	p := make([]byte, size)
	if err := readFull(a.src, p, b.body); err != nil {
		return err
	}
	out := bytes.Clone(p)
	if brand := string(p[:major]); !knownBrands[brand] {
		a.retain(core.CatDevice, core.RiskLow, "File-type brand", brand, b.typ, "readers use it to tell what kind of file this is")
		a.warnOnce("the file-type brand is not one HideMe recognises. It can name a camera maker's format, or hold anything else, and it was kept because readers need it")
	}
	clear(out[major:minor])
	pos := int64(firstCompatible)
	for ; pos+4 <= size; pos += 4 {
		brand := string(p[pos : pos+4])
		if !knownBrands[brand] && brand != string(p[:major]) {
			a.remove(core.CatDevice, core.RiskLow, "File-type brand", brand, b.typ)
			copy(out[pos:], p[:major])
		}
	}
	clear(out[pos:])
	if !bytes.Equal(out, p) {
		a.patch(core.Patch{Off: b.body, Len: size, Data: out})
	}
	return nil
}

// avcConfigLength and the functions after it return how much of a decoder configuration
// box its own fields account for, or -1 if they cannot tell. Decoders stop there.
func avcConfigLength(p []byte) int64 {
	const header = 5
	c := cursor{b: p}
	c.take(1)
	profile := c.u8()
	c.take(header - 2)
	lengths := func(count int) {
		for range count {
			c.take(int(c.u16()))
		}
	}
	lengths(int(c.u8() & 0x1F))
	lengths(int(c.u8()))
	if c.bad {
		return -1
	}
	// High profiles add chroma format, bit depths and SPS extensions, though many
	// writers leave them off.
	switch profile {
	case 100, 110, 122, 144:
		if c.remaining() >= 4 {
			c.take(3)
			lengths(int(c.u8()))
			if c.bad {
				return -1
			}
		}
	}
	return int64(c.pos)
}

func hevcConfigLength(p []byte) int64 {
	const fixed = 22
	c := cursor{b: p}
	c.take(fixed)
	arrays := int(c.u8())
	for range arrays {
		c.take(1)
		units := int(c.u16())
		for range units {
			c.take(int(c.u16()))
		}
		if c.bad {
			return -1
		}
	}
	if c.bad {
		return -1
	}
	return int64(c.pos)
}

// av1ConfigLength keeps the OBUs AV1's configuration record allows: a sequence header
// and metadata, each stating its own size, as the record's specification requires.
func av1ConfigLength(p []byte) int64 {
	const fixed, sequenceHeader, metadata = 4, 1, 5
	if len(p) < fixed || p[0] != 0x81 {
		return -1
	}
	pos := int64(fixed)
	for pos < int64(len(p)) {
		obu := p[pos]
		kind, extension, sized := obu>>3&0x0F, obu>>2&1, obu>>1&1
		if (kind != sequenceHeader && kind != metadata) || sized == 0 {
			break
		}
		next := pos + 1 + int64(extension)
		var size int64
		ok := false
		for shift := 0; shift < 56 && next < int64(len(p)); shift += 7 {
			v := p[next]
			next++
			size |= int64(v&0x7F) << shift
			if v&0x80 == 0 {
				ok = true
				break
			}
		}
		if !ok || size > int64(len(p))-next {
			break
		}
		pos = next + size
	}
	return pos
}

// descriptorHeader reads an MPEG-4 descriptor's tag and size. A size can take up to four
// bytes, with the top bit of each saying another follows.
func descriptorHeader(p []byte, pos int) (tag byte, size, next int, ok bool) {
	if pos >= len(p) {
		return 0, 0, 0, false
	}
	tag = p[pos]
	pos++
	for i := 0; i < 4; i++ {
		if pos >= len(p) {
			return 0, 0, 0, false
		}
		v := p[pos]
		pos++
		size = size<<7 | int(v&0x7F)
		if v&0x80 == 0 {
			return tag, size, pos, true
		}
	}
	return 0, 0, 0, false
}

// esdsLength reads the size of the elementary stream descriptor that makes up an esds
// box, after the box's version and flags. Old writers are known to get this size wrong
// and readers look past it, so it counts only if the descriptor it bounds is whole,
// with the decoder configuration that is the point of it inside.
func esdsLength(p []byte) int64 {
	const fixed, descriptor, decoderConfig, esHeader = 4, 3, 4, 3
	tag, size, pos, ok := descriptorHeader(p, fixed)
	if !ok || tag != descriptor || size > len(p)-pos || size < esHeader {
		return -1
	}
	end := pos + size
	for child := pos + esHeader; child < end; {
		tag, size, body, ok := descriptorHeader(p[:end], child)
		if !ok || size > end-body {
			return -1
		}
		if tag == decoderConfig {
			return int64(end)
		}
		child = body + size
	}
	return -1
}

// groupEntrySize gives the size of one entry of a sample group description, for the
// kinds whose entries have one. Other kinds can only be sized by their own lengths.
var groupEntrySize = map[string]int64{"roll": 2, "prol": 2, "rap ": 1, "sync": 1, "tele": 1, "stsa": 0, "tsas": 0}

func sampleGroupLength(p []byte) int64 {
	c := cursor{b: p}
	version := c.u8()
	c.take(3)
	kind := string(c.take(4))
	var defaultLength uint32
	switch {
	case version == 1:
		defaultLength = c.u32()
	case version >= 2:
		c.take(4)
	}
	count := int64(c.u32())
	if c.bad {
		return -1
	}
	head := int64(c.pos)
	if version != 1 || defaultLength != 0 {
		size := int64(defaultLength)
		if version != 1 {
			known, ok := groupEntrySize[kind]
			if !ok {
				return -1
			}
			size = known
		}
		return head + count*size
	}
	if count > int64(c.remaining()) {
		return int64(len(p)) + 1
	}
	for range count {
		c.take(int(c.u32()))
		if c.bad {
			return int64(len(p)) + 1
		}
	}
	return int64(c.pos)
}

func subSampleLength(p []byte) int64 {
	c := cursor{b: p}
	version := c.u8()
	c.take(3)
	count := int64(c.u32())
	if c.bad {
		return -1
	}
	size := 8
	if version == 1 {
		size = 10
	}
	if count > int64(c.remaining()) {
		return int64(len(p)) + 1
	}
	for range count {
		c.take(4)
		c.take(int(c.u16()) * size)
		if c.bad {
			return int64(len(p)) + 1
		}
	}
	return int64(c.pos)
}

func fragmentIndexLength(p []byte) int64 {
	const fixed = 16
	if len(p) < fixed {
		return -1
	}
	sizes := binary.BigEndian.Uint32(p[8:])
	entry := int64(sizes>>4&3) + int64(sizes>>2&3) + int64(sizes&3) + 3 + 8
	if p[0] == 1 {
		entry += 8
	}
	return fixed + int64(binary.BigEndian.Uint32(p[12:]))*entry
}

// checkChunkRuns points any run of chunks at the track's first description if the one
// it names does not exist, which is how readers treat it, and which leaves no room in
// that field in any run.
func (a *analysis) checkChunkRuns(m *image, b, stbl box, where string) error {
	const entries, entrySize, index = 8, 12, 8
	descriptions := uint32(0)
	if _, err := a.walk(m.data, stbl.body, stbl.end, func(c box) error {
		if c.typ == "stsd" && c.end-c.body >= 8 {
			descriptions = binary.BigEndian.Uint32(m.data[c.body+4:])
		}
		return nil
	}); err != nil {
		return err
	}
	if err := a.trimKept(m, b, where); err != nil {
		return err
	}
	if b.end-b.body < entries {
		return nil
	}
	count := int64(binary.BigEndian.Uint32(m.data[b.body+4:]))
	for i := range count {
		field := m.data[b.body+entries+i*entrySize+index:][:4]
		if v := binary.BigEndian.Uint32(field); v == 0 || v > descriptions {
			binary.BigEndian.PutUint32(field, 1)
			m.dirty = true
		}
	}
	return nil
}

// checkSampleNumbers cuts a table of sample numbers at the first number that is out of
// order or past the last sample. Players consult it only to seek, and a number no
// sample has only gives them nowhere to go, so it is no loss, and it was the one kind
// of bytes this table could hold that nothing would check.
func (a *analysis) checkSampleNumbers(m *image, b box, t *track, where string) error {
	if err := a.trimKept(m, b, where); err != nil {
		return err
	}
	const entries = 8
	if b.end-b.body < entries {
		return nil
	}
	count := int64(binary.BigEndian.Uint32(m.data[b.body+4:]))
	table := m.data[b.body+entries : b.body+entries+count*4]
	var previous uint64
	valid := int64(0)
	for ; valid < count; valid++ {
		n := uint64(binary.BigEndian.Uint32(table[valid*4:]))
		if n <= previous || n > t.sampleCount {
			break
		}
		previous = n
	}
	if valid < count {
		binary.BigEndian.PutUint32(m.data[b.body+4:], uint32(valid))
		clear(table[valid*4:])
		m.dirty = true
		a.reportPastEnd(b, (count-valid)*4, where)
	}
	return nil
}

// sampleGroupKinds are the kinds of sample group that specifications define. A group of
// any other kind is a table no reader can use.
var sampleGroupKinds = typeSet("roll", "prol", "alst", "rap ", "tele", "sync", "tsas", "stsa", "sap ", "rash", "seig", "rror",
	"oinf", "linf", "trif", "nalm", "tscl", "stmi", "sulm", "vipr", "dtrt", "scnm", "mvif", "scif", "avll", "avss", "msbf", "opin",
	"vmeg", "lvlm", "dfnd")

// editSampleGroup keeps a sample group or its description only if its kind, which
// follows the version and flags in both, is one that means something.
func (a *analysis) editSampleGroup(m *image, b box, where string) error {
	const kindAt = 4
	p := m.data[b.body:b.end]
	if len(p) < kindAt+4 || !sampleGroupKinds[string(p[kindAt:kindAt+4])] {
		a.removeNested(m, b, where)
		return nil
	}
	return a.trimKept(m, b, where)
}

// levelAssignmentLength walks a leva box's levels, whose sizes depend on how each one
// assigns its samples.
func levelAssignmentLength(p []byte) int64 {
	c := cursor{b: p}
	c.take(4)
	levels := int(c.u8())
	for range levels {
		c.take(4)
		switch c.u8() & 0x7F {
		case 0, 4:
			c.take(4)
		case 1:
			c.take(8)
		case 2, 3:
		default:
			return -1
		}
		if c.bad {
			return -1
		}
	}
	if c.bad {
		return -1
	}
	return int64(c.pos)
}

// trimToSamples zeroes what follows a table with one entry per sample, which is told
// only by the sample table, and not by the box.
func (a *analysis) trimToSamples(m *image, b box, t *track, where string) error {
	p := m.data[b.body:b.end]
	var length int64
	switch b.typ {
	case "sdtp":
		length = 4 + int64(t.sampleCount)
	case "stdp":
		length = 4 + 2*int64(t.sampleCount)
	case "padb":
		length = 8 + (int64(t.sampleCount)+1)/2
	}
	if length < int64(len(p)) && m.zero(b.body+length, b.end) {
		a.reportPastEnd(b, int64(len(p))-length, where)
	}
	a.zeroReserved(m, b, where)
	return nil
}
