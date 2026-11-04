package tiff

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

const (
	// A JFIF segment without a thumbnail, and the only Adobe segment there is, have
	// these lengths, counting the two length bytes.
	jfifSegmentLength  = 16
	adobeSegmentLength = 14
)

// scanJPEG walks one JPEG stream, in a strip or in JPEGTables, and returns the
// range to keep. Each strip of a JPEG-compressed TIFF is a JPEG stream in its own
// right, so it can carry an APP1 EXIF block or a comment just as a standalone JPEG
// can, and copying "the bytes the strip references" would carry them along. The
// walk reads every byte, because only by following the markers can it find where
// the stream truly ends and the data hidden after it begins.
//
// Segments are allowlisted: anything that is not needed to decode goes.
//
// Every strip of every page can point at the same stream, so the total walked is
// capped by what the output could hold anyway, or a small file could demand terabytes.
func (p *parser) scanJPEG(s span, where string) (span, error) {
	p.jpegScanned += s.size
	if most := growthLimit(p.src.Size()); p.jpegScanned > most {
		return span{}, fmt.Errorf("%w: strips and tables point at more than %d bytes of JPEG data", core.ErrTooLarge, most)
	}
	limit := s.end()
	first, err := p.jpegByte(s.off, limit)
	if err != nil {
		return span{}, err
	}
	second, err := p.jpegByte(s.off+1, limit)
	if err != nil {
		return span{}, err
	}
	if first != 0xFF || second != jpegmeta.MarkerSOI {
		return span{}, core.Malformedf("tiff: JPEG data at offset %d does not begin with SOI", s.off)
	}
	var cuts []core.Patch
	var apps jpegApps
	for pos := s.off + 2; ; {
		start := pos
		marker, next, err := p.jpegMarker(pos, limit)
		if err != nil {
			return span{}, err
		}
		pos = next
		switch {
		case marker == jpegmeta.MarkerEOI:
			return span{off: s.off, size: pos - s.off, cuts: cuts}, nil
		case marker == jpegmeta.MarkerTEM || (marker >= jpegmeta.MarkerRST0 && marker <= jpegmeta.MarkerRST7):
			continue // these stand alone, with no length after them
		case marker == jpegmeta.MarkerSOI || marker == 0x00:
			return span{}, core.Malformedf("tiff: unexpected JPEG marker %#02x at offset %d", marker, start)
		}
		end, err := p.jpegSegmentEnd(pos, limit)
		if err != nil {
			return span{}, err
		}
		switch {
		case marker == jpegmeta.MarkerSOS:
			if err := p.checkDecodingSegment(marker, start, pos, end); err != nil {
				return span{}, err
			}
			pos, err = p.skipEntropyData(end, limit)
			if err != nil {
				return span{}, err
			}
		case isDecodingSegment(marker):
			if err := p.checkDecodingSegment(marker, start, pos, end); err != nil {
				return span{}, err
			}
			pos = end
		case marker == jpegmeta.MarkerCOM || (marker >= jpegmeta.MarkerAPP0 && marker <= jpegmeta.MarkerAPP15):
			keep, err := p.isHarmlessAPP(marker, pos, end)
			if err != nil {
				return span{}, err
			}
			if keep {
				keep, err = p.settleAPP(&apps, marker, pos, s, &cuts, where)
				if err != nil {
					return span{}, err
				}
			}
			if !keep {
				if p.cuts == maxJPEGCuts {
					return span{}, fmt.Errorf("%w: more than %d metadata segments in JPEG data", core.ErrTooLarge, maxJPEGCuts)
				}
				p.cuts++
				cuts = append(cuts, core.Patch{Off: start - s.off, Len: end - start})
				p.reportJPEGSegment(marker, end-start, where)
			}
			pos = end
		default:
			return span{}, core.Malformedf("tiff: unknown JPEG marker %#02x at offset %d", marker, start)
		}
	}
}

func (p *parser) jpegByte(pos, limit int64) (byte, error) {
	if pos >= limit {
		return 0, core.Malformedf("tiff: JPEG data ends without an EOI marker")
	}
	return p.at(pos)
}

// jpegMarker reads the marker at pos, stepping over fill bytes, and returns its
// code and the position after it.
func (p *parser) jpegMarker(pos, limit int64) (byte, int64, error) {
	b, err := p.jpegByte(pos, limit)
	if err != nil {
		return 0, 0, err
	}
	if b != 0xFF {
		return 0, 0, core.Malformedf("tiff: expected a JPEG marker at offset %d", pos)
	}
	for b == 0xFF {
		pos++
		if b, err = p.jpegByte(pos, limit); err != nil {
			return 0, 0, err
		}
	}
	return b, pos + 1, nil
}

// jpegSegmentEnd reads the length field at pos and returns where the segment ends.
func (p *parser) jpegSegmentEnd(pos, limit int64) (int64, error) {
	high, err := p.jpegByte(pos, limit)
	if err != nil {
		return 0, err
	}
	low, err := p.jpegByte(pos+1, limit)
	if err != nil {
		return 0, err
	}
	length := int64(high)<<8 | int64(low)
	if length < 2 || length > limit-pos {
		return 0, core.Malformedf("tiff: JPEG segment at offset %d has length %d", pos, length)
	}
	return pos + length, nil
}

