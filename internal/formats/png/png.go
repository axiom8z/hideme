// Package png removes metadata from PNG and APNG images by rewriting the chunk
// list. Only chunks a decoder uses are kept, and kept chunks are copied byte for
// byte, so the decoded pixels and animation frames cannot change. The one thing cut
// from a kept chunk is what a decoder never reads: palette entries beyond the bit
// depth and bytes after the end of the compressed image. Every chunk's CRC is
// checked first, because a file with a wrong CRC is not one this handler can vouch
// for. Inspect and Clean run the same walk, which keeps them in agreement.
package png

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/exif"
)

const formatID = "png"

const (
	chunkHeaderSize = 8 // length and type
	chunkCRCSize    = 4
	maxChunkLength  = 1<<31 - 1 // the PNG specification's own limit

	// maxChunks bounds the slice and the work a hostile file can demand. Writers
	// split IDAT at 8 KiB or more, so a real file under core.MaxInMemory stays
	// far below this.
	maxChunks = 1 << 20
	// maxFindings bounds the report. A text chunk costs as little as 13 bytes, so
	// a hostile file could otherwise spend far more memory on findings than the
	// file itself takes up.
	maxFindings = 4096
)

var signature = []byte("\x89PNG\r\n\x1a\n")

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "PNG image",
		Family:     core.FamilyImage,
		Extensions: []string{".png", ".apng"},
		Limitations: []string{
			"Data hidden inside the compressed image stream cannot be removed without re-encoding the picture.",
			"A retained ICC colour profile is kept as it is and can name the device that made it; strict mode removes it.",
			"Values in the chunks a decoder needs (palette, transparency, gamma, colour and HDR settings, physical pixel size, frame timing and loop count) are copied as they are. Palette and transparency entries that no pixel uses can hold up to 1 KiB of hidden data, and the others a few bytes each.",
		},
	}
}

func (handler) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, signature)
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

