package tiff

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
)

// field is one tag in the output. Its value is inline bytes, a span of the source,
// or, for the strip tables, numbers that only exist once the output is laid out.
type field struct {
	tag, typ uint16
	count    uint32
	inline   [4]byte
	value    span
	array    []uint32
	offset   uint32 // set by layout for a value stored outside the entry
}

// outOfLine reports whether the value needs its own place in the output.
func (f *field) outOfLine() bool {
	return f.size() > 4
}

func (f *field) size() int64 {
	if f.array != nil {
		return 4 * int64(f.count)
	}
	return f.value.outSize()
}

// page is one image to write.
type page struct {
	fields     []field // sorted by tag
	strips     []span
	stripOffs  []uint32 // set by layout
	offset     uint32   // set by layout
	offsetsTag uint16
	countsTag  uint16
}

// geometry holds the tags that decide how much pixel data a page must have.
type geometry struct {
	width, height uint32
	samples       uint32
	planar        uint32
	compression   uint32
	photometric   uint32
	rowsPerStrip  uint32
	tiled         bool
	tileWidth     uint32
	tileHeight    uint32
	subsampleH    uint32
	subsampleV    uint32
	bits          []uint32
	pixelBits     uint64 // the sum of the bit depths of all samples
}

// buildPage validates a page and decides what becomes of each of its tags.
func (p *parser) buildPage(dir *directory, where string) (*page, error) {
	g, err := p.readGeometry(dir)
	if err != nil {
		return nil, err
	}
	pg := &page{}
	var removed []entry
	for _, e := range dir.entries {
		switch e.tag {
		case tagOrientation:
			p.applyOrientation(pg, e, where)
			continue
		case tagICCProfile:
			if err := p.applyICC(pg, e, where); err != nil {
				return nil, err
			}
			continue
		}
		rule, ok := allowed[e.tag]
		if !ok || !applies(e.tag, g) {
			removed = append(removed, e)
			continue
		}
		if err := checkShape(e, rule, g); err != nil {
			return nil, err
		}
		switch e.tag {
		case tagStripOffsets, tagStripByteCounts, tagTileOffsets, tagTileByteCounts:
			// Rewritten by layout from the strips below, so the old tables go.
			p.noteMetadata(p.location(e))
		case tagJPEGTables:
			if err := p.applyJPEGTables(pg, e, g, where); err != nil {
				return nil, err
			}
		default:
			f, err := p.keep(e)
			if err != nil {
				return nil, err
			}
			pg.fields = append(pg.fields, f)
		}
	}
	if err := p.readStrips(dir, pg, g, where); err != nil {
		return nil, err
	}
	slices.SortFunc(pg.fields, func(a, b field) int { return int(a.tag) - int(b.tag) })
	p.reportRemoved(removed, where)
	return pg, nil
}

// applies reports whether an allowlisted tag means anything on this page. Nothing
// reads a colour map on an image that is not a palette image, and it holds up to
// 393 KiB of whatever a writer likes, so it goes like any other metadata. The same
// for tile sizes on a page of strips.
func applies(tag uint16, g geometry) bool {
	switch tag {
	case tagColorMap:
		return g.photometric == photometricPalette
	case tagTileWidth, tagTileLength:
		return g.tiled
	}
	return true
}

