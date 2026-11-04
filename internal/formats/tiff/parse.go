package tiff

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	headerSize = 8
	entrySize  = 12
	// directoryOverhead is the entry count before a directory's table plus the
	// next-directory pointer after it.
	directoryOverhead = 2 + 4

	// These caps bound the work and memory a hostile file can demand. Real files
	// stay far below them: a directory has a few dozen entries and even a slide
	// scanner's pyramid has a few million tiles.
	maxEntriesPerIFD = 4096
	maxTotalEntries  = 1 << 20
	maxPages         = 1 << 16
	maxStrips        = 1 << 22
	maxJPEGCuts      = 1 << 16
	maxSamples       = 1024
	maxBitsPerSample = 64

	// outputSlack and doubling the input size allow for shared values and strips,
	// which the output writes out once per reference, while stopping a tiny file
	// from pointing a million strips at one region to fill the user's disk.
	outputSlack = 64 << 20
)

// span is a range of the source. cuts are JPEG segments inside it to leave out,
// relative to off and sorted.
type span struct {
	off, size int64
	cuts      []core.Patch
}

func (s span) end() int64 {
	return s.off + s.size
}

// outSize is how many bytes the span contributes to the output.
func (s span) outSize() int64 {
	n := s.size
	for _, c := range s.cuts {
		n += int64(len(c.Data)) - c.Len
	}
	return n
}

// entry is one directory entry as stored. raw is either the value itself or its
// offset, depending on the size of the value.
type entry struct {
	tag, typ uint16
	count    uint32
	raw      [4]byte
}

func (e entry) byteSize() int64 {
	return typeSize(e.typ) * int64(e.count)
}

// scalar returns the value of a single integer.
func (e entry) scalar(order binary.ByteOrder) (uint32, bool) {
	if e.count != 1 {
		return 0, false
	}
	switch e.typ {
	case typeByte:
		return uint32(e.raw[0]), true
	case typeShort:
		return uint32(order.Uint16(e.raw[:])), true
	case typeLong, typeIFD:
		return order.Uint32(e.raw[:]), true
	}
	return 0, false
}

// directory is one IFD in the page chain, with entries sorted by tag.
type directory struct {
	entries []entry
}

func (d *directory) find(tag uint16) (entry, bool) {
	i, ok := slices.BinarySearchFunc(d.entries, tag, func(e entry, t uint16) int { return int(e.tag) - int(t) })
	if !ok {
		return entry{}, false
	}
	return d.entries[i], true
}

func (d *directory) has(tag uint16) bool {
	_, ok := d.find(tag)
	return ok
}

func (d *directory) hasImageData() bool {
	return d.has(tagStripOffsets) || d.has(tagTileOffsets)
}

// isDetachedThumbnail recognises the EXIF-style IFD1: a directory with no strips
// or tiles, only a JPEG preview. That is the one legitimate use of the old-style
// JPEG tags, and unlike an old-style JPEG page it can be dropped without loss.
func (d *directory) isDetachedThumbnail() bool {
	return !d.hasImageData() && d.has(tagJPEGThumbnail)
}

// isPreview reports whether the page declares itself a reduced-resolution copy of
// another image in the file.
func (d *directory) isPreview(order binary.ByteOrder) bool {
	if e, ok := d.find(tagNewSubfileType); ok {
		if v, ok := e.scalar(order); ok && v&1 != 0 {
			return true
		}
	}
	if e, ok := d.find(tagSubfileType); ok {
		if v, ok := e.scalar(order); ok && v == 2 {
			return true
		}
	}
	return false
}

// parser holds the state of one parse. Nothing outlives it except the document,
// so the handler itself stays stateless and safe for concurrent use.
type parser struct {
	ctx    context.Context
	src    core.Source
	opts   core.Options
	order  binary.ByteOrder
	report *core.Report
	seen   map[string]bool
	window window

	// metadata holds the ranges of everything being removed, and image the ranges
	// of what stays; the two must not overlap. shared holds the rendering values of
	// dropped pages, which may. Together they say which bytes nothing refers to.
	metadata, image, shared []span

	entries      int
	strips       int
	cuts         int
	jpegScanned  int64 // bytes of JPEG data walked so far
	reportBudget int64
}

