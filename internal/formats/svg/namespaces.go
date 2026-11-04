package svg

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	nsNone   = ""
	nsSVG    = "http://www.w3.org/2000/svg"
	nsXLink  = "http://www.w3.org/1999/xlink"
	nsXML    = "http://www.w3.org/XML/1998/namespace"
	nsXHTML  = "http://www.w3.org/1999/xhtml"
	nsMathML = "http://www.w3.org/1998/Math/MathML"
	nsEvents = "http://www.w3.org/2001/xml-events"

	nsSodipodi = "http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"
	nsInkscape = "http://www.inkscape.org/namespaces/inkscape"

	// unresolvedNamespace stands for a declaration whose URI cannot be determined.
	// It is not a valid URI, so it cannot collide with a real one.
	unresolvedNamespace = "unresolved entity"

	// maxTextBytes is more than core.SafeText shows, and more than any heuristic
	// below needs to look at. maxTextTokens is more pieces than a title has.
	maxTextBytes  = 4096
	maxTextTokens = 16

	// maxDecodedBindings is more prefixes than real files have in scope. Past it, a
	// metadata element is reported without being decoded, because decoding repeats
	// every binding, and a hostile file could otherwise repeat them for every element.
	maxDecodedBindings = 64
)

// keptNamespaces lists what a renderer needs. Everything else is dropped, whether
// the file calls it sodipodi, inkscape, sketch, figma, serif, i, x, rdf or a name
// not yet invented. A renderer must ignore elements and attributes in namespaces it
// does not know, so removing them cannot change what is drawn.
var keptNamespaces = map[string]bool{
	nsNone:   true,
	nsSVG:    true,
	nsXLink:  true,
	nsXML:    true,
	nsXHTML:  true,
	nsMathML: true,
	nsEvents: true,
}

func isSVG(ns string) bool { return ns == nsSVG || ns == nsNone }

// splitName separates a qualified name. The scanner has already checked its shape.
func splitName(name string) (prefix, local string) {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

func displayNamespace(uri string) string {
	if uri == unresolvedNamespace {
		return "an entity declared outside this file"
	}
	return uri
}

// parseEntities reads the general entity declarations in a DOCTYPE's internal
// subset. XML gives the first declaration of a name precedence. Parameter entities
// and external entities are skipped, since neither can name a namespace here, and
// a prefix bound through one ends up removed anyway.
func parseEntities(subset []byte) map[string]string {
	entities := make(map[string]string)
	s := scanner{data: subset}
	marker := []byte("<!ENTITY")
	for s.pos < len(subset) {
		i := s.skipSpace(s.pos)
		rest := subset[min(i, len(subset)):]
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			s.pos = i
			if _, err := s.comment(); err != nil {
				return entities
			}
		case bytes.HasPrefix(rest, []byte("<?")):
			s.pos = i
			if _, err := s.instruction(); err != nil {
				return entities
			}
		case bytes.HasPrefix(rest, marker):
			end, err := s.declaration(i)
			if err != nil {
				return entities
			}
			if name, value, ok := parseEntity(subset[i+len(marker) : end-1]); ok {
				if _, dup := entities[name]; !dup {
					entities[name] = value
				}
			}
			s.pos = end
		case len(rest) > 0 && rest[0] == '<':
			end, err := s.declaration(i)
			if err != nil {
				return entities
			}
			s.pos = end
		default:
			s.pos = i + 1
		}
	}
	return entities
}

// parseEntity reads the inside of a declaration of the form: name "value".
func parseEntity(decl []byte) (name, value string, ok bool) {
	decl = bytes.TrimSpace(decl)
	i := bytes.IndexAny(decl, " \t\r\n")
	if i <= 0 {
		return "", "", false
	}
	rest := bytes.TrimSpace(decl[i:])
	if len(rest) < 2 || (rest[0] != '"' && rest[0] != '\'') || rest[len(rest)-1] != rest[0] {
		return "", "", false
	}
	return string(decl[:i]), string(rest[1 : len(rest)-1]), true
}

// describeMetadata reports a metadata element, with RDF contents decoded.
func (c *cleaner) describeMetadata(raw []byte) {
	c.addXMP(raw, "metadata", finding{"Metadata element", core.CatHidden, core.RiskMed})
}

// describeEditorElement reports a removed element in a foreign namespace.
func (c *cleaner) describeEditorElement(name, ns string, raw []byte) {
	generic := finding{"Editor element", core.CatSoftware, core.RiskLow}
	if xmp.Contains(raw) {
		c.addXMP(raw, "editor element", generic)
		return
	}
	c.add(generic, fmt.Sprintf("%s (%s, %d bytes)", name, displayNamespace(ns), len(raw)), "")
}

