// Package avi removes metadata from AVI files, including OpenDML files over 1 GiB.
//
// An AVI's indexes hold absolute offsets, and the file can run to many gigabytes,
// so nothing moves and nothing is loaded. A removed chunk keeps its header's size
// and becomes a JUNK chunk with a zeroed payload, and core.CopyPatched streams the
// rest unchanged. Every level uses an allowlist, down to the chunk headers inside
// the movi lists. Only the payloads of frames go unread.
package avi

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/riff"
	"github.com/axiom8z/hideme/internal/textscan"
)

const (
	formAVI  = "AVI "
	formAVIX = "AVIX"

	idLIST = "LIST"
	idJUNK = "JUNK"

	// riffHeaderSize is a RIFF chunk's ID, size and form type.
	riffHeaderSize = 12

	// maxPatches bounds the memory a hostile file can demand. Real files have
	// fewer than a hundred chunks to rewrite.
	maxPatches = 1 << 18
	// maxMoviLists and maxStreams bound the same. A real file has one movi list per
	// RIFF chunk, and a handful of streams; the two-digit chunk IDs allow 256.
	maxMoviLists = 1 << 12
	maxStreams   = 256
	// maxFindings bounds the report in the same way. What is past the limit is
	// still removed, and counted.
	maxFindings = 512
	// maxValueBytes is more than core.SafeText shows of any value.
	maxValueBytes = 1024
	// maxHeaderChunk comfortably exceeds the fixed-layout chunks: avih and strh are
	// 56 bytes in practice, and dmlh is 248 as ffmpeg writes it.
	maxHeaderChunk = 4096
	// avihLayout, strhLayout and dmlhLayout are how many bytes of those chunks mean
	// anything. The rest of avih is reserved, strh can be as long as this when its
	// rectangle uses 32-bit values, and dmlh holds one number and then padding.
	avihLayout = 40
	strhLayout = 64
	dmlhLayout = 4
	// vprpHeaderSize is the fixed part of vprp, ending in the number of field
	// descriptions that follow, each vprpFieldSize long. Video has at most two.
	vprpHeaderSize = 36
	vprpFieldSize  = 32
	vprpMaxFields  = 2
	// zeroScanBuffer bounds the memory used to look at a chunk of any size.
	zeroScanBuffer = 64 << 10
)

func init() { core.Register(handler{}) }

type handler struct{}

func (handler) Format() core.Format {
	return core.Format{
		ID:         "avi",
		Name:       "AVI video",
		Family:     core.FamilyVideo,
		Extensions: []string{".avi"},
		Limitations: []string{
			"The chunks in a movi list are read and held to an allowlist, but the frames are copied unread, so anything in the encoded streams stays. Chunks of a declared stream but an unusual type, and text or data streams, stay too, with a warning, because they may be media.",
			"Stream formats and OpenDML indexes stay because players need them, with a warning if they visibly hold text. Encoded data, or a string under 12 characters, cannot be told apart from what players need, and goes unnoticed. Bytes past the end of an audio format are cleared, but a video format can be followed by data its codec needs, so bytes there stay.",
			"An idx1 index is checked entry by entry against the file, and one that does not match is removed, with a warning, so a player must rebuild it to seek.",
			"Values in the file and stream headers, and video properties, are copied as they are, even when players ignore them: 40 bytes in the main header, 64 per stream, and up to 100 per video stream.",
		},
	}
}

func (handler) Match(head []byte, _ core.Source) bool {
	return riff.Form(head) == formAVI
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	p, err := analyse(ctx, src)
	if err != nil {
		return nil, err
	}
	return p.rep, nil
}

// Clean takes no notice of Strict, because nothing here is retained by default.
func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	p, err := analyse(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := core.CopyPatched(ctx, dst, src, p.patches); err != nil {
		return nil, err
	}
	return p.rep, nil
}

// plan is the result of walking a file: the report and the patches that clean it.
// Inspect and Clean both build one, so they cannot disagree.
type plan struct {
	ctx      context.Context
	src      core.Source
	rep      *core.Report
	patches  []core.Patch
	buf      []byte         // allocated on the first zero scan
	streams  []string       // the type of each stream, in order, from its strh
	removed  map[int64]bool // where chunks that are becoming JUNK start; maxPatches bounds it
	findings int
	further  int
	warned   [warningKinds]bool
}

