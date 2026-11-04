package exif

import (
	"encoding/binary"
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
)

// tagSpec describes one tag worth naming in a report.
type tagSpec struct {
	tag      uint16
	field    string
	category core.Category
	risk     core.Risk
	decode   func(entry, binary.ByteOrder) string
}

// namedTags lists the tags reported individually, in report order. IFD0, the
// Exif IFD and IFD1 share these numbers, and all three are read with this table
// because nothing stops a writer putting Artist in IFD1. Everything not listed
// still counts toward the "Other EXIF tags" finding.
var namedTags = []tagSpec{
	{0x013B, "Artist", core.CatIdentity, core.RiskHigh, textValue},
	{0xA430, "Owner name", core.CatIdentity, core.RiskHigh, textValue},
	{0xFDE8, "Owner name", core.CatIdentity, core.RiskHigh, textValue}, // written by Adobe tools
	{0xA437, "Photographer", core.CatIdentity, core.RiskHigh, textValue},
	{0xA438, "Image editor", core.CatIdentity, core.RiskHigh, textValue},
	{0x9C9D, "XP author", core.CatIdentity, core.RiskHigh, utf16LEValue},
	{0x8298, "Copyright", core.CatIdentity, core.RiskHigh, textValue},

	{0xA431, "Body serial number", core.CatDevice, core.RiskHigh, textValue},
	{0xA435, "Lens serial number", core.CatDevice, core.RiskHigh, textValue},
	{0xC62F, "Camera serial number", core.CatDevice, core.RiskHigh, textValue}, // DNG
	{0xFDE9, "Camera serial number", core.CatDevice, core.RiskHigh, textValue}, // written by Adobe tools
	{0x927C, "MakerNote", core.CatDevice, core.RiskHigh, sizeValue},            // commonly holds serials
	{0x010F, "Camera make", core.CatDevice, core.RiskMed, textValue},
	{0x0110, "Camera model", core.CatDevice, core.RiskMed, textValue},
	{0xA433, "Lens make", core.CatDevice, core.RiskLow, textValue},
	{0xA434, "Lens model", core.CatDevice, core.RiskMed, textValue},
	{0xFDEA, "Lens model", core.CatDevice, core.RiskMed, textValue}, // written by Adobe tools
	{0xA439, "Camera firmware", core.CatDevice, core.RiskLow, textValue},
	{0x013C, "Host computer", core.CatDevice, core.RiskMed, textValue},

	{0x9003, "Date taken", core.CatTime, core.RiskMed, textValue},
	{0x9004, "Date digitized", core.CatTime, core.RiskMed, textValue},
	{0x0132, "Date modified", core.CatTime, core.RiskMed, textValue},
	// A time zone narrows down where the picture was taken, so it is location.
	{0x9011, "Time zone taken", core.CatLocation, core.RiskMed, textValue},
	{0x9012, "Time zone digitized", core.CatLocation, core.RiskMed, textValue},
	{0x9010, "Time zone", core.CatLocation, core.RiskMed, textValue},

	{0x0131, "Software", core.CatSoftware, core.RiskLow, textValue},
	{0xA43A, "Raw developing software", core.CatSoftware, core.RiskLow, textValue},
	{0xA43B, "Image editing software", core.CatSoftware, core.RiskLow, textValue},
	{0xA43C, "Metadata editing software", core.CatSoftware, core.RiskLow, textValue},

	// Free text can say anything, including a name, so it is rated above a title.
	{0x010E, "Image description", core.CatContent, core.RiskMed, textValue},
	{0x9286, "User comment", core.CatContent, core.RiskMed, userCommentValue},
	{0x9C9C, "XP comment", core.CatContent, core.RiskMed, utf16LEValue},
	{0x010D, "Document name", core.CatContent, core.RiskMed, textValue}, // often a file path
	{0xA436, "Image title", core.CatContent, core.RiskLow, textValue},
	{0x9C9B, "XP title", core.CatContent, core.RiskLow, utf16LEValue},
	{0x9C9E, "XP keywords", core.CatContent, core.RiskLow, utf16LEValue},
	{0x9C9F, "XP subject", core.CatContent, core.RiskLow, utf16LEValue},

	// An identifier links this copy to every other copy of the same picture.
	{0xA420, "Image unique ID", core.CatHidden, core.RiskMed, textValue},
	// Other metadata formats nested inside TIFF tags: only the size is reported
	// because their content is opaque here, and they go with the block.
	{0x02BC, "Embedded XMP", core.CatHidden, core.RiskMed, sizeValue},
	{0x83BB, "Embedded IPTC", core.CatHidden, core.RiskMed, sizeValue},
	{0x8649, "Embedded Photoshop data", core.CatHidden, core.RiskMed, sizeValue},
}