func parse(ctx context.Context, src core.Source, opts core.Options) (*cleaner, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, signature) {
		return nil, core.Unsupportedf("png: the file does not start with the PNG signature")
	}
	c := &cleaner{data: data, opts: opts, rep: &core.Report{Format: formatID}, seen: make(map[string]bool), imageBudget: maxImageBytes}
	if err := c.walk(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// cleaner holds the state of one walk over a file.
type cleaner struct {
	data []byte
	opts core.Options
	rep  *core.Report

	parts     [][]byte // kept chunks, in file order
	firstIDAT int      // index into parts of the first IDAT

	header   imageHeader
	palette  int // entries in PLTE
	sawPLTE  bool
	sawIDAT  bool
	sawExif  bool
	sawICC   bool
	exifKeep []byte          // the EXIF block to write in place of the originals, if any
	seen     map[string]bool // chunks that may appear once and have been kept

	transparencyData []byte // the kept tRNS payload, if any

	previous string // the type of the chunk before this one

	streams []*imageStream // every run of image data, in file order
	image   *imageStream   // the main image's stream
	frame   *imageStream   // the stream of the animation frame being read

	animated       bool // an acTL came before the image data
	animationPart  int  // index into parts of the acTL
	declaredFrames uint32
	frames         uint32
	nextSequence   uint32

	imageZlib     io.ReadCloser // reused for each stream of image data
	inflateBuffer []byte

	inflated    int64 // bytes of text and profiles decompressed so far
	imageBudget int64 // bytes of image data that may still be decompressed
	overflow    int   // findings past maxFindings
}

// chunk is one chunk of the file, located by walk.
type chunk struct {
	typ  string
	data []byte // payload
	raw  []byte // the whole chunk, header and CRC included
}

// add records a finding, or counts it when the report is already full.
func (c *cleaner) add(f core.Finding) {
	if len(c.rep.Findings) >= maxFindings {
		c.overflow++
		return
	}
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

func (c *cleaner) walk(ctx context.Context) error {
	pos := len(signature)
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == maxChunks {
			return fmt.Errorf("%w: png: more than %d chunks", core.ErrTooLarge, maxChunks)
		}
		ch, next, err := c.readChunk(pos)
		if err != nil {
			return err
		}
		if n == 0 {
			if err := c.firstChunk(ch); err != nil {
				return err
			}
			pos = next
			continue
		}
		if ch.typ == "IEND" {
			return c.finish(ctx, ch, next)
		}
		if err := c.handle(ch); err != nil {
			return err
		}
		c.previous = ch.typ
		pos = next
	}
}

// readChunk reads and checks the chunk at pos. The type must be four ASCII
// letters, which the PNG specification requires and which a decoder relies on to
// tell critical chunks from ancillary ones.
func (c *cleaner) readChunk(pos int) (chunk, int, error) {
	if len(c.data)-pos < chunkHeaderSize+chunkCRCSize {
		return chunk{}, 0, core.Malformedf("png: the file ends at offset %d with no IEND chunk", pos)
	}
	length := binary.BigEndian.Uint32(c.data[pos:])
	typ := c.data[pos+4 : pos+chunkHeaderSize]
	if length > maxChunkLength || uint64(length) > uint64(len(c.data)-pos-chunkHeaderSize-chunkCRCSize) {
		return chunk{}, 0, core.Malformedf("png: chunk at offset %d declares %d bytes, which overruns the file", pos, length)
	}
	for _, b := range typ {
		if (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') {
			return chunk{}, 0, core.Malformedf("png: chunk at offset %d has an invalid type", pos)
		}
	}
	bodyEnd := pos + chunkHeaderSize + int(length)
	end := bodyEnd + chunkCRCSize
	if got, want := crc32.ChecksumIEEE(c.data[pos+4:bodyEnd]), binary.BigEndian.Uint32(c.data[bodyEnd:]); got != want {
		return chunk{}, 0, core.Malformedf("png: chunk at offset %d has a bad CRC", pos)
	}
	return chunk{typ: string(typ), data: c.data[pos+chunkHeaderSize : bodyEnd], raw: c.data[pos:end]}, end, nil
}

func (c *cleaner) firstChunk(ch chunk) error {
	switch ch.typ {
	case "IHDR":
	case "CgBI":
		return core.Unsupportedf("png: Apple's CgBI variant is not standard PNG")
	default:
		return core.Malformedf("png: the first chunk is not IHDR")
	}
	header, err := parseHeader(ch.data)
	if err != nil {
		return err
	}
	c.header = header
	c.parts = append(c.parts, ch.raw)
	return nil
}

func (c *cleaner) handle(ch chunk) error {
	switch ch.typ {
	case "IHDR":
		return core.Malformedf("png: more than one IHDR chunk")
	case "PLTE":
		return c.plte(ch)
	case "IDAT":
		c.imageData(ch)
	case "tRNS":
		return c.transparency(ch)
	case "gAMA", "cHRM", "sRGB", "sBIT", "bKGD", "pHYs", "cICP", "mDCV", "cLLI":
		c.ancillary(ch)
	case "acTL":
		c.animationControl(ch)
	case "fcTL":
		return c.frameControl(ch)
	case "fdAT":
		return c.frameData(ch)
	case "iCCP":
		return c.iccp(ch)
	case "tEXt", "zTXt", "iTXt":
		return c.text(ch)
	case "eXIf":
		c.exifChunk(ch.data)
	case "tIME":
		c.remove(core.CatTime, core.RiskMed, "Modification time", decodeTime(ch.data), "tIME")
	case "caBX":
		c.remove(core.CatIdentity, core.RiskHigh, "C2PA content credentials", core.ByteCount(len(ch.data)), "caBX")
	default:
		// A decoder may not skip a critical chunk it does not know, so dropping
		// one could change what the file decodes to. Refuse instead of guessing.
		if ch.typ[0]&0x20 == 0 {
			return core.Unsupportedf("png: the file has a critical chunk of an unknown type")
		}
		// That includes hIST, which only ever helped old displays choose colours.
		c.remove(core.CatHidden, core.RiskMed, "Other chunk", fmt.Sprintf("%s, %s", ch.typ, core.ByteCount(len(ch.data))), "other chunk")
	}
	return nil
}

// imageData collects the main image's stream. Only one unbroken run of IDAT counts:
// decoders read the run and stop, so an IDAT further on is not image data at all,
// whatever its name says.
func (c *cleaner) imageData(ch chunk) {
	switch {
	case !c.sawIDAT:
		c.sawIDAT = true
		c.firstIDAT = len(c.parts)
		c.image = &imageStream{width: c.header.width, height: c.header.height, where: "IDAT"}
		c.streams = append(c.streams, c.image)
	case c.previous != "IDAT":
		c.remove(core.CatHidden, core.RiskHigh, "Stray image data", core.ByteCount(len(ch.data)), "IDAT")
		return
	}
	c.image.pieces = append(c.image.pieces, streamPiece{part: len(c.parts), typ: ch.typ, data: ch.data})
	c.parts = append(c.parts, ch.raw)
}

// exifChunk reports a block and, for the first one, takes its replacement. That
// is the block readers honour, so no later block may change how the picture is
// shown.
func (c *cleaner) exifChunk(block []byte) {
	d, _ := exif.Parse(exif.StripPrefix(block)) // a damaged block reports itself through Findings
	opts := core.Options{Strict: true}
	if !c.sawExif {
		opts = c.opts
		c.exifKeep = d.Replacement(c.opts)
	}
	c.sawExif = true
	c.addAll(d.Findings("eXIf", opts))
}

// finish handles IEND and reports what was left behind.
func (c *cleaner) finish(ctx context.Context, iend chunk, next int) error {
	if len(iend.data) != 0 {
		return core.Malformedf("png: IEND chunk holds %d bytes", len(iend.data))
	}
	if !c.sawIDAT {
		return core.Malformedf("png: the file has no IDAT chunk")
	}
	if c.header.colorType == colorPalette && !c.sawPLTE {
		return core.Malformedf("png: a palette image has no PLTE chunk")
	}
	if err := c.finishFrame(); err != nil {
		return err
	}
	c.dropEmptyAnimation()
	for _, s := range c.streams {
		if err := c.checkStream(ctx, s); err != nil {
			return err
		}
	}
	c.parts = append(c.parts, iend.raw)
	if trailing := len(c.data) - next; trailing > 0 {
		c.remove(core.CatHidden, core.RiskHigh, "Trailing data", core.ByteCount(trailing), "after IEND")
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

// output returns the cleaned file. The replacement EXIF chunk goes right before
// the first IDAT, wherever the original was.
func (c *cleaner) output() [][]byte {
	out := append([][]byte{signature}, c.parts...)
	if c.exifKeep != nil {
		out = slices.Insert(out, 1+c.firstIDAT, buildChunk("eXIf", c.exifKeep))
	}
	return out
}

// buildChunk makes a complete chunk with a correct CRC.
func buildChunk(typ string, payload []byte) []byte {
	out := make([]byte, 0, chunkHeaderSize+len(payload)+chunkCRCSize)
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, typ...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

func decodeTime(b []byte) string {
	const size = 7
	if len(b) != size {
		return core.ByteCount(len(b)) + ", not a valid time"
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d UTC", binary.BigEndian.Uint16(b), b[2], b[3], b[4], b[5], b[6])
}
