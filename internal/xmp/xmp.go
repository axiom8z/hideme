// Package xmp reports what an XMP metadata packet reveals.
//
// HideMe removes every XMP packet whole, so nothing here decides what stays. The
// job is to tell the user what was in the packet, which is why Findings cannot
// fail: a packet that makes no sense still gets reported and still gets removed.
//
// A well-formed packet goes through encoding/xml, which resolves namespaces
// properly. Anything else goes to a tolerant scan, which recovers what it can
// from the same text, since damaged packets are exactly the ones a user should
// hear about.
package xmp

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
)

// maxPacketBytes bounds how much of a packet is examined, and so how much memory
// the XML parser can spend on a hostile packet stuffed with tiny attributes. Real
// packets are kilobytes, and the whole packet is removed either way, so a longer
// one only loses detail in its report.
const maxPacketBytes = 4 << 20

// signatures are the byte strings that give away an XMP packet. The rdf:RDF
// element counts on its own because the x:xmpmeta wrapper is optional, and
// x:xapmeta is the name older Adobe tools used for it. The namespace names are
// there because a packet may bind any prefix it likes, which changes the element
// names above but cannot change these.
var signatures = wideSignatures(
	"<x:xmpmeta",
	"<x:xapmeta",
	"<?xpacket",
	"ns.adobe.com/xap/",
	"<rdf:RDF",
	"adobe:ns:meta/",
	"/22-rdf-syntax-ns#",
)

// wideSignatures adds the UTF-16 and UTF-32 spellings, which XMP permits and a
// plain byte search would otherwise walk straight past.
func wideSignatures(ascii ...string) [][]byte {
	var out [][]byte
	for _, s := range ascii {
		out = append(out, []byte(s))
		for _, order := range []binary.AppendByteOrder{binary.LittleEndian, binary.BigEndian} {
			var two, four []byte
			for i := range len(s) {
				two = order.AppendUint16(two, uint16(s[i]))
				four = order.AppendUint32(four, uint32(s[i]))
			}
			out = append(out, two, four)
		}
	}
	return out
}

// Contains reports whether data holds what looks like an XMP packet, in any
// encoding XMP permits. It errs towards true: a false alarm removes something
// harmless, while a miss leaks metadata.
func Contains(data []byte) bool {
	for _, signature := range signatures {
		if bytes.Contains(data, signature) {
			return true
		}
	}
	return false
}

// Findings describes a packet, with where as the prefix of each Finding.Where.
// It returns at least one finding for any input at all, and every finding is
// core.ActionRemove. Values are sanitised, so they are safe to print directly.
func Findings(packet []byte, where string) []core.Finding {
	text := decodeText(packet[:min(len(packet), maxPacketBytes)])
	w := newWalker()
	malformed := parseXML(text, w) != nil
	if malformed {
		w = newWalker()
		scan(text, w)
	}
	findings := w.findings(where)
	if len(findings) == 0 {
		return []core.Finding{genericFinding(len(packet), malformed, where)}
	}
	return findings
}

func genericFinding(size int, malformed bool, where string) core.Finding {
	value := byteCount(size)
	if malformed {
		value += ", not well-formed"
	}
	return core.Finding{
		Category: core.CatHidden,
		Risk:     core.RiskMed,
		Field:    "XMP packet",
		Value:    value,
		Where:    core.SafeText(where),
		Action:   core.ActionRemove,
	}
}

