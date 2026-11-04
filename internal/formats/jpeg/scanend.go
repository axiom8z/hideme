package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
)

// Limits on walking scans. Within them a real picture is walked in full, and beyond
// them the scan is only copied, which is what happens to any scan this file cannot
// walk. A frame of 2^24 blocks is about 700 megapixels. A progressive scan's
// end-of-band runs cover thousands of blocks for two bytes, so what a hostile file can
// spend is bounded by counting blocks, not bytes.
const (
	maxWalkedFrameBlocks = 1 << 24
	maxWalkedBlocks      = 1 << 28
	walkContextStride    = 1 << 12 // blocks between context checks
)

// frameComponent is one component of the frame header.
type frameComponent struct {
	id, h, v byte
	nonzero  []uint64 // for each block, which coefficients a progressive scan has set
}

// huffmanTable decodes canonical codes one bit at a time, which is slow compared with
// a real decoder but needs no read-ahead, so the walk knows exactly where it stopped.
type huffmanTable struct {
	maxCode [17]int32 // the largest code of each length, or -1 if there are none
	valPtr  [17]int32 // the position in symbols of each length's smallest code
	minCode [17]int32
	symbols []byte
}

func parseHuffman(table []byte) *huffmanTable {
	const counts = 16
	if len(table) < 1+counts {
		return nil
	}
	h := &huffmanTable{symbols: table[1+counts:]}
	var code, next int32
	for length := 1; length <= counts; length++ {
		n := int32(table[length])
		h.valPtr[length], h.minCode[length], h.maxCode[length] = next, code, -1
		if n > 0 {
			code += n
			next += n
			if code > 1<<length {
				return nil // more codes than fit in this many bits
			}
			h.maxCode[length] = code - 1
		}
		code <<= 1
	}
	if int(next) != len(h.symbols) {
		return nil
	}
	return h
}

// bitReader reads the bits of entropy-coded data, undoing byte stuffing. It stops at
// any marker. It reads one byte at a time, so pos is always just past the byte that
// supplied the last bit.
type bitReader struct {
	data []byte
	pos  int
	cur  byte
	left uint
}

func (r *bitReader) load() bool {
	if r.pos >= len(r.data) {
		return false
	}
	b := r.data[r.pos]
	next := r.pos + 1
	if b == 0xFF {
		for next < len(r.data) && r.data[next] == 0xFF {
			next++ // fill bytes
		}
		if next == len(r.data) || r.data[next] != 0 {
			return false // a marker
		}
		next++
	}
	r.pos, r.cur, r.left = next, b, 8
	return true
}

func (r *bitReader) bits(n int) (int32, bool) {
	var v int32
	for n > 0 {
		if r.left == 0 && !r.load() {
			return 0, false
		}
		take := min(uint(n), r.left)
		r.left -= take
		v = v<<take | int32(r.cur>>r.left)&(1<<take-1)
		n -= int(take)
	}
	return v, true
}

func (r *bitReader) symbol(h *huffmanTable) (byte, bool) {
	var code int32
	for length := 1; length <= 16; length++ {
		if r.left == 0 && !r.load() {
			return 0, false
		}
		r.left--
		code = code<<1 | int32(r.cur>>r.left)&1
		if code <= h.maxCode[length] {
			i := h.valPtr[length] + code - h.minCode[length]
			if int(i) >= len(h.symbols) {
				return 0, false
			}
			return h.symbols[i], true
		}
	}
	return 0, false // no code of any length matches
}

// scanWalker follows one scan's Huffman codes, block by block, to find out where the
// picture's data stops.
type scanWalker struct {
	r      bitReader
	eobrun int32

	ss, se int
	ah     byte
	dc, ac *huffmanTable
}

