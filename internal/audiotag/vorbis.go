package audiotag

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
)

var (
	otherTagField    = Field{"Other tag", core.CatContent, core.RiskMed}
	damagedTagField  = Field{"Undecoded tag data", core.CatHidden, core.RiskMed}
	loudnessTagField = Field{"Loudness tag", core.CatTechnical, core.RiskNone}
	databaseTagField = Field{"Music database ID", core.CatContent, core.RiskLow}
)

// namedFields maps the free-form key names of Vorbis comments and APE tags, in
// upper case, to what they expose. Both formats leave the keys up to the writer,
// so this covers the common ones and otherTagField shows the rest as written.
var namedFields = map[string]Field{
	"TITLE":             TitleField,
	"SUBTITLE":          {"Subtitle", core.CatContent, core.RiskLow},
	"VERSION":           {"Version", core.CatContent, core.RiskLow},
	"ALBUM":             AlbumField,
	"ARTIST":            ArtistField,
	"ALBUMARTIST":       {"Album artist", core.CatIdentity, core.RiskHigh},
	"ALBUM ARTIST":      {"Album artist", core.CatIdentity, core.RiskHigh},
	"ALBUM_ARTIST":      {"Album artist", core.CatIdentity, core.RiskHigh},
	"PERFORMER":         {"Performer", core.CatIdentity, core.RiskHigh},
	"COMPOSER":          {"Composer", core.CatIdentity, core.RiskHigh},
	"CONDUCTOR":         {"Conductor", core.CatIdentity, core.RiskHigh},
	"LYRICIST":          {"Lyricist", core.CatIdentity, core.RiskHigh},
	"ARRANGER":          {"Arranger", core.CatIdentity, core.RiskHigh},
	"REMIXER":           {"Remixer", core.CatIdentity, core.RiskHigh},
	"ENGINEER":          {"Engineer", core.CatIdentity, core.RiskHigh},
	"PRODUCER":          {"Producer", core.CatIdentity, core.RiskHigh},
	"AUTHOR":            {"Author", core.CatIdentity, core.RiskHigh},
	"WRITER":            {"Writer", core.CatIdentity, core.RiskHigh},
	"CONTACT":           {"Contact", core.CatIdentity, core.RiskHigh},
	"EMAIL":             {"Contact", core.CatIdentity, core.RiskHigh},
	"LOCATION":          {"Location", core.CatLocation, core.RiskHigh},
	"RECORD LOCATION":   {"Location", core.CatLocation, core.RiskHigh},
	"RECORDINGLOCATION": {"Location", core.CatLocation, core.RiskHigh},
	"COUNTRY":           {"Country", core.CatLocation, core.RiskMed},
	"COPYRIGHT":         CopyrightField,
	"LICENSE":           {"License", core.CatIdentity, core.RiskLow},
	"ORGANIZATION":      {"Organization", core.CatIdentity, core.RiskMed},
	"PUBLISHER":         {"Publisher", core.CatIdentity, core.RiskMed},
	"LABEL":             {"Publisher", core.CatIdentity, core.RiskMed},
	"COMMENT":           CommentField,
	"COMMENTS":          CommentField,
	"DESCRIPTION":       {"Description", core.CatContent, core.RiskMed},
	"SUBJECT":           {"Subject", core.CatContent, core.RiskMed},
	"DATE":              DateField,
	"YEAR":              DateField,
	"RECORD DATE":       DateField,
	"RECORDINGDATE":     DateField,
	"CREATION_TIME":     DateField,
	"ORIGINALDATE":      {"Original release date", core.CatTime, core.RiskLow},
	"ENCODED_BY":        {"Encoded by", core.CatIdentity, core.RiskHigh},
	"ENCODEDBY":         {"Encoded by", core.CatIdentity, core.RiskHigh},
	"ENCODER":           EncoderField,
	"ENCODER_OPTIONS":   {"Encoder settings", core.CatSoftware, core.RiskLow},
	"ENCODERSETTINGS":   {"Encoder settings", core.CatSoftware, core.RiskLow},
	"GENRE":             GenreField,
	"TRACK":             TrackField,
	"TRACKNUMBER":       TrackField,
	"TRACKTOTAL":        TrackField,
	"TOTALTRACKS":       TrackField,
	"DISC":              {"Disc number", core.CatContent, core.RiskLow},
	"DISCNUMBER":        {"Disc number", core.CatContent, core.RiskLow},
	"DISCTOTAL":         {"Disc number", core.CatContent, core.RiskLow},
	"TOTALDISCS":        {"Disc number", core.CatContent, core.RiskLow},
	"ISRC":              {"ISRC", core.CatContent, core.RiskLow},
	"BARCODE":           {"Barcode", core.CatContent, core.RiskLow},
	"LANGUAGE":          {"Language", core.CatContent, core.RiskLow},
	"LYRICS":            LyricsField,
	"UNSYNCEDLYRICS":    LyricsField,
	"URL":               URLField,
	"WEBSITE":           URLField,
	"FILE":              {"Original file name", core.CatIdentity, core.RiskHigh},
}