// decodeText turns a packet in any encoding XMP permits into a UTF-8 string and
// drops whatever precedes the first tag, such as the signature JPEG puts in
// front of a packet, which would stop an XML parser at its first NUL.
func decodeText(b []byte) string {
	var text string
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		text = string(b[3:])
	// The UTF-32 marks come first because FF FE also begins the UTF-16LE mark.
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0x00, 0x00}):
		text = decodeUTF32(b[4:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0x00, 0x00, 0xFE, 0xFF}):
		text = decodeUTF32(b[4:], binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		text = decodeUTF16(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		text = decodeUTF16(b[2:], binary.BigEndian)
	// A packet can have its byte order mark inside the xpacket begin attribute
	// instead of at the start, so the shape of the first tag decides.
	case bytes.HasPrefix(b, []byte{'<', 0x00, 0x00, 0x00}):
		text = decodeUTF32(b, binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0x00, 0x00, 0x00, '<'}):
		text = decodeUTF32(b, binary.BigEndian)
	case bytes.HasPrefix(b, []byte{'<', 0x00}):
		text = decodeUTF16(b, binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0x00, '<'}):
		text = decodeUTF16(b, binary.BigEndian)
	default:
		text = string(b)
	}
	if i := strings.IndexByte(text, '<'); i > 0 {
		text = text[i:]
	}
	return text
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

func decodeUTF32(b []byte, order binary.ByteOrder) string {
	runes := make([]rune, len(b)/4)
	for i := range runes {
		// Out-of-range values become U+FFFD when converted to a string.
		runes[i] = rune(order.Uint32(b[4*i:]))
	}
	return string(runes)
}

// XML namespaces mapped to the prefix the properties table uses. Packets may
// bind any prefix to these, and encoding/xml reports names by namespace.
var canonicalPrefixes = map[string]string{
	"adobe:ns:meta/": "x",
	"http://www.w3.org/1999/02/22-rdf-syntax-ns#": "rdf",
	"http://www.w3.org/XML/1998/namespace":        "xml",
	"http://purl.org/dc/elements/1.1/":            "dc",
	"http://ns.adobe.com/xap/1.0/":                "xmp",
	"http://ns.adobe.com/xap/1.0/mm/":             "xmpMM",
	"http://ns.adobe.com/xap/1.0/rights/":         "xmpRights",
	"http://ns.adobe.com/exif/1.0/":               "exif",
	"http://cipa.jp/exif/1.0/":                    "exifEX",
	"http://ns.adobe.com/tiff/1.0/":               "tiff",
	"http://ns.adobe.com/exif/1.0/aux/":           "aux",
	"http://ns.adobe.com/photoshop/1.0/":          "photoshop",
	"http://iptc.org/std/Iptc4xmpCore/1.0/xmlns/": "Iptc4xmpCore",
	"http://ns.adobe.com/pdf/1.3/":                "pdf",
}

// qualified names an element or attribute using the canonical prefix. An
// undeclared prefix is left in Space by encoding/xml, so it still matches the
// table when a broken writer forgot the declaration.
func qualified(name xml.Name) string {
	if name.Space == "" {
		return name.Local
	}
	if prefix, ok := canonicalPrefixes[name.Space]; ok {
		return prefix + ":" + name.Local
	}
	return name.Space + ":" + name.Local
}

// parseXML feeds a well-formed packet to w and reports an error for anything
// else, sending the packet to the tolerant scan.
//
// The decoder stays in strict mode because its lenient mode repairs a mismatched
// end tag by closing every open element, and whatever follows the damage would
// then go unread with no error to say so.
//
// The one exception is what follows the root element. In a real packet that is
// padding, sometimes NUL bytes, which the decoder rejects but which holds nothing.
// A further tag in that text means the packet goes on, so it counts as damage.
func parseXML(text string, w *walker) error {
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.Entity = xml.HTMLEntity
	// The text is already UTF-8, whatever its XML declaration still says.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	sawElement := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if !sawElement {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			rest := text[min(int(dec.InputOffset()), len(text)):]
			if sawElement && w.depth == 0 && !strings.Contains(rest, "<") {
				return nil
			}
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			sawElement = true
			attrs := make([]attribute, len(t.Attr))
			for i, a := range t.Attr {
				attrs[i] = attribute{qualified(a.Name), a.Value}
			}
			w.start(qualified(t.Name), attrs)
		case xml.EndElement:
			w.end(qualified(t.Name))
		case xml.CharData:
			w.chars(string(t))
		}
	}
}
