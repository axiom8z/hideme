package audiotag

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// Kind names a tag format.
type Kind int

// The tag formats a Region can hold.
const (
	KindID3v2 Kind = iota
	KindAPE
	KindID3v1
	KindEnhanced
	KindLyrics3v1
	KindLyrics3v2
)

const (
	apeHeaderLen = 32
	id3v1Len     = 128
	enhancedLen  = 227

	apeFlagHasHeader  = 1 << 31
	apeFlagThisHeader = 1 << 29

	// maxLyrics3v1 is the longest Lyrics3 v1 tag: 5100 characters of lyrics plus
	// its begin and end markers.
	maxLyrics3v1 = 5100 + 11 + 9
	// maxTrailers stops a walk over a file built from thousands of empty tags
	// from turning into a walk over every one of them. Real files have three at
	// most; whatever remains is reported as trailing data and still dropped.
	maxTrailers = 16
	// maxTagValue bounds one displayed tag value, as maxFrameRead does for ID3.
	maxTagValue = 1 << 16
)

var (
	apeTagField     = Field{"APE tag", core.CatContent, core.RiskLow}
	id3v1TagField   = Field{"ID3v1 tag", core.CatContent, core.RiskLow}
	enhancedField   = Field{"Enhanced TAG+ tag", core.CatContent, core.RiskLow}
	lyrics3TagField = Field{"Lyrics3 tag", core.CatContent, core.RiskLow}
	yearField       = Field{"Year", core.CatTime, core.RiskMed}
)

var lyrics3Fields = map[string]Field{
	"LYR": LyricsField,
	"INF": CommentField,
	"AUT": {"Author", core.CatIdentity, core.RiskHigh},
	"EAL": AlbumField,
	"EAR": ArtistField,
	"ETT": TitleField,
}

// Region is the extent of one tag in a file.
type Region struct {
	Kind Kind
	Off  int64
	Len  int64
}

// TagAt recognises a tag at off that announces itself with a header and fits
// before limit, so that a walk meeting one mid-file can remove it whole. Tags
// without a usable header are found by TrailingTags instead.
func TagAt(win *Window, off, limit int64) (Region, bool) {
	b := win.AtMost(off, apeHeaderLen)
	var kind Kind
	var n int64
	var ok bool
	switch {
	case bytes.HasPrefix(b, []byte("ID3")):
		kind = KindID3v2
		n, ok = ID3v2Length(b)
	case bytes.HasPrefix(b, []byte("APETAGEX")):
		kind = KindAPE
		n, ok = apeLengthFromHeader(b)
	}
	if !ok || n > limit-off {
		return Region{}, false
	}
	return Region{kind, off, n}, true
}

func apeLengthFromHeader(b []byte) (int64, bool) {
	if len(b) < apeHeaderLen {
		return 0, false
	}
	version, size, flags := apeFields(b)
	if !validAPEVersion(version) || flags&apeFlagThisHeader == 0 {
		return 0, false
	}
	return apeHeaderLen + int64(size), true
}

func apeFields(b []byte) (version, size, flags uint32) {
	return binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:]), binary.LittleEndian.Uint32(b[20:])
}

func validAPEVersion(v uint32) bool {
	return v == 1000 || v == 2000
}

// TrailingTags finds the tags anchored to the end of the range [floor, end),
// working backwards, since these formats are designed to be found from the end.
// The result has the tag nearest the end first.
func TrailingTags(win *Window, floor, end int64) []Region {
	var tags []Region
	for range maxTrailers {
		r, ok := trailerEndingAt(win, floor, end)
		if !ok {
			break
		}
		tags = append(tags, r)
		end = r.Off
		if r.Kind == KindID3v1 && end-floor >= enhancedLen && string(win.At(end-enhancedLen, 4)) == "TAG+" {
			tags = append(tags, Region{KindEnhanced, end - enhancedLen, enhancedLen})
			end -= enhancedLen
		}
	}
	return tags
}

