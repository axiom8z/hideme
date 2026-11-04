package audiotag

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	id3HeaderLen = 10

	id3FlagUnsync   = 0x80
	id3FlagExtended = 0x40
	id3FlagFooter   = 0x10
	// id3v22FlagCompressed shares its bit with the extended header flag of later
	// versions. The v2.2 specification names no compression scheme and says to
	// ignore such a tag, so its frames cannot be decoded.
	id3v22FlagCompressed = 0x40

	// maxFrameRead bounds what decoding one frame can cost. Values are displayed
	// at a couple of hundred characters at most, so a cover image's bytes are of
	// no use here: only its description, which comes first.
	maxFrameRead = 1 << 20
)

var (
	id3TagField     = Field{"ID3v2 tag", core.CatContent, core.RiskLow}
	id3DamagedField = Field{"Damaged ID3 frames", core.CatHidden, core.RiskMed}
	id3OtherField   = Field{"Other ID3 frame", core.CatContent, core.RiskLow}
	id3CryptField   = Field{"Encrypted ID3 frame", core.CatHidden, core.RiskMed}
	id3UserText     = Field{"User-defined text", core.CatContent, core.RiskMed}
	id3PrivateField = Field{"Private data", core.CatHidden, core.RiskHigh}
	id3FileField    = Field{"Attached file", core.CatHidden, core.RiskHigh}
	id3UFIDField    = Field{"Unique file ID", core.CatIdentity, core.RiskMed}
)

// id3Fields maps text and URL frames to what they expose. The v2.2 three-letter
// IDs are translated by id3v22IDs before this lookup.
var id3Fields = map[string]Field{
	"TIT1": {"Content group", core.CatContent, core.RiskLow},
	"TIT2": TitleField,
	"TIT3": {"Subtitle", core.CatContent, core.RiskLow},
	"TALB": AlbumField,
	"TOAL": {"Original album", core.CatContent, core.RiskLow},
	"TPE1": ArtistField,
	"TPE2": {"Album artist", core.CatIdentity, core.RiskHigh},
	"TPE3": {"Conductor", core.CatIdentity, core.RiskHigh},
	"TPE4": {"Remixer", core.CatIdentity, core.RiskHigh},
	"TOPE": {"Original artist", core.CatIdentity, core.RiskHigh},
	"TCOM": {"Composer", core.CatIdentity, core.RiskHigh},
	"TEXT": {"Lyricist", core.CatIdentity, core.RiskHigh},
	"TOLY": {"Original lyricist", core.CatIdentity, core.RiskHigh},
	"TENC": {"Encoded by", core.CatIdentity, core.RiskHigh},
	"TOWN": {"File owner", core.CatIdentity, core.RiskHigh},
	"TIPL": {"Involved people", core.CatIdentity, core.RiskHigh},
	"IPLS": {"Involved people", core.CatIdentity, core.RiskHigh},
	"TMCL": {"Musician credits", core.CatIdentity, core.RiskHigh},
	"TCOP": CopyrightField,
	"TPRO": {"Produced notice", core.CatIdentity, core.RiskMed},
	"TPUB": {"Publisher", core.CatIdentity, core.RiskMed},
	"TRSN": {"Radio station", core.CatIdentity, core.RiskMed},
	"TRSO": {"Radio station owner", core.CatIdentity, core.RiskMed},
	"TOFN": {"Original file name", core.CatIdentity, core.RiskHigh},
	"TSSE": {"Encoder settings", core.CatSoftware, core.RiskLow},
	"TFLT": {"File type", core.CatTechnical, core.RiskNone},
	"TMED": {"Media type", core.CatTechnical, core.RiskNone},
	"TDRC": {"Recording date", core.CatTime, core.RiskMed},
	"TDEN": {"Encoding date", core.CatTime, core.RiskMed},
	"TDOR": {"Original release date", core.CatTime, core.RiskLow},
	"TDRL": {"Release date", core.CatTime, core.RiskLow},
	"TDTG": {"Tagging date", core.CatTime, core.RiskMed},
	"TYER": {"Year", core.CatTime, core.RiskMed},
	"TDAT": {"Recording day", core.CatTime, core.RiskMed},
	"TIME": {"Recording time", core.CatTime, core.RiskMed},
	"TRDA": {"Recording dates", core.CatTime, core.RiskMed},
	"TORY": {"Original release year", core.CatTime, core.RiskLow},
	"TCON": GenreField,
	"TRCK": TrackField,
	"TPOS": {"Disc number", core.CatContent, core.RiskLow},
	"TSRC": {"ISRC", core.CatContent, core.RiskLow},
	"TLAN": {"Language", core.CatContent, core.RiskLow},
	"TBPM": {"Tempo", core.CatTechnical, core.RiskNone},
	"TKEY": {"Musical key", core.CatTechnical, core.RiskNone},
	"TLEN": {"Length", core.CatTechnical, core.RiskNone},
	"TSIZ": {"Size", core.CatTechnical, core.RiskNone},
	"TDLY": {"Playlist delay", core.CatTechnical, core.RiskNone},
	"TOFF": {"Offset", core.CatTechnical, core.RiskNone},
	"TMOO": {"Mood", core.CatContent, core.RiskLow},
	"TSOA": {"Album sort order", core.CatContent, core.RiskLow},
	"TSOP": {"Artist sort order", core.CatIdentity, core.RiskHigh},
	"TSOT": {"Title sort order", core.CatContent, core.RiskLow},
	"TSST": {"Set subtitle", core.CatContent, core.RiskLow},
	"WCOM": URLField,
	"WCOP": URLField,
	"WOAF": URLField,
	"WOAR": URLField,
	"WOAS": URLField,
	"WORS": URLField,
	"WPAY": URLField,
	"WPUB": URLField,
	"WXXX": URLField,
	"COMM": CommentField,
	"USLT": LyricsField,
}

