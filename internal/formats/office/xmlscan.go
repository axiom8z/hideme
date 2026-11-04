package office

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
)

type tokenKind uint8

const (
	tokenText tokenKind = iota
	tokenCDATA
	tokenComment
	tokenInstruction
	tokenDoctype
	tokenStart
	tokenEnd
)

// xmlAttr is one attribute of a start tag. The offsets let an edit touch only
// this attribute and leave every other byte of the tag as it was.
type xmlAttr struct {
	name             []byte // raw qualified name
	start, end       int    // from the whitespace before the name to just past the closing quote
	valStart, valEnd int    // the raw value between the quotes

	space, local  string // resolved by the editor
	drop, replace bool
	newValue      string
}

// xmlToken is one piece of markup or text. Its slices point into the scanner's
// data, and its attrs are reused by the next token.
type xmlToken struct {
	kind               tokenKind
	start, end         int
	bodyStart, bodyEnd int // what lies inside CDATA markers, comments and instructions
	name               []byte
	selfClosing        bool
	attrs              []xmlAttr
}

// xmlScanner splits a document into tokens. It enforces the lexical rules that
// keep tags and text apart, because everything downstream depends on telling a
// real attribute from text that merely looks like one. Whatever it cannot
// classify with certainty is an error, never a guess.
type xmlScanner struct {
	data []byte
	pos  int
	tok  xmlToken
}

func scanError(pos int, what string) error {
	return core.Malformedf("XML at byte %d: %s", pos, what)
}

// next advances to the next token. It returns false at the end of the data.
func (s *xmlScanner) next() (bool, error) {
	if s.pos >= len(s.data) {
		return false, nil
	}
	start := s.pos
	s.tok = xmlToken{start: start, attrs: s.tok.attrs[:0]}
	rest := s.data[start:]
	var err error
	switch {
	case rest[0] != '<':
		s.tok.kind = tokenText
		s.pos = len(s.data)
		if i := bytes.IndexByte(rest, '<'); i >= 0 {
			s.pos = start + i
		}
	case bytes.HasPrefix(rest, []byte("<!--")):
		err = s.delimited(tokenComment, 4, "-->")
	case bytes.HasPrefix(rest, []byte("<![CDATA[")):
		err = s.delimited(tokenCDATA, 9, "]]>")
	case bytes.HasPrefix(rest, []byte("<?")):
		err = s.delimited(tokenInstruction, 2, "?>")
	case bytes.HasPrefix(rest, []byte("<!")):
		err = s.doctype()
	case bytes.HasPrefix(rest, []byte("</")):
		err = s.endTag()
	default:
		err = s.startTag()
	}
	if err != nil {
		return false, err
	}
	s.tok.end = s.pos
	return true, nil
}

func (s *xmlScanner) delimited(kind tokenKind, open int, closer string) error {
	s.tok.kind = kind
	s.tok.bodyStart = s.tok.start + open
	i := bytes.Index(s.data[s.tok.bodyStart:], []byte(closer))
	if i < 0 {
		return scanError(s.tok.start, "unterminated "+closer+" section")
	}
	s.tok.bodyEnd = s.tok.bodyStart + i
	s.pos = s.tok.bodyEnd + len(closer)
	return nil
}

// doctype accepts only a declaration with no internal subset. An internal subset
// can define entities, which would let a value such as w:author="&who;" hide from
// a scanner that deliberately never expands anything.
func (s *xmlScanner) doctype() error {
	s.tok.kind = tokenDoctype
	if !bytes.HasPrefix(s.data[s.tok.start:], []byte("<!DOCTYPE")) {
		return scanError(s.tok.start, "unknown declaration")
	}
	var quote byte
	for i := s.tok.start + 2; i < len(s.data); i++ {
		c := s.data[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			return core.Unsupportedf("XML at byte %d: DOCTYPE with an internal subset", s.tok.start)
		case c == '>':
			s.pos = i + 1
			return nil
		}
	}
	return scanError(s.tok.start, "unterminated DOCTYPE")
}

func (s *xmlScanner) endTag() error {
	s.tok.kind = tokenEnd
	nameStart := s.tok.start + 2
	nameEnd := s.nameEnd(nameStart)
	if nameEnd == nameStart {
		return scanError(s.tok.start, "end tag without a name")
	}
	s.tok.name = s.data[nameStart:nameEnd]
	i := s.skipSpace(nameEnd)
	if i >= len(s.data) || s.data[i] != '>' {
		return scanError(s.tok.start, "malformed end tag")
	}
	s.pos = i + 1
	return nil
}