// warning kinds are issued once each, so a hostile file cannot flood the output.
type warning uint8

const (
	warnOpaqueText warning = iota
	warnUnusualChunk
	warnDataStream
	warnBadIndex
	warningKinds
)

func (p *plan) warn(kind warning, message string) {
	if !p.warned[kind] {
		p.warned[kind] = true
		p.rep.Warn("%s", message)
	}
}

func analyse(ctx context.Context, src core.Source) (*plan, error) {
	root, form, err := riff.Open(src)
	if err != nil {
		return nil, err
	}
	if form != formAVI {
		return nil, core.Unsupportedf("avi: RIFF form %q is not AVI", form)
	}
	p := &plan{ctx: ctx, src: src, rep: &core.Report{Format: "avi"}}
	if err := p.firstSegment(root); err != nil {
		return nil, err
	}
	end, err := p.continuationSegments(root.End)
	if err != nil {
		return nil, err
	}
	if size := src.Size(); end < size {
		if err := p.refuseMediaAt(end); err != nil {
			return nil, err
		}
		p.add(findingTrailing, core.ByteCount(size-end), "file")
		if err := p.patch(core.Patch{Off: end, Len: size - end}); err != nil {
			return nil, err
		}
	}
	if p.further > 0 {
		p.rep.Add(core.Finding{
			Category: core.CatHidden, Risk: core.RiskMed, Field: "Further metadata",
			Value: strconv.Itoa(p.further) + " more items", Where: "RIFF", Action: core.ActionRemove,
		})
	}
	slices.SortFunc(p.patches, func(a, b core.Patch) int { return cmp.Compare(a.Off, b.Off) })
	return p, nil
}

// firstSegment walks the RIFF AVI chunk. A file without a header list, a stream or a
// movi list is not a video this handler understands. The movi lists wait for the
// header list, because their chunks are judged against its streams.
func (p *plan) firstSegment(root riff.Chunk) error {
	var sawHeader bool
	var movis, indexes []riff.Chunk
	for c, err := range riff.Children(p.ctx, p.src, root) {
		if err != nil {
			return err
		}
		listType, err := p.listType(c)
		if err != nil {
			return err
		}
		switch {
		case listType == "hdrl" && !sawHeader:
			sawHeader = true
			err = p.headerList(c)
		case listType == "movi":
			if len(movis) >= maxMoviLists {
				return fmt.Errorf("%w: more than %d movi lists", core.ErrTooLarge, maxMoviLists)
			}
			movis = append(movis, c)
		case c.ID == "idx1":
			if len(indexes) >= maxMoviLists {
				return fmt.Errorf("%w: more than %d index chunks", core.ErrTooLarge, maxMoviLists)
			}
			indexes = append(indexes, c)
		default:
			err = p.segmentChunk(c, listType, "RIFF")
		}
		if err != nil {
			return err
		}
	}
	if !sawHeader || len(movis) == 0 {
		return core.Malformedf("avi: the RIFF AVI chunk lacks a header list or a movi list")
	}
	if len(p.streams) == 0 {
		return core.Malformedf("avi: the header list declares no streams")
	}
	for _, movi := range movis {
		if err := p.moviList(movi, "RIFF/movi", false); err != nil {
			return err
		}
	}
	for _, index := range indexes {
		if err := p.checkIndex(index, movis[0]); err != nil {
			return err
		}
	}
	return p.keep(root)
}

// continuationSegments walks the RIFF AVIX chunks that OpenDML appends and returns
// where the last one ends. A chunk that says AVIX but overruns the file is a
// damaged video, not stray bytes, and dropping it would lose content silently.
func (p *plan) continuationSegments(pos int64) (int64, error) {
	size := p.src.Size()
	for size-pos >= riffHeaderSize {
		var head [riffHeaderSize]byte
		if err := p.read(head[:], pos); err != nil {
			return 0, err
		}
		if riff.Form(head[:]) != formAVIX {
			break
		}
		var segment riff.Chunk
		for c, err := range riff.Chunks(p.ctx, p.src, pos, size) {
			if err != nil {
				return 0, err
			}
			segment = c
			break
		}
		for c, err := range riff.Children(p.ctx, p.src, segment) {
			if err != nil {
				return 0, err
			}
			listType, err := p.listType(c)
			if err != nil {
				return 0, err
			}
			if listType == "movi" {
				err = p.moviList(c, "RIFF AVIX/movi", false)
			} else {
				err = p.segmentChunk(c, listType, "RIFF AVIX")
			}
			if err != nil {
				return 0, err
			}
		}
		if err := p.keep(segment); err != nil {
			return 0, err
		}
		pos = segment.End
	}
	return pos, nil
}

