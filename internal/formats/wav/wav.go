// Package wav cleans WAV files by rebuilding the RIFF container from its fmt, fact
// and data chunks. Everything else in a WAV file is optional, so everything else
// goes, including chunk types nobody has thought of yet.
package wav

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/riff"
	"github.com/axiom8z/hideme/internal/xmp"
)

const (
	formatID = "wav"
	formWAVE = "WAVE"

	idFmt  = "fmt "
	idFact = "fact"
	idData = "data"

	minFmtLen  = 16
	minFactLen = 4
	// extraFmtLen is the plain fmt fields and the count of the codec's extra bytes.
	extraFmtLen = 18
	// extensibleFmtLen is everything WAVE_FORMAT_EXTENSIBLE defines.
	extensibleFmtLen = 40

	// streamingSize is what writers put in a size field when they cannot go back
	// and fill it in. No extent can be trusted in such a file.
	streamingSize = math.MaxUint32

	formatPCM        = 0x0001
	formatFloat      = 0x0003
	formatALaw       = 0x0006
	formatMuLaw      = 0x0007
	formatExtensible = 0xFFFE
	// extensibleTagOff is where WAVE_FORMAT_EXTENSIBLE's sub-format begins in the
	// fmt chunk; its first two bytes are the effective format tag.
	extensibleTagOff = 24

	// maxTextRead bounds what is read of one text value for the report.
	maxTextRead = 64 << 10
	// maxXMLRead bounds what is read of an XML chunk. XMP goes through the xmp
	// package, whose own cap is larger than any packet seen in a WAV file.
	maxXMLRead = 1 << 20
	// maxListDepth caps recursion through LIST chunks, which a hostile file can
	// nest millions of levels deep.
	maxListDepth = 4
)

// sampleCodecs are the codecs whose data chunk is nothing but coded samples. It is
// copied without being read, which would keep the tags of a stream with a container of
// its own, as MP3, FLAC, Vorbis and AAC streams have.
var sampleCodecs = map[uint16]bool{
	formatPCM:   true,
	0x0002:      true, // Microsoft ADPCM
	formatFloat: true,
	formatALaw:  true,
	formatMuLaw: true,
	0x0010:      true, // OKI ADPCM
	0x0011:      true, // IMA ADPCM
	0x0014:      true, // G.723 ADPCM
	0x0017:      true, // Dialogic OKI ADPCM
	0x0020:      true, // Yamaha ADPCM
	0x0031:      true, // GSM 6.10
	0x0040:      true, // G.721 ADPCM
	0x0045:      true, // G.726 ADPCM
	0x0061:      true, // Duck DK4 ADPCM
	0x0062:      true, // Duck DK3 ADPCM
	0x0064:      true, // G.726 ADPCM
	0x0065:      true, // G.722 ADPCM
	0x0069:      true, // IMA ADPCM, as Voxware labels it
	0x0200:      true, // Creative ADPCM
	0x028F:      true, // G.722 ADPCM, as ffmpeg labels it
}

var (
	engineerField   = audiotag.Field{Name: "Engineer", Cat: core.CatIdentity, Risk: core.RiskHigh}
	originatorField = audiotag.Field{Name: "Originator", Cat: core.CatIdentity, Risk: core.RiskHigh}
	softwareField   = audiotag.Field{Name: "Software", Cat: core.CatSoftware, Risk: core.RiskLow}
	dateField       = audiotag.Field{Name: "Creation date", Cat: core.CatTime, Risk: core.RiskMed}
	bextDateField   = audiotag.Field{Name: "Origination date", Cat: core.CatTime, Risk: core.RiskMed}
	descField       = audiotag.Field{Name: "Description", Cat: core.CatContent, Risk: core.RiskMed}
	bextRefField    = audiotag.Field{Name: "Originator reference", Cat: core.CatIdentity, Risk: core.RiskMed}
	bextUMIDField   = audiotag.Field{Name: "UMID", Cat: core.CatIdentity, Risk: core.RiskMed}
	historyField    = audiotag.Field{Name: "Coding history", Cat: core.CatSoftware, Risk: core.RiskMed}
	bextField       = audiotag.Field{Name: "Broadcast WAV chunk", Cat: core.CatContent, Risk: core.RiskLow}
	ixmlField       = audiotag.Field{Name: "iXML metadata", Cat: core.CatDevice, Risk: core.RiskMed}
	axmlField       = audiotag.Field{Name: "AXML metadata", Cat: core.CatContent, Risk: core.RiskMed}
	xmpField        = audiotag.Field{Name: "XMP packet", Cat: core.CatHidden, Risk: core.RiskMed}
	cueField        = audiotag.Field{Name: "Cue points", Cat: core.CatContent, Risk: core.RiskLow}
	samplerField    = audiotag.Field{Name: "Sampler data", Cat: core.CatTechnical, Risk: core.RiskLow}
	labelField      = audiotag.Field{Name: "Cue label", Cat: core.CatContent, Risk: core.RiskLow}
	otherInfoField  = audiotag.Field{Name: "Other INFO field", Cat: core.CatContent, Risk: core.RiskMed}
	listField       = audiotag.Field{Name: "LIST chunk", Cat: core.CatContent, Risk: core.RiskMed}
	damagedField    = audiotag.Field{Name: "Damaged LIST chunk", Cat: core.CatHidden, Risk: core.RiskMed}
)

