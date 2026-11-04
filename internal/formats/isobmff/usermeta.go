package isobmff

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

// tagInfo says how to report one metadata tag. Everything here is only for the
// report: the boxes holding these tags are removed whole, whatever they contain.
type tagInfo struct {
	field    string
	category core.Category
	risk     core.Risk
}

// namedTags covers the QuickTime udta atoms, iTunes ilst atoms and vendor boxes
// that matter most. A tag that is not listed is still reported, under a generic
// field name, and removed the same way.
var namedTags = map[string]tagInfo{
	"\xa9mak": {"Camera make", core.CatDevice, core.RiskMed},
	"\xa9mod": {"Camera model", core.CatDevice, core.RiskMed},
	"\xa9swr": {"Software", core.CatSoftware, core.RiskLow},
	"\xa9too": {"Encoding tool", core.CatSoftware, core.RiskLow},
	"\xa9enc": {"Encoded by", core.CatIdentity, core.RiskMed},
	"\xa9day": {"Date", core.CatTime, core.RiskMed},
	"\xa9nam": {"Title", core.CatContent, core.RiskLow},
	"titl":    {"Title", core.CatContent, core.RiskLow},
	"\xa9alb": {"Album", core.CatContent, core.RiskLow},
	"albm":    {"Album", core.CatContent, core.RiskLow},
	"\xa9ART": {"Artist", core.CatIdentity, core.RiskHigh},
	"perf":    {"Artist", core.CatIdentity, core.RiskHigh},
	"aART":    {"Album artist", core.CatIdentity, core.RiskHigh},
	"\xa9aut": {"Author", core.CatIdentity, core.RiskHigh},
	"auth":    {"Author", core.CatIdentity, core.RiskHigh},
	"\xa9wrt": {"Composer", core.CatIdentity, core.RiskHigh},
	"cprt":    {"Copyright", core.CatIdentity, core.RiskHigh},
	"\xa9cpy": {"Copyright", core.CatIdentity, core.RiskHigh},
	"\xa9cmt": {"Comment", core.CatContent, core.RiskMed},
	"\xa9des": {"Description", core.CatContent, core.RiskMed},
	"desc":    {"Description", core.CatContent, core.RiskMed},
	"dscp":    {"Description", core.CatContent, core.RiskMed},
	"ldes":    {"Description", core.CatContent, core.RiskMed},
	"\xa9lyr": {"Lyrics", core.CatContent, core.RiskLow},
	"\xa9gen": {"Genre", core.CatContent, core.RiskLow},
	"gnre":    {"Genre", core.CatContent, core.RiskLow},
	"kywd":    {"Keywords", core.CatContent, core.RiskLow},
	"covr":    {"Cover art", core.CatThumbnail, core.RiskLow},
	"apID":    {"Purchase account", core.CatIdentity, core.RiskHigh},
	"ownr":    {"Owner name", core.CatIdentity, core.RiskHigh},
	"purd":    {"Purchase date", core.CatTime, core.RiskMed},
	"GPMF":    {"GoPro metadata", core.CatDevice, core.RiskHigh},
	"FIRM":    {"Camera firmware", core.CatDevice, core.RiskMed},
	"CAME":    {"Camera identifier", core.CatDevice, core.RiskHigh},
	"MUID":    {"Media unique ID", core.CatHidden, core.RiskMed},
	"LENS":    {"Lens", core.CatDevice, core.RiskMed},
}