// addXMP reports a subtree through the xmp package. The subtree's prefixes are
// usually declared on the root element, so it gets a wrapper repeating them, which
// keeps a renamed prefix from hiding a property.
func (c *cleaner) addXMP(raw []byte, where string, generic finding) {
	if c.findings >= maxFindings {
		c.further++
		return
	}
	if len(c.bindings) > maxDecodedBindings {
		c.add(generic, strconv.Itoa(len(raw))+" bytes", where)
		return
	}
	var wrapped bytes.Buffer
	wrapped.WriteString("<scope")
	c.inScopeDeclarations(&wrapped)
	wrapped.WriteByte('>')
	wrapped.Write(raw)
	wrapped.WriteString("</scope>")
	findings := xmp.Findings(wrapped.Bytes(), where)
	if len(findings) == 1 && findings[0].Field == "XMP packet" {
		c.add(generic, strconv.Itoa(len(raw))+" bytes", where)
		return
	}
	for _, f := range findings {
		c.add(finding{f.Field, f.Category, f.Risk}, f.Value, f.Where)
	}
}

// editorAttributes names the attributes worth a finding of their own: the ones that
// hold file names, paths, labels the user typed, or the tool's version.
var editorAttributes = map[string]finding{
	nsSodipodi + " docname":         {"Document name", core.CatIdentity, core.RiskMed},
	nsSodipodi + " docbase":         {"Document folder", core.CatIdentity, core.RiskHigh},
	nsSodipodi + " absref":          {"Absolute file path", core.CatIdentity, core.RiskHigh},
	nsInkscape + " export-filename": {"Export file name", core.CatIdentity, core.RiskHigh},
	nsInkscape + " version":         {"Editor version", core.CatSoftware, core.RiskLow},
	nsInkscape + " label":           {"Layer or object label", core.CatContent, core.RiskLow},
}

func (c *cleaner) describeEditorAttribute(ns, local string, value []byte) {
	if f, ok := editorAttributes[ns+" "+local]; ok {
		c.add(f, clipped(value), "")
		return
	}
	c.editorAttrs++
}

// clipped returns the start of b for display, so that a comment of a gigabyte does
// not get copied just to be truncated when it is shown.
func clipped(b []byte) string { return string(b[:min(len(b), maxTextBytes)]) }

// bannerPrefixes begin the text that editors put in title and desc elements, such
// as Sketch's "Created with Sketch." Lowercase, since it is compared in lowercase.
var bannerPrefixes = []string{
	"created with", "created using", "created by", "generated by", "generated with",
	"generator:", "made with", "exported from", "exported by", "exported with",
	"produced by", "drawn with", "saved with", "converted by", "converted with",
}