func (s *xmlScanner) startTag() error {
	s.tok.kind = tokenStart
	nameStart := s.tok.start + 1
	i := s.nameEnd(nameStart)
	if i == nameStart {
		return scanError(s.tok.start, "tag without a name")
	}
	s.tok.name = s.data[nameStart:i]
	for {
		spaceStart := i
		i = s.skipSpace(i)
		if i >= len(s.data) {
			return scanError(s.tok.start, "unterminated tag")
		}
		switch s.data[i] {
		case '>':
			s.pos = i + 1
			return s.checkDuplicateAttrs()
		case '/':
			if i+1 >= len(s.data) || s.data[i+1] != '>' {
				return scanError(i, "stray / in a tag")
			}
			s.tok.selfClosing = true
			s.pos = i + 2
			return s.checkDuplicateAttrs()
		}
		if i == spaceStart {
			return scanError(i, "attributes must be separated by whitespace")
		}
		var err error
		if i, err = s.attribute(spaceStart, i); err != nil {
			return err
		}
	}
}

func (s *xmlScanner) attribute(spaceStart, nameStart int) (int, error) {
	nameEnd := s.nameEnd(nameStart)
	if nameEnd == nameStart {
		return 0, scanError(nameStart, "malformed attribute")
	}
	i := s.skipSpace(nameEnd)
	if i >= len(s.data) || s.data[i] != '=' {
		return 0, scanError(nameStart, "attribute without a value")
	}
	i = s.skipSpace(i + 1)
	if i >= len(s.data) || (s.data[i] != '"' && s.data[i] != '\'') {
		return 0, scanError(nameStart, "attribute value without quotes")
	}
	valStart := i + 1
	length := bytes.IndexByte(s.data[valStart:], s.data[i])
	if length < 0 {
		return 0, scanError(nameStart, "unterminated attribute value")
	}
	valEnd := valStart + length
	if bytes.IndexByte(s.data[valStart:valEnd], '<') >= 0 {
		return 0, scanError(nameStart, "< in an attribute value")
	}
	s.tok.attrs = append(s.tok.attrs, xmlAttr{
		name:     s.data[nameStart:nameEnd],
		start:    spaceStart,
		end:      valEnd + 1,
		valStart: valStart,
		valEnd:   valEnd,
	})
	return valEnd + 1, nil
}

// checkDuplicateAttrs matters because parsers differ on which duplicate wins, so
// a second copy of an attribute could carry the real value past a check on the
// first. A tag rarely has more than a few attributes, so the simple quadratic
// loop is the common case and a map guards against the hostile one.
func (s *xmlScanner) checkDuplicateAttrs() error {
	attrs := s.tok.attrs
	if len(attrs) > 32 {
		seen := make(map[string]bool, len(attrs))
		for i := range attrs {
			if seen[string(attrs[i].name)] {
				return scanError(attrs[i].start, "duplicate attribute")
			}
			seen[string(attrs[i].name)] = true
		}
		return nil
	}
	for i := range attrs {
		for j := range i {
			if bytes.Equal(attrs[i].name, attrs[j].name) {
				return scanError(attrs[i].start, "duplicate attribute")
			}
		}
	}
	return nil
}

func (s *xmlScanner) skipSpace(i int) int {
	for i < len(s.data) && isSpace(s.data[i]) {
		i++
	}
	return i
}

