package svg

import (
	"bytes"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
)

type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokText
	tokCDATA
	tokComment
	tokPI
	tokDoctype
	tokStart
	tokEnd
)

// attr locates one attribute inside a start tag.
type attr struct {
	name                 string
	start, end           int // from the whitespace before the name to past the closing quote
	valueStart, valueEnd int
}

// token is one piece of the document. Offsets index the scanner's data.
type token struct {
	kind        tokenKind
	start, end  int
	name        string // element name or processing instruction target
	attrs       []attr // owned by the scanner: valid until the next token
	selfClosing bool
	// bodyStart and bodyEnd locate a processing instruction's data, a comment's
	// text, or a DOCTYPE's internal subset, whose absence leaves them equal.
	bodyStart, bodyEnd int
}

// scanner splits XML into tokens without building a tree or interpreting a single
// reference, which is what lets a cleaned file keep every byte it did not remove.
// It is deliberately strict about what it accepts, because a tag it misread would
// be a tag it failed to clean: anything that is not well-formed XML is an error.
type scanner struct {
	data  []byte
	pos   int
	attrs []attr
	seen  map[string]struct{}
}

// maxReusedAttrMap is more attributes than one tag holds in any real file.
const maxReusedAttrMap = 256

func (s *scanner) errorf(off int, format string, args ...any) error {
	return core.Malformedf("svg: offset %d: %s", off, fmt.Sprintf(format, args...))
}

// next returns the next token, or a tokEOF token at the end of the data.
func (s *scanner) next() (token, error) {
	if s.pos >= len(s.data) {
		return token{kind: tokEOF, start: s.pos, end: s.pos}, nil
	}
	if s.data[s.pos] != '<' {
		return s.text()
	}
	rest := s.data[s.pos:]
	switch {
	case bytes.HasPrefix(rest, []byte("<!--")):
		return s.comment()
	case bytes.HasPrefix(rest, []byte("<![CDATA[")):
		return s.cdata()
	case bytes.HasPrefix(rest, []byte("<?")):
		return s.instruction()
	case bytes.HasPrefix(rest, []byte("<!DOCTYPE")):
		return s.doctype()
	case bytes.HasPrefix(rest, []byte("<!")):
		return token{}, s.errorf(s.pos, "unexpected markup declaration")
	case bytes.HasPrefix(rest, []byte("</")):
		return s.endTag()
	}
	return s.startTag()
}

func (s *scanner) text() (token, error) {
	start := s.pos
	end := len(s.data)
	if i := bytes.IndexByte(s.data[start:], '<'); i >= 0 {
		end = start + i
	}
	body := s.data[start:end]
	if bytes.Contains(body, []byte("]]>")) {
		return token{}, s.errorf(start, "]]> outside a CDATA section")
	}
	if !validReferences(body) {
		return token{}, s.errorf(start, "invalid entity or character reference in text")
	}
	s.pos = end
	return token{kind: tokText, start: start, end: end, bodyStart: start, bodyEnd: end}, nil
}

func (s *scanner) comment() (token, error) {
	start := s.pos
	i := bytes.Index(s.data[start+len("<!--"):], []byte("-->"))
	if i < 0 {
		return token{}, s.errorf(start, "unterminated comment")
	}
	bodyStart := start + len("<!--")
	s.pos = bodyStart + i + len("-->")
	return token{kind: tokComment, start: start, end: s.pos, bodyStart: bodyStart, bodyEnd: bodyStart + i}, nil
}

func (s *scanner) cdata() (token, error) {
	start := s.pos
	bodyStart := start + len("<![CDATA[")
	i := bytes.Index(s.data[bodyStart:], []byte("]]>"))
	if i < 0 {
		return token{}, s.errorf(start, "unterminated CDATA section")
	}
	s.pos = bodyStart + i + len("]]>")
	return token{kind: tokCDATA, start: start, end: s.pos, bodyStart: bodyStart, bodyEnd: bodyStart + i}, nil
}

