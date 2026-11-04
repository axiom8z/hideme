package xmp

import (
	"html"
	"strings"
)

// scan is the tolerant path for packets encoding/xml rejects: truncated, with
// mismatched tags, or with bytes that are not valid in their stated encoding. It
// makes one pass over the text, so a hostile packet cannot make it quadratic,
// and it reads names as written, since a packet this broken has no reliable
// namespace declarations.
func scan(text string, w *walker) {
	for {
		lt := strings.IndexByte(text, '<')
		if lt < 0 {
			w.chars(html.UnescapeString(text))
			break
		}
		w.chars(html.UnescapeString(text[:lt]))
		text = text[lt+1:]
		switch {
		case strings.HasPrefix(text, "!--"):
			text = skipPast(text, "-->")
		case strings.HasPrefix(text, "![CDATA["):
			body := text[len("![CDATA["):]
			end := strings.Index(body, "]]>")
			if end < 0 {
				w.chars(body)
				text = ""
			} else {
				w.chars(body[:end])
				text = body[end+len("]]>"):]
			}
		case strings.HasPrefix(text, "?"), strings.HasPrefix(text, "!"):
			text = skipPast(text, ">")
		case strings.HasPrefix(text, "/"):
			name, rest := readName(text[1:])
			w.end(name)
			text = skipPast(rest, ">")
		default:
			text = scanStartTag(text, w)
		}
	}
	// A packet cut off inside a property still has a value worth reporting.
	if w.open != nil {
		w.flush()
		w.open = nil
	}
}

// scanStartTag reads a start tag whose "<" was already consumed and returns the
// text after it. A "<" not followed by a name is stray text and is skipped.
func scanStartTag(text string, w *walker) string {
	name, rest := readName(text)
	if name == "" {
		return text
	}
	var attrs []attribute
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" || rest[0] == '>' || strings.HasPrefix(rest, "/>") {
			break
		}
		attrName, afterName := readName(rest)
		if attrName == "" {
			rest = rest[1:] // a stray character, such as a lone quote
			continue
		}
		var value string
		value, rest = readAttrValue(afterName)
		attrs = append(attrs, attribute{attrName, html.UnescapeString(value)})
	}
	w.start(name, attrs)
	switch {
	case strings.HasPrefix(rest, "/>"):
		w.end(name)
		return rest[2:]
	case rest != "":
		return rest[1:]
	}
	return ""
}

// readName returns the name at the start of s and the text following it.
func readName(s string) (name, rest string) {
	end := strings.IndexAny(s, " \t\r\n=/><\"'")
	if end < 0 {
		return s, ""
	}
	return s[:end], s[end:]
}

// readAttrValue reads an optional `= "value"`. A missing closing quote takes the
// rest of the text, and a missing opening quote takes the next word.
func readAttrValue(s string) (value, rest string) {
	s = strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(s, "=") {
		return "", s
	}
	s = strings.TrimLeft(s[1:], " \t\r\n")
	if s == "" {
		return "", ""
	}
	if quote := s[0]; quote == '"' || quote == '\'' {
		s = s[1:]
		if end := strings.IndexByte(s, quote); end >= 0 {
			return s[:end], s[end+1:]
		}
		return s, ""
	}
	end := strings.IndexAny(s, " \t\r\n>")
	if end < 0 {
		return s, ""
	}
	return s[:end], s[end:]
}

// skipPast returns the text after the first occurrence of marker, or nothing if
// the marker never appears.
func skipPast(text, marker string) string {
	if i := strings.Index(text, marker); i >= 0 {
		return text[i+len(marker):]
	}
	return ""
}