// refuseMediaAt refuses a file that goes on past its RIFF chunks with more video. ffmpeg
// plays such files to the end, and two AVI files joined with cat make one, so these are
// no stray bytes: dropping them would silently lose content.
func (p *plan) refuseMediaAt(pos int64) error {
	var head [riffHeaderSize]byte
	n := min(p.src.Size()-pos, riffHeaderSize)
	if err := p.read(head[:n], pos); err != nil {
		return err
	}
	id, kind := string(head[:4]), string(head[8:12])
	switch {
	case n < int64(len(head)):
		return nil
	case id == "RIFF" && kind == formAVI, id == idLIST && kind == "movi", looksLikeFrame(id):
		return core.Unsupportedf("avi: more video follows the end of the RIFF data at offset %d", pos)
	}
	return nil
}

// segmentChunk judges a direct child of a RIFF chunk that is neither the header
// list nor a movi list.
func (p *plan) segmentChunk(c riff.Chunk, listType, where string) error {
	switch {
	case c.ID == idJUNK:
		return p.scrubJunk(c, where)
	case listType == "INFO":
		return p.removeInfo(c, where)
	case listType != "":
		return p.removeUnknown(c, listType, where)
	case c.ID == "idx1":
		return p.junkIndex(c)
	case isStandardIndex(c.ID):
		return p.keepOpaque(c)
	case looksLikeFrame(c.ID):
		return core.Malformedf("avi: frame %q at offset %d is outside a movi list, so a size in this file is wrong", c.ID, c.Offset)
	}
	return p.removeUnknown(c, "", where)
}

// moviList holds a movi list to what belongs there. Reading it costs one small read
// per chunk, so even a file of many gigabytes takes seconds, and nothing is held in
// memory. Lists of type "rec " group the chunks of old interleaved files, and go only
// one level down.
func (p *plan) moviList(list riff.Chunk, where string, nested bool) error {
	for c, err := range riff.Children(p.ctx, p.src, list) {
		if err != nil {
			return err
		}
		listType, err := p.listType(c)
		if err != nil {
			return err
		}
		switch {
		case listType == "rec " && !nested:
			err = p.moviList(c, where, true)
		case listType != "":
			err = p.removeUnknown(c, listType, where)
		case c.ID == idJUNK:
			err = p.scrubJunk(c, where)
		case isStandardIndex(c.ID) && p.declaresStream(c.ID):
			err = p.keepOpaque(c)
		case p.declaresStream(c.ID):
			if !p.usualChunkType(c.ID) {
				p.warn(warnUnusualChunk, "A movi list holds chunks of an unusual type. Players may treat them as frames, so they were kept and not read.")
			}
			err = p.keep(c)
		default:
			err = p.removeUnknown(c, "", where)
		}
		if err != nil {
			return err
		}
	}
	return p.keep(list)
}

// streamNumber reads the number from a chunk ID of the form ##xx or, for a standard
// index, ix##. Stream numbers are hexadecimal in the AVI specification.
func streamNumber(id string) (int, bool) {
	if len(id) != 4 {
		return 0, false
	}
	digits := id[:2]
	if id[:2] == "ix" {
		digits = id[2:]
	}
	n, err := strconv.ParseUint(digits, 16, 8)
	return int(n), err == nil
}

// declaresStream reports whether a chunk ID names a stream the header list declares.
func (p *plan) declaresStream(id string) bool {
	n, ok := streamNumber(id)
	return ok && n < len(p.streams)
}

// usualChunkType reports whether a chunk's type suits its stream. Streams that are
// neither video nor audio already get a warning of their own.
func (p *plan) usualChunkType(id string) bool {
	n, ok := streamNumber(id)
	if !ok || n >= len(p.streams) {
		return false
	}
	kind := id[2:]
	switch p.streams[n] {
	case "vids", "iavs":
		return kind == "dc" || kind == "db" || kind == "pc" || kind == "iv" || kind == "dd" || kind == "__"
	case "auds":
		return kind == "wb"
	}
	return true
}

