// Package jpegmeta holds what more than one handler reads of the JPEG and Adobe
// formats: the markers and shape rules of JPEG's decoding segments, which a JPEG
// file and a JPEG-compressed TIFF strip both hold, and the Photoshop resource and
// IPTC records that both can carry. Handler packages may not import one another, so
// this is where those rules live once.
package jpegmeta

import "encoding/binary"

// JPEG markers.
const (
	MarkerTEM   = 0x01
	MarkerDHT   = 0xC4
	MarkerJPG   = 0xC8
	MarkerDAC   = 0xCC
	MarkerRST0  = 0xD0
	MarkerRST7  = 0xD7
	MarkerSOI   = 0xD8
	MarkerEOI   = 0xD9
	MarkerSOS   = 0xDA
	MarkerDQT   = 0xDB
	MarkerDNL   = 0xDC
	MarkerDRI   = 0xDD
	MarkerDHP   = 0xDE
	MarkerEXP   = 0xDF
	MarkerAPP0  = 0xE0
	MarkerAPP14 = 0xEE
	MarkerAPP15 = 0xEF
	MarkerJPG0  = 0xF0
	MarkerJPG13 = 0xFD
	MarkerCOM   = 0xFE
)

// IsFrame reports a frame header (SOF) marker. The SOF codes are C0-CF apart from
// DHT, JPG and DAC.
func IsFrame(marker byte) bool {
	return marker >= 0xC0 && marker <= 0xCF && marker != MarkerDHT && marker != MarkerJPG && marker != MarkerDAC
}

// ValidFrame checks that a frame header's payload is exactly the size its component
// count says.
func ValidFrame(p []byte) bool {
	const fixed = 6 // precision, height, width, component count
	return len(p) > fixed && p[5] > 0 && len(p) == fixed+3*int(p[5])
}

// ValidScan checks that a scan header's payload is exactly the size its component
// count says.
func ValidScan(p []byte) bool {
	return len(p) >= 1 && p[0] >= 1 && p[0] <= 4 && len(p) == 4+2*int(p[0])
}

// TableEntry reads the first table in a segment's payload, which is not empty: its
// class and id, and how many bytes it takes up. Each marker has its own layout, and
// anything that is not exactly that layout is not ok, because room to spare in a kept
// segment would carry anything at all through cleaning.
type TableEntry func(p []byte) (class, id, size int, ok bool)

// QuantEntry reads one table of a DQT segment.
func QuantEntry(p []byte) (class, id, size int, ok bool) {
	precision, table := p[0]>>4, p[0]&0x0F
	if precision > 1 || table > 3 {
		return 0, 0, 0, false
	}
	size = 65
	if precision == 1 {
		size = 129
	}
	return 0, int(table), size, len(p) >= size
}

// HuffmanEntry reads one table of a DHT segment.
func HuffmanEntry(p []byte) (class, id, size int, ok bool) {
	const counts = 16
	if len(p) < 1+counts || p[0]>>4 > 1 || p[0]&0x0F > 3 {
		return 0, 0, 0, false
	}
	symbols := 0
	for _, n := range p[1 : 1+counts] {
		symbols += int(n)
	}
	size = 1 + counts + symbols
	return int(p[0] >> 4), int(p[0] & 0x0F), size, symbols <= 256 && len(p) >= size
}

// ArithmeticEntry reads one entry of a DAC segment, enforcing the ranges the
// specification gives: lower and upper bounds for a DC table, and a value of 1 to 63
// for an AC table. libjpeg is laxer, but no encoder writes anything else, and that
// leaves no room for text.
func ArithmeticEntry(p []byte) (class, id, size int, ok bool) {
	if len(p) < 2 || p[0]>>4 > 1 || p[0]&0x0F > 3 {
		return 0, 0, 0, false
	}
	value := p[1]
	if p[0]>>4 == 0 {
		ok = value&0x0F <= value>>4
	} else {
		ok = value >= 1 && value <= 63
	}
	return int(p[0] >> 4), int(p[0] & 0x0F), 2, ok
}

// RestartEntry reads a DRI segment, which holds one value and nothing else.
func RestartEntry(p []byte) (class, id, size int, ok bool) {
	return 0, 0, 2, len(p) == 2
}

// ValidTables checks that a segment's payload is one or more tables and nothing more.
func ValidTables(p []byte, entry TableEntry) bool {
	if len(p) == 0 {
		return false
	}
	for len(p) > 0 {
		_, _, size, ok := entry(p)
		if !ok {
			return false
		}
		p = p[size:]
	}
	return true
}

// Photoshop image resource IDs that handlers decode rather than count.
const (
	ResourceIPTC       = 0x0404
	ResourceThumbnail4 = 0x0409 // Photoshop 4 stored BGR thumbnails
	ResourceThumbnail5 = 0x040C
	ResourceExif1      = 0x0422
	ResourceExif3      = 0x0423
	ResourceXMP        = 0x0424
)

// NextResource reads one image resource block: a signature, an ID, a Pascal-style
// name padded to an even length, a size, and a value padded to an even length.
func NextResource(b []byte) (id uint16, value, rest []byte, ok bool) {
	const headerSize = 6 // signature and ID
	if len(b) < headerSize {
		return 0, nil, nil, false
	}
	switch string(b[:4]) {
	case "8BIM", "PHUT", "AgHg", "DCSR", "MeSa":
	default:
		return 0, nil, nil, false
	}
	id = binary.BigEndian.Uint16(b[4:])
	b = b[headerSize:]
	if len(b) < 1 {
		return 0, nil, nil, false
	}
	nameField := (int(b[0]) + 2) &^ 1 // the length byte counts towards the padding
	if len(b) < nameField+4 {
		return 0, nil, nil, false
	}
	b = b[nameField:]
	size := binary.BigEndian.Uint32(b)
	b = b[4:]
	if uint64(size) > uint64(len(b)) {
		return 0, nil, nil, false
	}
	value = b[:size]
	b = b[size:]
	if size%2 == 1 && len(b) > 0 {
		b = b[1:]
	}
	return id, value, b, true
}

const (
	// IPTCTagMarker starts every IIM dataset.
	IPTCTagMarker = 0x1C
	// IPTCApplicationRecord is the IIM record holding the fields people fill in.
	IPTCApplicationRecord = 2
)

// NextDataset reads one IIM dataset. A size with its top bit set is the width of a
// longer size field that follows, which is how IIM stores values over 32 KiB.
func NextDataset(b []byte) (record, dataset byte, value, rest []byte, ok bool) {
	const (
		headerSize   = 5
		extendedFlag = 0x8000
		maxSizeBytes = 4
	)
	if len(b) < headerSize || b[0] != IPTCTagMarker {
		return 0, 0, nil, nil, false
	}
	record, dataset = b[1], b[2]
	size := uint64(binary.BigEndian.Uint16(b[3:]))
	b = b[headerSize:]
	if size&extendedFlag != 0 {
		width := int(size &^ extendedFlag)
		if width > maxSizeBytes || width > len(b) {
			return 0, 0, nil, nil, false
		}
		size = 0
		for _, by := range b[:width] {
			size = size<<8 | uint64(by)
		}
		b = b[width:]
	}
	if size > uint64(len(b)) {
		return 0, 0, nil, nil, false
	}
	return record, dataset, b[:size], b[size:], true
}
