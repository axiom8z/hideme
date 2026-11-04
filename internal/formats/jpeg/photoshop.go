package jpeg

import (
	"strconv"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/jpegmeta"
	"github.com/axiom8z/hideme/internal/xmp"
)

// maxIPTCFindings bounds the report for a block that repeats datasets. A dataset
// costs as little as five bytes, so a hostile file could otherwise produce a
// finding per five bytes of input.
const maxIPTCFindings = 256

// reportPhotoshop describes the joined APP13 payloads. All of it is removed, so a
// failure to understand it can only make the report less specific.
func (c *cleaner) reportPhotoshop() {
	if !c.sawPhotoshop {
		return
	}
	const where = "APP13/Photoshop"
	start := len(c.rep.Findings)
	other := 0
	for b := c.photoshop; len(b) > 0; {
		id, value, rest, ok := jpegmeta.NextResource(b)
		if !ok {
			// Padding, which writers leave at the end of these blocks, hides nothing. This
			// is checked only once a parse has failed, because doing it on every pass would
			// rescan a long run of zeros each time and let a hostile file make the loop
			// quadratic.
			if !core.AllZero(b) {
				c.add(core.CatHidden, core.RiskHigh, "Damaged Photoshop data", core.ByteCount(len(b))+" could not be read", where)
			}
			break
		}
		switch id {
		case jpegmeta.ResourceIPTC:
			c.reportIPTC(value, where+"/IPTC")
		case jpegmeta.ResourceThumbnail4, jpegmeta.ResourceThumbnail5:
			c.add(core.CatThumbnail, core.RiskHigh, "Embedded thumbnail", core.ByteCount(len(value)), where)
		case jpegmeta.ResourceExif1, jpegmeta.ResourceExif3:
			d, _ := exif.Parse(exif.StripPrefix(value))
			c.addAll(d.Findings(where+"/Exif", core.Options{Strict: true}))
		case jpegmeta.ResourceXMP:
			c.addAll(xmp.Findings(value, where+"/XMP"))
		default:
			other++
		}
		b = rest
	}
	if other > 0 {
		c.add(core.CatHidden, core.RiskLow, "Other Photoshop resources", countOf(other, "resource"), where)
	}
	if len(c.rep.Findings) == start {
		c.add(core.CatHidden, core.RiskLow, "Photoshop data", core.ByteCount(len(c.photoshop)), where)
	}
}

type iptcField struct {
	dataset  byte
	field    string
	category core.Category
	risk     core.Risk
}

// iptcFields lists the application-record datasets reported by name.
var iptcFields = []iptcField{
	{5, "Object name", core.CatContent, core.RiskLow},
	{25, "Keywords", core.CatContent, core.RiskLow},
	{40, "Special instructions", core.CatContent, core.RiskLow},
	{55, "Date created", core.CatTime, core.RiskMed},
	{60, "Time created", core.CatTime, core.RiskMed},
	{62, "Digital creation date", core.CatTime, core.RiskMed},
	{63, "Digital creation time", core.CatTime, core.RiskMed},
	{65, "Originating program", core.CatSoftware, core.RiskLow},
	{80, "By-line", core.CatIdentity, core.RiskHigh},
	{85, "By-line title", core.CatIdentity, core.RiskMed},
	{90, "City", core.CatLocation, core.RiskHigh},
	{92, "Sublocation", core.CatLocation, core.RiskHigh},
	{95, "Province/State", core.CatLocation, core.RiskHigh},
	{100, "Country code", core.CatLocation, core.RiskMed},
	{101, "Country", core.CatLocation, core.RiskMed},
	{103, "Transmission reference", core.CatHidden, core.RiskLow},
	{105, "Headline", core.CatContent, core.RiskLow},
	{110, "Credit", core.CatIdentity, core.RiskHigh},
	{115, "Source", core.CatIdentity, core.RiskMed},
	{116, "Copyright notice", core.CatIdentity, core.RiskHigh},
	{118, "Contact", core.CatIdentity, core.RiskHigh},
	{120, "Caption", core.CatContent, core.RiskMed},
	{122, "Writer/Editor", core.CatIdentity, core.RiskHigh},
}

func (c *cleaner) reportIPTC(b []byte, where string) {
	named, other := 0, 0
	for len(b) > 0 {
		record, dataset, value, rest, ok := jpegmeta.NextDataset(b)
		if !ok {
			if !core.AllZero(b) {
				c.add(core.CatHidden, core.RiskHigh, "Damaged IPTC data", core.ByteCount(len(b))+" could not be read", where)
			}
			break
		}
		b = rest
		if spec, found := lookupIPTC(dataset); found && record == jpegmeta.IPTCApplicationRecord && named < maxIPTCFindings {
			named++
			c.add(spec.category, spec.risk, spec.field, core.DecodeText(value), where)
			continue
		}
		other++
	}
	if other > 0 {
		c.add(core.CatContent, core.RiskLow, "Other IPTC datasets", countOf(other, "dataset"), where)
	}
}

func lookupIPTC(dataset byte) (iptcField, bool) {
	for _, spec := range iptcFields {
		if spec.dataset == dataset {
			return spec, true
		}
	}
	return iptcField{}, false
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
