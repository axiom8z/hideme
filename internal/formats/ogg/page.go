package ogg

import (
	"encoding/binary"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
)

const (
	pageHeaderLen = 27
	maxSegments   = 255
	segmentMax    = 255

	flagContinued = 0x01
	flagBOS       = 0x02
	flagEOS       = 0x04

	crcOffset = 22
	seqOffset = 18

	// granuleNone is the granule position of a page on which no packet finishes.
	granuleNone = ^uint64(0)
)

var capturePattern = []byte("OggS")

var crcTable = makeCRCTable()

func makeCRCTable() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

// pageCRC computes the Ogg checksum of a page. The checksum field itself counts
// as zero, so a verified page need not be modified to check it.
func pageCRC(raw []byte) uint32 {
	var crc uint32
	for i, c := range raw {
		if i >= crcOffset && i < crcOffset+4 {
			c = 0
		}
		crc = crc<<8 ^ crcTable[byte(crc>>24)^c]
	}
	return crc
}

// page is one parsed page. Its slices point into the window's buffer, so they are
// valid only until the next read.
type page struct {
	raw     []byte
	flags   byte
	serial  uint32
	seq     uint32
	lacing  []byte
	payload []byte
}

// readPage parses the page at pos, where the capture pattern has already been
// seen. It reports false if the page runs past the end of the file. A damaged page
// is an error rather than something to skip, because skipping a page is how a
// comment packet with a wrong checksum would get past the parser.
func readPage(win *audiotag.Window, pos int64) (page, bool, error) {
	hdr := win.At(pos, pageHeaderLen)
	if hdr == nil {
		return page{}, false, win.Err()
	}
	if version := hdr[4]; version != 0 {
		return page{}, false, core.Malformedf("ogg: page at offset %d has version %d", pos, version)
	}
	flags := hdr[5]
	if flags&^(flagContinued|flagBOS|flagEOS) != 0 {
		return page{}, false, core.Malformedf("ogg: page at offset %d has reserved flags %#x", pos, flags)
	}
	segments := int(hdr[26])
	full := win.At(pos, pageHeaderLen+segments)
	if full == nil {
		return page{}, false, win.Err()
	}
	bodyLen := 0
	for _, lace := range full[pageHeaderLen:] {
		bodyLen += int(lace)
	}
	total := pageHeaderLen + segments + bodyLen
	raw := win.At(pos, total)
	if raw == nil {
		return page{}, false, win.Err()
	}
	if want := binary.LittleEndian.Uint32(raw[crcOffset:]); pageCRC(raw) != want {
		return page{}, false, core.Malformedf("ogg: page at offset %d fails its CRC", pos)
	}
	return page{
		raw:     raw,
		flags:   flags,
		serial:  binary.LittleEndian.Uint32(raw[14:]),
		seq:     binary.LittleEndian.Uint32(raw[seqOffset:]),
		lacing:  raw[pageHeaderLen : pageHeaderLen+segments],
		payload: raw[pageHeaderLen+segments:],
	}, true, nil
}

// buildPage assembles a page and its checksum.
func buildPage(flags byte, granule uint64, serial, seq uint32, lacing, payload []byte) []byte {
	raw := make([]byte, 0, pageHeaderLen+len(lacing)+len(payload))
	raw = append(raw, capturePattern...)
	raw = append(raw, 0, flags)
	raw = binary.LittleEndian.AppendUint64(raw, granule)
	raw = binary.LittleEndian.AppendUint32(raw, serial)
	raw = binary.LittleEndian.AppendUint32(raw, seq)
	raw = binary.LittleEndian.AppendUint32(raw, 0)
	raw = append(raw, byte(len(lacing)))
	raw = append(raw, lacing...)
	raw = append(raw, payload...)
	binary.LittleEndian.PutUint32(raw[crcOffset:], pageCRC(raw))
	return raw
}

// paginate lays packets out in pages, using as few as the 255-segment limit
// allows, and returns them numbered from seq. The final page takes lastFlags.
// The layout depends on nothing but the packets, so that cleaning a cleaned file
// reproduces it exactly.
func paginate(packets [][]byte, serial, seq uint32, lastFlags byte) [][]byte {
	var pages [][]byte
	var lacing, payload []byte
	var flags byte
	finished := false // a packet ends on the page being built
	flush := func(extra byte) {
		granule := granuleNone
		if finished {
			granule = 0
		}
		pages = append(pages, buildPage(flags|extra, granule, serial, seq, lacing, payload))
		seq++
		lacing, payload, finished = lacing[:0], payload[:0], false
	}
	for _, packet := range packets {
		rest := packet
		for {
			if len(lacing) == maxSegments {
				inPacket := len(rest) < len(packet) || lacing[maxSegments-1] == segmentMax
				flush(0)
				flags = 0
				if inPacket {
					flags = flagContinued
				}
			}
			n := min(len(rest), segmentMax)
			lacing = append(lacing, byte(n))
			payload = append(payload, rest[:n]...)
			rest = rest[n:]
			if n < segmentMax {
				finished = true
				break
			}
		}
	}
	flush(lastFlags)
	return pages
}
