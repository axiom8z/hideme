package tiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

var (
	littleEndian binary.ByteOrder = binary.LittleEndian
	bigEndian    binary.ByteOrder = binary.BigEndian
)

// pixels returns a distinctive picture so that a test can see whether its bytes
// survived. The seed keeps pages apart.
func pixels(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7) ^ seed
	}
	return out
}

// imageSpec describes one page for imageTags and build.
type imageSpec struct {
	width, height uint32
	samples       uint16
	rowsPerStrip  uint32
	tileWidth     uint32
	tileHeight    uint32
	planar        uint16
	compression   uint16
	photometric   uint16
	parts         [][]byte // strips or tiles
}

func (s imageSpec) tiled() bool {
	return s.tileWidth != 0
}

// imageTags returns the rendering tags for a page, with placeholder offsets that
// build fills in once it knows where the data goes.
func imageTags(s imageSpec) []coretest.Entry {
	samples := max(s.samples, 1)
	bits := make([]uint16, samples)
	for i := range bits {
		bits[i] = 8
	}
	compression := s.compression
	if compression == 0 {
		compression = compressionNone
	}
	photometric := s.photometric
	if photometric == 0 && samples == 3 {
		photometric = 2
	}
	if photometric == 0 && samples == 1 {
		photometric = 1
	}
	tags := []coretest.Entry{
		{Tag: tagImageWidth, Value: s.width},
		{Tag: tagImageLength, Value: s.height},
		{Tag: tagBitsPerSample, Value: bits},
		{Tag: tagCompression, Value: compression},
		{Tag: tagPhotometric, Value: photometric},
		{Tag: tagSamplesPerPixel, Value: samples},
		{Tag: tagResolutionUnit, Value: uint16(2)},
		{Tag: tagYResolution, Value: [][2]uint32{{300, 1}}},
	}
	if s.planar != 0 {
		tags = append(tags, coretest.Entry{Tag: tagPlanarConfig, Value: s.planar})
	}
	sizes := make([]uint32, len(s.parts))
	for i, part := range s.parts {
		sizes[i] = uint32(len(part))
	}
	offsets := make([]uint32, len(s.parts))
	if s.tiled() {
		return append(tags,
			coretest.Entry{Tag: tagTileWidth, Value: s.tileWidth},
			coretest.Entry{Tag: tagTileLength, Value: s.tileHeight},
			coretest.Entry{Tag: tagTileOffsets, Value: offsets},
			coretest.Entry{Tag: tagTileByteCounts, Value: sizes},
		)
	}
	return append(tags,
		coretest.Entry{Tag: tagRowsPerStrip, Value: s.rowsPerStrip},
		coretest.Entry{Tag: tagStripOffsets, Value: offsets},
		coretest.Entry{Tag: tagStripByteCounts, Value: sizes},
	)
}

// build encodes a directory tree and appends each page's strips or tiles, pointing
// the tables at them. coretest.IFD cannot do this itself because a strip offset is
// not known until the directories are laid out. gap goes in front of every part, so
// a test can plant data no tag refers to.
func build(order binary.ByteOrder, root *coretest.IFD, gap []byte, pages ...[][]byte) []byte {
	buf := root.Encode(order)
	off := order.Uint32(buf[4:])
	for _, parts := range pages {
		entries := int(order.Uint16(buf[off:]))
		for i := range entries {
			e := buf[int(off)+2+i*entrySize:]
			if tag := order.Uint16(e); tag != tagStripOffsets && tag != tagTileOffsets {
				continue
			}
			table := e[8:]
			if len(parts) > 1 {
				table = buf[order.Uint32(e[8:]):]
			}
			for j, part := range parts {
				buf = append(buf, gap...)
				order.PutUint32(table[4*j:], uint32(len(buf)))
				buf = append(buf, part...)
				// Appending may have moved the buffer.
				if len(parts) > 1 {
					table = buf[order.Uint32(buf[int(off)+2+i*entrySize+8:]):]
				} else {
					table = buf[int(off)+2+i*entrySize+8:]
				}
			}
			break
		}
		off = order.Uint32(buf[int(off)+2+entries*entrySize:])
	}
	return buf
}

// singlePage builds a one-page file with no metadata beyond extra.
func singlePage(order binary.ByteOrder, s imageSpec, extra ...coretest.Entry) []byte {
	root := &coretest.IFD{Entries: append(imageTags(s), extra...)}
	return build(order, root, nil, s.parts)
}

// plainSpec is a small two-strip RGB picture.
func plainSpec(seed byte) imageSpec {
	return imageSpec{
		width: 4, height: 4, samples: 3, rowsPerStrip: 2,
		parts: [][]byte{pixels(seed, 24), pixels(seed+1, 24)},
	}
}

// oddICC is a profile of odd length, which needs padding wherever it goes.
func oddICC() []byte {
	profile := append(fakeICC(), 'x')
	binary.BigEndian.PutUint32(profile, uint32(len(profile)))
	return profile
}

