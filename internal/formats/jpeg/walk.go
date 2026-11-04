package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

// maxSegments bounds the work and the slice a hostile file can demand. A real
// JPEG has a few dozen segments even when progressive, so this cannot get in the
// way of a genuine image.
const maxSegments = 1 << 16

// ctxCheckInterval is how many stuffed bytes the entropy scan passes between
// context checks, which keeps cancellation prompt without slowing the scan.
const ctxCheckInterval = 1 << 16

var signature = []byte{0xFF, jpegmeta.MarkerSOI, 0xFF}

// cleaner holds the state of one walk over a file.
type cleaner struct {
	data []byte
	opts core.Options
	rep  *core.Report

	jfif        []byte   // the rewritten JFIF segment, if the file has one
	orientation []byte   // the EXIF block to write in place of the originals, if any
	parts       [][]byte // kept segments, in file order
	icc         []iccChunk

	photoshop      []byte // APP13 payloads joined up: Photoshop splits long blocks
	sawPhotoshop   bool
	extendedXMP    []byte // the same for extended XMP
	sawExtendedXMP bool

	tables      tableSet
	frameMarker byte   // the marker of the frame header
	frameHeight int    // its image height; zero leaves the height to a DNL
	frameWidth  int    // its image width
	frameQuant  []byte // the quantisation table each component uses
	components  []frameComponent

	walkedBlocks int64 // blocks read by walkScan so far, against maxWalkedBlocks

	adobeTransform byte
	stray          int // bytes found where a marker belongs and left behind

	sawExif  bool
	sawFrame bool
	sawScan  bool
	sawAdobe bool
	sawDNL   bool
	sawMPF   bool
	cutOff   bool // the file ended with no end-of-image marker
}

// add records a finding about something the walk is dropping.
func (c *cleaner) add(category core.Category, risk core.Risk, field, value, where string) {
	c.rep.Add(core.Finding{
		Category: category,
		Risk:     risk,
		Field:    field,
		Value:    value,
		Where:    where,
		Action:   core.ActionRemove,
	})
}

func (c *cleaner) walk(ctx context.Context) error {
	if !bytes.HasPrefix(c.data, signature) {
		return core.Unsupportedf("jpeg: the file does not start with a JPEG marker")
	}
	pos := 2
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == maxSegments {
			return fmt.Errorf("%w: jpeg: more than %d segments", core.ErrTooLarge, maxSegments)
		}
		marker, body, found := c.nextMarker(pos)
		if !found {
			if !c.sawScan {
				return core.Malformedf("jpeg: the file ends with no image data and no end-of-image marker")
			}
			c.cutOff = true
			return nil
		}
		if marker == jpegmeta.MarkerEOI {
			if !c.sawScan {
				return core.Malformedf("jpeg: end-of-image marker before any scan")
			}
			c.reportTrailing(c.data[body:])
			return nil
		}
		var err error
		pos, err = c.segment(ctx, marker, body)
		if err != nil {
			return err
		}
	}
}

// nextMarker finds the first marker at or after pos and returns its code and the
// offset just past it. It searches exactly as libjpeg and image/jpeg do, stepping over
// anything that is not 0xFF, the stuffed pair FF 00, and fill bytes, so it finds the
// segments a decoder finds. Nothing it steps over is copied to the output, which is
// what makes it safe to carry on past such bytes rather than refuse the file: they
// mean nothing to a decoder and it displays anyway. They are counted for the report.
func (c *cleaner) nextMarker(pos int) (marker byte, body int, found bool) {
	start := pos
	for {
		i := bytes.IndexByte(c.data[pos:], 0xFF)
		if i < 0 {
			c.stray += len(c.data) - start
			return 0, 0, false
		}
		pos += i
		code := pos
		for code < len(c.data) && c.data[code] == 0xFF {
			code++
		}
		if code == len(c.data) {
			c.stray += pos - start
			return 0, 0, false
		}
		if c.data[code] != 0 {
			c.stray += pos - start
			return c.data[code], code + 1, true
		}
		pos = code + 1
	}
}

