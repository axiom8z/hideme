package png

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	// maxInflatedChunk caps one compressed chunk and maxInflatedTotal caps a whole
	// file. Going over either one is treated as a decompression bomb. Real text
	// chunks are small, and the largest ICC profiles are tens of megabytes.
	maxInflatedChunk = 64 << 20
	maxInflatedTotal = 256 << 20
	// maxTextBytes bounds how much of one value is decoded for display. core.SafeText
	// shows only a couple of hundred characters of it anyway, and each byte of
	// Latin-1 costs four while decoding, so a huge chunk must not be decoded whole.
	maxTextBytes = 4096

	xmpKeyword       = "XML:com.adobe.xmp"
	rawProfilePrefix = "Raw profile type "

	// neutralICCName replaces the free-text name in iCCP, which can identify a
	// person or a machine. Pillow writes the same name, so such files stay as is.
	neutralICCName = "ICC Profile"

	retainICC = "needed to display colours correctly; strict mode removes it"
)

// textField says how to report a text chunk with a well-known keyword.
type textField struct {
	field    string
	category core.Category
	risk     core.Risk
}

// textFields is keyed by lowercase keyword, because writers vary the case.
var textFields = map[string]textField{
	"title":         {"Title", core.CatContent, core.RiskLow},
	"author":        {"Author", core.CatIdentity, core.RiskHigh},
	"description":   {"Description", core.CatContent, core.RiskMed},
	"copyright":     {"Copyright", core.CatIdentity, core.RiskHigh},
	"creation time": {"Creation time", core.CatTime, core.RiskMed},
	"software":      {"Software", core.CatSoftware, core.RiskLow},
	"disclaimer":    {"Disclaimer", core.CatContent, core.RiskLow},
	"warning":       {"Warning", core.CatContent, core.RiskLow},
	"source":        {"Source device", core.CatDevice, core.RiskMed},
	"comment":       {"Comment", core.CatContent, core.RiskMed},
	"date":          {"Date", core.CatTime, core.RiskMed},
	"location":      {"Location", core.CatLocation, core.RiskHigh},
}

// text reports a tEXt, zTXt or iTXt chunk. The chunk is dropped whatever it
// holds, so damage can only make the report less specific. The one failure is a
// decompression bomb, since finishing the read would cost more than it is worth.
func (c *cleaner) text(ch chunk) error {
	keyword, rest, found := bytes.Cut(ch.data, []byte{0})
	key := decodeLatin1(keyword)
	if !found {
		c.remove(core.CatContent, core.RiskMed, "Text metadata", fmt.Sprintf("damaged, %s", core.ByteCount(len(ch.data))), ch.typ)
		return nil
	}
	var value []byte
	var damaged bool
	var err error
	switch ch.typ {
	case "tEXt":
		value = rest
	case "zTXt":
		value, damaged, err = c.inflateText(rest)
	default:
		value, damaged, err = c.internationalText(rest)
	}
	if err != nil {
		return err
	}
	c.reportText(ch.typ, key, value, damaged, ch.typ == "iTXt")
	return nil
}

// inflateText decompresses a method byte and compressed data, as in zTXt.
func (c *cleaner) inflateText(p []byte) (value []byte, damaged bool, err error) {
	if len(p) < 1 || p[0] != 0 {
		return nil, true, nil
	}
	return c.inflateAllowingDamage(p[1:])
}

func (c *cleaner) inflateAllowingDamage(p []byte) (value []byte, damaged bool, err error) {
	value, _, err = c.inflate(p)
	switch {
	case errors.Is(err, core.ErrTooLarge):
		return nil, false, err
	case err != nil:
		return value, true, nil
	}
	return value, false, nil
}

// internationalText reads iTXt's fields after the keyword: a compression flag and
// method, a language tag, a translated keyword, and the text itself.
func (c *cleaner) internationalText(p []byte) (value []byte, damaged bool, err error) {
	if len(p) < 2 {
		return nil, true, nil
	}
	compressed, method := p[0], p[1]
	_, rest, ok := bytes.Cut(p[2:], []byte{0})
	if !ok {
		return nil, true, nil
	}
	_, rest, ok = bytes.Cut(rest, []byte{0})
	if !ok {
		return nil, true, nil
	}
	switch {
	case compressed == 0:
		return rest, false, nil
	case method != 0:
		return nil, true, nil
	}
	return c.inflateAllowingDamage(rest)
}

func (c *cleaner) reportText(where, key string, value []byte, damaged, isUTF8 bool) {
	switch {
	case key == xmpKeyword || xmp.Contains(value):
		c.addAll(xmp.Findings(value, where+"/XMP"))
		return
	case strings.HasPrefix(strings.ToLower(key), strings.ToLower(rawProfilePrefix)):
		c.rawProfile(where, key, value)
		return
	}
	text := decodeLatin1(value)
	if isUTF8 && utf8.Valid(value) {
		text = string(value[:min(len(value), maxTextBytes)])
	}
	if damaged {
		text = strings.TrimSpace(text + " (damaged)")
	}
	if spec, ok := textFields[strings.ToLower(key)]; ok {
		c.remove(spec.category, spec.risk, spec.field, text, where)
		return
	}
	c.remove(core.CatContent, core.RiskMed, "Text metadata", key+": "+text, where)
}

