// Package webp removes metadata from WebP images by rebuilding the RIFF container
// from an allowlist of chunks. Bitstream chunks are copied byte for byte, so the
// decoded pixels and animation frames cannot change, and the VP8X flags and the
// RIFF size are recomputed to match what is left. Inspect and Clean run the same
// walk, which keeps them in agreement.
package webp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/riff"
	"github.com/axiom8z/hideme/internal/xmp"
)

const formatID = "webp"

// Chunk IDs. The trailing spaces are part of the format.
const (
	idVP8  = "VP8 "
	idVP8L = "VP8L"
	idVP8X = "VP8X"
	idALPH = "ALPH"
	idANIM = "ANIM"
	idANMF = "ANMF"
	idICCP = "ICCP"
	idEXIF = "EXIF"
	idXMP  = "XMP "
)

// VP8X flag bits. The rest of the byte is reserved and always written as zero.
const (
	flagICC       = 0x20
	flagAlpha     = 0x10
	flagEXIF      = 0x08
	flagAnimation = 0x02
)

const (
	vp8xSize       = 10
	animSize       = 6
	frameHeader    = 16 // position, size, duration and flags at the start of ANMF
	frameFlagsOff  = 15
	frameSizeOff   = 6    // where an ANMF header holds its width and height
	canvasOff      = 4    // where a VP8X payload holds the same
	frameFlagsMask = 0x03 // blending and disposal; the other bits are reserved
	riffHeaderSize = 12

	// maxChunks bounds the slice and the work a hostile file can demand. It allows
	// about half a million animation frames, far more than any real file has.
	maxChunks = 1 << 20
	// maxFindings bounds the report, since a chunk costs as little as 8 bytes.
	maxFindings = 4096
)

const retainICC = "needed to display colours correctly; strict mode removes it"

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "WebP image",
		Family:     core.FamilyImage,
		Extensions: []string{".webp"},
		Limitations: []string{
			"Data hidden inside the compressed image stream cannot be removed without re-encoding the picture. That includes bytes added after the end of the stream inside its chunk, which can be any length, because the stream is not decoded to find where it ends.",
			"A retained ICC colour profile is kept as it is and can name the device that made it; strict mode removes it.",
			"Values a decoder reads in animations are copied as they are: the background colour and loop count, 6 bytes, and the position, size and timing of each frame, 15 bytes a frame. Readers tolerate odd values in some of them, so hidden data could survive there.",
		},
	}
}

func (handler) Match(head []byte, _ core.Source) bool {
	return riff.Form(head) == "WEBP"
}

func (handler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	c, err := parse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	return c.rep, nil
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	c, err := parse(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	for _, part := range c.output() {
		if _, err := dst.Write(part); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
	}
	return c.rep, nil
}

// cleaner holds the state of one walk over a file.
type cleaner struct {
	src  core.Source
	data []byte
	opts core.Options
	rep  *core.Report

	parts [][]byte // kept chunks in file order; the header is built at the end

	vp8x        []byte // the original VP8X payload; nil for a simple file
	orientation []byte // the EXIF block to write in place of the originals, if any
	sawICC      bool   // a profile was kept or considered
	keptICC     bool
	sawEXIF     bool
	sawANIM     bool
	frames      int
	sawBits     bool // the still image's bitstream
	sawALPH     bool

	findings int
	overflow int
}

func parse(ctx context.Context, src core.Source, opts core.Options) (*cleaner, error) {
	root, form, err := riff.Open(src)
	if err != nil {
		return nil, err
	}
	if form != "WEBP" {
		return nil, core.Unsupportedf("webp: the RIFF form is not WEBP")
	}
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, err
	}
	c := &cleaner{src: src, data: data, opts: opts, rep: &core.Report{Format: formatID}}
	if err := c.walk(ctx, root); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cleaner) payload(ch riff.Chunk) []byte {
	return c.data[ch.DataOff : ch.DataOff+ch.Size]
}

func (c *cleaner) add(f core.Finding) {
	if c.findings >= maxFindings {
		c.overflow++
		return
	}
	c.findings++
	c.rep.Add(f)
}

func (c *cleaner) remove(category core.Category, risk core.Risk, field, value, where string) {
	c.add(core.Finding{
		Category: category,
		Risk:     risk,
		Field:    field,
		Value:    value,
		Where:    where,
		Action:   core.ActionRemove,
	})
}

