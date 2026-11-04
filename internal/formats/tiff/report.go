package tiff

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/jpegmeta"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	// maxReportBytes bounds how much metadata one file can make the parser read
	// just to describe it. Hostile directories can all point at one huge region,
	// so this has to be a total. Past it the report gets less specific, nothing
	// else changes, and it is the same on every run because it depends only on
	// the file.
	maxReportBytes = 64 << 20
	// maxBlockValue is the largest value copied into the block given to the exif
	// package. Its text values are clipped far below this, and the one large value
	// it meets, MakerNote, is reported by size alone.
	maxBlockValue = 64 << 10
	// maxBlockDirectories bounds the sub-IFDs followed for one page. The real
	// structure has three: Exif, GPS and Interop.
	maxBlockDirectories = 8
	// maxPacketValue is the most read from an XMP, IPTC or Photoshop tag. The
	// xmp package examines no more than 4 MiB, and those tags are usually kilobytes.
	maxPacketValue = 16 << 20
	maxTextValue   = 4 << 10
)

// namedPageTags are removed tags outside the exif package's table that are worth
// naming, because each says something particular about a person or an edit.
var namedPageTags = map[uint16]struct {
	field    string
	category core.Category
	risk     core.Risk
	text     bool
}{
	tagPageName:        {"Page name", core.CatContent, core.RiskLow, true},
	tagTargetPrinter:   {"Target printer", core.CatDevice, core.RiskMed, true},
	tagImageID:         {"Linked image path", core.CatIdentity, core.RiskMed, true},
	tagImageSourceData: {"Embedded Photoshop layers", core.CatHidden, core.RiskHigh, false},
}

// add records a finding once. A multi-page scan repeats the same make and software
// on every page, and a report saying so a thousand times would bury what matters.
func (p *parser) add(f core.Finding) {
	key := string(f.Category) + "\x00" + f.Field + "\x00" + f.Value + "\x00" + string(f.Action)
	if p.seen[key] {
		return
	}
	p.seen[key] = true
	p.report.Add(f)
}

// reportRemoved reports the tags leaving a page. Every entry ends up in at least
// one finding, so nothing is removed without the user hearing of it.
func (p *parser) reportRemoved(entries []entry, where string) {
	var forExif []entry
	geo := 0
	for _, e := range entries {
		p.noteMetadata(p.location(e))
		switch {
		case e.tag == tagXMP:
			p.reportXMP(e, where)
		case e.tag == tagIPTC:
			p.reportIPTC(e, where)
		case e.tag == tagPhotoshop:
			p.reportPhotoshop(e, where)
		case geoTags[e.tag]:
			geo++
		default:
			if named, ok := namedPageTags[e.tag]; ok {
				value := formatBytes(e.byteSize())
				if named.text {
					value = p.textOf(e)
				}
				p.add(core.Finding{Category: named.category, Risk: named.risk, Field: named.field, Value: value, Where: where, Action: core.ActionRemove})
				continue
			}
			forExif = append(forExif, e)
		}
	}
	if geo > 0 {
		p.add(core.Finding{
			Category: core.CatLocation,
			Risk:     core.RiskHigh,
			Field:    "GeoTIFF location",
			Value:    strconv.Itoa(geo) + " georeferencing tags",
			Where:    where,
			Action:   core.ActionRemove,
		})
	}
	if len(forExif) > 0 {
		d, _ := exif.Parse(p.exifBlock(forExif)) // a damaged block reports itself
		p.addExif(d, where)
	}
}

// addExif adds a block's findings. Strict makes each of them a removal, which is
// what they are: any orientation that stays is written by applyOrientation.
func (p *parser) addExif(d *exif.Data, where string) {
	for _, f := range d.Findings(where, core.Options{Strict: true}) {
		p.add(f)
	}
}

// readForReport returns a value for decoding, or nil if it runs outside the file,
// is longer than limit, or the file has used up its allowance for such reads.
func (p *parser) readForReport(e entry, limit int64) []byte {
	size := e.byteSize()
	if size <= 4 {
		return e.raw[:size]
	}
	if size > limit || size > p.reportBudget {
		return nil
	}
	value, err := p.value(e)
	if err != nil {
		return nil
	}
	p.reportBudget -= size
	return value
}

func (p *parser) textOf(e entry) string {
	value := p.readForReport(e, maxTextValue)
	if value == nil {
		return formatBytes(e.byteSize())
	}
	if i := bytes.IndexByte(value, 0); i >= 0 {
		value = value[:i]
	}
	return string(value)
}

func (p *parser) reportXMP(e entry, where string) {
	packet := p.readForReport(e, maxPacketValue)
	if len(packet) == 0 {
		p.addEmbedded("Embedded XMP", e, where)
		return
	}
	for _, f := range xmp.Findings(packet, where) {
		p.add(f)
	}
}

