package samples

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"slices"

	"github.com/axiom8z/hideme/internal/coretest"
)

func picture() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := range 16 {
		for x := range 16 {
			img.Set(x, y, color.NRGBA{uint8(x * 16), uint8(y * 16), 128, 255})
		}
	}
	return img
}

// JPEG returns an image from image/jpeg with EXIF, XMP and a comment spliced in.
func JPEG() Sample {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, picture(), nil); err != nil {
		panic(err)
	}
	segment := func(marker byte, payload []byte) []byte {
		return cat([]byte{0xFF, marker}, be16(uint16(len(payload)+2)), payload)
	}
	data := buf.Bytes()
	return Sample{
		Name:    "photo.jpg",
		Handler: "jpeg",
		Data: cat(data[:2],
			segment(0xE1, append([]byte("Exif\x00\x00"), coretest.FakeEXIF(6)...)),
			segment(0xE1, append([]byte("http://ns.adobe.com/xap/1.0/\x00"), coretest.FakeXMP()...)),
			segment(0xFE, []byte("Test JPEG Comment")),
			data[2:]),
		Secrets: slices.Concat(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), text("Test JPEG Comment")),
	}
}

// PNG returns an image from image/png with text chunks added before IEND.
func PNG() Sample {
	var buf bytes.Buffer
	if err := png.Encode(&buf, picture()); err != nil {
		panic(err)
	}
	chunk := func(typ string, body []byte) []byte {
		crc := crc32.ChecksumIEEE(append([]byte(typ), body...))
		return cat(be32(uint32(len(body))), []byte(typ), body, be32(crc))
	}
	data := buf.Bytes()
	end := len(data) - 12
	return Sample{
		Name:    "picture.png",
		Handler: "png",
		Data: cat(data[:end],
			chunk("tEXt", []byte("Author\x00"+Author)),
			chunk("tEXt", []byte("Software\x00Test Software")),
			data[end:]),
		Secrets: text(Author, "Test Software", "tEXt"),
	}
}

// GIF returns an image from image/gif with a comment added before the trailer.
func GIF() Sample {
	img := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		panic(err)
	}
	const comment = "Test GIF Comment"
	data := buf.Bytes()
	end := len(data) - 1
	return Sample{
		Name:    "animation.gif",
		Handler: "gif",
		Data: cat(data[:end],
			[]byte{0x21, 0xFE, byte(len(comment))}, []byte(comment), []byte{0},
			data[end:]),
		Secrets: text(comment),
	}
}

// WebP returns a VP8X file around a stand-in bitstream. The standard library has no
// WebP encoder, and the handler keeps the bitstream without decoding it.
func WebP() Sample {
	chunk := func(id string, body []byte) []byte {
		out := cat([]byte(id), le32(uint32(len(body))), body)
		if len(body)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	const flagEXIF = 0x08
	bits := []byte{0x10, 0, 0, 0x9D, 0x01, 0x2A, 16, 0, 16, 0, 'B', 'I', 'T', 'S'}
	body := cat([]byte("WEBP"),
		chunk("VP8X", []byte{flagEXIF, 0, 0, 0, 15, 0, 0, 15, 0, 0}),
		chunk("VP8 ", bits),
		chunk("EXIF", coretest.FakeEXIF(0)))
	return Sample{
		Name:    "picture.webp",
		Handler: "webp",
		Data:    cat([]byte("RIFF"), le32(uint32(len(body))), body),
		Secrets: coretest.FakeEXIFSecrets(),
		Content: bits,
	}
}

// TIFF returns a one-page grey image with EXIF and GPS directories.
func TIFF() Sample {
	pixels := pattern(1, 64)
	ifd := coretest.FakeIFD(0)
	ifd.Next = nil
	image := []coretest.Entry{
		{Tag: 256, Value: uint32(8)},           // ImageWidth
		{Tag: 257, Value: uint32(8)},           // ImageLength
		{Tag: 258, Value: uint16(8)},           // BitsPerSample
		{Tag: 259, Value: uint16(1)},           // Compression: none
		{Tag: 262, Value: uint16(1)},           // Photometric: black is zero
		{Tag: 273, Value: uint32(0)},           // StripOffsets, filled in below
		{Tag: 277, Value: uint16(1)},           // SamplesPerPixel
		{Tag: 278, Value: uint32(8)},           // RowsPerStrip
		{Tag: 279, Value: uint32(len(pixels))}, // StripByteCounts
	}
	ifd.Entries = append(ifd.Entries, image...)
	// The offset fits in the entry itself, so setting it does not move anything.
	size := len(ifd.Encode(binary.LittleEndian))
	offset := size + size%2
	for i := range ifd.Entries {
		if ifd.Entries[i].Tag == 273 {
			ifd.Entries[i].Value = uint32(offset)
		}
	}
	data := ifd.Encode(binary.LittleEndian)
	if size%2 == 1 {
		data = append(data, 0)
	}
	return Sample{
		Name:    "scan.tif",
		Handler: "tiff",
		Data:    append(data, pixels...),
		Secrets: coretest.FakeEXIFSecrets(),
		Content: pixels,
	}
}
