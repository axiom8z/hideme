package matroska

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/textscan"
)

const (
	codecVorbis = "A_VORBIS"
	codecTheora = "V_THEORA"
	codecFLAC   = "A_FLAC"

	// maxCodecHeaderRead bounds how much of a codec's private data is examined. The
	// structure to find is at its start, and the size of a comment's text only
	// matters to the report: every byte of it gets zeroed regardless.
	maxCodecHeaderRead = 1 << 20

	flacHeaderSize = 4

	flacStreamInfo    = 0
	flacPadding       = 1
	flacSeekTable     = 3
	flacVorbisComment = 4
	flacPicture       = 6
)

var flacMagic = []byte("fLaC")

// commentHeader describes the comment packet of a Xiph codec.
type commentHeader struct {
	magic   string
	framing bool // a final byte with the framing bit set, as Vorbis requires
}

var (
	vorbisComment = commentHeader{magic: "\x03vorbis", framing: true}
	theoraComment = commentHeader{magic: "\x81theora"}
)

// codecPrivate cleans what some codecs carry in a track's private data, which looks
// like an opaque blob but can hold the tags of the file it came from: remuxing a
// tagged Ogg or FLAC file into Matroska keeps its comment header there, including
// any artist, location or embedded picture. Everything is done in place, because
// the private data is copied byte for byte by every tool, and its size is fixed.
//
// Codecs are matched by prefix, because that is how ffmpeg matches them: a suffix
// must not get a comment header past this check.
//
// Private data of other codecs stays, because decoders need it, and gets a warning
// if it visibly holds text.
func (p *plan) codecPrivate(e element, where string) error {
	where += "/CodecPrivate"
	switch {
	case strings.HasPrefix(p.trackCodec, codecVorbis):
		return p.xiphComments(e, where, vorbisComment)
	case strings.HasPrefix(p.trackCodec, codecTheora):
		return p.xiphComments(e, where, theoraComment)
	case strings.HasPrefix(p.trackCodec, codecFLAC):
		return p.flacBlocks(e, where)
	case isStyledSubtitle(p.trackCodec):
		return p.styledSubtitleHeader(e, where)
	case p.trackPrivateEncoded:
		p.warn(warnEncodedPrivate, "A track's codec private data is stored compressed. It was kept and could not be inspected.")
		return nil
	}
	return p.opaque(e, where)
}

// opaque keeps data decoders need, and warns if it visibly holds text. It looks no
// further than maxCodecHeaderRead, so anything longer gets the warning unread.
func (p *plan) opaque(e element, _ string) error {
	if e.size > maxCodecHeaderRead || textscan.Has(p.r.peek(e, maxCodecHeaderRead)) {
		p.warn(warnOpaqueText, "Data kept for decoders, such as codec private data, holds readable text or is too large to check. It stays in the file.")
	}
	return nil
}

// xiphComments blanks the comment packet among a Xiph codec's three headers, whose
// length must not change because the private data stores the lengths of the first
// two. The bytes it leaves are a valid empty comment header followed by zeros, and
// decoders ignore what follows the part they read.
func (p *plan) xiphComments(e element, where string, h commentHeader) error {
	raw := p.r.peek(e, maxCodecHeaderRead)
	start, length, err := xiphCommentPacket(raw, e.size)
	if err != nil {
		return err
	}
	blank := append([]byte(h.magic), make([]byte, 8)...)
	if h.framing {
		blank = append(blank, 1)
	}
	if length < int64(len(blank)) || start+int64(len(h.magic)) > int64(len(raw)) ||
		string(raw[start:start+int64(len(h.magic))]) != h.magic {
		return core.Malformedf("matroska: track at offset %d has an unrecognised comment header", e.off)
	}
	packet := raw[start:min(start+length, int64(len(raw)))]
	off := e.dataOff + start
	rest := int64(len(blank))
	zero, err := p.r.isZero(off+rest, length-rest)
	if err != nil {
		return err
	}
	// The other two headers stay, as the decoder needs them.
	if err := p.opaque(element{dataOff: e.dataOff, size: start}, where); err != nil {
		return err
	}
	after := start + length
	if err := p.opaque(element{dataOff: e.dataOff + after, size: e.size - after}, where); err != nil {
		return err
	}
	if zero && bytes.Equal(packet[:min(int64(len(packet)), rest)], blank) {
		return nil
	}
	if !p.describeComments(packet[len(h.magic):], where) {
		p.add(field{"Codec comment header", core.CatHidden, core.RiskMed, decodeSize}, core.ByteCount(length), where)
	}
	if err := p.patch(core.Patch{Off: off, Len: rest, Data: blank}); err != nil {
		return err
	}
	return p.patch(core.Patch{Off: off + rest, Len: length - rest, Zero: true})
}