func (c *cleaner) addAll(findings []core.Finding) {
	for _, f := range findings {
		c.add(f)
	}
}

func (c *cleaner) walk(ctx context.Context, root riff.Chunk) error {
	n := 0
	for ch, err := range riff.Children(ctx, c.src, root) {
		if err != nil {
			return err
		}
		if n == maxChunks {
			return fmt.Errorf("%w: webp: more than %d chunks", core.ErrTooLarge, maxChunks)
		}
		if n == 0 {
			err = c.first(ch)
		} else if c.vp8x == nil {
			c.afterSimple(ch)
		} else {
			err = c.extended(ctx, ch)
		}
		if err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return core.Malformedf("webp: the file holds no chunks")
	}
	if c.vp8x != nil {
		if err := c.checkExtended(); err != nil {
			return err
		}
	}
	if logicalEnd := root.DataOff + root.Size; logicalEnd < c.src.Size() {
		c.remove(core.CatHidden, core.RiskHigh, "Trailing data", core.ByteCount(c.src.Size()-logicalEnd), "after the RIFF chunk")
	}
	if c.overflow > 0 {
		c.rep.Add(core.Finding{
			Category: core.CatHidden,
			Risk:     core.RiskMed,
			Field:    "Further metadata",
			Value:    fmt.Sprintf("%d more items", c.overflow),
			Action:   core.ActionRemove,
		})
	}
	return nil
}

// first handles the chunk that decides the file's layout.
func (c *cleaner) first(ch riff.Chunk) error {
	switch ch.ID {
	case idVP8, idVP8L:
		return c.keepBitstream(ch)
	case idVP8X:
		if ch.Size != vp8xSize {
			return core.Malformedf("webp: VP8X chunk is %d bytes, not %d", ch.Size, vp8xSize)
		}
		c.vp8x = c.payload(ch)
		return nil
	}
	return core.Malformedf("webp: the first chunk is not an image chunk")
}

// afterSimple handles what follows the bitstream of a simple file. Decoders stop
// at the bitstream, so whatever comes next has no effect on the picture: it all
// goes, including any orientation or colour profile, which no reader would apply
// without VP8X to announce them.
func (c *cleaner) afterSimple(ch riff.Chunk) {
	p := c.payload(ch)
	switch ch.ID {
	case idEXIF:
		c.exifBlock(p, false)
	case idXMP:
		c.addAll(xmp.Findings(p, "XMP"))
	case idICCP:
		c.remove(core.CatTechnical, core.RiskNone, "ICC colour profile", core.ByteCount(len(p)), idICCP)
	default:
		c.otherChunk(ch)
	}
}

// exifBlock reports a block and, when it is the one readers honour, takes its
// replacement. Readers apply the first block, and only if VP8X announced it.
func (c *cleaner) exifBlock(p []byte, announced bool) {
	d, _ := exif.Parse(exif.StripPrefix(p)) // a damaged block reports itself through Findings
	opts := core.Options{Strict: true}
	if announced && !c.sawEXIF {
		opts = c.opts
		c.orientation = d.Replacement(c.opts)
	}
	c.sawEXIF = true
	c.addAll(d.Findings(idEXIF, opts))
}

func (c *cleaner) otherChunk(ch riff.Chunk) {
	c.remove(core.CatHidden, core.RiskMed, "Other chunk", fmt.Sprintf("%s, %s", core.SafeText(ch.ID), core.ByteCount(int(ch.Size))), "other chunk")
}

// extended handles a top-level chunk after VP8X.
func (c *cleaner) extended(ctx context.Context, ch riff.Chunk) error {
	p := c.payload(ch)
	switch ch.ID {
	case idVP8X:
		return core.Malformedf("webp: more than one VP8X chunk")
	case idICCP:
		c.iccProfile(p)
	case idEXIF:
		c.exifBlock(p, c.vp8x[0]&flagEXIF != 0)
	case idXMP:
		c.addAll(xmp.Findings(p, "XMP"))
	case idANIM:
		if c.sawANIM || c.frames > 0 || len(p) != animSize {
			return core.Malformedf("webp: ANIM chunk is misplaced or the wrong size")
		}
		c.sawANIM = true
		c.emit(idANIM, p)
	case idANMF:
		return c.frame(ctx, ch)
	case idALPH:
		if c.sawALPH || c.sawBits {
			return core.Malformedf("webp: ALPH chunk is misplaced")
		}
		c.sawALPH = true
		c.emit(idALPH, c.alphaPlane(p, c.vp8x[canvasOff:]))
	case idVP8, idVP8L:
		if c.sawBits || (c.sawALPH && ch.ID == idVP8L) {
			return core.Malformedf("webp: %q chunk is misplaced", ch.ID)
		}
		return c.keepBitstream(ch)
	default:
		c.otherChunk(ch)
	}
	return nil
}

