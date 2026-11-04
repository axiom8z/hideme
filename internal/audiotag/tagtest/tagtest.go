// Package tagtest builds tag fixtures for the audio handlers' tests, with obviously
// fake values. Only test files may import it.
package tagtest

import (
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"unicode/utf16"
)

// Fake is one distinctive string planted in every tag fixture, so a test can search
// for it whichever tag a value came from.
const Fake = "Test Author"

// StreamingFileSize and StreamingBudget set up the proof that a handler streams: it
// is given a file of the first size and may allocate only the second in total. A
// handler that loaded the file, or kept anything per frame or page, could not pass.
const (
	StreamingFileSize = 64 << 20
	StreamingBudget   = 8 << 20
)

// Repeating is a Source of any size that holds almost nothing in memory: a prefix
// and then a period repeated up to Total.
type Repeating struct {
	Prefix, Period []byte
	Total          int64
}

// Size implements core.Source.
func (r Repeating) Size() int64 { return r.Total }

// ReadAt implements core.Source.
func (r Repeating) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.Total {
		return 0, io.EOF
	}
	var err error
	if int64(len(p)) > r.Total-off {
		p, err = p[:r.Total-off], io.EOF
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos < int64(len(r.Prefix)) {
			n += copy(p[n:], r.Prefix[pos:])
			continue
		}
		n += copy(p[n:], r.Period[(pos-int64(len(r.Prefix)))%int64(len(r.Period)):])
	}
	return n, err
}

// WholePeriods returns a Total that ends on a period boundary.
func WholePeriods(prefix, period int, size int64) int64 {
	return int64(prefix) + (size-int64(prefix))/int64(period)*int64(period)
}

// Allocated returns how many bytes f allocates in total, freed or not.
func Allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// CountingWriter discards what is written, keeping only its length.
type CountingWriter struct{ N int64 }

// Write implements io.Writer.
func (w *CountingWriter) Write(p []byte) (int, error) {
	w.N += int64(len(p))
	return len(p), nil
}

// Header builds an ID3v2 tag header.
func Header(major, flags byte, size int) []byte {
	out := []byte{'I', 'D', '3', major, 0, flags}
	return append(out, Syncsafe(size)...)
}

// Syncsafe encodes n in four bytes of seven bits each.
func Syncsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
}

// Tag wraps frames in an ID3v2 tag.
func Tag(major, flags byte, frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	return append(Header(major, flags, len(body)), body...)
}

// Frame22 builds a v2.2 frame.
func Frame22(id string, body []byte) []byte {
	n := len(body)
	out := append([]byte(id), byte(n>>16), byte(n>>8), byte(n))
	return append(out, body...)
}

// Frame23 builds a v2.3 frame.
func Frame23(id string, flags uint16, body []byte) []byte {
	out := binary.BigEndian.AppendUint32([]byte(id), uint32(len(body)))
	out = binary.BigEndian.AppendUint16(out, flags)
	return append(out, body...)
}

// Frame24 builds a v2.4 frame, whose size is syncsafe.
func Frame24(id string, flags uint16, body []byte) []byte {
	out := append([]byte(id), Syncsafe(len(body))...)
	out = binary.BigEndian.AppendUint16(out, flags)
	return append(out, body...)
}

// Latin1Text builds the body of a text frame in encoding 0.
func Latin1Text(s string) []byte { return append([]byte{0}, s...) }

// UTF8Text builds the body of a text frame in encoding 3.
func UTF8Text(s string) []byte { return append([]byte{3}, s...) }

// UTF16Text builds the body of a text frame in encoding 1, with a byte order mark.
func UTF16Text(s string) []byte {
	return append([]byte{1}, UTF16LEWithBOM(s)...)
}

// UTF16BEText builds the body of a text frame in encoding 2, which has no mark.
func UTF16BEText(s string) []byte {
	out := []byte{2}
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.BigEndian.AppendUint16(out, u)
	}
	return out
}

// UTF16LEWithBOM encodes s as UTF-16LE with a byte order mark.
func UTF16LEWithBOM(s string) []byte {
	return append([]byte{0xFF, 0xFE}, UTF16LE(s)...)
}