func (s *scanner) instruction() (token, error) {
	start := s.pos
	nameEnd, err := s.scanName(start + len("<?"))
	if err != nil {
		return token{}, err
	}
	i := bytes.Index(s.data[nameEnd:], []byte("?>"))
	if i < 0 {
		return token{}, s.errorf(start, "unterminated processing instruction")
	}
	if i > 0 && !isSpace(s.data[nameEnd]) {
		return token{}, s.errorf(nameEnd, "invalid processing instruction target")
	}
	s.pos = nameEnd + i + len("?>")
	return token{
		kind: tokPI, start: start, end: s.pos,
		name:      string(s.data[start+len("<?") : nameEnd]),
		bodyStart: nameEnd, bodyEnd: nameEnd + i,
	}, nil
}

// doctype finds the end of a DOCTYPE. A plain search for '>' would stop inside the
// first entity declaration, so it follows the grammar, and refuses anything else.
// Leniency here would be dangerous: XML parsers that recover from a stray quote do
// so in different ways, and could see a different root element than this scanner.
func (s *scanner) doctype() (token, error) {
	start := s.pos
	i := start + len("<!DOCTYPE")
	if i >= len(s.data) || !isSpace(s.data[i]) {
		return token{}, s.errorf(start, "malformed DOCTYPE")
	}
	i, err := s.scanName(s.skipSpace(i))
	if err != nil {
		return token{}, err
	}
	tok := token{kind: tokDoctype, start: start}
	sawSubset := false
	for {
		i = s.skipSpace(i)
		rest := s.data[min(i, len(s.data)):]
		switch {
		case len(rest) == 0:
			return token{}, s.errorf(start, "unterminated DOCTYPE")
		case rest[0] == '>':
			s.pos = i + 1
			tok.end = s.pos
			return tok, nil
		case sawSubset:
			return token{}, s.errorf(i, "unexpected content after a DOCTYPE's internal subset")
		case rest[0] == '[':
			end, err := s.internalSubset(i + 1)
			if err != nil {
				return token{}, err
			}
			tok.bodyStart, tok.bodyEnd = i+1, end
			sawSubset = true
			i = end + 1
		case rest[0] == '"' || rest[0] == '\'':
			if i, err = s.skipQuoted(i); err != nil {
				return token{}, err
			}
		case bytes.HasPrefix(rest, []byte("SYSTEM")) || bytes.HasPrefix(rest, []byte("PUBLIC")):
			i += len("SYSTEM")
		default:
			return token{}, s.errorf(i, "unexpected content in a DOCTYPE")
		}
	}
}

// internalSubset returns the offset of the ']' that closes a DOCTYPE's internal
// subset beginning at i. Only declarations, comments, instructions and parameter
// entity references may appear there.
func (s *scanner) internalSubset(i int) (int, error) {
	for i < len(s.data) {
		rest := s.data[i:]
		switch {
		case rest[0] == ']':
			return i, nil
		case isSpace(rest[0]):
			i++
		case bytes.HasPrefix(rest, []byte("<!--")):
			j := bytes.Index(rest[len("<!--"):], []byte("-->"))
			if j < 0 {
				return 0, s.errorf(i, "unterminated comment in DOCTYPE")
			}
			i += len("<!--") + j + len("-->")
		case bytes.HasPrefix(rest, []byte("<?")):
			j := bytes.Index(rest, []byte("?>"))
			if j < 0 {
				return 0, s.errorf(i, "unterminated processing instruction in DOCTYPE")
			}
			// XML allows quotes here, but encoding/xml, for one, takes them to open
			// a string. No real file needs this, so the disagreement is not worth
			// having.
			if bytes.ContainsAny(rest[:j], `"'`) {
				return 0, s.errorf(i, "quote in a processing instruction inside a DOCTYPE")
			}
			i += j + len("?>")
		case bytes.HasPrefix(rest, []byte("<!")):
			end, err := s.declaration(i)
			if err != nil {
				return 0, err
			}
			i = end
		case rest[0] == '%':
			end, err := s.scanName(i + 1)
			if err != nil {
				return 0, err
			}
			if end >= len(s.data) || s.data[end] != ';' {
				return 0, s.errorf(i, "malformed parameter entity reference")
			}
			i = end + 1
		default:
			return 0, s.errorf(i, "unexpected content in a DOCTYPE's internal subset")
		}
	}
	return 0, s.errorf(i, "unterminated DOCTYPE internal subset")
}