// segment handles the segment whose marker ends just before body and returns the
// offset to continue from.
func (c *cleaner) segment(ctx context.Context, marker byte, body int) (int, error) {
	if err := rejectMarker(marker, body-1); err != nil {
		return 0, err
	}
	if len(c.data)-body < 2 {
		return c.endsInSegment(body, core.Malformedf("jpeg: no room for the length of marker %#02x at offset %d", marker, body-1))
	}
	length := int(binary.BigEndian.Uint16(c.data[body:]))
	if length < 2 {
		return 0, core.Malformedf("jpeg: marker %#02x at offset %d declares an impossible length of %d", marker, body-1, length)
	}
	if length > len(c.data)-body {
		return c.endsInSegment(body, core.Malformedf("jpeg: marker %#02x at offset %d declares a length of %d, past the end of the file", marker, body-1, length))
	}
	end := body + length
	raw := c.data[body-2 : end] // from the final 0xFF, leaving any fill bytes behind
	payload := c.data[body+2 : end]

	switch {
	case marker >= jpegmeta.MarkerAPP0 && marker <= jpegmeta.MarkerAPP15:
		return end, c.app(int(marker-jpegmeta.MarkerAPP0), raw, payload)
	case marker == jpegmeta.MarkerCOM:
		c.add(core.CatContent, core.RiskMed, "Comment", core.DecodeText(payload), "COM")
		return end, nil
	case marker == jpegmeta.MarkerSOS:
		return c.scan(ctx, raw, payload, end)
	case jpegmeta.IsFrame(marker):
		return end, c.frame(marker, raw, payload)
	case marker == jpegmeta.MarkerDQT:
		return end, c.keepTables(tableQuant, marker, raw, payload, jpegmeta.QuantEntry)
	case marker == jpegmeta.MarkerDHT:
		return end, c.keepTables(tableHuffman, marker, raw, payload, jpegmeta.HuffmanEntry)
	case marker == jpegmeta.MarkerDAC:
		return end, c.keepTables(tableArithmetic, marker, raw, payload, jpegmeta.ArithmeticEntry)
	case marker == jpegmeta.MarkerDRI:
		return end, c.keepTables(tableRestart, marker, raw, payload, jpegmeta.RestartEntry)
	default: // only DNL is left, because rejectMarker turned away everything else
		return end, c.numberOfLines(marker, raw, payload)
	}
}

// endsInSegment handles a file that ends part way through a segment. After a scan
// that is just a file that got cut off, and the partial segment is left behind. Before
// one there is no picture to save.
func (c *cleaner) endsInSegment(body int, refusal error) (int, error) {
	if !c.sawScan {
		return 0, refusal
	}
	c.stray += len(c.data) - (body - 2)
	return len(c.data), nil
}

func (c *cleaner) frame(marker byte, raw, payload []byte) error {
	if c.sawFrame {
		return core.Malformedf("jpeg: more than one frame header")
	}
	if !validSOF(payload) {
		return core.Malformedf("jpeg: segment with marker %#02x has an invalid structure", marker)
	}
	c.sawFrame = true
	c.frameMarker = marker
	c.frameHeight = int(binary.BigEndian.Uint16(payload[1:]))
	c.frameWidth = int(binary.BigEndian.Uint16(payload[3:]))
	for i := range int(payload[5]) {
		spec := payload[6+3*i:]
		c.frameQuant = append(c.frameQuant, spec[2])
		c.components = append(c.components, frameComponent{id: spec[0], h: spec[1] >> 4, v: spec[1] & 0x0F})
	}
	c.parts = append(c.parts, raw)
	return nil
}

// numberOfLines keeps the one DNL a frame with no height depends on. Nothing reads
// any other.
func (c *cleaner) numberOfLines(marker byte, raw, payload []byte) error {
	if len(payload) != 2 || !c.sawScan {
		return core.Malformedf("jpeg: segment with marker %#02x has an invalid structure", marker)
	}
	if c.frameHeight != 0 || c.sawDNL {
		c.tables.unused += len(raw)
		return nil
	}
	c.sawDNL = true
	c.parts = append(c.parts, raw)
	return nil
}

// rejectMarker turns away markers that cannot start a segment here. Parts of JPEG
// that HideMe does not model are unsupported rather than dropped, because the
// picture may depend on them and dropping them could damage it.
func rejectMarker(marker byte, off int) error {
	switch {
	case marker >= jpegmeta.MarkerAPP0 && marker <= jpegmeta.MarkerAPP15, jpegmeta.IsFrame(marker), marker == jpegmeta.MarkerCOM, marker == jpegmeta.MarkerSOS,
		marker == jpegmeta.MarkerDQT, marker == jpegmeta.MarkerDHT, marker == jpegmeta.MarkerDAC, marker == jpegmeta.MarkerDRI, marker == jpegmeta.MarkerDNL:
		return nil
	case marker == jpegmeta.MarkerDHP, marker == jpegmeta.MarkerEXP:
		return core.Unsupportedf("jpeg: hierarchical JPEG (marker %#02x at offset %d) is not supported", marker, off)
	case marker == jpegmeta.MarkerJPG, marker >= jpegmeta.MarkerJPG0 && marker <= jpegmeta.MarkerJPG13:
		return core.Unsupportedf("jpeg: JPEG extension marker %#02x at offset %d is not supported", marker, off)
	}
	return core.Malformedf("jpeg: marker %#02x is not valid at offset %d", marker, off)
}

// validSOF also refuses a selector for a quantisation table that cannot exist, which a
// TIFF strip's check leaves to its own decoder.
func validSOF(p []byte) bool {
	if !jpegmeta.ValidFrame(p) {
		return false
	}
	const fixed = 6 // precision, height, width, component count
	for i := range int(p[5]) {
		if p[fixed+3*i+2] > 3 {
			return false // selects a quantisation table that cannot exist
		}
	}
	return true
}

