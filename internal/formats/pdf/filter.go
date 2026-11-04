package pdf

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/ascii85"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/axiom8z/hideme/internal/core"
)

// maxFilterChain bounds how many filters one stream may stack. Real files use
// three at most. Naming a filter costs four bytes and decoding through it costs
// memory and stack, so without this a small file could ask for millions of them.
const maxFilterChain = 8

// filterStep is one entry of a stream's filter chain.
type filterStep struct {
	name  string
	parms *pdfDict
}

// filterChain reads /Filter and /DecodeParms, each of which may be a single
// value or an array.
func (d *document) filterChain(dict *pdfDict) ([]filterStep, error) {
	filter, err := d.deref(dict.get("Filter"))
	if err != nil {
		return nil, err
	}
	short, err := d.abbreviatedFilter(dict)
	if err != nil {
		return nil, err
	}
	if short != nil {
		if filter != nil {
			return nil, core.Malformedf("a stream names its filters twice, as /Filter and as /F")
		}
		filter = short
	}
	if dict.has("DecodeParms") && dict.has("DP") {
		return nil, core.Malformedf("a stream names its filter parameters twice, as /DecodeParms and as /DP")
	}
	parmsKey := "DecodeParms"
	if dict.has("DP") {
		parmsKey = "DP"
	}
	parms, err := d.deref(dict.get(parmsKey))
	if err != nil {
		return nil, err
	}
	switch f := filter.(type) {
	case nil:
		return nil, nil
	case pdfName:
		step := filterStep{name: string(f)}
		if pa, ok := parms.(pdfArray); ok && len(pa) > 0 {
			parms, err = d.deref(pa[0])
			if err != nil {
				return nil, err
			}
		}
		step.parms, _ = parms.(*pdfDict)
		return []filterStep{step}, nil
	case pdfArray:
		if len(f) > maxFilterChain {
			return nil, fmt.Errorf("%w: a stream stacks %d filters", core.ErrTooLarge, len(f))
		}
		pa, _ := parms.(pdfArray)
		steps := make([]filterStep, 0, len(f))
		for i, e := range f {
			val, err := d.deref(e)
			if err != nil {
				return nil, err
			}
			n, ok := val.(pdfName)
			if !ok {
				return nil, core.Malformedf("invalid /Filter entry")
			}
			step := filterStep{name: string(n)}
			switch {
			case i < len(pa):
				val, err := d.deref(pa[i])
				if err != nil {
					return nil, err
				}
				step.parms, _ = val.(*pdfDict)
			case pa == nil && len(f) == 1:
				step.parms, _ = parms.(*pdfDict)
			}
			steps = append(steps, step)
		}
		return steps, nil
	}
	return nil, core.Malformedf("invalid /Filter")
}

// abbreviatedFilter reads /F when it holds what an inline image means by it. On a
// stream /F names an external file, which is a string or a dictionary. A name or a
// list of names can only be filters, and readers such as pdf.js decode the stream
// with them, so this must too or it would be blind to what they show.
func (d *document) abbreviatedFilter(dict *pdfDict) (object, error) {
	val, err := d.deref(dict.get("F"))
	if err != nil {
		return nil, err
	}
	switch f := val.(type) {
	case pdfName:
		return f, nil
	case pdfArray:
		if len(f) == 0 {
			return nil, nil
		}
		for _, e := range f {
			val, err := d.deref(e)
			if err != nil {
				return nil, err
			}
			if _, ok := val.(pdfName); !ok {
				return nil, nil
			}
		}
		return f, nil
	}
	return nil, nil
}

// errUnknownFilter means a chain uses a filter this package does not decode. It
// is not itself an error in the file, which may hold JBIG2 images and so on.
var errUnknownFilter = errors.New("filter is not decoded by this package")

// decodes reports whether this package can read through a filter.
func (d *document) decodes(step filterStep) bool {
	switch step.name {
	case "FlateDecode", "Fl", "LZWDecode", "LZW", "ASCII85Decode", "A85",
		"ASCIIHexDecode", "AHx", "RunLengthDecode", "RL":
		return true
	case "Crypt":
		// Encrypted documents are refused, so only the identity filter can do
		// nothing, and any other name means data that cannot be read.
		if step.parms == nil {
			return true
		}
		name := d.nameOf(step.parms, "Name")
		return name == "" || name == "Identity"
	}
	return false
}

// filterInput is what one filter reads, counting what it takes. A filter that has
// reached its end-of-data marker stops taking, so afterwards the count says where
// its data ended. flate.NewReader reads no further than it needs from a reader that
// has ReadByte, which it would not promise of any other.
type filterInput struct {
	r *bufio.Reader
	n int64
	// trailer is how many bytes after the end of the data belong to the format,
	// and so are not extra: a zlib stream's checksum.
	trailer int64
}