// looksLikeFrame recognises the IDs of frames by their shape alone, for where no
// frame should be.
func looksLikeFrame(id string) bool {
	kind := id[min(2, len(id)):]
	return (kind == "dc" || kind == "db" || kind == "wb") && len(id) == 4 && isHex(id[:2])
}

// isStandardIndex recognises the OpenDML per-stream index chunks, ix00 or 00ix.
// A stream's indx chunk points at them by absolute offset, so a player that
// follows it must find an index there.
func isStandardIndex(id string) bool {
	return len(id) == 4 && (id[:2] == "ix" && isHex(id[2:]) || id[2:] == "ix" && isHex(id[:2]))
}

func isHex(s string) bool {
	_, err := strconv.ParseUint(s, 16, 8)
	return err == nil
}

// headerList walks LIST hdrl. A file with no avih is not an AVI.
func (p *plan) headerList(list riff.Chunk) error {
	const where = "hdrl"
	var sawMainHeader bool
	for c, err := range riff.Children(p.ctx, p.src, list) {
		if err != nil {
			return err
		}
		listType, err := p.listType(c)
		if err != nil {
			return err
		}
		switch {
		case c.ID == "avih":
			sawMainHeader = true
			err = p.keepHeader(c, avihLayout, where)
		case listType == "strl":
			err = p.streamList(c)
		case listType == "odml":
			err = p.odmlList(c)
		case listType == "INFO":
			err = p.removeInfo(c, where)
		case listType != "":
			err = p.removeUnknown(c, listType, where)
		case c.ID == idJUNK:
			err = p.scrubJunk(c, where)
		default:
			err = p.removeNamedOrUnknown(c, where)
		}
		if err != nil {
			return err
		}
	}
	if !sawMainHeader {
		return core.Malformedf("avi: the header list has no avih chunk")
	}
	return p.keep(list)
}

// streamList walks LIST strl.
func (p *plan) streamList(list riff.Chunk) error {
	const where = "hdrl/strl"
	if len(p.streams) >= maxStreams {
		return fmt.Errorf("%w: more than %d streams", core.ErrTooLarge, maxStreams)
	}
	p.streams = append(p.streams, "")
	for c, err := range riff.Children(p.ctx, p.src, list) {
		if err != nil {
			return err
		}
		switch c.ID {
		case "strh":
			if err = p.noteStreamType(c); err == nil {
				err = p.keepHeader(c, strhLayout, where)
			}
		case "vprp":
			err = p.videoProperties(c, where)
		case "strf":
			err = p.streamFormat(c, where)
		case "indx":
			err = p.keepOpaque(c)
		case idJUNK:
			err = p.scrubJunk(c, where)
		default:
			err = p.removeNamedOrUnknown(c, where)
		}
		if err != nil {
			return err
		}
	}
	return p.keep(list)
}

// odmlList walks LIST odml, whose dmlh chunk holds the true frame count of a
// file spanning several segments.
func (p *plan) odmlList(list riff.Chunk) error {
	const where = "hdrl/odml"
	for c, err := range riff.Children(p.ctx, p.src, list) {
		if err != nil {
			return err
		}
		switch c.ID {
		case "dmlh":
			err = p.keepHeader(c, dmlhLayout, where)
		case idJUNK:
			err = p.scrubJunk(c, where)
		default:
			err = p.removeNamedOrUnknown(c, where)
		}
		if err != nil {
			return err
		}
	}
	return p.keep(list)
}

// streamFormat keeps a stream's format. An audio one says how long it is, in a
// WAVEFORMATEX, so what follows it is spare. A video one can be followed by data its
// codec needs, which nothing here can tell from anything else, so that stays.
func (p *plan) streamFormat(c riff.Chunk, where string) error {
	const waveFormatSize, extraSizeAt = 18, 16
	if p.streams[len(p.streams)-1] != "auds" || c.Size < waveFormatSize {
		return p.keepOpaque(c)
	}
	var extra [2]byte
	if err := p.read(extra[:], c.DataOff+extraSizeAt); err != nil {
		return err
	}
	layout := waveFormatSize + int64(binary.LittleEndian.Uint16(extra[:]))
	if c.Size <= layout {
		return p.keepOpaque(c)
	}
	off, n := c.DataOff+layout, c.Size-layout
	zero, err := p.isZero(off, n)
	if err != nil {
		return err
	}
	if !zero {
		p.add(findingSlack, core.ByteCount(n), where+"/"+c.ID)
		if err := p.patch(core.Patch{Off: off, Len: n, Zero: true}); err != nil {
			return err
		}
	}
	found, err := p.hasReadableText(c.DataOff, layout)
	if err != nil {
		return err
	}
	if found {
		p.warn(warnOpaqueText, "Data kept for players, such as a stream format or an index, holds readable text. It stays in the file.")
	}
	return p.keep(c)
}

