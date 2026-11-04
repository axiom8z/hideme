package pdf

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

// scanOverlap is how much of each chunk of a scanned stream is searched again with
// the next one, so that something split across chunks is still seen. It comfortably
// exceeds every signature xmp.Contains knows, including their UTF-32 spellings, and
// the part of an inline image dictionary that gets read.
const scanOverlap = maxInlineDict + 256

// scanNeeds says what to look for in a stream besides XMP, inline images and bytes
// past its end. Inline images are searched for in every stream, because a Type 3
// glyph has no key to say that it is drawn, and a page draws whatever /Contents
// points at, whatever it claims to be.
type scanNeeds struct {
	content bool  // a content stream, which has no reason to hold comments
	pixels  int64 // the bytes an image's dimensions use, or -1 if not known
}

// streamScan is what reading a stream from end to end found.
type streamScan struct {
	opaque   bool // part of the chain could not be read through
	damaged  bool // the data ended in an error, so what follows is unknowable
	hasXMP   bool
	inline   bool // an inline image that can hold metadata of its own
	comments bool
	excess   bool // data past what the image's dimensions use

	cutLayer int   // the filter whose input has bytes past its end, or -1
	cutAt    int64 // where in that input they begin
	cutBytes int64 // how many
	hidden   bool  // they are more than padding
}

// scanStream decompresses a stream to look through it, without keeping it. This
// costs no memory, so its cap can be high enough for large images. Whatever the
// chain cannot read through is reported as opaque. Filters before that are still
// checked, because a chain's end can be found even if not what it holds.
func (d *document) scanStream(s *pdfStream, needs scanNeeds) (streamScan, error) {
	scan := streamScan{cutLayer: -1}
	steps, err := d.filterChain(s.dict)
	if err != nil {
		return scan, err
	}
	usable := 0
	for usable < len(steps) && d.decodes(steps[usable]) {
		usable++
	}
	scan.opaque = usable < len(steps)
	dec, err := d.openChain(s.raw, steps[:usable])
	if err != nil {
		// A predictor this package cannot undo, or parameters that make no sense.
		// Neither says anything about the document, only about this one stream.
		if isFinal(err) && !errors.Is(err, core.ErrUnsupported) {
			return scan, err
		}
		scan.opaque = true
		return scan, nil
	}
	if err := d.scanDecoded(dec.r, needs, &scan); err != nil {
		return scan, err
	}
	if scan.opaque {
		scan.hasXMP, scan.inline, scan.comments, scan.excess = false, false, false, false
	}
	if !scan.damaged {
		if err := d.findExtra(dec, &scan); err != nil {
			return scan, err
		}
	}
	return scan, nil
}

// scanDecoded reads a stream's final output, searching it. It fills its buffer each
// time rather than searching what each read returns, because a read may return as
// little as one row of a predictor.
func (d *document) scanDecoded(r io.Reader, needs scanNeeds, scan *streamScan) error {
	buf := make([]byte, 64<<10)
	var total int64 // bytes seen before buf[kept:]
	kept := 0
	inline := inlineSearch{budget: &d.inlineBudget}
	var comments commentFinder
	for {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(r, buf[kept:])
		d.scanned += int64(n)
		if d.scanned > d.limits.maxScanned {
			return fmt.Errorf("%w: streams decompress past the %d byte limit", core.ErrTooLarge, d.limits.maxScanned)
		}
		fresh := buf[kept : kept+n]
		if needs.content && !scan.comments {
			scan.comments = comments.feed(fresh)
		}
		if needs.pixels >= 0 && !scan.excess {
			scan.excess = hasExcess(fresh, total, needs.pixels)
		}
		total += int64(n)
		end := kept + n
		final := readErr != nil
		if !scan.hasXMP && xmp.Contains(buf[:end]) {
			scan.hasXMP = true
		}
		if !scan.inline {
			scan.inline = inline.search(buf[:end], final, needs.content)
		}
		if final {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				if isFinal(readErr) {
					return readErr
				}
				scan.damaged = true
			}
			return nil
		}
		kept = min(end, scanOverlap)
		copy(buf, buf[end-kept:end])
	}
}

// hasExcess reports whether any byte of a chunk that started total bytes into the
// data lies past limit and is more than the padding writers add by habit.
func hasExcess(chunk []byte, total, limit int64) bool {
	skip := limit - total
	if skip >= int64(len(chunk)) {
		return false
	}
	if skip > 0 {
		chunk = chunk[skip:]
	}
	return !onlySpace(chunk)
}

func onlySpace(b []byte) bool {
	for _, c := range b {
		if !isSpace(c) {
			return false
		}
	}
	return true
}

