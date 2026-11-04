package isobmff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/xmp"
)

// itemClass says what an item is and so whether it stays.
type itemClass int

const (
	itemImage itemClass = iota // stays
	itemExif
	itemMime
	itemURI
	itemThumbnail
	itemUnknown
	itemOrphaned // an image only something removed used
)

func (it *heifItem) removed() bool { return it.class != itemImage }

// imageItemTypes are the item types that are pictures, or are derived from them.
// Anything else is removed, so a metadata item type invented next year goes too.
var imageItemTypes = typeSet(
	"hvc1", "hev1", "av01", "avc1", "vvc1", "lhv1", "hvt1", "jpeg", "j2k1", "jxs1", "unci",
	"grid", "iovl", "iden", "tmap", "mski",
)

// itemProperties are the ipco boxes that describe how to decode and display an
// image. Anything else, such as udes (a free-text user description), is removed.
var itemProperties = typeSet(
	"hvcC", "av1C", "avcC", "vvcC", "lhvC", "jpgC", "j2kH", "uncC", "cmpd", "cpal", "cloc",
	"sbpm", "snuc", "fnuc", "cpat", "ispe", "pixi", "irot", "imir", "clap", "pasp", "colr",
	"auxC", "rloc", "lsel", "tols", "oinf", "a1op", "a1lx", "clli", "mdcv", "cclv", "amve",
	"reve", "iscl", "dvcC", "dvvC",
)

// property is one child of ipco and how it is used.
type property struct {
	box         box
	icc         bool
	orientation bool
	freeSpace   bool
	unknown     bool
	refs        int  // associations from items that stay
	removedRefs int  // associations from items being removed
	strictDrop  bool // strict mode is removing this from an item that stays
}

// metaBox is one pass over a HEIF file's top-level meta box.
type metaBox struct {
	a        *analysis
	m        *image
	top      box
	children []box // the children that stay, in file order

	primary    uint32
	hasPrimary bool

	items []*heifItem
	byID  map[uint32]*heifItem
	iinf  uint8 // iinf version
	iloc  *ilocTable

	irefVersion uint8
	refs        []itemRef

	props []*property
	ipmas []*ipmaTable

	dataRefs int  // entries in dref
	idat     *box // nil if absent
	ipco     *box // nil if absent
}

// cleanHEIFMeta cleans the meta box. The tables that list items are rebuilt
// smaller, then a free box fills the difference, so that the box keeps its size
// and every offset into the mdat stays right. Offsets into idat are relative to
// idat, so they stay right when the rebuilt boxes move it, and offsets from the
// start of the file that point into this box are refused by resolveMedia.
func (a *analysis) cleanHEIFMeta(top box) error {
	m, err := a.load(top)
	if err != nil {
		return err
	}
	h := &metaBox{a: a, m: m, top: top, byID: make(map[uint32]*heifItem)}
	if err := h.parseChildren(); err != nil {
		return err
	}
	if err := h.classifyItems(); err != nil {
		return err
	}
	h.reportItems()
	if err := h.editGroups(); err != nil {
		return err
	}
	if err := h.resolveProperties(); err != nil {
		return err
	}
	if err := h.resolveExtents(); err != nil {
		return err
	}
	data, err := h.rebuild()
	if err != nil {
		return err
	}
	a.patch(core.Patch{Off: top.start, Len: top.size(), Data: data})
	return nil
}

func (h *metaBox) find(typ string) *box {
	for i := range h.children {
		if h.children[i].typ == typ {
			return &h.children[i]
		}
	}
	return nil
}