// GPS IFD tags that are decoded rather than counted.
const (
	gpsLatitudeRef  = 0x0001
	gpsLatitude     = 0x0002
	gpsLongitudeRef = 0x0003
	gpsLongitude    = 0x0004
	gpsAltitudeRef  = 0x0005
	gpsAltitude     = 0x0006
	gpsTimeStamp    = 0x0007
	gpsDateStamp    = 0x001D
)

// Thumbnail tags, meaningful in IFD1 and later only: the same strip tags in IFD0
// of a full TIFF describe the main image.
const (
	tagStripOffsets    = 0x0111
	tagStripByteCounts = 0x0117
	tagJPEGOffset      = 0x0201
	tagJPEGLength      = 0x0202
)

// findingList accumulates findings and the catch-all counts for one Findings
// call. Values are sanitised here as well as in core.Report.Add so that a
// caller can print them directly.
type findingList struct {
	out      []core.Finding
	other    int
	otherGPS int
}

func (l *findingList) add(category core.Category, risk core.Risk, field, value, where string) {
	l.out = append(l.out, core.Finding{
		Category: category,
		Risk:     risk,
		Field:    field,
		Value:    core.SafeText(value),
		Where:    core.SafeText(where),
		Action:   core.ActionRemove,
	})
}

// Findings describes the block. Orientation is ActionRetain with a Reason when
// Replacement would keep it; every other finding is ActionRemove.
//
// The result holds a removal unless the block is exactly what Minimal produces
// and is being retained, so verifying a handler's output with Inspect cannot
// pass while anything else is left in an EXIF block, even hidden in bytes no tag
// refers to. Findings in a report are in a fixed order for any given block.
func (d *Data) Findings(where string, opts core.Options) []core.Finding {
	var l findingList
	for i := range d.dirs {
		dir := &d.dirs[i]
		loc := joinWhere(where, dir.name)
		switch dir.kind {
		case kindGPS:
			d.addGPS(&l, dir, loc)
		case kindInterop:
			l.other += len(dir.entries)
		default:
			d.addNamed(&l, dir, loc, opts)
		}
	}
	if l.otherGPS > 0 {
		l.add(core.CatLocation, core.RiskMed, "Other GPS tags", countValue(l.otherGPS), where)
	}
	if l.other > 0 {
		l.add(core.CatTechnical, core.RiskLow, "Other EXIF tags", countValue(l.other), where)
	}
	if d.err != nil {
		// Unknown contents get the highest rating: the unreadable part may hold
		// anything, including a location.
		l.add(core.CatHidden, core.RiskHigh, "Damaged EXIF block", formatSize(uint64(d.size))+" could not be fully read", where)
	}
	if !d.minimal && !hasRemoval(l.out) {
		l.add(core.CatHidden, core.RiskLow, "EXIF block", formatSize(uint64(d.size)), where)
	}
	return l.out
}

func hasRemoval(findings []core.Finding) bool {
	for _, f := range findings {
		if f.Action != core.ActionRetain {
			return true
		}
	}
	return false
}

func (d *Data) addNamed(l *findingList, dir *directory, loc string, opts core.Options) {
	reported := 0
	for _, spec := range namedTags {
		e, ok := dir.find(spec.tag)
		if !ok {
			continue
		}
		// Cameras fill unused text tags with spaces. Those fall through to the
		// catch-all count instead of reporting a blank Artist as high risk.
		if value := spec.decode(e, d.order); value != "" {
			l.add(spec.category, spec.risk, spec.field, value, loc)
			reported++
		}
	}
	if dir.kind == kindRoot {
		if d.addOrientation(l, dir, loc, opts) {
			reported++
		}
	}
	if dir.kind == kindChained {
		reported += d.addThumbnail(l, dir, loc)
	}
	structural := 0
	for _, e := range dir.entries {
		if e.tag == tagExifIFD || e.tag == tagGPSIFD || e.tag == tagInteropIFD {
			structural++
		}
	}
	l.other += len(dir.entries) - reported - structural
}