func (p *parser) reportIPTC(e entry, where string) {
	p.addEmbedded("Embedded IPTC", e, where)
	if data := p.readForReport(e, maxPacketValue); len(data) > 0 {
		p.addIPTC(p.unswappedIPTC(data, e), where)
	}
}

func (p *parser) reportPhotoshop(e entry, where string) {
	p.addEmbedded("Embedded Photoshop data", e, where)
	if data := p.readForReport(e, maxPacketValue); len(data) > 0 {
		p.addPhotoshop(data, where)
	}
}

func (p *parser) addEmbedded(field string, e entry, where string) {
	p.add(core.Finding{
		Category: core.CatHidden,
		Risk:     core.RiskMed,
		Field:    field,
		Value:    formatBytes(e.byteSize()),
		Where:    where,
		Action:   core.ActionRemove,
	})
}

func formatBytes(n int64) string {
	return strconv.FormatInt(n, 10) + " bytes"
}

// iptcFields names the IPTC datasets (record 2) worth decoding.
var iptcFields = map[byte]struct {
	field    string
	category core.Category
	risk     core.Risk
}{
	5:   {"IPTC title", core.CatContent, core.RiskLow},
	25:  {"IPTC keywords", core.CatContent, core.RiskLow},
	55:  {"IPTC date created", core.CatTime, core.RiskMed},
	60:  {"IPTC time created", core.CatTime, core.RiskMed},
	80:  {"IPTC by-line", core.CatIdentity, core.RiskHigh},
	85:  {"IPTC by-line title", core.CatIdentity, core.RiskMed},
	90:  {"IPTC city", core.CatLocation, core.RiskHigh},
	92:  {"IPTC sub-location", core.CatLocation, core.RiskHigh},
	95:  {"IPTC province or state", core.CatLocation, core.RiskMed},
	100: {"IPTC country code", core.CatLocation, core.RiskMed},
	101: {"IPTC country", core.CatLocation, core.RiskMed},
	105: {"IPTC headline", core.CatContent, core.RiskLow},
	110: {"IPTC credit", core.CatIdentity, core.RiskMed},
	115: {"IPTC source", core.CatIdentity, core.RiskMed},
	116: {"IPTC copyright notice", core.CatIdentity, core.RiskHigh},
	118: {"IPTC contact", core.CatIdentity, core.RiskHigh},
	120: {"IPTC caption", core.CatContent, core.RiskMed},
	122: {"IPTC writer", core.CatIdentity, core.RiskHigh},
}

// unswappedIPTC undoes a historical mistake. Writers commonly declare this tag as
// LONG even though it holds a byte stream, and some of them then swap every four
// bytes on a little-endian machine, as a LONG array would be.
func (p *parser) unswappedIPTC(data []byte, e entry) []byte {
	if len(data) == 0 || data[0] == jpegmeta.IPTCTagMarker || e.typ != typeLong || p.order != binary.ByteOrder(binary.LittleEndian) {
		return data
	}
	swapped := make([]byte, len(data)&^3)
	for i := 0; i+4 <= len(data); i += 4 {
		binary.BigEndian.PutUint32(swapped[i:], binary.LittleEndian.Uint32(data[i:]))
	}
	return swapped
}

// addIPTC decodes an IPTC-IIM stream, stopping at the first thing that does not
// parse: the whole block is being removed regardless, so this only affects how
// much the report can say.
func (p *parser) addIPTC(data []byte, where string) {
	for {
		record, dataset, value, rest, ok := jpegmeta.NextDataset(data)
		if !ok {
			return
		}
		data = rest
		if spec, ok := iptcFields[dataset]; ok && record == jpegmeta.IPTCApplicationRecord {
			p.add(core.Finding{Category: spec.category, Risk: spec.risk, Field: spec.field, Value: string(value), Where: where, Action: core.ActionRemove})
		}
	}
}

// addPhotoshop walks Photoshop image resource blocks. Photoshop stores a second
// copy of the IPTC, EXIF and XMP here, along with a preview that can predate any
// later crop, so the report looks inside them.
func (p *parser) addPhotoshop(data []byte, where string) {
	where += "/Photoshop"
	for {
		id, block, rest, ok := jpegmeta.NextResource(data)
		if !ok {
			return
		}
		data = rest
		switch id {
		case jpegmeta.ResourceIPTC:
			p.addIPTC(block, where)
		case jpegmeta.ResourceThumbnail5, jpegmeta.ResourceThumbnail4:
			p.add(core.Finding{Category: core.CatThumbnail, Risk: core.RiskHigh, Field: "Embedded thumbnail", Value: "Photoshop, " + formatBytes(int64(len(block))), Where: where, Action: core.ActionRemove})
		case jpegmeta.ResourceExif1, jpegmeta.ResourceExif3:
			d, _ := exif.Parse(block) // a damaged block reports itself
			p.addExif(d, where)
		case jpegmeta.ResourceXMP:
			for _, f := range xmp.Findings(block, where) {
				p.add(f)
			}
		}
	}
}

