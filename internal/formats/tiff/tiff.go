// Package tiff cleans classic TIFF images by rewriting them from scratch.
//
// Most containers have to be patched in place because their contents hold
// absolute offsets. In a TIFF only the directories hold offsets, and those are
// rewritten anyway, so this handler can do better than patching: it walks every
// page, keeps an allowlist of rendering tags, copies only the bytes that strips
// and tiles reference, and lays the output out afresh. Whatever sat between the
// strips, including stale directories left behind by earlier edits, simply never
// reaches the output.
//
// Files are never loaded whole. Directories are read on demand and pixel data is
// copied as a stream, so memory follows the amount of structure in a file rather
// than its size.
package tiff

import (
	"context"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

const formatID = "tiff"

func init() {
	core.Register(handler{})
}

type handler struct{}

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "TIFF image",
		Family:     core.FamilyImage,
		Extensions: []string{".tif", ".tiff"},
		Limitations: []string{
			"Data hidden inside compressed strips (LZW, Deflate, PackBits and similar), including after the end of a compressed stream, cannot be seen without decoding the image and is copied as it is.",
			"An ICC colour profile is kept unless --strict is used, and it can name its creator and creation date.",
			"Values of the tags a decoder uses (such as resolution, white point and colour settings) are copied as they are, so a few bytes of hidden data could survive in them. So are the tables in JPEG strips and JPEGTables, which are not checked for use: entries or whole tables no picture reads can hold kilobytes.",
			"Georeferencing tags count as location data, so a GeoTIFF loses its map position.",
			"Reduced-resolution pages count as thumbnails and are removed, so a pyramid TIFF keeps only its full-size pages.",
			"BigTIFF, old-style JPEG compression, camera raw files that look like TIFF (DNG, CR2, NEF, ARW, ORF, RW2, IIQ and similar) and ImageJ or MetaMorph stacks that store frames outside the TIFF structure are refused, not cleaned.",
		},
	}
}

// Match claims BigTIFF and the raw formats' private magic numbers as well as
// classic TIFF. The handler refuses those variants, but claiming them means the
// user hears why, rather than a generic message that nothing recognised the file.
func (handler) Match(head []byte, _ core.Source) bool {
	if len(head) < 4 {
		return false
	}
	switch string(head[:4]) {
	case magicLittle, magicBig, magicBigTIFFLittle, magicBigTIFFBig, magicORF, magicORFBig, magicORFAlternate, magicRW2:
		return true
	}
	return false
}

func (handler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	doc, err := parse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	return doc.report, nil
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	doc, err := parse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	if err := doc.write(ctx, dst); err != nil {
		return nil, err
	}
	return doc.report, nil
}