// fakeICC is enough of a colour profile to pass the signature and size checks.
func fakeICC() []byte {
	profile := bytes.Repeat([]byte("ICC-PROFILE-BYTES-"), 12)
	binary.BigEndian.PutUint32(profile, uint32(len(profile)))
	copy(profile[36:], "acsp")
	return profile
}

// fakeIPTC is an IIM stream holding the fields the report decodes.
func fakeIPTC() []byte {
	var out []byte
	add := func(dataset byte, value string) {
		out = append(out, jpegmeta.IPTCTagMarker, 2, dataset, byte(len(value)>>8), byte(len(value)))
		out = append(out, value...)
	}
	add(80, "Test IPTC Author")
	add(90, "Test IPTC City")
	add(116, "Test IPTC Copyright")
	add(120, "Test IPTC Caption")
	return out
}

// irb wraps data in one Photoshop image resource block.
func irb(id uint16, data []byte) []byte {
	out := []byte("8BIM")
	out = binary.BigEndian.AppendUint16(out, id)
	out = append(out, 0, 0) // an empty name, padded
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// fakePhotoshop holds the second copies of metadata Photoshop keeps, with fields
// and values that appear nowhere else so that a test knows this path was walked.
func fakePhotoshop() []byte {
	exifBlock := (&coretest.IFD{GPS: &coretest.IFD{Entries: []coretest.Entry{
		{Tag: 1, Value: "S"},
		{Tag: 2, Value: [][2]uint32{{33, 1}, {30, 1}, {0, 1}}},
		{Tag: 3, Value: "W"},
		{Tag: 4, Value: [][2]uint32{{70, 1}, {15, 1}, {0, 1}}},
	}}}).Encode(binary.LittleEndian)
	var out []byte
	out = append(out, irb(jpegmeta.ResourceIPTC, []byte{jpegmeta.IPTCTagMarker, 2, 122, 0, 18, 'T', 'e', 's', 't', ' ', 'I', 'R', 'B', ' ', 'W', 'r', 'i', 't', 'e', 'r', ' ', 'I', 'P'})...)
	out = append(out, irb(jpegmeta.ResourceThumbnail5, []byte("Test IRB Thumbnail"))...)
	out = append(out, irb(jpegmeta.ResourceExif1, exifBlock)...)
	out = append(out, irb(jpegmeta.ResourceXMP, []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:City="Test IRB City"/></rdf:RDF></x:xmpmeta>`))...)
	return out
}

// loadedSpec is the picture in loaded: two strips with a known pattern.
func loadedSpec() imageSpec { return plainSpec(0x40) }

// loaded builds a file carrying every kind of metadata the handler reports, at
// every level: EXIF tags, Exif and GPS sub-IFDs, an EXIF-style thumbnail, XMP, IPTC,
// Photoshop resources, GeoTIFF tags, and the retained orientation and ICC profile.
func loaded(order binary.ByteOrder) []byte {
	s := loadedSpec()
	root := coretest.FakeIFD(6)
	root.Entries = append(root.Entries, imageTags(s)...)
	root.Entries = append(root.Entries,
		coretest.Entry{Tag: tagICCProfile, Value: fakeICC()},
		coretest.Entry{Tag: tagXMP, Type: coretest.TypeByte, Value: coretest.FakeXMP()},
		coretest.Entry{Tag: tagIPTC, Value: fakeIPTC()},
		coretest.Entry{Tag: tagPhotoshop, Type: coretest.TypeByte, Value: fakePhotoshop()},
		coretest.Entry{Tag: tagPageName, Value: "Test Page Name"},
		coretest.Entry{Tag: tagTargetPrinter, Value: "Test Printer"},
		coretest.Entry{Tag: tagImageID, Value: `C:\Users\testuser\linked.tif`},
		coretest.Entry{Tag: tagImageSourceData, Value: []byte("Test Photoshop Layers")},
		coretest.Entry{Tag: 0x8482, Value: [][2]uint32{{1, 1}, {2, 1}, {3, 1}, {4, 1}, {5, 1}, {6, 1}}},
		coretest.Entry{Tag: 0x87B1, Value: "Test Geo Datum"},
	)
	return build(order, root, nil, s.parts)
}

// loadedFields lists what Inspect must report for loaded.
func loadedFields() []string {
	fields := append(coretest.FakeEXIFFields(), coretest.FakeXMPFields()...)
	return append(fields,
		"Embedded IPTC", "IPTC by-line", "IPTC city", "IPTC copyright notice", "IPTC caption", "IPTC writer",
		"Embedded Photoshop data", "Page name", "Target printer", "Linked image path",
		"Embedded Photoshop layers", "GeoTIFF location",
	)
}

// loadedSecrets lists what must not survive cleaning loaded.
func loadedSecrets() [][]byte {
	secrets := append(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets()...)
	for _, s := range []string{
		"Test IPTC", "Test IRB", "Test Page Name", "Test Printer", "testuser",
		"Test Photoshop Layers", "Test Geo Datum", "8BIM",
	} {
		secrets = append(secrets, []byte(s))
	}
	return secrets
}

// realJPEG encodes a small picture with the standard library, so that a JPEG strip
// in a test is something a real decoder can read.
func realJPEG(t testing.TB, seed byte) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = byte(i) ^ seed
	}
	img.SetGray(0, 0, color.Gray{Y: 0})
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// jpegSegment returns one marker segment with its length field filled in.
func jpegSegment(marker byte, payload []byte) []byte {
	length := len(payload) + 2
	return append([]byte{0xFF, marker, byte(length >> 8), byte(length)}, payload...)
}

// withSegments inserts segments right after a JPEG stream's SOI marker and adds
// trailer after its EOI.
func withSegments(stream, trailer []byte, segments ...[]byte) []byte {
	out := append([]byte{}, stream[:2]...)
	for _, s := range segments {
		out = append(out, s...)
	}
	out = append(out, stream[2:]...)
	return append(out, trailer...)
}

// decodedPage is a page decoded by readPages. Rows hold the image as a viewer would
// assemble it, so strips and tiles compare equal when the picture is the same.
type decodedPage struct {
	width, height int
	samples       int
	pix           []byte
}

// readPages is a small uncompressed TIFF decoder that shares nothing with the
// handler's parser. Comparing its output before and after cleaning proves the
// picture survived, without trusting the code under test to read its own output.
func readPages(data []byte) ([]decodedPage, error) {
	var order binary.ByteOrder
	switch string(data[:4]) {
	case magicLittle:
		order = binary.LittleEndian
	case magicBig:
		order = binary.BigEndian
	default:
		return nil, fmt.Errorf("bad magic %q", data[:4])
	}
	var pages []decodedPage
	for off := order.Uint32(data[4:]); off != 0; {
		page, next, err := readDecodedPage(data, order, off)
		if err != nil {
			return nil, err
		}
		if page.width != 0 {
			pages = append(pages, page)
		}
		off = next
	}
	return pages, nil
}

func readDecodedPage(data []byte, order binary.ByteOrder, off uint32) (decodedPage, uint32, error) {
	n := int(order.Uint16(data[off:]))
	tags := make(map[uint16][]uint32)
	for i := range n {
		e := data[int(off)+2+i*12:]
		tag, typ, count := order.Uint16(e), order.Uint16(e[2:]), order.Uint32(e[4:])
		if typ != typeShort && typ != typeLong {
			continue
		}
		size := uint32(2)
		if typ == typeLong {
			size = 4
		}
		raw := e[8:]
		if count*size > 4 {
			raw = data[order.Uint32(e[8:]):]
		}
		values := make([]uint32, count)
		for j := range values {
			if typ == typeLong {
				values[j] = order.Uint32(raw[4*j:])
			} else {
				values[j] = uint32(order.Uint16(raw[2*j:]))
			}
		}
		tags[tag] = values
	}
	next := order.Uint32(data[int(off)+2+n*12:])
	if _, ok := tags[tagImageWidth]; !ok {
		return decodedPage{}, next, nil // an EXIF-style thumbnail directory
	}
	if got := first(tags[tagCompression], compressionNone); got != compressionNone {
		return decodedPage{}, 0, fmt.Errorf("compression %d is not something this decoder reads", got)
	}
	width, height := int(tags[tagImageWidth][0]), int(tags[tagImageLength][0])
	samples := int(first(tags[tagSamplesPerPixel], 1))
	page := decodedPage{width: width, height: height, samples: samples, pix: make([]byte, width*height*samples)}
	part := func(offsets, sizes []uint32, i int) []byte {
		return data[offsets[i] : offsets[i]+sizes[i]]
	}
	switch {
	case tags[tagTileOffsets] != nil:
		tw, th := int(tags[tagTileWidth][0]), int(tags[tagTileLength][0])
		i := 0
		for top := 0; top < height; top += th {
			for left := 0; left < width; left += tw {
				tile := part(tags[tagTileOffsets], tags[tagTileByteCounts], i)
				i++
				for y := range min(th, height-top) {
					copy(page.pix[(top+y)*width*samples+left*samples:][:min(tw, width-left)*samples], tile[y*tw*samples:])
				}
			}
		}
	case first(tags[tagPlanarConfig], 1) == 2:
		perPlane := len(tags[tagStripOffsets]) / samples
		for plane := range samples {
			y := 0
			for i := range perPlane {
				strip := part(tags[tagStripOffsets], tags[tagStripByteCounts], plane*perPlane+i)
				for j, b := range strip {
					page.pix[(y+j/width)*width*samples+(j%width)*samples+plane] = b
				}
				y += len(strip) / width
			}
		}
	default:
		pos := 0
		for i := range tags[tagStripOffsets] {
			pos += copy(page.pix[pos:], part(tags[tagStripOffsets], tags[tagStripByteCounts], i))
		}
	}
	return page, next, nil
}

func first(values []uint32, fallback uint32) uint32 {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}