// skipEntropyData returns the position of the next real marker. Inside entropy-coded
// data, FF 00 is a stuffed byte and RSTn a restart, and neither ends the scan.
func (p *parser) skipEntropyData(pos, limit int64) (int64, error) {
	for {
		b, err := p.jpegByte(pos, limit)
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			pos++
			continue
		}
		next, err := p.jpegByte(pos+1, limit)
		if err != nil {
			return 0, err
		}
		switch {
		case next == 0xFF:
			pos++ // a fill byte; the marker follows
		case next == 0x00 || (next >= jpegmeta.MarkerRST0 && next <= jpegmeta.MarkerRST7):
			pos += 2
		default:
			return pos, nil
		}
	}
}

// isDecodingSegment allowlists the segments a decoder needs. Hierarchical JPEG's DHP
// and EXP, and the reserved codes, stay out of the list: they do not occur in real
// TIFF strips.
func isDecodingSegment(marker byte) bool {
	switch marker {
	case jpegmeta.MarkerDQT, jpegmeta.MarkerDHT, jpegmeta.MarkerDAC, jpegmeta.MarkerDRI, jpegmeta.MarkerDNL:
		return true
	}
	return jpegmeta.IsFrame(marker)
}

// checkDecodingSegment refuses a kept segment that is not exactly the shape its marker
// defines, because room to spare in one would carry anything at all through cleaning.
// libjpeg rejects the same segments, so no strip that decodes is lost to this.
func (p *parser) checkDecodingSegment(marker byte, start, pos, end int64) error {
	payload, err := p.read(pos+2, end-pos-2)
	if err != nil {
		return err
	}
	if !validDecodingSegment(marker, payload) {
		return core.Malformedf("tiff: JPEG segment %#02x at offset %d has an invalid structure", marker, start)
	}
	return nil
}

func validDecodingSegment(marker byte, p []byte) bool {
	switch marker {
	case jpegmeta.MarkerDQT:
		return jpegmeta.ValidTables(p, jpegmeta.QuantEntry)
	case jpegmeta.MarkerDHT:
		return jpegmeta.ValidTables(p, jpegmeta.HuffmanEntry)
	case jpegmeta.MarkerDAC:
		return jpegmeta.ValidTables(p, jpegmeta.ArithmeticEntry)
	case jpegmeta.MarkerDRI, jpegmeta.MarkerDNL:
		return len(p) == 2
	case jpegmeta.MarkerSOS:
		return jpegmeta.ValidScan(p)
	}
	return jpegmeta.ValidFrame(p)
}

// jpegApps is what one stream's kept application segments have said so far.
type jpegApps struct {
	sawJFIF   bool
	sawAdobe  bool
	transform byte
}

// adobeNeutralValues is what the version and flags of an Adobe segment become. No
// decoder reads them, and only the colour transform after them matters.
var adobeNeutralValues = []byte{0, 100, 0, 0, 0, 0}

// settleAPP decides about a harmless segment given the ones before it. Only the first
// of each kind stays, since the others would be free room as often as a file likes,
// and only the transform of an Adobe segment stays as it was. Decoders disagree on
// which of several Adobe segments wins, so there is no one picture to preserve when
// their transforms differ.
func (p *parser) settleAPP(apps *jpegApps, marker byte, pos int64, s span, cuts *[]core.Patch, where string) (bool, error) {
	if marker == jpegmeta.MarkerAPP0 {
		first := !apps.sawJFIF
		apps.sawJFIF = true
		return first, nil
	}
	const idSize, valuesSize = 5, 6
	values, err := p.read(pos+2+idSize, valuesSize+1)
	if err != nil {
		return false, err
	}
	transform := values[valuesSize]
	if apps.sawAdobe {
		if transform != apps.transform {
			return false, core.Malformedf("tiff: Adobe segments in one JPEG stream disagree on the colour transform")
		}
		return false, nil
	}
	apps.sawAdobe = true
	apps.transform = transform
	if !bytes.Equal(values[:valuesSize], adobeNeutralValues) {
		*cuts = append(*cuts, core.Patch{Off: pos + 2 + idSize - s.off, Len: valuesSize, Data: adobeNeutralValues})
		p.add(core.Finding{
			Category: core.CatSoftware,
			Risk:     core.RiskLow,
			Field:    "Adobe header values",
			Value:    strconv.Itoa(valuesSize) + " bytes",
			Where:    where,
			Action:   core.ActionAnonymize,
		})
	}
	return true, nil
}

// isHarmlessAPP recognises the two application segments worth keeping: the Adobe
// segment, which says whether the samples went through a colour transform, and a
// JFIF header with no thumbnail. Both have fixed sizes with nothing free-form in
// them. pos and end bound the segment including its length bytes.
func (p *parser) isHarmlessAPP(marker byte, pos, end int64) (bool, error) {
	length := end - pos
	switch {
	case marker == jpegmeta.MarkerAPP14 && length == adobeSegmentLength:
		id, err := p.read(pos+2, 5)
		return err == nil && string(id) == "Adobe", err
	case marker == jpegmeta.MarkerAPP0 && length == jfifSegmentLength:
		payload, err := p.read(pos+2, length-2)
		if err != nil {
			return false, err
		}
		// The last two payload bytes are the thumbnail's width and height.
		return string(payload[:5]) == "JFIF\x00" && payload[12] == 0 && payload[13] == 0, nil
	}
	return false, nil
}

func (p *parser) reportJPEGSegment(marker byte, size int64, where string) {
	name := "COM"
	if marker != jpegmeta.MarkerCOM {
		name = "APP" + strconv.Itoa(int(marker-jpegmeta.MarkerAPP0))
	}
	p.add(core.Finding{
		Category: core.CatHidden,
		Risk:     core.RiskHigh,
		Field:    "Metadata in JPEG data",
		Value:    name + " segment, " + strconv.FormatInt(size, 10) + " bytes",
		Where:    where,
		Action:   core.ActionRemove,
	})
}
