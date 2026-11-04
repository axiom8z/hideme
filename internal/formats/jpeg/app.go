package jpeg

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/jpegmeta"
	"github.com/axiom8z/hideme/internal/xmp"
)

// Signatures that say what an APPn segment holds.
const (
	sigJFIF        = "JFIF\x00"
	sigJFXX        = "JFXX\x00"
	sigExif        = "Exif\x00"
	sigXMP         = "http://ns.adobe.com/xap/1.0/\x00"
	sigExtendedXMP = "http://ns.adobe.com/xmp/extension/\x00"
	sigICC         = "ICC_PROFILE\x00"
	sigMPF         = "MPF\x00"
	sigFlashPix    = "FPXR\x00"
	sigPhotoshop   = "Photoshop 3.0\x00"
	sigAdobe       = "Adobe"
)

const (
	// jfifSize is a JFIF payload with no thumbnail: signature, version (2),
	// units (1), densities (4) and the thumbnail dimensions (2).
	jfifSize = 14
	// adobeSize is the Adobe payload: signature, version (2), flags (4) and the
	// colour transform (1). libjpeg ignores a shorter one and reads no further
	// than this in a longer one.
	adobeSize = 12
	// adobeVersion is the version all Adobe segments are rewritten to, with both
	// flag fields zero. No decoder reads any of the three.
	adobeVersion = 100
	// extendedXMPHeader is the GUID (32), total length (4) and offset (4) that
	// come after the signature in each extended XMP segment.
	extendedXMPHeader = 40
	// iccHeader is the sequence number and chunk count after the signature.
	iccHeader = 2
)

// retainICC is why an ICC profile survives by default.
const retainICC = "needed to display colours correctly; strict mode removes it"

func (c *cleaner) app(n int, raw, p []byte) error {
	switch n {
	case 0:
		return c.app0(p)
	case 1:
		c.app1(p)
	case 2:
		c.app2(raw, p)
	case 13:
		c.app13(p)
	case 14:
		return c.app14(p)
	default:
		c.otherApp(n, p)
	}
	return nil
}

// app0 rewrites the JFIF header without its thumbnail. The header itself has to
// stay because decoders use its presence to choose the colour transform, which
// is also why a truncated one fails the file instead of just being dropped.
func (c *cleaner) app0(p []byte) error {
	switch {
	case bytes.HasPrefix(p, []byte(sigJFIF)):
		if len(p) < jfifSize {
			return core.Malformedf("jpeg: JFIF header of %d bytes is too short", len(p))
		}
		const where = "APP0/JFIF"
		thumbnail := 3 * int(p[12]) * int(p[13])
		if thumbnail > 0 {
			c.add(core.CatThumbnail, core.RiskHigh, "Embedded thumbnail", fmt.Sprintf("%dx%d pixels", p[12], p[13]), where)
		}
		if extra := len(p) - jfifSize - thumbnail; extra > 0 {
			c.add(core.CatHidden, core.RiskMed, "Extra JFIF data", core.ByteCount(extra), where)
		}
		if c.jfif != nil || c.sawScan {
			c.add(core.CatHidden, core.RiskLow, "Stray JFIF header", core.ByteCount(len(p)), where)
			return nil
		}
		c.jfif = binary.BigEndian.AppendUint16([]byte{0xFF, jpegmeta.MarkerAPP0}, jfifSize+2)
		c.jfif = append(c.jfif, p[:jfifSize-2]...)
		c.jfif = append(c.jfif, 0, 0)
	case bytes.HasPrefix(p, []byte(sigJFXX)):
		c.add(core.CatThumbnail, core.RiskHigh, "Embedded thumbnail", core.ByteCount(len(p)), "APP0/JFXX")
	default:
		c.otherApp(0, p)
	}
	return nil
}

