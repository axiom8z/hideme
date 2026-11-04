// Package pdf removes metadata from PDF documents.
//
// A PDF is an object graph with a table saying where each object is, and it hides
// data in three ways a patch-in-place tool cannot see: earlier revisions kept by
// incremental updates, objects nothing refers to any more, and metadata scattered
// through the graph. So this handler parses the document, walks the graph from its
// catalog, and writes out only what it reached and kept, in a fresh layout. What
// was not walked cannot survive, whatever it was.
//
// A PDF's dictionaries are open-ended and a renderer may need any key, so much of
// the graph cannot be allowlisted. The trailer, catalog, pages, resources, XObjects,
// fonts and actions have keys the specification enumerates, and those are
// allowlisted; everywhere else the removal rules name what to take out, and a key
// nobody has heard of gets a warning. See sanitize.go.
package pdf

import (
	"bytes"
	"context"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

const formatID = "pdf"

var pdfHeader = []byte("%PDF-")

// limits are the caps that make hostile files harmless. They live in a struct so
// that tests can trip them without producing gigabytes.
type limits struct {
	maxDecodedStream int64 // one stream that must be held in memory
	maxDecodedTotal  int64 // all such streams together
	maxScanned       int64 // decompressed only to look through it, so not held
	maxValues        int   // parsed values, each of which costs memory
}

var defaultLimits = limits{
	maxDecodedStream: 256 << 20,
	maxDecodedTotal:  1 << 30,
	maxScanned:       8 << 30,
	maxValues:        1 << 26,
}

func init() {
	core.Register(handler{limits: defaultLimits})
}

type handler struct {
	limits limits
}

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "PDF document",
		Family:     core.FamilyDocument,
		Extensions: []string{".pdf"},
		Limitations: []string{
			"Anything visible on a page stays, including comment text, form field values and the appearance of a removed signature.",
			"Digital signatures are removed, because any rewrite would invalidate them.",
			"JPEG 2000, JBIG2 and CCITT images, JPEGs inline in a content stream, and streams in a filter HideMe does not decode keep anything hidden inside them, with a warning.",
			"Page content is kept as it stands, so text placed out of view stays, like any other content. Comments in it get a warning.",
			"JavaScript and automatic actions are kept, with a warning, because they are behaviour rather than metadata.",
			"ICC colour profiles stay even in strict mode, because the page's colour spaces need them to render.",
			"Keys a producer invented are removed from the catalog, pages, resources, images, form XObjects, fonts and actions. Elsewhere they stay, with a warning, and data put under a key the specification defines for something else cannot be told apart from what belongs there.",
			"Embedded fonts keep their own name tables.",
			"A compressed stream is copied as it stands once it is read, so its final checksum, 4 bytes, and the padding bits before it stay.",
		},
	}
}

func (handler) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, pdfHeader)
}

// Inspect shares all of Clean except the writing, so the two cannot disagree. The
// cost is that Inspect does the work of a clean, including cleaning embedded
// images in memory, which is fine for a format that is loaded whole anyway.
func (h handler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	return h.process(ctx, src, nil, opts)
}

func (h handler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	return h.process(ctx, src, dst, opts)
}

// process does the work of Inspect and Clean. A nil dst skips writing.
func (h handler) process(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, err
	}
	doc, err := load(ctx, data, h.limits)
	if err != nil {
		return nil, err
	}
	rep := &core.Report{Format: formatID}
	objects, err := sanitize(ctx, doc, opts, rep)
	if err != nil {
		return nil, err
	}
	if dst == nil {
		return rep, nil
	}
	if err := writeDocument(ctx, dst, headerVersion(data), objects); err != nil {
		return nil, err
	}
	return rep, nil
}
