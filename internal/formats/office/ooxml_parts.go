package office

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	nsPackageRels  = "http://schemas.openxmlformats.org/package/2006/relationships"
	nsContentTypes = "http://schemas.openxmlformats.org/package/2006/content-types"

	emptyCoreProps = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"` +
		` xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/"` +
		` xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"/>`

	emptyAppProps = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"/>`
)

// ooxmlFormats maps a main part's lowercase content type to the format detected.
// A type missing here, such as Visio or the binary .xlsb, is refused: its parts
// are not the XML the rules below understand.
var ooxmlFormats = map[string]string{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml":   "docx",
	"application/vnd.ms-word.document.macroenabled.main+xml":                             "docm",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.template.main+xml":   "dotx",
	"application/vnd.ms-word.template.macroenabledtemplate.main+xml":                     "dotm",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml":         "xlsx",
	"application/vnd.ms-excel.sheet.macroenabled.main+xml":                               "xlsm",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template.main+xml":      "xltx",
	"application/vnd.ms-excel.template.macroenabled.main+xml":                            "xltm",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml": "pptx",
	"application/vnd.ms-powerpoint.presentation.macroenabled.main+xml":                   "pptm",
	"application/vnd.openxmlformats-officedocument.presentationml.template.main+xml":     "potx",
	"application/vnd.ms-powerpoint.template.macroenabled.main+xml":                       "potm",
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow.main+xml":    "ppsx",
	"application/vnd.ms-powerpoint.slideshow.macroenabled.main+xml":                      "ppsm",
}

// partKind is what the cleaner does with a whole part.
type partKind int

const (
	partOrdinary  partKind = iota // edited in place, or copied
	partCoreProps                 // replaced with an empty document
	partAppProps                  // replaced with an empty document

	// Everything from here on is removed, along with its relationships and its
	// content type.
	partCustomProps
	partThumbnail
	partCustomXML
	partPrinterSettings
	partTemplate
	partPeople
	partCommentIDs
	partSlideRevisions
	partWorkbookHistory
	partSignature
	partSensitivityLabel
	partDiscarded
)

func (k partKind) removed() bool { return k >= partCustomProps }

// relKinds is keyed by relTypeKey.
var relKinds = map[string]partKind{
	"coreproperties":       partCoreProps,
	"extendedproperties":   partAppProps,
	"customproperties":     partCustomProps,
	"thumbnail":            partThumbnail,
	"customxml":            partCustomXML,
	"customxmlprops":       partCustomXML,
	"printersettings":      partPrinterSettings,
	"attachedtemplate":     partTemplate,
	"people":               partPeople,
	"commentsids":          partCommentIDs,
	"commentsextensible":   partCommentIDs,
	"revisioninfo":         partSlideRevisions,
	"changesinfo":          partSlideRevisions,
	"usernames":            partWorkbookHistory,
	"revisionheaders":      partWorkbookHistory,
	"revisionlog":          partWorkbookHistory,
	"origin":               partSignature,
	"signature":            partSignature,
	"certificate":          partSignature,
	"classificationlabels": partSensitivityLabel,
}

// contentTypeKinds is keyed by lowercase content type.
var contentTypeKinds = map[string]partKind{
	"application/vnd.openxmlformats-package.core-properties+xml":                            partCoreProps,
	"application/vnd.openxmlformats-officedocument.extended-properties+xml":                 partAppProps,
	"application/vnd.openxmlformats-officedocument.custom-properties+xml":                   partCustomProps,
	"application/vnd.openxmlformats-officedocument.customxmlproperties+xml":                 partCustomXML,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.printersettings":        partPrinterSettings,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.printersettings":           partPrinterSettings,
	"application/vnd.openxmlformats-officedocument.presentationml.printersettings":          partPrinterSettings,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.people+xml":             partPeople,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.commentsids+xml":        partCommentIDs,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.commentsextensible+xml": partCommentIDs,
	"application/vnd.ms-powerpoint.revisioninfo+xml":                                        partSlideRevisions,
	"application/vnd.ms-powerpoint.changesinfo+xml":                                         partSlideRevisions,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.usernames+xml":             partWorkbookHistory,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.revisionheaders+xml":       partWorkbookHistory,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.revisionlog+xml":           partWorkbookHistory,
	"application/vnd.openxmlformats-package.digital-signature-origin":                       partSignature,
	"application/vnd.openxmlformats-package.digital-signature-xmlsignature+xml":             partSignature,
	"application/vnd.ms-office.classificationlabels+xml":                                    partSensitivityLabel,
	"application/vnd.openxmlformats-package.digital-signature-certificate":                  partSignature,
}

