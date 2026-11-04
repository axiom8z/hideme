package matroska

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"slices"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxPatches and maxTopLevel bound the memory a hostile file can demand. Real
	// files produce a handful of patches, and even a day of one-second clusters
	// stays far below the top-level limit.
	maxPatches  = 1 << 18
	maxTopLevel = 1 << 20
	// maxTracks bounds the track table. Matroska players handle a few dozen tracks.
	maxTracks = 1 << 12
	// crcBufferSize bounds the memory used to check a checksum over a master of any size.
	crcBufferSize = 1 << 20

	segmentWhere = "Segment"
)

// topLevel records a kept child of Segment, so that SeekHead entries can be checked
// against it and clusters walked once every track is known. It is small because a
// long recording has many clusters.
type topLevel struct {
	off  int64
	size int64
	id   uint32
}

// plan is the result of walking a file: the report and the patches that clean it.
// Inspect and Clean both build one, so they cannot disagree.
type plan struct {
	r         reader
	rep       *core.Report
	patches   []core.Patch
	top       []topLevel
	seekHeads []element
	segData   int64 // where the segment's payload starts: SeekPosition is relative to it
	findings  int
	further   int
	// tracks holds every declared track number, which blocks are checked against.
	tracks map[uint64]bool
	// trackCodec is the codec ID of the track being walked, which decides how to
	// read its private data, and trackPrivateEncoded says that data is compressed.
	trackCodec          string
	trackPrivateEncoded bool
	warned              [warningKinds]bool
}

// warning kinds are issued once each, so a hostile file cannot flood the output.
type warning uint8

const (
	warnSubtitles warning = iota
	warnSubtitleHeader
	warnOpaqueText
	warnEncodedPrivate
	warnBlockAdditions
	warnUnknownCodec
	warningKinds
)

func (p *plan) warn(kind warning, message string) {
	if !p.warned[kind] {
		p.warned[kind] = true
		p.rep.Warn("%s", message)
	}
}

// analyse walks the whole file structure, down to the block headers in clusters.
// Only block payloads go unread.
func analyse(ctx context.Context, src core.Source) (*plan, error) {
	p := &plan{r: reader{ctx: ctx, src: src}, rep: &core.Report{}, tracks: make(map[uint64]bool)}
	header, doc, err := readDocHeader(&p.r)
	if err != nil {
		return nil, err
	}
	if doc.docType != docMatroska && doc.docType != docWebM {
		return nil, core.Unsupportedf("matroska: document type %q is not Matroska or WebM", doc.docType)
	}
	if err := doc.checkVersions(); err != nil {
		return nil, err
	}
	p.rep.Format = doc.docType
	if err := p.walk(header, ebmlHeaderRules, "EBML header"); err != nil {
		return nil, err
	}
	seg, err := p.findSegment(header.end())
	if err != nil {
		return nil, err
	}
	end, err := p.segment(seg)
	if err != nil {
		return nil, err
	}
	if err := p.trailing(end); err != nil {
		return nil, err
	}
	p.finishFindings()
	slices.SortFunc(p.patches, func(a, b core.Patch) int { return cmp.Compare(a.Off, b.Off) })
	return p, nil
}

// findSegment scans from off for the Segment. Only Void may stand between the
// EBML header and the Segment; anything else means the file is not understood.
func (p *plan) findSegment(off int64) (element, error) {
	size := p.r.src.Size()
	for off < size {
		e, err := p.r.header(off, size)
		if err != nil {
			return element{}, err
		}
		switch {
		case e.id == idSegment:
			return e, nil
		case e.id == idVoid && !e.unknown:
			if err := p.scrubVoid(e, "file"); err != nil {
				return element{}, err
			}
			off = e.end()
		default:
			return element{}, core.Malformedf("matroska: unexpected element %#x at offset %d before the Segment", e.id, e.off)
		}
	}
	return element{}, core.Malformedf("matroska: the file has no Segment")
}