func (s *xmlScanner) nameEnd(i int) int {
	for i < len(s.data) && isNameByte(s.data[i]) {
		i++
	}
	return i
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isNameByte(c byte) bool {
	if c <= ' ' {
		return false
	}
	switch c {
	case '/', '>', '=', '<', '"', '\'', '&':
		return false
	}
	return true
}

func isBlank(b []byte) bool {
	for _, c := range b {
		if !isSpace(c) {
			return false
		}
	}
	return true
}

// decodeEntities expands the predefined and numeric references. Anything else
// stays as written: without an internal subset nothing in the file can define
// others, and the editor refuses them where an external DTD might.
func decodeEntities(raw []byte) string {
	amp := bytes.IndexByte(raw, '&')
	if amp < 0 {
		return string(raw)
	}
	const longestEntity = 12 // &#x0010FFFF; with room for leading zeros
	var b strings.Builder
	for amp >= 0 {
		b.Write(raw[:amp])
		raw = raw[amp:]
		window := raw[:min(len(raw), longestEntity)]
		semi := bytes.IndexByte(window, ';')
		if semi < 0 {
			b.WriteByte('&')
			raw = raw[1:]
		} else {
			expanded, ok := expandEntity(string(raw[1:semi]))
			if ok {
				b.WriteString(expanded)
				raw = raw[semi+1:]
			} else {
				b.WriteByte('&')
				raw = raw[1:]
			}
		}
		amp = bytes.IndexByte(raw, '&')
	}
	b.Write(raw)
	return b.String()
}

// hasUnknownEntity finds a reference other than the predefined and numeric ones.
// With no DTD that is the reader's error, and the part reaches it unchanged. Behind
// a DOCTYPE it may be legal, and removing the DOCTYPE would then break the part.
func hasUnknownEntity(raw []byte) bool {
	for {
		amp := bytes.IndexByte(raw, '&')
		if amp < 0 {
			return false
		}
		raw = raw[amp+1:]
		semi := bytes.IndexByte(raw, ';')
		if semi < 0 {
			return false
		}
		if _, ok := expandEntity(string(raw[:semi])); !ok {
			return true
		}
	}
}

func expandEntity(name string) (string, bool) {
	switch name {
	case "amp":
		return "&", true
	case "lt":
		return "<", true
	case "gt":
		return ">", true
	case "quot":
		return `"`, true
	case "apos":
		return "'", true
	}
	if !strings.HasPrefix(name, "#") {
		return "", false
	}
	digits, base := name[1:], 10
	if strings.HasPrefix(digits, "x") {
		digits, base = digits[1:], 16
	}
	n, err := strconv.ParseUint(digits, base, 32)
	if err != nil || n > utf8.MaxRune {
		return "", false
	}
	return string(rune(n)), true
}

// escapeXML makes text safe in both text and attribute positions.
func escapeXML(s string) string {
	if !strings.ContainsAny(s, `&<>"'`) {
		return s
	}
	return xmlEscaper.Replace(s)
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// decodeWide does what utf16.Decode does, turning a lone surrogate into U+FFFD, with
// one output buffer instead of a slice of units and a slice of runes on the way there.
func decodeWide(body []byte, order binary.ByteOrder) []byte {
	out := make([]byte, 0, len(body)/2)
	for i := 0; i < len(body); i += 2 {
		r := rune(order.Uint16(body[i:]))
		if utf16.IsSurrogate(r) {
			var low rune
			if i+3 < len(body) {
				low = rune(order.Uint16(body[i+2:]))
			}
			if r = utf16.DecodeRune(r, low); r != utf8.RuneError {
				i += 2
			}
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

var declaredEncoding = regexp.MustCompile(`^\x{FEFF}?<\?xml\s[^?]*?\bencoding\s*=\s*["']([^"']*)["']`)

// normalizeXML makes a part's bytes safe to scan. The scanner works on bytes, so
// it sees markup only in encodings where markup looks like ASCII. UTF-16 is legal
// in OPC parts and is converted to UTF-8, with the declaration updated to match.
// Encodings that could disguise markup (UTF-7 spells < as +ADw-) are refused
// instead of being skimmed for tags that the scanner cannot see.
func normalizeXML(data []byte) ([]byte, error) {
	transcoded := false
	var order binary.ByteOrder
	body := data
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		order, body = binary.LittleEndian, data[2:]
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		order, body = binary.BigEndian, data[2:]
	case bytes.HasPrefix(data, []byte{'<', 0}):
		order = binary.LittleEndian
	case bytes.HasPrefix(data, []byte{0, '<'}):
		order = binary.BigEndian
	}
	if order != nil {
		if len(body)%2 != 0 {
			return nil, core.Malformedf("UTF-16 XML with an odd number of bytes")
		}
		data = decodeWide(body, order)
		transcoded = true
	}
	// NUL is never legal in XML. It also marks UTF-32 and BOM-less wide text that
	// got this far, which would otherwise look like one long run of plain text.
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, core.Malformedf("XML contains a NUL byte")
	}
	match := declaredEncoding.FindSubmatchIndex(data)
	name := ""
	if match != nil {
		name = strings.ToLower(string(data[match[2]:match[3]]))
	}
	switch {
	case transcoded:
		if match != nil {
			data = append(append(bytes.Clone(data[:match[2]]), "UTF-8"...), data[match[3]:]...)
		}
	case name == "" || name == "utf-8" || name == "utf8":
		if !utf8.Valid(data) {
			return nil, core.Malformedf("XML declared as UTF-8 is not valid UTF-8")
		}
	case name == "us-ascii" || strings.HasPrefix(name, "iso-8859-") || strings.HasPrefix(name, "windows-125"):
		// Every byte below 0x80 means what it means in ASCII, so markup is visible.
	case strings.HasPrefix(name, "utf-16") || strings.HasPrefix(name, "ucs-2"):
		return nil, core.Malformedf("XML declared as %q has no wide characters", name)
	default:
		return nil, core.Unsupportedf("XML encoding %q", name)
	}
	return data, nil
}