func (p *parser) readGeometry(dir *directory) (geometry, error) {
	g := geometry{}
	scalars := []struct {
		tag      uint16
		dst      *uint32
		fallback uint32
		required bool
	}{
		{tagCompression, &g.compression, compressionNone, false},
		{tagImageWidth, &g.width, 0, true},
		{tagImageLength, &g.height, 0, true},
		{tagPhotometric, &g.photometric, 0, true},
		{tagSamplesPerPixel, &g.samples, 1, false},
		{tagPlanarConfig, &g.planar, 1, false},
		{tagRowsPerStrip, &g.rowsPerStrip, math.MaxUint32, false},
	}
	for _, s := range scalars {
		e, ok := dir.find(s.tag)
		if !ok {
			if s.required {
				return g, core.Malformedf("tiff: required tag %d is missing", s.tag)
			}
			*s.dst = s.fallback
			continue
		}
		v, ok := e.scalar(p.order)
		if !ok {
			return g, core.Malformedf("tiff: tag %d is not a single integer", s.tag)
		}
		*s.dst = v
		// Refusals come first so the user learns the file is out of scope rather
		// than that some other tag in a format this handler does not read is odd.
		if s.tag == tagCompression {
			if v == compressionOldJPEG {
				return g, core.Unsupportedf("tiff: old-style JPEG compression is not supported")
			}
			if !supportedCompression[v] {
				return g, core.Unsupportedf("tiff: compression %d is not supported", v)
			}
		}
	}
	if !supportedPhotometric[g.photometric] {
		return g, core.Unsupportedf("tiff: photometric interpretation %d is not supported", g.photometric)
	}
	switch {
	case g.width == 0 || g.height == 0:
		return g, core.Malformedf("tiff: image is %d by %d", g.width, g.height)
	case g.samples == 0 || g.samples > maxSamples:
		return g, core.Malformedf("tiff: %d samples per pixel", g.samples)
	case g.planar != 1 && g.planar != 2:
		return g, core.Malformedf("tiff: planar configuration %d", g.planar)
	case g.rowsPerStrip == 0:
		return g, core.Malformedf("tiff: zero rows per strip")
	}
	g.bits = []uint32{1}
	if e, ok := dir.find(tagBitsPerSample); ok {
		if e.count != 1 && e.count != g.samples {
			return g, core.Malformedf("tiff: %d bit depths for %d samples", e.count, g.samples)
		}
		bits, err := p.integers(e)
		if err != nil {
			return g, err
		}
		g.bits = bits
	}
	for _, b := range g.bits {
		if b == 0 || b > maxBitsPerSample {
			return g, core.Malformedf("tiff: %d bits per sample", b)
		}
	}
	for i := range uint64(g.samples) {
		g.pixelBits += uint64(g.sampleBits(i))
	}
	g.subsampleH, g.subsampleV = 2, 2 // the default the specification gives
	if e, ok := dir.find(tagYCbCrSubSamp); ok {
		if values, err := p.integers(e); err == nil && len(values) == 2 {
			g.subsampleH, g.subsampleV = values[0], values[1]
		}
	}
	return g, p.readTileGeometry(dir, &g)
}

func (p *parser) readTileGeometry(dir *directory, g *geometry) error {
	if !dir.has(tagTileOffsets) && !dir.has(tagTileByteCounts) {
		return nil
	}
	g.tiled = true
	for _, t := range []struct {
		tag uint16
		dst *uint32
	}{{tagTileWidth, &g.tileWidth}, {tagTileLength, &g.tileHeight}} {
		e, ok := dir.find(t.tag)
		if !ok {
			return core.Malformedf("tiff: tiled image is missing tag %d", t.tag)
		}
		v, ok := e.scalar(p.order)
		if !ok || v == 0 {
			return core.Malformedf("tiff: tag %d is not a usable tile size", t.tag)
		}
		*t.dst = v
	}
	return nil
}

// checkShape rejects an allowlisted tag whose type or count could not be what the
// tag means. An oversized BitsPerSample, say, is a place to hide anything at all.
func checkShape(e entry, rule shape, g geometry) error {
	if !rule.types.has(e.typ) {
		return core.Malformedf("tiff: tag %d has type %d", e.tag, e.typ)
	}
	ok := true
	switch rule.count {
	case countOne:
		ok = e.count == 1
	case countFixed:
		ok = e.count == rule.n
	case countPerSample:
		ok = e.count == 1 || e.count == g.samples
	case countUpToSamples:
		ok = e.count <= g.samples
	case countColorMap:
		ok = g.bits[0] <= 16 && e.count == 3<<g.bits[0]
	case countDotRange:
		ok = e.count == 2 || e.count == 2*g.samples
	}
	if !ok {
		return core.Malformedf("tiff: tag %d has %d values", e.tag, e.count)
	}
	return nil
}