// document is a parsed file: all Inspect needs in report and all Clean needs in
// pages. Both come from the one parse, which is what keeps the two in agreement.
type document struct {
	src    core.Source
	order  binary.ByteOrder
	pages  []*page
	report *core.Report
}

func parse(ctx context.Context, src core.Source, opts core.Options) (*document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &parser{
		ctx:          ctx,
		src:          src,
		opts:         opts,
		report:       &core.Report{Format: formatID},
		seen:         make(map[string]bool),
		metadata:     []span{{off: 0, size: headerSize}},
		reportBudget: maxReportBytes,
	}
	first, err := p.readHeader()
	if err != nil {
		return nil, err
	}
	dirs, err := p.readChain(first)
	if err != nil {
		return nil, err
	}
	if err := p.refuseVariants(dirs); err != nil {
		return nil, err
	}

	doc := &document{src: src, order: p.order, report: p.report}
	// A file of nothing but previews has no full page to fall back on, so its
	// previews are the content.
	hasFullPage := slices.ContainsFunc(dirs, func(d *directory) bool {
		return d.hasImageData() && !d.isPreview(p.order)
	})
	for i, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		where := "page " + strconv.Itoa(i+1)
		if dir.isDetachedThumbnail() || (hasFullPage && dir.isPreview(p.order)) {
			p.dropPage(dir, where)
			continue
		}
		pg, err := p.buildPage(dir, where)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		doc.pages = append(doc.pages, pg)
	}
	if len(doc.pages) == 0 {
		return nil, core.Malformedf("tiff: no page holds image data")
	}
	if overlapping(p.image, p.metadata) {
		return nil, core.Malformedf("tiff: image data overlaps metadata, so it cannot be kept without keeping the metadata")
	}
	if err := p.reportUnreferenced(); err != nil {
		return nil, err
	}
	if err := doc.layout(growthLimit(src.Size())); err != nil {
		return nil, err
	}
	return doc, nil
}

func growthLimit(size int64) int64 {
	return size*2 + outputSlack
}

// refuseVariants turns away files that would lose pictures if rewritten as plain
// TIFF. It looks at every page first, since a raw file's second page gives it away
// as surely as its first.
func (p *parser) refuseVariants(dirs []*directory) error {
	for i, dir := range dirs {
		for _, e := range dir.entries {
			if why, refused := refusedTags[e.tag]; refused {
				return core.Unsupportedf("tiff: page %d has %s", i+1, why)
			}
		}
		if p.hasPhaseOneMakerNote(dir) {
			return core.Unsupportedf("tiff: this is a Phase One IIQ camera raw file")
		}
	}
	if n := p.imageJFrames(dirs[0]); n > len(dirs) {
		return core.Unsupportedf("tiff: this ImageJ stack declares %d frames but only %d have directories, and the others would be lost", n, len(dirs))
	}
	return nil
}

// hasPhaseOneMakerNote reports whether a page's Exif block holds the maker note of an IIQ
// raw file. Only that note leads to the raw image, so the rest of the file passes for a
// small ordinary picture, and removing the note would lose the photograph. A damaged
// block is left for the report to describe.
func (p *parser) hasPhaseOneMakerNote(dir *directory) bool {
	ptr, ok := dir.find(tagExifIFD)
	if !ok || ptr.byteSize() != 4 {
		return false
	}
	off := int64(p.order.Uint32(ptr.raw[:]))
	countBytes, err := p.read(off, 2)
	if err != nil {
		return false
	}
	table, err := p.read(off+2, int64(p.order.Uint16(countBytes))*entrySize)
	if err != nil {
		return false
	}
	for ; len(table) >= entrySize; table = table[entrySize:] {
		if p.order.Uint16(table) != tagMakerNote {
			continue
		}
		note := entry{tag: tagMakerNote, typ: p.order.Uint16(table[2:]), count: p.order.Uint32(table[4:])}
		if note.byteSize() < phaseOneSignatureSize {
			return false
		}
		head, err := p.read(int64(p.order.Uint32(table[8:])), phaseOneSignatureSize)
		return err == nil && isPhaseOneSignature(head)
	}
	return false
}

