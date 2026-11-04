package matroska

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxFindings keeps a hostile file with millions of tags from growing the
	// report without limit. What is past the limit is still removed, and counted.
	maxFindings = 512
	// maxValueBytes is more than core.SafeText shows of any value.
	maxValueBytes = 1024
	// maxHexBytes shows the whole of a 16-byte segment UID.
	maxHexBytes = 32
	// maxDescribeDepth limits the recursion through nested tags and chapters,
	// which are removed whole whatever they hold.
	maxDescribeDepth = 16
)

// matroskaEpoch is the zero point of EBML dates.
var matroskaEpoch = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

// add records a finding, or counts it once the report has reached its limit.
func (p *plan) add(f field, value, where string) {
	if p.findings >= maxFindings {
		p.further++
		return
	}
	p.findings++
	p.rep.Add(core.Finding{Category: f.category, Risk: f.risk, Field: f.name, Value: value, Where: where, Action: core.ActionRemove})
}

func (p *plan) finishFindings() {
	if p.further == 0 {
		return
	}
	p.rep.Add(core.Finding{
		Category: core.CatHidden, Risk: core.RiskMed, Field: "Further metadata",
		Value: strconv.Itoa(p.further) + " more items", Where: segmentWhere, Action: core.ActionRemove,
	})
}

func (p *plan) describe(e element, d decoding) string {
	switch d {
	case decodeText:
		return p.text(e)
	case decodeHex:
		return hex.EncodeToString(p.r.peek(e, maxHexBytes))
	case decodeDate:
		if e.size == 8 {
			if raw := p.r.peek(e, 8); raw != nil {
				nanos := int64(binary.BigEndian.Uint64(raw))
				return matroskaEpoch.Add(time.Duration(nanos)).Format(time.RFC3339)
			}
		}
	}
	return core.ByteCount(e.size)
}

// text reads a string element for display. EBML strings may be padded with NULs.
func (p *plan) text(e element) string {
	return string(bytes.TrimRight(p.r.peek(e, maxValueBytes), "\x00"))
}

// removeTags reports each tag and voids the whole Tags element. Damage inside it
// only stops the description, because the element's bounds are already known and
// everything within them goes.
func (p *plan) removeTags(e element, where string) error {
	before := p.findings + p.further
	for tag, err := range p.r.children(e) {
		if err != nil {
			break
		}
		if tag.id != idTag {
			continue
		}
		for child, err := range p.r.children(tag) {
			if err != nil {
				break
			}
			if child.id == idSimpleTag {
				p.describeSimpleTag(child, where+"/Tags", 0)
			}
		}
	}
	if p.findings+p.further == before {
		p.add(field{"Tags", core.CatContent, core.RiskLow, decodeSize}, core.ByteCount(e.size), where)
	}
	return p.void(e)
}

func (p *plan) describeSimpleTag(e element, where string, depth int) {
	if depth >= maxDescribeDepth {
		return
	}
	var name, value string
	var nested []element
	for child, err := range p.r.children(e) {
		if err != nil {
			break
		}
		switch child.id {
		case idTagName:
			name = p.text(child)
		case idTagString:
			value = p.text(child)
		case idTagBinary:
			value = core.ByteCount(child.size)
		case idSimpleTag:
			// A hostile tag can hold millions of children. Only the first few are
			// described, and everything goes regardless.
			if len(nested) < maxFindings {
				nested = append(nested, child)
			}
		}
	}
	p.addTag(name, value, where)
	for _, child := range nested {
		p.describeSimpleTag(child, where, depth+1)
	}
}

// addTag reports one tag. Field names are printed even by clean, which promises to
// print nothing from the file, and tag names are free text. So only a name in this
// program's own table gets a field of its own. Any other goes in the value, which clean
// hides, under a fixed field name.
func (p *plan) addTag(name, value, where string) {
	category, risk := classifyTag(name)
	if upper := strings.ToUpper(name); knownTags[upper] {
		p.add(field{name: "Tag " + upper, category: category, risk: risk}, value, where)
		return
	}
	p.add(field{name: "Custom tag", category: category, risk: risk}, name+"="+value, where)
}

