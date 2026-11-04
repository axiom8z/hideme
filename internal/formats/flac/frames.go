package flac

import (
	"context"
	"encoding/binary"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxFrameHeaderLen is the longest possible frame header: sync and fields (4),
	// a 7-byte coded number, 2 bytes each of block size and sample rate, and CRC-8.
	maxFrameHeaderLen = 16
	// minFrameLen is the shortest possible frame: a 6-byte header, one byte of
	// subframe and the CRC-16.
	minFrameLen = 9
)

var (
	crc8Table  = makeCRC8Table()
	crc16Table = makeCRC16Table()
)

func makeCRC8Table() (t [256]byte) {
	for i := range t {
		c := byte(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

func makeCRC16Table() (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

// frameHeaderLen returns the length of the valid frame header at the start of b,
// or 0 if there is none. The CRC-8 makes accepting a chance sync improbable.
func frameHeaderLen(b []byte) int {
	if len(b) < 6 || b[0] != 0xFF || b[1]&0xFE != 0xF8 {
		return 0
	}
	blockCode, rateCode := b[2]>>4, b[2]&0x0F
	channelCode, sizeCode := b[3]>>4, b[3]>>1&0x07
	if blockCode == 0 || rateCode == 15 || channelCode >= 11 || sizeCode == 3 || b[3]&1 != 0 {
		return 0
	}
	n := 4 + codedNumberLen(b[4:])
	if n == 4 {
		return 0
	}
	switch blockCode {
	case 6:
		n++
	case 7:
		n += 2
	}
	switch rateCode {
	case 12:
		n++
	case 13, 14:
		n += 2
	}
	if len(b) < n+1 {
		return 0
	}
	var crc byte
	for _, c := range b[:n] {
		crc = crc8Table[crc^c]
	}
	if crc != b[n] {
		return 0
	}
	return n + 1
}

// frameBlockSize returns how many samples the frame with the valid header at the start
// of b holds.
func frameBlockSize(b []byte) int64 {
	code := b[2] >> 4
	extra := b[4+codedNumberLen(b[4:]):]
	switch {
	case code == 1:
		return 192
	case code <= 5:
		return 576 << (code - 2)
	case code == 6:
		return int64(extra[0]) + 1
	case code == 7:
		return int64(binary.BigEndian.Uint16(extra)) + 1
	}
	return 256 << (code - 8)
}

// codedNumberLen returns the length of the UTF-8-style coded frame or sample
// number at the start of b, or 0 if it is invalid. The coding extends UTF-8 to 36
// bits, so unicode/utf8 cannot read it.
func codedNumberLen(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	first := b[0]
	var n int
	switch {
	case first < 0x80:
		return 1
	case first < 0xC0:
		return 0
	case first < 0xE0:
		n = 2
	case first < 0xF0:
		n = 3
	case first < 0xF8:
		n = 4
	case first < 0xFC:
		n = 5
	case first < 0xFE:
		n = 6
	case first == 0xFE:
		n = 7
	default:
		return 0
	}
	if len(b) < n {
		return 0
	}
	for _, c := range b[1:n] {
		if c&0xC0 != 0x80 {
			return 0
		}
	}
	return n
}

// frameWalker finds where audio frames end.
//
// FLAC frames do not state their length, and finding it by decoding every subframe
// would mean implementing the codec. But each frame ends in a CRC-16 of itself, so
// a CRC run over a frame reaches zero exactly at its end. It also reaches zero by
// chance about once in 64 KiB, so a zero only counts if something that should
// follow a frame comes next.
//
// This validates every audio byte as a side effect, and is why junk in or after
// the audio can be told from the audio itself.
type frameWalker struct {
	ctx      context.Context
	win      *audiotag.Window
	tagStart int64 // where the tags anchored to the end of the file begin

	audioOff int64
	sample   uint64 // samples in the frames so far
	// points is the rest of the seek table. Each point's sample number, offset and
	// block size must be what its frame says, which leaves no room to stash anything
	// in the table, and real encoders fill it in from the frames in just this way.
	points []byte
}

// walk returns the end of the run of frames starting at pos.
func (f *frameWalker) walk(pos int64) (int64, error) {
	for pos < f.tagStart {
		header := f.win.AtMost(pos, maxFrameHeaderLen)
		if frameHeaderLen(header) == 0 {
			break
		}
		samples := uint64(frameBlockSize(header))
		end, ok, err := f.frameEnd(pos)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		f.matchSeekPoint(uint64(pos-f.audioOff), samples)
		f.sample += samples
		pos = end
	}
	return pos, f.win.Err()
}

// matchSeekPoint ticks off the next seek point if it describes the frame at offset.
func (f *frameWalker) matchSeekPoint(offset, samples uint64) {
	if len(f.points) < seekPointLen {
		return
	}
	point := f.points[:seekPointLen]
	if binary.BigEndian.Uint64(point) == f.sample && binary.BigEndian.Uint64(point[8:]) == offset &&
		uint64(binary.BigEndian.Uint16(point[16:])) == samples {
		f.points = f.points[seekPointLen:]
	}
}

// allPointsFound reports whether every seek point described a frame, leaving only
// placeholders, which normalizeSeekTable has checked come last.
func (f *frameWalker) allPointsFound() bool {
	return len(f.points) < seekPointLen || binary.BigEndian.Uint64(f.points) == seekPlaceholder
}

// frameEnd finds the end of the frame starting at start. It reports false when no
// point in the rest of the file can be its end, as for a truncated last frame.
func (f *frameWalker) frameEnd(start int64) (int64, bool, error) {
	size := f.win.Size()
	var crc uint16
	var zeros int
	var firstZero int64
	for pos := start; pos < size; {
		if err := f.ctx.Err(); err != nil {
			return 0, false, err
		}
		chunk := f.win.From(pos)
		if chunk == nil {
			return 0, false, f.win.Err()
		}
		advanced := int64(len(chunk))
		for i, c := range chunk {
			crc = crc<<8 ^ crc16Table[byte(crc>>8)^c]
			q := pos + int64(i) + 1
			if crc != 0 || q-start < minFrameLen {
				continue
			}
			if f.endsFrame(q) {
				return q, true, nil
			}
			if q <= f.tagStart {
				if zeros == 0 {
					firstZero = q
				}
				zeros++
			}
			// endsFrame may have moved the window under chunk.
			advanced = int64(i) + 1
			break
		}
		pos += advanced
	}
	if err := f.win.Err(); err != nil {
		return 0, false, err
	}
	switch zeros {
	case 0:
		return 0, false, nil
	case 1:
		return firstZero, true, nil // the last frame, followed by unrecognised bytes
	}
	// Any of these points could be the true end, and guessing wrong either keeps
	// junk or drops audio.
	return 0, false, core.Malformedf("flac: cannot tell where the audio frame at offset %d ends", start)
}

// endsFrame reports whether what follows q is what follows a frame: another
// frame, a tag, or the end of the file. Reaching the end of the file with a good
// CRC outranks a tag signature before it, because the CRC is what proves those
// bytes belong to the frame and a signature could be there by chance.
func (f *frameWalker) endsFrame(q int64) bool {
	if q == f.tagStart || q == f.win.Size() {
		return true
	}
	if frameHeaderLen(f.win.AtMost(q, maxFrameHeaderLen)) > 0 {
		return true
	}
	_, ok := audiotag.TagAt(f.win, q, f.win.Size())
	return ok
}

// headerIn looks for a valid frame header in the n bytes at off.
func (f *frameWalker) headerIn(off, n int64) (bool, error) {
	for end := off + n; off < end; {
		if err := f.ctx.Err(); err != nil {
			return false, err
		}
		chunk := f.win.From(off)
		if chunk == nil {
			return false, f.win.Err()
		}
		chunk = chunk[:min(int64(len(chunk)), end-off)]
		advanced := int64(len(chunk))
		for i, c := range chunk {
			if c != 0xFF {
				continue
			}
			if frameHeaderLen(f.win.AtMost(off+int64(i), maxFrameHeaderLen)) > 0 {
				return true, nil
			}
			advanced = int64(i) + 1 // AtMost may have moved the window under chunk
			break
		}
		off += advanced
	}
	return false, f.win.Err()
}
