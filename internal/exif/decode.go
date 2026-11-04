package exif

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxTextBytes bounds the bytes decoded from one value. Hostile entries can all
// point at the same huge region, so decoding must not scale with its size, and
// core.SafeText shows only a couple of hundred characters of the result anyway.
const maxTextBytes = 4096

// undecodable is the value shown for data that is present but unusable, such as
// a coordinate of 0/0. Saying so beats hiding that the tag exists.
const undecodable = "present but not decodable"

// number returns the value of a single SHORT, LONG or IFD element.
func (e entry) number(order binary.ByteOrder) (uint32, bool) {
	if e.count != 1 || e.value == nil {
		return 0, false
	}
	switch e.typ {
	case typeShort:
		return uint32(order.Uint16(e.value)), true
	case typeLong, typeIFD:
		return order.Uint32(e.value), true
	}
	return 0, false
}

// sum adds up an array of SHORT or LONG values, as in a strip byte-count table.
func (e entry) sum(order binary.ByteOrder) uint64 {
	var total uint64
	switch e.typ {
	case typeShort:
		for b := e.value; len(b) >= 2; b = b[2:] {
			total += uint64(order.Uint16(b))
		}
	case typeLong:
		for b := e.value; len(b) >= 4; b = b[4:] {
			total += uint64(order.Uint32(b))
		}
	}
	return total
}

// byteSize is the size the entry declares, which may exceed what the block
// holds. Types TIFF does not define count as one byte per element.
func (e entry) byteSize() uint64 {
	if size := TypeSize(e.typ); size != 0 {
		return uint64(e.count) * size
	}
	return uint64(e.count)
}

// holdsText reports whether the value is in a type that can carry characters.
// Text in any other type would decode as noise.
func (e entry) holdsText() bool {
	return e.typ == typeASCII || e.typ == typeByte || e.typ == typeUndefined
}

func textValue(e entry, _ binary.ByteOrder) string {
	if !e.holdsText() {
		return ""
	}
	return decodeText(e.value)
}

// decodeText reads a NUL-terminated string. EXIF says these are ASCII, but real
// files hold UTF-8, and older cameras and editors write Latin-1 (the copyright
// sign above all), which would otherwise lose those letters to SafeText.
func decodeText(b []byte) string {
	b = clipUTF8(b, maxTextBytes)
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	if utf8.Valid(b) {
		return strings.TrimSpace(string(b))
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return strings.TrimSpace(string(runes))
}

// clipUTF8 shortens b to at most limit bytes without splitting a final rune,
// which would make a valid UTF-8 value look like Latin-1.
func clipUTF8(b []byte, limit int) []byte {
	if len(b) <= limit {
		return b
	}
	b = b[:limit]
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				b = b[:i]
			}
			break
		}
	}
	return b
}

// utf16LEValue decodes the Windows XP tags. They are UTF-16LE whatever the byte
// order of the surrounding block, which is why this ignores the block's order.
func utf16LEValue(e entry, _ binary.ByteOrder) string {
	if !e.holdsText() {
		return ""
	}
	return decodeUTF16(e.value, binary.LittleEndian)
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		b, order = b[2:], binary.LittleEndian
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		b, order = b[2:], binary.BigEndian
	}
	b = b[:min(len(b), maxTextBytes)]
	units := make([]uint16, 0, len(b)/2)
	for ; len(b) >= 2; b = b[2:] {
		unit := order.Uint16(b)
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return strings.TrimSpace(string(utf16.Decode(units)))
}

// userCommentCodeSize is the length of the character-code field that begins a
// UserComment.
const userCommentCodeSize = 8