// xiphCommentPacket finds the second of three headers stored with Xiph lacing: a
// packet count less one, then each length but the last as a run of bytes that ends
// with the first below 255.
func xiphCommentPacket(raw []byte, size int64) (start, length int64, err error) {
	if len(raw) == 0 || raw[0] != 2 {
		return 0, 0, core.Malformedf("matroska: codec private data does not hold three laced headers")
	}
	i := 1
	var lengths [2]int64
	for n := range lengths {
		for {
			if i >= len(raw) {
				return 0, 0, core.Malformedf("matroska: truncated header lengths in codec private data")
			}
			b := raw[i]
			i++
			lengths[n] += int64(b)
			if b < 255 {
				break
			}
		}
	}
	start = int64(i) + lengths[0]
	if start > size || lengths[1] > size-start {
		return 0, 0, core.Malformedf("matroska: laced headers overrun the codec private data")
	}
	return start, lengths[1], nil
}

// describeComments reports the vendor string and fields of a Vorbis comment block.
// It says whether it found anything to name. Running out of bytes only stops the
// description: the whole block goes, and a large picture may lie past what was read.
func (p *plan) describeComments(b []byte, where string) bool {
	found := false
	if len(b) < 4 {
		return false
	}
	vendorLen := uint64(binary.LittleEndian.Uint32(b))
	b = b[4:]
	if vendorLen > uint64(len(b)) {
		return false
	}
	if vendorLen > 0 {
		p.add(field{"Codec vendor", core.CatSoftware, core.RiskLow, decodeText}, string(b[:vendorLen]), where)
		found = true
	}
	b = b[vendorLen:]
	if len(b) < 4 {
		return found
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	for range count {
		if len(b) < 4 {
			break
		}
		n := uint64(binary.LittleEndian.Uint32(b))
		b = b[4:]
		if n > uint64(len(b)) {
			break
		}
		name, value, _ := strings.Cut(string(b[:n]), "=")
		p.addTag(name, value, where)
		found = true
		b = b[n:]
	}
	return found
}

// flacBlocks turns every metadata block a decoder does not need into padding. It
// keeps its length, so the blocks after it stay where they are, and the last-block
// flag stays too, so the chain still ends in the same place.
func (p *plan) flacBlocks(e element, where string) error {
	if e.size < int64(len(flacMagic)) || !bytes.Equal(p.r.peek(e, int64(len(flacMagic))), flacMagic) {
		return core.Malformedf("matroska: track at offset %d has FLAC private data with no signature", e.off)
	}
	end := e.end()
	pos := e.dataOff + int64(len(flacMagic))
	for pos < end {
		if end-pos < flacHeaderSize {
			return core.Malformedf("matroska: truncated FLAC metadata block at offset %d", pos)
		}
		var header [flacHeaderSize]byte
		if err := p.r.read(header[:], pos); err != nil {
			return err
		}
		last := header[0]&0x80 != 0
		kind := header[0] & 0x7F
		body := pos + flacHeaderSize
		length := int64(header[1])<<16 | int64(header[2])<<8 | int64(header[3])
		if length > end-body {
			return core.Malformedf("matroska: FLAC metadata block at offset %d overruns the private data", pos)
		}
		if err := p.flacBlock(pos, body, length, kind, header[0]&0x80, where); err != nil {
			return err
		}
		pos = body + length
		if last {
			break
		}
	}
	return p.scrubRegion(pos, end-pos, where)
}

func (p *plan) flacBlock(pos, body, length int64, kind, lastFlag byte, where string) error {
	switch kind {
	case flacStreamInfo, flacSeekTable:
		return p.opaque(element{dataOff: body, size: length}, where)
	case flacPadding:
		return p.scrubRegion(body, length, where)
	case flacVorbisComment:
		raw := p.r.peek(element{dataOff: body, size: length}, maxCodecHeaderRead)
		if !p.describeComments(raw, where) {
			p.add(field{"Codec comment header", core.CatHidden, core.RiskMed, decodeSize}, core.ByteCount(length), where)
		}
	case flacPicture:
		p.add(field{"Embedded picture", core.CatThumbnail, core.RiskHigh, decodeSize}, core.ByteCount(length), where)
	default:
		p.add(field{"FLAC metadata block", core.CatHidden, core.RiskMed, decodeSize},
			"type "+strconv.Itoa(int(kind))+", "+core.ByteCount(length), where)
	}
	if err := p.patch(core.Patch{Off: pos, Len: 1, Data: []byte{lastFlag | flacPadding}}); err != nil {
		return err
	}
	return p.patch(core.Patch{Off: body, Len: length, Zero: true})
}

// scrubRegion zeroes bytes that have no purpose, reporting them if they held
// anything.
func (p *plan) scrubRegion(off, n int64, where string) error {
	zero, err := p.r.isZero(off, n)
	if err != nil || zero {
		return err
	}
	p.add(field{"Hidden data in codec private data", core.CatHidden, core.RiskMed, decodeSize}, core.ByteCount(n), where)
	return p.patch(core.Patch{Off: off, Len: n, Zero: true})
}