func (d *Data) addOrientation(l *findingList, dir *directory, loc string, opts core.Options) bool {
	e, ok := dir.find(tagOrientation)
	if !ok {
		return false
	}
	finding := core.Finding{
		Category: core.CatTechnical,
		Risk:     core.RiskNone,
		Field:    "Orientation",
		Value:    orientationValue(e, d.order),
		Where:    core.SafeText(loc),
		Action:   core.ActionRemove,
	}
	if d.Replacement(opts) != nil {
		finding.Action = core.ActionRetain
		finding.Reason = "needed to display the image the right way up; strict mode removes it"
	}
	l.out = append(l.out, finding)
	return true
}

func orientationValue(e entry, order binary.ByteOrder) string {
	v, ok := e.number(order)
	if !ok {
		return "invalid"
	}
	if v >= 1 && v <= 8 {
		return strconv.FormatUint(uint64(v), 10) + " (" + orientationNames[v-1] + ")"
	}
	return "invalid value " + strconv.FormatUint(uint64(v), 10)
}

var orientationNames = [8]string{
	"normal",
	"mirrored horizontally",
	"rotated 180 degrees",
	"mirrored vertically",
	"mirrored horizontally and rotated 270 degrees clockwise",
	"rotated 90 degrees clockwise",
	"mirrored horizontally and rotated 90 degrees clockwise",
	"rotated 270 degrees clockwise",
}

// addThumbnail reports an embedded preview and returns how many entries that
// accounted for. It rates the preview high because a preview generated before
// cropping can show exactly what the crop was meant to hide. The declared size
// is reported as is; nothing here reads the thumbnail, so one that overruns a
// truncated block is not a reason to call the block damaged.
func (d *Data) addThumbnail(l *findingList, dir *directory, loc string) int {
	format, lengthTag := "JPEG", uint16(tagJPEGLength)
	if _, ok := dir.find(tagJPEGOffset); !ok {
		format, lengthTag = "uncompressed", tagStripByteCounts
		if _, ok := dir.find(tagStripOffsets); !ok {
			return 0
		}
	}
	used, value := 1, format+", size not given"
	if length, ok := dir.find(lengthTag); ok {
		used, value = 2, format+", "+formatSize(length.sum(d.order))
	}
	l.add(core.CatThumbnail, core.RiskHigh, "Embedded thumbnail", value, loc)
	return used
}

func (d *Data) addGPS(l *findingList, dir *directory, loc string) {
	reported := 0
	lat, hasLat := dir.find(gpsLatitude)
	lon, hasLon := dir.find(gpsLongitude)
	if hasLat || hasLon {
		latRef, hasLatRef := dir.find(gpsLatitudeRef)
		lonRef, hasLonRef := dir.find(gpsLongitudeRef)
		reported += countTrue(hasLat, hasLon, hasLatRef, hasLonRef)
		l.add(core.CatLocation, core.RiskHigh, "GPS coordinates", coordinatesValue(lat, latRef, lon, lonRef, d.order), loc)
	}
	if alt, ok := dir.find(gpsAltitude); ok {
		ref, hasRef := dir.find(gpsAltitudeRef)
		reported += countTrue(true, hasRef)
		l.add(core.CatLocation, core.RiskMed, "GPS altitude", altitudeValue(alt, ref, d.order), loc)
	}
	if e, ok := dir.find(gpsTimeStamp); ok {
		reported++
		l.add(core.CatTime, core.RiskMed, "GPS timestamp", gpsTimeValue(e, d.order), loc)
	}
	if e, ok := dir.find(gpsDateStamp); ok {
		value := textValue(e, d.order)
		if value == "" {
			value = undecodable
		}
		reported++
		l.add(core.CatTime, core.RiskMed, "GPS date", value, loc)
	}
	l.otherGPS += len(dir.entries) - reported
}

func countTrue(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

func countValue(n int) string {
	if n == 1 {
		return "1 tag"
	}
	return strconv.Itoa(n) + " tags"
}

func joinWhere(where, name string) string {
	if where == "" {
		return name
	}
	return where + "/" + name
}
