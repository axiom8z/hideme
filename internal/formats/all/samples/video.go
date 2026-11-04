package samples

import (
	"encoding/binary"
	"slices"

	"github.com/axiom8z/hideme/internal/coretest"
)

func box(typ string, parts ...[]byte) []byte {
	body := cat(parts...)
	return cat(be32(uint32(len(body)+8)), []byte(typ), body)
}

func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	return box(typ, append([][]byte{be32(uint32(version)<<24 | flags)}, parts...)...)
}

// qtText is a QuickTime international text atom's payload.
func qtText(s string) []byte {
	return cat(be16(uint16(len(s))), be16(0x15C7), []byte(s))
}

// MP4 returns a one-track movie whose user data holds a location and an author.
func MP4() Sample {
	sample := []byte("VIDEO SAMPLE BYTES MUST SURVIVE")
	const captureTime = 3064017906 // 2001-02-03 in the 1904 epoch
	ftyp := box("ftyp", []byte("isom"), be32(512), []byte("isomiso2avc1mp41"))
	build := func(mdatPayload uint32) []byte {
		entry := box("avc1", make([]byte, 78), box("avcC", []byte{1, 2, 3, 4}))
		stbl := box("stbl",
			fullBox("stsd", 0, 0, be32(1), entry),
			fullBox("stts", 0, 0, be32(1), be32(1), be32(1)),
			fullBox("stsc", 0, 0, be32(1), be32(1), be32(1), be32(1)),
			fullBox("stsz", 0, 0, be32(0), be32(1), be32(uint32(len(sample)))),
			fullBox("stco", 0, 0, be32(1), be32(mdatPayload)))
		minf := box("minf",
			fullBox("vmhd", 0, 1, make([]byte, 8)),
			box("dinf", fullBox("dref", 0, 0, be32(1), fullBox("url ", 0, 1))),
			stbl)
		mdia := box("mdia",
			fullBox("mdhd", 0, 0, be32(captureTime), be32(captureTime), be32(600), be32(100), be16(0x55C4), be16(0)),
			fullBox("hdlr", 0, 0, []byte("mhlr"), []byte("vide"), []byte("appl"), make([]byte, 8), []byte("Test Handler Name\x00")),
			minf)
		trak := box("trak",
			fullBox("tkhd", 0, 7, be32(captureTime), be32(captureTime), be32(1), make([]byte, 68)),
			mdia)
		udta := box("udta",
			box("\xa9xyz", qtText("+48.8584+002.2945+035.000/")),
			box("\xa9ART", qtText(Author)),
			box("\xa9nam", qtText(Title)))
		moov := box("moov",
			fullBox("mvhd", 0, 0, be32(captureTime), be32(captureTime), be32(600), be32(100), make([]byte, 80)),
			trak, udta)
		return cat(ftyp, moov, box("mdat", sample))
	}
	// The moov's size does not depend on the offset inside it, so measuring with a
	// placeholder and building again gives the real offset.
	placeholder := build(0)
	data := build(uint32(len(placeholder) - len(sample)))
	return Sample{
		Name:    "clip.mp4",
		Handler: "mp4",
		Data:    data,
		Secrets: text(Author, Title, "+48.8584", "Test Handler Name"),
		Content: sample,
	}
}

// HEIC returns a one-image file with EXIF stored as an item.
func HEIC() Sample {
	picture := []byte("PRIMARY IMAGE BYTES MUST SURVIVE")
	exif := append(make([]byte, 4), coretest.FakeEXIF(0)...) // a zero TIFF header offset
	ftyp := box("ftyp", []byte("heic"), be32(0), []byte("mif1"), []byte("heic"))
	infe := func(id uint16, typ string) []byte {
		return fullBox("infe", 2, 0, be16(id), be16(0), []byte(typ), []byte{0})
	}
	build := func(mdatPayload uint32) []byte {
		location := func(id uint16, off uint32, length int) []byte {
			return cat(be16(id), be16(0), be16(1), be32(off), be32(uint32(length)))
		}
		meta := fullBox("meta", 0, 0,
			fullBox("hdlr", 0, 0, be32(0), []byte("pict"), make([]byte, 12), []byte("Test Handler Name\x00")),
			fullBox("pitm", 0, 0, be16(1)),
			fullBox("iloc", 0, 0, []byte{0x44, 0x00}, be16(2),
				location(1, mdatPayload, len(picture)),
				location(2, mdatPayload+uint32(len(picture)), len(exif))),
			fullBox("iinf", 0, 0, be16(2), infe(1, "hvc1"), infe(2, "Exif")),
			box("iprp",
				box("ipco", box("hvcC", []byte("PRIMARY DECODER CONFIG"))),
				fullBox("ipma", 0, 0, be32(1), be16(1), []byte{1, 0x81})))
		return cat(ftyp, meta, box("mdat", picture, exif))
	}
	placeholder := build(0)
	data := build(uint32(len(placeholder) - len(picture) - len(exif)))
	return Sample{
		Name:    "photo.heic",
		Handler: "heif",
		Data:    data,
		Secrets: slices.Concat(coretest.FakeEXIFSecrets(), text("Test Handler Name")),
		Content: picture,
	}
}