// segment walks the Segment's children and returns where it ends. An unknown-size
// Segment runs to the end of the file, unless another EBML document starts first.
func (p *plan) segment(seg element) (int64, error) {
	limit := seg.end()
	if seg.unknown {
		limit = p.r.src.Size()
	}
	p.segData = seg.dataOff
	guard := crcGuard{before: len(p.patches), to: limit}
	for pos := seg.dataOff; pos < limit; {
		if err := p.r.ctx.Err(); err != nil {
			return 0, err
		}
		e, err := p.r.header(pos, limit)
		if err != nil {
			return 0, err
		}
		if seg.unknown && (e.id == idEBML || e.id == idSegment) {
			return 0, errChained(e.off)
		}
		switch {
		case e.id == idCluster:
			if e.unknown {
				end, err := p.clusterEnd(e, limit)
				if err != nil {
					return 0, err
				}
				e.size, e.unknown = end-e.dataOff, false
			}
			if err := p.noteTop(e); err != nil {
				return 0, err
			}
		case e.unknown:
			return 0, core.Malformedf("matroska: element %#x at offset %d has an unknown size, which only segments and clusters may have", e.id, e.off)
		case e.id == idCRC32:
			if err := guard.offer(p, e, segmentWhere); err != nil {
				return 0, err
			}
		case e.id == idSeekHead:
			if err := p.noteTop(e); err != nil {
				return 0, err
			}
			p.seekHeads = append(p.seekHeads, e)
		default:
			if e.id == idInfo || e.id == idTracks || e.id == idCues {
				if err := p.noteTop(e); err != nil {
					return 0, err
				}
			}
			if err := p.apply(e, segmentRules, segmentWhere); err != nil {
				return 0, err
			}
		}
		pos = e.end()
	}
	// A SeekHead may come after what it indexes, so these wait for the full list
	// of elements. Tracks may come after the clusters that use them, so clusters
	// wait too.
	for _, head := range p.seekHeads {
		if err := p.walk(head, seekHeadRules, segmentWhere+"/SeekHead"); err != nil {
			return 0, err
		}
	}
	for _, t := range p.top {
		if t.id != idCluster {
			continue
		}
		c, err := p.r.header(t.off, limit)
		if err != nil {
			return 0, err
		}
		c.size, c.unknown = t.size, false
		if err := p.walk(c, clusterRules, segmentWhere+"/Cluster"); err != nil {
			return 0, err
		}
	}
	if err := guard.settle(p); err != nil {
		return 0, err
	}
	return limit, nil
}

// clusterEnd walks an unknown-size cluster to find where it stops: at the first
// ID that cannot be a cluster child, or at limit. Its contents get judged later,
// like those of any other cluster.
func (p *plan) clusterEnd(c element, limit int64) (int64, error) {
	for pos := c.dataOff; pos < limit; {
		if err := p.r.ctx.Err(); err != nil {
			return 0, err
		}
		e, err := p.r.header(pos, limit)
		if err != nil {
			return 0, err
		}
		if !clusterChildren[e.id] {
			return pos, nil
		}
		if e.unknown {
			return 0, core.Malformedf("matroska: cluster child %#x at offset %d has an unknown size", e.id, e.off)
		}
		pos = e.end()
	}
	return limit, nil
}

// block keeps a block only if it belongs to a declared track. Readers skip any
// other, so its payload would be a place to hide anything, and no decoder would
// ever miss it. A file with no tracks at all is different: its blocks may well be
// its video, in a layout this parser does not understand.
func (p *plan) block(e element, where string) error {
	if len(p.tracks) == 0 {
		return core.Malformedf("matroska: the block at offset %d belongs to a file that declares no tracks", e.off)
	}
	var raw [maxSizeWidth]byte
	head := raw[:min(e.size, maxSizeWidth)]
	if err := p.r.read(head, e.dataOff); err != nil {
		return err
	}
	if number, _, unknown, err := parseSize(head); err == nil && !unknown && p.tracks[uint64(number)] {
		return nil
	}
	p.add(field{"Block of an undeclared track", core.CatHidden, core.RiskMed, decodeSize}, core.ByteCount(e.size), where)
	return p.void(e)
}

func (p *plan) encryptedBlock(element, string) error {
	return fmt.Errorf("%w: a cluster holds an encrypted block", core.ErrEncrypted)
}

// misplaced refuses a top-level element inside a known-size cluster. The cluster's
// size cannot be right, readers disagree on whether such an element ends the
// cluster, and what follows it could be video that voiding it would destroy.
func (p *plan) misplaced(e element, _ string) error {
	return core.Malformedf("matroska: top-level element %#x at offset %d is inside a cluster", e.id, e.off)
}

