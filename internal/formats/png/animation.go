package png

import (
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	animationControlSize = 8
	frameControlSize     = 26
	sequenceSize         = 4
)

// animationControl handles acTL. An animation is only one if this comes before the
// image data and promises at least one frame. Otherwise decoders show a still image,
// and this chunk and any frame chunks are skipped by all of them, which would make
// fdAT a place for any amount of plain text to survive cleaning.
func (c *cleaner) animationControl(ch chunk) {
	switch {
	case c.seen[ch.typ]:
		c.dropChunk(ch, "repeated")
	case c.sawIDAT:
		c.dropChunk(ch, "after the image data")
	case len(ch.data) != animationControlSize || binary.BigEndian.Uint32(ch.data) == 0:
		c.dropChunk(ch, "not valid here")
	default:
		c.seen[ch.typ] = true
		c.animated = true
		c.animationPart = len(c.parts)
		c.declaredFrames = binary.BigEndian.Uint32(ch.data)
		c.parts = append(c.parts, ch.raw)
	}
}

// frameControl handles fcTL. In a real animation a bad frame is damage: decoders
// disagree on how far they get, so there is no one output to vouch for.
func (c *cleaner) frameControl(ch chunk) error {
	if !c.animated {
		c.dropChunk(ch, "not in an animation")
		return nil
	}
	if len(ch.data) != frameControlSize {
		return core.Malformedf("png: fcTL chunk is %d bytes, not %d", len(ch.data), frameControlSize)
	}
	if err := c.checkSequence(ch.data); err != nil {
		return err
	}
	width, height := binary.BigEndian.Uint32(ch.data[4:]), binary.BigEndian.Uint32(ch.data[8:])
	x, y := binary.BigEndian.Uint32(ch.data[12:]), binary.BigEndian.Uint32(ch.data[16:])
	dispose, blend := ch.data[24], ch.data[25]
	if width == 0 || height == 0 ||
		uint64(x)+uint64(width) > uint64(c.header.width) || uint64(y)+uint64(height) > uint64(c.header.height) ||
		dispose > 2 || blend > 1 {
		return core.Malformedf("png: fcTL chunk describes an impossible frame")
	}
	c.frames++
	if uint64(c.frames) > uint64(c.declaredFrames) {
		return core.Malformedf("png: more frames than the %d acTL declares", c.declaredFrames)
	}
	if err := c.finishFrame(); err != nil {
		return err
	}
	if c.sawIDAT {
		c.frame = &imageStream{width: width, height: height, where: fmt.Sprintf("frame %d", c.frames)}
		c.streams = append(c.streams, c.frame)
	}
	c.parts = append(c.parts, ch.raw)
	return nil
}

// frameData handles fdAT, which holds a frame's image data after its number.
func (c *cleaner) frameData(ch chunk) error {
	if !c.animated {
		c.dropChunk(ch, "not in an animation")
		return nil
	}
	if len(ch.data) < sequenceSize {
		return core.Malformedf("png: fdAT chunk of %d bytes has no sequence number", len(ch.data))
	}
	if c.frame == nil {
		return core.Malformedf("png: fdAT chunk with no frame to belong to")
	}
	if err := c.checkSequence(ch.data); err != nil {
		return err
	}
	c.frame.pieces = append(c.frame.pieces, streamPiece{
		part: len(c.parts),
		typ:  ch.typ,
		head: ch.data[:sequenceSize],
		data: ch.data[sequenceSize:],
	})
	c.parts = append(c.parts, ch.raw)
	return nil
}

func (c *cleaner) checkSequence(p []byte) error {
	if seq := binary.BigEndian.Uint32(p); seq != c.nextSequence {
		return core.Malformedf("png: frame chunk numbered %d where %d belongs", seq, c.nextSequence)
	}
	c.nextSequence++
	return nil
}

// dropEmptyAnimation removes an acTL no frame followed: decoders show a still image.
func (c *cleaner) dropEmptyAnimation() {
	if c.animated && c.frames == 0 {
		c.parts[c.animationPart] = nil
		c.remove(core.CatHidden, core.RiskMed, "Other chunk", "acTL with no frames after it", "acTL")
	}
}

// finishFrame checks that the frame being read has image data. A run of fcTL with
// none would otherwise be free room inside a chunk this handler keeps.
func (c *cleaner) finishFrame() error {
	if c.frame != nil && len(c.frame.pieces) == 0 {
		return core.Malformedf("png: %s has no image data", c.frame.where)
	}
	return nil
}