// infoFields maps LIST INFO subchunks to what they expose.
var infoFields = map[string]audiotag.Field{
	"IART": audiotag.ArtistField,
	"INAM": audiotag.TitleField,
	"IPRD": audiotag.AlbumField,
	"ICMT": audiotag.CommentField,
	"ICOP": audiotag.CopyrightField,
	"IGNR": audiotag.GenreField,
	"ITRK": audiotag.TrackField,
	"ICRD": dateField,
	"IDIT": dateField,
	"ISFT": softwareField,
	"ITCH": {Name: "Technician", Cat: core.CatIdentity, Risk: core.RiskHigh},
	"IENG": engineerField,
	"ICMS": {Name: "Commissioned by", Cat: core.CatIdentity, Risk: core.RiskHigh},
	"ISRC": {Name: "Source", Cat: core.CatIdentity, Risk: core.RiskMed},
	"IARL": {Name: "Archival location", Cat: core.CatLocation, Risk: core.RiskHigh},
	"ICNT": {Name: "Country", Cat: core.CatLocation, Risk: core.RiskMed},
	"ISBJ": {Name: "Subject", Cat: core.CatContent, Risk: core.RiskMed},
	"IKEY": {Name: "Keywords", Cat: core.CatContent, Risk: core.RiskMed},
	"ILNG": {Name: "Language", Cat: core.CatContent, Risk: core.RiskLow},
	"ISMP": {Name: "SMPTE time", Cat: core.CatTime, Risk: core.RiskMed},
	"IMED": {Name: "Medium", Cat: core.CatTechnical, Risk: core.RiskNone},
	"ISRF": {Name: "Source form", Cat: core.CatTechnical, Risk: core.RiskNone},
}

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "WAV audio",
		Family:     core.FamilyAudio,
		Extensions: []string{".wav", ".wave", ".bwf"},
		Limitations: []string{
			"RF64 and BW64 (WAV over 4 GiB), RIFX, and files with streaming sizes are refused.",
			"Only PCM, floating point, A-law, mu-law, ADPCM and GSM audio is accepted. Other codecs, such as MP3 or FLAC inside a WAV file, are refused, because tags can sit inside the data chunk where this handler does not look.",
			"The samples are copied unread, and so are the extra format bytes that a codec such as ADPCM declares, so anything hidden in those stays. Readers do not check the byte rate in the format chunk either, which is 4 bytes.",
			"A file whose data chunk is empty with bytes after it is refused: its writer probably never filled in its sizes, and those bytes are the audio.",
		},
	}
}

// Match also claims the WAV variants that Clean refuses, so that the error names
// the variant rather than saying nothing recognises the file.
func (handler) Match(head []byte, _ core.Source) bool {
	if len(head) < 12 || string(head[8:12]) != formWAVE {
		return false
	}
	switch string(head[:4]) {
	case "RIFF", "RIFX", "RF64", "BW64":
		return true
	}
	return false
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	return process(ctx, src, nil)
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	return process(ctx, src, dst)
}

