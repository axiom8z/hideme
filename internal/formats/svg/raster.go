package svg

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// whitespaceRefs are the character references that Inkscape and others use to wrap
// long base64 values inside an attribute.
var whitespaceRefs = strings.NewReplacer(
	"&#10;", "", "&#xA;", "", "&#xa;", "", "&#13;", "", "&#xD;", "", "&#xd;", "",
	"&#9;", "", "&#x9;", "", "&#32;", "", "&#x20;", "",
)

// embeddedImage cleans the image in a data URI. handled is false if value is not a
// data URI at all. An image that cannot be reached, or that no handler knows, stays
// as it was with a warning; replacement is nil in those cases.
//
// The value is read as a browser reads it, with its character references expanded and
// the whitespace that URLs ignore taken out, because any of those forms draws the image.
func (c *cleaner) embeddedImage(value []byte) (replacement []byte, handled bool, err error) {
	text := normalizedURL(value)
	if len(text) < len("data:") || !strings.EqualFold(text[:len("data:")], "data:") {
		return nil, false, nil
	}
	header, payload, ok := strings.Cut(text[len("data:"):], ",")
	if !ok {
		return nil, true, core.Malformedf("svg: a data URI has no comma")
	}
	mediaType, params, _ := strings.Cut(header, ";")
	base64Encoded := false
	var extra []string
	for param := range strings.SplitSeq(params, ";") {
		switch {
		case param == "":
		case strings.EqualFold(strings.TrimSpace(param), "base64"):
			base64Encoded = true
		default:
			extra = append(extra, param)
		}
	}
	if !base64Encoded || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		c.warn(warnOpaqueImage, "An image element holds a data URI that is not a base64 image. It was kept and not cleaned.")
		return nil, true, nil
	}
	// Parameters are the one place a data URI can hold a file name.
	raw, err := decodeImage(payload, maxEmbeddedImage)
	if err != nil {
		return nil, true, err
	}
	cleaned, nested, err := core.CleanBytes(c.ctx, raw, c.opts)
	if errors.Is(err, core.ErrUnsupported) {
		c.warn(warnUnsupportedImage, "An embedded image is in a format HideMe cannot clean. It was kept unchanged.")
		return nil, true, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("svg: embedded image: %w", err)
	}
	c.mergeNested(nested)
	if len(extra) > 0 {
		c.add(finding{"Embedded image parameters", core.CatHidden, core.RiskMed}, strings.Join(extra, ";"), "embedded image")
	}
	return encodeImage(mediaType, cleaned), true, nil
}

// decodeImage decodes a data URI's base64 payload, refusing a result over limit
// before allocating it. Browsers accept the wrapped and percent-encoded forms that
// real files contain, so this does too. Anything else cannot be vouched for.
func decodeImage(payload string, limit int) ([]byte, error) {
	if strings.Contains(payload, "&") {
		payload = whitespaceRefs.Replace(payload)
	}
	if strings.Contains(payload, "%") {
		unescaped, err := url.PathUnescape(payload)
		if err != nil {
			return nil, core.Malformedf("svg: a data URI has invalid percent-encoding")
		}
		payload = unescaped
	}
	if strings.ContainsAny(payload, " \t\r\n") {
		payload = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(payload)
	}
	payload = strings.TrimRight(payload, "=")
	if n := base64.RawStdEncoding.DecodedLen(len(payload)); n > limit {
		return nil, fmt.Errorf("%w: an embedded image of %d bytes", core.ErrTooLarge, n)
	}
	raw, err := base64.RawStdEncoding.DecodeString(payload)
	if err != nil {
		return nil, core.Malformedf("svg: a data URI holds invalid base64: %v", err)
	}
	return raw, nil
}

// encodeImage writes the canonical form: no parameters, no wrapping and padding as
// standard. That also makes the result stable under a second clean.
func encodeImage(mediaType string, image []byte) []byte {
	var b bytes.Buffer
	b.Grow(len("data:;base64,") + len(mediaType) + base64.StdEncoding.EncodedLen(len(image)))
	b.WriteString("data:")
	b.WriteString(mediaType)
	b.WriteString(";base64,")
	enc := base64.NewEncoder(base64.StdEncoding, &b)
	// A bytes.Buffer never fails to write.
	_, _ = enc.Write(image)
	_ = enc.Close()
	return b.Bytes()
}
