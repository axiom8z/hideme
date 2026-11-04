// Package office cleans ZIP-based office documents: Office Open XML (Word, Excel
// and PowerPoint) and OpenDocument (LibreOffice and friends).
//
// Both are ZIP archives of XML parts, so the work happens at three levels:
//
//   - The archive is always rewritten from scratch, which disposes of timestamps,
//     extra fields, comments and anything hidden around the entries.
//   - XML parts are edited at the byte level by a tag scanner. encoding/xml cannot
//     round-trip them, because it renames namespaces, and a text search cannot tell
//     a real attribute from body text that merely spells one.
//   - Embedded pictures and documents go back through core.CleanBytes.
//
// A complete allowlist of parts is not possible here: both formats allow any
// number of vendor extension parts that documents really need, and dropping one
// breaks the file. So parts are removed by a list of known metadata, and whatever
// looks like metadata but is not on the list earns a warning. Inside the archive
// layer, where the structure is fully known, nothing is carried over at all.
package office

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

func init() {
	core.Register(ooxmlHandler{limits: defaultLimits})
	core.Register(odfHandler{limits: defaultLimits})
}

const (
	contentTypesKey = "[content_types].xml"
	mimetypeName    = "mimetype"
	odfMimePrefix   = "application/vnd.oasis.opendocument."
)

// packageKind is what the central directory says a ZIP file is.
type packageKind int

const (
	packageNone packageKind = iota
	packageOOXML
	packageODF
)

// detectPackage looks only at the central directory and the short mimetype
// entry, so that a plain ZIP or a multi-gigabyte non-ZIP costs almost nothing.
//
// A file carrying both markers belongs to neither handler. Each handler reads
// only its own format's metadata, so accepting such a polyglot would leave the
// other format's metadata behind while the report said the file was clean.
func detectPackage(src core.Source) packageKind {
	zr, err := zip.NewReader(src, src.Size())
	if err != nil {
		return packageNone
	}
	var hasTypes, hasODFMime bool
	for _, f := range zr.File {
		switch strings.ToLower(f.Name) {
		case contentTypesKey:
			hasTypes = true
		case mimetypeName:
			hasODFMime = hasODFMime || strings.HasPrefix(readMimetype(f), odfMimePrefix)
		}
	}
	switch {
	case hasTypes && !hasODFMime:
		return packageOOXML
	case hasODFMime && !hasTypes:
		return packageODF
	}
	return packageNone
}

// otherFormats are signatures of files that keep working with a ZIP archive glued
// to their end, because each reader looks at its own end of the file. Such a file
// belongs to the handler for what it shows the user. Claiming it here would rewrite
// it as a bare document and throw the picture away as "data before the archive".
var otherFormats = [][]byte{
	[]byte("%PDF-"),
	[]byte("\xFF\xD8"),
	[]byte("\x89PNG"),
	[]byte("GIF8"),
	[]byte("RIFF"),
	[]byte("FORM"),
	[]byte("OggS"),
	[]byte("fLaC"),
	[]byte("ID3"),
	[]byte("II*\x00"),
	[]byte("MM\x00*"),
	[]byte("\x1A\x45\xDF\xA3"), // Matroska and WebM
	[]byte("<"),                // SVG and other text markup
}

func startsAsOtherFormat(head []byte) bool {
	for _, signature := range otherFormats {
		if bytes.HasPrefix(head, signature) {
			return true
		}
	}
	switch {
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0: // MPEG audio
		return true
	case len(head) >= 8 && string(head[4:8]) == "ftyp": // ISO media
		return true
	}
	return false
}

// readMimetype returns the start of a mimetype entry, or nothing if it cannot be
// read. The limit is generous, since the longest real value is about 60 bytes.
func readMimetype(f *zip.File) string {
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 256))
	if err != nil {
		return ""
	}
	return string(b)
}