// listType returns the type of a LIST chunk, or "" for any other chunk. A nested
// RIFF chunk is not treated as a list, so the allowlists remove it.
func (p *plan) listType(c riff.Chunk) (string, error) {
	if c.ID != idLIST {
		return "", nil
	}
	return riff.ListType(p.src, c)
}

// noteStreamType records what a stream holds. Anything but video and audio can be a
// timed log inside the movi list, such as a dash camera's GPS track.
func (p *plan) noteStreamType(c riff.Chunk) error {
	if c.Size < 4 {
		return nil
	}
	var kind [4]byte
	if err := p.read(kind[:], c.DataOff); err != nil {
		return err
	}
	p.streams[len(p.streams)-1] = string(kind[:])
	switch string(kind[:]) {
	case "vids", "auds", "iavs":
	default:
		p.warn(warnDataStream, "This file has a text or data stream. It was kept and not inspected, and some devices store GPS positions in such streams.")
	}
	return nil
}

// videoProperties keeps a vprp chunk's layout. One too short to hold it is not one.
func (p *plan) videoProperties(c riff.Chunk, where string) error {
	if c.Size < vprpHeaderSize {
		return p.removeUnknown(c, "", where)
	}
	var count [4]byte
	if err := p.read(count[:], c.DataOff+vprpHeaderSize-4); err != nil {
		return err
	}
	fields := int64(min(binary.LittleEndian.Uint32(count[:]), vprpMaxFields))
	return p.keepHeader(c, vprpHeaderSize+fields*vprpFieldSize, where)
}

// keepHeader keeps the first layout bytes of a chunk with a small, fixed-layout
// payload, and zeroes the rest, which nothing reads. One far larger than its layout
// is not understood, and an allowlisted chunk must not be a place to hide anything.
func (p *plan) keepHeader(c riff.Chunk, layout int64, where string) error {
	if c.Size > maxHeaderChunk {
		return core.Malformedf("avi: %q chunk at offset %d is %d bytes long", c.ID, c.Offset, c.Size)
	}
	if c.Size > layout {
		off, n := c.DataOff+layout, c.Size-layout
		zero, err := p.isZero(off, n)
		if err != nil {
			return err
		}
		if !zero {
			p.add(findingSlack, core.ByteCount(n), where+"/"+c.ID)
			if err := p.patch(core.Patch{Off: off, Len: n, Zero: true}); err != nil {
				return err
			}
		}
	}
	return p.keep(c)
}

// Index entries and the flags that mean something in them: the entry is a list, the
// frame is a key frame, or the frame is not to be counted.
const (
	indexEntrySize  = 16
	indexFlagsKnown = 0x111
	indexBufferSize = 64 << 10
)

// checkIndex keeps an idx1 chunk only if it is what it says: a list of the chunks in
// the file. Nothing else would ever notice otherwise, because a player that finds an
// index useless goes without one. Each entry has to name a chunk that is really at the
// place it gives, so that leaves no room in it, past its flags. Offsets count from the
// movi list, or from the start of the file in some old writers, and the first entry
// shows which.
func (p *plan) checkIndex(c, movi riff.Chunk) error {
	entries := c.Size / indexEntrySize
	buf := make([]byte, min(c.Size, indexBufferSize))
	base := int64(-1)
	for i := int64(0); i < entries; {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		chunk := buf[:min((entries-i)*indexEntrySize, indexBufferSize/indexEntrySize*indexEntrySize)]
		if err := p.read(chunk, c.DataOff+i*indexEntrySize); err != nil {
			return err
		}
		for ; len(chunk) >= indexEntrySize; chunk = chunk[indexEntrySize:] {
			i++
			var ok bool
			var err error
			if base, ok, err = p.indexEntryMatches(chunk[:indexEntrySize], base, movi); err != nil {
				return err
			}
			if !ok {
				return p.junkIndex(c)
			}
		}
	}
	if tail := c.Size % indexEntrySize; tail != 0 {
		zero, err := p.isZero(c.DataOff+c.Size-tail, tail)
		if err != nil {
			return err
		}
		if !zero {
			p.add(findingSlack, core.ByteCount(tail), "RIFF/idx1")
			if err := p.patch(core.Patch{Off: c.DataOff + c.Size - tail, Len: tail, Zero: true}); err != nil {
				return err
			}
		}
	}
	return p.keep(c)
}