// process walks the chunks and, when dst is not nil, writes the cleaned copy.
func process(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
	win := audiotag.NewWindow(src)
	if head := win.AtMost(0, 8); len(head) == 8 && string(head[:4]) == "RIFF" && binary.LittleEndian.Uint32(head[4:]) == streamingSize {
		return nil, core.Unsupportedf("wav: the RIFF size is the streaming placeholder")
	}
	root, form, err := riff.Open(src)
	if err != nil {
		return nil, err
	}
	if form != formWAVE {
		return nil, core.Unsupportedf("wav: RIFF form %q is not WAVE", form)
	}
	p := &parser{ctx: ctx, src: src, win: win, sink: audiotag.NewSink(formatID)}
	if err := p.walk(root); err != nil {
		return nil, err
	}
	if p.fmtChunk == nil || p.dataChunk == nil {
		return nil, core.Malformedf("wav: the fmt and data chunks are both required")
	}
	// ffmpeg plays everything after an empty data chunk as samples, so going by the
	// sizes here could write out a silent file and report success.
	if p.dataChunk.Size == 0 && p.dataChunk.End < src.Size() {
		return nil, core.Unsupportedf("wav: the data chunk is empty but bytes follow it, as if its writer never filled in its sizes")
	}
	if root.End < src.Size() {
		if _, _, err := audiotag.ScanTrailing(ctx, win, root.End, "after the RIFF chunk", p.sink); err != nil {
			return nil, err
		}
	}
	if dst != nil {
		if err := p.write(dst); err != nil {
			return nil, err
		}
	}
	return p.sink.Report(), win.Err()
}

// parser holds the state of one pass over a file.
type parser struct {
	ctx  context.Context
	src  core.Source
	win  *audiotag.Window
	sink *audiotag.Sink

	kept      []riff.Chunk // in file order
	fmtChunk  *riff.Chunk
	factChunk *riff.Chunk
	dataChunk *riff.Chunk
}

func (p *parser) walk(root riff.Chunk) error {
	next := root.DataOff + 4
	for c, err := range riff.Children(p.ctx, p.src, root) {
		if err != nil {
			return p.explain(next, err)
		}
		next = c.End
		if err := p.chunk(c); err != nil {
			return err
		}
	}
	return p.win.Err()
}

// explain turns a chunk overrun caused by a streaming data size into the refusal
// it deserves. riff.Chunks reports it as damage, which it would be in any other
// chunk, but a file like this is well formed and just not supported.
func (p *parser) explain(next int64, err error) error {
	hdr := p.win.At(next, 8)
	if hdr != nil && string(hdr[:4]) == idData && binary.LittleEndian.Uint32(hdr[4:]) == streamingSize {
		return core.Unsupportedf("wav: the data chunk has the streaming size placeholder")
	}
	return err
}

func (p *parser) chunk(c riff.Chunk) error {
	switch c.ID {
	case idFmt:
		return p.keepFmt(c)
	case idFact:
		if p.factChunk != nil {
			return core.Malformedf("wav: more than one fact chunk")
		}
		if c.Size < minFactLen {
			return core.Malformedf("wav: fact chunk of %d bytes", c.Size)
		}
		if err := p.dropSpare(&c, minFactLen, idFact); err != nil {
			return err
		}
		p.factChunk = &c
		p.kept = append(p.kept, c)
	case idData:
		if p.fmtChunk == nil {
			return core.Malformedf("wav: data chunk comes before the fmt chunk")
		}
		if p.dataChunk != nil {
			return core.Malformedf("wav: more than one data chunk")
		}
		p.dataChunk = &c
		p.kept = append(p.kept, c)
	default:
		return p.report(c)
	}
	return nil
}

func (p *parser) keepFmt(c riff.Chunk) error {
	if p.fmtChunk != nil {
		return core.Malformedf("wav: more than one fmt chunk")
	}
	if c.Size < minFmtLen {
		return core.Malformedf("wav: fmt chunk of %d bytes", c.Size)
	}
	body := p.win.At(c.DataOff, int(min(c.Size, extensibleTagOff+2)))
	if body == nil {
		return p.win.Err()
	}
	tag := binary.LittleEndian.Uint16(body)
	codec := tag
	if tag == formatExtensible {
		if len(body) < extensibleTagOff+2 {
			return core.Malformedf("wav: extensible fmt chunk of %d bytes", c.Size)
		}
		codec = binary.LittleEndian.Uint16(body[extensibleTagOff:])
	}
	if !sampleCodecs[codec] {
		return core.Unsupportedf("wav: audio format %#04x is not plain samples, so its data chunk could hold tags", codec)
	}
	if err := p.dropSpare(&c, fmtFieldsLen(tag, body, c.Size), "fmt"); err != nil {
		return err
	}
	p.fmtChunk = &c
	p.kept = append(p.kept, c)
	return nil
}

// fmtFieldsLen returns how much of a fmt chunk its fields define, given its format
// tag and at least its first extraFmtLen bytes, if it has that many.
func fmtFieldsLen(tag uint16, body []byte, size int64) int64 {
	if tag == formatExtensible {
		return min(size, extensibleFmtLen)
	}
	if size < extraFmtLen {
		return minFmtLen
	}
	extra := int64(binary.LittleEndian.Uint16(body[minFmtLen:]))
	switch tag {
	case formatPCM, formatFloat, formatALaw, formatMuLaw:
		// None of these has a use for extra bytes, so the count can only be zero.
		if extra != 0 {
			return minFmtLen
		}
	}
	return min(size, extraFmtLen+extra)
}