// id3StructuredFrames are the frames with layouts of their own, which id3Fields does
// not cover.
var id3StructuredFrames = map[string]bool{"TXXX": true, "APIC": true, "GEOB": true, "PRIV": true, "UFID": true}

// id3v22IDs translates v2.2 frame IDs for the frames reported in detail.
var id3v22IDs = map[string]string{
	"TT1": "TIT1", "TT2": "TIT2", "TT3": "TIT3", "TAL": "TALB", "TOT": "TOAL",
	"TP1": "TPE1", "TP2": "TPE2", "TP3": "TPE3", "TP4": "TPE4", "TOA": "TOPE",
	"TCM": "TCOM", "TXT": "TEXT", "TOL": "TOLY", "TEN": "TENC", "IPL": "IPLS",
	"TCR": "TCOP", "TPB": "TPUB", "TOF": "TOFN", "TSS": "TSSE", "TFT": "TFLT",
	"TMT": "TMED", "TYE": "TYER", "TDA": "TDAT", "TIM": "TIME", "TRD": "TRDA",
	"TOR": "TORY", "TCO": "TCON", "TRK": "TRCK", "TPA": "TPOS", "TRC": "TSRC",
	"TLA": "TLAN", "TBP": "TBPM", "TKE": "TKEY", "TLE": "TLEN", "TSI": "TSIZ",
	"TDY": "TDLY", "WCM": "WCOM", "WCP": "WCOP", "WAF": "WOAF", "WAR": "WOAR",
	"WAS": "WOAS", "WPB": "WPUB", "TXX": "TXXX", "WXX": "WXXX", "COM": "COMM",
	"ULT": "USLT", "PIC": "APIC", "GEO": "GEOB", "UFI": "UFID",
}

// ID3v2Length returns the total length of the ID3v2 tag whose header starts b,
// including the header and any footer. It accepts any version, because a tag of
// a future version still states its size and is still metadata.
func ID3v2Length(b []byte) (int64, bool) {
	if len(b) < id3HeaderLen || string(b[:3]) != "ID3" || b[3] == 0xFF || b[4] == 0xFF {
		return 0, false
	}
	size, ok := syncsafe(b[6:id3HeaderLen])
	if !ok {
		return 0, false
	}
	total := id3HeaderLen + size
	if b[3] == 4 && b[5]&id3FlagFooter != 0 {
		total += id3HeaderLen
	}
	return total, true
}

// syncsafe decodes a big-endian integer that uses seven bits of each byte.
func syncsafe(b []byte) (int64, bool) {
	var n int64
	for _, c := range b {
		if c >= 0x80 {
			return 0, false
		}
		n = n<<7 | int64(c)
	}
	return n, true
}

