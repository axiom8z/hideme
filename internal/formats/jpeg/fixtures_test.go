package jpeg

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"math/rand/v2"
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

// Fake values that must not survive cleaning.
const (
	fakeComment = "Test Comment Secret"
	fakeByline  = "Test Byline"
	fakeCity    = "Test IPTC City"
	fakeState   = "Test IPTC State"
	fakeCountry = "Test IPTC Country"
	fakeCaption = "Test IPTC Caption"
	fakeCredit  = "Test IPTC Credit"
	fakeKeyword = "Test IPTC Keyword"
	fakeICCTail = "Test ICC Tail"
)

// noisyJPEG encodes a noisy picture at top quality. Noise makes the entropy coder
// emit 0xFF often, so the scan really contains the stuffed bytes under test.
func noisyJPEG(t *testing.T) []byte {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			img.SetRGBA(x, y, color.RGBA{uint8(rng.UintN(256)), uint8(rng.UintN(256)), uint8(rng.UintN(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, img, &stdjpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func segment(marker byte, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	seg := binary.BigEndian.AppendUint16([]byte{0xFF, marker}, uint16(len(body)+2))
	return append(seg, body...)
}

// insertAfterSOI adds segments right after SOI, where real files keep metadata.
func insertAfterSOI(base []byte, segs ...[]byte) []byte {
	out := bytes.Clone(base[:2])
	for _, seg := range segs {
		out = append(out, seg...)
	}
	return append(out, base[2:]...)
}

// lastScan returns the entropy-coded data of base's last scan, which must come
// through cleaning byte for byte.
func lastScan(t *testing.T, base []byte) []byte {
	t.Helper()
	sos := bytes.LastIndex(base, []byte{0xFF, jpegmeta.MarkerSOS})
	if sos < 0 {
		t.Fatal("fixture has no scan")
	}
	length := int(binary.BigEndian.Uint16(base[sos+2:]))
	return base[sos+2+length : len(base)-2]
}

func exifSegmentFor(block []byte) []byte {
	return segment(jpegmeta.MarkerAPP0+1, []byte(exif.Prefix), block)
}

func xmpSegment() []byte {
	return segment(jpegmeta.MarkerAPP0+1, []byte(sigXMP), coretest.FakeXMP())
}

// extendedXMPSegments splits a packet the way Photoshop does when it outgrows one
// segment, with a GUID, the total length and each chunk's offset.
func extendedXMPSegments(packet []byte, chunk int) [][]byte {
	guid := []byte("0123456789ABCDEF0123456789ABCDEF")
	var segs [][]byte
	for off := 0; off < len(packet); off += chunk {
		end := min(off+chunk, len(packet))
		header := binary.BigEndian.AppendUint32(nil, uint32(len(packet)))
		header = binary.BigEndian.AppendUint32(header, uint32(off))
		segs = append(segs, segment(jpegmeta.MarkerAPP0+1, []byte(sigExtendedXMP), guid, header, packet[off:end]))
	}
	return segs
}

func iptcDataset(dataset byte, value string) []byte {
	out := []byte{0x1C, 2, dataset}
	out = binary.BigEndian.AppendUint16(out, uint16(len(value)))
	return append(out, value...)
}

func resource(id uint16, value []byte) []byte {
	out := append([]byte("8BIM"), byte(id>>8), byte(id))
	out = append(out, 0, 0) // empty Pascal name, padded to even
	out = binary.BigEndian.AppendUint32(out, uint32(len(value)))
	out = append(out, value...)
	if len(value)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func fakeIPTC() []byte {
	return bytes.Join([][]byte{
		iptcDataset(0, "\x00\x02"),
		iptcDataset(80, fakeByline),
		iptcDataset(90, fakeCity),
		iptcDataset(95, fakeState),
		iptcDataset(101, fakeCountry),
		iptcDataset(120, fakeCaption),
		iptcDataset(110, fakeCredit),
		iptcDataset(25, fakeKeyword),
	}, nil)
}

func photoshopSegment() []byte {
	return segment(jpegmeta.MarkerAPP0+13, []byte(sigPhotoshop),
		resource(jpegmeta.ResourceIPTC, fakeIPTC()),
		resource(jpegmeta.ResourceThumbnail5, []byte("Test IRB Thumbnail")),
		resource(0x03ED, make([]byte, 16)),
	)
}

// fakeICC returns a profile that passes the header checks real profiles pass.
func fakeICC(size int) []byte {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(size))
	copy(p[36:], "acsp")
	copy(p[size-len(fakeICCTail):], fakeICCTail)
	return p
}

func iccSegments(profile []byte, chunk int) [][]byte {
	count := (len(profile) + chunk - 1) / chunk
	segs := make([][]byte, 0, count)
	for i := range count {
		end := min((i+1)*chunk, len(profile))
		segs = append(segs, segment(jpegmeta.MarkerAPP0+2, []byte(sigICC), []byte{byte(i + 1), byte(count)}, profile[i*chunk:end]))
	}
	return segs
}

func jfifSegment(xThumb, yThumb byte, thumbnail []byte) []byte {
	return segment(jpegmeta.MarkerAPP0, []byte(sigJFIF), []byte{1, 2, 1, 0, 72, 0, 72, xThumb, yThumb}, thumbnail)
}

// grayScanEncoder writes Huffman-coded coefficients for the small hand-made images
// below. image/jpeg can only encode baseline without restart markers, and tests
// must cover progressive scans and restart intervals with only the standard library.
type grayScanEncoder struct {
	out []byte
	acc uint32
	n   uint
}

func (w *grayScanEncoder) bits(code uint32, size uint) {
	w.acc = w.acc<<size | code&(1<<size-1)
	w.n += size
	for w.n >= 8 {
		b := byte(w.acc >> (w.n - 8))
		w.out = append(w.out, b)
		if b == 0xFF {
			w.out = append(w.out, 0x00)
		}
		w.n -= 8
	}
}

func (w *grayScanEncoder) flush() {
	if w.n > 0 {
		w.bits(1<<(8-w.n)-1, 8-w.n)
	}
}

// Both Huffman tables give every symbol a 4-bit code equal to its position in the
// symbol list, which keeps the encoder trivial.
var (
	dcSymbols = []byte{0, 1, 2, 3, 4, 5, 6, 7}
	acSymbols = []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
)

func (w *grayScanEncoder) symbol(table []byte, symbol byte) {
	i := bytes.IndexByte(table, symbol)
	if i < 0 {
		panic("symbol not in table")
	}
	w.bits(uint32(i), 4)
}

func magnitude(v int) (size uint, code uint32) {
	abs := max(v, -v)
	for abs>>size != 0 {
		size++
	}
	if v < 0 {
		return size, uint32(v + (1 << size) - 1)
	}
	return size, uint32(v)
}

func (w *grayScanEncoder) dc(diff int) {
	size, code := magnitude(diff)
	w.symbol(dcSymbols, byte(size))
	w.bits(code, size)
}

func (w *grayScanEncoder) ac(v int) {
	size, code := magnitude(v)
	w.symbol(acSymbols, byte(size))
	w.bits(code, size)
}

func dhtSegment(class byte, symbols []byte) []byte {
	counts := make([]byte, 16)
	counts[3] = byte(len(symbols)) // all codes are four bits long
	return segment(jpegmeta.MarkerDHT, []byte{class << 4}, counts, symbols)
}

func grayHeader(sofMarker byte) []byte {
	dqt := append([]byte{0}, bytes.Repeat([]byte{4}, 64)...)
	sof := []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0}
	return bytes.Join([][]byte{{0xFF, jpegmeta.MarkerSOI}, segment(jpegmeta.MarkerDQT, dqt), segment(sofMarker, sof)}, nil)
}

func sosSegment(ss, se byte) []byte {
	return segment(jpegmeta.MarkerSOS, []byte{1, 1, 0x00, ss, se, 0})
}

// blockCoefficients returns the DC difference and two AC values of a block, chosen
// so that the picture has visible structure a comparison would notice losing.
func blockCoefficients(block int) (dc, ac1, ac2 int) {
	return []int{20, -5, 13, -9}[block], (block + 1) * 3, -(block + 2)
}

// progressiveJPEG builds a 16x16 gray progressive image with one DC scan and two
// AC scans. The between hook supplies segments to put between scans, which is
// where real progressive files have further DHT segments.
func progressiveJPEG(between func(scan int) []byte) []byte {
	out := grayHeader(0xC2)
	out = append(out, dhtSegment(0, dcSymbols)...)
	out = append(out, sosSegment(0, 0)...)
	dc := &grayScanEncoder{}
	for block := range 4 {
		diff, _, _ := blockCoefficients(block)
		dc.dc(diff)
	}
	dc.flush()
	out = append(out, dc.out...)
	out = append(out, between(1)...)
	out = append(out, dhtSegment(1, acSymbols)...)
	out = append(out, sosSegment(1, 2)...)
	low := &grayScanEncoder{}
	for block := range 4 {
		_, ac1, ac2 := blockCoefficients(block)
		low.ac(ac1)
		low.ac(ac2)
	}
	low.flush()
	out = append(out, low.out...)
	out = append(out, between(2)...)
	out = append(out, sosSegment(3, 63)...)
	high := &grayScanEncoder{}
	for range 4 {
		high.symbol(acSymbols, 0x00) // end of block
	}
	high.flush()
	out = append(out, high.out...)
	return append(out, 0xFF, jpegmeta.MarkerEOI)
}

// restartJPEG builds a 16x16 gray baseline image with a restart marker after every
// block, optionally with fill bytes in front of each marker.
func restartJPEG(fill bool) []byte {
	out := grayHeader(0xC0)
	out = append(out, dhtSegment(0, dcSymbols)...)
	out = append(out, dhtSegment(1, acSymbols)...)
	out = append(out, segment(jpegmeta.MarkerDRI, []byte{0, 1})...)
	out = append(out, sosSegment(0, 63)...)
	for block := range 4 {
		w := &grayScanEncoder{}
		diff, ac1, ac2 := blockCoefficients(block)
		w.dc(diff)
		w.ac(ac1)
		w.ac(ac2)
		w.symbol(acSymbols, 0x00)
		w.flush()
		out = append(out, w.out...)
		if block < 3 {
			if fill {
				out = append(out, 0xFF, 0xFF)
			}
			out = append(out, 0xFF, 0xD0+byte(block))
		}
	}
	return append(out, 0xFF, jpegmeta.MarkerEOI)
}
