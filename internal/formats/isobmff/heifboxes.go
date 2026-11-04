package isobmff

import (
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// maxItems and maxItemRefs bound the tables of a HEIF meta box. The largest real
// photos are grids of a few hundred tiles.
const (
	maxItems    = 1 << 16
	maxItemRefs = 1 << 20
	maxExtents  = 1 << 20
)

// ilocEntry is one item's location. raw is the entry exactly as written, so that
// rewriting the table cannot change an offset of an item that stays.
type ilocEntry struct {
	id      uint32
	method  uint8
	dataRef uint16
	base    uint64
	extents []ilocExtent
	raw     []byte
}

type ilocExtent struct{ offset, length uint64 }

// ilocTable is a parsed iloc box.
type ilocTable struct {
	header  []byte // version, flags and size fields
	version uint8
	entries []ilocEntry
	index   map[uint32]int // item ID -> position in entries
}

// entry returns an item's location, or nil if it has none.
func (t *ilocTable) entry(id uint32) *ilocEntry {
	if i, ok := t.index[id]; ok {
		return &t.entries[i]
	}
	return nil
}

// parseIloc reads an iloc payload, supporting versions 0, 1 and 2 with every
// field size. An unknown version fails closed because its entries cannot be
// located, and an entry that cannot be located might be the one holding the GPS.
func parseIloc(p []byte) (*ilocTable, error) {
	c := cursor{b: p}
	version := c.u8()
	c.take(3)
	if version > 2 {
		return nil, core.Unsupportedf("isobmff: iloc version %d", version)
	}
	sizes := c.u16()
	offsetSize, lengthSize := int(sizes>>12), int(sizes>>8&0xF)
	baseSize, indexSize := int(sizes>>4&0xF), int(sizes&0xF)
	if version == 0 {
		indexSize = 0 // these bits are reserved in version 0
	}
	for _, size := range []int{offsetSize, lengthSize, baseSize, indexSize} {
		if size != 0 && size != 4 && size != 8 {
			return nil, core.Malformedf("iloc field size %d is not 0, 4 or 8", size)
		}
	}
	idSize := 2
	var count int
	if version == 2 {
		idSize = 4
		count = int(c.u32())
	} else {
		count = int(c.u16())
	}
	if c.bad {
		return nil, core.Malformedf("iloc box of %d bytes is too short", len(p))
	}
	minEntry := idSize + 2 + baseSize + 2
	if version >= 1 {
		minEntry += 2
	}
	if count > c.remaining()/minEntry {
		return nil, core.Malformedf("iloc declares %d items but has room for %d", count, c.remaining()/minEntry)
	}
	if count > maxItems {
		return nil, fmt.Errorf("%w: iloc lists %d items", core.ErrTooLarge, count)
	}
	table := &ilocTable{header: p[:c.pos], version: version, entries: make([]ilocEntry, 0, count), index: make(map[uint32]int, count)}
	total := 0
	for range count {
		start := c.pos
		var e ilocEntry
		if version == 2 {
			e.id = c.u32()
		} else {
			e.id = uint32(c.u16())
		}
		if version >= 1 {
			e.method = uint8(c.u16() & 0xF)
		}
		e.dataRef = c.u16()
		e.base = c.sized(baseSize)
		extents := int(c.u16())
		total += extents
		if total > maxExtents {
			return nil, fmt.Errorf("%w: iloc lists more than %d extents", core.ErrTooLarge, maxExtents)
		}
		for range extents {
			c.sized(indexSize)
			offset := c.sized(offsetSize)
			length := c.sized(lengthSize)
			if c.bad {
				break
			}
			e.extents = append(e.extents, ilocExtent{offset, length})
		}
		if c.bad {
			return nil, core.Malformedf("iloc entry for item %d overruns the box", e.id)
		}
		// Two locations for one item would let a removed item's bytes be adopted by
		// a kept item that happens to share its ID.
		if _, seen := table.index[e.id]; seen {
			return nil, core.Malformedf("iloc lists item %d twice", e.id)
		}
		table.index[e.id] = len(table.entries)
		e.raw = p[start:c.pos]
		table.entries = append(table.entries, e)
	}
	return table, nil
}

// heifItem is one entry of the iinf box and what is to become of it.
type heifItem struct {
	id          uint32
	version     uint8
	flags       uint32
	typ         string
	name        string
	contentType string
	protected   bool
	class       itemClass
}

// hidden reports the infe flag saying an item is not to be shown by itself.
func (it *heifItem) hidden() bool { return it.flags&1 != 0 }

// parseInfe reads an infe payload. Versions 0 and 1 have no type field and only
// ever describe a file-like item, so they are treated as mime items.
func parseInfe(p []byte) (*heifItem, error) {
	c := cursor{b: p}
	version := c.u8()
	flags := uint32(c.u8())<<16 | uint32(c.u16())
	it := &heifItem{version: version, flags: flags}
	switch version {
	case 0, 1:
		it.id = uint32(c.u16())
		it.protected = c.u16() != 0
		it.typ = "mime"
		it.name = c.cstring()
		it.contentType = c.cstring()
	case 2, 3:
		if version == 2 {
			it.id = uint32(c.u16())
		} else {
			it.id = c.u32()
		}
		it.protected = c.u16() != 0
		it.typ = string(c.take(4))
		it.name = c.cstring()
		if it.typ == "mime" || it.typ == "uri " {
			it.contentType = c.cstring()
		}
	default:
		return nil, core.Unsupportedf("isobmff: infe version %d", version)
	}
	if c.bad {
		return nil, core.Malformedf("infe box of %d bytes is too short", len(p))
	}
	return it, nil
}

// encodeInfe writes a kept item back without its name, which is free text that
// can say anything. Types that carry further fields are never kept.
func encodeInfe(dst []byte, it *heifItem) []byte {
	var p []byte
	p = binary.BigEndian.AppendUint32(p, uint32(it.version)<<24|it.flags)
	if it.version == 3 {
		p = binary.BigEndian.AppendUint32(p, it.id)
	} else {
		p = binary.BigEndian.AppendUint16(p, uint16(it.id))
	}
	p = append(p, 0, 0) // protection index; a protected file was already refused
	p = append(p, it.typ...)
	p = append(p, 0) // the empty name
	return appendBox(dst, "infe", p)
}

// itemRef is one typed reference from the iref box.
type itemRef struct {
	typ  string
	from uint32
	to   []uint32
}

// parseIref reads an iref payload. The IDs are 16 bits in version 0 and 32 in 1.
func (a *analysis) parseIref(data []byte, iref box) (uint8, []itemRef, error) {
	if iref.end-iref.body < 4 {
		return 0, nil, core.Malformedf("iref box of %d bytes is too short", iref.size())
	}
	version := data[iref.body]
	if version > 1 {
		return 0, nil, core.Unsupportedf("isobmff: iref version %d", version)
	}
	id := func(c *cursor) uint32 {
		if version == 0 {
			return uint32(c.u16())
		}
		return c.u32()
	}
	var refs []itemRef
	total := 0
	_, err := a.walk(data, iref.body+4, iref.end, func(b box) error {
		c := cursor{b: data[b.body:b.end]}
		ref := itemRef{typ: b.typ, from: id(&c)}
		count := int(c.u16())
		total += count
		if total > maxItemRefs {
			return fmt.Errorf("%w: iref lists more than %d references", core.ErrTooLarge, maxItemRefs)
		}
		for range count {
			to := id(&c)
			if c.bad {
				return core.Malformedf("%q item reference overruns its box", b.typ)
			}
			ref.to = append(ref.to, to)
		}
		if c.bad {
			return core.Malformedf("%q item reference of %d bytes is too short", b.typ, b.size())
		}
		refs = append(refs, ref)
		return nil
	})
	return version, refs, err
}

func encodeIref(version uint8, refs []itemRef) []byte {
	if len(refs) == 0 {
		return nil
	}
	appendID := func(dst []byte, id uint32) []byte {
		if version == 0 {
			return binary.BigEndian.AppendUint16(dst, uint16(id))
		}
		return binary.BigEndian.AppendUint32(dst, id)
	}
	body := []byte{version, 0, 0, 0}
	for _, ref := range refs {
		p := appendID(nil, ref.from)
		p = binary.BigEndian.AppendUint16(p, uint16(len(ref.to)))
		for _, to := range ref.to {
			p = appendID(p, to)
		}
		body = appendBox(body, ref.typ, p)
	}
	return appendBox(nil, "iref", body)
}

// propertyAssoc links an item to a property by its 1-based index in ipco.
type propertyAssoc struct {
	essential bool
	index     uint16
}

type ipmaEntry struct {
	id     uint32
	assocs []propertyAssoc
}

// ipmaTable is a parsed ipma box.
type ipmaTable struct {
	version uint8
	flags   uint32
	entries []ipmaEntry
}

func parseIpma(p []byte) (*ipmaTable, error) {
	c := cursor{b: p}
	version := c.u8()
	flags := uint32(c.u8())<<16 | uint32(c.u16())
	if version > 1 {
		return nil, core.Unsupportedf("isobmff: ipma version %d", version)
	}
	wide := flags&1 != 0
	count := c.u32()
	if c.bad {
		return nil, core.Malformedf("ipma box of %d bytes is too short", len(p))
	}
	idSize := 2
	if version == 1 {
		idSize = 4
	}
	if uint64(count) > uint64(c.remaining()/(idSize+1)) {
		return nil, core.Malformedf("ipma declares %d entries but has room for %d", count, c.remaining()/(idSize+1))
	}
	if count > maxItems {
		return nil, fmt.Errorf("%w: ipma lists %d items", core.ErrTooLarge, count)
	}
	table := &ipmaTable{version: version, flags: flags, entries: make([]ipmaEntry, 0, count)}
	for range count {
		var e ipmaEntry
		if version == 1 {
			e.id = c.u32()
		} else {
			e.id = uint32(c.u16())
		}
		n := int(c.u8())
		for range n {
			var assoc propertyAssoc
			if wide {
				v := c.u16()
				assoc = propertyAssoc{v&0x8000 != 0, v & 0x7FFF}
			} else {
				v := c.u8()
				assoc = propertyAssoc{v&0x80 != 0, uint16(v & 0x7F)}
			}
			e.assocs = append(e.assocs, assoc)
		}
		if c.bad {
			return nil, core.Malformedf("ipma entry for item %d overruns the box", e.id)
		}
		table.entries = append(table.entries, e)
	}
	return table, nil
}

func (t *ipmaTable) encode() []byte {
	body := binary.BigEndian.AppendUint32(nil, uint32(t.version)<<24|t.flags)
	body = binary.BigEndian.AppendUint32(body, uint32(len(t.entries)))
	for _, e := range t.entries {
		if t.version == 1 {
			body = binary.BigEndian.AppendUint32(body, e.id)
		} else {
			body = binary.BigEndian.AppendUint16(body, uint16(e.id))
		}
		body = append(body, byte(len(e.assocs)))
		for _, assoc := range e.assocs {
			if t.flags&1 != 0 {
				v := assoc.index
				if assoc.essential {
					v |= 0x8000
				}
				body = binary.BigEndian.AppendUint16(body, v)
				continue
			}
			v := byte(assoc.index)
			if assoc.essential {
				v |= 0x80
			}
			body = append(body, v)
		}
	}
	return appendBox(nil, "ipma", body)
}