func (p *plan) blockAdditional(element, string) error {
	p.warn(warnBlockAdditions, "This file has block additions, such as an alpha channel or HDR data, stored beside its frames. They were kept and not inspected.")
	return nil
}

func (p *plan) noteTop(e element) error {
	if len(p.top) >= maxTopLevel {
		return fmt.Errorf("%w: more than %d top-level elements", core.ErrTooLarge, maxTopLevel)
	}
	p.top = append(p.top, topLevel{off: e.off, size: e.size, id: e.id})
	return nil
}

// trailing drops whatever follows the Segment, unless that is a further EBML
// document or more video. EBML allows the former, so dropping one could lose a real
// video, and the rule for a variant the handler does not support is to say so.
func (p *plan) trailing(end int64) error {
	size := p.r.src.Size()
	if end >= size {
		return nil
	}
	if e, err := p.r.header(end, size); err == nil {
		switch e.id {
		case idEBML:
			return errChained(end)
		case idCluster, idSegment:
			// ffmpeg plays on past a segment's declared end, so this is video.
			return core.Malformedf("matroska: element %#x at offset %d is past the end of the Segment, so its size must be wrong", e.id, end)
		}
	}
	p.add(field{"Trailing data", core.CatHidden, core.RiskHigh, decodeSize}, core.ByteCount(size-end), "file")
	return p.patch(core.Patch{Off: end, Len: size - end})
}

func errChained(off int64) error {
	return core.Unsupportedf("matroska: a second EBML document starts at offset %d", off)
}

// walk holds a master's children to its allowlist. It needs no depth limit of its
// own, because it only descends where a rule table says to and the tables have no
// cycles.
func (p *plan) walk(m element, rules map[uint32]rule, where string) error {
	guard := crcGuard{before: len(p.patches), to: m.end()}
	for e, err := range p.r.children(m) {
		if err != nil {
			return err
		}
		if e.id == idCRC32 {
			err = guard.offer(p, e, where)
		} else {
			err = p.apply(e, rules, where)
		}
		if err != nil {
			return err
		}
	}
	return guard.settle(p)
}

// apply does what the rule table says for one child.
func (p *plan) apply(e element, rules map[uint32]rule, where string) error {
	if e.id == idVoid {
		return p.scrubVoid(e, where)
	}
	r, ok := rules[e.id]
	if !ok {
		return p.removeUnknown(e, where)
	}
	switch r.kind {
	case ruleKeep:
		if r.maxSize > 0 && e.size > r.maxSize {
			return core.Malformedf("matroska: element %#x at offset %d is %d bytes long, over its limit of %d", e.id, e.off, e.size, r.maxSize)
		}
		return nil
	case ruleMaster:
		return p.walk(e, r.children, where+"/"+r.field.name)
	case ruleBlank:
		zero, err := p.r.isZero(e.dataOff, e.size)
		if err != nil || zero {
			return err
		}
		p.add(r.field, p.describe(e, r.field.decode), where)
		return p.patch(core.Patch{Off: e.dataOff, Len: e.size, Zero: true})
	case ruleRemove:
		p.add(r.field, p.describe(e, r.field.decode), where)
		return p.void(e)
	default:
		return r.visit(p, e, where)
	}
}

// crcGuard remembers a master's CRC-32 child while its other children are
// walked, since a checksum is only wrong if something it covers changed. A valid
// CRC in an untouched master stays, so cleaning a clean file changes nothing.
type crcGuard struct {
	crc    element
	seen   bool
	before int
	to     int64 // where what the checksum covers ends: the end of its master
}

// offer accepts the first well-formed CRC-32. A second one, or one of the wrong
// size, is no checksum this handler can account for.
func (g *crcGuard) offer(p *plan, e element, where string) error {
	if g.seen || e.size != 4 {
		return p.removeUnknown(e, where)
	}
	g.crc, g.seen = e, true
	return nil
}

// settle removes a checksum over bytes that changed, and one that never matched them.
// Readers do not check it, so one that does not match is four bytes of anything.
func (g *crcGuard) settle(p *plan) error {
	if !g.seen {
		return nil
	}
	if len(p.patches) != g.before {
		return p.void(g.crc)
	}
	ok, err := p.checksumMatches(g.crc, g.to)
	if err != nil || ok {
		return err
	}
	p.add(field{"Data in a checksum", core.CatHidden, core.RiskLow, decodeSize}, core.ByteCount(g.crc.size), "CRC-32")
	return p.void(g.crc)
}