func (f *filterInput) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	f.n += int64(n)
	return n, err
}

func (f *filterInput) ReadByte() (byte, error) {
	c, err := f.r.ReadByte()
	if err == nil {
		f.n++
	}
	return c, err
}

// decoder is a filter chain opened for reading.
type decoder struct {
	r      io.Reader
	inputs []*filterInput
}

// openChain returns a reader of a stream's decoded bytes, predictors included.
// Stream data does not need to be held in memory to be read this way.
func (d *document) openChain(raw []byte, steps []filterStep) (*decoder, error) {
	dec := &decoder{r: bytes.NewReader(raw)}
	for _, step := range steps {
		if !d.decodes(step) {
			return nil, errUnknownFilter // the name is the file's own text, and errors get printed
		}
		in := &filterInput{r: bufio.NewReader(dec.r)}
		var r io.Reader
		switch step.name {
		case "FlateDecode", "Fl":
			r = newFlateReader(in)
		case "LZWDecode", "LZW":
			r = newLZWReader(in, d.earlyChange(step.parms))
		case "ASCII85Decode", "A85":
			r = ascii85.NewDecoder(&untilByte{r: in, stop: '~', tail: '>'})
		case "ASCIIHexDecode", "AHx":
			r = hex.NewDecoder(&untilByte{r: in, stop: '>', dropSpace: true})
		case "RunLengthDecode", "RL":
			r = &runLengthReader{r: in}
		default: // Crypt, which decodes says is the identity filter
			r = in
		}
		if step.name != "Crypt" {
			var err error
			if r, err = d.withPredictor(r, step); err != nil {
				return nil, err
			}
		}
		dec.r = r
		dec.inputs = append(dec.inputs, in)
	}
	return dec, nil
}

// newFlateReader reads zlib data without zlib's checksum, since a wrong checksum
// on a stream that otherwise decodes is common and costs nothing here. Data with
// no valid zlib header is tried as bare deflate, as other readers do.
func newFlateReader(in *filterInput) io.Reader {
	if h, err := in.r.Peek(2); err == nil && h[0]&0x0F == 8 && (uint(h[0])<<8|uint(h[1]))%31 == 0 {
		discarded, _ := in.r.Discard(2)
		in.n += int64(discarded)
		in.trailer = 4
	}
	return flate.NewReader(in)
}

// untilByte ends a reader at an end-of-data marker, which the standard decoders
// do not understand, and optionally drops whitespace, which encoding/hex rejects.
// A marker of two bytes that is not completed is damage, not an end.
type untilByte struct {
	r         *filterInput
	stop      byte
	tail      byte // the marker's second byte, if it has one
	dropSpace bool
	done      bool
}

func (u *untilByte) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && !u.done {
		c, err := u.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		switch {
		case c == u.stop:
			u.done = true
			if err := u.finishMarker(); err != nil {
				return n, err
			}
		case u.dropSpace && isSpace(c):
		default:
			p[n] = c
			n++
		}
	}
	if n == 0 && u.done {
		return 0, io.EOF
	}
	return n, nil
}

func (u *untilByte) finishMarker() error {
	if u.tail == 0 {
		return nil
	}
	c, err := u.r.ReadByte()
	if err != nil {
		return nil // the file gave out inside the marker, leaving nothing after it
	}
	if c != u.tail {
		return core.Malformedf("end-of-data marker is damaged")
	}
	return nil
}

// runLengthReader undoes RunLengthDecode: a length byte says how many literal
// bytes follow or how many times the next byte repeats, and 128 ends the data.
type runLengthReader struct {
	r      io.ByteReader
	left   int
	repeat bool
	value  byte
	err    error
}

func (l *runLengthReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if l.left > 0 {
			if !l.repeat {
				if l.value, l.err = l.r.ReadByte(); l.err != nil {
					l.left = 0
					break
				}
			}
			p[n] = l.value
			n++
			l.left--
			continue
		}
		if l.err != nil {
			break
		}
		l.next()
	}
	if n > 0 {
		return n, nil
	}
	return 0, l.err
}

func (l *runLengthReader) next() {
	c, err := l.r.ReadByte()
	switch {
	case err != nil:
		l.err = err
	case c == 128:
		l.err = io.EOF
	case c < 128:
		l.left, l.repeat = int(c)+1, false
	default:
		if l.value, l.err = l.r.ReadByte(); l.err == nil {
			l.left, l.repeat = 257-int(c), true
		}
	}
}