// junkIndex removes an index that does not describe the file, and warns, since a
// player has to rebuild one to seek.
func (p *plan) junkIndex(c riff.Chunk) error {
	p.add(findingBadIndex, core.ByteCount(c.Size), "RIFF/idx1")
	p.warn(warnBadIndex, "The index did not match the file, so it was removed. Players can usually rebuild one, but seeking may be slower.")
	return p.junk(c)
}

// indexEntryMatches checks one entry against the chunk it points at. base is what its
// offsets count from, or -1 until the first entry has settled that.
func (p *plan) indexEntryMatches(entry []byte, base int64, movi riff.Chunk) (int64, bool, error) {
	id := string(entry[:4])
	flags := binary.LittleEndian.Uint32(entry[4:])
	offset := int64(binary.LittleEndian.Uint32(entry[8:]))
	size := int64(binary.LittleEndian.Uint32(entry[12:]))
	if flags&^indexFlagsKnown != 0 {
		return base, false, nil
	}
	if base >= 0 {
		ok, err := p.chunkIsAt(base+offset, id, size, movi)
		return base, ok, err
	}
	for _, candidate := range []int64{movi.DataOff, 0} {
		ok, err := p.chunkIsAt(candidate+offset, id, size, movi)
		if err != nil {
			return base, false, err
		}
		if ok {
			return candidate, true, nil
		}
	}
	return base, false, nil
}

// chunkIsAt reports whether a chunk with this ID and size will sit at pos in the output,
// inside the movi list. A chunk this clean removes will not, whatever the input says. An
// index lists a record list by the type of the list, not by "LIST".
func (p *plan) chunkIsAt(pos int64, id string, size int64, movi riff.Chunk) (bool, error) {
	const headerSize = 8
	if pos < movi.DataOff || pos+headerSize > movi.DataOff+movi.Size || p.removed[pos] {
		return false, nil
	}
	var head [headerSize + 4]byte
	n := min(int64(len(head)), movi.DataOff+movi.Size-pos)
	if err := p.read(head[:n], pos); err != nil {
		return false, err
	}
	gotSize := int64(binary.LittleEndian.Uint32(head[4:]))
	if string(head[:4]) == idLIST && n == int64(len(head)) && string(head[8:12]) == id {
		return gotSize == size, nil
	}
	return string(head[:4]) == id && gotSize == size, nil
}

// keepOpaque keeps a chunk that players need and that has no fixed layout to hold it
// to, and warns if it visibly holds text.
func (p *plan) keepOpaque(c riff.Chunk) error {
	found, err := p.hasReadableText(c.DataOff, c.Size)
	if err != nil {
		return err
	}
	if found {
		p.warn(warnOpaqueText, "Data kept for players, such as a stream format or an index, holds readable text. It stays in the file.")
	}
	return p.keep(c)
}

// hasReadableText looks for a run of text, in bounded reads that overlap by enough to
// see a run across two.
func (p *plan) hasReadableText(off, n int64) (bool, error) {
	if p.buf == nil && n > 0 {
		p.buf = make([]byte, zeroScanBuffer)
	}
	for n > 0 {
		if err := p.ctx.Err(); err != nil {
			return false, err
		}
		chunk := p.buf[:min(n, zeroScanBuffer)]
		if err := p.read(chunk, off); err != nil {
			return false, err
		}
		if textscan.Has(chunk) {
			return true, nil
		}
		if n <= int64(len(chunk)) {
			return false, nil
		}
		step := int64(len(chunk)) - textscan.Overlap
		off += step
		n -= step
	}
	return false, nil
}

// keep zeroes the pad byte of a chunk that stays. The byte has no meaning, and
// the riff package does not check it, so it could hold anything.
func (p *plan) keep(c riff.Chunk) error {
	padOff := c.DataOff + c.Size
	if c.End == padOff {
		return nil
	}
	var pad [1]byte
	if err := p.read(pad[:], padOff); err != nil {
		return err
	}
	if pad[0] == 0 {
		return nil
	}
	return p.patch(core.Patch{Off: padOff, Len: 1, Zero: true})
}