// keep turns an allowlisted entry into an output field. Inline values are copied
// by their real size, so whatever the unused bytes of the value slot held is gone.
func (p *parser) keep(e entry) (field, error) {
	f := field{tag: e.tag, typ: e.typ, count: e.count}
	size := e.byteSize()
	if size <= 4 {
		copy(f.inline[:], e.raw[:size])
		f.value = span{size: size}
		return f, nil
	}
	s := p.location(e)
	if !p.inBounds(s.off, s.size) {
		return f, core.Malformedf("tiff: tag %d has a %d-byte value at offset %d, outside the file", e.tag, s.size, s.off)
	}
	f.value = s
	p.image = append(p.image, s)
	return f, nil
}

const orientationReason = "needed to display the image the right way up; strict mode removes it"

// applyOrientation keeps a valid orientation unless Strict. It is written as a
// plain SHORT whatever the input used, the one form the specification allows.
func (p *parser) applyOrientation(pg *page, e entry, where string) {
	finding := core.Finding{
		Category: core.CatTechnical,
		Risk:     core.RiskNone,
		Field:    "Orientation",
		Value:    "invalid",
		Where:    where,
		Action:   core.ActionRemove,
	}
	p.noteMetadata(p.location(e))
	v, ok := e.scalar(p.order)
	valid := ok && v >= 1 && v <= 8
	if valid {
		finding.Value = orientationText(uint16(v))
	}
	if valid && !p.opts.Strict {
		finding.Action = core.ActionRetain
		finding.Reason = orientationReason
		f := field{tag: tagOrientation, typ: typeShort, count: 1, value: span{size: 2}}
		p.order.PutUint16(f.inline[:], uint16(v))
		pg.fields = append(pg.fields, f)
	}
	p.add(finding)
}

// orientationText borrows the exif package's wording, so that this reads the same
// as an orientation found in a JPEG. v is a valid orientation, which makes Minimal's
// block one with exactly one tag and so exactly one finding.
func orientationText(v uint16) string {
	d, _ := exif.Parse(exif.Minimal(v))
	return d.Findings("", core.Options{})[0].Value
}

const (
	iccReason = "needed to display colours correctly; strict mode removes it"
	// iccMinSize and iccSignatureOffset come from the ICC header, which is 128
	// bytes long and holds "acsp" at this position.
	iccMinSize         = 128
	iccSignatureOffset = 36
)

// applyICC keeps a real colour profile unless Strict. Anything else claiming the
// tag is removed, because retaining it would wave through whatever it holds.
func (p *parser) applyICC(pg *page, e entry, where string) error {
	finding := core.Finding{
		Category: core.CatTechnical,
		Risk:     core.RiskNone,
		Field:    "ICC colour profile",
		Where:    where,
		Action:   core.ActionRemove,
	}
	s := p.location(e)
	if !rawBytes.has(e.typ) || !p.isICCProfile(s) {
		finding.Value = "not a valid profile"
		finding.Risk = core.RiskLow
		p.noteMetadata(s)
		p.add(finding)
		return nil
	}
	finding.Value = strconv.FormatInt(s.size, 10) + " bytes"
	if p.opts.Strict {
		p.noteMetadata(s)
		p.add(finding)
		return nil
	}
	finding.Action = core.ActionRetain
	finding.Reason = iccReason
	p.add(finding)
	f, err := p.keep(e)
	if err != nil {
		return err
	}
	pg.fields = append(pg.fields, f)
	return nil
}

func (p *parser) isICCProfile(s span) bool {
	if s.size < iccMinSize || !p.inBounds(s.off, s.size) {
		return false
	}
	signature, err := p.read(s.off+iccSignatureOffset, 4)
	if err != nil || string(signature) != "acsp" {
		return false
	}
	// A profile states its size, and bytes past that are no part of it.
	declared, err := p.read(s.off, 4)
	return err == nil && int64(binary.BigEndian.Uint32(declared)) == s.size
}