// quickTimeKeys are the reverse-DNS names phones use in moov/meta. The key names
// are the same on iPhone and many Android builds.
var quickTimeKeys = map[string]tagInfo{
	"com.apple.quicktime.make":                         {"Camera make", core.CatDevice, core.RiskMed},
	"com.apple.quicktime.model":                        {"Camera model", core.CatDevice, core.RiskMed},
	"com.android.manufacturer":                         {"Camera make", core.CatDevice, core.RiskMed},
	"com.android.model":                                {"Camera model", core.CatDevice, core.RiskMed},
	"com.apple.quicktime.software":                     {"Software", core.CatSoftware, core.RiskLow},
	"com.android.version":                              {"Software", core.CatSoftware, core.RiskLow},
	"com.apple.quicktime.creationdate":                 {"Date", core.CatTime, core.RiskMed},
	"com.apple.quicktime.title":                        {"Title", core.CatContent, core.RiskLow},
	"com.apple.quicktime.author":                       {"Author", core.CatIdentity, core.RiskHigh},
	"com.apple.quicktime.artist":                       {"Artist", core.CatIdentity, core.RiskHigh},
	"com.apple.quicktime.displayname":                  {"Title", core.CatContent, core.RiskLow},
	"com.apple.quicktime.comment":                      {"Comment", core.CatContent, core.RiskMed},
	"com.apple.quicktime.description":                  {"Description", core.CatContent, core.RiskMed},
	"com.apple.quicktime.location.name":                {"Location name", core.CatLocation, core.RiskHigh},
	"com.apple.quicktime.location.accuracy.horizontal": {"GPS accuracy", core.CatLocation, core.RiskMed},
	"com.apple.quicktime.artwork":                      {"Cover art", core.CatThumbnail, core.RiskLow},
}

// locationTags hold an ISO 6709 string under various names.
var locationTags = map[string]bool{
	"\xa9xyz":                              true,
	"com.apple.quicktime.location.ISO6709": true,
	"location":                             true,
}

// threeGPPStrings are the 3GPP asset boxes that hold a language-coded string.
var threeGPPStrings = typeSet("titl", "dscp", "cprt", "perf", "auth", "gnre", "albm", "kywd")

// xmpUUID is the extended type Adobe registered for XMP in ISOBMFF files.
var xmpUUID = []byte{0xBE, 0x7A, 0xCF, 0xCB, 0x97, 0xA9, 0x42, 0xE8, 0x9C, 0x71, 0x99, 0x94, 0x91, 0xE3, 0xAF, 0xAC}

// describeRemoved reports what a box being removed held. payload may be only the
// start of the box, while size is its true payload size.
func (a *analysis) describeRemoved(typ string, payload []byte, size int64, where string) {
	switch typ {
	case "udta":
		a.describeContainer(payload, where+"/udta", "User data", a.describeUserData)
	case "meta":
		a.describeContainer(payload, where+"/meta", "Metadata box", a.describeMeta)
	case "uuid":
		a.describeUUID(payload, size, where)
	case "XMP_":
		a.addXMP(payload, where+"/XMP_")
	case "Xtra":
		a.remove(core.CatHidden, core.RiskMed, "Windows metadata", fmt.Sprintf("%d bytes", size), where+"/Xtra")
	case "iods":
		// Nothing decodes with it, and its descriptors can hold names and URLs.
		a.remove(core.CatTechnical, core.RiskLow, "MPEG-4 object descriptor", fmt.Sprintf("%d bytes", size), where)
	case "mpvd":
		// Samsung and Google motion photos keep a whole video in this box.
		a.remove(core.CatHidden, core.RiskHigh, "Motion photo video", fmt.Sprintf("%d bytes", size), where)
	default:
		a.remove(core.CatHidden, core.RiskMed, "Unknown box", fmt.Sprintf("%q, %d bytes", typ, size), where)
	}
}

// describeContainer decodes a box's children, and falls back to a generic finding
// if that produced nothing, so a box holding anything at all is always reported.
func (a *analysis) describeContainer(payload []byte, where, field string, decode func([]byte, string)) {
	before := a.emitted
	decode(payload, where)
	if a.emitted == before && !core.AllZero(payload) {
		a.remove(core.CatHidden, core.RiskMed, field, fmt.Sprintf("%d bytes", len(payload)), where)
	}
}

func (a *analysis) describeUUID(payload []byte, size int64, where string) {
	if len(payload) >= len(xmpUUID) && string(payload[:len(xmpUUID)]) == string(xmpUUID) {
		a.addXMP(payload[len(xmpUUID):], where+"/XMP")
		return
	}
	if xmp.Contains(payload) {
		a.addXMP(payload, where+"/uuid")
		return
	}
	value := fmt.Sprintf("%d bytes", size)
	if len(payload) >= 16 {
		value = hex.EncodeToString(payload[:16]) + ", " + value
	}
	a.remove(core.CatHidden, core.RiskMed, "Vendor UUID box", value, where)
}