// rawProfile decodes the hex-encoded profiles ImageMagick stores in text chunks.
// They carry whole EXIF and XMP blocks, including GPS coordinates, into a PNG
// converted from a phone photo, so reporting just the keyword would undersell them.
//
// The keyword is free text from the file, so it goes in the value alone: clean prints
// Where, and promises not to repeat what it removes.
func (c *cleaner) rawProfile(where, key string, value []byte) {
	where += "/raw profile"
	block := decodeRawProfile(value)
	switch {
	case block == nil:
		c.remove(core.CatHidden, core.RiskHigh, "Raw metadata profile", fmt.Sprintf("%s, %s", key, core.ByteCount(len(value))), where)
	case xmp.Contains(block):
		c.addAll(xmp.Findings(block, where))
	default:
		if d, err := exif.Parse(exif.StripPrefix(block)); err == nil {
			c.addAll(d.Findings(where, core.Options{Strict: true}))
			return
		}
		c.remove(core.CatHidden, core.RiskHigh, "Raw metadata profile", fmt.Sprintf("%s, %s", key, core.ByteCount(len(block))), where)
	}
}

// decodeRawProfile returns the bytes of an ImageMagick raw profile, which reads
// "\nNAME\n   LENGTH\nHEX...", or nil if value does not have that shape. It walks
// the bytes by hand because splitting a value of mostly spaces into fields would
// allocate many times its size.
func decodeRawProfile(value []byte) []byte {
	const space = " \t\r\n"
	if len(value) > maxInflatedChunk {
		return nil
	}
	rest := value
	for range 2 { // the name and the length
		rest = bytes.TrimLeft(rest, space)
		i := bytes.IndexAny(rest, space)
		if i < 0 {
			return nil
		}
		rest = rest[i:]
	}
	raw := make([]byte, 0, len(rest))
	for _, b := range rest {
		if !strings.ContainsRune(space, rune(b)) {
			raw = append(raw, b)
		}
	}
	if len(raw) == 0 {
		return nil
	}
	decoded := make([]byte, hex.DecodedLen(len(raw)))
	if _, err := hex.Decode(decoded, raw); err != nil {
		return nil
	}
	return decoded
}

// inflate decompresses a zlib stream and reports how many input bytes followed
// it. A bytes.Reader implements io.ByteReader, so zlib reads no further than the
// end of the stream and that count is exact.
//
// Output past the caps returns core.ErrTooLarge. Any other error is damage, and
// comes with whatever could be decompressed.
func (c *cleaner) inflate(p []byte) (out []byte, trailing int, err error) {
	src := bytes.NewReader(p)
	zr, err := zlib.NewReader(src)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	limit := min(maxInflatedChunk, maxInflatedTotal-c.inflated)
	out, err = io.ReadAll(io.LimitReader(zr, limit+1))
	c.inflated += int64(len(out))
	if int64(len(out)) > limit {
		return nil, 0, fmt.Errorf("%w: png: compressed chunk expands past %d bytes", core.ErrTooLarge, limit)
	}
	return out, src.Len(), err
}

// iccp handles the colour profile chunk. Like JPEG's, it is only worth keeping if
// it is a complete, recognisable profile. A decoder ignores anything else, so
// dropping it cannot change the picture, and it could be holding anything at all.
func (c *cleaner) iccp(ch chunk) error {
	const where = "iCCP"
	if c.sawICC || c.sawIDAT {
		c.remove(core.CatHidden, core.RiskLow, "Extra ICC profile", core.ByteCount(len(ch.data)), where)
		return nil
	}
	c.sawICC = true
	name, rest, found := bytes.Cut(ch.data, []byte{0})
	var profile []byte
	valid := false
	if found && len(name) >= 1 && len(name) <= 79 && len(rest) >= 1 && rest[0] == 0 {
		var trailing int
		var err error
		profile, trailing, err = c.inflate(rest[1:])
		if errors.Is(err, core.ErrTooLarge) {
			return err
		}
		valid = err == nil && trailing == 0 && validICC(profile)
	}
	if !valid {
		c.remove(core.CatHidden, core.RiskMed, "Damaged ICC profile", core.ByteCount(len(ch.data)), where)
		return nil
	}
	finding := core.Finding{
		Category: core.CatTechnical,
		Risk:     core.RiskNone,
		Field:    "ICC colour profile",
		Value:    core.ByteCount(len(profile)),
		Where:    where,
		Action:   core.ActionRemove,
	}
	if !c.opts.Strict {
		finding.Action, finding.Reason = core.ActionRetain, retainICC
		c.parts = append(c.parts, buildChunk("iCCP", append([]byte(neutralICCName+"\x00"), rest...)))
	}
	c.add(finding)
	if string(name) != neutralICCName {
		c.add(core.Finding{
			Category: core.CatIdentity,
			Risk:     core.RiskLow,
			Field:    "ICC profile name",
			Value:    decodeLatin1(name),
			Where:    where,
			Action:   nameAction(c.opts),
		})
	}
	return nil
}

// nameAction says what happens to the profile's name: it is replaced when the
// profile stays and goes with the profile otherwise.
func nameAction(opts core.Options) core.Action {
	if opts.Strict {
		return core.ActionRemove
	}
	return core.ActionAnonymize
}

// validICC checks the profile header's size and signature.
func validICC(profile []byte) bool {
	const (
		signatureOff = 36
		signatureEnd = 40
	)
	return len(profile) >= signatureEnd &&
		string(profile[signatureOff:signatureEnd]) == "acsp" &&
		int64(binary.BigEndian.Uint32(profile)) == int64(len(profile))
}

// decodeLatin1 converts PNG's Latin-1 keywords and tEXt values to UTF-8.
func decodeLatin1(b []byte) string {
	b = b[:min(len(b), maxTextBytes)]
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}