// checksumMatches reads what a CRC-32 covers, everything after it in its master.
func (p *plan) checksumMatches(crc element, to int64) (bool, error) {
	var stored [4]byte
	if err := p.r.read(stored[:], crc.dataOff); err != nil {
		return false, err
	}
	sum := uint32(0)
	buf := make([]byte, min(to-crc.end(), crcBufferSize))
	for pos := crc.end(); pos < to; {
		if err := p.r.ctx.Err(); err != nil {
			return false, err
		}
		chunk := buf[:min(to-pos, int64(len(buf)))]
		if err := p.r.read(chunk, pos); err != nil {
			return false, err
		}
		sum = crc32.Update(sum, crc32.IEEETable, chunk)
		pos += int64(len(chunk))
	}
	return binary.LittleEndian.Uint32(stored[:]) == sum, nil
}

// scrubVoid zeroes a Void element's payload. Writers are free to leave anything
// there, and tools that shrink metadata in place commonly leave the old contents.
func (p *plan) scrubVoid(e element, where string) error {
	zero, err := p.r.isZero(e.dataOff, e.size)
	if err != nil || zero {
		return err
	}
	p.add(field{"Hidden data in Void element", core.CatHidden, core.RiskMed, decodeSize}, core.ByteCount(e.size), where)
	return p.patch(core.Patch{Off: e.dataOff, Len: e.size, Zero: true})
}

func (p *plan) removeUnknown(e element, where string) error {
	p.add(field{"Unknown element", core.CatHidden, core.RiskMed, decodeSize},
		fmt.Sprintf("ID %#x, %s", e.id, core.ByteCount(e.size)), where)
	return p.void(e)
}

// void replaces a whole element with a Void of the same total length, which is
// how every later offset stays valid.
func (p *plan) void(e element) error {
	total := e.end() - e.off
	header, err := voidHeader(total)
	if err != nil {
		return err
	}
	if err := p.patch(core.Patch{Off: e.off, Len: int64(len(header)), Data: header}); err != nil {
		return err
	}
	return p.patch(core.Patch{Off: e.off + int64(len(header)), Len: total - int64(len(header)), Zero: true})
}

// patch records one change. An empty patch would change nothing, and could sort
// after a real patch at the same offset, which core.CopyPatched refuses.
func (p *plan) patch(patch core.Patch) error {
	if patch.Len == 0 {
		return nil
	}
	if len(p.patches) >= maxPatches {
		return fmt.Errorf("%w: more than %d elements to rewrite", core.ErrTooLarge, maxPatches)
	}
	p.patches = append(p.patches, patch)
	return nil
}

// seek keeps a SeekHead entry only if it names a kept element at the position it
// gives. An entry for a voided element would send a player to a Void and would
// also reveal that the file used to hold, say, tags.
func (p *plan) seek(e element, where string) error {
	target, pos, ok, err := p.readSeek(e)
	if err != nil {
		return err
	}
	if !ok || !p.isKept(target, pos) {
		return p.void(e)
	}
	return p.walk(e, seekRules, where+"/Seek")
}

// readSeek reads a Seek entry's target. ok is false if the entry does not hold
// exactly one usable SeekID and one SeekPosition; such an entry is useless, and
// that is not a reason to refuse the file.
func (p *plan) readSeek(e element) (target uint32, pos uint64, ok bool, err error) {
	var ids, positions int
	for child, err := range p.r.children(e) {
		if err != nil {
			return 0, 0, false, err
		}
		switch child.id {
		case idSeekID:
			ids++
			if child.size < 1 || child.size > maxIDWidth {
				return 0, 0, false, nil
			}
			var id uint64
			if id, err = p.r.readUint(child); err != nil {
				return 0, 0, false, err
			}
			target = uint32(id)
		case idSeekPosition:
			positions++
			if child.size > maxScalarSize {
				return 0, 0, false, nil
			}
			if pos, err = p.r.readUint(child); err != nil {
				return 0, 0, false, err
			}
		}
	}
	return target, pos, ids == 1 && positions == 1, nil
}

