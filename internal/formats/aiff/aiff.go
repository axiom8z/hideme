// Package aiff cleans AIFF and AIFF-C files by rebuilding the FORM container from
// its COMM, SSND and FVER chunks. Everything else is optional and goes, whether or
// not it is recognised here.
package aiff

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
)

const (
	formatID = "aiff"

	chunkHeaderLen = 8
	formHeaderLen  = 12

	idCOMM = "COMM"
	idSSND = "SSND"
	idFVER = "FVER"
	idCHAN = "CHAN"

	minCommLen  = 18
	minCommCLen = 22
	fverLen     = 4
	// chanLen is a channel layout with no channel descriptions, which is three
	// numbers. The descriptions that can follow them are not needed to place channels
	// in any standard layout, and have a free-form part.
	chanLen = 12
	// emptyNameLen is what a compression name of nothing takes: its length byte and
	// the pad that brings it to an even length.
	emptyNameLen = 2
	// ssndHeaderLen is the sound chunk's offset and block size fields.
	ssndHeaderLen = 8
	// commFramesOff locates numSampleFrames in COMM, after numChannels, and
	// commBitsOff the sample size after that.
	commFramesOff = 2
	commBitsOff   = 6

	// maxTextRead bounds what is read of one text value for the report.
	maxTextRead = 64 << 10
)

// aifcVersion1 is the one value the FVER chunk can hold, the date of the AIFF-C
// specification.
var aifcVersion1 = []byte{0xA2, 0x80, 0x51, 0x40}

// linearTypes are the AIFF-C compression types with a fixed size per sample, for which
// the sound chunk's size can be checked against the frame count.
var linearTypes = map[string]bool{
	"NONE": true, "twos": true, "sowt": true, "raw ": true, "in24": true,
	"in32": true, "fl32": true, "fl64": true, "FL32": true, "FL64": true,
}

var (
	compressionField = audiotag.Field{Name: "Compression name", Cat: core.CatContent, Risk: core.RiskLow}
	annotationField  = audiotag.Field{Name: "Annotation", Cat: core.CatContent, Risk: core.RiskMed}
	authorField      = audiotag.Field{Name: "Author", Cat: core.CatIdentity, Risk: core.RiskHigh}
	markerField      = audiotag.Field{Name: "Marker", Cat: core.CatContent, Risk: core.RiskLow}
	instrumentField  = audiotag.Field{Name: "Instrument data", Cat: core.CatTechnical, Risk: core.RiskLow}
	applField        = audiotag.Field{Name: "Application data", Cat: core.CatHidden, Risk: core.RiskMed}
	offsetField      = audiotag.Field{Name: "Sound data offset padding", Cat: core.CatHidden, Risk: core.RiskNone}
	hiddenField      = audiotag.Field{Name: "Data hidden in sound data offset", Cat: core.CatHidden, Risk: core.RiskMed}
)

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "AIFF audio",
		Family:     core.FamilyAudio,
		Extensions: []string{".aiff", ".aif", ".aifc"},
		Limitations: []string{
			"Sound bytes past the declared number of frames stay, because decoders such as ffmpeg play them. A warning says when there are any, for uncompressed sound. For compressed sound they cannot be told from audio.",
			"A file whose sound chunk is empty with unknown bytes after it is refused: its writer probably never filled in its sizes, and those bytes are the audio.",
			"The frame count and the low bytes of the sample rate in the COMM chunk are copied as they are, and readers go by the sound data and round the rate, so up to 12 bytes could survive there.",
		},
	}
}

func (handler) Match(head []byte, _ core.Source) bool {
	if len(head) < formHeaderLen || string(head[:4]) != "FORM" {
		return false
	}
	form := string(head[8:12])
	return form == "AIFF" || form == "AIFC"
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	return process(ctx, src, nil)
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	return process(ctx, src, dst)
}

// chunk describes one chunk of the FORM.
type chunk struct {
	id      string
	offset  int64
	dataOff int64
	size    int64
}

