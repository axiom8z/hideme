// Package svg removes metadata from SVG images.
//
// SVG is XML text, and a round trip through an XML library would reorder and
// reformat everything, and expand the entities Illustrator's files depend on. So
// this package scans tags at the byte level and copies every byte it does not
// remove. Namespaces are resolved through the file's own xmlns declarations, and
// only namespaces that affect rendering survive, so renaming a prefix or inventing
// a new editor namespace does not get anything past it.
package svg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxProlog limits how far into a file the root element may start, which also
	// limits what Match reads. Editors write a few hundred bytes here.
	maxProlog = 1 << 20

	// maxDepth and maxBindings limit how much a hostile file can make the scanner
	// remember. Real files nest a few dozen elements and declare a dozen prefixes.
	// Every prefixed name costs a search of the bindings, so that limit also bounds
	// the time a hostile file can demand.
	maxDepth    = 1 << 12
	maxBindings = 1 << 10

	// maxFindings bounds the report. What is past the limit is still removed, and
	// counted.
	maxFindings = 512

	// maxEmbeddedImage limits the memory one embedded raster can demand.
	maxEmbeddedImage = 256 << 20

	// contextCheckInterval is how many tokens pass between checks for cancellation.
	contextCheckInterval = 1 << 12
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func init() { core.Register(handler{}) }

type handler struct{}

func (handler) Format() core.Format {
	return core.Format{
		ID:         "svg",
		Name:       "SVG image",
		Family:     core.FamilyImage,
		Extensions: []string{".svg"},
		// A drawing's visible text can quote any signature, so a match in the raw
		// bytes does not by itself mean that metadata survived.
		ResidueWarnOnly: true,
		Limitations: []string{
			"Only UTF-8 and US-ASCII files are supported. UTF-16 and other encodings are refused, and compressed .svgz files are not recognised.",
			"A DOCTYPE with an internal subset is kept untouched, with a warning, because Illustrator's files need its entities. Anything else in that subset stays too.",
			"Elements and attributes outside the SVG standards are removed, but drawn text, element IDs, class names, namespace prefixes, CSS and the values of standard attributes are content and stay. Style sheet comments, scripts, data: URIs outside image elements, and HTML or MathML in a foreignObject stay too, each with a warning. An image in CSS is not cleaned.",
			"Title and desc elements are kept and reported unless they look like a tool banner or a file name, because they can be accessibility text. Strict removes them. Attributes that hold such text are kept and reported too, and Strict does not remove those.",
		},
	}
}

func (handler) Match(head []byte, src core.Source) bool {
	data := head
	if len(head) >= core.HeadSize && src.Size() > int64(len(head)) {
		data = make([]byte, min(src.Size(), maxProlog))
		n, err := src.ReadAt(data, 0)
		if n < len(data) && err != nil {
			return false
		}
	}
	return sniff(data) != sniffOther
}

func (handler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	_, rep, err := analyse(ctx, src, opts)
	return rep, err
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	out, rep, err := analyse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	if _, err := dst.Write(out); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	return rep, nil
}

// analyse produces the cleaned file and the report together. Inspect discards the
// bytes, which is what guarantees that it reports what Clean does, nested images
// included.
func analyse(ctx context.Context, src core.Source, opts core.Options) ([]byte, *core.Report, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, nil, err
	}
	switch sniff(data[:min(len(data), maxProlog)]) {
	case sniffUTF16:
		return nil, nil, core.Unsupportedf("svg: UTF-16 files are not supported")
	case sniffOther:
		return nil, nil, core.Unsupportedf("svg: the root element is not svg")
	}
	if err := validChars(data); err != nil {
		return nil, nil, err
	}
	c := newCleaner(ctx, data, opts, true)
	if err := c.run(); err != nil {
		return nil, nil, err
	}
	// data- attributes stay only if something left in the file may read them. Judging
	// that from the output, not the input, is what makes a second clean agree with the
	// first. A file with nothing to decide takes only one pass.
	if c.warned[warnDataAttributes] && !readsDataAttributes(c.out) {
		c = newCleaner(ctx, data, opts, false)
		if err := c.run(); err != nil {
			return nil, nil, err
		}
	}
	return c.out, c.rep, nil
}

// readsDataAttributes reports whether a file holds anything that could read a data-
// attribute: a script, in an element or a handler, or a CSS attribute selector. A false
// alarm only leaves the attributes in, with a warning.
func readsDataAttributes(out []byte) bool {
	return bytes.Contains(out, []byte("script")) || bytes.Contains(out, []byte("[data-")) || hasEventHandler(out)
}

// hasEventHandler looks for what starts an event attribute: whitespace, then "on".
func hasEventHandler(out []byte) bool {
	for i := 0; i+3 < len(out); i++ {
		if isSpace(out[i]) && out[i+1] == 'o' && out[i+2] == 'n' && out[i+3] >= 'a' && out[i+3] <= 'z' {
			end := i + 4
			for end < len(out) && out[end] >= 'a' && out[end] <= 'z' {
				end++
			}
			if end < len(out) && (out[end] == '=' || isSpace(out[end])) {
				return true
			}
		}
	}
	return false
}

type sniffResult uint8

const (
	sniffOther sniffResult = iota
	sniffUTF8
	sniffUTF16
)

// sniff decides from the start of a file whether its root element is svg. A
// UTF-16 file counts, so that it gets a clear refusal instead of "unknown format".
func sniff(data []byte) sniffResult {
	if decoded, ok := decodeUTF16(data); ok {
		if rootIsSVG(decoded) {
			return sniffUTF16
		}
		return sniffOther
	}
	if rootIsSVG(data) {
		return sniffUTF8
	}
	return sniffOther
}

// decodeUTF16 converts data if it begins with a UTF-16 byte order mark or with a
// '<' in UTF-16. It tolerates truncation, because it is only used to look at the
// start of the file.
func decodeUTF16(data []byte) ([]byte, bool) {
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		order, data = binary.LittleEndian, data[2:]
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		order, data = binary.BigEndian, data[2:]
	case bytes.HasPrefix(data, []byte{'<', 0}):
		order = binary.LittleEndian
	case bytes.HasPrefix(data, []byte{0, '<'}):
		order = binary.BigEndian
	default:
		return nil, false
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = order.Uint16(data[2*i:])
	}
	return []byte(string(utf16.Decode(units))), true
}

// rootIsSVG walks past the prolog to the root element and checks its local name.
// It uses the same scanner as cleaning, but stops at the root's name, because a
// long root tag may not fit in the data Match was given.
func rootIsSVG(data []byte) bool {
	s := scanner{data: bytes.TrimPrefix(data, utf8BOM)}
	for {
		s.pos = s.skipSpace(s.pos)
		rest := s.data[min(s.pos, len(s.data)):]
		var err error
		switch {
		case len(rest) == 0 || rest[0] != '<':
			return false
		case bytes.HasPrefix(rest, []byte("<!--")):
			_, err = s.comment()
		case bytes.HasPrefix(rest, []byte("<?")):
			_, err = s.instruction()
		case bytes.HasPrefix(rest, []byte("<!DOCTYPE")):
			_, err = s.doctype()
		case bytes.HasPrefix(rest, []byte("<!")), bytes.HasPrefix(rest, []byte("</")):
			return false
		default:
			nameEnd, err := s.scanName(s.pos + 1)
			if err != nil {
				return false
			}
			_, local := splitName(string(s.data[s.pos+1 : nameEnd]))
			return local == "svg"
		}
		if err != nil {
			return false
		}
	}
}