func (c *cleaner) keepBitstream(ch riff.Chunk) error {
	p := c.payload(ch)
	if !validBitstream(ch.ID, p) {
		return core.Malformedf("webp: %q chunk does not hold a valid image bitstream", ch.ID)
	}
	c.sawBits = true
	c.emit(ch.ID, p)
	return nil
}

// validBitstream checks the fixed bytes at the start of each bitstream. Without
// this, a chunk with any contents at all could pass through as an image.
func validBitstream(id string, p []byte) bool {
	if id == idVP8L {
		const headerSize = 5
		return len(p) >= headerSize && p[0] == 0x2F
	}
	const headerSize = 10
	return len(p) >= headerSize && p[3] == 0x9D && p[4] == 0x01 && p[5] == 0x2A
}

// iccProfile keeps the first complete profile that VP8X announced. Readers skip
// anything else, so dropping it cannot change the picture.
func (c *cleaner) iccProfile(p []byte) {
	finding := core.Finding{
		Category: core.CatTechnical,
		Risk:     core.RiskNone,
		Field:    "ICC colour profile",
		Value:    core.ByteCount(len(p)),
		Where:    idICCP,
		Action:   core.ActionRemove,
	}
	switch {
	case c.sawICC:
		finding.Category, finding.Risk, finding.Field = core.CatHidden, core.RiskLow, "Extra ICC profile"
	case !validICC(p):
		finding.Category, finding.Risk, finding.Field = core.CatHidden, core.RiskMed, "Damaged ICC profile"
	case c.vp8x[0]&flagICC != 0 && !c.opts.Strict:
		finding.Action, finding.Reason = core.ActionRetain, retainICC
		c.keptICC = true
		c.emit(idICCP, p)
	}
	c.sawICC = true
	c.add(finding)
}

// alphaPlane returns an ALPH payload without whatever follows its pixels. An
// uncompressed plane has a header byte and one byte per pixel and decoders read no
// further, so the rest is no part of the picture. A compressed plane has no size to
// check against without decoding it, which is what the format's limitations cover.
// size holds the width and height, each less one, in three bytes.
func (c *cleaner) alphaPlane(p, size []byte) []byte {
	const headerSize, methodMask = 1, 0x03
	if len(p) == 0 || p[0]&methodMask != 0 {
		return p
	}
	width := uint64(size[0]) | uint64(size[1])<<8 | uint64(size[2])<<16
	height := uint64(size[3]) | uint64(size[4])<<8 | uint64(size[5])<<16
	want := (width+1)*(height+1) + headerSize
	if uint64(len(p)) <= want {
		return p
	}
	c.remove(core.CatHidden, core.RiskHigh, "Data after the alpha plane", core.ByteCount(len(p)-int(want)), idALPH)
	return p[:want]
}

func validICC(p []byte) bool {
	const (
		signatureOff = 36
		signatureEnd = 40
	)
	return len(p) >= signatureEnd &&
		string(p[signatureOff:signatureEnd]) == "acsp" &&
		int64(binary.BigEndian.Uint32(p)) == int64(len(p))
}

