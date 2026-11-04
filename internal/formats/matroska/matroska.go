// Package matroska removes metadata from Matroska and WebM files.
//
// These files can run to many gigabytes and hold absolute offsets into themselves
// (SeekHead and Cues), so nothing is moved and nothing is loaded. Removed elements
// become EBML Void elements of exactly the same length, with zeroed payloads, and
// core.CopyPatched streams everything else unchanged. Every child of the metadata
// masters is held to an allowlist, so anything not known to be needed goes.
package matroska

import (
	"bytes"
	"context"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	docMatroska = "matroska"
	docWebM     = "webm"
)

// ebmlMagic is the EBML header ID that starts every EBML document.
var ebmlMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}

func init() { core.Register(handler{}) }

type handler struct{}

func (handler) Format() core.Format {
	return core.Format{
		ID:         docMatroska,
		Name:       "Matroska or WebM video",
		Family:     core.FamilyVideo,
		Extensions: []string{".mkv", ".mka", ".mks", ".mk3d", ".webm"},
		Limitations: []string{
			"Everything in a cluster but the block payloads is read and held to an allowlist. Payloads are copied unread, so anything in the encoded streams stays. Block additions stay too, with a warning.",
			"Subtitle text stays, with a warning, and is not inspected. Tracks of any type other than video, audio or subtitle are refused rather than kept.",
			"Comment headers in the codec private data of Vorbis, Theora and FLAC tracks are blanked, and so are the author fields, comments and project sections of ASS and SSA subtitle headers, though their style lines, with their font names, stay. Other codecs' private data stays because decoders need it, with a warning if it visibly holds text. Encoded data, or a string under 12 characters, cannot be told apart from what decoders need, and goes unnoticed.",
			"Attachments are removed, including fonts that styled subtitles may need to display as intended.",
			"A codec ID HideMe does not recognise stays, with a warning, because decoders need it. Some read only its start, so a suffix on one would stay too.",
			"Fixed-size values players read, such as a file's duration and a track's sample rate and frame duration, stay as written, even if a player ignores them: 8 bytes each. A checksum that does not match what it covers is removed.",
			"Track UIDs stay, because every track must have one. They are random numbers that name nobody, but they can show that two copies of a file are the same mux.",
		},
	}
}

// Match requires a Matroska or WebM document type, because other formats use EBML
// too. It reads through src, not head, because the EBML header can legally be
// padded past the end of head.
func (handler) Match(head []byte, src core.Source) bool {
	if !bytes.HasPrefix(head, ebmlMagic) {
		return false
	}
	_, doc, err := readDocHeader(&reader{ctx: context.Background(), src: src})
	return err == nil && (doc.docType == docMatroska || doc.docType == docWebM)
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	p, err := analyse(ctx, src)
	if err != nil {
		return nil, err
	}
	return p.rep, nil
}

// Clean takes no notice of Strict, because nothing here is retained by default.
func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	p, err := analyse(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := core.CopyPatched(ctx, dst, src, p.patches); err != nil {
		return nil, err
	}
	return p.rep, nil
}

// docHeader holds the fields of the EBML header that decide whether this parser
// can read the document. Absent fields take the defaults the specification gives.
type docHeader struct {
	docType       string
	ebmlRead      uint64
	maxIDLength   uint64
	maxSizeLength uint64
	docTypeRead   uint64
}

func (d docHeader) checkVersions() error {
	switch {
	case d.ebmlRead > maxEBMLReadVersion:
		return core.Unsupportedf("matroska: EBMLReadVersion %d is newer than this parser", d.ebmlRead)
	case d.docTypeRead > maxDocTypeReadVersion:
		return core.Unsupportedf("matroska: DocTypeReadVersion %d is newer than this parser", d.docTypeRead)
	case d.maxIDLength > maxIDWidth:
		return core.Unsupportedf("matroska: EBMLMaxIDLength %d is over the Matroska limit", d.maxIDLength)
	case d.maxSizeLength > maxSizeWidth:
		return core.Unsupportedf("matroska: EBMLMaxSizeLength %d is over the Matroska limit", d.maxSizeLength)
	}
	return nil
}

// readDocHeader parses the EBML header at the start of the file.
func readDocHeader(r *reader) (element, docHeader, error) {
	doc := docHeader{ebmlRead: 1, maxIDLength: maxIDWidth, maxSizeLength: maxSizeWidth, docTypeRead: 1}
	size := r.src.Size()
	if size == 0 {
		return element{}, doc, core.Malformedf("matroska: the file is empty")
	}
	header, err := r.header(0, size)
	if err != nil {
		return element{}, doc, err
	}
	if header.id != idEBML {
		return element{}, doc, core.Unsupportedf("matroska: the file does not start with an EBML header")
	}
	if header.unknown {
		return element{}, doc, core.Malformedf("matroska: the EBML header has an unknown size")
	}
	seenDocType := false
	for e, err := range r.children(header) {
		if err != nil {
			return element{}, doc, err
		}
		switch e.id {
		case idDocType:
			if seenDocType {
				return element{}, doc, core.Malformedf("matroska: the EBML header has two document types")
			}
			if e.size > maxShortStringSize {
				return element{}, doc, core.Malformedf("matroska: the document type is %d bytes long", e.size)
			}
			seenDocType = true
			doc.docType = string(bytes.TrimRight(r.peek(e, maxShortStringSize), "\x00"))
		case idEBMLReadVersion:
			doc.ebmlRead, err = r.readUint(e)
		case idDocTypeReadVersion:
			doc.docTypeRead, err = r.readUint(e)
		case idEBMLMaxIDLength:
			doc.maxIDLength, err = r.readUint(e)
		case idEBMLMaxSizeLength:
			doc.maxSizeLength, err = r.readUint(e)
		}
		if err != nil {
			return element{}, doc, err
		}
	}
	return header, doc, nil
}
