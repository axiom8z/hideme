package isobmff

import (
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// kind says which of the two registered handlers owns a file.
type kind int

const (
	kindNone kind = iota
	kindHEIF
	kindMP4
)

// imageBrands are the ftyp brands that declare a still image or image sequence.
var imageBrands = map[string]bool{
	"heic": true, "heix": true, "hevc": true, "hevx": true,
	"heim": true, "heis": true, "hevm": true, "hevs": true,
	"mif1": true, "msf1": true, "avif": true, "avis": true,
}

const (
	// maxDetectBoxes bounds how far detection looks for a meta or moov, so that a
	// file of tiny boxes cannot make every detection slow. Real files have these
	// within the first few boxes or just after one mdat.
	maxDetectBoxes = 1 << 16
	// maxBrandBox bounds an ftyp or styp at about sixty brands. Real ones list
	// fewer than twenty, so anything longer is treated as somewhere to hide data.
	maxBrandBox = 256
)

// detection is what the first boxes of a file say about it.
type detection struct {
	kind   kind
	format string // value for Report.Format
	refuse string // set when the handler owns the file but must not clean it
}

// detect classifies a file from its top-level boxes. Both Match and analyze call
// it, so a file cannot be accepted by one and judged differently by the other,
// and the two handlers cannot both claim a file.
func detect(src core.Source) detection {
	var head [16]byte
	n := min(src.Size(), int64(len(head)))
	if n < 8 || readFull(src, head[:n], 0) != nil {
		return detection{}
	}
	first, ok := parseHeader(head[:n], 0, src.Size())
	if !ok {
		return detection{}
	}
	switch first.typ {
	case "ftyp":
		return detectBrands(src, first)
	case "moov", "mdat", "wide", "free", "skip":
		// Legacy QuickTime files have no ftyp. Those first boxes are too common to
		// claim on their own, so a moov must also be present.
		if hasTopLevel(src, "moov") {
			return detection{kind: kindMP4, format: "mov"}
		}
	}
	return detection{}
}

func detectBrands(src core.Source, ftyp box) detection {
	payload := ftyp.end - ftyp.body
	if payload < 8 || payload > maxBrandBox {
		return detection{}
	}
	brands := make([]byte, payload)
	if readFull(src, brands, ftyp.body) != nil {
		return detection{}
	}
	major := string(brands[:4])
	isImage := imageBrands[major]
	isAVIF := major == "avif" || major == "avis"
	// Bytes 4 to 8 are the minor version, not a brand.
	for i := int64(8); i+4 <= payload; i += 4 {
		brand := string(brands[i : i+4])
		isImage = isImage || imageBrands[brand]
		isAVIF = isAVIF || brand == "avif" || brand == "avis"
	}
	if isImage && hasTopLevel(src, "meta") {
		format := "heif"
		switch {
		case isAVIF:
			format = "avif"
		case major != "mif1" && major != "msf1" && imageBrands[major]:
			format = "heic"
		}
		return detection{kind: kindHEIF, format: format}
	}
	d := detection{kind: kindMP4, format: mediaFormat(major)}
	if major == "crx " {
		d.refuse = "Canon CR3 raw files are not supported"
	}
	return d
}

// mediaFormat names the format a movie's major brand declares.
func mediaFormat(major string) string {
	switch {
	case major == "M4A ":
		return "m4a"
	case major == "M4B ":
		return "m4b"
	case strings.HasPrefix(major, "M4V"):
		return "m4v"
	case major == "qt  ":
		return "mov"
	case strings.HasPrefix(major, "3g"):
		return "3gp"
	}
	return "mp4"
}

func hasTopLevel(src core.Source, typ string) bool {
	found := false
	count := 0
	// A read error leaves found false, which is the right answer for Match:
	// anything that cannot be read simply does not match.
	_, _ = scanTop(src, func(b box) bool {
		found = b.typ == typ
		count++
		return !found && count < maxDetectBoxes
	})
	return found
}