// declaration returns the offset just past a markup declaration starting at i.
func (s *scanner) declaration(i int) (int, error) {
	for j := i + 1; j < len(s.data); {
		switch s.data[j] {
		case '"', '\'':
			end, err := s.skipQuoted(j)
			if err != nil {
				return 0, err
			}
			j = end
		case '>':
			return j + 1, nil
		case '<':
			return 0, s.errorf(j, "< inside a declaration in a DOCTYPE")
		default:
			j++
		}
	}
	return 0, s.errorf(i, "unterminated declaration in DOCTYPE")
}

// skipQuoted returns the offset past the quoted string starting at i.
func (s *scanner) skipQuoted(i int) (int, error) {
	j := bytes.IndexByte(s.data[i+1:], s.data[i])
	if j < 0 {
		return 0, s.errorf(i, "unterminated string in DOCTYPE")
	}
	return i + j + 2, nil
}

func (s *scanner) endTag() (token, error) {
	start := s.pos
	nameEnd, err := s.scanName(start + len("</"))
	if err != nil {
		return token{}, err
	}
	i := s.skipSpace(nameEnd)
	if i >= len(s.data) || s.data[i] != '>' {
		return token{}, s.errorf(i, "malformed end tag")
	}
	s.pos = i + 1
	return token{kind: tokEnd, start: start, end: s.pos, name: string(s.data[start+len("</") : nameEnd])}, nil
}

func (s *scanner) startTag() (token, error) {
	start := s.pos
	nameEnd, err := s.scanName(start + 1)
	if err != nil {
		return token{}, err
	}
	name := s.data[start+1 : nameEnd]
	if !validQName(name) {
		return token{}, s.errorf(start, "invalid element name %q", name)
	}
	s.attrs = s.attrs[:0]
	end, selfClosing, err := s.scanAttributes(nameEnd, ">", "/>")
	if err != nil {
		return token{}, err
	}
	s.pos = end
	return token{
		kind: tokStart, start: start, end: end, name: string(name),
		attrs: s.attrs, selfClosing: selfClosing,
	}, nil
}

// scanAttributes reads attributes from i until one of the two closers, appending
// to s.attrs. It returns the offset past the closer, and whether it was the second.
func (s *scanner) scanAttributes(i int, closer, altCloser string) (end int, alt bool, err error) {
	// Clearing a map takes time in proportion to the largest it has been, so one huge
	// tag must not make every later tag slow.
	if s.seen == nil || len(s.seen) > maxReusedAttrMap {
		s.seen = make(map[string]struct{})
	}
	clear(s.seen)
	for {
		spaceStart := i
		i = s.skipSpace(i)
		rest := s.data[min(i, len(s.data)):]
		if len(rest) == 0 {
			return 0, false, s.errorf(i, "unterminated tag")
		}
		if bytes.HasPrefix(rest, []byte(closer)) {
			return i + len(closer), false, nil
		}
		if bytes.HasPrefix(rest, []byte(altCloser)) {
			return i + len(altCloser), true, nil
		}
		if i == spaceStart {
			return 0, false, s.errorf(i, "expected whitespace before an attribute")
		}
		a, next, err := s.scanAttribute(spaceStart, i)
		if err != nil {
			return 0, false, err
		}
		if _, dup := s.seen[a.name]; dup {
			return 0, false, s.errorf(i, "duplicate attribute %q", a.name)
		}
		s.seen[a.name] = struct{}{}
		s.attrs = append(s.attrs, a)
		i = next
	}
}

func (s *scanner) scanAttribute(spaceStart, i int) (attr, int, error) {
	nameEnd, err := s.scanName(i)
	if err != nil {
		return attr{}, 0, err
	}
	if !validQName(s.data[i:nameEnd]) {
		return attr{}, 0, s.errorf(i, "invalid attribute name %q", s.data[i:nameEnd])
	}
	j := s.skipSpace(nameEnd)
	if j >= len(s.data) || s.data[j] != '=' {
		return attr{}, 0, s.errorf(j, "attribute without a value")
	}
	j = s.skipSpace(j + 1)
	if j >= len(s.data) || (s.data[j] != '"' && s.data[j] != '\'') {
		return attr{}, 0, s.errorf(j, "attribute value without quotes")
	}
	quote := s.data[j]
	valueStart := j + 1
	k := bytes.IndexByte(s.data[valueStart:], quote)
	if k < 0 {
		return attr{}, 0, s.errorf(j, "unterminated attribute value")
	}
	value := s.data[valueStart : valueStart+k]
	if bytes.IndexByte(value, '<') >= 0 {
		return attr{}, 0, s.errorf(valueStart, "< inside an attribute value")
	}
	if !validReferences(value) {
		return attr{}, 0, s.errorf(valueStart, "invalid entity or character reference in an attribute value")
	}
	end := valueStart + k + 1
	return attr{
		name: string(s.data[i:nameEnd]), start: spaceStart, end: end,
		valueStart: valueStart, valueEnd: valueStart + k,
	}, end, nil
}