// findExtra looks for bytes after each filter's end, innermost first. Reading the
// input of one filter is what makes the filter before it reach its own end. The
// innermost filter with something hidden after it decides where the stream is cut,
// since a cut there also drops the filters outside it and whatever follows them.
// Padding is only cut from the outermost, where that costs nothing: a cut further
// in changes the chain, which padding is no reason to do.
func (d *document) findExtra(dec *decoder, scan *streamScan) error {
	for i := len(dec.inputs) - 1; i >= 0; i-- {
		in := dec.inputs[i]
		end := in.n
		extra, hidden, err := d.drain(in)
		if err != nil {
			return err
		}
		if extra.damaged {
			scan.damaged = true
			return nil
		}
		if extra.bytes == 0 {
			continue
		}
		if hidden || i == 0 {
			scan.cutLayer, scan.cutAt, scan.cutBytes, scan.hidden = i, end+extra.kept, extra.bytes, hidden
			if hidden {
				return nil
			}
		}
	}
	return nil
}

// drained says what a filter left unread. kept is what of it the format explains,
// and bytes is the rest.
type drained struct {
	kept    int64
	bytes   int64
	damaged bool
}

func (d *document) drain(in *filterInput) (drained, bool, error) {
	var out drained
	hidden := false
	buf := make([]byte, 16<<10)
	for {
		if err := d.ctx.Err(); err != nil {
			return out, false, err
		}
		n, err := in.Read(buf)
		d.scanned += int64(n)
		if d.scanned > d.limits.maxScanned {
			return out, false, fmt.Errorf("%w: streams decompress past the %d byte limit", core.ErrTooLarge, d.limits.maxScanned)
		}
		chunk := buf[:n]
		if owed := in.trailer - out.kept; owed > 0 {
			take := min(owed, int64(n))
			out.kept += take
			chunk = chunk[take:]
		}
		out.bytes += int64(len(chunk))
		hidden = hidden || !onlySpace(chunk)
		if err != nil {
			if isFinal(err) {
				return out, false, err
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				out.damaged = true
			}
			return out, hidden, nil
		}
	}
}

// commentFinder follows content stream syntax byte by byte to find comments, which
// no reader shows. It needs to know about strings, where a percent sign means
// nothing, and about inline image data, where nothing does. Getting lost in binary
// data can only cost a false warning or a missed comment, never any data.
type commentFinder struct {
	state  commentState
	depth  int
	escape bool
	last   [3]byte // the last three bytes in the normal or inline state
}

type commentState uint8

const (
	inContent commentState = iota
	inString
	inComment
	inInlineData
)

func (f *commentFinder) feed(b []byte) bool {
	for _, c := range b {
		switch f.state {
		case inContent:
			switch {
			case c == '(':
				f.state, f.depth, f.escape = inString, 1, false
			case c == '%':
				f.state = inComment
			case isSpace(c) && f.last[2] == 'D' && f.last[1] == 'I' && !isRegular(f.last[0]):
				f.state = inInlineData
			}
		case inString:
			switch {
			case f.escape:
				f.escape = false
			case c == '\\':
				f.escape = true
			case c == '(':
				f.depth++
			case c == ')':
				f.depth--
				if f.depth == 0 {
					f.state = inContent
				}
			}
		case inComment:
			switch {
			case c == '\n' || c == '\r':
				f.state = inContent
			case !isSpace(c):
				return true
			}
		case inInlineData:
			if isSpace(c) && f.last[2] == 'I' && f.last[1] == 'E' && isSpace(f.last[0]) {
				f.state = inContent
			}
		}
		f.last[0], f.last[1], f.last[2] = f.last[1], f.last[2], c
	}
	return false
}

// maxInlineDict is how much of a possible inline image dictionary gets read. Real
// ones take well under a hundred bytes. One that does not end within this is treated
// as holding something, since nothing a renderer needs takes that much room.
const maxInlineDict = 1 << 10

// maxInlineWork bounds what a document can spend on the search. The candidates of
// a stream full of "BI /A(" each read a whole window, which adds up to something
// quadratic-looking unless capped. Running out is reported as if an image had been
// found, because the search can no longer say there is none.
const maxInlineWork = 256 << 20

// inlineSearch finds inline images that can hold metadata in a stream's bytes. It
// does not track where in the syntax it is, so that binary data cannot put it out of
// step and hide what comes after. The price is that it would believe text spelling
// out such an image, which nothing writes.
type inlineSearch struct {
	budget *int64
}

// search looks through window, which ends at the end of the stream if final. A
// candidate too close to the end to be read whole is left for the next window,
// which will still have it because of scanOverlap. A page's content may be divided
// into streams at any token, so in a content stream a dictionary that reaches the
// end of the stream has not been read whole either, and that one cannot be followed.
func (s inlineSearch) search(window []byte, final, content bool) bool {
	limit := len(window)
	if !final {
		limit -= maxInlineDict
	}
	pos := 0
	for pos < limit {
		i := bytes.Index(window[pos:], []byte("BI"))
		if i < 0 || pos+i >= limit {
			return false
		}
		at := pos + i
		pos = at + 2
		if at > 0 && isRegular(window[at-1]) {
			continue
		}
		dict := window[pos:min(pos+maxInlineDict, len(window))]
		if *s.budget -= int64(len(dict)); *s.budget < 0 {
			return true
		}
		if inlineImageCanHoldData(dict, content || len(dict) == maxInlineDict) {
			return true
		}
	}
	return false
}

