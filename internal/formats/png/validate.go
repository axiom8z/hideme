package png

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// PNG colour types.
const (
	colorGray      = 0
	colorRGB       = 2
	colorPalette   = 3
	colorGrayAlpha = 4
	colorRGBA      = 6
)

type imageHeader struct {
	width, height uint32
	depth         byte
	colorType     byte
	interlaced    bool
}

// parseHeader checks IHDR by the same rules libpng applies. A header that fails
// them describes no image a decoder would show, so nothing about it can be trusted.
func parseHeader(p []byte) (imageHeader, error) {
	const size = 13
	if len(p) != size {
		return imageHeader{}, core.Malformedf("png: IHDR is %d bytes, not %d", len(p), size)
	}
	width, height := binary.BigEndian.Uint32(p), binary.BigEndian.Uint32(p[4:])
	if width == 0 || height == 0 || width > maxChunkLength || height > maxChunkLength {
		return imageHeader{}, core.Malformedf("png: invalid image size %dx%d", width, height)
	}
	depth, colorType := p[8], p[9]
	if !validDepth(colorType, depth) {
		return imageHeader{}, core.Malformedf("png: colour type %d does not allow bit depth %d", colorType, depth)
	}
	if p[10] != 0 || p[11] != 0 || p[12] > 1 {
		return imageHeader{}, core.Malformedf("png: unknown compression, filter or interlace method")
	}
	return imageHeader{width: width, height: height, depth: depth, colorType: colorType, interlaced: p[12] == 1}, nil
}

func validDepth(colorType, depth byte) bool {
	switch colorType {
	case colorGray:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8 || depth == 16
	case colorPalette:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8
	case colorRGB, colorGrayAlpha, colorRGBA:
		return depth == 8 || depth == 16
	}
	return false
}

// fixedSizes lists the allowlisted ancillary chunks whose length never varies.
var fixedSizes = map[string]int{
	"gAMA": 4,
	"cHRM": 32,
	"sRGB": 1,
	"pHYs": 9,
	"cICP": 4,
	"mDCV": 24,
	"cLLI": 8,
}

// ancillary keeps an optional rendering chunk if it is one a decoder would use.
// libpng skips a chunk that has the wrong size, comes too late or repeats one it has
// already read, and the picture displays the same without it, so such a file is not
// refused: the chunk goes and takes with it whatever it was holding. Without these
// checks, a repeated gAMA or an oversized pHYs would be a place for arbitrary data to
// survive cleaning.
func (c *cleaner) ancillary(ch chunk) {
	switch {
	case c.seen[ch.typ]:
		c.dropChunk(ch, "repeated")
	case c.sawIDAT:
		c.dropChunk(ch, "after the image data")
	case !c.validAncillary(ch):
		c.dropChunk(ch, "not valid here")
	default:
		c.seen[ch.typ] = true
		c.parts = append(c.parts, ch.raw)
	}
}

func (c *cleaner) dropChunk(ch chunk, why string) {
	c.remove(core.CatHidden, core.RiskMed, "Other chunk", fmt.Sprintf("%s, %s, %s", ch.typ, core.ByteCount(len(ch.data)), why), ch.typ)
}

// validAncillary checks a chunk against its definition.
func (c *cleaner) validAncillary(ch chunk) bool {
	if want, fixed := fixedSizes[ch.typ]; fixed && len(ch.data) != want {
		return false
	}
	switch ch.typ {
	case "cHRM":
		return validChromaticities(ch.data)
	case "sBIT":
		return len(ch.data) == significantBitsSize(c.header.colorType)
	case "bKGD":
		return len(ch.data) == backgroundSize(c.header.colorType)
	}
	return true
}

// validChromaticities checks that each of the eight coordinates is at most 1.0,
// stored times 100000. libpng skips the chunk otherwise, and text cannot pass this.
func validChromaticities(p []byte) bool {
	for ; len(p) >= 4; p = p[4:] {
		if binary.BigEndian.Uint32(p) > 100000 {
			return false
		}
	}
	return true
}

// plte handles the palette. On a true-colour image it is only a suggestion for old
// displays, with no effect on the picture, so it goes. On a palette image it is
// required, and an image can only reach as many entries as its bit depth allows, so
// decoders ignore any others and they go too.
func (c *cleaner) plte(ch chunk) error {
	if c.sawPLTE {
		return core.Malformedf("png: more than one PLTE chunk")
	}
	switch c.header.colorType {
	case colorRGB, colorRGBA:
		c.sawPLTE = true
		c.remove(core.CatHidden, core.RiskLow, "Suggested palette", core.ByteCount(len(ch.data)), "PLTE")
		return nil
	case colorGray, colorGrayAlpha:
		return core.Malformedf("png: a gray image has a PLTE chunk")
	}
	if c.sawIDAT {
		return core.Malformedf("png: PLTE chunk after the image data")
	}
	if len(ch.data) == 0 || len(ch.data)%3 != 0 || len(ch.data) > 3*256 {
		return core.Malformedf("png: PLTE chunk of %d bytes", len(ch.data))
	}
	c.sawPLTE = true
	reachable := 3 << c.header.depth
	if len(ch.data) <= reachable {
		c.palette = len(ch.data) / 3
		c.parts = append(c.parts, ch.raw)
		return nil
	}
	c.remove(core.CatHidden, core.RiskMed, "Unreachable palette entries", core.ByteCount(len(ch.data)-reachable), "PLTE")
	c.palette = reachable / 3
	c.parts = append(c.parts, buildChunk("PLTE", ch.data[:reachable]))
	return nil
}

// transparency handles tRNS, the one optional chunk that changes the pixels, which
// is why a doubtful one is refused rather than dropped. libpng skips a tRNS that has
// the wrong size, comes too late or repeats, while Pillow applies it, so either
// choice here would change the picture for one of them. Only an image with its own
// alpha channel is simple: everyone ignores a tRNS on it.
func (c *cleaner) transparency(ch chunk) error {
	switch {
	case c.header.colorType == colorGrayAlpha || c.header.colorType == colorRGBA:
		c.dropChunk(ch, "not valid here")
		return nil
	case c.transparencyData != nil:
		if !bytes.Equal(c.transparencyData, ch.data) {
			return core.Malformedf("png: tRNS chunks disagree")
		}
		c.dropChunk(ch, "repeated")
		return nil
	case c.sawIDAT || !c.validTransparency(len(ch.data)):
		return core.Malformedf("png: tRNS chunk of %d bytes is not valid here", len(ch.data))
	}
	c.transparencyData = ch.data
	c.parts = append(c.parts, ch.raw)
	return nil
}

func (c *cleaner) validTransparency(size int) bool {
	switch c.header.colorType {
	case colorGray:
		return size == 2
	case colorRGB:
		return size == 6
	case colorPalette:
		return c.sawPLTE && size > 0 && size <= c.palette
	}
	return false
}

func significantBitsSize(colorType byte) int {
	switch colorType {
	case colorGray:
		return 1
	case colorGrayAlpha:
		return 2
	case colorRGB, colorPalette:
		return 3
	}
	return 4
}

func backgroundSize(colorType byte) int {
	switch colorType {
	case colorGray, colorGrayAlpha:
		return 2
	case colorRGB, colorRGBA:
		return 6
	}
	return 1
}