// scanComponent is one component a scan covers.
type scanComponent struct {
	comp         *frameComponent
	dc, ac       *huffmanTable
	across, down int // the size of a non-interleaved scan, in blocks
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// walkScan returns the scan's data without what follows its blocks. It reports false
// when it cannot follow the scan to its end, and the data must then be kept whole,
// since nothing is known about it. That covers arithmetic and lossless coding, a height
// left to a DNL segment, tables the file never defines, and damaged or short data.
func (c *cleaner) walkScan(ctx context.Context, header, data []byte) (kept []byte, removed int, ok bool) {
	if c.frameMarker > 0xC2 || c.frameHeight == 0 || c.frameWidth == 0 || c.walkedBlocks >= maxWalkedBlocks {
		return nil, 0, false
	}
	progressive := c.frameMarker == 0xC2
	var hMax, vMax int
	for i := range c.components {
		hMax, vMax = max(hMax, int(c.components[i].h)), max(vMax, int(c.components[i].v))
	}
	if hMax == 0 || vMax == 0 || hMax > 4 || vMax > 4 {
		return nil, 0, false
	}

	count := int(header[0])
	ss, se, approx := int(header[1+2*count]), int(header[2+2*count]), header[3+2*count]
	ah := approx >> 4
	if progressive {
		if se > 63 || ss > se || (ss == 0 && se != 0) || (ss > 0 && count != 1) {
			return nil, 0, false
		}
	} else {
		ss, se, ah = 0, 63, 0 // libjpeg and image/jpeg do not read these in a sequential scan
	}

	var comps []scanComponent
	blocksPerMCU := 0
	for i := range count {
		selector := header[1+2*i]
		var comp *frameComponent
		for j := range c.components {
			if c.components[j].id == selector {
				comp = &c.components[j]
				break
			}
		}
		if comp == nil || comp.h == 0 || comp.v == 0 {
			return nil, 0, false
		}
		tables := header[2+2*i]
		sc := scanComponent{comp: comp}
		sc.across = ceilDiv(ceilDiv(c.frameWidth*int(comp.h), hMax), 8)
		sc.down = ceilDiv(ceilDiv(c.frameHeight*int(comp.v), vMax), 8)
		// A table is only read if this kind of scan uses it.
		if !progressive || (ss == 0 && ah == 0) {
			if sc.dc = c.huffman(0, tables>>4); sc.dc == nil {
				return nil, 0, false
			}
		}
		if !progressive || ss > 0 {
			if sc.ac = c.huffman(1, tables&0x0F); sc.ac == nil {
				return nil, 0, false
			}
		}
		comps = append(comps, sc)
		blocksPerMCU += int(comp.h) * int(comp.v)
	}

	// A scan of one component is not interleaved: its unit is one block, in that
	// component's own grid, whatever its sampling factors say.
	mcus := comps[0].across * comps[0].down
	if count > 1 {
		mcus = ceilDiv(c.frameWidth, 8*hMax) * ceilDiv(c.frameHeight, 8*vMax)
	} else {
		blocksPerMCU = 1
	}
	if int64(mcus)*int64(blocksPerMCU) > maxWalkedFrameBlocks {
		return nil, 0, false
	}

	w := &scanWalker{r: bitReader{data: data}, ss: ss, se: se, ah: ah}
	if progressive && ss > 0 && comps[0].comp.nonzero == nil {
		comps[0].comp.nonzero = make([]uint64, comps[0].across*comps[0].down)
	}
	interval := c.restartInterval()
	var keep [][2]int
	start := 0 // where the interval being walked starts
	nextRestart := byte(0)
	for mcu := range mcus {
		if interval > 0 && mcu > 0 && mcu%interval == 0 {
			marker, after, found := findRestart(data, w.r.pos)
			if !found || data[marker+1] != 0xD0+nextRestart {
				return nil, 0, false
			}
			nextRestart = (nextRestart + 1) % 8
			keep = append(keep, [2]int{start, w.r.pos}, [2]int{marker, after})
			start = after
			w.r = bitReader{data: data, pos: after}
			w.eobrun = 0
		}
		for i := range comps {
			sc := &comps[i]
			w.dc, w.ac = sc.dc, sc.ac
			blocks := 1
			if count > 1 {
				blocks = int(sc.comp.h) * int(sc.comp.v)
			}
			for range blocks {
				if c.walkedBlocks%walkContextStride == 0 && ctx.Err() != nil {
					return nil, 0, false
				}
				if c.walkedBlocks++; c.walkedBlocks > maxWalkedBlocks {
					return nil, 0, false
				}
				var nonzero *uint64
				if progressive && ss > 0 {
					nonzero = &sc.comp.nonzero[mcu]
				}
				if !w.block(progressive, nonzero) {
					return nil, 0, false
				}
			}
		}
	}
	keep = append(keep, [2]int{start, w.r.pos})

	// Fill bytes in front of a marker are padding that real writers use, and any number
	// of them means the same thing. Anything else is what this walk is here to find.
	spare := false
	end := 0
	for _, k := range keep {
		if k[0] > end && !allFill(data[end:k[0]]) {
			spare = true
		}
		end = k[1]
	}
	if end < len(data) && !allFill(data[end:]) {
		spare = true
	}
	if !spare {
		return nil, 0, false
	}
	for _, k := range keep {
		kept = append(kept, data[k[0]:k[1]]...)
	}
	return kept, len(data) - len(kept), true
}

func allFill(b []byte) bool {
	for _, v := range b {
		if v != 0xFF {
			return false
		}
	}
	return true
}

// findRestart returns the offset of the next restart marker and the offset just past
// it, stepping over stuffed bytes and fill bytes, as a decoder does when it looks for
// the marker that ends an interval.
func findRestart(data []byte, pos int) (marker, after int, found bool) {
	for {
		i := bytes.IndexByte(data[pos:], 0xFF)
		if i < 0 || pos+i+1 >= len(data) {
			return 0, 0, false
		}
		pos += i
		switch next := data[pos+1]; {
		case next >= 0xD0 && next <= 0xD7:
			return pos, pos + 2, true
		case next == 0xFF:
			pos++
		default:
			pos += 2
		}
	}
}

// huffman returns the table of a class and id in effect at this point in the file.
func (c *cleaner) huffman(class, id byte) *huffmanTable {
	if class > 1 || id > 3 {
		return nil
	}
	i := c.tables.current[tableHuffman][class][id]
	if i == 0 {
		return nil
	}
	def := c.tables.defs[i-1]
	return parseHuffman(c.parts[def.part][def.start:def.end])
}

// restartInterval returns the interval in effect at this point in the file.
func (c *cleaner) restartInterval() int {
	i := c.tables.current[tableRestart][0][0]
	if i == 0 {
		return 0
	}
	def := c.tables.defs[i-1]
	return int(binary.BigEndian.Uint16(c.parts[def.part][def.start:def.end]))
}

// block reads one block's codes, in the way each kind of scan lays them out.
func (w *scanWalker) block(progressive bool, nonzero *uint64) bool {
	switch {
	case !progressive:
		return w.sequentialBlock()
	case w.ss == 0 && w.ah == 0:
		return w.dcValue()
	case w.ss == 0:
		_, ok := w.r.bits(1)
		return ok
	case w.ah == 0:
		return w.firstACBlock(nonzero)
	}
	return w.refineACBlock(nonzero)
}

func (w *scanWalker) dcValue() bool {
	size, ok := w.r.symbol(w.dc)
	if !ok || size > 16 {
		return false
	}
	_, ok = w.r.bits(int(size))
	return ok
}

func (w *scanWalker) sequentialBlock() bool {
	if !w.dcValue() {
		return false
	}
	for k := 1; k < 64; k++ {
		rs, ok := w.r.symbol(w.ac)
		if !ok {
			return false
		}
		run, size := int(rs>>4), int(rs&0x0F)
		if size == 0 {
			if run != 15 {
				return true
			}
			k += 15
			continue
		}
		k += run
		if _, ok := w.r.bits(size); !ok {
			return false
		}
	}
	return true
}

// firstACBlock reads a band's first pass, noting which coefficients it sets, because
// how a later refinement reads depends on them.
func (w *scanWalker) firstACBlock(nonzero *uint64) bool {
	if w.eobrun > 0 {
		w.eobrun--
		return true
	}
	for k := w.ss; k <= w.se; k++ {
		rs, ok := w.r.symbol(w.ac)
		if !ok {
			return false
		}
		run, size := int(rs>>4), int(rs&0x0F)
		if size != 0 {
			k += run
			if _, ok := w.r.bits(size); !ok {
				return false
			}
			*nonzero |= 1 << min(k, 63)
			continue
		}
		if run != 15 {
			extra, ok := w.r.bits(run)
			if !ok {
				return false
			}
			w.eobrun = 1<<run + extra - 1
			return true
		}
		k += 15
	}
	return true
}

// refineACBlock reads a band's later pass, with the same steps as libjpeg's.
func (w *scanWalker) refineACBlock(nonzero *uint64) bool {
	k := w.ss
	if w.eobrun == 0 {
		for ; k <= w.se; k++ {
			rs, ok := w.r.symbol(w.ac)
			if !ok {
				return false
			}
			run, size := int(rs>>4), int(rs&0x0F)
			if size != 0 {
				if _, ok := w.r.bits(1); !ok {
					return false
				}
			} else if run != 15 {
				extra, ok := w.r.bits(run)
				if !ok {
					return false
				}
				w.eobrun = 1<<run + extra
				break
			}
			for ; k <= w.se; k++ {
				if *nonzero>>k&1 != 0 {
					if _, ok := w.r.bits(1); !ok {
						return false
					}
				} else if run--; run < 0 {
					break
				}
			}
			if size != 0 {
				*nonzero |= 1 << min(k, 63)
			}
		}
	}
	if w.eobrun > 0 {
		for ; k <= w.se; k++ {
			if *nonzero>>k&1 != 0 {
				if _, ok := w.r.bits(1); !ok {
					return false
				}
			}
		}
		w.eobrun--
	}
	return true
}