// inlineKeys are all the keys an inline image may have, with their abbreviations.
// /L is the length that PDF 2.0 added.
var inlineKeys = keySet("BitsPerComponent", "BPC", "ColorSpace", "CS", "Decode", "D", "DecodeParms", "DP",
	"Filter", "F", "Height", "H", "ImageMask", "IM", "Interpolate", "I", "Width", "W", "Intent", "L")

// inlineFilters are those whose output is only samples, leaving no room for
// metadata. The others hold whole image files.
var inlineFilters = keySet("ASCIIHexDecode", "AHx", "ASCII85Decode", "A85", "LZWDecode", "LZW",
	"FlateDecode", "Fl", "RunLengthDecode", "RL", "CCITTFaxDecode", "CCF")

// inlineImageCanHoldData parses what follows a "BI" as an inline image dictionary,
// up to its "ID". Anything that does not parse as one is simply not one. If open,
// one that gets to the end of dict without an "ID" may go on past it.
func inlineImageCanHoldData(dict []byte, open bool) bool {
	budget := valueBudget{limit: maxInlineDict}
	p := parser{data: dict, budget: &budget}
	found := false
	for {
		p.skipSpace()
		if p.pos >= len(dict) {
			return open
		}
		if p.hasKeyword("ID") {
			return found
		}
		if dict[p.pos] != '/' {
			return false
		}
		key := string(p.name())
		val, err := p.value(0)
		if err != nil {
			return open && p.pos >= len(dict)
		}
		if !inlineKeys[key] {
			found = true
		}
		if key == "F" || key == "Filter" {
			found = found || !inlineFiltersAreSamples(val)
		}
	}
}

func inlineFiltersAreSamples(v object) bool {
	switch t := v.(type) {
	case pdfName:
		return inlineFilters[string(t)]
	case pdfArray:
		for _, e := range t {
			if !inlineFiltersAreSamples(e) {
				return false
			}
		}
		return true
	}
	return false
}

// imagePixelBytes works out how many bytes of samples an image's dimensions use, or
// -1 if its colour space does not say. Abbreviated keys are accepted, as they are on
// the allowlist.
func (d *document) imagePixelBytes(dict *pdfDict) int64 {
	dims := func(long, short string) (int64, bool) {
		v := d.resolve(dict.get(long))
		if v == nil {
			v = d.resolve(dict.get(short))
		}
		n, ok := v.(int64)
		return n, ok
	}
	width, ok1 := dims("Width", "W")
	height, ok2 := dims("Height", "H")
	// These bounds keep the product below within an int64.
	if !ok1 || !ok2 || width < 1 || height < 1 || width > 1<<24 || height > 1<<24 {
		return -1
	}
	bits, comps := int64(1), int64(1)
	mask, _ := d.resolve(dict.get("ImageMask")).(bool)
	short, _ := d.resolve(dict.get("IM")).(bool)
	if !mask && !short {
		var ok bool
		if bits, ok = dims("BitsPerComponent", "BPC"); !ok || bits < 1 || bits > 16 {
			return -1
		}
		space := dict.get("ColorSpace")
		if space == nil {
			space = dict.get("CS")
		}
		if comps = d.colorComponents(space); comps < 1 {
			return -1
		}
	}
	return (width*comps*bits + 7) / 8 * height
}

// colorComponents counts a colour space's components, or returns 0 if it cannot say.
func (d *document) colorComponents(v object) int64 {
	val := d.resolve(v)
	name, _ := val.(pdfName)
	a, isArray := val.(pdfArray)
	if isArray && len(a) > 0 {
		name, _ = d.resolve(a[0]).(pdfName)
	}
	switch name {
	case "DeviceGray", "G", "CalGray", "Indexed", "I", "Separation":
		return 1
	case "DeviceRGB", "RGB", "CalRGB", "Lab":
		return 3
	case "DeviceCMYK", "CMYK":
		return 4
	case "ICCBased":
		if isArray && len(a) > 1 {
			if s, ok := d.resolve(a[1]).(*pdfStream); ok {
				if n, ok := d.resolve(s.dict.get("N")).(int64); ok && n >= 1 && n <= 32 {
					return n
				}
			}
		}
	case "DeviceN":
		if isArray && len(a) > 1 {
			if names, ok := d.resolve(a[1]).(pdfArray); ok && len(names) >= 1 && len(names) <= 32 {
				return int64(len(names))
			}
		}
	}
	return 0
}