// dropSpare shortens a kept chunk to the bytes its fields define, reporting the rest.
// Nothing reads those, so they cannot be left in place as part of a chunk that stays.
func (p *parser) dropSpare(c *riff.Chunk, keep int64, where string) error {
	if keep < c.Size {
		spare := riff.Chunk{DataOff: c.DataOff + keep, Size: c.Size - keep}
		if err := p.reportPadding(spare, where); err != nil {
			return err
		}
		c.Size = keep
	}
	return nil
}

// report adds findings for a chunk that is being dropped.
func (p *parser) report(c riff.Chunk) error {
	id := audiotag.FixedText([]byte(c.ID))
	where := id
	switch c.ID {
	case "LIST":
		return p.reportList(c, 0)
	case "bext":
		p.reportBext(c)
	case "iXML":
		p.reportXML(ixmlField, c, where)
	case "axml", "aXML":
		p.reportXML(axmlField, c, where)
	case "_PMX":
		p.reportXML(xmpField, c, where)
	case "id3 ", "ID3 ", "id3\x00", "ID3\x00":
		return p.reportID3(c)
	case "cue ":
		p.sink.Add(cueField.Remove(core.ByteCount(c.Size), where))
	case "smpl", "inst", "acid", "strc":
		p.sink.Add(samplerField.Remove(core.ByteCount(c.Size), where))
	case "JUNK", "junk", "PAD ", "FLLR", "fllr":
		return p.reportPadding(c, where)
	default:
		p.sink.Add(audiotag.OtherChunkField.Remove(fmt.Sprintf("%q, %s", c.ID, core.ByteCount(c.Size)), "RIFF"))
	}
	return p.win.Err()
}

func (p *parser) reportPadding(c riff.Chunk, where string) error {
	empty, err := audiotag.AllZero(p.ctx, p.win, c.DataOff, c.Size)
	if err != nil {
		return err
	}
	field := audiotag.PaddingField
	if !empty {
		field = audiotag.HiddenPaddingField
	}
	p.sink.Add(field.Remove(core.ByteCount(c.Size), where))
	return nil
}

func (p *parser) reportID3(c riff.Chunk) error {
	r, ok := audiotag.TagAt(p.win, c.DataOff, c.DataOff+c.Size)
	if !ok || r.Kind != audiotag.KindID3v2 {
		p.sink.Add(audiotag.OtherChunkField.Remove(fmt.Sprintf("%q, %s", c.ID, core.ByteCount(c.Size)), "RIFF"))
		return p.win.Err()
	}
	return audiotag.Describe(p.ctx, p.win, r, "ID3 chunk", p.sink)
}

// reportList decodes a LIST chunk. A LIST that does not parse is reported and
// removed like any other, since its parent's size says where it ends either way.
func (p *parser) reportList(c riff.Chunk, depth int) error {
	listType, err := riff.ListType(p.src, c)
	if err != nil {
		p.sink.Add(damagedField.Remove(core.ByteCount(c.Size), "LIST"))
		return nil
	}
	// clean prints Where, and a LIST can have any type a writer likes.
	where := "LIST"
	if listType == "INFO" || listType == "adtl" {
		where = "LIST " + listType
	}
	if depth >= maxListDepth {
		p.sink.Add(damagedField.Remove("nested too deeply, "+core.ByteCount(c.Size), where))
		return nil
	}
	if listType != "INFO" && listType != "adtl" {
		p.sink.Add(listField.Remove(fmt.Sprintf("%q, %s", listType, core.ByteCount(c.Size)), "RIFF"))
	}
	for sub, err := range riff.Children(p.ctx, p.src, c) {
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			p.sink.Add(damagedField.Remove(core.ByteCount(c.Size), where))
			return nil
		}
		if sub.ID == "LIST" {
			if err := p.reportList(sub, depth+1); err != nil {
				return err
			}
			continue
		}
		p.reportListItem(listType, sub, where)
	}
	return p.win.Err()
}