// scrubJunk zeroes a JUNK chunk's payload, where tools leave their banners and
// old contents, and its pad byte.
func (p *plan) scrubJunk(c riff.Chunk, where string) error {
	zero, err := p.isZero(c.DataOff, c.End-c.DataOff)
	if err != nil || zero {
		return err
	}
	p.add(findingJunk, core.ByteCount(c.Size), where)
	return p.patch(core.Patch{Off: c.DataOff, Len: c.End - c.DataOff, Zero: true})
}

// junk turns a chunk into a zeroed JUNK chunk of the same size. Its size field
// and every offset after it stay as they were.
func (p *plan) junk(c riff.Chunk) error {
	if err := p.patch(core.Patch{Off: c.Offset, Len: int64(len(idJUNK)), Data: []byte(idJUNK)}); err != nil {
		return err
	}
	if p.removed == nil {
		p.removed = make(map[int64]bool)
	}
	p.removed[c.Offset] = true
	return p.patch(core.Patch{Off: c.DataOff, Len: c.End - c.DataOff, Zero: true})
}

func (p *plan) removeUnknown(c riff.Chunk, listType, where string) error {
	id := c.ID
	if listType != "" {
		id += " " + listType
	}
	p.add(findingUnknown, fmt.Sprintf("%q, %s", id, core.ByteCount(c.Size)), where)
	return p.junk(c)
}

// removeNamedOrUnknown removes a chunk the allowlists do not name, describing it
// as well as its ID allows.
func (p *plan) removeNamedOrUnknown(c riff.Chunk, where string) error {
	named, ok := namedChunks[c.ID]
	if !ok {
		return p.removeUnknown(c, "", where)
	}
	value := core.ByteCount(c.Size)
	if named.isText {
		value = p.text(c)
	}
	p.add(named.finding, value, where)
	return p.junk(c)
}

// removeInfo reports each entry of a LIST INFO and turns the whole list to JUNK.
// Damage inside it only stops the description, because the list's bounds are
// already known and everything within them goes.
func (p *plan) removeInfo(list riff.Chunk, where string) error {
	where += "/INFO"
	found := false
	for c, err := range riff.Children(p.ctx, p.src, list) {
		if err != nil {
			break
		}
		found = true
		f, ok := infoFields[c.ID]
		if !ok {
			f = findingOtherInfo
		}
		p.add(f, p.text(c), where)
	}
	if !found {
		p.add(findingInfo, core.ByteCount(list.Size), where)
	}
	return p.junk(list)
}

// text reads a chunk's payload for display. RIFF strings are NUL-terminated, and
// older writers use Latin-1, which would otherwise be dropped as invalid UTF-8.
func (p *plan) text(c riff.Chunk) string {
	buf := make([]byte, min(c.Size, maxValueBytes))
	if p.read(buf, c.DataOff) != nil {
		return ""
	}
	s := strings.TrimRight(string(buf), "\x00")
	if utf8.ValidString(s) {
		return s
	}
	runes := make([]rune, len(s))
	for i := range len(s) {
		runes[i] = rune(s[i])
	}
	return string(runes)
}

// add records a finding, or counts it once the report has reached its limit.
func (p *plan) add(f finding, value, where string) {
	if p.findings >= maxFindings {
		p.further++
		return
	}
	p.findings++
	p.rep.Add(core.Finding{Category: f.category, Risk: f.risk, Field: f.field, Value: value, Where: where, Action: core.ActionRemove})
}

func (p *plan) patch(patch core.Patch) error {
	if patch.Len == 0 {
		return nil
	}
	if len(p.patches) >= maxPatches {
		return fmt.Errorf("%w: more than %d chunks to rewrite", core.ErrTooLarge, maxPatches)
	}
	p.patches = append(p.patches, patch)
	return nil
}

func (p *plan) isZero(off, n int64) (bool, error) {
	if p.buf == nil && n > 0 {
		p.buf = make([]byte, zeroScanBuffer)
	}
	return core.IsZeroRange(p.ctx, p.src, off, n, p.buf)
}

// read fills buf from off. io.ReaderAt allows io.EOF alongside a full read at the
// very end of the input, so only a short read counts as a failure.
func (p *plan) read(buf []byte, off int64) error {
	n, err := p.src.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("avi: read %d bytes at offset %d: %w", len(buf), off, err)
}