func (p *plan) isKept(id uint32, relative uint64) bool {
	if relative > uint64(p.r.src.Size()) {
		return false
	}
	off := p.segData + int64(relative)
	i, found := slices.BinarySearchFunc(p.top, off, func(t topLevel, off int64) int { return cmp.Compare(t.off, off) })
	return found && p.top[i].id == id
}

// track refuses a track this handler cannot vouch for, because the blocks of
// every track sit in clusters it copies unread.
func (p *plan) track(e element, where string) error {
	var number, kind uint64
	var seenType, seenCodec bool
	p.trackCodec, p.trackPrivateEncoded = "", false
	for child, err := range p.r.children(e) {
		if err != nil {
			return err
		}
		switch child.id {
		case idTrackNumber:
			if number, err = p.r.readUint(child); err != nil {
				return err
			}
		case idTrackType:
			if seenType {
				return core.Malformedf("matroska: track at offset %d has two types", e.off)
			}
			seenType = true
			if kind, err = p.r.readUint(child); err != nil {
				return err
			}
		case idCodecID:
			if seenCodec {
				return core.Malformedf("matroska: track at offset %d has two codec IDs", e.off)
			}
			seenCodec = true
			p.trackCodec = p.text(child)
		case idContentEncodings:
			if p.trackPrivateEncoded, err = p.encodesPrivateData(child); err != nil {
				return err
			}
		}
	}
	if len(p.tracks) >= maxTracks {
		return fmt.Errorf("%w: more than %d tracks", core.ErrTooLarge, maxTracks)
	}
	p.tracks[number] = true
	if !codecSuitsTrack(p.trackCodec, kind) {
		return core.Unsupportedf("matroska: track %d has type %#x, but a codec of another type", number, kind)
	}
	switch {
	case kind == trackVideo || kind == trackAudio:
	case kind == trackSubtitle:
		p.warn(warnSubtitles, "This file has subtitle tracks. Their text was kept and not inspected, and some devices store GPS positions in subtitles.")
	default:
		return core.Unsupportedf("matroska: track %d has type %#x, which can hold data this handler cannot remove", number, kind)
	}
	return p.walk(e, trackEntryRules, where+"/TrackEntry")
}

// codecSuitsTrack checks a codec ID's prefix against its track's type. ffmpeg goes by the
// codec and quietly skips a track that disagrees, which would make its blocks a place to
// hide anything. A track of a type this handler refuses anyway needs no check.
func codecSuitsTrack(codec string, kind uint64) bool {
	if codec == "" {
		return true
	}
	switch kind {
	case trackVideo:
		return strings.HasPrefix(codec, "V_")
	case trackAudio:
		return strings.HasPrefix(codec, "A_")
	case trackSubtitle:
		return strings.HasPrefix(codec, "S_") || strings.HasPrefix(codec, "D_")
	}
	return true
}