func (c *cleaner) app1(p []byte) {
	switch {
	case bytes.HasPrefix(p, []byte(sigExif)):
		// Stripping once more covers writers that add the prefix to a block that
		// already has it. Pillow reads those files, so their GPS is just as real.
		c.exifBlock(exif.StripPrefix(p[min(len(p), len(exif.Prefix)):]))
	case bytes.HasPrefix(p, []byte(sigXMP)):
		c.addAll(xmp.Findings(p[len(sigXMP):], "APP1/XMP"))
	case bytes.HasPrefix(p, []byte(sigExtendedXMP)):
		c.sawExtendedXMP = true
		chunk := p[len(sigExtendedXMP):]
		c.extendedXMP = append(c.extendedXMP, chunk[min(len(chunk), extendedXMPHeader):]...)
	case xmp.Contains(p):
		c.addAll(xmp.Findings(p, "APP1/XMP"))
	default:
		c.otherApp(1, p)
	}
}

// exifBlock reports a block and, for the first block ahead of the image data,
// takes its replacement. That is the block readers honour, so no later block may
// change how the picture is shown.
func (c *cleaner) exifBlock(block []byte) {
	d, _ := exif.Parse(block) // a damaged block reports itself through Findings
	opts := core.Options{Strict: true}
	if !c.sawExif && !c.sawScan {
		opts = c.opts
		c.orientation = d.Replacement(c.opts)
	}
	c.sawExif = true
	c.addAll(d.Findings("APP1/Exif", opts))
}

func exifSegment(block []byte) []byte {
	seg := binary.BigEndian.AppendUint16([]byte{0xFF, jpegmeta.MarkerAPP0 + 1}, uint16(2+len(exif.Prefix)+len(block)))
	seg = append(seg, exif.Prefix...)
	return append(seg, block...)
}

func (c *cleaner) addAll(findings []core.Finding) {
	for _, f := range findings {
		c.rep.Add(f)
	}
}

func (c *cleaner) reportExtendedXMP() {
	if c.sawExtendedXMP {
		c.addAll(xmp.Findings(c.extendedXMP, "APP1/ExtendedXMP"))
	}
}

func (c *cleaner) app2(raw, p []byte) {
	switch {
	case bytes.HasPrefix(p, []byte(sigICC)) && len(p) >= len(sigICC)+iccHeader:
		if c.sawScan {
			c.add(core.CatHidden, core.RiskLow, "Stray ICC profile data", core.ByteCount(len(p)), "APP2")
			return
		}
		c.icc = append(c.icc, iccChunk{
			part:  len(c.parts),
			seq:   p[len(sigICC)],
			count: p[len(sigICC)+1],
			data:  p[len(sigICC)+iccHeader:],
		})
		c.parts = append(c.parts, raw)
	case bytes.HasPrefix(p, []byte(sigMPF)):
		c.sawMPF = true
		c.add(core.CatHidden, core.RiskMed, "Multi-picture index", core.ByteCount(len(p)), "APP2/MPF")
	case bytes.HasPrefix(p, []byte(sigFlashPix)):
		c.add(core.CatHidden, core.RiskMed, "FlashPix data", core.ByteCount(len(p)), "APP2/FlashPix")
	default:
		c.otherApp(2, p)
	}
}

// iccChunk is one APP2 segment of an ICC profile.
type iccChunk struct {
	part       int // index into cleaner.parts
	seq, count byte
	data       []byte
}

// resolveICC decides what happens to the profile once all its chunks are known.
// Only a complete, recognisable profile is worth keeping: a decoder ignores
// anything else, so dropping it cannot change the picture, and it could be
// holding anything at all.
func (c *cleaner) resolveICC() {
	if len(c.icc) == 0 {
		return
	}
	total := 0
	for _, chunk := range c.icc {
		total += len(chunk.data)
	}
	finding := core.Finding{
		Category: core.CatTechnical,
		Risk:     core.RiskNone,
		Field:    "ICC colour profile",
		Value:    core.ByteCount(total),
		Where:    "APP2",
		Action:   core.ActionRemove,
	}
	valid := validICC(c.icc, total)
	switch {
	case !valid:
		finding.Category, finding.Risk, finding.Field = core.CatHidden, core.RiskMed, "Damaged ICC profile"
	case !c.opts.Strict:
		finding.Action, finding.Reason = core.ActionRetain, retainICC
	}
	c.rep.Add(finding)
	if !valid || c.opts.Strict {
		for _, chunk := range c.icc {
			c.parts[chunk.part] = nil
		}
	}
}

