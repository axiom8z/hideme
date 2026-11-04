// Package jpeg removes metadata from JPEG images by rewriting the marker stream.
//
// The stream is walked with an allowlist: only the segments a decoder needs are
// kept, and the entropy-coded data is copied byte for byte up to where its last block
// ends, so the decoded pixels cannot change. Everything else, including whatever future
// tools invent, is dropped. Inspect and Clean run the same walk, which is what keeps
// them in agreement.
package jpeg

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

const formatID = "jpeg"

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "JPEG image",
		Family:     core.FamilyImage,
		Extensions: []string{".jpg", ".jpeg", ".jpe", ".jfif"},
		Limitations: []string{
			"Data hidden inside the compressed image stream cannot be removed without re-encoding the picture. Bytes after a scan's last block are removed, except in arithmetic or lossless files, and files whose height comes from a DNL marker, whose scans are not walked. Up to 7 padding bits per scan or restart interval stay.",
			"A retained ICC colour profile is kept as it is and can name the device that made it; strict mode removes it.",
			"Values in the tables a decoder uses (quantisation, Huffman, colour transform, JFIF density) are copied as they are. A decoder reads only the entries a picture uses, so hidden data could survive in the rest: up to 2 KiB in Huffman tables, 512 bytes in quantisation tables and 7 bytes in the JFIF header.",
			"Only the first picture of a multi-picture file is kept: further images, motion-photo video and HDR gain maps appended to it are removed, and such a file gets a warning because the photo really changes.",
		},
	}
}

// Match requires SOI followed by the start of another marker, because two bytes
// alone would claim arbitrary binary files.
func (handler) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, signature)
}

func (handler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	c, err := parse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	return c.rep, nil
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	c, err := parse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	for _, part := range c.output() {
		if _, err := dst.Write(part); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
	}
	return c.rep, nil
}

func parse(ctx context.Context, src core.Source, opts core.Options) (*cleaner, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, err
	}
	c := &cleaner{data: data, opts: opts, rep: &core.Report{Format: formatID}}
	if err := c.walk(ctx); err != nil {
		return nil, err
	}
	c.finish()
	return c, nil
}