// applyJPEGTables keeps JPEG tables only where they mean something and only their
// real segments. On a page that is not JPEG-compressed nothing would ever read
// them, which makes them a free place to hide data. Four bytes or fewer cannot
// hold a table at all, so they are dropped, here and after filtering, keeping the
// value out of line and the second pass through a cleaned file the same as the first.
func (p *parser) applyJPEGTables(pg *page, e entry, g geometry, where string) error {
	if g.compression != compressionJPEG {
		return core.Malformedf("tiff: JPEG tables on a page with compression %d", g.compression)
	}
	if e.byteSize() <= 4 {
		return nil
	}
	s := p.location(e)
	if !p.inBounds(s.off, s.size) {
		return core.Malformedf("tiff: JPEG tables at offset %d are outside the file", s.off)
	}
	filtered, err := p.scanJPEG(s, where)
	if err != nil {
		return err
	}
	if filtered.outSize() <= 4 {
		return nil
	}
	p.image = append(p.image, span{off: filtered.off, size: filtered.size})
	pg.fields = append(pg.fields, field{tag: e.tag, typ: e.typ, count: uint32(filtered.outSize()), value: filtered})
	return nil
}

// readStrips validates the strip or tile tables and decides which bytes each
// strip contributes. Tables longer than the image needs are refused rather than
// trimmed: extra entries are invisible to any viewer, so they can only be hiding
// something, and a file that disagrees with itself is not one to guess about.
func (p *parser) readStrips(dir *directory, pg *page, g geometry, where string) error {
	pg.offsetsTag, pg.countsTag = tagStripOffsets, tagStripByteCounts
	if g.tiled {
		if dir.has(tagStripOffsets) || dir.has(tagStripByteCounts) {
			return core.Malformedf("tiff: page has both strips and tiles")
		}
		pg.offsetsTag, pg.countsTag = tagTileOffsets, tagTileByteCounts
	}
	offsetsEntry, hasOffsets := dir.find(pg.offsetsTag)
	countsEntry, hasCounts := dir.find(pg.countsTag)
	if !hasOffsets || !hasCounts {
		return core.Malformedf("tiff: page lacks tag %d or %d", pg.offsetsTag, pg.countsTag)
	}
	if offsetsEntry.count != countsEntry.count {
		return core.Malformedf("tiff: %d data offsets but %d byte counts", offsetsEntry.count, countsEntry.count)
	}
	want := g.partCount()
	if uint64(offsetsEntry.count) != want {
		return core.Malformedf("tiff: %d data parts where the image dimensions call for %d", offsetsEntry.count, want)
	}
	if !p.chargeStrips(offsetsEntry.count) {
		return fmt.Errorf("%w: more than %d strips or tiles", core.ErrTooLarge, maxStrips)
	}
	n := int(offsetsEntry.count)
	offsets, err := p.integers(offsetsEntry)
	if err != nil {
		return err
	}
	counts, err := p.integers(countsEntry)
	if err != nil {
		return err
	}
	pg.strips = make([]span, n)
	for i := range n {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		s, err := p.readStrip(int64(offsets[i]), int64(counts[i]), i, g, where)
		if err != nil {
			return err
		}
		pg.strips[i] = s
	}
	// Placeholder arrays so the fields sort and size correctly; layout fills them.
	pg.fields = append(pg.fields,
		field{tag: pg.offsetsTag, typ: typeLong, count: uint32(n), array: make([]uint32, n)},
		field{tag: pg.countsTag, typ: typeLong, count: uint32(n), array: make([]uint32, n)},
	)
	return nil
}