// process walks the chunks and, when dst is not nil, writes the cleaned copy.
func process(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
	win := audiotag.NewWindow(src)
	head := win.At(0, formHeaderLen)
	if head == nil {
		if err := win.Err(); err != nil {
			return nil, err
		}
		return nil, core.Malformedf("aiff: %d bytes is too short for a FORM header", src.Size())
	}
	form := string(head[8:12])
	if string(head[:4]) != "FORM" || (form != "AIFF" && form != "AIFC") {
		return nil, core.Unsupportedf("aiff: not a FORM AIFF or AIFC file")
	}
	formSize := int64(binary.BigEndian.Uint32(head[4:]))
	if formSize < 4 || formSize > src.Size()-chunkHeaderLen {
		return nil, core.Malformedf("aiff: FORM declares %d bytes in a %d-byte file", formSize, src.Size())
	}
	formEnd := chunkHeaderLen + formSize

	p := &parser{ctx: ctx, src: src, win: win, form: form, sink: audiotag.NewSink(formatID)}
	if err := p.walk(formEnd); err != nil {
		return nil, err
	}
	if err := p.checkRequired(); err != nil {
		return nil, err
	}
	p.warnAboutExtraSound()
	if formEnd < src.Size() {
		_, unknown, err := audiotag.ScanTrailing(ctx, win, formEnd, "after the FORM chunk", p.sink)
		if err != nil {
			return nil, err
		}
		if unknown > 0 && p.ssnd != nil && p.soundLen() == 0 {
			return nil, core.Malformedf("aiff: the sound chunk is empty but %d unknown bytes follow the FORM, as if its writer never filled in its sizes", unknown)
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
	form string
	sink *audiotag.Sink

	kept     []chunk // in file order
	comm     *chunk
	ssnd     *chunk
	fver     *chunk
	layout   *chunk
	ssndSkip int64 // bytes of offset padding at the start of the sound data
}

func (p *parser) walk(end int64) error {
	for pos := int64(formHeaderLen); pos < end; {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if end-pos < chunkHeaderLen {
			return core.Malformedf("aiff: %d stray bytes at offset %d are too few for a chunk header", end-pos, pos)
		}
		hdr := p.win.At(pos, chunkHeaderLen)
		if hdr == nil {
			return p.win.Err()
		}
		c := chunk{id: string(hdr[:4]), offset: pos, dataOff: pos + chunkHeaderLen, size: int64(binary.BigEndian.Uint32(hdr[4:]))}
		if c.size > end-c.dataOff {
			return core.Malformedf("aiff: chunk %q at offset %d declares %d bytes, past the end of the FORM", c.id, pos, c.size)
		}
		if err := p.chunk(c); err != nil {
			return err
		}
		// Chunks are padded to an even length, but writers often omit the final
		// pad, so a chunk that ends exactly at the end of the FORM needs none.
		pos = min(c.dataOff+c.size+c.size%2, end)
	}
	return p.win.Err()
}

func (p *parser) chunk(c chunk) error {
	switch c.id {
	case idCOMM:
		return p.keepCOMM(c)
	case idSSND:
		return p.keepSSND(c)
	case idFVER:
		if p.fver != nil {
			return core.Malformedf("aiff: more than one FVER chunk")
		}
		if c.size != fverLen {
			return core.Malformedf("aiff: FVER chunk of %d bytes", c.size)
		}
		if !bytes.Equal(p.win.At(c.dataOff, fverLen), aifcVersion1) {
			p.sink.Add(audiotag.HiddenPaddingField.Remove(core.ByteCount(fverLen), idFVER))
		}
		p.fver = &c
		p.kept = append(p.kept, c)
		return nil
	case idCHAN:
		if p.layout == nil && c.size == chanLen {
			if body := p.win.At(c.dataOff, chanLen); body != nil && binary.BigEndian.Uint32(body[8:]) == 0 {
				p.layout = &c
				p.kept = append(p.kept, c)
				return nil
			}
		}
	}
	return p.report(c)
}

func (p *parser) keepCOMM(c chunk) error {
	if p.comm != nil {
		return core.Malformedf("aiff: more than one COMM chunk")
	}
	minLen := int64(minCommLen)
	if p.form == "AIFC" {
		minLen = minCommCLen
	}
	if c.size < minLen {
		return core.Malformedf("aiff: COMM chunk of %d bytes", c.size)
	}
	p.comm = &c
	p.kept = append(p.kept, c)
	return p.reportCOMMExtras(c, minLen)
}

// reportCOMMExtras reports what follows the fields decoders read, none of which is
// written back. That is the compression name, free text that decoders go by the
// compression type instead of, and whatever comes after it, which nothing reads.
func (p *parser) reportCOMMExtras(c chunk, fields int64) error {
	spareOff, spare := c.dataOff+fields, c.size-fields
	if p.form == "AIFC" && spare > 0 {
		name := p.win.At(spareOff, 1)
		if name == nil {
			return p.win.Err()
		}
		n := int64(name[0])
		if text := p.text(spareOff+1, min(n, spare-1)); text != "" {
			p.sink.Add(compressionField.Remove(text, idCOMM))
		}
		used := min(n+1+(n+1)%2, spare)
		spareOff, spare = spareOff+used, spare-used
	}
	if spare == 0 {
		return nil
	}
	empty, err := audiotag.AllZero(p.ctx, p.win, spareOff, spare)
	if err != nil {
		return err
	}
	field := audiotag.PaddingField
	if !empty {
		field = audiotag.HiddenPaddingField
	}
	p.sink.Add(field.Remove(core.ByteCount(spare), idCOMM))
	return nil
}

// keepSSND works out how much of the sound chunk is padding. Its offset field
// exists to align sound data to disk blocks, which nothing needs any more, and the
// bytes it skips are not audio. They are removed like any other padding, leaving
// the offset zero.
func (p *parser) keepSSND(c chunk) error {
	if p.ssnd != nil {
		return core.Malformedf("aiff: more than one SSND chunk")
	}
	if c.size < ssndHeaderLen {
		return core.Malformedf("aiff: SSND chunk of %d bytes", c.size)
	}
	hdr := p.win.At(c.dataOff, ssndHeaderLen)
	if hdr == nil {
		return p.win.Err()
	}
	skip := int64(binary.BigEndian.Uint32(hdr))
	if skip > c.size-ssndHeaderLen {
		return core.Malformedf("aiff: SSND offset %d is past the end of a %d-byte chunk", skip, c.size)
	}
	if skip > 0 {
		empty, err := audiotag.AllZero(p.ctx, p.win, c.dataOff+ssndHeaderLen, skip)
		if err != nil {
			return err
		}
		field := offsetField
		if !empty {
			field = hiddenField
		}
		p.sink.Add(field.Remove(core.ByteCount(skip), idSSND))
	}
	p.ssnd = &c
	p.ssndSkip = skip
	p.kept = append(p.kept, c)
	return nil
}

// checkRequired enforces the structure the format demands. The sound chunk may
// only be missing if COMM says there are no sample frames.
func (p *parser) checkRequired() error {
	if p.comm == nil {
		return core.Malformedf("aiff: no COMM chunk")
	}
	if p.ssnd != nil {
		return nil
	}
	body := p.win.At(p.comm.dataOff, commFramesOff+4)
	if body == nil {
		return p.win.Err()
	}
	if frames := binary.BigEndian.Uint32(body[commFramesOff:]); frames != 0 {
		return core.Malformedf("aiff: COMM declares %d sample frames but there is no SSND chunk", frames)
	}
	return nil
}

// soundLen returns how many bytes of sound the sound chunk holds.
func (p *parser) soundLen() int64 {
	return p.ssnd.size - ssndHeaderLen - p.ssndSkip
}

// warnAboutExtraSound says when the sound chunk holds more than COMM's frame count
// accounts for. Those bytes stay, as decoders such as ffmpeg play them, but nothing
// else vouches for them. One spare byte is a writer counting its pad in the size.
func (p *parser) warnAboutExtraSound() {
	if p.comm == nil || p.ssnd == nil {
		return
	}
	body := p.win.At(p.comm.dataOff, int(min(p.comm.size, minCommCLen)))
	if len(body) < minCommLen || (p.form == "AIFC" && !linearTypes[string(body[minCommLen:])]) {
		return
	}
	channels := int64(binary.BigEndian.Uint16(body))
	frames := int64(binary.BigEndian.Uint32(body[commFramesOff:]))
	sampleBytes := (int64(binary.BigEndian.Uint16(body[commBitsOff:])) + 7) / 8
	if extra := p.soundLen() - frames*channels*sampleBytes; extra > 1 {
		p.sink.Warn("the sound chunk holds %d bytes past the sample frames the file declares. They were kept, because some players decode them, and they could hold anything", extra)
	}
}

// report adds findings for a chunk that is being dropped.
func (p *parser) report(c chunk) error {
	id := audiotag.FixedText([]byte(c.id))
	switch c.id {
	case "NAME":
		p.sink.Add(audiotag.TitleField.Remove(p.text(c.dataOff, c.size), id))
	case "AUTH":
		p.sink.Add(authorField.Remove(p.text(c.dataOff, c.size), id))
	case "(c) ":
		p.sink.Add(audiotag.CopyrightField.Remove(p.text(c.dataOff, c.size), id))
	case "ANNO":
		p.sink.Add(annotationField.Remove(p.text(c.dataOff, c.size), id))
	case "COMT":
		p.reportComments(c, id)
	case "MARK":
		p.reportMarkers(c, id)
	case "INST":
		p.sink.Add(instrumentField.Remove(core.ByteCount(c.size), id))
	case "APPL":
		signature := audiotag.FixedText(p.win.At(c.dataOff, int(min(c.size, 4))))
		p.sink.Add(applField.Remove(fmt.Sprintf("%s, %s", signature, core.ByteCount(c.size)), id))
	case "ID3 ", "id3 ":
		return p.reportID3(c)
	default:
		p.sink.Add(audiotag.OtherChunkField.Remove(fmt.Sprintf("%q, %s", c.id, core.ByteCount(c.size)), "FORM"))
	}
	return p.win.Err()
}

func (p *parser) reportID3(c chunk) error {
	r, ok := audiotag.TagAt(p.win, c.dataOff, c.dataOff+c.size)
	if !ok || r.Kind != audiotag.KindID3v2 {
		p.sink.Add(audiotag.OtherChunkField.Remove(fmt.Sprintf("%q, %s", c.id, core.ByteCount(c.size)), "FORM"))
		return p.win.Err()
	}
	return audiotag.Describe(p.ctx, p.win, r, "ID3 chunk", p.sink)
}

func (p *parser) text(off, size int64) string {
	return audiotag.FixedText(p.win.At(off, int(min(size, maxTextRead))))
}

// reportComments decodes a COMT chunk: a count, then for each comment a timestamp,
// a marker ID, a length and text padded to an even length.
func (p *parser) reportComments(c chunk, where string) {
	body := p.win.At(c.dataOff, int(min(c.size, maxTextRead)))
	if len(body) < 2 {
		p.sink.Add(annotationField.Remove(core.ByteCount(c.size), where))
		return
	}
	count := int(binary.BigEndian.Uint16(body))
	body = body[2:]
	decoded := false
	for range count {
		if len(body) < 8 {
			break
		}
		n := int(binary.BigEndian.Uint16(body[6:]))
		body = body[8:]
		if n > len(body) {
			n = len(body)
		}
		p.sink.Add(audiotag.CommentField.Remove(audiotag.FixedText(body[:n]), where))
		body = body[min(n+n%2, len(body)):]
		decoded = true
	}
	if !decoded {
		p.sink.Add(annotationField.Remove(core.ByteCount(c.size), where))
	}
}

// reportMarkers decodes a MARK chunk: a count, then for each marker an ID, a
// position and a Pascal string padded to an even total length.
func (p *parser) reportMarkers(c chunk, where string) {
	body := p.win.At(c.dataOff, int(min(c.size, maxTextRead)))
	if len(body) < 2 {
		p.sink.Add(markerField.Remove(core.ByteCount(c.size), where))
		return
	}
	count := int(binary.BigEndian.Uint16(body))
	body = body[2:]
	decoded := false
	for range count {
		if len(body) < 7 {
			break
		}
		n := int(body[6])
		body = body[7:]
		if n > len(body) {
			n = len(body)
		}
		p.sink.Add(markerField.Remove(audiotag.FixedText(body[:n]), where))
		body = body[min(n+(n+1)%2, len(body)):]
		decoded = true
	}
	if !decoded {
		p.sink.Add(markerField.Remove(core.ByteCount(c.size), where))
	}
}

// write emits the kept chunks in their original order. Pad bytes are written as
// zeros, not copied, because their value is unchecked and could hold anything.
func (p *parser) write(dst io.Writer) error {
	total := int64(4)
	for _, c := range p.kept {
		size := p.outputSize(c)
		total += chunkHeaderLen + size + size%2
	}
	if total > math.MaxUint32 {
		return fmt.Errorf("%w: padding the last chunk takes the FORM size past 4 GiB", core.ErrTooLarge)
	}
	head := make([]byte, 0, formHeaderLen)
	head = append(head, "FORM"...)
	head = binary.BigEndian.AppendUint32(head, uint32(total))
	head = append(head, p.form...)
	if _, err := dst.Write(head); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	for _, c := range p.kept {
		if err := p.writeChunk(dst, c); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) outputSize(c chunk) int64 {
	switch c.id {
	case idSSND:
		return c.size - p.ssndSkip
	case idCOMM:
		if p.form == "AIFC" {
			return minCommCLen + emptyNameLen
		}
		return minCommLen
	}
	return c.size
}

func (p *parser) writeChunk(dst io.Writer, c chunk) error {
	size := p.outputSize(c)
	hdr := make([]byte, 0, chunkHeaderLen+ssndHeaderLen)
	hdr = append(hdr, c.id...)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(size))
	bodyOff, bodyLen := c.dataOff, c.size
	var after []byte
	switch c.id {
	case idSSND:
		// The offset and block size only describe alignment to disk blocks. With the
		// padding gone there is none, so both are zero, whatever they held before.
		hdr = append(hdr, make([]byte, ssndHeaderLen)...)
		bodyOff = c.dataOff + ssndHeaderLen + p.ssndSkip
		bodyLen = p.soundLen()
	case idCOMM:
		bodyLen = min(size, minCommCLen)
		if p.form == "AIFC" {
			after = make([]byte, emptyNameLen)
		}
	case idFVER:
		bodyLen = 0
		after = aifcVersion1
	}
	if _, err := dst.Write(hdr); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := core.CopyRange(p.ctx, dst, p.src, bodyOff, bodyLen); err != nil {
		return err
	}
	if _, err := dst.Write(after); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if size%2 == 1 {
		if _, err := dst.Write([]byte{0}); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	return nil
}