func trailerEndingAt(win *Window, floor, end int64) (Region, bool) {
	room := end - floor
	if room >= id3v1Len && string(win.At(end-id3v1Len, 3)) == "TAG" {
		return Region{KindID3v1, end - id3v1Len, id3v1Len}, true
	}
	if room >= 9 {
		switch string(win.At(end-9, 9)) {
		case "LYRICS200":
			return lyrics3v2EndingAt(win, floor, end)
		case "LYRICSEND":
			return lyrics3v1EndingAt(win, floor, end)
		}
	}
	if room >= apeHeaderLen && string(win.At(end-apeHeaderLen, 8)) == "APETAGEX" {
		return apeEndingAt(win, floor, end)
	}
	if room >= id3HeaderLen && string(win.At(end-id3HeaderLen, 3)) == "3DI" {
		return id3EndingAt(win, floor, end)
	}
	return Region{}, false
}

func lyrics3v2EndingAt(win *Window, floor, end int64) (Region, bool) {
	if end-floor < 15 {
		return Region{}, false
	}
	size, err := strconv.ParseUint(string(win.At(end-15, 6)), 10, 32)
	if err != nil {
		return Region{}, false
	}
	total := int64(size) + 15
	if total > end-floor || string(win.At(end-total, 11)) != "LYRICSBEGIN" {
		return Region{}, false
	}
	return Region{KindLyrics3v2, end - total, total}, true
}

func lyrics3v1EndingAt(win *Window, floor, end int64) (Region, bool) {
	n := min(end-floor, maxLyrics3v1)
	i := bytes.LastIndex(win.At(end-n, int(n)), []byte("LYRICSBEGIN"))
	if i < 0 {
		return Region{}, false
	}
	start := end - n + int64(i)
	return Region{KindLyrics3v1, start, end - start}, true
}

func apeEndingAt(win *Window, floor, end int64) (Region, bool) {
	footer := win.At(end-apeHeaderLen, apeHeaderLen)
	if footer == nil {
		return Region{}, false
	}
	version, size, flags := apeFields(footer)
	if !validAPEVersion(version) || flags&apeFlagThisHeader != 0 || size < apeHeaderLen {
		return Region{}, false
	}
	total := int64(size)
	hasHeader := flags&apeFlagHasHeader != 0
	if hasHeader {
		total += apeHeaderLen
	}
	if total > end-floor {
		return Region{}, false
	}
	if hasHeader && string(win.At(end-total, 8)) != "APETAGEX" {
		return Region{}, false
	}
	return Region{KindAPE, end - total, total}, true
}

func id3EndingAt(win *Window, floor, end int64) (Region, bool) {
	footer := win.At(end-id3HeaderLen, id3HeaderLen)
	if footer == nil {
		return Region{}, false
	}
	size, ok := syncsafe(footer[6:])
	total := size + 2*id3HeaderLen
	if !ok || total > end-floor || string(win.At(end-total, 3)) != "ID3" {
		return Region{}, false
	}
	return Region{KindID3v2, end - total, total}, true
}

// LooksLikeTrailer reports whether tail, the end of a file, holds what
// TrailingTags treats as a tag. It only checks the signatures, so it errs towards
// true. TailSize is enough bytes to decide.
func LooksLikeTrailer(tail []byte) bool {
	n := len(tail)
	return n >= id3v1Len && string(tail[n-id3v1Len:n-id3v1Len+3]) == "TAG" ||
		n >= 9 && (string(tail[n-9:]) == "LYRICS200" || string(tail[n-9:]) == "LYRICSEND") ||
		n >= apeHeaderLen && string(tail[n-apeHeaderLen:n-apeHeaderLen+8]) == "APETAGEX" ||
		n >= id3HeaderLen && string(tail[n-id3HeaderLen:n-id3HeaderLen+3]) == "3DI"
}

// TailSize is how much of a file's end LooksLikeTrailer needs.
const TailSize = id3v1Len