// phaseOneSignatureSize is how many bytes of a maker note isPhaseOneSignature needs.
const phaseOneSignatureSize = 8

// isPhaseOneSignature recognises "IIII?waR" and "MMMMRaw?", the same test ExifTool uses.
func isPhaseOneSignature(head []byte) bool {
	s := string(head)
	return s[:4] == "IIII" && s[5:8] == "waR" || s[:7] == "MMMMRaw"
}

// imageJFrames returns how many frames an ImageJ description declares, or 0 if the
// page has no such description. ImageJ writes very large stacks with one directory
// and the remaining frames simply following the first, with nothing but this text
// to say that they exist, so they look exactly like hidden data.
func (p *parser) imageJFrames(dir *directory) int {
	e, ok := dir.find(tagImageDescription)
	if !ok || e.byteSize() > maxTextValue {
		return 0
	}
	text, err := p.value(e)
	if err != nil || !bytes.HasPrefix(text, []byte("ImageJ=")) {
		return 0
	}
	for line := range strings.SplitSeq(string(text), "\n") {
		if value, found := strings.CutPrefix(line, "images="); found {
			n, err := strconv.Atoi(strings.TrimRight(value, "\r\x00"))
			if err == nil {
				return n
			}
		}
	}
	return 0
}

func (p *parser) readHeader() (uint32, error) {
	head, err := p.read(0, min(p.src.Size(), headerSize+2))
	if err != nil {
		return 0, err
	}
	if len(head) < 4 {
		return 0, core.Unsupportedf("tiff: %d bytes is too short to be a TIFF file", len(head))
	}
	switch string(head[:4]) {
	case magicLittle:
		p.order = binary.LittleEndian
	case magicBig:
		p.order = binary.BigEndian
	case magicBigTIFFLittle, magicBigTIFFBig:
		return 0, core.Unsupportedf("tiff: BigTIFF is not supported")
	case magicORF, magicORFBig, magicORFAlternate:
		return 0, core.Unsupportedf("tiff: this is an Olympus ORF camera raw file")
	case magicRW2:
		return 0, core.Unsupportedf("tiff: this is a Panasonic RW2 camera raw file")
	default:
		return 0, core.Unsupportedf("tiff: not a TIFF file")
	}
	if len(head) < headerSize {
		return 0, core.Malformedf("tiff: %d bytes is too short for a TIFF header", len(head))
	}
	if len(head) >= headerSize+2 && string(head[8:10]) == "CR" {
		return 0, core.Unsupportedf("tiff: this is a Canon CR2 camera raw file")
	}
	first := p.order.Uint32(head[4:])
	if first < headerSize {
		return 0, core.Malformedf("tiff: first directory offset %d points into the header", first)
	}
	return first, nil
}

// readChain reads every directory in the page chain. It reads the whole chain
// before deciding anything, because whether a page may be dropped depends on the
// other pages.
func (p *parser) readChain(first uint32) ([]*directory, error) {
	var dirs []*directory
	seen := make(map[uint32]bool)
	for off := first; off != 0; {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		if seen[off] {
			return nil, core.Malformedf("tiff: directory at offset %d was already visited, so the pages form a cycle", off)
		}
		if len(dirs) == maxPages {
			return nil, fmt.Errorf("%w: more than %d pages", core.ErrTooLarge, maxPages)
		}
		seen[off] = true
		dir, next, err := p.readPageDirectory(off)
		if err != nil {
			return nil, err
		}
		if next != 0 && next < headerSize {
			return nil, core.Malformedf("tiff: next directory offset %d points into the header", next)
		}
		dirs = append(dirs, dir)
		off = next
	}
	return dirs, nil
}