// validICC checks the sequence numbers as libjpeg-based readers do, and the size
// and signature in the profile header.
func validICC(chunks []iccChunk, total int) bool {
	const (
		sizeEnd      = 4
		signatureOff = 36
		signatureEnd = 40
	)
	if len(chunks) > 255 {
		return false
	}
	var seen [256]bool
	var first []byte
	for _, chunk := range chunks {
		if int(chunk.count) != len(chunks) || chunk.seq == 0 || int(chunk.seq) > len(chunks) || seen[chunk.seq] {
			return false
		}
		seen[chunk.seq] = true
		if chunk.seq == 1 {
			first = chunk.data
		}
	}
	if len(first) < signatureEnd {
		return false
	}
	return string(first[signatureOff:signatureEnd]) == "acsp" &&
		int64(binary.BigEndian.Uint32(first[:sizeEnd])) == int64(total)
}

func (c *cleaner) app13(p []byte) {
	if bytes.HasPrefix(p, []byte(sigPhotoshop)) {
		c.sawPhotoshop = true
		c.photoshop = append(c.photoshop, p[len(sigPhotoshop):]...)
		return
	}
	c.otherApp(13, p)
}

// app14 keeps the Adobe segment because it holds the colour transform that CMYK
// and RGB files depend on. Nothing else in it matters to a decoder, so the rest is
// written afresh, or those six bytes and any number of repeats would be free room.
//
// Decoders disagree on which of several segments wins, so a file whose segments
// disagree on the transform has no one picture to preserve, and is refused. One past
// the image data comes too late to change anything.
func (c *cleaner) app14(p []byte) error {
	if !bytes.HasPrefix(p, []byte(sigAdobe)) || len(p) < adobeSize {
		c.otherApp(14, p)
		return nil
	}
	const where = "APP14"
	if extra := len(p) - adobeSize; extra > 0 {
		c.add(core.CatHidden, core.RiskMed, "Extra Adobe data", core.ByteCount(extra), where)
	}
	transform := p[adobeSize-1]
	if c.sawScan || c.sawAdobe {
		if !c.sawScan && transform != c.adobeTransform {
			return core.Malformedf("jpeg: Adobe segments disagree on the colour transform")
		}
		c.add(core.CatHidden, core.RiskLow, "Stray Adobe segment", core.ByteCount(len(p)), where)
		return nil
	}
	c.sawAdobe = true
	c.adobeTransform = transform
	payload := append([]byte(sigAdobe), 0, adobeVersion, 0, 0, 0, 0, transform)
	if !bytes.Equal(payload, p[:adobeSize]) {
		c.rep.Add(core.Finding{
			Category: core.CatSoftware,
			Risk:     core.RiskLow,
			Field:    "Adobe header values",
			Value:    core.ByteCount(adobeSize - len(sigAdobe) - 1),
			Where:    where,
			Action:   core.ActionAnonymize,
		})
	}
	seg := binary.BigEndian.AppendUint16([]byte{0xFF, jpegmeta.MarkerAPP0 + 14}, adobeSize+2)
	c.parts = append(c.parts, append(seg, payload...))
	return nil
}

// otherApp reports any APPn segment HideMe has no specific knowledge of. They are
// all dropped, so one that is not listed here still cannot leak.
func (c *cleaner) otherApp(n int, p []byte) {
	field := fmt.Sprintf("APP%d segment", n)
	value := core.ByteCount(len(p))
	if label := segmentLabel(p); label != "" {
		value = label + ", " + value
	}
	c.add(core.CatHidden, core.RiskMed, field, value, fmt.Sprintf("APP%d", n))
}

// segmentLabel returns the printable name that most APPn payloads start with,
// which tells the user what wrote the segment.
func segmentLabel(p []byte) string {
	const maxLabel = 32
	for i, b := range p[:min(len(p), maxLabel)] {
		if b == 0 {
			return string(p[:i])
		}
		if b < 0x20 || b > 0x7E {
			return ""
		}
	}
	return ""
}