// embeddedRelTypes marks relationships whose target is a file in its own right.
var embeddedRelTypes = map[string]bool{
	"image": true, "audio": true, "video": true, "media": true,
	"hdphoto": true, "oleobject": true, "package": true,
}

// kindFromName applies the conventional names, for parts that nothing else
// identifies. Keys are lowercase.
func kindFromName(key string) partKind {
	in := "/" + key
	switch {
	case key == "docprops/core.xml":
		return partCoreProps
	case key == "docprops/app.xml":
		return partAppProps
	case key == "docprops/custom.xml":
		return partCustomProps
	case strings.HasPrefix(key, "docprops/thumbnail."):
		return partThumbnail
	case strings.HasPrefix(key, "customxml/"):
		return partCustomXML
	case strings.Contains(in, "/printersettings/"):
		return partPrinterSettings
	case strings.HasSuffix(in, "/people.xml"):
		return partPeople
	case strings.HasSuffix(in, "/commentsids.xml"), strings.HasSuffix(in, "/commentsextensible.xml"):
		return partCommentIDs
	case strings.HasSuffix(in, "/revisioninfo.xml"), strings.Contains(in, "/changesinfos/"):
		return partSlideRevisions
	case strings.HasPrefix(key, "xl/revisions/"):
		return partWorkbookHistory
	case strings.HasPrefix(key, "_xmlsignatures/"):
		return partSignature
	case strings.HasPrefix(key, "docmetadata/"):
		return partSensitivityLabel
	case strings.HasPrefix(key, "[trash]/"):
		return partDiscarded
	}
	return partOrdinary
}