func ebmlID(id uint32) []byte {
	raw := be32(id)
	for len(raw) > 1 && raw[0] == 0 {
		raw = raw[1:]
	}
	return raw
}

func ebmlSize(n int) []byte {
	for width := 1; width <= 8; width++ {
		if int64(n) <= int64(1)<<(7*width)-2 {
			raw := binary.BigEndian.AppendUint64(nil, uint64(n)|uint64(1)<<(7*width))
			return raw[8-width:]
		}
	}
	panic("EBML size too large")
}

func element(id uint32, parts ...[]byte) []byte {
	body := cat(parts...)
	return cat(ebmlID(id), ebmlSize(len(body)), body)
}

func ebmlUint(id uint32, v byte) []byte { return element(id, []byte{v}) }

// Matroska returns a one-cluster file with tags and a title.
func Matroska() Sample {
	const (
		idEBML      = 0x1A45DFA3
		idDocType   = 0x4282
		idSegment   = 0x18538067
		idInfo      = 0x1549A966
		idTimeScale = 0x2AD7B1
		idMuxingApp = 0x4D80
		idWriteApp  = 0x5741
		idTitle     = 0x7BA9
		idTracks    = 0x1654AE6B
		idEntry     = 0xAE
		idNumber    = 0xD7
		idUID       = 0x73C5
		idType      = 0x83
		idCodec     = 0x86
		idCluster   = 0x1F43B675
		idTimestamp = 0xE7
		idBlock     = 0xA3
		idTags      = 0x1254C367
		idTag       = 0x7373
		idSimple    = 0x67C8
		idTagName   = 0x45A3
		idTagString = 0x4487
	)
	media := []byte("MEDIA BYTES MUST SURVIVE")
	header := element(idEBML,
		ebmlUint(0x4286, 1), ebmlUint(0x42F7, 1), ebmlUint(0x42F2, 4), ebmlUint(0x42F3, 8),
		element(idDocType, []byte("matroska")), ebmlUint(0x4287, 4), ebmlUint(0x4285, 2))
	segment := element(idSegment,
		element(idInfo,
			element(idTimeScale, []byte{0x0F, 0x42, 0x40}),
			element(idMuxingApp, []byte("Test Muxer")),
			element(idWriteApp, []byte("Test Writer")),
			element(idTitle, []byte(Title))),
		element(idTracks, element(idEntry,
			ebmlUint(idNumber, 1), ebmlUint(idUID, 7), ebmlUint(idType, 1),
			element(idCodec, []byte("V_VP9")))),
		// A block starts with its track number, a two-byte time and its flags.
		element(idCluster, ebmlUint(idTimestamp, 0), element(idBlock, cat([]byte{0x81, 0, 0, 0x80}, media))),
		element(idTags, element(idTag, element(idSimple,
			element(idTagName, []byte("ARTIST")),
			element(idTagString, []byte(Author))))))
	return Sample{
		Name:    "movie.mkv",
		Handler: "matroska",
		Data:    cat(header, segment),
		Secrets: text(Author, Title, "Test Muxer", "Test Writer"),
		Content: media,
	}
}

// AVI returns a minimal file with INFO tags and a capture date.
func AVI() Sample {
	chunk := func(id string, parts ...[]byte) []byte {
		body := cat(parts...)
		out := cat([]byte(id), le32(uint32(len(body))), body)
		if len(body)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	list := func(typ string, children ...[]byte) []byte {
		return chunk("LIST", []byte(typ), cat(children...))
	}
	frame := []byte("VIDEO FRAME BYTES MUST SURVIVE")
	strl := list("strl", chunk("strh", []byte("vidsSTREAM-HEADER")), chunk("strf", []byte("STREAM-FORMAT")))
	hdrl := list("hdrl", chunk("avih", []byte("MAIN-AVI-HEADER1")), strl, chunk("IDIT", []byte("MON JAN 01 12:00:00 2001\n\x00")))
	info := list("INFO", chunk("IART", []byte(Author+"\x00")), chunk("INAM", []byte(Title+"\x00")))
	body := cat([]byte("AVI "), hdrl, info, list("movi", chunk("00dc", frame)), chunk("idx1", []byte("INDEX-ENTRY")))
	return Sample{
		Name:    "movie.avi",
		Handler: "avi",
		Data:    chunk("RIFF", body),
		Secrets: text(Author, Title, "JAN 01"),
		Content: frame,
	}
}