// encodesPrivateData reports whether a track's encodings apply to its private data,
// which then cannot be read as stored. The scope defaults to frames only.
func (p *plan) encodesPrivateData(encodings element) (bool, error) {
	for encoding, err := range p.r.children(encodings) {
		if err != nil {
			return false, err
		}
		if encoding.id != idContentEncoding {
			continue
		}
		for child, err := range p.r.children(encoding) {
			if err != nil {
				return false, err
			}
			if child.id != idContentEncodingScope {
				continue
			}
			scope, err := p.r.readUint(child)
			if err != nil {
				return false, err
			}
			if scope&contentScopePrivate != 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// language keeps a language only if it has the shape of one. Readers show this
// field to the user, so it must not be a place for free text. It is optional, and
// nothing that fails this check names a language anyway.
func (p *plan) language(e element, where string) error {
	if e.size <= maxShortStringSize && validLanguage(p.text(e)) {
		return nil
	}
	p.add(field{"Track language", core.CatContent, core.RiskLow, decodeText}, p.text(e), where)
	return p.void(e)
}

// validLanguage accepts ISO 639-2 codes and BCP 47 tags: up to three letters, then
// subtags of letters and digits.
func validLanguage(s string) bool {
	for i, tag := range strings.Split(s, "-") {
		if len(tag) > maxLanguageSubtag || (i > 0 && tag == "") || (i == 0 && len(tag) > maxLanguageCode) {
			return false
		}
		for j := range len(tag) {
			c := tag[j]
			letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
			if !letter && (i == 0 || c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

// codecID refuses a codec ID that is not one. It cannot be removed, because decoders
// need it, and some readers match only its start, so a suffix would be free text
// that every copy of the file carries.
func (p *plan) codecID(e element, _ string) error {
	if e.size > maxShortStringSize {
		return core.Malformedf("matroska: the codec ID at offset %d is %d bytes long", e.off, e.size)
	}
	id := p.text(e)
	for i := range len(id) {
		c := id[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("_/.+-", c) >= 0) {
			return core.Malformedf("matroska: the codec ID at offset %d holds the byte %#02x", e.off, c)
		}
	}
	if !knownCodecID(id) {
		p.warn(warnUnknownCodec, "A track's codec ID is not one HideMe recognises. It was kept because decoders need it, and some read only its start, so anything after that stays too.")
	}
	return nil
}

// knownCodecs are the codec IDs of the Matroska codec registry, and those that begin
// this way take a part of their own after a slash, such as the AAC profile.
var (
	knownCodecs = map[string]bool{
		"V_VP8": true, "V_VP9": true, "V_AV1": true, "V_MPEG1": true, "V_MPEG2": true, "V_MPEG4/ISO/AVC": true, "V_MPEGH/ISO/HEVC": true,
		"V_MPEGI/ISO/VVC": true, "V_MPEG4/ISO/ASP": true, "V_MPEG4/ISO/AP": true, "V_MPEG4/ISO/SP": true, "V_MPEG4/MS/V3": true,
		"V_MS/VFW/FOURCC": true, "V_THEORA": true, "V_FFV1": true, "V_DIRAC": true, "V_PRORES": true, "V_UNCOMPRESSED": true, "V_QUICKTIME": true,
		"V_MJPEG": true, "V_AVS2": true, "V_AVS3": true, "V_CAVS": true, "A_AAC": true, "A_AC3": true, "A_EAC3": true, "A_DTS": true, "A_FLAC": true, "A_OPUS": true,
		"A_VORBIS": true, "A_TRUEHD": true, "A_MLP": true, "A_ALAC": true, "A_MS/ACM": true, "A_QUICKTIME": true, "A_TTA1": true, "A_WAVPACK4": true,
		"A_MPC": true, "A_APE": true, "A_ATRAC/AT1": true, "S_ASS": true, "S_SSA": true, "S_VOBSUB": true, "S_HDMV/PGS": true, "S_HDMV/TEXTST": true,
		"S_DVBSUB": true, "S_KATE": true, "S_IMAGE/BMP": true, "S_ARIBSUB": true, "D_WEBVTT/SUBTITLES": true, "D_WEBVTT/CAPTIONS": true, "D_WEBVTT/DESCRIPTIONS": true, "D_WEBVTT/METADATA": true,
		"S_TEXT/UTF8": true, "S_TEXT/ASCII": true, "S_TEXT/SSA": true, "S_TEXT/ASS": true, "S_TEXT/USF": true, "S_TEXT/WEBVTT": true,
	}
	knownCodecFamilies = []string{"V_REAL/", "A_REAL/", "A_AAC/", "A_DTS/", "A_MPEG/", "A_PCM/", "A_QUICKTIME/", "V_QUICKTIME/", "A_ATRAC/"}
)

func knownCodecID(id string) bool {
	if knownCodecs[id] {
		return true
	}
	return slices.ContainsFunc(knownCodecFamilies, func(family string) bool {
		rest, found := strings.CutPrefix(id, family)
		return found && rest != "" && len(rest) <= maxCodecFamilyPart
	})
}

// maxCodecFamilyPart is longer than the part after the family in any registered ID,
// such as MPEG4/MAIN/SBR/PS, which is enough to leave little room in one that is not.
const maxCodecFamilyPart = 16

func (p *plan) contentEncodingType(e element, _ string) error {
	kind, err := p.r.readUint(e)
	if err != nil {
		return err
	}
	if kind == contentEncodingEncryption {
		return fmt.Errorf("%w: a track's content is encrypted", core.ErrEncrypted)
	}
	return nil
}

func (p *plan) contentEncryption(element, string) error {
	return fmt.Errorf("%w: a track declares content encryption", core.ErrEncrypted)
}