// LZW codes that mean something other than a table entry. The standard library's
// compress/lzw says of itself that it does not implement the variant TIFF and PDF
// use, which changes code width one code early.
const (
	lzwClear    = 256
	lzwEnd      = 257
	lzwFirst    = 258
	lzwTable    = 4096
	lzwMinWidth = 9
	lzwMaxWidth = 12
)

type lzwReader struct {
	r       io.ByteReader
	early   int
	bits    uint32
	nbits   uint
	width   uint
	next    int
	prev    int // the previous code, or -1 straight after a clear
	prefix  [lzwTable]uint16
	suffix  [lzwTable]byte
	stack   [lzwTable + 1]byte
	pending []byte
	err     error
}

func newLZWReader(r io.ByteReader, early int) *lzwReader {
	return &lzwReader{r: r, early: early, width: lzwMinWidth, next: lzwFirst, prev: -1}
}

func (l *lzwReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(l.pending) > 0 {
			c := copy(p[n:], l.pending)
			l.pending = l.pending[c:]
			n += c
			continue
		}
		if l.err != nil {
			break
		}
		l.step()
	}
	if n > 0 {
		return n, nil
	}
	return 0, l.err
}

func (l *lzwReader) readCode() (int, error) {
	for l.nbits < l.width {
		c, err := l.r.ReadByte()
		if err != nil {
			return 0, err
		}
		l.bits = l.bits<<8 | uint32(c)
		l.nbits += 8
	}
	l.nbits -= l.width
	return int(l.bits>>l.nbits) & (1<<l.width - 1), nil
}

// step reads one code and leaves its bytes in pending. An entry's prefix was always
// defined before it, so following prefixes cannot loop.
func (l *lzwReader) step() {
	code, err := l.readCode()
	switch {
	case err != nil:
		l.err = err
		return
	case code == lzwClear:
		l.width, l.next, l.prev = lzwMinWidth, lzwFirst, -1
		return
	case code == lzwEnd:
		l.err = io.EOF
		return
	case code > l.next, code == l.next && l.prev < 0:
		l.err = core.Malformedf("invalid LZW code %d", code)
		return
	}
	i := len(l.stack)
	walk := code
	if code == l.next {
		// The code being defined by this very use: the previous string again, plus
		// its own first byte, which the walk below is about to find.
		i--
		walk = l.prev
	}
	for walk >= lzwFirst {
		i--
		l.stack[i] = l.suffix[walk]
		walk = int(l.prefix[walk])
	}
	first := byte(walk)
	i--
	l.stack[i] = first
	if code == l.next {
		l.stack[len(l.stack)-1] = first
	}
	l.pending = l.stack[i:]
	if l.prev >= 0 && l.next < lzwTable {
		l.prefix[l.next] = uint16(l.prev)
		l.suffix[l.next] = first
		l.next++
		if l.next+l.early >= 1<<l.width && l.width < lzwMaxWidth {
			l.width++
		}
	}
	l.prev = code
}

func (d *document) earlyChange(parms *pdfDict) int {
	if parms == nil {
		return 1
	}
	if d.parmInt(parms, "EarlyChange", 1) == 0 {
		return 0
	}
	return 1
}

// decodeStream decodes a stream into memory, predictor included, enforcing the
// caps on memory that make flate bombs harmless. Damage partway through keeps
// what decoded before it, because the callers cope with short data: structural
// data fails its own checks, and a metadata packet is reported as far as it goes.
func (d *document) decodeStream(s *pdfStream) ([]byte, error) {
	steps, err := d.filterChain(s.dict)
	if err != nil {
		return nil, err
	}
	return d.decodeSteps(s.raw, steps)
}

func (d *document) decodeSteps(raw []byte, steps []filterStep) ([]byte, error) {
	dec, err := d.openChain(raw, steps)
	if errors.Is(err, errUnknownFilter) {
		return nil, core.Unsupportedf("%v", err)
	}
	if err != nil {
		return nil, err
	}
	limit := min(d.limits.maxDecodedStream, d.limits.maxDecodedTotal-d.decoded)
	out, err := io.ReadAll(io.LimitReader(dec.r, limit+1))
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%w: a stream decompresses past the %d byte limit", core.ErrTooLarge, limit)
	}
	if err != nil && len(out) == 0 {
		return nil, core.Malformedf("stream cannot be decoded") // decoders quote the bytes they reject
	}
	d.decoded += int64(len(out))
	return out, nil
}

