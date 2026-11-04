// Package gif removes metadata from GIF images by parsing the whole block
// structure and copying only the blocks a decoder needs. Image data is copied
// byte for byte up to the end of its compressed stream, so every frame decodes as
// before. Comments and application data are dropped, except the two application
// extensions that say how an animation loops. Inspect and Clean run the same walk,
// which keeps them in agreement.
package gif

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/xmp"
)

const formatID = "gif"

// Block introducers and extension labels.
const (
	introImage     = 0x2C
	introExtension = 0x21
	introTrailer   = 0x3B

	labelGraphicControl = 0xF9
	labelPlainText      = 0x01
	labelComment        = 0xFE
	labelApplication    = 0xFF
)

const (
	headerSize      = 6
	screenSize      = 7 // the logical screen descriptor
	descriptorSize  = 10
	graphicSize     = 8  // introducer, label, size, 4 bytes of data, terminator
	plainTextHeader = 12 // the fixed block every Plain Text extension starts with
	applicationID   = 11 // application identifier and authentication code
	maxLZWWidth     = 11 // codes top out at 12 bits, and the width is one less

	// maxBlocks bounds the slice and the work a hostile file can demand. It allows
	// half a million animation frames, far more than any real GIF has.
	maxBlocks = 1 << 20
	// maxFindings bounds the report, since a comment costs as little as 3 bytes.
	maxFindings = 4096
	// ctxCheckInterval is how many sub-blocks are walked between context checks.
	ctxCheckInterval = 1 << 16
)

// errCutOff says that the file ends inside a block. Truncated GIFs are common on the
// web and browsers show what arrived, so this does not fail the file.
var errCutOff = errors.New("gif: the file ends inside a block")

// xmpMagic is the trailer the XMP specification puts after the packet. Reading
// the raw packet as sub-blocks lands somewhere in this descending run, and the run
// then steers that reader onto the final zero, so a decoder that knows nothing of
// XMP skips the whole extension.
var xmpMagic = func() []byte {
	magic := []byte{0x01}
	for v := 0xFF; v >= 0; v-- {
		magic = append(magic, byte(v))
	}
	return append(magic, 0x00)
}()

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "GIF image",
		Family:     core.FamilyImage,
		Extensions: []string{".gif"},
		Limitations: []string{
			"Data hidden inside the compressed image stream cannot be removed without re-encoding the picture.",
			"Text in Plain Text extensions is part of the picture and is kept, and a file holding any gets a warning.",
			"Values in the blocks a decoder needs (colour tables, frame timing, loop counts) are copied as they are. Colours that no pixel uses can hold up to 768 bytes of hidden data per table, and the others a few bytes each.",
			"A colour profile in an application extension is removed, because GIF has no standard place for one.",
		},
	}
}

func (handler) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a"))
}

func (h handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	c, err := h.parse(ctx, src)
	if err != nil {
		return nil, err
	}
	return c.rep, nil
}

func (h handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	c, err := h.parse(ctx, src)
	if err != nil {
		return nil, err
	}
	for _, part := range c.parts {
		if _, err := dst.Write(part); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
	}
	return c.rep, nil
}

