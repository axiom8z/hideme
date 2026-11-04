package pdf

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
)

// maxNesting bounds how deeply arrays and dictionaries may nest, so a hostile
// file cannot exhaust the stack here or in anything that later walks the value.
const maxNesting = 500

// valueBudget caps how many values a document may parse into. Each one costs
// memory however few bytes it took in the file.
type valueBudget struct{ used, limit int }

func (b *valueBudget) spend() error {
	b.used++
	if b.used > b.limit {
		return fmt.Errorf("%w: more than %d values in the document", core.ErrTooLarge, b.limit)
	}
	return nil
}

// parser reads PDF syntax from a byte slice, either the file or a decoded object
// stream.
type parser struct {
	data   []byte
	pos    int
	budget *valueBudget
}

func isSpace(c byte) bool {
	switch c {
	case 0, '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

func isDelimiter(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func isRegular(c byte) bool { return !isSpace(c) && !isDelimiter(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// skipSpace passes whitespace and comments.
func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case isSpace(c):
			p.pos++
		case c == '%':
			for p.pos < len(p.data) && p.data[p.pos] != '\n' && p.data[p.pos] != '\r' {
				p.pos++
			}
		default:
			return
		}
	}
}

// token reads a run of regular characters, which is empty at a delimiter.
func (p *parser) token() []byte {
	start := p.pos
	for p.pos < len(p.data) && isRegular(p.data[p.pos]) {
		p.pos++
	}
	return p.data[start:p.pos]
}

// hasKeyword reports whether keyword comes next and takes it if so. Whitespace
// and comments before it are passed either way.
func (p *parser) hasKeyword(keyword string) bool {
	p.skipSpace()
	if !bytes.HasPrefix(p.data[p.pos:], []byte(keyword)) {
		return false
	}
	end := p.pos + len(keyword)
	if end < len(p.data) && isRegular(p.data[end]) {
		return false
	}
	p.pos = end
	return true
}

// unsigned reads a plain non-negative integer that fits in 31 bits, which covers
// every offset, object number and generation in a file below core.MaxInMemory.
func (p *parser) unsigned() (int, bool) {
	p.skipSpace()
	save := p.pos
	if n, ok := parseUnsigned(p.token()); ok {
		return n, true
	}
	p.pos = save
	return 0, false
}

func parseUnsigned(tok []byte) (int, bool) {
	if len(tok) == 0 || len(tok) > 10 {
		return 0, false
	}
	n := 0
	for _, c := range tok {
		if !isDigit(c) {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n <= math.MaxInt32
}

// value reads one direct object or reference.
func (p *parser) value(depth int) (object, error) {
	if depth > maxNesting {
		return nil, core.Malformedf("values nested deeper than %d levels", maxNesting)
	}
	if err := p.budget.spend(); err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos >= len(p.data) {
		return nil, core.Malformedf("unexpected end of data")
	}
	switch c := p.data[p.pos]; c {
	case '/':
		return p.name(), nil
	case '(':
		return p.literalString()
	case '[':
		return p.array(depth)
	case '<':
		if p.pos+1 < len(p.data) && p.data[p.pos+1] == '<' {
			return p.dictionary(depth)
		}
		return p.hexString()
	case ')', ']', '>', '{', '}':
		return nil, core.Malformedf("unexpected %q at offset %d", c, p.pos)
	}
	return p.numberOrKeyword()
}

func (p *parser) numberOrKeyword() (object, error) {
	start := p.pos
	tok := p.token()
	switch string(tok) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	num, ok := parseNumber(tok)
	if !ok {
		return nil, core.Malformedf("unexpected token at offset %d", start)
	}
	if n, isInt := num.(int64); isInt && n >= 0 && n <= math.MaxInt32 {
		if r, isRef := p.referenceTail(int(n)); isRef {
			return r, nil
		}
	}
	return num, nil
}

// referenceTail looks past an integer for "gen R". The position only moves if
// they are there, because a pair of integers in an array is common.
func (p *parser) referenceTail(num int) (pdfRef, bool) {
	save := p.pos
	if gen, ok := p.unsigned(); ok && p.hasKeyword("R") {
		return pdfRef{num, gen}, true
	}
	p.pos = save
	return pdfRef{}, false
}

// parseNumber accepts the PDF number syntax only, with no exponents or doubled
// signs, so anything it returns can be written back and read the same way.
func parseNumber(tok []byte) (object, bool) {
	digits, dots := 0, 0
	for i, c := range tok {
		switch {
		case isDigit(c):
			digits++
		case c == '.':
			dots++
		case (c == '+' || c == '-') && i == 0:
		default:
			return nil, false
		}
	}
	if digits == 0 || dots > 1 {
		return nil, false
	}
	if dots == 0 {
		if n, err := strconv.ParseInt(string(tok), 10, 64); err == nil {
			return n, true
		}
	}
	return pdfReal(tok), true
}

// name reads a name, decoding #xx. A # not followed by two hex digits stands for
// itself, which is what readers do with names from before PDF 1.2.
func (p *parser) name() pdfName {
	p.pos++ // the slash
	tok := p.token()
	if bytes.IndexByte(tok, '#') < 0 {
		return pdfName(tok)
	}
	out := make([]byte, 0, len(tok))
	for i := 0; i < len(tok); i++ {
		if tok[i] == '#' && i+2 < len(tok) {
			hi, ok1 := hexDigit(tok[i+1])
			lo, ok2 := hexDigit(tok[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, tok[i])
	}
	return pdfName(out)
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (p *parser) literalString() (object, error) {
	start := p.pos
	p.pos++
	var out []byte
	depth := 1
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		p.pos++
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return pdfString{data: out}, nil
			}
		case '\\':
			out = p.escape(out)
			continue
		case '\r':
			// A raw end of line inside a string reads as a single line feed.
			if p.pos < len(p.data) && p.data[p.pos] == '\n' {
				p.pos++
			}
			c = '\n'
		}
		out = append(out, c)
	}
	return nil, core.Malformedf("unterminated string at offset %d", start)
}

// escape decodes what follows a backslash and appends it to out.
func (p *parser) escape(out []byte) []byte {
	if p.pos >= len(p.data) {
		return out
	}
	c := p.data[p.pos]
	p.pos++
	switch c {
	case 'n':
		return append(out, '\n')
	case 'r':
		return append(out, '\r')
	case 't':
		return append(out, '\t')
	case 'b':
		return append(out, '\b')
	case 'f':
		return append(out, '\f')
	case '\r':
		if p.pos < len(p.data) && p.data[p.pos] == '\n' {
			p.pos++
		}
		return out
	case '\n':
		return out
	}
	if c < '0' || c > '7' {
		return append(out, c)
	}
	v := int(c - '0')
	for range 2 {
		if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '7' {
			break
		}
		v = v*8 + int(p.data[p.pos]-'0')
		p.pos++
	}
	return append(out, byte(v)) // the high bit of an overflowing \ooo is ignored
}

func (p *parser) hexString() (object, error) {
	start := p.pos
	p.pos++
	var out []byte
	var pending byte
	odd := false
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		p.pos++
		if c == '>' {
			if odd {
				out = append(out, pending<<4) // a missing final digit reads as 0
			}
			return pdfString{data: out, hex: true}, nil
		}
		if isSpace(c) {
			continue
		}
		digit, ok := hexDigit(c)
		if !ok {
			return nil, core.Malformedf("invalid hex string at offset %d", start)
		}
		if odd {
			out = append(out, pending<<4|digit)
		} else {
			pending = digit
		}
		odd = !odd
	}
	return nil, core.Malformedf("unterminated hex string at offset %d", start)
}

func (p *parser) array(depth int) (object, error) {
	start := p.pos
	p.pos++
	out := pdfArray{}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, core.Malformedf("unterminated array at offset %d", start)
		}
		if p.data[p.pos] == ']' {
			p.pos++
			return out, nil
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *parser) dictionary(depth int) (object, error) {
	start := p.pos
	p.pos += 2
	d := &pdfDict{}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, core.Malformedf("unterminated dictionary at offset %d", start)
		}
		if bytes.HasPrefix(p.data[p.pos:], []byte(">>")) {
			p.pos += 2
			return d, nil
		}
		if p.data[p.pos] != '/' {
			return nil, core.Malformedf("dictionary key expected at offset %d", p.pos)
		}
		key := p.name()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		d.set(string(key), v)
	}
}
