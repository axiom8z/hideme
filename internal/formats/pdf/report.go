package pdf

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

func removal(cat core.Category, risk core.Risk, field, value, where string) core.Finding {
	return core.Finding{Category: cat, Risk: risk, Field: field, Value: value, Where: where, Action: core.ActionRemove}
}

// add records a finding unless the same one is already in the report. A document
// with 300 comments by one person has one author to report.
func (c *cleaner) add(f core.Finding) {
	if c.reported[f] {
		return
	}
	c.reported[f] = true
	c.rep.Add(f)
}

func (c *cleaner) warn(message string) {
	if c.warned[message] {
		return
	}
	c.warned[message] = true
	c.rep.Warn("%s", message)
}

// text renders a value for a report. Only values that read as text give a result,
// so that a dictionary does not print as an internal address and a loop of arrays
// cannot recurse.
func (d *document) text(v object) string {
	switch t := d.resolve(v).(type) {
	case pdfString:
		return decodeText(t.data)
	case pdfName:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case pdfReal:
		return string(t)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// infoField says how to report one standard key of the Info dictionary.
type infoField struct {
	field    string
	category core.Category
	risk     core.Risk
	date     bool
}

var infoFields = map[string]infoField{
	"Author":       {"Author", core.CatIdentity, core.RiskHigh, false},
	"Creator":      {"Creator", core.CatSoftware, core.RiskLow, false},
	"Producer":     {"Producer", core.CatSoftware, core.RiskLow, false},
	"Title":        {"Title", core.CatContent, core.RiskLow, false},
	"Subject":      {"Subject", core.CatContent, core.RiskLow, false},
	"Keywords":     {"Keywords", core.CatContent, core.RiskLow, false},
	"CreationDate": {"Creation date", core.CatTime, core.RiskMed, true},
	"ModDate":      {"Modification date", core.CatTime, core.RiskMed, true},
	"Trapped":      {"Trapped", core.CatTechnical, core.RiskNone, false},
}

// structuralTrailerKeys are the trailer keys that say nothing about the document's
// origin, including those of a cross-reference stream's dictionary, which serves
// as its trailer. Anything else in a trailer was put there by the producer, such
// as pdfTeX's /PTEX.Fullbanner.
var structuralTrailerKeys = keySet("Size", "Root", "Prev", "Info", "ID", "XRefStm",
	"Type", "W", "Index", "Length", "Filter", "DecodeParms", "DL")

// reportDocumentLevel reports what lives outside the object graph being rewritten.
func (c *cleaner) reportDocumentLevel() {
	c.reportInfoDict(c.doc.info, "Info")
	c.reportID()
	for _, t := range c.doc.trailers {
		for _, e := range t.entries {
			if !structuralTrailerKeys[e.key] {
				c.add(removal(core.CatHidden, core.RiskLow, "Private trailer entry", c.keyAndText(e), "Trailer"))
			}
		}
	}
	if c.doc.recovered {
		c.rep.Warn("the cross-reference table is damaged, so objects were recovered by scanning the file; check that the result is complete")
		if n := c.doc.superseded; n > 0 {
			c.add(removal(core.CatHidden, core.RiskHigh, "Earlier revisions", plural(n, "superseded object definition"), "File"))
		}
	} else if n := c.doc.sections - 1; n > 0 {
		c.add(removal(core.CatHidden, core.RiskHigh, "Earlier revisions", plural(n, "older revision"), "File"))
	}
	if n := c.doc.orphans; n > 0 {
		c.add(removal(core.CatHidden, core.RiskHigh, "Orphan objects", plural(n, "object")+" nothing refers to", "File"))
	}
	if n := c.doc.trailingBytes(); n > 0 {
		c.add(removal(core.CatHidden, core.RiskHigh, "Data after end of file", plural(n, "byte"), "File"))
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// reportInfoDict reports a dictionary in the format of the document Info
// dictionary, which is what the trailer and article threads have.
func (c *cleaner) reportInfoDict(v object, where string) {
	info, ok := c.doc.resolve(v).(*pdfDict)
	if !ok {
		return
	}
	for _, e := range info.entries {
		value := c.doc.text(e.val)
		spec, known := infoFields[e.key]
		switch {
		case value == "":
			continue
		case !known:
			c.add(removal(core.CatHidden, core.RiskMed, "Custom metadata", e.key+": "+value, where))
			continue
		case spec.date:
			value = formatDate(value)
		}
		c.add(removal(spec.category, spec.risk, spec.field, value, where))
	}
}

func (c *cleaner) reportID() {
	id := c.doc.resolve(c.doc.id)
	if id == nil {
		return
	}
	var parts []string
	if a, ok := id.(pdfArray); ok {
		for _, e := range a {
			if s, ok := c.doc.resolve(e).(pdfString); ok {
				parts = append(parts, hex.EncodeToString(s.data))
			}
		}
	}
	c.add(removal(core.CatHidden, core.RiskMed, "Document ID", strings.Join(slices.Compact(parts), ", "), "Trailer"))
}

// reportMetadata reports a metadata stream, or what a key claiming to be metadata
// points at. Only the limits and cancellation are errors. A packet that cannot be
// decoded is removed all the same, so it is reported from its raw bytes.
func (c *cleaner) reportMetadata(v object, where string) error {
	switch t := c.doc.resolve(v).(type) {
	case nil:
	case *pdfStream:
		packet, err := c.doc.decodeStream(t)
		if errors.Is(err, core.ErrTooLarge) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil {
			packet = t.raw
		}
		for _, f := range xmp.Findings(packet, where) {
			c.add(f)
		}
	default:
		c.add(removal(core.CatHidden, core.RiskMed, "Metadata entry", "", where))
	}
	return nil
}

func (c *cleaner) reportSignature(d *pdfDict, where string) {
	fields := []struct {
		key, field string
		category   core.Category
		date       bool
	}{
		{"Name", "Signer name", core.CatIdentity, false},
		{"Reason", "Signature reason", core.CatContent, false},
		{"Location", "Signature location", core.CatLocation, false},
		{"ContactInfo", "Signer contact", core.CatIdentity, false},
		{"M", "Signature date", core.CatTime, true},
	}
	for _, f := range fields {
		value := c.doc.text(d.get(f.key))
		if value == "" {
			continue
		}
		if f.date {
			value = formatDate(value)
		}
		c.add(removal(f.category, core.RiskHigh, f.field, value, where))
	}
	size := ""
	if s, ok := c.doc.resolve(d.get("Contents")).(pdfString); ok {
		size = plural(len(s.data), "byte")
	}
	c.add(removal(core.CatIdentity, core.RiskHigh, "Digital signature", size, where))
	c.warn("a digital signature was removed, because rewriting the file would have invalidated it; the result no longer proves who signed it, and any visible signature on the page remains")
}

func (c *cleaner) reportOffListKey(e dictEntry, r role, where string) {
	switch {
	case r&roleCatalog != 0 && e.key == "Collection":
		c.add(removal(core.CatHidden, core.RiskMed, "Attachment collection", "", where))
	case r&roleCatalog != 0 && e.key == "Perms":
		if perms, ok := c.doc.resolve(e.val).(*pdfDict); ok {
			for _, p := range perms.entries {
				if sig, ok := c.doc.resolve(p.val).(*pdfDict); ok && c.doc.isSignature(sig) {
					c.reportSignature(sig, where+"/Perms")
				}
			}
		}
		c.add(removal(core.CatIdentity, core.RiskMed, "Permission signatures", "", where))
	case r&roleCatalog != 0 && e.key == "DSS":
		c.add(removal(core.CatIdentity, core.RiskHigh, "Signature validation data", "", where))
	case r&roleCatalog != 0:
		c.add(removal(core.CatHidden, core.RiskMed, "Unknown catalog entry", c.keyAndText(e), where))
	case r&rolePage != 0:
		c.add(removal(core.CatHidden, core.RiskMed, "Unknown page entry", c.keyAndText(e), where))
	case r&(roleFont|roleFontDescriptor) != 0:
		c.add(removal(core.CatHidden, core.RiskMed, "Unknown font entry", c.keyAndText(e), where))
	case r&roleAction != 0:
		c.add(removal(core.CatHidden, core.RiskMed, "Unknown action entry", c.keyAndText(e), where))
	case r&roleResources != 0:
		c.add(removal(core.CatHidden, core.RiskMed, "Unknown resource entry", c.keyAndText(e), where))
	default:
		c.add(removal(core.CatHidden, core.RiskMed, "Unknown image or form entry", c.keyAndText(e), where))
	}
}

// keyAndText names a removed entry, with its value when that reads as text.
func (c *cleaner) keyAndText(e dictEntry) string {
	if text := c.doc.text(e.val); text != "" {
		return e.key + ": " + text
	}
	return e.key
}

func (c *cleaner) creatorInfoName(v object) string {
	if info, ok := c.doc.resolve(v).(*pdfDict); ok {
		return c.doc.text(info.get("Creator"))
	}
	return ""
}

func (c *cleaner) layerUserName(v object) string {
	if user, ok := c.doc.resolve(v).(*pdfDict); ok {
		return c.doc.text(user.get("Name"))
	}
	return ""
}

func (c *cleaner) reportAttachment(d *pdfDict, where string) {
	name := fileName(c.doc, d)
	if name == "" {
		name = "unnamed"
	}
	c.add(removal(core.CatHidden, core.RiskHigh, "Attachment", name, where))
}

// fileName finds the name in a file specification or in a file attachment
// annotation's /FS, preferring the Unicode form.
func fileName(d *document, x *pdfDict) string {
	if fs, ok := d.resolve(x.get("FS")).(*pdfDict); ok {
		x = fs
	}
	for _, key := range []string{"UF", "F", "Unix", "DOS", "Mac"} {
		if s, ok := d.resolve(x.get(key)).(pdfString); ok && len(s.data) > 0 {
			return decodeText(s.data)
		}
	}
	return ""
}

// reportNameTree lists the attachments in a name tree. The walk is iterative with
// visited objects tracked, because a hostile tree may loop or run deep.
func (c *cleaner) reportNameTree(v object) {
	const where = "Names/EmbeddedFiles"
	seen := make(map[int]bool)
	stack := []object{v}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if r, ok := top.(pdfRef); ok {
			if seen[r.num] {
				continue
			}
			seen[r.num] = true
		}
		node, ok := c.doc.resolve(top).(*pdfDict)
		if !ok {
			continue
		}
		if kids, ok := c.doc.resolve(node.get("Kids")).(pdfArray); ok {
			stack = append(stack, kids...)
		}
		names, ok := c.doc.resolve(node.get("Names")).(pdfArray)
		if !ok {
			continue
		}
		for i := 0; i+1 < len(names); i += 2 {
			label := c.doc.text(names[i])
			if spec, ok := c.doc.resolve(names[i+1]).(*pdfDict); ok {
				if name := fileName(c.doc, spec); name != "" {
					label = name
				}
			}
			if label == "" {
				label = "unnamed"
			}
			c.add(removal(core.CatHidden, core.RiskHigh, "Attachment", label, where))
		}
	}
}

func (c *cleaner) reportAssociatedFiles(v object, where string) {
	where += "/AF"
	switch t := c.doc.resolve(v).(type) {
	case *pdfDict:
		c.reportAttachment(t, where)
	case pdfArray:
		for _, e := range t {
			if spec, ok := c.doc.resolve(e).(*pdfDict); ok {
				c.reportAttachment(spec, where)
			}
		}
	}
}

func (c *cleaner) pieceInfoNames(v object) string {
	info, ok := c.doc.resolve(v).(*pdfDict)
	if !ok {
		return ""
	}
	names := make([]string, 0, len(info.entries))
	for _, e := range info.entries {
		names = append(names, e.key)
	}
	return strings.Join(names, ", ")
}

func (c *cleaner) streamSize(v object) string {
	if s, ok := c.doc.resolve(v).(*pdfStream); ok {
		return plural(len(s.raw), "byte")
	}
	return ""
}