// scanName returns the offset past the XML name at i.
func (s *scanner) scanName(i int) (int, error) {
	start := i
	for i < len(s.data) {
		r, size := rune(s.data[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRune(s.data[i:])
		}
		if i == start && !isNameStart(r) || i > start && !isNameChar(r) {
			break
		}
		i += size
	}
	if i == start {
		return 0, s.errorf(start, "expected a name")
	}
	return i, nil
}

func (s *scanner) skipSpace(i int) int {
	for i < len(s.data) && isSpace(s.data[i]) {
		i++
	}
	return i
}

// skipSpaceIn returns how many leading bytes of b are XML whitespace.
func (s *scanner) skipSpaceIn(b []byte) int {
	i := 0
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// isNameStart and isNameChar implement the Name production of XML 1.0.
func isNameStart(r rune) bool {
	switch {
	case r == ':' || r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z':
		return true
	case r < 0xC0 || r == 0xD7 || r == 0xF7:
		return false
	case r <= 0x2FF, r >= 0x370 && r <= 0x1FFF && r != 0x37E:
		return true
	case r == 0x200C, r == 0x200D, r >= 0x2070 && r <= 0x218F, r >= 0x2C00 && r <= 0x2FEF:
		return true
	case r >= 0x3001 && r <= 0xD7FF, r >= 0xF900 && r <= 0xFDCF, r >= 0xFDF0 && r <= 0xFFFD:
		return true
	}
	return r >= 0x10000 && r <= 0xEFFFF
}

func isNameChar(r rune) bool {
	return isNameStart(r) || r == '-' || r == '.' || r >= '0' && r <= '9' ||
		r == 0xB7 || r >= 0x300 && r <= 0x36F || r == 0x203F || r == 0x2040
}

// validQName applies the Namespaces in XML rule: at most one colon, with text on
// both sides. A name that breaks it has no namespace this handler could judge.
func validQName(name []byte) bool {
	i := bytes.IndexByte(name, ':')
	if i < 0 {
		return true
	}
	return i > 0 && i < len(name)-1 && bytes.IndexByte(name[i+1:], ':') < 0
}

// validReferences checks every '&' in text or an attribute value starts a
// reference. It does not expand them: an undeclared entity may come from an
// external DTD, so only the syntax can be judged.
func validReferences(b []byte) bool {
	for {
		i := bytes.IndexByte(b, '&')
		if i < 0 {
			return true
		}
		b = b[i+1:]
		j := bytes.IndexByte(b, ';')
		if j <= 0 || !validReference(b[:j]) {
			return false
		}
		b = b[j+1:]
	}
}

func validReference(ref []byte) bool {
	if ref[0] != '#' {
		for i := 0; i < len(ref); {
			r, size := utf8.DecodeRune(ref[i:])
			if i == 0 && !isNameStart(r) || !isNameChar(r) {
				return false
			}
			i += size
		}
		return true
	}
	digits, base := string(ref[1:]), 10
	if len(digits) > 0 && digits[0] == 'x' {
		digits, base = digits[1:], 16
	}
	n, err := strconv.ParseUint(digits, base, 32)
	return err == nil && n <= 0x10FFFF && isXMLChar(rune(n))
}

// isXMLChar implements the Char production of XML 1.0.
func isXMLChar(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' ||
		r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF
}

// validChars checks that data is UTF-8 holding only characters XML allows.
func validChars(data []byte) error {
	for i := 0; i < len(data); {
		c := data[i]
		if c < utf8.RuneSelf {
			if c < 0x20 && !isSpace(c) {
				return core.Malformedf("svg: offset %d: control character %#02x", i, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return core.Malformedf("svg: offset %d: invalid UTF-8", i)
		}
		if !isXMLChar(r) {
			return core.Malformedf("svg: offset %d: character %#x is not allowed in XML", i, r)
		}
		i += size
	}
	return nil
}
