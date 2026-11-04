package isobmff

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

// iccReason explains retaining a colour profile, here and in HEIF properties.
const iccReason = "needed to show the colours correctly; strict mode removes it"

// What stays in each container of a movie. Any other child is freed and reported,
// apart from free space, which stays with a zeroed payload, and children that have
// an editor of their own.
var (
	keepInMvex = typeSet("mehd", "trex", "leva")
	// Edit lists, apertures and old QuickTime layout boxes all change how the track
	// plays and say nothing about the person.
	keepInTrack = typeSet("edts", "tapt", "load", "clip", "matt", "txas")
	keepInMedia = typeSet("elng")
	keepInInfo  = typeSet("vmhd", "smhd", "hmhd", "nmhd", "sthd", "gmhd")
	// saiz, saio and senc are left off on purpose. They locate auxiliary data that
	// is only needed for encryption, and encrypted tracks are refused.
	keepInTable = typeSet("stts", "ctts", "cslg", "stsc", "stsz", "stz2", "stco", "co64",
		"stss", "stsh", "stdp", "sdtp", "sbgp", "sgpd", "subs", "padb", "stps")
	keepInFragment = typeSet("mfhd")
	keepInTraf     = typeSet("tfhd", "tfdt", "trun", "sbgp", "sgpd", "subs")
	keepInIndex    = typeSet("tfra", "mfro")

	// keptChildren lists what stays inside the kept boxes that are containers. These
	// need an allowlist of their own: ffmpeg walks into an edts as it does a moov,
	// and reports a udta it finds there as the track's metadata.
	keptChildren = map[string]map[string]bool{
		"edts": typeSet("elst"),
		"tapt": typeSet("clef", "prof", "enof"),
		"clip": typeSet("crgn"),
		"matt": typeSet("kmat"),
		"gmhd": typeSet("gmin", "text"),
	}
)

// movie edits a moov box. Tracks are classified before anything is edited, since
// a kept track's references depend on which other tracks are going away.
func (a *analysis) movie(top box) error {
	m, err := a.load(top)
	if err != nil {
		return err
	}
	var traks []box
	err = a.editChildren(m, top.body-top.start, int64(len(m.data)), "moov", editor{
		edit: map[string]func(box) error{
			"mvhd": func(b box) error { return a.scrubTimes(m, b, "moov/mvhd") },
			"trak": func(b box) error { traks = append(traks, b); return nil },
			"mvex": func(b box) error {
				a.fragmented = true
				return a.editChildren(m, b.body, b.end, "moov/mvex", editor{keep: keepInMvex})
			},
			"cmov": func(box) error { return core.Unsupportedf("isobmff: compressed QuickTime movie header") },
			"rmra": func(box) error { return core.Unsupportedf("isobmff: QuickTime reference movie") },
			"pssh": func(box) error { return fmt.Errorf("%w: protection data in the movie header", core.ErrEncrypted) },
		},
	})
	if err != nil {
		return err
	}

	tracks := make([]*track, 0, len(traks))
	going := make(map[uint32]bool) // each track's ID, and whether it is being removed
	for _, b := range traks {
		t, err := a.parseTrack(m, b)
		if err != nil {
			return err
		}
		tracks = append(tracks, t)
		t.position = len(tracks)
		going[t.id] = t.removed()
		if !t.removed() {
			continue
		}
		// Fragments locate samples of their own, in trafs that name the track. An
		// empty sample table here says nothing about those, so the track's samples
		// cannot be found without resolving every fragment, and freeing only the
		// trak would leave the GPS log in the file.
		if a.fragmented {
			return core.Unsupportedf("isobmff: removing a %q track from a fragmented file", t.handler)
		}
	}
	for _, t := range tracks {
		if t.removed() {
			a.removeTrack(m, t)
			continue
		}
		if err := a.editTrack(m, t, going); err != nil {
			return err
		}
	}
	return a.commit(m)
}