func (a *analysis) addXMP(packet []byte, where string) {
	for _, f := range xmp.Findings(packet, where) {
		a.add(f)
	}
}

// describeUserData reports the children of a udta box.
func (a *analysis) describeUserData(p []byte, where string) {
	eachBox(p, func(typ string, payload []byte) {
		switch {
		case typ == "meta":
			a.describeContainer(payload, where+"/meta", "Metadata box", a.describeMeta)
		case typ == "XMP_":
			a.addXMP(payload, where+"/XMP_")
		case typ == "loci":
			a.addLocationInfo(payload, where+"/loci")
		case threeGPPStrings[typ]:
			a.addTag(typ, languageString(payload), where)
		case typ[0] == 0xA9:
			a.addTag(typ, internationalText(payload), where)
		case typ == "Xtra":
			a.remove(core.CatHidden, core.RiskMed, "Windows metadata", fmt.Sprintf("%d bytes", len(payload)), where+"/Xtra")
		default:
			a.addTag(typ, fmt.Sprintf("%d bytes", len(payload)), where)
		}
	})
}

// describeMeta reports a meta box, which on a phone or in iTunes holds an ilst of
// tags, named either by four-byte codes or through a separate keys table.
func (a *analysis) describeMeta(p []byte, where string) {
	var keys []string
	var ilst []byte
	eachBox(metaBody(p), func(typ string, payload []byte) {
		switch typ {
		case "hdlr", "free", "skip":
		case "keys":
			keys = parseKeys(payload)
		case "ilst":
			ilst = payload
		case "xml ", "bxml":
			if xmp.Contains(payload) {
				a.addXMP(payload, where+"/"+typ)
				return
			}
			a.remove(core.CatHidden, core.RiskMed, "XML metadata", fmt.Sprintf("%d bytes", len(payload)), where)
		default:
			a.remove(core.CatHidden, core.RiskMed, "Metadata box", fmt.Sprintf("%q, %d bytes", typ, len(payload)), where)
		}
	})
	eachBox(ilst, func(typ string, payload []byte) {
		a.describeItem(typ, payload, keys, where+"/ilst")
	})
}

// parseKeys reads a keys table. The first key has index 1.
func parseKeys(p []byte) []string {
	c := cursor{b: p}
	c.take(4)
	count := c.u32()
	var keys []string
	for i := uint32(0); i < count; i++ {
		size := c.u32()
		if c.bad || size < 8 || int64(size)-4 > int64(c.remaining()) {
			break
		}
		entry := c.take(int(size) - 4)
		keys = append(keys, string(entry[4:])) // after the namespace, such as "mdta"
	}
	return keys
}

func (a *analysis) describeItem(typ string, payload []byte, keys []string, where string) {
	if typ == "----" {
		a.describeFreeform(payload, where)
		return
	}
	name := typ
	if index := binary.BigEndian.Uint32([]byte(typ)); index >= 1 && index <= uint32(len(keys)) {
		name = keys[index-1]
	}
	var value string
	eachBox(payload, func(child string, data []byte) {
		if child == "data" && value == "" {
			value = dataValue(data)
		}
	})
	a.addTag(name, value, where)
}

// describeFreeform reports an iTunes "----" tag, named by its mean and name
// children. iTunSMPB describes how much silence to trim. It cannot stay when the
// box around it goes, and a player without it may add a little silence, so that
// gets its own warning.
func (a *analysis) describeFreeform(payload []byte, where string) {
	var name, value string
	eachBox(payload, func(child string, data []byte) {
		switch {
		case child == "name" && len(data) > 4:
			name = string(data[4:])
		case child == "data" && value == "":
			value = dataValue(data)
		}
	})
	if name == "iTunSMPB" {
		a.report.Warn("gapless playback information (iTunSMPB) was removed with the other tags; a player may add a short silence at the start")
	}
	a.addTag(name, value, where)
}

