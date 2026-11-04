package jpeg

import (
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

// What a decoding table is for. A later table of the same kind, class and id replaces
// an earlier one.
const (
	tableQuant = iota
	tableHuffman
	tableArithmetic
	tableRestart
	tableKinds
)

// maxTableDefs bounds the records a hostile file can demand, at as little as 17 bytes
// of file each. A real JPEG has a few dozen tables even when progressive.
const maxTableDefs = 1 << 16

// tableDef is one table in a kept segment.
type tableDef struct {
	part       int32 // index into cleaner.parts
	start, end int32 // the table's range within that segment
	kind       uint8
	used       bool
}

// tableSet tracks which table definitions a decoder could ever read. The tables
// themselves stay as they are, but one nothing reads is free room for up to 64 KiB
// of anything, in as many segments as a file likes, with nothing to show for it on
// screen. It is removed like any other data a decoder does not need.
type tableSet struct {
	defs    []tableDef
	current [tableKinds][2][4]int32 // index into defs plus one; zero means none
	unused  int                     // bytes found to be unused outside defs
}

// keepTables records a segment of tables, each of which must be exactly the shape
// its marker defines, since spare room in a kept segment would be a place for data
// to survive cleaning. libjpeg rejects the same segments, so no file that displays
// is lost to this.
func (c *cleaner) keepTables(kind int, marker byte, raw, payload []byte, entry jpegmeta.TableEntry) error {
	const payloadOffset = 4 // the marker and the length
	invalid := core.Malformedf("jpeg: segment with marker %#02x has an invalid structure", marker)
	if len(payload) == 0 {
		return invalid
	}
	part := int32(len(c.parts))
	for off := 0; off < len(payload); {
		class, id, size, ok := entry(payload[off:])
		if !ok {
			return invalid
		}
		if len(c.tables.defs) == maxTableDefs {
			return fmt.Errorf("%w: jpeg: more than %d decoding tables", core.ErrTooLarge, maxTableDefs)
		}
		c.tables.defs = append(c.tables.defs, tableDef{
			part:  part,
			start: int32(payloadOffset + off),
			end:   int32(payloadOffset + off + size),
			kind:  uint8(kind),
		})
		c.tables.current[kind][class][id] = int32(len(c.tables.defs))
		off += size
	}
	c.parts = append(c.parts, raw)
	return nil
}

func (s *tableSet) markUsed(kind int, class, id byte) {
	if i := s.current[kind][class][id]; i > 0 {
		s.defs[i-1].used = true
	}
}

// markQuant marks the tables in effect for the frame's components. Decoders disagree
// on when they read these: libjpeg at each component's first scan, and image/jpeg for
// a progressive file at the very end. So a table in effect at any of those moments
// counts as used.
func (c *cleaner) markQuant() {
	for _, id := range c.frameQuant {
		c.tables.markUsed(tableQuant, 0, id)
	}
}

// markScanTables marks what a scan's decoder will read. A scan may not need both of
// a component's tables, which only means that a few spare tables stay.
func (c *cleaner) markScanTables(payload []byte) error {
	for i := range int(payload[0]) {
		selectors := payload[2+2*i]
		dc, ac := selectors>>4, selectors&0x0F
		if dc > 3 || ac > 3 {
			return core.Malformedf("jpeg: a scan selects table %d and %d, which cannot exist", dc, ac)
		}
		c.tables.markUsed(tableHuffman, 0, dc)
		c.tables.markUsed(tableHuffman, 1, ac)
		c.tables.markUsed(tableArithmetic, 0, dc)
		c.tables.markUsed(tableArithmetic, 1, ac)
	}
	c.markQuant()
	c.tables.markUsed(tableRestart, 0, 0)
	return nil
}

// dropUnusedTables rewrites each segment holding a table nothing reads. A file with
// none keeps its segments byte for byte.
func (c *cleaner) dropUnusedTables() {
	c.markQuant()
	arithmetic := isArithmetic(c.frameMarker)
	dead := func(d tableDef) bool {
		return !d.used || (d.kind == tableHuffman && arithmetic) || (d.kind == tableArithmetic && !arithmetic)
	}
	removed := c.tables.unused
	defs := c.tables.defs
	for i := 0; i < len(defs); {
		j := i
		var lost int
		for j < len(defs) && defs[j].part == defs[i].part {
			if dead(defs[j]) {
				lost += int(defs[j].end - defs[j].start)
			}
			j++
		}
		if lost > 0 {
			removed += lost
			c.rewriteTables(defs[i:j], dead)
		}
		i = j
	}
	if removed > 0 {
		c.add(core.CatHidden, core.RiskLow, "Unused decoding tables", core.ByteCount(removed), "tables")
	}
}

func (c *cleaner) rewriteTables(defs []tableDef, dead func(tableDef) bool) {
	part := defs[0].part
	raw := c.parts[part]
	var kept []byte
	for _, d := range defs {
		if !dead(d) {
			kept = append(kept, raw[d.start:d.end]...)
		}
	}
	if len(kept) == 0 {
		c.parts[part] = nil
		return
	}
	seg := binary.BigEndian.AppendUint16([]byte{0xFF, raw[1]}, uint16(len(kept)+2))
	c.parts[part] = append(seg, kept...)
}

func isArithmetic(frameMarker byte) bool {
	return frameMarker >= 0xC9
}