// describeID3v2 reports the tag in region r.
func describeID3v2(ctx context.Context, win *Window, r Region, where string, sink *Sink) error {
	hdr := win.At(r.Off, id3HeaderLen)
	if hdr == nil {
		return win.Err()
	}
	major, flags := hdr[3], hdr[5]
	if major >= 2 && major <= 4 {
		where = Join(where, fmt.Sprintf("ID3v2.%d", major))
	} else {
		where = Join(where, "ID3v2")
	}
	sink.Add(id3TagField.Remove(core.ByteCount(r.Len), where))

	payload := r.Len - id3HeaderLen
	if major == 4 && flags&id3FlagFooter != 0 {
		payload -= id3HeaderLen
	}
	if major < 2 || major > 4 || payload <= 0 {
		return nil
	}
	if major == 2 && flags&id3v22FlagCompressed != 0 {
		sink.Add(id3DamagedField.Remove("compressed tag, not decoded", where))
		return nil
	}
	raw := bufio.NewReader(io.NewSectionReader(win.Source(), r.Off+id3HeaderLen, payload))
	t := &id3Reader{
		src: win.Source(), r: raw, major: major, where: where, sink: sink,
		pos: r.Off + id3HeaderLen, end: r.Off + id3HeaderLen + payload,
	}
	// Tag-wide unsynchronisation applies to the bytes after the header in v2.2 and
	// v2.3, and frame sizes count the decoded bytes. In v2.4 it is per frame.
	if major < 4 && flags&id3FlagUnsync != 0 {
		t.r = bufio.NewReader(&desyncReader{r: raw})
	}
	if flags&id3FlagExtended != 0 && !t.skipExtendedHeader() {
		sink.Add(id3DamagedField.Remove("extended header", where))
		return nil
	}
	return t.frames(ctx)
}

// id3Reader walks the frames of one tag.
type id3Reader struct {
	src   core.Source
	r     *bufio.Reader
	major byte
	where string
	sink  *Sink
	// pos and end are file offsets for the v2.4 size check. They are meaningful
	// only for v2.4, where the reader sees the file's bytes undecoded.
	pos, end int64
}

func (t *id3Reader) skipExtendedHeader() bool {
	hdr, err := t.r.Peek(4)
	if err != nil {
		return false
	}
	var skip int64
	if t.major == 4 {
		size, ok := syncsafe(hdr)
		if !ok || size < 4 {
			return false
		}
		skip = size // the v2.4 size counts its own four bytes
	} else {
		skip = int64(binary.BigEndian.Uint32(hdr)) + 4 // the v2.3 size does not
	}
	n, err := io.CopyN(io.Discard, t.r, skip)
	t.pos += n
	return err == nil
}

func (t *id3Reader) frames(ctx context.Context) error {
	headerLen := id3HeaderLen
	if t.major == 2 {
		headerLen = 6
	}
	var buf []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.sink.full() {
			t.sink.omitted++
			return nil
		}
		hdr, err := t.r.Peek(headerLen)
		if err != nil || hdr[0] == 0 {
			return nil // the end of the tag, or the padding before it
		}
		id, size, flags := t.parseFrameHeader(hdr)
		if !validFrameID(id) {
			t.sink.Add(id3DamagedField.Remove("invalid frame ID", t.where))
			return nil
		}
		if _, err := t.r.Discard(headerLen); err != nil {
			return nil
		}
		n := min(size, maxFrameRead)
		if int64(cap(buf)) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		got, err := io.ReadFull(t.r, buf)
		if err == nil && size > n {
			_, err = io.CopyN(io.Discard, t.r, size-n)
		}
		t.frame(id, flags, buf[:got], size)
		if err != nil {
			t.sink.Add(id3DamagedField.Remove("frame runs past the end of the tag", t.frameWhere(id)))
			return nil
		}
		t.pos += int64(headerLen) + size
	}
}

func (t *id3Reader) parseFrameHeader(hdr []byte) (id string, size int64, flags uint16) {
	switch t.major {
	case 2:
		return string(hdr[:3]), int64(hdr[3])<<16 | int64(hdr[4])<<8 | int64(hdr[5]), 0
	case 3:
		return string(hdr[:4]), int64(binary.BigEndian.Uint32(hdr[4:])), binary.BigEndian.Uint16(hdr[8:])
	default:
		return string(hdr[:4]), t.size24(hdr[4:8]), binary.BigEndian.Uint16(hdr[8:])
	}
}

// size24 decodes a v2.4 frame size, which should be syncsafe. Old versions of
// iTunes wrote plain integers instead, which is indistinguishable for small
// frames and wrong for the rest, so the two readings are tried against where the
// next frame would have to start.
func (t *id3Reader) size24(b []byte) int64 {
	plain := int64(binary.BigEndian.Uint32(b))
	safe, ok := syncsafe(b)
	switch {
	case !ok:
		return plain
	case plain < 0x80:
		return safe
	case t.frameStartsAt(t.pos + id3HeaderLen + safe):
		return safe
	case t.frameStartsAt(t.pos + id3HeaderLen + plain):
		return plain
	}
	return safe
}