var (
	fieldCoreOther = fieldSpec{"Core property", core.CatContent, core.RiskLow, core.ActionRemove}
	coreFields     = map[string]fieldSpec{
		"creator":        {"Author", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"lastModifiedBy": {"Last modified by", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"created":        {"Created", core.CatTime, core.RiskMed, core.ActionRemove},
		"modified":       {"Modified", core.CatTime, core.RiskMed, core.ActionRemove},
		"lastPrinted":    {"Last printed", core.CatTime, core.RiskMed, core.ActionRemove},
		"revision":       {"Revision number", core.CatHidden, core.RiskLow, core.ActionRemove},
		"title":          {"Title", core.CatContent, core.RiskLow, core.ActionRemove},
		"subject":        {"Subject", core.CatContent, core.RiskLow, core.ActionRemove},
		"keywords":       {"Keywords", core.CatContent, core.RiskLow, core.ActionRemove},
		"description":    {"Description", core.CatContent, core.RiskLow, core.ActionRemove},
		"category":       {"Category", core.CatContent, core.RiskLow, core.ActionRemove},
		"contentStatus":  {"Content status", core.CatContent, core.RiskLow, core.ActionRemove},
		"language":       {"Language", core.CatContent, core.RiskLow, core.ActionRemove},
		"version":        {"Version", core.CatContent, core.RiskLow, core.ActionRemove},
		"identifier":     {"Identifier", core.CatContent, core.RiskLow, core.ActionRemove},
	}

	fieldAppOther = fieldSpec{"Document statistics", core.CatTechnical, core.RiskLow, core.ActionRemove}
	appFields     = map[string]fieldSpec{
		"Application":   {"Application", core.CatSoftware, core.RiskLow, core.ActionRemove},
		"AppVersion":    {"Application version", core.CatSoftware, core.RiskLow, core.ActionRemove},
		"Company":       {"Company", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"Manager":       {"Manager", core.CatIdentity, core.RiskHigh, core.ActionRemove},
		"TotalTime":     {"Total editing time", core.CatTime, core.RiskLow, core.ActionRemove},
		"Template":      {"Template name", core.CatIdentity, core.RiskMed, core.ActionRemove},
		"HyperlinkBase": {"Hyperlink base", core.CatIdentity, core.RiskMed, core.ActionRemove},
		"TitlesOfParts": {"Titles of parts", core.CatContent, core.RiskLow, core.ActionRemove},
		"HeadingPairs":  {"Headings of parts", core.CatContent, core.RiskLow, core.ActionRemove},
	}

	fieldCustomProperty = fieldSpec{"Custom property", core.CatContent, core.RiskMed, core.ActionRemove}
	fieldThumbnail      = fieldSpec{"Thumbnail", core.CatThumbnail, core.RiskHigh, core.ActionRemove}
	fieldCustomXML      = fieldSpec{"Custom XML data", core.CatHidden, core.RiskHigh, core.ActionRemove}
	fieldPrinter        = fieldSpec{"Printer settings", core.CatDevice, core.RiskMed, core.ActionRemove}
	fieldTemplate       = fieldSpec{"Attached template", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldPeople         = fieldSpec{"People list", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldCommentIDs     = fieldSpec{"Comment tracking data", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldSlideRevisions = fieldSpec{"Presentation revision data", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldWorkbookLog    = fieldSpec{"Shared workbook history", core.CatHidden, core.RiskHigh, core.ActionRemove}
	fieldSignature      = fieldSpec{"Digital signature", core.CatIdentity, core.RiskHigh, core.ActionRemove}
	fieldLabel          = fieldSpec{"Sensitivity label", core.CatIdentity, core.RiskMed, core.ActionRemove}
	fieldDiscarded      = fieldSpec{"Discarded package data", core.CatHidden, core.RiskMed, core.ActionRemove}

	fieldXMLComment     = fieldSpec{"XML comment", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldXMLInstruction = fieldSpec{"XML instruction", core.CatHidden, core.RiskLow, core.ActionRemove}
	fieldXMLForeign     = fieldSpec{"Markup the format does not define", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldDanglingRel    = fieldSpec{"Relationship to a missing part", core.CatHidden, core.RiskLow, core.ActionRemove}
)

// describeLimit is how much text is gathered for one report value. core.SafeText
// truncates further for display.
const describeLimit = 1024

// reportProperties lists the direct children of a properties document. The whole
// part is replaced regardless, so a part that cannot be parsed loses nothing by it
// and is reported as one finding. Unknown fields are reported too, because the
// replacement removes them as well.
func reportProperties(ctx context.Context, data []byte, table map[string]fieldSpec, other fieldSpec, t *tally) error {
	_, err := scanXML(ctx, data, func(e *xmlElement) error {
		if e.depth() != 1 {
			return nil
		}
		value := e.subtreeText(describeLimit)
		e.drop()
		if value == "" {
			return nil
		}
		if spec, ok := table[e.local]; ok {
			t.value(spec, value)
			return nil
		}
		t.value(other, e.local+": "+value)
		return nil
	})
	if err != nil && !errors.Is(err, core.ErrMalformed) && !errors.Is(err, core.ErrUnsupported) {
		return err
	}
	if err != nil {
		t.value(other, "unreadable, "+byteCount(int64(len(data))))
	}
	return nil
}

// reportRemoved tells the user what a part held. The part is going away whatever
// happens here, so damage inside it only costs detail in the report.
func (p *ooxmlPackage) reportRemoved(ctx context.Context, kind partKind, data []byte, t *tally, rep *core.Report, name string) error {
	size := byteCount(int64(len(data)))
	switch kind {
	case partCustomProps:
		return reportCustomProperties(ctx, data, t)
	case partThumbnail:
		t.value(fieldThumbnail, size)
	case partCustomXML:
		reportCustomXML(data, name, t, rep)
	case partPrinterSettings:
		t.value(fieldPrinter, describeDevmode(data))
	case partTemplate:
		t.value(fieldTemplate, size)
	case partPeople:
		return reportPeople(ctx, data, t)
	case partCommentIDs:
		t.value(fieldCommentIDs, size)
	case partSlideRevisions:
		t.value(fieldSlideRevisions, size)
	case partWorkbookHistory:
		t.value(fieldWorkbookLog, size)
	case partSignature:
		t.value(fieldSignature, size)
		rep.Warn("%s: a digital signature was removed; cleaning changes the document, so it could not stay valid", name)
	case partSensitivityLabel:
		// The label's site ID names the organisation's Microsoft 365 tenant.
		t.value(fieldLabel, size)
	case partDiscarded:
		t.value(fieldDiscarded, size)
	}
	return nil
}

func reportCustomProperties(ctx context.Context, data []byte, t *tally) error {
	_, err := scanXML(ctx, data, func(e *xmlElement) error {
		if e.depth() != 1 {
			return nil
		}
		name, _ := e.attr("", "name")
		t.value(fieldCustomProperty, name+" = "+e.subtreeText(describeLimit))
		e.drop()
		return nil
	})
	if err != nil && !errors.Is(err, core.ErrMalformed) && !errors.Is(err, core.ErrUnsupported) {
		return err
	}
	if err != nil {
		t.value(fieldCustomProperty, "unreadable, "+byteCount(int64(len(data))))
	}
	return nil
}

// reportCustomXML favours the XMP reader when the data is a packet, since it gives
// decoded values, and otherwise shows the start of the text, which is what makes
// SharePoint data or an email address recognisable.
func reportCustomXML(data []byte, name string, t *tally, rep *core.Report) {
	if xmp.Contains(data) {
		for _, f := range xmp.Findings(data, name) {
			rep.Add(f)
		}
		return
	}
	value := plainText(data)
	if value == "" {
		value = byteCount(int64(len(data)))
	}
	t.value(fieldCustomXML, value)
}

// plainText strips the tags from a small document for display.
func plainText(data []byte) string {
	normal, err := normalizeXML(data)
	if err != nil {
		return ""
	}
	var b strings.Builder
	sc := xmlScanner{data: normal}
	for b.Len() < describeLimit {
		more, err := sc.next()
		if err != nil || !more {
			break
		}
		if sc.tok.kind != tokenText {
			continue
		}
		if body := sc.data[sc.tok.start:sc.tok.end]; !isBlank(body) {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strings.TrimSpace(decodeEntities(body)))
		}
	}
	return b.String()
}

func reportPeople(ctx context.Context, data []byte, t *tally) error {
	found := false
	_, err := scanXML(ctx, data, func(e *xmlElement) error {
		if e.local == "person" {
			if author, ok := e.attr(nsWord15, "author"); ok {
				t.value(fieldPeople, author)
				found = true
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, core.ErrMalformed) && !errors.Is(err, core.ErrUnsupported) {
		return err
	}
	if !found {
		t.value(fieldPeople, byteCount(int64(len(data))))
	}
	return nil
}

// describeDevmode pulls the printer name from a Windows DEVMODE structure, which
// starts with a fixed 32-character UTF-16 device name.
func describeDevmode(data []byte) string {
	const nameBytes = 64
	size := byteCount(int64(len(data)))
	if len(data) < nameBytes {
		return size
	}
	var units []uint16
	for i := 0; i < nameBytes; i += 2 {
		unit := binary.LittleEndian.Uint16(data[i:])
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	name := strings.TrimSpace(string(utf16.Decode(units)))
	if name == "" || strings.ContainsRune(name, utf8.RuneError) {
		return size
	}
	return fmt.Sprintf("%s, %s", name, size)
}