// withPredictor undoes the PNG or TIFF predictor of a Flate or LZW filter, if it
// has one. Cross-reference streams use these routinely to make their columns
// compress well.
func (d *document) withPredictor(r io.Reader, step filterStep) (io.Reader, error) {
	if step.parms == nil || (step.name != "FlateDecode" && step.name != "Fl" && step.name != "LZWDecode" && step.name != "LZW") {
		return r, nil
	}
	pred := d.parmInt(step.parms, "Predictor", 1)
	if pred <= 1 {
		return r, nil
	}
	colors := d.parmInt(step.parms, "Colors", 1)
	bits := d.parmInt(step.parms, "BitsPerComponent", 8)
	columns := d.parmInt(step.parms, "Columns", 1)
	// These bounds make the multiplications below safe. A row wider than the
	// limit is no longer plausible for the structural data this decodes.
	if colors < 1 || colors > 32 || bits < 1 || bits > 16 || columns < 1 || columns > 1<<20 {
		return nil, core.Malformedf("invalid predictor parameters")
	}
	p := &predictorReader{
		doc:      d,
		r:        bufio.NewReader(r),
		rowLen:   (colors*bits*columns + 7) / 8,
		pixelLen: max((colors*bits+7)/8, 1),
		png:      pred >= 10,
	}
	switch {
	case pred == 2:
		if bits != 8 {
			return nil, core.Unsupportedf("TIFF predictor with %d-bit components", bits)
		}
	case pred < 10:
		return nil, core.Malformedf("unknown predictor %d", pred)
	}
	return p, nil
}

func (d *document) parmInt(parms *pdfDict, key string, fallback int) int {
	n, ok := d.resolve(parms.get(key)).(int64)
	if !ok || n < -1<<31 || n > 1<<31 {
		return fallback
	}
	return int(n)
}

// predictorRowCost is what a row counts for against limits.maxScanned on top of its
// bytes. Rows of an ordinary width never notice, but one-byte rows take several times
// as long as the same bytes without a predictor, and a flate bomb can be made of them.
const predictorRowCost = 32

// predictorReader undoes a predictor one row at a time. Rows grow as they are read,
// because their size is a claim: a tiny stream may say its rows are huge, and
// should cost what it holds rather than what it says.
type predictorReader struct {
	doc      *document
	r        *bufio.Reader
	rowLen   int
	pixelLen int
	png      bool
	row      []byte
	prev     []byte
	pending  []byte
	err      error
}

// Read returns as many rows as fit, because a hostile stream with one-byte rows must
// not cost a call per byte all the way up the chain.
func (p *predictorReader) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		if len(p.pending) > 0 {
			c := copy(b[n:], p.pending)
			p.pending = p.pending[c:]
			n += c
			continue
		}
		if p.err != nil {
			break
		}
		p.nextRow()
	}
	if n > 0 {
		return n, nil
	}
	return 0, p.err
}

func (p *predictorReader) nextRow() {
	p.doc.scanned += predictorRowCost
	var tag byte
	if p.png {
		var err error
		if tag, err = p.r.ReadByte(); err != nil {
			p.err = err
			return
		}
	}
	err := p.fillRow()
	if len(p.row) > 0 {
		if p.png {
			if tagErr := undoPNGRow(tag, p.row, p.prev, p.pixelLen); tagErr != nil {
				p.err = tagErr
				return
			}
		} else {
			for i := p.pixelLen; i < len(p.row); i++ {
				p.row[i] += p.row[i-p.pixelLen]
			}
		}
		p.row, p.prev = p.prev, p.row
		p.pending = p.prev
	}
	if err != nil {
		p.err = err
	}
}

// fillRow reads up to one row. A short last row is kept, as other readers keep it.
func (p *predictorReader) fillRow() error {
	const step = 64 << 10
	p.row = p.row[:0]
	for len(p.row) < p.rowLen {
		start := len(p.row)
		want := min(p.rowLen-start, step)
		p.row = slices.Grow(p.row, want)[:start+want]
		n, err := io.ReadFull(p.r, p.row[start:])
		p.row = p.row[:start+n]
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return io.EOF
			}
			return err
		}
	}
	return nil
}

func undoPNGRow(tag byte, row, prev []byte, pixelLen int) error {
	above := func(i int) byte {
		if i < len(prev) {
			return prev[i]
		}
		return 0
	}
	left := func(i int) byte {
		if i >= pixelLen {
			return row[i-pixelLen]
		}
		return 0
	}
	aboveLeft := func(i int) byte {
		if i >= pixelLen {
			return above(i - pixelLen)
		}
		return 0
	}
	switch tag {
	case 0:
	case 1:
		for i := range row {
			row[i] += left(i)
		}
	case 2:
		for i := range row {
			row[i] += above(i)
		}
	case 3:
		for i := range row {
			row[i] += byte((int(left(i)) + int(above(i))) / 2)
		}
	case 4:
		for i := range row {
			row[i] += paeth(left(i), above(i), aboveLeft(i))
		}
	default:
		return core.Malformedf("invalid PNG predictor tag %d", tag)
	}
	return nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