func (t *id3Reader) frameStartsAt(off int64) bool {
	if off >= t.end {
		return off == t.end
	}
	var next [4]byte
	if n, _ := t.src.ReadAt(next[:], off); n < len(next) {
		return false
	}
	return next[0] == 0 || validFrameID(string(next[:]))
}

func validFrameID(id string) bool {
	for i := range len(id) {
		if c := id[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// frameWhere locates a frame for a finding. clean prints Where, and a writer can give
// a frame any ID it likes, so only IDs from the specification go in it. The others
// are shown in the finding's value.
func (t *id3Reader) frameWhere(id string) string {
	var known bool
	if t.major == 2 {
		_, known = id3v22IDs[id]
	} else {
		_, known = id3Fields[id]
		known = known || id3StructuredFrames[id]
	}
	if known {
		return Join(t.where, id)
	}
	return t.where
}

// frame reports one frame. data may be shorter than size when the frame is large.
func (t *id3Reader) frame(id string, flags uint16, data []byte, size int64) {
	where := t.frameWhere(id)
	if t.major == 2 {
		if mapped, ok := id3v22IDs[id]; ok {
			id = mapped
		}
	}
	data, encrypted, ok := t.unwrap(flags, data)
	switch {
	case encrypted:
		t.sink.Add(id3CryptField.Remove(core.ByteCount(size), where))
		return
	case !ok:
		t.sink.Add(id3DamagedField.Remove("frame could not be decoded", where))
		return
	}
	switch {
	case id == "TXXX":
		enc, body := splitEncoding(data)
		desc, value := splitText(enc, body)
		t.sink.Add(id3UserText.Remove(pair(decodeText(enc, desc), joinValues(decodeText(enc, value))), where))
	case id == "WXXX":
		enc, body := splitEncoding(data)
		desc, url := splitText(enc, body)
		t.sink.Add(URLField.Remove(pair(decodeText(enc, desc), Latin1(trimNUL(url))), where))
	case id == "COMM" || id == "USLT":
		enc, body := splitEncoding(data)
		if len(body) >= 3 {
			body = body[3:] // the language code
		}
		desc, text := splitText(enc, body)
		t.sink.Add(id3Fields[id].Remove(pair(decodeText(enc, desc), decodeText(enc, text)), where))
	case id == "APIC":
		t.picture(data, size, where)
	case id == "GEOB":
		t.attachment(data, size, where)
	case id == "PRIV":
		owner, rest := splitText(0, data)
		t.reportXMP(rest, where)
		t.sink.Add(id3PrivateField.Remove(pair(Latin1(owner), core.ByteCount(size)), where))
	case id == "UFID":
		owner, _ := splitText(0, data)
		t.sink.Add(id3UFIDField.Remove(pair(Latin1(owner), core.ByteCount(size)), where))
	case id[0] == 'T':
		enc, body := splitEncoding(data)
		field, known := id3Fields[id]
		if !known {
			field = id3OtherField
		}
		value := joinValues(decodeText(enc, body))
		if !known {
			value = pair(id, value)
		}
		t.sink.Add(field.Remove(value, where))
	case id[0] == 'W':
		field, known := id3Fields[id]
		value := Latin1(trimNUL(data))
		if !known {
			field, value = URLField, pair(id, value)
		}
		t.sink.Add(field.Remove(value, where))
	default:
		t.sink.Add(id3OtherField.Remove(pair(id, core.ByteCount(size)), where))
	}
}

// unwrap removes the per-frame encodings: the extra bytes that frame flags add
// to the front of the data, unsynchronisation and compression.
func (t *id3Reader) unwrap(flags uint16, data []byte) (out []byte, encrypted, ok bool) {
	var prefix int
	var compressed, unsync bool
	switch t.major {
	case 3:
		compressed = flags&0x0080 != 0
		encrypted = flags&0x0040 != 0
		prefix = b2i(compressed)*4 + b2i(encrypted) + b2i(flags&0x0020 != 0)
	case 4:
		compressed = flags&0x0008 != 0
		encrypted = flags&0x0004 != 0
		unsync = flags&0x0002 != 0
		prefix = b2i(flags&0x0040 != 0) + b2i(encrypted) + b2i(flags&0x0001 != 0)*4
	}
	if encrypted {
		return nil, true, true
	}
	if len(data) < prefix {
		return nil, false, false
	}
	data = data[prefix:]
	if unsync {
		data = bytes.ReplaceAll(data, []byte{0xFF, 0x00}, []byte{0xFF})
	}
	if compressed {
		budget := min(maxFrameRead, maxInflated-t.sink.inflated)
		if budget <= 0 {
			return nil, false, false
		}
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false, false
		}
		defer zr.Close()
		// The cap makes a decompression bomb harmless. A short read just gives a
		// truncated value, and a value is only ever displayed in part anyway.
		data, err = io.ReadAll(io.LimitReader(zr, budget))
		t.sink.inflated += int64(len(data))
		if err != nil && len(data) == 0 {
			return nil, false, false
		}
	}
	return data, false, true
}

func (t *id3Reader) picture(data []byte, size int64, where string) {
	enc, body := splitEncoding(data)
	var mime string
	if t.major == 2 {
		if len(body) < 3 {
			t.sink.Add(PictureField.Remove(core.ByteCount(size), where))
			return
		}
		mime, body = Latin1(trimNUL(body[:3])), body[3:]
	} else {
		var m []byte
		m, body = splitText(0, body)
		mime = Latin1(m)
	}
	var kind string
	if len(body) > 0 {
		kind = pictureKind(uint32(body[0]))
		body = body[1:]
	}
	desc, _ := splitText(enc, body)
	t.sink.Add(PictureField.Remove(PictureValue(mime, kind, decodeText(enc, desc), size), where))
}

func (t *id3Reader) attachment(data []byte, size int64, where string) {
	enc, body := splitEncoding(data)
	mime, body := splitText(0, body)
	name, body := splitText(enc, body)
	desc, rest := splitText(enc, body)
	t.reportXMP(rest, where)
	value := pair(decodeText(enc, name), pair(Latin1(mime), pair(decodeText(enc, desc), core.ByteCount(size))))
	t.sink.Add(id3FileField.Remove(value, where))
}

func (t *id3Reader) reportXMP(data []byte, where string) {
	if xmp.Contains(data) {
		for _, f := range xmp.Findings(data, where) {
			t.sink.Add(f)
		}
	}
}

// pair joins a label and a value, leaving out whichever is empty.
func pair(label, value string) string {
	switch {
	case label == "":
		return value
	case value == "":
		return label
	}
	return label + ": " + value
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func splitEncoding(data []byte) (byte, []byte) {
	if len(data) == 0 {
		return 0, nil
	}
	return data[0], data[1:]
}

// splitText cuts b at the first terminator for the encoding: one NUL for the
// single-byte encodings and an aligned pair of NULs for UTF-16.
func splitText(enc byte, b []byte) (text, rest []byte) {
	if enc == 1 || enc == 2 {
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return b[:i], b[i+2:]
			}
		}
		return b, nil
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

func trimNUL(b []byte) []byte {
	return bytes.TrimRight(b, "\x00")
}

// joinValues displays the NUL-separated list that v2.4 text frames can hold.
func joinValues(s string) string {
	return strings.ReplaceAll(strings.Trim(s, "\x00"), "\x00", ", ")
}

// decodeText decodes an ID3 text field. Encodings outside the four defined are
// read as Latin-1, which at least shows the user something.
func decodeText(enc byte, b []byte) string {
	switch enc {
	case 1, 2:
		return decodeUTF16(enc, b)
	case 3:
		return string(b)
	}
	return Latin1(b)
}

// decodeUTF16 honours a byte order mark over the encoding's nominal order, since
// writers get this wrong. Encoding 1 without a mark is taken as little-endian,
// the order nearly every tagger uses.
func decodeUTF16(enc byte, b []byte) string {
	var order binary.ByteOrder = binary.LittleEndian
	if enc == 2 {
		order = binary.BigEndian
	}
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			order, b = binary.LittleEndian, b[2:]
		case b[0] == 0xFE && b[1] == 0xFF:
			order, b = binary.BigEndian, b[2:]
		}
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

// desyncReader undoes ID3 unsynchronisation, which inserts a zero after every
// 0xFF, as a stream. A tag with a cover image can be megabytes long, so it is not
// loaded just to find its frame boundaries.
type desyncReader struct {
	r      *bufio.Reader
	prevFF bool
}

func (d *desyncReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c, err := d.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if d.prevFF && c == 0 {
			d.prevFF = false
			continue
		}
		d.prevFF = c == 0xFF
		p[n] = c
		n++
	}
	return n, nil
}
