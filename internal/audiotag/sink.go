package audiotag

import (
	"fmt"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// maxFindings caps one file's report. A hostile file can be nothing but tiny tags
// and, left uncapped, would cost far more memory as findings than it does as
// bytes. Everything past the cap is still removed; it just goes unlisted.
const maxFindings = 4096

// maxInflated caps what one file's tags can decompress in total. Each compressed ID3
// frame is capped too, but a hostile file can hold millions of tiny frames that each
// expand a thousandfold. Tags are removed whole, so past this they only lose detail.
const maxInflated = 64 << 20

// Sink builds a report under those caps. It lives as long as the walk over one file,
// which makes it the natural place for limits that must span all of a file's tags.
type Sink struct {
	report   *core.Report
	omitted  int
	inflated int64
}

// full reports whether further findings would go unlisted. There is no use decoding
// a tag past that point, and a file of nothing but tiny tags would make it expensive.
func (s *Sink) full() bool {
	return len(s.report.Findings) >= maxFindings
}

// NewSink returns a Sink for a file of the given format ID.
func NewSink(format string) *Sink {
	return &Sink{report: &core.Report{Format: format, Findings: []core.Finding{}}}
}

// Add records a finding.
func (s *Sink) Add(f core.Finding) {
	if len(s.report.Findings) >= maxFindings {
		s.omitted++
		return
	}
	s.report.Add(f)
}

// Warn records a warning.
func (s *Sink) Warn(format string, args ...any) {
	s.report.Warn(format, args...)
}

// Report returns the finished report. Call it once, after the last Add.
func (s *Sink) Report() *core.Report {
	if s.omitted > 0 {
		s.report.Add(moreItemsField.Remove(fmt.Sprintf("%d more", s.omitted), ""))
	}
	return s.report
}

// Field says how to report one kind of metadata.
type Field struct {
	Name string
	Cat  core.Category
	Risk core.Risk
}

// Remove returns a finding for a value that Clean removes.
func (f Field) Remove(value, where string) core.Finding {
	return core.Finding{Category: f.Cat, Risk: f.Risk, Field: f.Name, Value: value, Where: where, Action: core.ActionRemove}
}

// Fields shared by more than one handler or tag format.
var (
	TitleField     = Field{"Title", core.CatContent, core.RiskLow}
	ArtistField    = Field{"Artist", core.CatIdentity, core.RiskHigh}
	AlbumField     = Field{"Album", core.CatContent, core.RiskLow}
	CommentField   = Field{"Comment", core.CatContent, core.RiskMed}
	CopyrightField = Field{"Copyright", core.CatIdentity, core.RiskMed}
	DateField      = Field{"Date", core.CatTime, core.RiskMed}
	EncoderField   = Field{"Encoder", core.CatSoftware, core.RiskLow}
	GenreField     = Field{"Genre", core.CatContent, core.RiskLow}
	LyricsField    = Field{"Lyrics", core.CatContent, core.RiskLow}
	TrackField     = Field{"Track number", core.CatContent, core.RiskLow}
	URLField       = Field{"URL", core.CatIdentity, core.RiskMed}
	PictureField   = Field{"Embedded picture", core.CatThumbnail, core.RiskMed}
	// TrailingField covers bytes after the logical end of a file, where phones
	// and steganography tools hide data.
	TrailingField = Field{"Trailing data", core.CatHidden, core.RiskMed}
	// PaddingField covers padding that is empty; hidden data in padding is
	// reported with HiddenPaddingField instead, so the distinction reaches the user.
	PaddingField       = Field{"Padding", core.CatHidden, core.RiskNone}
	HiddenPaddingField = Field{"Data hidden in padding", core.CatHidden, core.RiskMed}
	// OtherChunkField covers container chunks and blocks that nothing recognises.
	OtherChunkField = Field{"Unknown chunk", core.CatHidden, core.RiskMed}

	moreItemsField = Field{"Further metadata items", core.CatContent, core.RiskMed}
)

// Join builds a Finding.Where path.
func Join(where, what string) string {
	if where == "" {
		return what
	}
	return where + "/" + what
}

// Latin1 decodes ISO 8859-1, which is what ID3v1 and many other tags hold. Each
// byte is the code point of the same value.
func Latin1(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// FixedText decodes a fixed-width text field: it stops at the first NUL and trims
// the padding that such fields usually carry.
func FixedText(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(Latin1(b))
}