func (p *parser) readPageDirectory(off uint32) (*directory, uint32, error) {
	entries, next, err := p.readEntries(off)
	if err != nil {
		return nil, 0, err
	}
	if len(entries) == 0 {
		return nil, 0, core.Malformedf("tiff: directory at offset %d is empty", off)
	}
	p.entries += len(entries)
	if p.entries > maxTotalEntries {
		return nil, 0, fmt.Errorf("%w: more than %d directory entries", core.ErrTooLarge, maxTotalEntries)
	}
	// Two readers could pick different copies of a repeated tag, so what this
	// handler validated might not be what a viewer uses.
	slices.SortStableFunc(entries, func(a, b entry) int { return int(a.tag) - int(b.tag) })
	for i := 1; i < len(entries); i++ {
		if entries[i].tag == entries[i-1].tag {
			return nil, 0, core.Malformedf("tiff: tag %d appears twice in one directory", entries[i].tag)
		}
	}
	return &directory{entries: entries}, next, nil
}

// readEntries reads a directory's table and records its bytes as metadata.
func (p *parser) readEntries(off uint32) ([]entry, uint32, error) {
	countBytes, err := p.read(int64(off), 2)
	if err != nil {
		return nil, 0, err
	}
	count := int(p.order.Uint16(countBytes))
	if count > maxEntriesPerIFD {
		return nil, 0, fmt.Errorf("%w: directory has %d entries, over the limit of %d", core.ErrTooLarge, count, maxEntriesPerIFD)
	}
	table, err := p.read(int64(off)+2, int64(count)*entrySize+4)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]entry, count)
	for i := range entries {
		raw := table[i*entrySize:]
		entries[i] = entry{
			tag:   p.order.Uint16(raw),
			typ:   p.order.Uint16(raw[2:]),
			count: p.order.Uint32(raw[4:]),
		}
		copy(entries[i].raw[:], raw[8:12])
	}
	p.metadata = append(p.metadata, span{off: int64(off), size: int64(count)*entrySize + directoryOverhead})
	return entries, p.order.Uint32(table[count*entrySize:]), nil
}

// read returns n bytes at off. Callers cap n or know it to be small; the check
// here guarantees only that a file cannot demand more memory than its own size.
func (p *parser) read(off, n int64) ([]byte, error) {
	if !p.inBounds(off, n) {
		return nil, core.Malformedf("tiff: %d bytes at offset %d are outside the %d-byte file", n, off, p.src.Size())
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(p.src, off, n), buf); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return buf, nil
}

func (p *parser) inBounds(off, n int64) bool {
	return off >= 0 && n >= 0 && off <= p.src.Size() && n <= p.src.Size()-off
}

// value returns an entry's bytes wherever they are stored.
func (p *parser) value(e entry) ([]byte, error) {
	size := e.byteSize()
	if size <= 4 {
		return e.raw[:size], nil
	}
	return p.read(int64(p.order.Uint32(e.raw[:])), size)
}

// location returns the range of a value stored outside its entry. It is empty for
// one stored inline, which cannot hide anything once the directory is rewritten.
func (p *parser) location(e entry) span {
	size := e.byteSize()
	if size <= 4 {
		return span{}
	}
	return span{off: int64(p.order.Uint32(e.raw[:])), size: size}
}

// integers decodes an array of BYTE, SHORT or LONG values. Strip tables can be
// long, so this counts them against maxStrips before allocating.
func (p *parser) integers(e entry) ([]uint32, error) {
	if !integers.has(e.typ) {
		return nil, core.Malformedf("tiff: tag %d has type %d where integers belong", e.tag, e.typ)
	}
	raw, err := p.value(e)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, e.count)
	for i := range out {
		switch e.typ {
		case typeByte:
			out[i] = uint32(raw[i])
		case typeShort:
			out[i] = uint32(p.order.Uint16(raw[2*i:]))
		default:
			out[i] = p.order.Uint32(raw[4*i:])
		}
	}
	return out, nil
}

