package mp3

import (
	"bytes"
	"encoding/binary"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	headerLen = 4

	versionMPEG25 = 0
	versionMPEG2  = 2
	versionMPEG1  = 3
)

// frameHeader is a decoded MPEG audio frame header.
type frameHeader struct {
	version   uint32
	layer     int // 1, 2 or 3
	rate      int // samples per second
	mono      bool
	protected bool // a 16-bit CRC follows the header
	length    int64
}

// compatible reports whether two frames could belong to one stream. Bit rate and
// channel mode vary freely from frame to frame, so only these can be compared.
func (h frameHeader) compatible(o frameHeader) bool {
	return h.version == o.version && h.layer == o.layer && h.rate == o.rate
}

type headerStatus int

const (
	headerInvalid headerStatus = iota
	// headerFree is a valid header using the free-format bit rate, whose frame
	// length is not stated anywhere and can only be found by searching for the
	// next sync. Such streams are too rare to justify that guesswork.
	headerFree
	headerOK
)

var (
	// bitratesV1 and bitratesV2 give kilobits per second, indexed by layer (minus
	// one) and then by the header's bit rate index (minus one).
	bitratesV1 = [3][14]int{
		{32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	}
	bitratesV2 = [3][14]int{
		{32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	}
	sampleRates = [3]int{44100, 48000, 32000}
)

// parseHeader decodes the frame header at the start of b. A reserved value in any
// field that decides how the frame is laid out makes the header invalid, which is
// most of the defence against mistaking other data for audio. The emphasis field does
// not count: it only tells a player how to filter the output, and decoders such as
// ffmpeg and mpg123 play frames with the reserved value in it.
func parseHeader(b []byte) (frameHeader, headerStatus) {
	if len(b) < headerLen {
		return frameHeader{}, headerInvalid
	}
	raw := binary.BigEndian.Uint32(b)
	if raw>>21 != 0x7FF {
		return frameHeader{}, headerInvalid
	}
	version := raw >> 19 & 3
	layerBits := raw >> 17 & 3
	bitrateIndex := raw >> 12 & 15
	rateIndex := raw >> 10 & 3
	if version == 1 || layerBits == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return frameHeader{}, headerInvalid
	}
	if bitrateIndex == 0 {
		return frameHeader{}, headerFree
	}

	layer := int(4 - layerBits)
	rate := sampleRates[rateIndex]
	bitrate := bitratesV1[layer-1][bitrateIndex-1]
	switch version {
	case versionMPEG2:
		rate /= 2
		bitrate = bitratesV2[layer-1][bitrateIndex-1]
	case versionMPEG25:
		rate /= 4
		bitrate = bitratesV2[layer-1][bitrateIndex-1]
	}

	padding := int64(raw >> 9 & 1)
	var length int64
	switch {
	case layer == 1:
		length = (int64(12*bitrate*1000/rate) + padding) * 4
	case layer == 3 && version != versionMPEG1:
		length = int64(72*bitrate*1000/rate) + padding
	default:
		length = int64(144*bitrate*1000/rate) + padding
	}
	return frameHeader{
		version:   version,
		layer:     layer,
		rate:      rate,
		mono:      raw>>6&3 == 3,
		protected: raw>>16&1 == 0,
		length:    length,
	}, headerOK
}

// sideInfoLen returns the size of Layer III side information, after which a Xing
// or Info header sits in a frame that holds no audio.
func (h frameHeader) sideInfoLen() int {
	switch {
	case h.version == versionMPEG1 && h.mono:
		return 17
	case h.version == versionMPEG1:
		return 32
	case h.mono:
		return 9
	}
	return 17
}

// seekHeader is what a stream's first frame holds instead of audio, if it is one of
// the seek frames encoders write.
type seekHeader struct {
	kind    string // "Xing" or "VBRI"
	encoder string // what it says about the encoder; empty if nothing
	size    int64  // the audio size it states; 0 if none
	end     int    // where its fields end. Nothing reads the frame past this point.
	// silent says the side information ahead of the header is empty, as in every seek
	// frame. Without it, the frame could be audio that reads like one by chance.
	silent bool
}

// The layouts of these headers come from no standard, only from what the encoders
// that introduced them write and what players have since agreed to read.
const (
	xingEncoderLen = 9
	// xingExtensionLen is the encoder string and the LAME fields after it, ending
	// with the tag's own CRC.
	xingExtensionLen = 36

	vbriOffset    = 36
	vbriSizeOff   = 10
	vbriCountOff  = 18
	vbriWidthOff  = 22
	vbriHeaderLen = 26
)

// parseSeekHeader reads the seek header in a stream's first frame, if there is one.
func parseSeekHeader(frame []byte, h frameHeader) (seekHeader, bool) {
	side := headerLen
	if h.protected {
		side += 2 // the CRC
	}
	if len(frame) >= vbriOffset+4 && string(frame[vbriOffset:vbriOffset+4]) == "VBRI" {
		seek := parseVBRI(frame)
		seek.silent = core.AllZero(frame[side:vbriOffset])
		return seek, true
	}
	off := side + h.sideInfoLen()
	if len(frame) < off+8 {
		return seekHeader{}, false
	}
	if magic := string(frame[off : off+4]); magic != "Xing" && magic != "Info" {
		return seekHeader{}, false
	}
	seek := seekHeader{kind: "Xing", silent: core.AllZero(frame[side:off])}
	flags := binary.BigEndian.Uint32(frame[off+4:])
	off += 8
	// Optional fields: frame count, byte count, seek table and quality.
	for _, field := range []struct {
		bit  uint32
		size int
	}{{1, 4}, {2, 4}, {4, 100}, {8, 4}} {
		if flags&field.bit == 0 {
			continue
		}
		if field.bit == 2 && len(frame) >= off+field.size {
			seek.size = int64(binary.BigEndian.Uint32(frame[off:]))
		}
		off += field.size
	}
	seek.end = min(off, len(frame))
	if len(frame) >= off+xingEncoderLen {
		if text, ok := encoderString(frame[off : off+xingEncoderLen]); ok {
			seek.encoder = text
			seek.end = min(off+xingExtensionLen, len(frame))
		}
	}
	return seek, true
}

// parseVBRI reads a VBRI header (Fraunhofer), which holds no version string, but
// whose presence still identifies the encoder family.
func parseVBRI(frame []byte) seekHeader {
	seek := seekHeader{kind: "VBRI", encoder: "VBRI seek header (Fraunhofer encoder)", end: len(frame)}
	head := frame[vbriOffset:]
	if len(head) < vbriHeaderLen {
		return seek
	}
	seek.size = int64(binary.BigEndian.Uint32(head[vbriSizeOff:]))
	table := int(binary.BigEndian.Uint16(head[vbriCountOff:])) * int(binary.BigEndian.Uint16(head[vbriWidthOff:]))
	seek.end = min(vbriOffset+vbriHeaderLen+table, len(frame))
	return seek
}

// encoderString decodes the version string in a Xing header. Anything unprintable
// means it is not one, just the rest of an empty frame.
func encoderString(b []byte) (string, bool) {
	text := bytes.TrimRight(b, "\x00 ")
	for _, c := range text {
		if c < 0x20 || c > 0x7E {
			return "", false
		}
	}
	return string(text), len(text) > 0
}

// encoderTag extracts what the seek header in a stream's first frame says about
// the encoder.
func encoderTag(frame []byte, h frameHeader) (string, bool) {
	seek, ok := parseSeekHeader(frame, h)
	return seek.encoder, ok && seek.encoder != ""
}
