package exif

import (
	"bytes"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	minimalSize = 26
	// minimalValueOffset locates the low byte of the orientation value in the
	// big-endian layout Minimal writes.
	minimalValueOffset = 19
)

// Minimal returns the smallest valid TIFF block holding only an orientation tag,
// without any JPEG prefix, or nil if orientation is not a valid EXIF value (1
// through 8). The byte order is fixed so that the same orientation always gives
// the same bytes, whatever the order of the block it came from.
func Minimal(orientation uint16) []byte {
	if orientation < 1 || orientation > 8 {
		return nil
	}
	return []byte{
		'M', 'M', 0, tiffMagic, 0, 0, 0, headerSize, // big-endian header; IFD0 follows it
		0, 1, // one entry
		tagOrientation >> 8, tagOrientation & 0xFF, 0, typeShort, 0, 0, 0, 1, // tag, type, count
		0, byte(orientation), 0, 0, // SHORT values are left-justified in the field
		0, 0, 0, 0, // no next IFD
	}
}

// IsMinimal reports whether data, with or without Prefix, is byte for byte what
// Minimal produces. Anything else, even a block holding nothing but an
// orientation tag in the other byte order, may carry extra bytes in places no
// tag refers to.
func IsMinimal(data []byte) bool {
	data = StripPrefix(data)
	if len(data) != minimalSize {
		return false
	}
	return bytes.Equal(data, Minimal(uint16(data[minimalValueOffset])))
}

// Orientation returns the image orientation from IFD0, from 1 through 8, or 0 if
// the tag is absent or its value is not a valid orientation.
//
// Damage elsewhere in the block does not count against it. Viewers read the tag all
// the same, so dropping it would show the picture sideways, a bad MakerNote offset
// being a common kind of damage, and a number from 1 to 8 cannot carry anything out
// of the block.
func (d *Data) Orientation() uint16 {
	if len(d.dirs) == 0 {
		return 0
	}
	e, ok := d.dirs[0].find(tagOrientation) // Parse always records IFD0 first
	if !ok {
		return 0
	}
	if v, ok := e.number(d.order); ok && v >= 1 && v <= 8 {
		return uint16(v)
	}
	return 0
}

// Replacement returns the block a handler must write in place of the original,
// without any JPEG prefix, or nil if the block must simply be dropped. It keeps
// the rule that decides what is retained in one place, so that Findings and a
// handler's Clean cannot come to different answers.
//
// An orientation of 1 is not kept: it is the default and means no rotation, so
// dropping it changes nothing on screen.
func (d *Data) Replacement(opts core.Options) []byte {
	if o := d.Orientation(); !opts.Strict && o > 1 {
		return Minimal(o)
	}
	return nil
}