// UTF16LE encodes s as UTF-16LE.
func UTF16LE(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// join concatenates byte strings.
func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// fakePicture stands in for image bytes.
var fakePicture = []byte("\xFF\xD8\xFF\xE0Test Cover Art Bytes\xFF\xD9")

// ID3v23 returns a v2.3 tag holding every kind of frame the decoder treats
// specially, in each text encoding. ID3Fields lists what it must report.
func ID3v23() []byte {
	return Tag(3, 0,
		Frame23("TIT2", 0, Latin1Text("Test Title")),
		Frame23("TPE1", 0, UTF16Text(Fake)),
		Frame23("TALB", 0, UTF16BEText("Test Album")),
		Frame23("TPE2", 0, UTF8Text("Test Album Artist")),
		Frame23("TCOM", 0, Latin1Text("Test Composer")),
		Frame23("TENC", 0, Latin1Text("Test Encoded By")),
		Frame23("TSSE", 0, Latin1Text("Test Encoder")),
		Frame23("TYER", 0, Latin1Text("2001")),
		Frame23("TCOP", 0, Latin1Text("Test Copyright")),
		Frame23("TOFN", 0, Latin1Text("C:/Users/Test/secret.wav")),
		Frame23("TZZZ", 0, Latin1Text("Test Unknown Text")),
		Frame23("COMM", 0, join([]byte{0}, []byte("eng"), []byte("Test Desc\x00Test Comment"))),
		Frame23("USLT", 0, join([]byte{0}, []byte("eng"), []byte("\x00Test Lyrics"))),
		Frame23("TXXX", 0, join([]byte{0}, []byte("Test Key\x00Test Value"))),
		Frame23("WXXX", 0, join([]byte{0}, []byte("Test Link\x00http://example.com/test-wxxx"))),
		Frame23("WOAR", 0, []byte("http://example.com/test-woar")),
		Frame23("PRIV", 0, []byte("Test Owner\x00Test Private Bytes")),
		Frame23("GEOB", 0, []byte("\x00text/plain\x00test-file.txt\x00Test File Desc\x00Test File Bytes")),
		Frame23("UFID", 0, []byte("http://example.com/test-ufid\x00TESTID")),
		Frame23("APIC", 0, join([]byte("\x00image/jpeg\x00\x03Test Cover Desc\x00"), fakePicture)),
		Frame23("MCDI", 0, []byte("Test Binary Frame")),
	)
}

// ID3Fields returns the Finding.Field values ID3v23 must produce.
func ID3Fields() []string {
	return []string{
		"ID3v2 tag", "Title", "Artist", "Album", "Album artist", "Composer", "Encoded by",
		"Encoder settings", "Year", "Copyright", "Original file name", "Other ID3 frame",
		"Comment", "Lyrics", "User-defined text", "URL", "Private data", "Attached file",
		"Unique file ID", "Embedded picture",
	}
}

// ID3Secrets returns byte strings from ID3v23 that must not survive cleaning.
func ID3Secrets() [][]byte {
	return [][]byte{
		[]byte("ID3"), UTF16LE(Fake), []byte("Test Title"), []byte("Test Composer"),
		[]byte("Test Encoded By"), []byte("Test Copyright"), []byte("secret.wav"),
		[]byte("Test Comment"), []byte("Test Lyrics"), []byte("Test Value"),
		[]byte("test-wxxx"), []byte("test-woar"), []byte("Test Private Bytes"),
		[]byte("test-file.txt"), []byte("Test File Bytes"), []byte("test-ufid"),
		[]byte("Test Cover Art Bytes"), []byte("Test Cover Desc"),
	}
}

// ID3v24 returns a v2.4 tag.
func ID3v24() []byte {
	return Tag(4, 0,
		Frame24("TIT2", 0, UTF8Text("Test Title")),
		Frame24("TPE1", 0, UTF8Text(Fake)),
		Frame24("TDRC", 0, UTF8Text("2001-02-03")),
		Frame24("TIPL", 0, UTF8Text("engineer\x00Test Engineer")),
	)
}

// ID3v22 returns a v2.2 tag.
func ID3v22() []byte {
	return Tag(2, 0,
		Frame22("TT2", Latin1Text("Test Title")),
		Frame22("TP1", Latin1Text(Fake)),
		Frame22("COM", []byte("\x00engTest Desc\x00Test Comment")),
		Frame22("PIC", join([]byte("\x00JPG\x03Test Cover Desc\x00"), fakePicture)),
	)
}

// fixed pads a string to a fixed-width field with zeros.
func fixed(s string, n int) []byte {
	out := make([]byte, n)
	copy(out, s)
	return out
}

// ID3v1 returns a 128-byte ID3v1 tag.
func ID3v1() []byte {
	return join([]byte("TAG"), fixed("Test V1 Title", 30), fixed(Fake, 30), fixed("Test V1 Album", 30),
		[]byte("2001"), fixed("Test V1 Comment", 30), []byte{12})
}

// Enhanced returns a 227-byte Enhanced TAG+ block, which always precedes ID3v1.
func Enhanced() []byte {
	return join([]byte("TAG+"), fixed("Test Plus Title", 60), fixed(Fake, 60), fixed("Test Plus Album", 60),
		[]byte{0}, fixed("Test Plus Genre", 30), []byte("000:00000:00"))
}

// apeItems returns the items of a fake APE tag and how many there are.
func apeItems() ([]byte, int) {
	item := func(key string, flags uint32, value []byte) []byte {
		out := binary.LittleEndian.AppendUint32(nil, uint32(len(value)))
		out = binary.LittleEndian.AppendUint32(out, flags)
		out = append(out, key...)
		out = append(out, 0)
		return append(out, value...)
	}
	items := join(
		item("Title", 0, []byte("Test APE Title")),
		item("Artist", 0, []byte(Fake)),
		item("Custom Key", 0, []byte("Test APE Custom")),
		item("Cover Art (Front)", 2, join([]byte("cover.jpg\x00"), fakePicture)),
		item("Binary Blob", 2, []byte("Test APE Binary")),
	)
	return items, 5
}

func apeBlock(version, size, count, flags uint32) []byte {
	out := []byte("APETAGEX")
	for _, v := range []uint32{version, size, count, flags} {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	return append(out, make([]byte, 8)...)
}

// APE returns an APEv2 tag with a header and a footer.
func APE() []byte {
	items, count := apeItems()
	size := uint32(len(items) + 32)
	const hasHeader, isHeader = 1 << 31, 1 << 29
	return join(apeBlock(2000, size, uint32(count), hasHeader|isHeader), items, apeBlock(2000, size, uint32(count), hasHeader))
}

// APEFooterOnly returns an APEv1-style tag, which has only a footer.
func APEFooterOnly() []byte {
	items, count := apeItems()
	return join(items, apeBlock(1000, uint32(len(items)+32), uint32(count), 0))
}

// Lyrics3v1 returns a Lyrics3 v1 tag.
func Lyrics3v1() []byte {
	return []byte("LYRICSBEGINTest Lyrics One by " + Fake + "LYRICSEND")
}

// Lyrics3v2 returns a Lyrics3 v2 tag.
func Lyrics3v2() []byte {
	field := func(id, value string) string { return fmt.Sprintf("%s%05d%s", id, len(value), value) }
	body := "LYRICSBEGIN" + field("IND", "00") + field("LYR", "Test Lyrics Two") + field("AUT", Fake) +
		field("ETT", "Test Lyrics Title")
	return []byte(fmt.Sprintf("%s%06dLYRICS200", body, len(body)))
}

// VorbisComment builds a Vorbis comment structure, without any magic or framing.
func VorbisComment(vendor string, comments ...string) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(vendor)))
	out = append(out, vendor...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(comments)))
	for _, c := range comments {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(c)))
		out = append(out, c...)
	}
	return out
}

// FLACPicture builds the picture structure of a FLAC PICTURE block.
func FLACPicture() []byte {
	var out []byte
	text := func(s string) {
		out = binary.BigEndian.AppendUint32(out, uint32(len(s)))
		out = append(out, s...)
	}
	out = binary.BigEndian.AppendUint32(out, 3) // front cover
	text("image/jpeg")
	text("Test Cover Desc")
	out = append(out, make([]byte, 16)...) // width, height, depth, colours
	out = binary.BigEndian.AppendUint32(out, uint32(len(fakePicture)))
	return append(out, fakePicture...)
}
