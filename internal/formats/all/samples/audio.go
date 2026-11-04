package samples

import (
	"encoding/binary"
	"slices"

	"github.com/axiom8z/hideme/internal/audiotag/tagtest"
)

// MP3 returns MPEG frames between an ID3v2 tag and an ID3v1 tag.
func MP3() Sample {
	const frameLen = 417 // 128 kbit/s, 44.1 kHz, MPEG-1 Layer III
	var audio []byte
	for i := range 8 {
		frame := pattern(i, frameLen)
		copy(frame, []byte{0xFF, 0xFB, 0x90, 0xC4})
		audio = append(audio, frame...)
	}
	return Sample{
		Name:    "song.mp3",
		Handler: "mp3",
		Data:    cat(tagtest.ID3v23(), audio, tagtest.ID3v1()),
		Secrets: slices.Concat(tagtest.ID3Secrets(), text(tagtest.Fake)),
		Content: audio,
	}
}

// crc8 and crc16 are FLAC's frame checksums.
func crc8(b []byte) byte {
	var c byte
	for _, v := range b {
		c ^= v
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func crc16(b []byte) uint16 {
	var c uint16
	for _, v := range b {
		c ^= uint16(v) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// FLAC returns stand-in frames with valid checksums after a tagged metadata chain.
func FLAC() Sample {
	const blockStreamInfo, blockVorbis, lastBlock = 0, 4, 0x80
	var audio []byte
	for i := range 6 {
		frame := []byte{0xFF, 0xF8, 0xC0, 0x00, byte(i)}
		frame = append(frame, crc8(frame))
		frame = append(frame, pattern(i, 200)...)
		audio = append(audio, binary.BigEndian.AppendUint16(frame, crc16(frame))...)
	}
	info := make([]byte, 34)
	binary.BigEndian.PutUint16(info, 4096)
	binary.BigEndian.PutUint16(info[2:], 4096)
	info[10], info[11], info[12] = 0x0A, 0xC4, 0x40 // 44.1 kHz, mono, 16 bit
	block := func(kind byte, body []byte) []byte {
		n := len(body)
		return cat([]byte{kind, byte(n >> 16), byte(n >> 8), byte(n)}, body)
	}
	comment := tagtest.VorbisComment("Test Vendor", "TITLE="+Title, "ARTIST="+tagtest.Fake)
	return Sample{
		Name:    "song.flac",
		Handler: "flac",
		Data: cat([]byte("fLaC"),
			block(blockStreamInfo, info),
			block(blockVorbis|lastBlock, comment),
			audio),
		Secrets: text(tagtest.Fake, Title, "Test Vendor"),
		Content: audio,
	}
}

func oggCRC(b []byte) uint32 {
	var c uint32
	for _, v := range b {
		c ^= uint32(v) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// Ogg returns an Opus stream with stand-in audio packets.
func Ogg() Sample {
	const flagBOS, flagEOS, serial = 0x02, 0x04, 0x11111111
	page := func(flags byte, granule uint64, seq uint32, packets ...[]byte) []byte {
		var lacing []byte
		for _, p := range packets {
			n := len(p)
			for ; n >= 255; n -= 255 {
				lacing = append(lacing, 255)
			}
			lacing = append(lacing, byte(n))
		}
		out := []byte("OggS\x00")
		out = append(out, flags)
		out = binary.LittleEndian.AppendUint64(out, granule)
		out = binary.LittleEndian.AppendUint32(out, serial)
		out = binary.LittleEndian.AppendUint32(out, seq)
		out = binary.LittleEndian.AppendUint32(out, 0)
		out = append(out, byte(len(lacing)))
		out = append(out, lacing...)
		out = append(out, cat(packets...)...)
		binary.LittleEndian.PutUint32(out[22:], oggCRC(out))
		return out
	}
	head := cat([]byte("OpusHead\x01\x01"), le16(312), le32(48000), []byte{0, 0, 0})
	tags := cat([]byte("OpusTags"), tagtest.VorbisComment("Test Vendor", "TITLE="+Title, "ARTIST="+tagtest.Fake))
	audio := pattern(3, 100)
	return Sample{
		Name:    "voice.opus",
		Handler: "ogg",
		Data: cat(
			page(flagBOS, 0, 0, head),
			page(0, 0, 1, tags),
			page(0, 960, 2, audio, pattern(4, 100)),
			page(flagEOS, 1920, 3, pattern(5, 100))),
		Secrets: text(tagtest.Fake, Title, "Test Vendor"),
		Content: audio,
	}
}

// WAV returns PCM with a LIST INFO chunk.
func WAV() Sample {
	chunk := func(id string, body []byte) []byte {
		out := cat([]byte(id), le32(uint32(len(body))), body)
		if len(body)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	format := cat(le16(1), le16(1), le32(44100), le32(88200), le16(2), le16(16))
	samples := pattern(1, 400)
	info := cat([]byte("INFO"),
		chunk("IART", []byte(tagtest.Fake+"\x00")),
		chunk("INAM", []byte(Title+"\x00")))
	body := cat([]byte("WAVE"), chunk("fmt ", format), chunk("LIST", info), chunk("data", samples))
	return Sample{
		Name:    "voice.wav",
		Handler: "wav",
		Data:    cat([]byte("RIFF"), le32(uint32(len(body))), body),
		Secrets: text(tagtest.Fake, Title, "INFO"),
		Content: samples,
	}
}

// AIFF returns big-endian PCM with name and author chunks.
func AIFF() Sample {
	chunk := func(id string, body []byte) []byte {
		out := cat([]byte(id), be32(uint32(len(body))), body)
		if len(body)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	samples := pattern(2, 400)
	comm := cat(be16(1), be32(uint32(len(samples)/2)), be16(16), []byte{0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0})
	body := cat([]byte("AIFF"),
		chunk("COMM", comm),
		chunk("NAME", []byte(Title)),
		chunk("AUTH", []byte(tagtest.Fake)),
		chunk("SSND", cat(make([]byte, 8), samples)))
	return Sample{
		Name:    "voice.aiff",
		Handler: "aiff",
		Data:    cat([]byte("FORM"), be32(uint32(len(body))), body),
		Secrets: text(tagtest.Fake, Title),
		Content: samples,
	}
}