// knownTags lists the tag names of RFC 9559, those of the Vorbis comment convention, and
// what muxers commonly carry over from other containers.
var knownTags = func() map[string]bool {
	set := make(map[string]bool)
	for _, name := range strings.Fields(`
		ORIGINAL SAMPLE COUNTRY TOTAL_PARTS PART_NUMBER PART_OFFSET TITLE SUBTITLE URL SORT_WITH INSTRUMENTS EMAIL ADDRESS
		FAX PHONE ARTIST LEAD_PERFORMER ACCOMPANIMENT COMPOSER ARRANGER LYRICS LYRICIST CONDUCTOR DIRECTOR ASSISTANT_DIRECTOR
		DIRECTOR_OF_PHOTOGRAPHY SOUND_ENGINEER ART_DIRECTOR PRODUCTION_DESIGNER CHOREGRAPHER COSTUME_DESIGNER ACTOR CHARACTER
		WRITTEN_BY SCREENPLAY_BY EDITED_BY PRODUCER COPRODUCER EXECUTIVE_PRODUCER DISTRIBUTED_BY MASTERED_BY ENCODED_BY MIXED_BY
		REMIXED_BY PRODUCTION_STUDIO THANKS_TO PUBLISHER LABEL GENRE MOOD ORIGINAL_MEDIA_TYPE CONTENT_TYPE SUBJECT DESCRIPTION
		KEYWORDS SUMMARY SYNOPSIS INITIAL_KEY PERIOD LAW_RATING DATE_RELEASED DATE_RECORDED DATE_ENCODED DATE_TAGGED
		DATE_DIGITIZED DATE_WRITTEN DATE_PURCHASED RECORDING_LOCATION COMPOSITION_LOCATION COMPOSER_NATIONALITY COMMENT
		PLAY_COUNTER RATING ENCODER ENCODER_SETTINGS BPS FPS BPM MEASURE TUNING REPLAYGAIN_GAIN REPLAYGAIN_PEAK ISRC MCDI ISBN
		BARCODE CATALOG_NUMBER LABEL_CODE LCCN IMDB TMDB TVDB PURCHASE_ITEM PURCHASE_INFO PURCHASE_OWNER PURCHASE_PRICE
		PURCHASE_CURRENCY COPYRIGHT PRODUCTION_COPYRIGHT LICENSE TERMS_OF_USE
		ALBUM ALBUMARTIST TRACKNUMBER DISCNUMBER DATE VERSION PERFORMER ORGANIZATION CONTACT LOCATION VENDOR
		DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES CREATION_TIME HANDLER_NAME VENDOR_ID MAJOR_BRAND MINOR_VERSION
		COMPATIBLE_BRANDS MAKE MODEL SOFTWARE
		COM.APPLE.QUICKTIME.LOCATION.ISO6709 COM.APPLE.QUICKTIME.MAKE COM.APPLE.QUICKTIME.MODEL COM.APPLE.QUICKTIME.SOFTWARE
		COM.APPLE.QUICKTIME.CREATIONDATE _STATISTICS_TAGS _STATISTICS_WRITING_APP _STATISTICS_WRITING_DATE_UTC`) {
		set[name] = true
	}
	return set
}()

// tagClasses rates a tag by words in its name. The names are free text: RFC 9559
// lists common ones, and muxers invent others, such as creation_time or
// com.apple.quicktime.location.ISO6709 carried over from a phone's MOV. The table
// is searched in order, so the location entries win over the rest.
var tagClasses = []struct {
	words    []string
	category core.Category
	risk     core.Risk
}{
	{[]string{"LOCATION", "GPS", "ISO6709", "LATITUDE", "LONGITUDE"}, core.CatLocation, core.RiskHigh},
	{[]string{"ARTIST", "AUTHOR", "COMPOSER", "CREATOR", "DIRECTOR", "PRODUCER", "PUBLISHER", "COPYRIGHT",
		"ENCODED_BY", "WRITTEN_BY", "OWNER", "PERFORMER", "ACTOR", "EMAIL", "CONTACT", "PHONE"}, core.CatIdentity, core.RiskHigh},
	{[]string{"SERIAL", "MAKE", "MODEL", "DEVICE", "CAMERA", "LENS"}, core.CatDevice, core.RiskMed},
	{[]string{"DATE", "TIME"}, core.CatTime, core.RiskMed},
	{[]string{"ENCODER", "SOFTWARE", "WRITING_APP", "MUXING_APP", "HANDLER", "VENDOR"}, core.CatSoftware, core.RiskLow},
	{[]string{"COMMENT", "DESCRIPTION", "SUMMARY", "SYNOPSIS"}, core.CatContent, core.RiskMed},
}

func classifyTag(name string) (core.Category, core.Risk) {
	upper := strings.ToUpper(name)
	for _, class := range tagClasses {
		for _, word := range class.words {
			if strings.Contains(upper, word) {
				return class.category, class.risk
			}
		}
	}
	return core.CatContent, core.RiskLow
}

// removeAttachments reports each attached file and voids the whole element.
func (p *plan) removeAttachments(e element, where string) error {
	before := p.findings + p.further
	for file, err := range p.r.children(e) {
		if err != nil {
			break
		}
		if file.id == idAttachedFile {
			p.describeAttachment(file, where+"/Attachments")
		}
	}
	if p.findings+p.further == before {
		p.add(field{"Attachments", core.CatHidden, core.RiskHigh, decodeSize}, core.ByteCount(e.size), where)
	}
	return p.void(e)
}

func (p *plan) describeAttachment(e element, where string) {
	var name, mediaType, description string
	var size int64
	for child, err := range p.r.children(e) {
		if err != nil {
			break
		}
		switch child.id {
		case idFileName:
			name = p.text(child)
		case idFileMediaType:
			mediaType = p.text(child)
		case idFileDescription:
			description = p.text(child)
		case idFileData:
			size = child.size
		}
	}
	p.add(field{"Attachment", core.CatHidden, core.RiskHigh, decodeSize},
		fmt.Sprintf("%s (%s, %s)", name, mediaType, core.ByteCount(size)), where)
	if description != "" {
		p.add(field{"Attachment description", core.CatContent, core.RiskMed, decodeSize}, description, where)
	}
}

// removeChapters reports chapter titles and voids the whole element.
func (p *plan) removeChapters(e element, where string) error {
	p.add(field{"Chapters", core.CatContent, core.RiskLow, decodeSize}, core.ByteCount(e.size), where)
	p.describeChapters(e, where+"/Chapters", 0)
	return p.void(e)
}

func (p *plan) describeChapters(e element, where string, depth int) {
	if depth >= maxDescribeDepth {
		return
	}
	for child, err := range p.r.children(e) {
		if err != nil {
			return
		}
		switch child.id {
		case idEditionEntry, idChapterAtom, idChapterDisplay:
			p.describeChapters(child, where, depth+1)
		case idChapString:
			p.add(field{"Chapter title", core.CatContent, core.RiskLow, decodeSize}, p.text(child), where)
		}
	}
}