func (p *parser) reportListItem(listType string, c riff.Chunk, where string) {
	text := p.text(c.DataOff, c.Size)
	switch listType {
	case "INFO":
		if field, ok := infoFields[c.ID]; ok {
			p.sink.Add(field.Remove(text, audiotag.Join(where, c.ID)))
			return
		}
		p.sink.Add(otherInfoField.Remove(c.ID+": "+text, where))
	case "adtl":
		// labl and note hold a 4-byte cue point ID before their text.
		if (c.ID == "labl" || c.ID == "note") && c.Size >= 4 {
			text = p.text(c.DataOff+4, c.Size-4)
		}
		switch c.ID {
		case "labl", "note", "ltxt":
			p.sink.Add(labelField.Remove(text, audiotag.Join(where, c.ID)))
		default:
			p.sink.Add(labelField.Remove(c.ID+": "+text, where))
		}
	}
}

// text reads a NUL-terminated or padded text value.
func (p *parser) text(off, size int64) string {
	return decodeText(p.win.At(off, int(min(size, maxTextRead))))
}

func decodeText(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(audiotag.Latin1OrUTF8(b))
}

// reportBext decodes the Broadcast Wave chunk, whose fixed layout comes from EBU
// Tech 3285.
func (p *parser) reportBext(c riff.Chunk) {
	const (
		descriptionLen = 256
		originatorLen  = 32
		referenceLen   = 32
		dateLen        = 10
		timeLen        = 8
		timeRefLen     = 8
		versionLen     = 2
		umidLen        = 64
		loudnessLen    = 10
		reservedLen    = 180
		fixedLen       = descriptionLen + originatorLen + referenceLen + dateLen + timeLen + timeRefLen + versionLen + umidLen + loudnessLen + reservedLen
	)
	const where = "bext"
	p.sink.Add(bextField.Remove(core.ByteCount(c.Size), where))
	if c.Size < descriptionLen+originatorLen+referenceLen+dateLen+timeLen {
		return
	}
	fixed := p.win.At(c.DataOff, int(min(c.Size, fixedLen)))
	if fixed == nil {
		return
	}
	take := func(n int) []byte {
		field := fixed[:n]
		fixed = fixed[n:]
		return field
	}
	add := func(field audiotag.Field, value []byte) {
		if text := decodeText(value); text != "" {
			p.sink.Add(field.Remove(text, where))
		}
	}
	add(descField, take(descriptionLen))
	add(originatorField, take(originatorLen))
	add(bextRefField, take(referenceLen))
	date, clock := decodeText(take(dateLen)), decodeText(take(timeLen))
	if date != "" || clock != "" {
		p.sink.Add(bextDateField.Remove(date+" "+clock, where))
	}
	if len(fixed) >= timeRefLen+versionLen+umidLen {
		umid := fixed[timeRefLen+versionLen : timeRefLen+versionLen+umidLen]
		if !core.AllZero(umid) {
			p.sink.Add(bextUMIDField.Remove(fmt.Sprintf("%x", umid), where))
		}
	}
	if c.Size > fixedLen {
		if history := p.text(c.DataOff+fixedLen, c.Size-fixedLen); history != "" {
			p.sink.Add(historyField.Remove(history, where))
		}
	}
}

// reportXML reports an XML chunk, in detail if it is an XMP packet.
func (p *parser) reportXML(field audiotag.Field, c riff.Chunk, where string) {
	body := p.win.At(c.DataOff, int(min(c.Size, maxXMLRead)))
	if xmp.Contains(body) {
		for _, f := range xmp.Findings(body, where) {
			p.sink.Add(f)
		}
		return
	}
	p.sink.Add(field.Remove(fmt.Sprintf("%s: %s", core.ByteCount(c.Size), decodeText(body)), where))
}

// write emits the kept chunks in their original order. Pad bytes are written as
// zeros, not copied, because their value is unchecked and could hold anything.
func (p *parser) write(dst io.Writer) error {
	total := int64(len(formWAVE))
	for _, c := range p.kept {
		total += paddedLen(c)
	}
	if total > math.MaxUint32 {
		return fmt.Errorf("%w: padding the last chunk takes the RIFF size past 4 GiB", core.ErrTooLarge)
	}
	var head [12]byte
	copy(head[:], "RIFF")
	binary.LittleEndian.PutUint32(head[4:], uint32(total))
	copy(head[8:], formWAVE)
	if _, err := dst.Write(head[:]); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	for _, c := range p.kept {
		if _, err := dst.Write(binary.LittleEndian.AppendUint32([]byte(c.ID), uint32(c.Size))); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		if err := core.CopyRange(p.ctx, dst, p.src, c.DataOff, c.Size); err != nil {
			return err
		}
		if c.Size%2 == 1 {
			if _, err := dst.Write([]byte{0}); err != nil {
				return fmt.Errorf("write: %w", err)
			}
		}
	}
	return nil
}

func paddedLen(c riff.Chunk) int64 {
	return c.DataOff - c.Offset + c.Size + c.Size%2
}