func userCommentValue(e entry, order binary.ByteOrder) string {
	if !e.holdsText() {
		return ""
	}
	if len(e.value) < userCommentCodeSize {
		return decodeText(e.value)
	}
	code, body := e.value[:userCommentCodeSize], e.value[userCommentCodeSize:]
	switch string(code) {
	case "ASCII\x00\x00\x00", "\x00\x00\x00\x00\x00\x00\x00\x00":
		return decodeText(body)
	case "UNICODE\x00":
		return decodeUTF16(body, order)
	case "JIS\x00\x00\x00\x00\x00":
		// The standard library has no JIS decoder, and printing the raw bytes
		// would show nothing useful, but the user should still know it is text.
		return fmt.Sprintf("JIS-encoded text, %d bytes", len(body))
	}
	// A writer that ignored the code field has put text straight in the value.
	return decodeText(e.value)
}

func sizeValue(e entry, _ binary.ByteOrder) string {
	return formatSize(e.byteSize())
}

func formatSize(n uint64) string {
	return strconv.FormatUint(n, 10) + " bytes"
}

// rational returns element i of a RATIONAL or SRATIONAL array. A zero
// denominator has no value; writers use 0/0 to mean "unknown".
func (e entry) rational(i int, order binary.ByteOrder) (float64, bool) {
	if (i+1)*8 > len(e.value) {
		return 0, false
	}
	raw := e.value[i*8:]
	switch e.typ {
	case typeRational:
		num, den := order.Uint32(raw), order.Uint32(raw[4:])
		if den == 0 {
			return 0, false
		}
		return float64(num) / float64(den), true
	case typeSRational:
		num, den := int32(order.Uint32(raw)), int32(order.Uint32(raw[4:]))
		if den == 0 {
			return 0, false
		}
		return float64(num) / float64(den), true
	}
	return 0, false
}

// coordinate converts a GPS degrees/minutes/seconds array to signed decimal
// degrees. Only the degrees are required: some writers store decimal minutes
// and leave the seconds as 0/0.
//
// The sign comes from the reference, or from the degrees for a writer that used
// signed rationals against the specification. It applies to the whole value:
// 33 degrees 30 minutes south is -33.5, not -33 + 0.5.
func coordinate(value, ref entry, order binary.ByteOrder) (float64, bool) {
	degrees, ok := value.rational(0, order)
	if !ok {
		return 0, false
	}
	minutes, _ := value.rational(1, order)
	seconds, _ := value.rational(2, order)
	result := math.Abs(degrees) + math.Abs(minutes)/60 + math.Abs(seconds)/3600
	r := strings.ToUpper(textValue(ref, order))
	southOrWest := strings.HasPrefix(r, "S") || strings.HasPrefix(r, "W")
	// The zero check keeps a value of exactly zero from printing as "-0.000000".
	if (southOrWest || degrees < 0) && result != 0 {
		result = -result
	}
	return result, true
}

// coordinatesValue formats the pair for display, naming whichever half is alone
// and never hiding a coordinate that exists but cannot be decoded.
func coordinatesValue(lat, latRef, lon, lonRef entry, order binary.ByteOrder) string {
	latitude, latOK := coordinate(lat, latRef, order)
	longitude, lonOK := coordinate(lon, lonRef, order)
	switch {
	case latOK && lonOK:
		return formatDegrees(latitude) + ", " + formatDegrees(longitude)
	case latOK:
		return "latitude " + formatDegrees(latitude)
	case lonOK:
		return "longitude " + formatDegrees(longitude)
	}
	return undecodable
}

func formatDegrees(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

func altitudeValue(value, ref entry, order binary.ByteOrder) string {
	metres, ok := value.rational(0, order)
	if !ok {
		return undecodable
	}
	// A reference of 1 means below sea level. BYTE and UNDEFINED hold it alike.
	if len(ref.value) > 0 && ref.value[0] == 1 {
		metres = -metres
	}
	return strconv.FormatFloat(metres, 'f', 1, 64) + " m"
}

func gpsTimeValue(e entry, order binary.ByteOrder) string {
	hours, hoursOK := e.rational(0, order)
	minutes, minutesOK := e.rational(1, order)
	seconds, secondsOK := e.rational(2, order)
	if !hoursOK || !minutesOK || !secondsOK {
		return undecodable
	}
	return fmt.Sprintf("%02d:%02d:%02d UTC", int64(hours), int64(minutes), int64(seconds))
}