// namedField finds the field for a tag key, by its exact name or by the families
// of machine-generated keys.
func namedField(key string) (Field, bool) {
	upper := strings.ToUpper(key)
	if f, ok := namedFields[upper]; ok {
		return f, true
	}
	switch {
	case strings.HasPrefix(upper, "REPLAYGAIN_"), strings.HasPrefix(upper, "R128_"):
		return loudnessTagField, true
	case strings.HasPrefix(upper, "MUSICBRAINZ_"), strings.HasPrefix(upper, "ACOUSTID_"), strings.HasPrefix(upper, "DISCOGS_"):
		return databaseTagField, true
	}
	return Field{}, false
}

// keyWhere locates a tag for a finding. clean prints Where, and the keys of these
// formats are whatever the writer wants, so only a name from namedFields goes in it,
// in one case, since even the choice of case is the writer's.
func keyWhere(where, key string) string {
	upper := strings.ToUpper(key)
	if _, ok := namedFields[upper]; ok {
		return Join(where, upper)
	}
	return where
}

// namedValue is the value to report for a key namedField knows. One known only by its
// family's prefix has a part chosen by the writer, which only Value can show.
func namedValue(key, value string) string {
	if _, ok := namedFields[strings.ToUpper(key)]; ok {
		return value
	}
	return key + ": " + value
}

// VorbisComments reports a Vorbis comment structure, as found in a FLAC block and
// in Ogg comment packets once their magic is stripped. It returns the bytes after
// the last comment, which the caller judges: Ogg Vorbis has a framing bit there.
// An empty vendor and no comments report nothing, because that is what cleaning
// leaves behind.
func VorbisComments(data []byte, where string, sink *Sink) []byte {
	vendor, rest, ok := lengthPrefixed(data)
	if !ok {
		sink.Add(damagedTagField.Remove(core.ByteCount(int64(len(data))), where))
		return nil
	}
	if len(vendor) > 0 {
		sink.Add(EncoderField.Remove(Latin1OrUTF8(vendor), Join(where, "vendor")))
	}
	if len(rest) < 4 {
		sink.Add(damagedTagField.Remove("comment count is missing", where))
		return nil
	}
	count := binary.LittleEndian.Uint32(rest)
	rest = rest[4:]
	for i := range count {
		var comment []byte
		comment, rest, ok = lengthPrefixed(rest)
		if !ok {
			sink.Add(damagedTagField.Remove(fmt.Sprintf("%d of %d comments are unreadable, %s", count-i, count, core.ByteCount(int64(len(rest)))), where))
			return nil
		}
		reportComment(string(comment), where, sink)
	}
	return rest
}

func reportComment(comment, where string, sink *Sink) {
	key, value, found := strings.Cut(comment, "=")
	if !found {
		sink.Add(otherTagField.Remove(comment, where))
		return
	}
	switch upper := strings.ToUpper(key); upper {
	case "METADATA_BLOCK_PICTURE":
		sink.Add(PictureField.Remove(vorbisPicture(value), Join(where, upper)))
		return
	case "COVERART", "COVERARTMIME":
		sink.Add(PictureField.Remove(fmt.Sprintf("%d bytes of %s", len(value), upper), Join(where, upper)))
		return
	}
	if field, ok := namedField(key); ok {
		sink.Add(field.Remove(namedValue(key, value), keyWhere(where, key)))
		return
	}
	sink.Add(otherTagField.Remove(key+": "+value, where))
}

// Latin1OrUTF8 displays a string that should be UTF-8 but might be anything,
// without losing the characters of a Latin-1 string to the sanitiser.
func Latin1OrUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return Latin1(b)
}

// lengthPrefixed splits off a string with a 32-bit little-endian length.
func lengthPrefixed(b []byte) (value, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := uint64(binary.LittleEndian.Uint32(b))
	b = b[4:]
	if n > uint64(len(b)) {
		return nil, nil, false
	}
	return b[:n], b[n:], true
}