// dataValue decodes the payload of an iTunes-style data box: a 4-byte type, a
// 4-byte locale and the value.
func dataValue(p []byte) string {
	if len(p) < 8 {
		return ""
	}
	dataType := binary.BigEndian.Uint32(p) & 0xFFFFFF
	v := p[8:]
	switch dataType {
	case 1:
		return string(v)
	case 2:
		return decodeUTF16BE(v)
	case 13:
		return fmt.Sprintf("JPEG image, %d bytes", len(v))
	case 14:
		return fmt.Sprintf("PNG image, %d bytes", len(v))
	case 21, 22:
		if len(v) <= 8 {
			return strconv.FormatUint(bigEndian(v), 10)
		}
	case 23:
		if len(v) == 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(v))), 'f', -1, 32)
		}
	case 24:
		if len(v) == 8 {
			return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(v)), 'f', -1, 64)
		}
	case 0:
		if utf8.Valid(v) {
			return string(v)
		}
	}
	return fmt.Sprintf("%d bytes", len(v))
}

func decodeUTF16BE(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

// internationalText decodes a QuickTime text atom: a 2-byte length, a 2-byte
// language code and the text, possibly repeated per language. Only the first
// entry is shown, since the rest are translations of the same value.
func internationalText(p []byte) string {
	if len(p) >= 4 {
		if n := int(binary.BigEndian.Uint16(p)); n <= len(p)-4 {
			return string(p[4 : 4+n])
		}
	}
	return string(p)
}

// languageString decodes a 3GPP asset box: a full box header, a 2-byte language
// code and a string in UTF-8 or, after a byte order mark, UTF-16.
func languageString(p []byte) string {
	if len(p) < 6 {
		return ""
	}
	s := p[6:]
	if len(s) >= 2 && s[0] == 0xFE && s[1] == 0xFF {
		return decodeUTF16BE(s[2:])
	}
	return trimmedText(s)
}

// addLocationInfo decodes a 3GPP loci box: a full box header, a language code, a
// name, a role byte, then longitude, latitude and altitude as 16.16 fixed point.
func (a *analysis) addLocationInfo(p []byte, where string) {
	c := cursor{b: p}
	c.take(6)
	name := c.cstring()
	c.u8()
	lon := float64(int32(c.u32())) / 65536
	lat := float64(int32(c.u32())) / 65536
	if c.bad {
		a.remove(core.CatLocation, core.RiskHigh, "GPS coordinates", fmt.Sprintf("%d bytes, could not be read", len(p)), where)
		return
	}
	a.remove(core.CatLocation, core.RiskHigh, "GPS coordinates", formatCoordinates(lat, lon), where)
	if name != "" {
		a.remove(core.CatLocation, core.RiskHigh, "Location name", name, where)
	}
}

// addTag reports one named value.
func (a *analysis) addTag(name, value, where string) {
	if locationTags[name] {
		a.addLocation(value, where)
		return
	}
	if info, ok := namedTags[name]; ok {
		a.remove(info.category, info.risk, info.field, value, where)
		return
	}
	if info, ok := quickTimeKeys[name]; ok {
		a.remove(info.category, info.risk, info.field, value, where)
		return
	}
	a.remove(core.CatHidden, core.RiskMed, "Other metadata tag", strings.ToValidUTF8(name, "?")+": "+value, where)
}

// addLocation reports an ISO 6709 string as coordinates. One that does not parse
// is still reported as location, showing the raw text.
func (a *analysis) addLocation(value, where string) {
	lat, lon, alt, hasAlt, ok := parseISO6709(value)
	if !ok {
		a.remove(core.CatLocation, core.RiskHigh, "GPS coordinates", value, where)
		return
	}
	a.remove(core.CatLocation, core.RiskHigh, "GPS coordinates", formatCoordinates(lat, lon), where)
	if hasAlt {
		a.remove(core.CatLocation, core.RiskMed, "GPS altitude", strconv.FormatFloat(alt, 'f', -1, 64)+" m", where)
	}
}