func (h handler) parse(ctx context.Context, src core.Source) (*cleaner, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if !h.Match(data, nil) {
		return nil, core.Unsupportedf("gif: the file does not start with a GIF signature")
	}
	c := &cleaner{data: data, rep: &core.Report{Format: formatID}}
	if err := c.walk(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// cleaner holds the state of one walk over a file.
type cleaner struct {
	data     []byte
	rep      *core.Report
	parts    [][]byte // kept blocks, in file order
	images   int
	overflow int // findings past maxFindings

	lzw            lzwWalker
	pendingControl int  // index into parts plus one of a graphic control with no frame yet
	stray          int  // zero bytes found where a block belongs
	sawPlainText   bool // the warning about them has been given
	cutOff         bool // the file ended before its trailer

	// noXMPMagic records that no magic trailer exists from some earlier offset to
	// the end of the file, so that a hostile file with many unterminated XMP
	// extensions cannot make each of them rescan the rest of the file.
	noXMPMagic bool
}

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

func (c *cleaner) walk(ctx context.Context) error {
	pos, err := c.header()
	if err != nil {
		return err
	}
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == maxBlocks {
			return fmt.Errorf("%w: gif: more than %d blocks", core.ErrTooLarge, maxBlocks)
		}
		if pos >= len(c.data) {
			c.cutOff = true
			return c.finish(0)
		}
		var next int
		switch intro := c.data[pos]; intro {
		case introImage:
			next, err = c.image(ctx, pos)
		case introExtension:
			next, err = c.extension(ctx, pos)
		case introTrailer:
			return c.finish(len(c.data) - pos - 1)
		case 0: // an extra terminator, which is the usual kind of stray byte
			c.stray++
			next = pos + 1
		default:
			return c.unknownBlock(intro, pos)
		}
		if errors.Is(err, errCutOff) {
			c.remove(core.CatHidden, core.RiskMed, "Incomplete block", core.ByteCount(len(c.data)-pos), "at the end of the file")
			c.cutOff = true
			return c.finish(0)
		}
		if err != nil {
			return err
		}
		pos = next
	}
}

// unknownBlock handles a byte that starts no block. Decoders part ways here: browsers
// treat it as the end of the file, and others skip ahead looking for the next block,
// which would be a guess here. Stopping is what keeps unparsed bytes out of the
// output, and the warning says what that may cost.
func (c *cleaner) unknownBlock(intro byte, pos int) error {
	if c.images == 0 {
		return core.Malformedf("gif: unknown block type %#02x at offset %d", intro, pos)
	}
	rest := len(c.data) - pos
	c.rep.MayLoseContent = bytes.IndexByte(c.data[pos:], introImage) >= 0
	c.rep.Warn("found %s that are no GIF block where the next block should be: they were removed along with anything after them, which would include any further frames", core.ByteCount(rest))
	return c.finish(rest)
}

// header keeps the signature, logical screen descriptor and global colour table.
func (c *cleaner) header() (int, error) {
	pos := headerSize + screenSize
	if len(c.data) < pos {
		return 0, core.Malformedf("gif: %d bytes is too short for a GIF header", len(c.data))
	}
	end, err := c.colorTable(pos, c.data[pos-3])
	if err != nil {
		return 0, core.Malformedf("gif: the global colour table overruns the file")
	}
	c.parts = append(c.parts, c.data[:end])
	return end, nil
}

// colorTable returns the offset past the colour table flags describes, if any.
func (c *cleaner) colorTable(pos int, flags byte) (int, error) {
	if flags&0x80 == 0 {
		return pos, nil
	}
	size := 3 << ((flags & 0x07) + 1)
	if size > len(c.data)-pos {
		return 0, errCutOff
	}
	return pos + size, nil
}

// image keeps an image descriptor, its local colour table and its compressed data.
func (c *cleaner) image(ctx context.Context, pos int) (int, error) {
	if descriptorSize > len(c.data)-pos {
		return 0, errCutOff
	}
	data, err := c.colorTable(pos+descriptorSize, c.data[pos+descriptorSize-1])
	if err != nil {
		return 0, err
	}
	if data >= len(c.data) {
		return 0, errCutOff
	}
	if width := c.data[data]; width == 0 || width > maxLZWWidth {
		return 0, core.Malformedf("gif: the image at offset %d has an impossible LZW code size of %d", pos, width)
	}
	c.lzw.start(c.data[data], framePixels(c.data[pos:]))
	end, err := c.imageData(ctx, pos, data+1)
	if err != nil {
		return 0, err
	}
	c.images++
	c.pendingControl = 0
	return end, nil
}

// imageData keeps a frame from its descriptor at pos through its sub-blocks starting
// at first, and returns the offset past them. A frame normally comes through byte for
// byte. What is rewritten is the end of one that does not finish properly: sub-blocks
// after the end of the compressed stream, which look like image data to anything that
// does not decompress them, are cut, and a frame the end of the file cuts short keeps
// its whole sub-blocks, which is as far as decoders read it, and gets the terminator
// it lacks.
func (c *cleaner) imageData(ctx context.Context, pos, first int) (int, error) {
	at := first
	for n := 0; ; n++ {
		if n%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if at >= len(c.data) {
			c.cutOff = true
			c.parts = append(c.parts, c.data[pos:at], []byte{0})
			return at, nil
		}
		size := int(c.data[at])
		if size == 0 {
			c.parts = append(c.parts, c.data[pos:at+1])
			return at + 1, nil
		}
		blockEnd := at + 1 + size
		if blockEnd > len(c.data) {
			c.remove(core.CatHidden, core.RiskMed, "Incomplete block", core.ByteCount(len(c.data)-at), "at the end of the file")
			c.cutOff = true
			c.parts = append(c.parts, c.data[pos:at], []byte{0})
			return len(c.data), nil
		}
		payload := c.data[at+1 : blockEnd]
		used, done, err := c.lzw.feed(payload)
		if err != nil {
			return 0, err
		}
		if done {
			return c.endImageData(ctx, pos, at, payload[:used], blockEnd)
		}
		at = blockEnd
	}
}

// endImageData finishes a frame whose compressed stream ended inside the sub-block at
// at, having used only used of it.
func (c *cleaner) endImageData(ctx context.Context, pos, at int, used []byte, blockEnd int) (int, error) {
	end := len(c.data)
	complete := false
	next, err := c.subBlocks(ctx, blockEnd)
	switch {
	case err == nil:
		end, complete = next, true
	case !errors.Is(err, errCutOff):
		return 0, err
	}
	c.cutOff = c.cutOff || !complete
	consumed := at + 1 + len(used)
	tail := end - consumed
	if complete {
		tail-- // the terminator
	}
	if complete && tail == 0 {
		c.parts = append(c.parts, c.data[pos:end])
		return end, nil
	}
	if tail > 0 {
		c.remove(core.CatHidden, core.RiskHigh, "Data after the compressed image", core.ByteCount(tail), "image data")
	}
	c.parts = append(c.parts, c.data[pos:at], []byte{byte(len(used))}, used, []byte{0})
	return end, nil
}

// subBlocks returns the offset past the sequence of sub-blocks at pos.
func (c *cleaner) subBlocks(ctx context.Context, pos int) (int, error) {
	for n := 0; ; n++ {
		if n%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if pos >= len(c.data) {
			return 0, errCutOff
		}
		size := int(c.data[pos])
		pos++
		if size == 0 {
			return pos, nil
		}
		if size > len(c.data)-pos {
			return 0, errCutOff
		}
		pos += size
	}
}

// joinSubBlocks returns the payloads of the sub-blocks in raw, a range subBlocks
// has already walked, including its terminator.
func joinSubBlocks(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for len(raw) > 0 && raw[0] != 0 {
		size := int(raw[0])
		out = append(out, raw[1:1+size]...)
		raw = raw[1+size:]
	}
	return out
}

func (c *cleaner) extension(ctx context.Context, pos int) (int, error) {
	if len(c.data)-pos < 2 {
		return 0, errCutOff
	}
	switch label := c.data[pos+1]; label {
	case labelGraphicControl:
		return c.graphicControl(pos)
	case labelPlainText:
		return c.plainText(ctx, pos)
	case labelComment:
		return c.comment(ctx, pos)
	case labelApplication:
		return c.application(ctx, pos)
	default:
		end, err := c.subBlocks(ctx, pos+2)
		if err != nil {
			return 0, err
		}
		c.remove(core.CatHidden, core.RiskMed, "Other extension", fmt.Sprintf("label %#02x, %s", label, core.ByteCount(end-pos)), "extension")
		return end, nil
	}
}

// graphicControl keeps a Graphic Control extension, which holds frame timing and
// transparency. Its size is fixed, so any other shape is refused rather than
// offering a place for data to survive. It applies to the next frame, and decoders
// use the last before that frame, so only that one stays: the rest would be free room,
// as many times over as a file likes.
func (c *cleaner) graphicControl(pos int) (int, error) {
	const dataSize = 4
	if len(c.data)-pos > 2 && c.data[pos+2] != dataSize {
		return 0, core.Malformedf("gif: the Graphic Control extension at offset %d is not valid", pos)
	}
	if graphicSize > len(c.data)-pos {
		return 0, errCutOff
	}
	if c.data[pos+graphicSize-1] != 0 {
		return 0, core.Malformedf("gif: the Graphic Control extension at offset %d is not valid", pos)
	}
	c.dropPendingControl()
	c.parts = append(c.parts, c.data[pos:pos+graphicSize])
	c.pendingControl = len(c.parts)
	return pos + graphicSize, nil
}

// dropPendingControl removes a graphic control no frame came to use.
func (c *cleaner) dropPendingControl() {
	if c.pendingControl > 0 {
		c.parts[c.pendingControl-1] = nil
		c.pendingControl = 0
		c.remove(core.CatHidden, core.RiskLow, "Unused graphic control", core.ByteCount(graphicSize), "graphic control extension")
	}
}

// plainText keeps a Plain Text extension, because its text is drawn as part of
// the picture. Text that stays could be anything, so the user hears about it, here as
// everywhere without the value in the warning, since that is printed by clean.
func (c *cleaner) plainText(ctx context.Context, pos int) (int, error) {
	first := pos + 3 + plainTextHeader
	if len(c.data)-pos > 2 && c.data[pos+2] != plainTextHeader {
		return 0, core.Malformedf("gif: the Plain Text extension at offset %d is not valid", pos)
	}
	if first > len(c.data) {
		return 0, errCutOff
	}
	end, err := c.subBlocks(ctx, first)
	if err != nil {
		return 0, err
	}
	c.add(core.Finding{
		Category: core.CatContent,
		Risk:     core.RiskMed,
		Field:    "Plain Text extension",
		Value:    core.DecodeText(joinSubBlocks(c.data[first:end])),
		Where:    "plain text extension",
		Action:   core.ActionRetain,
		Reason:   "part of the picture",
	})
	if !c.sawPlainText {
		c.sawPlainText = true
		c.rep.Warn("this file holds Plain Text extensions, whose text is part of the picture and stays in the output, though most decoders do not draw it; inspect shows it")
	}
	c.pendingControl = 0
	c.parts = append(c.parts, c.data[pos:end])
	return end, nil
}

func (c *cleaner) comment(ctx context.Context, pos int) (int, error) {
	end, err := c.subBlocks(ctx, pos+2)
	if err != nil {
		return 0, err
	}
	c.remove(core.CatContent, core.RiskMed, "Comment", core.DecodeText(joinSubBlocks(c.data[pos+2:end])), "comment extension")
	return end, nil
}

// application keeps the looping extensions and removes all others. A looping
// extension with unexpected contents is removed too: the worst that does is stop
// an animation from repeating, while keeping it could carry anything.
func (c *cleaner) application(ctx context.Context, pos int) (int, error) {
	first := pos + 3 + applicationID
	if first > len(c.data) || int(c.data[pos+2]) != applicationID {
		end, err := c.subBlocks(ctx, pos+2)
		if err != nil {
			return 0, err
		}
		c.remove(core.CatHidden, core.RiskMed, "Application data", core.ByteCount(end-pos), "application extension")
		return end, nil
	}
	id := string(c.data[pos+3 : first])
	if id == "XMP DataXMP" {
		return c.xmpExtension(ctx, first)
	}
	end, err := c.subBlocks(ctx, first)
	if err != nil {
		return 0, err
	}
	if (id == "NETSCAPE2.0" || id == "ANIMEXTS1.0") && validLoopData(c.data[first:end]) {
		c.parts = append(c.parts, c.data[pos:end])
		return end, nil
	}
	c.remove(core.CatHidden, core.RiskMed, "Application data", fmt.Sprintf("%s, %s", id, core.ByteCount(end-pos)), "application extension")
	return end, nil
}

// validLoopData checks for the two sub-blocks these extensions define: a loop
// count (ID 1, two more bytes) and a buffer size (ID 2, four more bytes), at most
// one of each.
func validLoopData(raw []byte) bool {
	var seen [3]bool
	for raw[0] != 0 {
		size, id := int(raw[0]), raw[1]
		switch {
		case id == 1 && size == 3 && !seen[1], id == 2 && size == 5 && !seen[2]:
			seen[id] = true
		default:
			return false
		}
		raw = raw[1+size:]
	}
	return true
}

// xmpExtension removes an XMP extension. The packet is raw text, not real
// sub-blocks, so the magic trailer is what marks its end. An extension with no
// trailer is treated as real sub-blocks, which is what an ordinary decoder would do.
func (c *cleaner) xmpExtension(ctx context.Context, first int) (int, error) {
	const where = "application extension XMP"
	if !c.noXMPMagic {
		if i := bytes.Index(c.data[first:], xmpMagic); i >= 0 {
			c.addXMP(c.data[first:first+i], where)
			return first + i + len(xmpMagic), nil
		}
		c.noXMPMagic = true
	}
	end, err := c.subBlocks(ctx, first)
	if err != nil {
		return 0, err
	}
	c.addXMP(joinSubBlocks(c.data[first:end]), where)
	return end, nil
}

func (c *cleaner) addXMP(packet []byte, where string) {
	for _, f := range xmp.Findings(packet, where) {
		c.add(f)
	}
}

// finish writes the trailer, which a cut-off file lacks, and reports what was left
// behind.
func (c *cleaner) finish(trailing int) error {
	if c.images == 0 {
		return core.Malformedf("gif: the file holds no images")
	}
	c.dropPendingControl()
	c.parts = append(c.parts, []byte{introTrailer})
	if trailing > 0 {
		c.remove(core.CatHidden, core.RiskHigh, "Trailing data", core.ByteCount(trailing), "after the last block")
	}
	if c.stray > 0 {
		c.remove(core.CatHidden, core.RiskLow, "Stray bytes", core.ByteCount(c.stray), "between blocks")
	}
	if c.cutOff {
		c.rep.Warn("this file is cut off before its trailer: the output holds what is there with the file properly ended, so any missing frames stay missing")
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