// Describe adds findings for a tag. Only a cancelled context or a read failure
// makes it fail; damage inside the tag is itself reported.
func Describe(ctx context.Context, win *Window, r Region, where string, sink *Sink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sink.full() {
		sink.omitted++
		return nil
	}
	switch r.Kind {
	case KindID3v2:
		return describeID3v2(ctx, win, r, where, sink)
	case KindAPE:
		describeAPE(ctx, win, r, where, sink)
	case KindID3v1:
		describeID3v1(win, r, where, sink)
	case KindEnhanced:
		describeEnhanced(win, r, where, sink)
	case KindLyrics3v1, KindLyrics3v2:
		describeLyrics3(win, r, where, sink)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return win.Err()
}

func describeID3v1(win *Window, r Region, where string, sink *Sink) {
	b := win.At(r.Off, id3v1Len)
	if b == nil {
		return
	}
	where = Join(where, "ID3v1")
	sink.Add(id3v1TagField.Remove(core.ByteCount(r.Len), where))
	addIfSet(sink, TitleField, FixedText(b[3:33]), where)
	addIfSet(sink, ArtistField, FixedText(b[33:63]), where)
	addIfSet(sink, AlbumField, FixedText(b[63:93]), where)
	addIfSet(sink, yearField, FixedText(b[93:97]), where)
	addIfSet(sink, CommentField, FixedText(b[97:127]), where)
	// ID3v1.1 keeps a track number in the last two bytes of the comment field.
	if b[125] == 0 && b[126] != 0 {
		sink.Add(TrackField.Remove(strconv.Itoa(int(b[126])), where))
	}
	// 255 means no genre, and 0 is what an empty tag holds, even though it officially
	// means Blues. The number is shown as stored, since the names add nothing to a
	// report of what was removed.
	if genre := b[127]; genre != 0 && genre != 0xFF {
		sink.Add(GenreField.Remove(fmt.Sprintf("genre number %d", genre), where))
	}
}

func describeEnhanced(win *Window, r Region, where string, sink *Sink) {
	b := win.At(r.Off, enhancedLen)
	if b == nil {
		return
	}
	where = Join(where, "TAG+")
	sink.Add(enhancedField.Remove(core.ByteCount(r.Len), where))
	addIfSet(sink, TitleField, FixedText(b[4:64]), where)
	addIfSet(sink, ArtistField, FixedText(b[64:124]), where)
	addIfSet(sink, AlbumField, FixedText(b[124:184]), where)
	addIfSet(sink, GenreField, FixedText(b[185:215]), where)
}

func addIfSet(sink *Sink, field Field, value, where string) {
	if value != "" {
		sink.Add(field.Remove(value, where))
	}
}

func describeAPE(ctx context.Context, win *Window, r Region, where string, sink *Sink) {
	where = Join(where, "APE")
	sink.Add(apeTagField.Remove(core.ByteCount(r.Len), where))
	pos, end := r.Off, r.Off+r.Len
	if string(win.At(pos, 8)) == "APETAGEX" {
		pos += apeHeaderLen
	}
	if end-r.Off >= apeHeaderLen && string(win.At(end-apeHeaderLen, 8)) == "APETAGEX" {
		if footer := win.At(end-apeHeaderLen, apeHeaderLen); footer != nil {
			if _, _, flags := apeFields(footer); flags&apeFlagThisHeader == 0 {
				end -= apeHeaderLen
			}
		}
	}
	for pos < end {
		if ctx.Err() != nil {
			return
		}
		next, ok := describeAPEItem(win, pos, end, where, sink)
		if !ok {
			sink.Add(damagedTagField.Remove(core.ByteCount(end-pos), where))
			return
		}
		pos = next
	}
}

// describeAPEItem reports the item at pos and returns the offset of the next.
func describeAPEItem(win *Window, pos, end int64, where string, sink *Sink) (int64, bool) {
	hdr := win.At(pos, 8)
	if hdr == nil || end-pos < 8 {
		return 0, false
	}
	size := int64(binary.LittleEndian.Uint32(hdr))
	flags := binary.LittleEndian.Uint32(hdr[4:])
	keyArea := win.AtMost(pos+8, 256)
	nul := bytes.IndexByte(keyArea, 0)
	if nul < 0 {
		return 0, false
	}
	key := string(keyArea[:nul])
	valueOff := pos + 8 + int64(nul) + 1
	if valueOff > end || size > end-valueOff {
		return 0, false
	}
	const typeMask, typeBinary = 0x6, 0x2
	field, known := namedField(key)
	switch {
	case strings.HasPrefix(strings.ToUpper(key), "COVER ART"):
		sink.Add(PictureField.Remove(fmt.Sprintf("%s, %s", key, core.ByteCount(size)), Join(where, "Cover Art")))
	case flags&typeMask == typeBinary:
		sink.Add(otherTagField.Remove(fmt.Sprintf("%s: %s of binary data", key, core.ByteCount(size)), where))
	default:
		value := Latin1OrUTF8(win.At(valueOff, int(min(size, maxTagValue))))
		if known {
			sink.Add(field.Remove(namedValue(key, value), keyWhere(where, key)))
		} else {
			sink.Add(otherTagField.Remove(key+": "+value, where))
		}
	}
	return valueOff + size, true
}

func describeLyrics3(win *Window, r Region, where string, sink *Sink) {
	where = Join(where, "Lyrics3")
	sink.Add(lyrics3TagField.Remove(core.ByteCount(r.Len), where))
	b := win.At(r.Off, int(min(r.Len, maxFrameRead)))
	if len(b) < 11 {
		return
	}
	body := b[11:] // after LYRICSBEGIN
	if r.Kind == KindLyrics3v1 {
		addIfSet(sink, LyricsField, Latin1(bytes.TrimSuffix(body, []byte("LYRICSEND"))), where)
		return
	}
	// Each v2 field is a three-letter ID, a five-digit length and the data. The
	// six-digit size and the end marker follow the last one.
	for len(body) >= 8 {
		size, err := strconv.ParseUint(string(body[3:8]), 10, 32)
		if err != nil || size > uint64(len(body)-8) {
			return
		}
		if field, ok := lyrics3Fields[string(body[:3])]; ok {
			addIfSet(sink, field, Latin1(body[8:8+size]), where)
		}
		body = body[8+size:]
	}
}

// ScanTrailing reports everything from start to the end of the file, none of
// which is audio. Tags are decoded and whatever else is there is reported as
// trailing data, whose extent it returns for callers that want to examine it.
func ScanTrailing(ctx context.Context, win *Window, start int64, where string, sink *Sink) (unknownOff, unknownLen int64, err error) {
	tags := TrailingTags(win, start, win.Size())
	end := win.Size()
	if len(tags) > 0 {
		end = tags[len(tags)-1].Off
	}
	pos := start
	for pos < end {
		r, ok := TagAt(win, pos, end)
		if !ok {
			break
		}
		if err := Describe(ctx, win, r, where, sink); err != nil {
			return 0, 0, err
		}
		pos += r.Len
	}
	for i := len(tags) - 1; i >= 0; i-- {
		if err := Describe(ctx, win, tags[i], where, sink); err != nil {
			return 0, 0, err
		}
	}
	if err := win.Err(); err != nil {
		return 0, 0, err
	}
	if pos < end {
		sink.Add(TrailingField.Remove(core.ByteCount(end-pos), where))
	}
	return pos, end - pos, nil
}

// AllZero reports whether the n bytes at off are all zero.
func AllZero(ctx context.Context, win *Window, off, n int64) (bool, error) {
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		chunk := win.From(off)
		if chunk == nil {
			if err := win.Err(); err != nil {
				return false, err
			}
			return false, core.Malformedf("region at offset %d runs past the end of the file", off)
		}
		chunk = chunk[:min(int64(len(chunk)), n)]
		for _, c := range chunk {
			if c != 0 {
				return false, nil
			}
		}
		off += int64(len(chunk))
		n -= int64(len(chunk))
	}
	return true, nil
}