// exifBlock rebuilds a page's removed tags as a small stand-alone TIFF block for
// the exif package to read. Handing it the file itself would not work: it would
// take every later page for a thumbnail, count rendering tags as metadata, stop at
// its own low limit on pages, and need the entire file in memory.
//
// Pointer tags are followed, and the sub-IFDs they lead to are copied in, so the
// package finds GPS and Exif data where it expects to. The block is only ever read
// for its report, so a value too large to copy is enough to describe by its size.
func (p *parser) exifBlock(entries []entry) []byte {
	b := blockBuilder{p: p, visited: make(map[uint32]bool)}
	if p.order == binary.ByteOrder(binary.BigEndian) {
		b.buf = []byte{'M', 'M', 0, 42, 0, 0, 0, headerSize}
	} else {
		b.buf = []byte{'I', 'I', 42, 0, headerSize, 0, 0, 0}
	}
	b.writeDirectory(entries)
	return b.buf
}

// invalidOffset is past the end of any block, so the exif package reports the
// entry as damage, which is the truth about an entry leading nowhere.
const invalidOffset = math.MaxUint32

type blockBuilder struct {
	p       *parser
	buf     []byte
	visited map[uint32]bool
}

func (b *blockBuilder) writeDirectory(entries []entry) uint32 {
	start := len(b.buf)
	b.buf = append(b.buf, make([]byte, directoryOverhead+len(entries)*entrySize)...)
	b.p.order.PutUint16(b.buf[start:], uint16(len(entries)))
	for i, e := range entries {
		b.writeEntry(start+2+i*entrySize, e)
	}
	return uint32(start)
}

// writeEntry addresses b.buf by index because appending values may move it.
func (b *blockBuilder) writeEntry(at int, e entry) {
	order := b.p.order
	order.PutUint16(b.buf[at:], e.tag)
	order.PutUint16(b.buf[at+2:], e.typ)
	order.PutUint32(b.buf[at+4:], e.count)
	copy(b.buf[at+8:at+12], e.raw[:])
	if e.tag == tagExifIFD || e.tag == tagGPSIFD || e.tag == tagInteropIFD {
		b.writePointer(at, e)
		return
	}
	size := e.byteSize()
	if size <= 4 {
		return
	}
	s := b.p.location(e)
	if !b.p.inBounds(s.off, s.size) {
		order.PutUint32(b.buf[at+8:], invalidOffset)
		return
	}
	if size > maxBlockValue || size > b.p.reportBudget {
		// Type zero means "no readable value" to the exif package, which then
		// counts the elements as bytes. That gives a true size without the bytes.
		order.PutUint16(b.buf[at+2:], 0)
		order.PutUint32(b.buf[at+4:], uint32(min(size, math.MaxUint32)))
		return
	}
	value, err := b.p.read(s.off, s.size)
	if err != nil {
		order.PutUint32(b.buf[at+8:], invalidOffset)
		return
	}
	b.p.reportBudget -= size
	order.PutUint32(b.buf[at+8:], uint32(len(b.buf)))
	b.buf = append(b.buf, value...)
}

// writePointer copies the directory a pointer tag leads to. An entry that is not
// a pointer at all is left for the exif package to call damaged, and so is one
// leading nowhere. One that merely exceeds the read allowance reads as absent,
// since nothing is wrong with the file.
func (b *blockBuilder) writePointer(at int, e entry) {
	order := b.p.order
	off, ok := e.scalar(order)
	if !ok || off == 0 {
		return
	}
	order.PutUint16(b.buf[at+2:], typeLong)
	order.PutUint32(b.buf[at+4:], 1)
	if b.visited[off] || len(b.visited) == maxBlockDirectories {
		order.PutUint32(b.buf[at+8:], invalidOffset)
		return
	}
	b.visited[off] = true
	if b.p.reportBudget < maxEntriesPerIFD*entrySize {
		order.PutUint32(b.buf[at+8:], 0)
		return
	}
	entries, _, err := b.p.readEntries(off)
	if err != nil {
		order.PutUint32(b.buf[at+8:], invalidOffset)
		return
	}
	b.p.reportBudget -= int64(len(entries)) * entrySize
	for _, sub := range entries {
		b.p.noteMetadata(b.p.location(sub))
	}
	child := b.writeDirectory(entries)
	order.PutUint32(b.buf[at+8:], child)
}