func (p *parser) readStrip(off, size int64, index int, g geometry, where string) (span, error) {
	// An empty strip is how sparse files say "all background"; its offset means
	// nothing and is often zero.
	if size == 0 {
		return span{}, nil
	}
	if !p.inBounds(off, size) {
		return span{}, core.Malformedf("tiff: strip %d (%d bytes at offset %d) is outside the file", index, size, off)
	}
	s := span{off: off, size: size}
	switch {
	case g.compression == compressionJPEG:
		filtered, err := p.scanJPEG(s, where)
		if err != nil {
			return span{}, err
		}
		s = filtered
	case g.compression == compressionNone && g.photometric == photometricYCbCr:
		if err := g.checkUncompressedYCbCr(index, size); err != nil {
			return span{}, err
		}
	case g.compression == compressionNone:
		// Uncompressed pixels have a known size, so anything beyond it is not pixels.
		s.size = min(s.size, int64(min(g.uncompressedSize(index), math.MaxInt64)))
	}
	p.image = append(p.image, span{off: s.off, size: s.size})
	return s, nil
}

// partCount returns how many strips or tiles the dimensions call for. A hostile
// file can make this overflow, so it saturates: the result then matches no 32-bit
// count and the file is refused, so every later calculation has an exact count.
func (g geometry) partCount() uint64 {
	var n uint64
	if g.tiled {
		n = saturatingMul(ceilDiv(uint64(g.width), uint64(g.tileWidth)), ceilDiv(uint64(g.height), uint64(g.tileHeight)))
	} else {
		n = ceilDiv(uint64(g.height), uint64(g.rowsPerStrip))
	}
	if g.planar == 2 {
		n = saturatingMul(n, uint64(g.samples))
	}
	return n
}

// uncompressedSize returns how many bytes strip or tile index holds when stored
// uncompressed. It saturates too, and a size that large never trims anything.
func (g geometry) uncompressedSize(index int) uint64 {
	perPlane := g.partCount()
	if g.planar == 2 {
		perPlane /= uint64(g.samples)
	}
	bitsPerRowSample := g.bitsFor(index, perPlane)
	if g.tiled {
		return saturatingMul(rowBytes(g.tileWidth, bitsPerRowSample), uint64(g.tileHeight))
	}
	rows := uint64(g.height) - (uint64(index)%perPlane)*uint64(g.rowsPerStrip)
	return saturatingMul(rowBytes(g.width, bitsPerRowSample), min(rows, uint64(g.rowsPerStrip)))
}

// checkUncompressedYCbCr refuses a strip larger than its pixels. Other uncompressed
// strips are simply cut at that size, but for subsampled YCbCr, decoders do not agree
// on the size: libtiff goes by the subsampling tag, whose default is 2 by 2, and Pillow
// reads three bytes per pixel whatever it says. So there is no one right place to cut,
// and no way to leave the rest in without keeping whatever it holds.
func (g geometry) checkUncompressedYCbCr(index int, size int64) error {
	if g.planar != 1 {
		return core.Unsupportedf("tiff: uncompressed YCbCr in separate planes is not supported")
	}
	if g.samples != 3 {
		return core.Unsupportedf("tiff: uncompressed YCbCr with %d samples per pixel is not supported", g.samples)
	}
	hs, vs := uint64(g.subsampleH), uint64(g.subsampleV)
	if (hs != 1 && hs != 2 && hs != 4) || (vs != 1 && vs != 2 && vs != 4) {
		return core.Malformedf("tiff: YCbCr subsampling of %d by %d", hs, vs)
	}
	width, rows := uint64(g.tileWidth), uint64(g.tileHeight)
	if !g.tiled {
		perPlane := g.partCount()
		width = uint64(g.width)
		rows = min(uint64(g.height)-(uint64(index)%perPlane)*uint64(g.rowsPerStrip), uint64(g.rowsPerStrip))
	}
	blockSamples := saturatingMul(ceilDiv(width, hs), hs*vs+2)
	rowBits := saturatingMul(blockSamples, uint64(g.bits[0]))
	if rowBits > math.MaxUint32 {
		return nil // no real file is this wide, and a strip this short has no tail
	}
	if uint64(size) > saturatingMul((rowBits+7)/8, ceilDiv(rows, vs)) {
		return core.Malformedf("tiff: an uncompressed YCbCr strip of %d bytes is larger than its pixels", size)
	}
	return nil
}