// scan keeps a scan header and the entropy-coded data after it, returning the
// offset of the marker that follows the data.
func (c *cleaner) scan(ctx context.Context, raw, payload []byte, end int) (int, error) {
	if !c.sawFrame {
		return 0, core.Malformedf("jpeg: scan before any frame header")
	}
	if !jpegmeta.ValidScan(payload) {
		return 0, core.Malformedf("jpeg: scan header has an invalid structure")
	}
	if err := c.markScanTables(payload); err != nil {
		return 0, err
	}
	next, err := c.entropyEnd(ctx, end)
	if err != nil {
		return 0, err
	}
	c.sawScan = true
	data := c.data[end:next]
	if kept, removed, ok := c.walkScan(ctx, payload, data); ok {
		data = kept
		c.add(core.CatHidden, core.RiskHigh, "Data after the compressed image", core.ByteCount(removed), "scan")
	}
	c.parts = append(c.parts, raw, data)
	return next, nil
}

// entropyEnd returns the offset of the marker that ends the entropy-coded data
// starting at pos. Stuffed zeros and restart markers belong to the data, as do
// fill bytes, which are kept so the data stays byte-identical. The returned
// offset is that of the final 0xFF before the marker code. A file with no such
// marker has been cut off, and all of it up to a final half marker is then data.
func (c *cleaner) entropyEnd(ctx context.Context, pos int) (int, error) {
	for n := 0; ; n++ {
		if n%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		i := bytes.IndexByte(c.data[pos:], 0xFF)
		if i < 0 {
			return len(c.data), nil
		}
		pos += i
		if pos+1 == len(c.data) {
			return pos, nil
		}
		switch next := c.data[pos+1]; {
		case next == 0x00, next >= 0xD0 && next <= 0xD7:
			pos += 2
		case next == 0xFF:
			pos++
		default:
			return pos, nil
		}
	}
}

// reportTrailing describes what follows EOI, where phones put motion-photo video,
// HDR gain maps and further images. All of it is dropped, whatever it is, and that
// is more than a finding: those are part of the photo as the user knows it, so the
// file gets a warning too. Padding of zeros loses nothing and gets none.
func (c *cleaner) reportTrailing(b []byte) {
	if len(b) == 0 {
		return
	}
	var held []string
	if bytes.HasPrefix(b, signature) {
		held = append(held, "another JPEG image")
	}
	if holdsVideo(b) {
		held = append(held, "a video")
	}
	value := core.ByteCount(len(b))
	if len(held) > 0 {
		value += ", holding " + strings.Join(held, " and ")
	}
	c.add(core.CatHidden, core.RiskHigh, "Trailing data", value, "after EOI")
	switch {
	case core.AllZero(b):
	case len(held) > 0:
		c.rep.MayLoseContent = true
		c.rep.Warn("removed %s after the end of the main picture, holding %s: any HDR gain map, depth map, further picture%s or motion-photo video kept there is lost",
			core.ByteCount(len(b)), strings.Join(held, " and "), c.listedByIndex())
	default:
		c.rep.Warn("removed %s after the end of the image: if this file was a motion photo or held further pictures%s, they are lost", core.ByteCount(len(b)), c.listedByIndex())
	}
}

// listedByIndex adds to a warning that an MPF segment had listed further pictures.
// The segment alone is no loss, since all it can list follows EOI.
func (c *cleaner) listedByIndex() string {
	if c.sawMPF {
		return " listed in its multi-picture index"
	}
	return ""
}

// holdsVideo looks for the MP4 header of a motion photo. Google's sits right after
// EOI or after a gain map, and Samsung's follows a label of its own. An "ftyp" counts
// only with its size field before it and a printable brand after it, so that one
// turning up by chance in compressed data does not call a picture a video.
func holdsVideo(b []byte) bool {
	if bytes.Contains(b, []byte("MotionPhoto_Data")) {
		return true
	}
	const sizeField, brandEnd = 4, 8
	for off := 0; ; {
		i := bytes.Index(b[off:], []byte("ftyp"))
		if i < 0 {
			return false
		}
		pos := off + i
		if pos >= sizeField && pos+brandEnd <= len(b) && printable(b[pos+sizeField:pos+brandEnd]) {
			return true
		}
		off = pos + 1
	}
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// finish reports the items that span several segments, once all are collected.
func (c *cleaner) finish() {
	c.reportExtendedXMP()
	c.reportPhotoshop()
	c.resolveICC()
	c.dropUnusedTables()
	if c.stray > 0 {
		c.add(core.CatHidden, core.RiskMed, "Stray bytes", core.ByteCount(c.stray), "between segments")
	}
	if c.cutOff {
		c.rep.Warn("this file is cut off before its end-of-image marker: the output holds what is there with the marker restored, so the picture stays incomplete")
	}
}

// output returns the cleaned file. JFIF requires its segment right after SOI, so
// it and the replacement EXIF block go first wherever the originals were.
func (c *cleaner) output() [][]byte {
	out := make([][]byte, 0, len(c.parts)+4)
	out = append(out, []byte{0xFF, jpegmeta.MarkerSOI})
	if c.jfif != nil {
		out = append(out, c.jfif)
	}
	if c.orientation != nil {
		out = append(out, exifSegment(c.orientation))
	}
	out = append(out, c.parts...)
	return append(out, []byte{0xFF, jpegmeta.MarkerEOI})
}