// frame rebuilds an animation frame, since metadata can hide inside one as well
// as next to it. The sub-chunks are walked with the same allowlist, and what
// remains gets a fresh size and zeroed reserved bits.
func (c *cleaner) frame(ctx context.Context, ch riff.Chunk) error {
	if ch.Size < frameHeader {
		return core.Malformedf("webp: ANMF chunk of %d bytes has no room for its header", ch.Size)
	}
	if !c.sawANIM {
		return core.Malformedf("webp: ANMF chunk before ANIM")
	}
	c.frames++
	header := bytes.Clone(c.data[ch.DataOff : ch.DataOff+frameHeader])
	header[frameFlagsOff] &= frameFlagsMask

	var kept [][]byte
	var sawALPH, sawBits bool
	for sub, err := range riff.Chunks(ctx, c.src, ch.DataOff+frameHeader, ch.DataOff+ch.Size) {
		if err != nil {
			return err
		}
		p := c.payload(sub)
		switch sub.ID {
		case idALPH:
			if sawALPH || sawBits {
				return core.Malformedf("webp: ALPH chunk is misplaced in an animation frame")
			}
			sawALPH = true
			kept = appendChunk(kept, idALPH, c.alphaPlane(p, header[frameSizeOff:]))
		case idVP8, idVP8L:
			if sawBits || !validBitstream(sub.ID, p) || (sawALPH && sub.ID == idVP8L) {
				return core.Malformedf("webp: %q chunk in an animation frame is misplaced or invalid", sub.ID)
			}
			sawBits = true
			kept = appendChunk(kept, sub.ID, p)
		default:
			c.otherChunk(sub)
		}
	}
	if !sawBits {
		return core.Malformedf("webp: an animation frame holds no image")
	}
	size := frameHeader
	for _, part := range kept {
		size += len(part)
	}
	c.parts = append(c.parts, chunkHeaderFor(idANMF, size), header)
	c.parts = append(c.parts, kept...)
	return nil
}

// checkExtended enforces the layout the WebP specification requires once all
// chunks are seen. A file that breaks it may decode differently from one reader
// to the next, so there is no output HideMe could vouch for.
func (c *cleaner) checkExtended() error {
	animated := c.vp8x[0]&flagAnimation != 0
	switch {
	case animated && (!c.sawANIM || c.frames == 0):
		return core.Malformedf("webp: VP8X announces an animation but the file has no frames")
	case !animated && (c.sawANIM || c.frames > 0):
		return core.Malformedf("webp: the file has animation chunks that VP8X does not announce")
	case animated && (c.sawBits || c.sawALPH):
		return core.Malformedf("webp: an animated file also has a still image")
	case !animated && !c.sawBits:
		return core.Malformedf("webp: the file holds no image")
	}
	return nil
}

// emit adds a whole top-level chunk.
func (c *cleaner) emit(id string, p []byte) {
	c.parts = appendChunk(c.parts, id, p)
}

// appendChunk adds a chunk's header, payload and pad byte to parts. The pad is
// always written as zero, because internal/riff leaves its value unchecked and
// it would otherwise be one byte of data surviving cleaning.
func appendChunk(parts [][]byte, id string, p []byte) [][]byte {
	parts = append(parts, chunkHeaderFor(id, len(p)), p)
	if len(p)%2 == 1 {
		parts = append(parts, []byte{0})
	}
	return parts
}

func chunkHeaderFor(id string, size int) []byte {
	return binary.LittleEndian.AppendUint32([]byte(id), uint32(size))
}

// output returns the cleaned file, with the VP8X flags recomputed from what is
// actually left. The alpha flag is copied because this handler removes nothing
// that bears on transparency and cannot judge it without decoding the bitstreams.
func (c *cleaner) output() [][]byte {
	body := c.parts
	if c.orientation != nil {
		body = appendChunk(body, idEXIF, c.orientation)
	}
	size := 0
	var vp8x []byte
	if c.vp8x != nil {
		flags := c.vp8x[0] & (flagAlpha | flagAnimation)
		if c.keptICC {
			flags |= flagICC
		}
		if c.orientation != nil {
			flags |= flagEXIF
		}
		payload := make([]byte, 0, vp8xSize)
		payload = append(payload, flags, 0, 0, 0)
		payload = append(payload, c.vp8x[4:vp8xSize]...)
		vp8x = append(chunkHeaderFor(idVP8X, vp8xSize), payload...)
		size += len(vp8x)
	}
	for _, part := range body {
		size += len(part)
	}
	header := make([]byte, 0, riffHeaderSize)
	header = append(header, "RIFF"...)
	header = binary.LittleEndian.AppendUint32(header, uint32(size+4))
	header = append(header, "WEBP"...)
	out := make([][]byte, 0, len(body)+2)
	out = append(out, header)
	if vp8x != nil {
		out = append(out, vp8x)
	}
	return append(out, body...)
}