func saturatingMul(a, b uint64) uint64 {
	high, low := bits.Mul64(a, b)
	if high != 0 {
		return math.MaxUint64
	}
	return low
}

// bitsFor returns the bits per pixel in the plane holding part index.
func (g geometry) bitsFor(index int, perPlane uint64) uint64 {
	if g.planar == 2 {
		return uint64(g.sampleBits(uint64(index) / perPlane))
	}
	return g.pixelBits
}

func (g geometry) sampleBits(sample uint64) uint32 {
	if len(g.bits) == 1 {
		return g.bits[0]
	}
	return g.bits[sample]
}

func rowBytes(width uint32, bits uint64) uint64 {
	return (uint64(width)*bits + 7) / 8
}

func ceilDiv(a, b uint64) uint64 {
	return (a + b - 1) / b
}

// chargeStrips counts n strips against maxStrips before their tables are read, and
// reports whether they fit. Every page can claim the same table, so a table's
// being inside the file bounds one read but not the total.
func (p *parser) chargeStrips(n uint32) bool {
	if int64(n) > maxStrips-int64(p.strips) {
		return false
	}
	p.strips += int(n)
	return true
}

// dropPage removes a whole page that exists only as a preview of another. Its
// allowlisted tags go along without a word, because they describe only the preview.
func (p *parser) dropPage(dir *directory, where string) {
	var removed []entry
	for _, e := range dir.entries {
		_, rendering := allowed[e.tag]
		switch {
		case e.tag == tagJPEGThumbnail || e.tag == tagJPEGThumbLength:
		case rendering || e.tag == tagOrientation || e.tag == tagICCProfile:
			// A writer may let pages share a palette or a resolution, so these are
			// not metadata whose overlap with a kept page would mean anything.
			if s := p.location(e); s.size > 0 && p.inBounds(s.off, s.size) {
				p.shared = append(p.shared, s)
			}
		default:
			removed = append(removed, e)
		}
	}
	p.add(core.Finding{
		Category: core.CatThumbnail,
		Risk:     core.RiskHigh,
		Field:    "Embedded thumbnail",
		Value:    p.notePreviewData(dir),
		Where:    where,
		Action:   core.ActionRemove,
	})
	p.reportRemoved(removed, where)
}

// notePreviewData records where a dropped page's picture lies, so its bytes are
// not reported a second time as unreferenced, and describes it. A table that cannot
// be read costs only that accounting, since none of this reaches the output.
func (p *parser) notePreviewData(dir *directory) string {
	if e, ok := dir.find(tagJPEGThumbnail); ok {
		var size int64
		if length, ok := dir.find(tagJPEGThumbLength); ok {
			if v, ok := length.scalar(p.order); ok {
				size = int64(v)
			}
		}
		if off, ok := e.scalar(p.order); ok {
			p.noteMetadata(span{off: int64(off), size: size})
		}
		return "JPEG, " + strconv.FormatInt(size, 10) + " bytes"
	}
	var total int64
	for _, tags := range [][2]uint16{{tagStripOffsets, tagStripByteCounts}, {tagTileOffsets, tagTileByteCounts}} {
		offsets, hasOffsets := dir.find(tags[0])
		counts, hasCounts := dir.find(tags[1])
		if !hasOffsets || !hasCounts || offsets.count != counts.count || !p.chargeStrips(offsets.count) {
			continue
		}
		offs, err := p.integers(offsets)
		if err != nil {
			continue
		}
		sizes, err := p.integers(counts)
		if err != nil {
			continue
		}
		for i := range offs {
			p.noteMetadata(span{off: int64(offs[i]), size: int64(sizes[i])})
			total += int64(sizes[i])
		}
	}
	return "reduced-resolution page, " + strconv.FormatInt(total, 10) + " bytes"
}