func (h *metaBox) parseChildren() error {
	a, m := h.a, h.m
	header := h.top.body - h.top.start
	size := int64(len(m.data))
	if size < header+4 {
		return core.Malformedf("meta box of %d bytes is too short", size)
	}
	if v := m.data[header]; v != 0 {
		return core.Unsupportedf("isobmff: meta box version %d", v)
	}
	tail, err := a.walk(m.data, header+4, size, func(b box) error {
		switch b.typ {
		case "hdlr", "dinf", "pitm", "iloc", "iinf", "iref", "iprp", "idat", "grpl":
			if h.find(b.typ) != nil {
				return core.Malformedf("more than one %q box in meta", b.typ)
			}
			h.children = append(h.children, b)
		case "ipro":
			return fmt.Errorf("%w: item protection box", core.ErrEncrypted)
		case "free", "skip":
			if !core.AllZero(m.data[b.body:b.end]) {
				a.reportFreeSpace(b.end-b.body, "meta")
			}
		default:
			// Dropped when the box is rebuilt. Only what is listed above survives.
			a.describeRemoved(b.typ, m.data[b.body:b.end], b.end-b.body, "meta")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !core.AllZero(m.data[tail:size]) {
		a.reportFreeSpace(size-tail, "meta")
	}

	hdlr := h.find("hdlr")
	if hdlr == nil {
		return core.Malformedf("meta box without an hdlr box")
	}
	handler, err := handlerType(m.data[hdlr.body:hdlr.end])
	if err != nil {
		return err
	}
	if handler != "pict" {
		return core.Unsupportedf("isobmff: meta box with handler %q in an image file", handler)
	}
	if err := a.scrubHandler(m, *hdlr, "meta/hdlr"); err != nil {
		return err
	}
	if dinf := h.find("dinf"); dinf != nil {
		if h.dataRefs, err = a.checkDataRefs(m, *dinf, "meta/dinf"); err != nil {
			return err
		}
	}
	h.idat = h.find("idat")
	if err := h.parsePrimary(); err != nil {
		return err
	}
	if err := h.parseItems(); err != nil {
		return err
	}
	if b := h.find("iloc"); b != nil {
		if h.iloc, err = parseIloc(m.data[b.body:b.end]); err != nil {
			return err
		}
	}
	if b := h.find("iref"); b != nil {
		if h.irefVersion, h.refs, err = a.parseIref(m.data, *b); err != nil {
			return err
		}
	}
	return h.parseProperties()
}

func (h *metaBox) parsePrimary() error {
	b := h.find("pitm")
	if b == nil {
		return nil
	}
	c := cursor{b: h.m.data[b.body:b.end]}
	version := c.u8()
	c.take(3)
	switch version {
	case 0:
		h.primary = uint32(c.u16())
	case 1:
		h.primary = c.u32()
	default:
		return core.Unsupportedf("isobmff: pitm version %d", version)
	}
	if c.bad {
		return core.Malformedf("pitm box of %d bytes is too short", b.size())
	}
	h.hasPrimary = true
	return h.a.trimKept(h.m, *b, "meta/pitm")
}

// entityGroups are the groups of items that stay. Each holds only item IDs. Others
// exist, such as bracketed sets, but they only describe how pictures were taken.
var entityGroups = typeSet("altr", "ster", "brst")

// editGroups keeps a group only if it is a list of items this file declares, which
// also leaves it nothing to say about the ones being removed.
func (h *metaBox) editGroups() error {
	b := h.find("grpl")
	if b == nil {
		return nil
	}
	a, m := h.a, h.m
	const where = "meta/grpl"
	tail, err := a.walk(m.data, b.body, b.end, func(g box) error {
		switch {
		case isFreeSpace(g.typ):
			a.scrubNested(m, g, where)
		case !entityGroups[g.typ] || !h.editGroup(g, where):
			a.removeNested(m, g, where)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.scrubTail(m, tail, b.end, where)
	return nil
}

// editGroup rewrites a group's list down to the items that stay and clears the rest
// of the box. It reports false, changing nothing, if the box is anything else or if
// no item in it stays.
func (h *metaBox) editGroup(g box, where string) bool {
	const countField = 8 // after the version, flags and group ID
	m := h.m
	c := cursor{b: m.data[g.body:g.end]}
	c.take(countField)
	count := c.u32()
	if c.bad || int64(count) > int64(c.remaining()/4) {
		return false
	}
	var kept []uint32
	for range count {
		id := c.u32()
		it := h.byID[id]
		if it == nil {
			return false
		}
		if !it.removed() {
			kept = append(kept, id)
		}
	}
	if len(kept) == 0 {
		return false
	}
	listEnd := g.body + int64(c.pos)
	if m.zero(listEnd, g.end) {
		h.a.reportPastEnd(g, g.end-listEnd, where)
	}
	binary.BigEndian.PutUint32(m.data[g.body+countField:], uint32(len(kept)))
	for i, id := range kept {
		binary.BigEndian.PutUint32(m.data[g.body+countField+4+int64(i)*4:], id)
	}
	m.zero(g.body+countField+4+int64(len(kept))*4, listEnd)
	return true
}

func (h *metaBox) parseItems() error {
	b := h.find("iinf")
	if b == nil {
		return nil
	}
	m := h.m
	c := cursor{b: m.data[b.body:b.end]}
	h.iinf = c.u8()
	c.take(3)
	var declared uint32
	switch h.iinf {
	case 0:
		declared = uint32(c.u16())
	case 1, 2:
		// The standard gives every version but 0 this wider count. The count is checked
		// against the entries below, so a version 2 box laid out differently is refused.
		declared = c.u32()
	default:
		return core.Unsupportedf("isobmff: iinf version %d", h.iinf)
	}
	if c.bad {
		return core.Malformedf("iinf box of %d bytes is too short", b.size())
	}
	_, err := h.a.walk(m.data, b.body+int64(c.pos), b.end, func(e box) error {
		if e.typ != "infe" {
			return nil // dropped when iinf is rebuilt
		}
		if len(h.items) == maxItems {
			return fmt.Errorf("%w: more than %d items", core.ErrTooLarge, maxItems)
		}
		it, err := parseInfe(m.data[e.body:e.end])
		if err != nil {
			return err
		}
		if h.byID[it.id] != nil {
			return core.Malformedf("item %d is declared twice", it.id)
		}
		h.byID[it.id] = it
		h.items = append(h.items, it)
		return nil
	})
	if err != nil {
		return err
	}
	if int64(declared) != int64(len(h.items)) {
		return core.Malformedf("iinf declares %d items but holds %d", declared, len(h.items))
	}
	return nil
}

func (h *metaBox) parseProperties() error {
	iprp := h.find("iprp")
	if iprp == nil {
		return nil
	}
	a, m := h.a, h.m
	_, err := a.walk(m.data, iprp.body, iprp.end, func(b box) error {
		switch b.typ {
		case "ipco":
			return assign(&h.ipco, b)
		case "ipma":
			table, err := parseIpma(m.data[b.body:b.end])
			if err != nil {
				return err
			}
			h.ipmas = append(h.ipmas, table)
		}
		return nil
	})
	if err != nil || h.ipco == nil {
		return err
	}
	ipco := *h.ipco
	tail, err := a.walk(m.data, ipco.body, ipco.end, func(b box) error {
		if len(h.props) == maxItems {
			return fmt.Errorf("%w: more than %d item properties", core.ErrTooLarge, maxItems)
		}
		p := &property{box: b}
		switch {
		case b.typ == "free" || b.typ == "skip":
			p.freeSpace = true
			a.scrubNested(m, b, "meta/iprp/ipco")
		case !itemProperties[b.typ]:
			p.unknown = true
		default:
			if err := a.trimKept(m, b, "meta/iprp/ipco"); err != nil {
				return err
			}
			payload := m.data[b.body:b.end]
			p.orientation = b.typ == "irot" || b.typ == "imir"
			p.icc = b.typ == "colr" && len(payload) >= 4 && (string(payload[:4]) == "prof" || string(payload[:4]) == "rICC")
		}
		h.props = append(h.props, p)
		return nil
	})
	if err != nil {
		return err
	}
	a.scrubTail(m, tail, ipco.end, "meta/iprp/ipco")
	return nil
}

// classifyItems decides what stays. Removing a thumbnail can leave images that
// only it used: the hidden tiles of a thumbnail grid, for example, or its alpha
// plane. Those would be left in the file with nothing using them, so removal
// spreads along the dimg and auxl references, using a worklist so that a chain of
// hostile references costs time in proportion to its length.
func (h *metaBox) classifyItems() error {
	if !h.hasPrimary && len(h.items) > 0 {
		return core.Malformedf("meta box has items but no primary item")
	}
	for _, it := range h.items {
		switch {
		case it.protected:
			return fmt.Errorf("%w: item %d is protected", core.ErrEncrypted, it.id)
		case it.typ == "Exif":
			it.class = itemExif
		case it.typ == "mime":
			it.class = itemMime
		case it.typ == "uri ":
			it.class = itemURI
		case !imageItemTypes[it.typ]:
			it.class = itemUnknown
		}
	}
	inputs := make(map[uint32][]uint32)     // derived image -> its inputs
	dependents := make(map[uint32][]uint32) // main image -> its auxiliary images
	inputUsers := make(map[uint32]int)      // input -> how many derived images use it
	auxMasters := make(map[uint32]int)      // auxiliary image -> how many images it serves
	var work []*heifItem
	for _, ref := range h.refs {
		switch ref.typ {
		case "thmb":
			if it := h.byID[ref.from]; it != nil && it.class == itemImage {
				it.class = itemThumbnail
			}
		case "dimg":
			inputs[ref.from] = append(inputs[ref.from], ref.to...)
			for _, to := range ref.to {
				inputUsers[to]++
			}
		case "auxl":
			auxMasters[ref.from] += len(ref.to)
			for _, to := range ref.to {
				dependents[to] = append(dependents[to], ref.from)
			}
		}
	}
	for _, it := range h.items {
		if it.removed() {
			work = append(work, it)
		}
	}
	orphan := func(id uint32) {
		if it := h.byID[id]; it != nil && !it.removed() && !(h.hasPrimary && id == h.primary) {
			it.class = itemOrphaned
			work = append(work, it)
		}
	}
	for len(work) > 0 {
		it := work[len(work)-1]
		work = work[:len(work)-1]
		for _, input := range inputs[it.id] {
			inputUsers[input]--
			if target := h.byID[input]; inputUsers[input] == 0 && target != nil && target.hidden() {
				orphan(input)
			}
		}
		for _, aux := range dependents[it.id] {
			auxMasters[aux]--
			if auxMasters[aux] == 0 {
				orphan(aux)
			}
		}
	}

	if h.hasPrimary {
		primary := h.byID[h.primary]
		switch {
		case primary == nil:
			return core.Malformedf("primary item %d is not declared", h.primary)
		case primary.removed():
			return core.Malformedf("primary item %d is metadata or a thumbnail", h.primary)
		}
	}
	for _, ref := range h.refs {
		if from := h.byID[ref.from]; ref.typ != "dimg" || from == nil || from.removed() {
			continue
		}
		for _, to := range ref.to {
			if target := h.byID[to]; target == nil || target.removed() {
				return core.Malformedf("image %d is derived from item %d, which is not an image that stays", ref.from, to)
			}
		}
	}
	return nil
}

// reportItems reports each removed item, decoding the ones that have a format.
func (h *metaBox) reportItems() {
	const where = "meta"
	a := h.a
	for _, it := range h.items {
		switch it.class {
		case itemImage:
			if it.name != "" {
				a.remove(core.CatContent, core.RiskLow, "Item name", it.name, where)
			}
			if it.typ == "jpeg" || it.typ == "j2k1" {
				a.warnOnce("%q image items are whole image files, and metadata inside them, such as EXIF, is not removed", it.typ)
			}
		case itemExif:
			h.reportExif(it)
		case itemMime:
			data := h.itemData(it.id)
			if strings.Contains(it.contentType, "rdf+xml") || strings.Contains(it.contentType, "xmp") || xmp.Contains(data) {
				a.addXMP(data, where+"/XMP")
				continue
			}
			a.remove(core.CatHidden, core.RiskMed, "Embedded data item", fmt.Sprintf("%s, %d bytes", it.contentType, h.itemSize(it.id)), where)
		case itemURI:
			a.remove(core.CatHidden, core.RiskMed, "URI item", it.contentType, where)
		case itemThumbnail:
			a.remove(core.CatThumbnail, core.RiskHigh, "Embedded thumbnail", fmt.Sprintf("%s item, %d bytes", it.typ, h.itemSize(it.id)), where)
		case itemUnknown:
			a.remove(core.CatHidden, core.RiskMed, "Unknown item", fmt.Sprintf("%q, %d bytes", it.typ, h.itemSize(it.id)), where)
		}
	}
	if h.iloc != nil {
		for _, e := range h.iloc.entries {
			if h.byID[e.id] == nil {
				a.remove(core.CatHidden, core.RiskMed, "Unknown item", fmt.Sprintf("item %d has a location but no declaration", e.id), "meta/iloc")
			}
		}
	}
}

// reportExif decodes an Exif item. The item starts with a 4-byte offset to the
// TIFF header, which Apple sets to 6 to skip an "Exif\0\0" signature. Everything
// is reported as removed, including orientation: in HEIF that lives in the irot
// and imir properties, and readers ignore the copy in the Exif item.
func (h *metaBox) reportExif(it *heifItem) {
	if h.a.decoded >= maxDecodeTotal {
		h.a.remove(core.CatHidden, core.RiskHigh, "Exif metadata", fmt.Sprintf("%d bytes, not decoded", h.itemSize(it.id)), "meta/Exif")
		return
	}
	data := h.itemData(it.id)
	var block []byte
	if len(data) >= 4 {
		offset := uint64(binary.BigEndian.Uint32(data))
		if offset <= uint64(len(data)-4) {
			block = data[4+offset:]
		} else {
			block = data[4:]
		}
	}
	d, _ := exif.Parse(exif.StripPrefix(block))
	for _, f := range d.Findings("meta/Exif", core.Options{Strict: true}) {
		h.a.add(f)
	}
}

// itemData reads up to maxDecodeBytes of an item, for reporting only, while the
// file's budget lasts. An extent outside the file is skipped here and refused later
// by resolveExtents.
func (h *metaBox) itemData(id uint32) []byte {
	if h.iloc == nil {
		return nil
	}
	e := h.iloc.entry(id)
	if e == nil {
		return nil
	}
	var out []byte
	size := uint64(h.a.src.Size())
	limit := min(maxDecodeBytes, maxDecodeTotal-h.a.decoded)
	for _, x := range e.extents {
		start := e.base + x.offset
		if start < e.base {
			continue
		}
		n := min(x.length, uint64(max(limit-int64(len(out)), 0)))
		if n == 0 {
			continue
		}
		switch e.method {
		case 0:
			if start > size || n > size-start {
				continue
			}
			chunk := make([]byte, n)
			if readFull(h.a.src, chunk, int64(start)) == nil {
				out = append(out, chunk...)
			}
		case 1:
			if h.idat == nil {
				continue
			}
			idat := h.m.data[h.idat.body:h.idat.end]
			if start <= uint64(len(idat)) && n <= uint64(len(idat))-start {
				out = append(out, idat[start:start+n]...)
			}
		}
	}
	h.a.decoded += int64(len(out))
	return out
}

func (h *metaBox) itemSize(id uint32) uint64 {
	if h.iloc == nil {
		return 0
	}
	e := h.iloc.entry(id)
	if e == nil {
		return 0
	}
	var total uint64
	for _, x := range e.extents {
		total += x.length
	}
	return total
}

// resolveProperties rewrites the item-to-property links and frees properties
// nothing uses any more. A freed property stays the same size in place, because
// items refer to properties by position in ipco.
func (h *metaBox) resolveProperties() error {
	a, m := h.a, h.m
	auxTypes := make(map[uint32]string)
	for _, table := range h.ipmas {
		kept := table.entries[:0]
		for _, e := range table.entries {
			it := h.byID[e.id]
			removed := it != nil && it.removed()
			assocs := e.assocs[:0]
			for _, assoc := range e.assocs {
				if assoc.index == 0 {
					continue // "no property", which has no meaning to keep
				}
				if int(assoc.index) > len(h.props) {
					return core.Malformedf("item %d uses property %d of %d", e.id, assoc.index, len(h.props))
				}
				p := h.props[assoc.index-1]
				switch {
				case removed:
					p.removedRefs++
				case p.freeSpace:
				case p.unknown:
					if assoc.essential {
						return core.Unsupportedf("isobmff: item %d requires unknown property %q", e.id, p.box.typ)
					}
				case (p.icc || p.orientation) && a.opts.Strict:
					p.strictDrop = true
				default:
					p.refs++
					assocs = append(assocs, assoc)
					if p.box.typ == "auxC" {
						auxTypes[e.id] = auxiliaryType(m.data[p.box.body:p.box.end])
					}
				}
			}
			if !removed && len(assocs) > 0 {
				kept = append(kept, ipmaEntry{e.id, assocs})
			}
		}
		table.entries = kept
	}

	for _, p := range h.props {
		b := p.box
		switch {
		case p.freeSpace:
		case p.unknown:
			h.reportUnknownProperty(p)
			m.free(b)
		case p.strictDrop:
			if p.orientation {
				a.remove(core.CatTechnical, core.RiskNone, "Orientation", h.orientationValue(b), "meta/iprp/ipco")
			} else {
				a.remove(core.CatTechnical, core.RiskLow, "ICC colour profile", fmt.Sprintf("%d bytes", b.end-b.body-4), "meta/iprp/ipco")
			}
			m.free(b)
		case p.refs == 0:
			if p.removedRefs == 0 {
				a.remove(core.CatHidden, core.RiskLow, "Unreferenced image property", fmt.Sprintf("%q, %d bytes", b.typ, b.size()), "meta/iprp/ipco")
			}
			m.free(b)
		case p.icc:
			a.retain(core.CatTechnical, core.RiskLow, "ICC colour profile", fmt.Sprintf("%d bytes", b.end-b.body-4), "meta/iprp/ipco", iccReason)
		case p.orientation:
			a.retain(core.CatTechnical, core.RiskNone, "Orientation", h.orientationValue(b), "meta/iprp/ipco", orientationReason)
		}
	}
	h.reportRetainedImages(auxTypes)
	return nil
}

// orientationReason explains retaining a rotation or mirroring, in the same words
// as the exif package uses for the tag that does this job in other formats.
const orientationReason = "needed to display the image the right way up; strict mode removes it"

func (h *metaBox) orientationValue(b box) string {
	payload := h.m.data[b.body:b.end]
	if b.typ == "imir" || len(payload) == 0 {
		return "mirrored"
	}
	return fmt.Sprintf("rotated %d degrees", int(payload[0]&3)*90)
}

func (h *metaBox) reportUnknownProperty(p *property) {
	payload := h.m.data[p.box.body:p.box.end]
	if p.box.typ == "udes" && len(payload) > 4 {
		c := cursor{b: payload[4:]}
		c.cstring() // language
		name := c.cstring()
		description := c.cstring()
		h.a.remove(core.CatContent, core.RiskMed, "Image description", strings.TrimSpace(name+" "+description), "meta/iprp/ipco")
		return
	}
	h.a.remove(core.CatHidden, core.RiskMed, "Unknown image property", fmt.Sprintf("%q, %d bytes", p.box.typ, p.box.size()), "meta/iprp/ipco")
}

// auxiliaryType reads the URN from an auxC payload, after its version and flags.
func auxiliaryType(p []byte) string {
	if len(p) < 4 {
		return ""
	}
	return trimmedText(p[4:])
}

// reportRetainedImages tells the user about pictures that stay besides the main
// one, since a depth map or a second image can reveal more than they might expect.
func (h *metaBox) reportRetainedImages(auxTypes map[uint32]string) {
	a := h.a
	used := make(map[uint32]bool) // inputs to a derived image, and auxiliary images
	for _, ref := range h.refs {
		from := h.byID[ref.from]
		if from == nil || from.removed() {
			continue
		}
		switch ref.typ {
		case "dimg":
			for _, to := range ref.to {
				used[to] = true
			}
		case "auxl":
			used[ref.from] = true
			a.retain(core.CatContent, core.RiskLow, "Auxiliary image", auxTypes[ref.from], "meta",
				"depth, transparency or HDR data that changes how the photo looks; removing it would change the image")
		}
	}
	extra := 0
	for _, it := range h.items {
		if !it.removed() && !used[it.id] && !(h.hasPrimary && it.id == h.primary) {
			extra++
		}
	}
	if extra > 0 {
		a.retain(core.CatContent, core.RiskLow, "Additional image", fmt.Sprintf("%d more", extra), "meta",
			"other pictures in the file are content, not metadata")
	}
}

// resolveExtents sorts every item's bytes into kept and removed, and zeroes what
// is removed and unreferenced in idat. Bytes in the mdat are handed to
// resolveMedia, along with any from a moov, so they are handled in one place.
func (h *metaBox) resolveExtents() error {
	a := h.a
	var idatKeep, idatDrop []span
	var idatSize uint64
	if h.idat != nil {
		idatSize = uint64(h.idat.end - h.idat.body)
	}
	fileSize := uint64(a.src.Size())
	var entries []ilocEntry
	if h.iloc != nil {
		entries = h.iloc.entries
	}
	for _, e := range entries {
		it := h.byID[e.id]
		removed := it == nil || it.removed()
		if e.dataRef != 0 && int(e.dataRef) > h.dataRefs {
			return core.Malformedf("item %d uses data reference %d of %d", e.id, e.dataRef, h.dataRefs)
		}
		if e.method > 2 {
			return core.Unsupportedf("isobmff: item %d has construction method %d", e.id, e.method)
		}
		if e.method == 2 {
			// Bytes addressed through another item cannot be told apart from it. If
			// that item stays they stay too, which would leave a removed item's bytes
			// in the file.
			return core.Unsupportedf("isobmff: item %d is constructed from another item", e.id)
		}
		for _, x := range e.extents {
			start := e.base + x.offset
			if start < e.base {
				return core.Malformedf("item %d has an extent offset that overflows", e.id)
			}
			if x.length == 0 {
				// Zero means "the rest of the data". If a kept item used it, nothing
				// else in the mdat could be told apart from its bytes.
				if !removed {
					return core.Unsupportedf("isobmff: item %d has an extent of unstated length", e.id)
				}
				continue
			}
			limit := fileSize
			if e.method == 1 {
				if h.idat == nil {
					return core.Malformedf("item %d is stored in an idat box that does not exist", e.id)
				}
				limit = idatSize
			}
			if start > limit || x.length > limit-start {
				return core.Malformedf("item %d has an extent outside its data", e.id)
			}
			s := span{int64(start), int64(start + x.length)}
			if !removed && it != nil && len(e.extents) == 1 {
				s.end = min(s.end, s.start+h.derivedLength(it))
			}
			switch {
			case e.method == 1 && removed:
				idatDrop = append(idatDrop, s)
			case e.method == 1:
				idatKeep = append(idatKeep, s)
			case removed:
				a.drop = append(a.drop, s)
			default:
				a.keep = append(a.keep, s)
			}
		}
	}
	if err := h.checkItemsLocated(); err != nil {
		return err
	}
	return h.zeroIdat(idatKeep, idatDrop)
}

// checkItemsLocated refuses a kept picture with no location. Its bytes could not
// be told from unreferenced ones, and zeroing those would destroy the picture.
// An iden item is the one kind that has no bytes of its own.
func (h *metaBox) checkItemsLocated() error {
	located := make(map[uint32]bool)
	if h.iloc != nil {
		for _, e := range h.iloc.entries {
			if len(e.extents) > 0 {
				located[e.id] = true
			}
		}
	}
	for _, it := range h.items {
		if !it.removed() && it.typ != "iden" && !located[it.id] {
			return core.Malformedf("image item %d has no location", it.id)
		}
	}
	return nil
}

// zeroIdat clears removed items' bytes in idat, and anything no item refers to.
func (h *metaBox) zeroIdat(keep, drop []span) error {
	if h.idat == nil {
		return nil
	}
	keep, drop = mergeSpans(keep), mergeSpans(drop)
	if s, overlap := spansOverlap(keep, drop); overlap {
		return core.Malformedf("idat bytes %d to %d belong to both a kept and a removed item", s.start, s.end)
	}
	base := h.idat.body
	for _, s := range drop {
		h.m.zero(base+s.start, base+s.end)
	}
	size := h.idat.end - base
	var orphaned, pos int64
	for _, s := range append(mergeSpans(append(keep, drop...)), span{size, size}) {
		if s.start > pos && h.m.zero(base+pos, base+s.start) {
			orphaned += s.start - pos
		}
		pos = s.end
	}
	if orphaned > 0 {
		h.a.remove(core.CatHidden, core.RiskMed, "Unreferenced item data", fmt.Sprintf("%d bytes", orphaned), "meta/idat")
	}
	return nil
}

// rebuild lays the meta box out again from what stays, in the original order.
func (h *metaBox) rebuild() ([]byte, error) {
	m := h.m
	size := int64(len(m.data))
	var hdlrAt int
	header := h.top.body - h.top.start
	out := bytes.Clone(m.data[:header+4])
	for _, b := range h.children {
		switch b.typ {
		case "hdlr":
			hdlrAt = len(out)
			out = appendBox(out, "hdlr", m.data[b.body:b.end])
		case "iloc":
			out = h.appendIloc(out)
		case "iinf":
			out = h.appendIinf(out)
		case "iref":
			out = append(out, encodeIref(h.irefVersion, h.keptRefs())...)
		case "iprp":
			out = h.appendIprp(out)
		default:
			out = append(out, m.data[b.start:b.end]...)
		}
	}
	slack := size - int64(len(out))
	switch {
	case slack < 0:
		return nil, core.Malformedf("rebuilt meta box is larger than the original")
	case slack >= 8:
		out = appendBox(out, "free", make([]byte, slack-8))
	case slack > 0:
		// A free box cannot be smaller than its own header. The handler name is
		// already blank, so it can take the bytes as padding after its first NUL.
		hdlrSize := binary.BigEndian.Uint32(out[hdlrAt:])
		binary.BigEndian.PutUint32(out[hdlrAt:], hdlrSize+uint32(slack))
		insert := hdlrAt + int(hdlrSize)
		out = append(out[:insert], append(make([]byte, slack), out[insert:]...)...)
	}
	return out, nil
}

func (h *metaBox) appendIloc(out []byte) []byte {
	var kept [][]byte
	for _, e := range h.iloc.entries {
		if it := h.byID[e.id]; it != nil && !it.removed() {
			kept = append(kept, e.raw)
		}
	}
	// The count sits at the end of the header, at 2 or 4 bytes wide.
	body := bytes.Clone(h.iloc.header)
	if h.iloc.version == 2 {
		binary.BigEndian.PutUint32(body[len(body)-4:], uint32(len(kept)))
	} else {
		binary.BigEndian.PutUint16(body[len(body)-2:], uint16(len(kept)))
	}
	for _, raw := range kept {
		body = append(body, raw...)
	}
	return appendBox(out, "iloc", body)
}

func (h *metaBox) appendIinf(out []byte) []byte {
	body := []byte{h.iinf, 0, 0, 0}
	kept := 0
	var entries []byte
	for _, it := range h.items {
		if !it.removed() {
			entries = encodeInfe(entries, it)
			kept++
		}
	}
	if h.iinf == 0 {
		body = binary.BigEndian.AppendUint16(body, uint16(kept))
	} else {
		body = binary.BigEndian.AppendUint32(body, uint32(kept))
	}
	return appendBox(out, "iinf", body, entries)
}

// derivedLength returns how many bytes of a derived image's description its own fields
// account for. A decoder stops there, so the rest of the item is treated like any other
// bytes no item uses. Other items' bytes all count, as does a description that does not
// follow the only version there is.
func (h *metaBox) derivedLength(it *heifItem) int64 {
	const noLimit = int64(1) << 62
	if it.typ != "grid" && it.typ != "iovl" {
		return noLimit
	}
	data := h.itemData(it.id)
	if len(data) < 2 || data[0] != 0 {
		return noLimit
	}
	size := int64(2) // bytes per dimension and per offset
	if data[1]&1 != 0 {
		size = 4
	}
	if it.typ == "grid" {
		return 4 + 2*size // version, flags, rows and columns, then width and height
	}
	references := int64(0)
	for _, ref := range h.refs {
		if ref.typ == "dimg" && ref.from == it.id {
			references += int64(len(ref.to))
		}
	}
	return 10 + 2*size + references*2*size // then the fill colour, the size and each offset
}

// keptRefs drops references from removed items and to removed items, and drops a
// reference altogether once nothing is left for it to point at. One that names an item
// the file does not declare points at nothing either, and an ID there is only somewhere
// to put two or four bytes.
func (h *metaBox) keptRefs() []itemRef {
	var kept []itemRef
	for _, ref := range h.refs {
		if from := h.byID[ref.from]; from == nil || from.removed() {
			continue
		}
		var to []uint32
		for _, id := range ref.to {
			if target := h.byID[id]; target != nil && !target.removed() {
				to = append(to, id)
			}
		}
		if len(to) > 0 {
			kept = append(kept, itemRef{ref.typ, ref.from, to})
		}
	}
	return kept
}

func (h *metaBox) appendIprp(out []byte) []byte {
	var body []byte
	if h.ipco != nil {
		body = append(body, h.m.data[h.ipco.start:h.ipco.end]...)
	}
	for _, table := range h.ipmas {
		body = append(body, table.encode()...)
	}
	return appendBox(out, "iprp", body)
}