// scrubTimes zeroes the creation and modification times in an mvhd, tkhd or mdhd
// box, whose first fields are laid out the same way in all three.
func (a *analysis) scrubTimes(m *image, b box, where string) error {
	p := m.data[b.body:b.end]
	if len(p) < 4 {
		return core.Malformedf("%s box of %d bytes is too short", b.typ, b.size())
	}
	var width int64
	switch version := p[0]; version {
	case 0:
		width = 4
	case 1:
		width = 8
	default:
		return core.Unsupportedf("isobmff: %s box version %d", b.typ, version)
	}
	if int64(len(p)) < 4+2*width {
		return core.Malformedf("%s box of %d bytes is too short", b.typ, b.size())
	}
	fields := [2]string{"Capture time", "Modification time"}
	for i, field := range fields {
		raw := p[4+int64(i)*width:][:width]
		if seconds := bigEndian(raw); seconds != 0 {
			a.remove(core.CatTime, core.RiskMed, field, movieTime(seconds), where)
		}
	}
	m.zero(b.body+4, b.body+4+2*width)
	return a.trimKept(m, b, where)
}

func bigEndian(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// maxMovieSeconds is about the year 9999 in the 1904 epoch. A larger value is
// nonsense, which is shown as it is rather than overflowing time.Duration.
const maxMovieSeconds = 8099 * 365 * 24 * 3600

// movieTime decodes a timestamp counted from 1904, the QuickTime epoch.
func movieTime(seconds uint64) string {
	if seconds > maxMovieSeconds {
		return fmt.Sprintf("%d seconds after 1904", seconds)
	}
	epoch := time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	days := seconds / 86400
	return epoch.AddDate(0, 0, int(days)).Add(time.Duration(seconds%86400) * time.Second).Format("2006-01-02 15:04:05 UTC")
}

// scrubHandler zeroes the reserved fields and name of an hdlr box in place.
// Handler names reveal the device and software ("GoPro AVC"), and QuickTime
// stores a vendor code in the reserved fields. The field before the handler type is
// zero in ISO files, while QuickTime names a kind of component there, and ffmpeg reads
// it to tell the two apart, so those names stay and anything else goes.
func (a *analysis) scrubHandler(m *image, b box, where string) error {
	const flags, component, reserved, name = 4, 8, 12, 24 // where each field ends or starts
	p := m.data[b.body:b.end]
	if len(p) < name {
		return core.Malformedf("hdlr box of %d bytes is too short", b.size())
	}
	if m.zero(b.body, b.body+flags) {
		a.reportReserved(b.typ, where)
	}
	if kind := string(p[flags:component]); kind != "mhlr" && kind != "dhlr" && m.zero(b.body+flags, b.body+component) {
		a.reportReserved(b.typ, where)
	}
	if !core.AllZero(p[reserved:name]) {
		a.remove(core.CatSoftware, core.RiskLow, "Handler vendor code", trimmedText(p[reserved:reserved+4]), where)
	}
	if !core.AllZero(p[name:]) {
		a.remove(core.CatSoftware, core.RiskLow, "Handler name", pascalOrCString(p[name:]), where)
	}
	m.zero(b.body+reserved, b.end)
	return nil
}

// pascalOrCString decodes a name that may have a length byte in front, as in
// QuickTime, or a terminating NUL, as in ISO files.
func pascalOrCString(b []byte) string {
	if len(b) > 0 && int(b[0]) == len(b)-1 {
		return trimmedText(b[1:])
	}
	return trimmedText(b)
}

// trimmedText stops at the first NUL.
func trimmedText(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// checkDataRefs refuses a dinf box that points outside this file, since then the
// chunk offsets do not describe this file and the entry itself holds a file path
// or URL. It returns how many entries the dref holds.
func (a *analysis) checkDataRefs(m *image, dinf box, where string) (int, error) {
	entries := 0
	err := a.editChildren(m, dinf.body, dinf.end, where, editor{edit: map[string]func(box) error{
		"dref": func(dref box) error {
			if dref.end-dref.body < 8 {
				return core.Malformedf("dref box of %d bytes is too short", dref.size())
			}
			if m.zero(dref.body, dref.body+4) {
				a.reportReserved(dref.typ, where)
			}
			tail, err := a.walk(m.data, dref.body+8, dref.end, func(e box) error {
				entries++
				return a.checkDataEntry(m, e)
			})
			if err != nil {
				return err
			}
			a.scrubTail(m, tail, dref.end, where)
			return nil
		},
	}})
	return entries, err
}

func (a *analysis) checkDataEntry(m *image, e box) error {
	const selfContained = 1
	if e.end-e.body < 4 {
		return core.Malformedf("%q data reference of %d bytes is too short", e.typ, e.size())
	}
	location := m.data[e.body+4 : e.end]
	if fullBoxFlags(m.data[e.body:e.end])&selfContained == 0 && !core.AllZero(location) {
		return core.Unsupportedf("isobmff: media stored in another file (%q data reference)", e.typ)
	}
	// A self-contained entry has no use for a location, so anything in one was
	// left behind by whatever wrote the file, such as the path of its source.
	if m.zero(e.body+4, e.end) {
		a.remove(core.CatHidden, core.RiskHigh, "Stale data reference", fmt.Sprintf("%d bytes", len(location)), "dref")
	}
	// What is left can only say that the media is in this file, whatever the flags or,
	// if it is no type a reader knows, the type say.
	if head := m.data[e.body : e.body+4]; binary.BigEndian.Uint32(head) != selfContained {
		binary.BigEndian.PutUint32(head, selfContained)
		m.dirty = true
	}
	if !dataEntryTypes[e.typ] {
		copy(m.data[e.start+4:], "url ")
		m.dirty = true
	}
	return nil
}

// dataEntryTypes are the data references readers know: ISO's two and QuickTime's.
var dataEntryTypes = typeSet("url ", "urn ", "alis", "rsrc", "cios")

// editTrack edits a track that stays.
func (a *analysis) editTrack(m *image, t *track, going map[uint32]bool) error {
	where := t.where()
	if t.subtitle() {
		a.report.Warn("subtitle track %d (%s) was kept and its text was not inspected; some drones record GPS in subtitles", t.position, t.handler)
	}
	if t.handler == "auxv" {
		a.retain(core.CatContent, core.RiskLow, "Auxiliary video track", fmt.Sprintf("format %q, %d samples", t.format, t.samples), where,
			"depth or transparency that changes how the video looks; removing it would change the picture")
	}
	return a.editChildren(m, t.box.body, t.box.end, where, editor{
		keep: keepInTrack,
		edit: map[string]func(box) error{
			"tkhd": func(b box) error { return a.scrubTimes(m, b, where+"/tkhd") },
			"tref": func(b box) error { return a.editReferences(m, b, going, where+"/tref") },
			"trgr": func(b box) error { return a.editTrackGroups(m, b, where+"/trgr") },
			"mdia": func(b box) error { return a.editMedia(m, t, b, where+"/mdia") },
		},
	})
}

// editReferences keeps a reference list only if it is what the box says: IDs of
// tracks that stay. One naming a removed track is freed, so that no player goes
// looking for it. Anything else is a hiding place, and ffmpeg reads a udta here.
func (a *analysis) editReferences(m *image, tref box, going map[uint32]bool, where string) error {
	tail, err := a.walk(m.data, tref.body, tref.end, func(b box) error {
		if isFreeSpace(b.typ) {
			a.scrubNested(m, b, where)
			return nil
		}
		valid := (b.end-b.body)%4 == 0
		removed := false
		for p := b.body; valid && p < b.end; p += 4 {
			id := binary.BigEndian.Uint32(m.data[p:])
			gone, known := going[id]
			valid = known || id == 0 // zero is how a list leaves an entry blank
			removed = removed || gone
		}
		switch {
		case !valid:
			a.removeNested(m, b, where)
		case removed:
			m.free(b)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.scrubTail(m, tail, tref.end, where)
	return nil
}

// editTrackGroups keeps a track group only if it holds no more than a group ID and
// a flag, which is all that any kind of group holds.
func (a *analysis) editTrackGroups(m *image, trgr box, where string) error {
	const maxGroup = 12
	tail, err := a.walk(m.data, trgr.body, trgr.end, func(b box) error {
		switch {
		case isFreeSpace(b.typ):
			a.scrubNested(m, b, where)
		case b.end-b.body > maxGroup:
			a.removeNested(m, b, where)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.scrubTail(m, tail, trgr.end, where)
	return nil
}

func (a *analysis) editMedia(m *image, t *track, mdia box, where string) error {
	return a.editChildren(m, mdia.body, mdia.end, where, editor{
		keep: keepInMedia,
		edit: map[string]func(box) error{
			"mdhd": func(b box) error { return a.scrubTimes(m, b, where+"/mdhd") },
			"hdlr": func(b box) error { return a.scrubHandler(m, b, where+"/hdlr") },
			"minf": func(b box) error { return a.editMediaInfo(m, t, b, where+"/minf") },
		},
	})
}

func (a *analysis) editMediaInfo(m *image, t *track, minf box, where string) error {
	return a.editChildren(m, minf.body, minf.end, where, editor{
		keep: keepInInfo,
		edit: map[string]func(box) error{
			// QuickTime has a second hdlr here, for the data handler.
			"hdlr": func(b box) error { return a.scrubHandler(m, b, where+"/hdlr") },
			"dinf": func(box) error { return nil }, // done by parseTrack
			"stbl": func(b box) error { return a.editSampleTable(m, t, b, where+"/stbl") },
		},
	})
}

func (a *analysis) editSampleTable(m *image, t *track, stbl box, where string) error {
	return a.editChildren(m, stbl.body, stbl.end, where, editor{
		keep: keepInTable,
		edit: map[string]func(box) error{
			"stsd": func(b box) error { return a.editDescriptions(m, t, b, where+"/stsd") },
			"stsc": func(b box) error { return a.checkChunkRuns(m, b, stbl, where) },
			"stss": func(b box) error { return a.checkSampleNumbers(m, b, t, where) },
			"stps": func(b box) error { return a.checkSampleNumbers(m, b, t, where) },
			"sgpd": func(b box) error { return a.editSampleGroup(m, b, where) },
			"sbgp": func(b box) error { return a.editSampleGroup(m, b, where) },
			"sdtp": func(b box) error { return a.trimToSamples(m, b, t, where) },
			"stdp": func(b box) error { return a.trimToSamples(m, b, t, where) },
			"padb": func(b box) error { return a.trimToSamples(m, b, t, where) },
		},
	})
}

// Sample entry layout. Every sample entry starts with an 8-byte header of its
// own, 6 reserved bytes and a data reference index.
const (
	entryVersion   = 16 // QuickTime version, in both visual and audio entries
	entryVendor    = 20 // QuickTime vendor code, such as "appl" or "FFMP"
	visualCompName = 50 // the 32-byte compressor name
	visualFixed    = 86 // child boxes start here
	audioFixed     = 36 // child boxes start here in version 0
	audioFixedV1   = 52 // QuickTime version 1 adds four 32-bit fields
	audioFixedV2   = 72 // QuickTime version 2 replaces them with a larger block
)

// encryptedFormats are sample entry formats that mean the media is encrypted.
var encryptedFormats = typeSet("encv", "enca", "enct", "encs", "drms", "drmi", "sinf")

// imageFrameFormats are sample entry formats whose every frame is a whole image file.
// MP4 files store Motion JPEG under the generic mp4v, so readChunks also looks at the
// first frame of each video track.
var imageFrameFormats = typeSet("jpeg", "mjpa", "mjpb", "mjp2", "AVDJ", "dmb1", "png ", "tiff", "gif ")

func (a *analysis) warnImageFrames() {
	a.warnOnce("this video's frames are whole image files, and metadata inside them, such as EXIF, is not removed")
}

func (a *analysis) editDescriptions(m *image, t *track, stsd box, where string) error {
	p := m.data[stsd.body:stsd.end]
	if len(p) < 8 {
		return core.Malformedf("stsd box of %d bytes is too short", stsd.size())
	}
	stsdVersion := p[0]
	declared := binary.BigEndian.Uint32(p[4:])
	if m.zero(stsd.body+1, stsd.body+4) {
		a.reportReserved(stsd.typ, where)
	}
	var found uint32
	tail, err := a.walk(m.data, stsd.body+8, stsd.end, func(e box) error {
		found++
		if encryptedFormats[e.typ] {
			return fmt.Errorf("%w: track %d uses sample format %q", core.ErrEncrypted, t.id, e.typ)
		}
		switch {
		case t.visual():
			if imageFrameFormats[e.typ] {
				a.warnImageFrames()
			}
			return a.editVisualEntry(m, e, where)
		case t.handler == "soun":
			return a.editAudioEntry(m, e, stsdVersion, where)
		}
		// A subtitle entry's layout depends on its format, and the text it applies
		// to is covered by the warning about subtitles.
		return nil
	})
	if err != nil {
		return err
	}
	if found != declared {
		return core.Malformedf("stsd declares %d entries but holds %d", declared, found)
	}
	a.scrubTail(m, tail, stsd.end, where)
	return nil
}

func (a *analysis) editVisualEntry(m *image, e box, where string) error {
	if e.size() < visualFixed {
		return core.Malformedf("visual sample entry of %d bytes is too short", e.size())
	}
	a.scrubVendor(m, e, where)
	a.scrubVisualFixed(m, e, where)
	name := m.data[e.start+visualCompName : e.start+visualCompName+32]
	if !core.AllZero(name) {
		a.remove(core.CatSoftware, core.RiskLow, "Compressor name", pascalOrCString(trailingTrim(name)), where)
		m.zero(e.start+visualCompName, e.start+visualCompName+32)
	}
	return a.editEntryChildren(m, e, e.start+visualFixed+embeddedPalette(m.data[e.start:e.end]), where)
}

// embeddedPalette returns the size of a colour table stored after a visual sample
// entry's fixed fields, which QuickTime does for video of 8 bits or fewer when the
// entry names no table of its own. It starts with a seed that is often zero, so it
// must not be read as a list of boxes.
func embeddedPalette(entry []byte) int64 {
	const depthField, tableField = 82, 84
	const header = 8 // the seed, the flags and the index of the last colour
	depth, table := binary.BigEndian.Uint16(entry[depthField:]), binary.BigEndian.Uint16(entry[tableField:])
	if table != 0 || (depth != 1 && depth != 2 && depth != 4 && depth != 8) || len(entry) < visualFixed+header {
		return 0
	}
	last := int64(binary.BigEndian.Uint16(entry[visualFixed+6:]))
	size := header + (last+1)*8
	if size > int64(len(entry)-visualFixed) {
		return 0
	}
	return size
}

// trailingTrim cuts a fixed-size name at its padding, so that a QuickTime length
// byte can be told apart from the first character.
func trailingTrim(b []byte) []byte {
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	return b[:end]
}

func (a *analysis) editAudioEntry(m *image, e box, stsdVersion byte, where string) error {
	if e.size() < audioFixed {
		return core.Malformedf("audio sample entry of %d bytes is too short", e.size())
	}
	a.scrubVendor(m, e, where)
	a.scrubAudioFixed(m, e, where)
	start := e.start + audioFixed
	// The ISO version 1 entry keeps the version 0 layout and raises the stsd
	// version instead, so only a version 0 stsd has the QuickTime layouts.
	if stsdVersion == 0 {
		switch binary.BigEndian.Uint16(m.data[e.start+entryVersion:]) {
		case 1:
			start = e.start + audioFixedV1
		case 2:
			start = e.start + audioFixedV2
		}
	}
	if start > e.end {
		return core.Malformedf("audio sample entry of %d bytes is too short for its version", e.size())
	}
	return a.editEntryChildren(m, e, start, where)
}

func (a *analysis) scrubVendor(m *image, e box, where string) {
	vendor := m.data[e.start+entryVendor : e.start+entryVendor+4]
	if !core.AllZero(vendor) {
		a.remove(core.CatSoftware, core.RiskLow, "Encoder vendor code", trimmedText(vendor), where)
		m.zero(e.start+entryVendor, e.start+entryVendor+4)
	}
}

// editEntryChildren cleans the boxes after a sample entry's fixed fields.
//
// This is a denylist, the one place in a movie where an allowlist is impossible:
// new codecs add required configuration boxes here all the time, and removing one
// would stop the track decoding. So what is known to carry metadata is removed, and
// what is not known to configure a decoder stays with a warning.
func (a *analysis) editEntryChildren(m *image, e box, start int64, where string) error {
	return a.editEntryBoxes(m, start, e.end, where, e.typ, true)
}

func (a *analysis) editEntryBoxes(m *image, start, end int64, where, format string, inEntry bool) error {
	tail, err := a.walk(m.data, start, end, func(b box) error {
		switch {
		case b.typ == "sinf":
			return fmt.Errorf("%w: protection scheme in a sample entry", core.ErrEncrypted)
		case entryMetadata[b.typ] || b.typ[0] == 0xA9:
			a.removeNested(m, b, where)
		case isFreeSpace(b.typ):
			a.scrubNested(m, b, where)
		case b.typ == "wave":
			// QuickTime's wrapper for audio configuration, which only belongs directly
			// in a sample entry. Readers parse it the same way as the entry.
			if !inEntry {
				a.removeNested(m, b, where)
				return nil
			}
			return a.editEntryBoxes(m, b.body, b.end, where+"/wave", format, false)
		case b.typ == "colr":
			return a.editColour(m, b, where)
		case b.typ == "btrt":
			// Three figures for a player's buffering, for which zero means not stated.
			if m.zero(b.body, b.end) {
				a.remove(core.CatTechnical, core.RiskNone, "Bit rate figures", fmt.Sprintf("%d bytes", b.end-b.body), where)
			}
		case entryConfig[b.typ]:
			return a.trimKept(m, b, where)
		case !inEntry && b.typ == format && b.end-b.body <= maxConfigStub:
			// Nothing to say about the short box that names the codec inside a wave box.
		case !inEntry && b.typ == waveTerminator && b.end == b.body:
			// Nothing to say about the empty box that ends a wave box. It holds no bytes.
		default:
			// The warning names no box: clean prints warnings, and the type is the file's.
			a.retain(core.CatHidden, core.RiskLow, "Unrecognised box in a track description", fmt.Sprintf("%q, %d bytes", b.typ, b.end-b.body), where,
				"kept in case a decoder needs it")
			a.warnOnce("track descriptions hold boxes HideMe does not recognise, which were kept in case a decoder needs them")
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.scrubTail(m, tail, end, where)
	return nil
}

// maxConfigStub is the largest box named after the codec that goes unmentioned inside
// a wave box, where one is ordinary. It has no room for much, and a wave box has only
// the one name to offer it, so it cannot be repeated to make more.
const maxConfigStub = 8

// waveTerminator is the type of the empty box that ends a wave box in QuickTime files,
// including every iPhone video.
const waveTerminator = "\x00\x00\x00\x00"

// entryMetadata lists what a sample entry has no use for: boxes that carry metadata
// elsewhere, and containers. ffmpeg parses a sample entry's children with the same
// table as the rest of the file, so a udta in a dinf in a sample entry is reported as
// the track's metadata, and an ilst or a chapter list as the file's.
var entryMetadata = typeSet("udta", "meta", "uuid", "ilst", "keys", "----", "chpl", "XMP_", "Xtra", "loci",
	"moov", "trak", "mdia", "minf", "stbl", "stsd", "dinf", "edts", "tref", "mvex", "moof", "traf", "mfra", "mdat", "schi")

// entryConfig lists the boxes known to configure a decoder or to describe its output.
var entryConfig = typeSet(
	"avcC", "hvcC", "lhvC", "vvcC", "vvnC", "av1C", "vpcC", "evcC", "dvcC", "dvvC", "dvwC", "mvcC", "svcC",
	"d263", "devc", "dqcp", "dsmv", "damr", "m4ds", "esds", "glbl", "jp2h", "strf", "SMI ",
	"btrt", "pasp", "clap", "colr", "nclc", "gama", "chrm", "fiel", "clli", "mdcv", "cclv", "amve",
	"st3d", "sv3d", "vexu", "hfov", "ccst", "sbas", "kmat", "ACLR", "APRG", "ARES", "AORD",
	"frma", "enda", "chan", "chnl", "srat", "alac", "dOps", "dfLa", "dac3", "dec3", "dac4", "ddts", "udts",
	"dmlp", "mhaC", "pcmC", "QDCA", "QDCI", "SA3D",
)

func (a *analysis) editColour(m *image, b box, where string) error {
	p := m.data[b.body:b.end]
	if len(p) < 4 || (string(p[:4]) != "prof" && string(p[:4]) != "rICC") {
		return a.trimKept(m, b, where)
	}
	value := fmt.Sprintf("%d bytes", len(p)-4)
	if a.opts.Strict {
		a.remove(core.CatTechnical, core.RiskLow, "ICC colour profile", value, where)
		m.free(b)
		return nil
	}
	a.retain(core.CatTechnical, core.RiskLow, "ICC colour profile", value, where, iccReason)
	return a.trimKept(m, b, where)
}