func isToolBanner(text string) bool {
	lower := strings.ToLower(text)
	for _, prefix := range bannerPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// fileExtensions end the file names that editors copy into a title.
var fileExtensions = []string{
	".svg", ".svgz", ".ai", ".eps", ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".tif", ".tiff",
	".webp", ".bmp", ".psd", ".sketch", ".fig", ".cdr", ".xd", ".afdesign", ".drawio",
}

func isFileName(text string) bool {
	lower := strings.ToLower(text)
	for _, ext := range fileExtensions {
		if len(lower) > len(ext) && strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return isLocalLink(text)
}

// isLocalLink recognises a reference into the local file system, which can name the
// user whose folder it is in. A root-relative web path looks the same as a Unix one,
// and a false alarm here costs only a warning. A relative link counts if it passes
// through home folders, as Inkscape's can.
func isLocalLink(value string) bool {
	v := strings.TrimSpace(value)
	lower := strings.ToLower(v)
	switch {
	case strings.HasPrefix(lower, "file:"):
		return true
	case strings.HasPrefix(v, `\\`):
		return true
	case strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//"):
		return true
	case strings.HasPrefix(v, "~/") || strings.HasPrefix(v, `~\`):
		return true
	case climbsThroughHomeFolders(lower):
		return true
	case len(v) >= 3 && v[1] == ':' && (v[2] == '\\' || v[2] == '/') && isASCIILetter(v[0]):
		return true
	}
	return false
}

// climbsThroughHomeFolders recognises paths such as ../../Users/name/picture.png.
func climbsThroughHomeFolders(lower string) bool {
	path := strings.ReplaceAll(lower, `\`, "/")
	for strings.HasPrefix(path, "../") {
		path = path[len("../"):]
	}
	return strings.HasPrefix(path, "users/") || strings.HasPrefix(path, "home/")
}

// quotesOnlyStandardIDs checks that the quoted strings of a DOCTYPE's external
// identifier are W3C's own, which every SVG file that has one uses.
func quotesOnlyStandardIDs(text []byte) bool {
	for {
		i := bytes.IndexAny(text, `"'`)
		if i < 0 {
			return true
		}
		quote := text[i]
		text = text[i+1:]
		j := bytes.IndexByte(text, quote)
		if j < 0 {
			return false
		}
		id := string(text[:j])
		if !strings.HasPrefix(id, "-//W3C//DTD SVG ") && !strings.HasPrefix(id, "http://www.w3.org/") {
			return false
		}
		text = text[j+1:]
	}
}

// decodeReferences expands character references, and removes the whitespace that URL
// parsers ignore, so that a value can be judged as a browser would see it. Nothing it
// produces is written to the output, except for an embedded image, which is re-encoded.
func decodeReferences(value []byte) string {
	if !bytes.Contains(value, []byte("&#")) {
		return string(value)
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); {
		if value[i] != '&' || i+1 >= len(value) || value[i+1] != '#' {
			b.WriteByte(value[i])
			i++
			continue
		}
		end := bytes.IndexByte(value[i:], ';')
		if end < 0 {
			b.Write(value[i:])
			break
		}
		digits, base := string(value[i+2:i+end]), 10
		if len(digits) > 0 && digits[0] == 'x' {
			digits, base = digits[1:], 16
		}
		if n, err := strconv.ParseUint(digits, base, 32); err == nil && n <= 0x10FFFF {
			b.WriteRune(rune(n))
		}
		i += end + 1
	}
	return b.String()
}

// urlWhitespace is what URL parsers remove from anywhere in a URL.
var urlWhitespace = strings.NewReplacer("\t", "", "\n", "", "\r", "")

// normalizedURL returns a URL attribute's value as a browser parses it.
func normalizedURL(value []byte) string {
	return strings.TrimSpace(urlWhitespace.Replace(decodeReferences(value)))
}

// holdsDataURI looks anywhere in text for a data: URI, in any case and through any
// character references. The scheme must stand on its own and run straight into a media
// type or its parameters, so that "metadata:" and "the data: 5" are not mistaken for one.
func holdsDataURI(text []byte) bool {
	if bytes.Contains(text, []byte("&#")) {
		text = []byte(urlWhitespace.Replace(decodeReferences(text)))
	}
	for {
		i := bytes.IndexByte(text, ':')
		if i < 0 || i+1 >= len(text) {
			return false
		}
		next := text[i+1]
		if i >= 4 && bytes.EqualFold(text[i-4:i], []byte("data")) &&
			(i == 4 || !isWordByte(text[i-5])) && (isASCIILetter(next) || next == ',' || next == ';') {
			return true
		}
		text = text[i+1:]
	}
}

func isWordByte(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// asciiLower lowercases only ASCII letters, so that offsets in the result are good for
// the original too. Full Unicode lowercasing can change how long a character is.
func asciiLower(text []byte) []byte {
	out := make([]byte, len(text))
	for i, c := range text {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// linksToLocalFile looks through CSS for what isLocalLink recognises: any url(), and
// bare file: URLs, which an @import can use without one.
func linksToLocalFile(text []byte) bool {
	if bytes.Contains(text, []byte("&#")) {
		text = []byte(decodeReferences(text))
	}
	lower := asciiLower(text)
	for from := 0; ; {
		i := bytes.Index(lower[from:], []byte("file:"))
		if i < 0 {
			break
		}
		i += from
		if i == 0 || !isWordByte(lower[i-1]) {
			return true
		}
		from = i + 1
	}
	for from := 0; ; {
		i := bytes.Index(lower[from:], []byte("url("))
		if i < 0 {
			return false
		}
		start := from + i + len("url(")
		end := bytes.IndexByte(text[start:], ')')
		if end < 0 {
			return false
		}
		target := strings.Trim(string(text[start:start+end]), " \t\r\n\"'")
		if !strings.HasPrefix(target, "#") && isLocalLink(target) {
			return true
		}
		from = start + end
	}
}

// quotesLocalLink looks for a local path among the quoted strings of a DOCTYPE's
// external identifier.
func quotesLocalLink(text []byte) bool {
	for {
		i := bytes.IndexAny(text, `"'`)
		if i < 0 {
			return false
		}
		quote := text[i]
		text = text[i+1:]
		j := bytes.IndexByte(text, quote)
		if j < 0 {
			return false
		}
		if isLocalLink(string(text[:j])) {
			return true
		}
		text = text[j+1:]
	}
}

func isASCIILetter(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

var basicEntities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&")

// unescapeBasic decodes the predefined entities in text shown to the user or tested
// against the heuristics. Nothing it produces is written to the output.
func unescapeBasic(s string) string { return basicEntities.Replace(s) }

var attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", `"`, "&quot;")

func escapeAttr(s string) string { return attrEscaper.Replace(s) }