// noteMetadata records a removed value's range. A value running off the end of the
// file is left out: it is damage for the report to mention, not a claim on the
// bytes it covers, so it must not make its neighbours look like an overlap.
func (p *parser) noteMetadata(s span) {
	if s.size > 0 && p.inBounds(s.off, s.size) {
		p.metadata = append(p.metadata, s)
	}
}

func byOffset(x, y span) int {
	return cmp.Compare(x.off, y.off)
}

// overlapping reports whether any range in a intersects any range in b.
func overlapping(a, b []span) bool {
	slices.SortFunc(a, byOffset)
	slices.SortFunc(b, byOffset)
	for i, j := 0, 0; i < len(a) && j < len(b); {
		x, y := a[i], b[j]
		if x.off < y.end() && y.off < x.end() {
			return true
		}
		// The range that ends first cannot reach anything later in the other list.
		if x.end() <= y.end() {
			i++
		} else {
			j++
		}
	}
	return false
}

// reportUnreferenced finds bytes no tag refers to. They never reach the output
// whatever they hold, but a user should hear about them, because this is where a
// previous version's metadata survives after an editor appends a new directory
// and where steganography tools hide data. Zero bytes are ordinary padding.
func (p *parser) reportUnreferenced() error {
	all := slices.Concat(p.metadata, p.image, p.shared)
	slices.SortFunc(all, byOffset)
	var hidden, pos int64
	check := func(end int64) error {
		if end <= pos {
			return nil
		}
		zero, err := core.IsZeroRange(p.ctx, p.src, pos, end-pos, make([]byte, min(end-pos, windowSize)))
		if err != nil {
			return err
		}
		if !zero {
			hidden += end - pos
		}
		return nil
	}
	for _, s := range all {
		if s.size == 0 {
			continue
		}
		if err := check(s.off); err != nil {
			return err
		}
		pos = max(pos, s.end())
	}
	if err := check(p.src.Size()); err != nil {
		return err
	}
	if hidden > 0 {
		// Unknown contents get the highest rating: they may hold anything.
		p.add(core.Finding{
			Category: core.CatHidden,
			Risk:     core.RiskHigh,
			Field:    "Unreferenced data",
			Value:    strconv.FormatInt(hidden, 10) + " bytes that no tag refers to",
			Action:   core.ActionRemove,
		})
	}
	// More hidden than visible is unusual enough to mention, because some software
	// keeps real frames this way and not every such file can be recognised.
	var visible int64
	for _, s := range p.image {
		visible += s.size
	}
	if hidden > visible {
		p.report.Warn("%d bytes that no tag refers to were dropped, more than the %d bytes of image kept; if this file should hold more frames than the output shows, keep the original", hidden, visible)
		p.report.MayLoseContent = true
	}
	return nil
}

// window is a read-ahead buffer for the byte-at-a-time JPEG scan, so that scanning
// a strip costs one read per windowSize bytes rather than one per byte.
type window struct {
	buf  []byte
	base int64
}

const windowSize = 64 << 10

// at returns the byte at pos, which the caller has checked lies inside the file.
func (p *parser) at(pos int64) (byte, error) {
	w := &p.window
	if i := pos - w.base; i >= 0 && i < int64(len(w.buf)) {
		return w.buf[i], nil
	}
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n := min(p.src.Size()-pos, windowSize)
	if cap(w.buf) < windowSize {
		w.buf = make([]byte, windowSize)
	}
	w.buf = w.buf[:n]
	if _, err := io.ReadFull(io.NewSectionReader(p.src, pos, n), w.buf); err != nil {
		w.buf = w.buf[:0]
		return 0, fmt.Errorf("read: %w", err)
	}
	w.base = pos
	return w.buf[0], nil
}
