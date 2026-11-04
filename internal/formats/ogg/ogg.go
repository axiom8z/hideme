// Package ogg cleans Ogg Vorbis and Ogg Opus files.
//
// The metadata is one packet, the comment header, but an Ogg file is a sequence of
// checksummed pages and a Vorbis comment usually shares its page with the setup
// header. So the header packets are laid out in new pages, later pages of that
// logical stream are renumbered, and every page that changes gets a new checksum.
// Audio packets, granule positions and serial numbers are copied as they are.
package ogg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
)

const (
	formatID = "ogg"

	// maxActiveStreams bounds per-stream state, since a hostile file can start a
	// new logical stream every few dozen bytes. Real files have one or two.
	maxActiveStreams = 16
	// maxSetupLen bounds the one packet that must be kept in memory whole in order
	// to re-paginate it. Real Vorbis setup headers are kilobytes. Together with
	// maxActiveStreams, this limits what a file can make the handler hold to 128 MiB.
	maxSetupLen = 8 << 20
	// maxCommentRead bounds how much of a comment packet is decoded for the
	// report. Cover art can make the packet far larger, and everything in it is
	// removed whether or not it was read.
	maxCommentRead = 8 << 20
	// captureScanLen is how much is searched at once for the next page.
	captureScanLen = 64 << 10
	// maxLeadingTags bounds how many ID3v2 tags can come before the first page.
	maxLeadingTags = 8
	// opusHeadMinLen and the like come from RFC 7845, section 5.1.
	opusHeadMinLen     = 19
	opusFamilyTableLen = 21
	vorbisIDLen        = 30
)

var (
	vorbisIDMagic      = []byte("\x01vorbis")
	vorbisCommentMagic = []byte("\x03vorbis")
	vorbisSetupMagic   = []byte("\x05vorbis")
	opusHeadMagic      = []byte("OpusHead")
	opusTagsMagic      = []byte("OpusTags")

	betweenPagesField = audiotag.Field{Name: "Non-page data", Cat: core.CatHidden, Risk: core.RiskMed}
)

// otherCodecs names the codecs that are recognised only to explain why the file
// is refused.
var otherCodecs = []struct {
	magic []byte
	name  string
}{
	{[]byte("\x80theora"), "Theora"},
	{[]byte("Speex   "), "Speex"},
	{[]byte("\x7fFLAC"), "FLAC"},
	{[]byte("fLaC"), "FLAC"},
	{[]byte("fishead\x00"), "Skeleton"},
	{[]byte("fisbone\x00"), "Skeleton"},
	{[]byte("BBCD\x00"), "Dirac"},
	{[]byte("\x01video"), "OGM video"},
}

type codec int

const (
	codecVorbis codec = iota
	codecOpus
)

func (c codec) String() string {
	if c == codecVorbis {
		return "Vorbis"
	}
	return "Opus"
}

// headerPackets is how many packets each codec has before its audio.
func (c codec) headerPackets() int {
	if c == codecVorbis {
		return 3 // identification, comment, setup
	}
	return 2 // identification, comment
}

// minimalComment is the replacement comment packet: an empty vendor string, no
// comments and, for Vorbis, the framing bit that its specification requires.
func (c codec) minimalComment() []byte {
	if c == codecVorbis {
		return append(append([]byte(nil), vorbisCommentMagic...), 0, 0, 0, 0, 0, 0, 0, 0, 1)
	}
	return append(append([]byte(nil), opusTagsMagic...), 0, 0, 0, 0, 0, 0, 0, 0)
}

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "Ogg Vorbis or Opus audio",
		Family:     core.FamilyAudio,
		Extensions: []string{".ogg", ".oga", ".opus"},
		Limitations: []string{
			"Only Vorbis and Opus are supported. Files containing Theora, Speex, FLAC or Skeleton streams are refused.",
			"The Vorbis setup header stays unchanged. It is needed for decoding and is not parsed, so data appended inside that packet would not be noticed.",
			"The bit rate hints in a Vorbis identification header stay, 12 bytes, and readers ignore them.",
			"Audio packets and the page fields around them, such as granule positions, are copied without being decoded, so data hidden in them stays.",
		},
	}
}

// Match claims every Ogg file, including those with codecs it will refuse, so
// that the user hears which codec is the problem and not just that no handler
// recognised the file. Taggers can put an ID3v2 tag in front of any file, and players
// skip it, so one is looked past. The walk then treats it like any tag between pages.
func (handler) Match(head []byte, src core.Source) bool {
	if bytes.HasPrefix(head, []byte("ID3")) {
		win := audiotag.NewWindow(src)
		var pos int64
		for range maxLeadingTags {
			r, ok := audiotag.TagAt(win, pos, win.Size())
			if !ok {
				break
			}
			pos += r.Len
		}
		head = win.AtMost(pos, pageHeaderLen)
	}
	return len(head) >= pageHeaderLen && bytes.HasPrefix(head, capturePattern) && head[4] == 0 && head[5]&flagBOS != 0
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	return process(ctx, src, nil)
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	return process(ctx, src, dst)
}

// process walks the pages and, when dst is not nil, writes the cleaned stream.
func process(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
	w := &walker{
		win:     audiotag.NewWindow(src),
		sink:    audiotag.NewSink(formatID),
		dst:     dst,
		streams: make(map[uint32]*stream),
	}
	if err := w.run(ctx); err != nil {
		return nil, err
	}
	if w.pages == 0 {
		return nil, core.Malformedf("ogg: no pages found")
	}
	for _, st := range w.streams {
		if st.inHeaders() {
			return nil, core.Malformedf("ogg: the %s stream %#x ends before its headers do", st.codec, st.serial)
		}
	}
	return w.sink.Report(), nil
}

// walker holds the state of one pass over a file.
type walker struct {
	win     *audiotag.Window
	sink    *audiotag.Sink
	dst     io.Writer
	streams map[uint32]*stream
	pages   int64
	scratch []byte // reused for renumbered pages, of which there can be millions

	gapBytes   int64
	gapRegions int64
}

func (w *walker) run(ctx context.Context) error {
	var pos int64
	size := w.win.Size()
	for pos < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !bytes.HasPrefix(w.win.AtMost(pos, 4), capturePattern) {
			if err := w.win.Err(); err != nil {
				return err
			}
			next, more, err := w.afterGap(ctx, pos)
			if err != nil {
				return err
			}
			if !more {
				break
			}
			pos = next
			continue
		}
		p, ok, err := readPage(w.win, pos)
		if err != nil {
			return err
		}
		if !ok {
			// A file cut off mid-page. A partial page cannot be verified and no
			// player would use it, so it goes the way of other trailing bytes.
			if _, _, err := audiotag.ScanTrailing(ctx, w.win, pos, "after the last page", w.sink); err != nil {
				return err
			}
			w.sink.Warn("the file ends in a partial page, which was dropped")
			break
		}
		pos += int64(len(p.raw))
		w.pages++
		if err := w.page(p); err != nil {
			return err
		}
	}
	if w.gapBytes > 0 {
		value := fmt.Sprintf("%s in %d places", core.ByteCount(w.gapBytes), w.gapRegions)
		w.sink.Add(betweenPagesField.Remove(value, "between pages"))
	}
	return w.win.Err()
}

// afterGap handles bytes at pos that are not a page. It returns where the next
// page starts, or reports no more pages after dealing with the rest of the file
// as trailing data. Taggers leave ID3 and APE tags in such places.
func (w *walker) afterGap(ctx context.Context, pos int64) (int64, bool, error) {
	size := w.win.Size()
	if r, ok := audiotag.TagAt(w.win, pos, size); ok {
		if err := audiotag.Describe(ctx, w.win, r, "", w.sink); err != nil {
			return 0, false, err
		}
		return pos + r.Len, true, nil
	}
	next, found, err := w.findCapture(ctx, pos+1)
	if err != nil {
		return 0, false, err
	}
	if !found {
		_, _, err := audiotag.ScanTrailing(ctx, w.win, pos, "after the last page", w.sink)
		return 0, false, err
	}
	w.gapBytes += next - pos
	w.gapRegions++
	return next, true, nil
}

// findCapture returns the offset of the next capture pattern, if there is one.
func (w *walker) findCapture(ctx context.Context, pos int64) (int64, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		chunk := w.win.AtMost(pos, captureScanLen)
		if i := bytes.Index(chunk, capturePattern); i >= 0 {
			return pos + int64(i), true, nil
		}
		if len(chunk) < captureScanLen {
			return 0, false, w.win.Err()
		}
		// Overlap the next read, in case the pattern straddles the two.
		pos += int64(captureScanLen - (len(capturePattern) - 1))
	}
}

func (w *walker) page(p page) error {
	if p.flags&flagBOS != 0 {
		return w.beginStream(p)
	}
	st, ok := w.streams[p.serial]
	if !ok {
		return core.Malformedf("ogg: page %d of stream %#x comes without a beginning-of-stream page", p.seq, p.serial)
	}
	var err error
	if st.inHeaders() {
		err = w.headerPage(st, p)
	} else {
		err = w.audioPage(st, p)
	}
	if err != nil {
		return err
	}
	if p.flags&flagEOS != 0 && !st.inHeaders() {
		// The serial number may legally be reused by a chained stream after this.
		delete(w.streams, p.serial)
	}
	return nil
}

func (w *walker) beginStream(p page) error {
	if _, active := w.streams[p.serial]; active {
		return core.Malformedf("ogg: stream %#x begins twice", p.serial)
	}
	c, err := identify(p)
	if err != nil {
		return err
	}
	if len(w.streams) >= maxActiveStreams {
		return fmt.Errorf("%w: more than %d simultaneous logical streams", core.ErrTooLarge, maxActiveStreams)
	}
	if p.flags&flagEOS != 0 || p.flags&flagContinued != 0 {
		return core.Malformedf("ogg: the first page of stream %#x has invalid flags", p.serial)
	}
	w.streams[p.serial] = &stream{serial: p.serial, codec: c, firstSeq: p.seq, packets: 1}
	return w.write(p.raw)
}

// identify works out a stream's codec from its first page, which must hold exactly
// one whole packet: both Vorbis and Opus require it, and a parser that assumed it
// without checking could be walked past a hidden packet.
func identify(p page) (codec, error) {
	var c codec
	switch {
	case bytes.HasPrefix(p.payload, vorbisIDMagic):
		c = codecVorbis
	case bytes.HasPrefix(p.payload, opusHeadMagic):
		c = codecOpus
	default:
		for _, other := range otherCodecs {
			if bytes.HasPrefix(p.payload, other.magic) {
				return 0, core.Unsupportedf("ogg: %s streams are not supported", other.name)
			}
		}
		return 0, core.Unsupportedf("ogg: stream %#x uses an unknown codec", p.serial)
	}
	for i, lace := range p.lacing {
		if lace < segmentMax != (i == len(p.lacing)-1) {
			return 0, core.Malformedf("ogg: the first page of stream %#x does not hold exactly one packet", p.serial)
		}
	}
	if err := checkIdentification(c, p.payload); err != nil {
		return 0, err
	}
	return c, nil
}

// checkIdentification validates the one header packet that is kept unchanged.
// Both formats fix its length, so spare bytes in it can only be hidden data.
func checkIdentification(c codec, packet []byte) error {
	if c == codecVorbis {
		if len(packet) != vorbisIDLen {
			return core.Malformedf("ogg: Vorbis identification header is %d bytes, not %d", len(packet), vorbisIDLen)
		}
		if version := binary.LittleEndian.Uint32(packet[7:]); version != 0 {
			return core.Unsupportedf("ogg: Vorbis version %d", version)
		}
		return nil
	}
	if len(packet) < opusHeadMinLen {
		return core.Malformedf("ogg: OpusHead is only %d bytes", len(packet))
	}
	if major := packet[8] >> 4; major != 0 {
		return core.Unsupportedf("ogg: Opus encapsulation version %d", packet[8])
	}
	channels, family := int(packet[9]), packet[18]
	var want int
	switch family {
	case 0:
		want = opusHeadMinLen
	case 1, 2, 255:
		want = opusFamilyTableLen + channels
	case 3:
		if len(packet) < opusFamilyTableLen {
			return core.Malformedf("ogg: OpusHead is only %d bytes", len(packet))
		}
		want = opusFamilyTableLen + 2*(int(packet[19])+int(packet[20]))*channels
	default:
		return core.Unsupportedf("ogg: Opus channel mapping family %d", family)
	}
	if len(packet) != want {
		return core.Malformedf("ogg: OpusHead is %d bytes where its channel mapping needs %d", len(packet), want)
	}
	return nil
}

// stream is the state of one logical stream.
type stream struct {
	serial   uint32
	codec    codec
	firstSeq uint32
	// packets counts the header packets completed so far.
	packets int
	// partial is true while a header packet continues onto the next page.
	partial bool
	current []byte
	size    int64 // current packet's full length, which may exceed len(current)
	setup   []byte
	// delta is added to the sequence number of every page after the headers.
	delta uint32
	// audioOpen is true while an audio packet continues onto the next page.
	audioOpen bool
}

func (st *stream) inHeaders() bool {
	return st.packets < st.codec.headerPackets()
}

// headerPage takes in a page holding comment or setup data. Nothing is written
// until the headers are complete, when they are rewritten as new pages.
func (w *walker) headerPage(st *stream, p page) error {
	if p.flags&flagContinued != 0 != st.partial {
		return core.Malformedf("ogg: page %d of stream %#x breaks packet continuation", p.seq, st.serial)
	}
	body := p.payload
	for _, lace := range p.lacing {
		if !st.inHeaders() {
			return core.Malformedf("ogg: audio shares a page with the headers in stream %#x", st.serial)
		}
		if err := st.appendFragment(body[:lace]); err != nil {
			return err
		}
		body = body[lace:]
		st.partial = lace == segmentMax
		if !st.partial {
			if err := w.finishHeaderPacket(st); err != nil {
				return err
			}
		}
	}
	if st.inHeaders() {
		if p.flags&flagEOS != 0 {
			return core.Malformedf("ogg: stream %#x ends before its headers do", st.serial)
		}
		return nil
	}
	if st.partial {
		return core.Malformedf("ogg: stream %#x has a partial packet after its headers", st.serial)
	}
	return w.emitHeaders(st, p)
}

// appendFragment adds packet data, holding on to only as much as is needed.
func (st *stream) appendFragment(frag []byte) error {
	st.size += int64(len(frag))
	limit := int64(maxCommentRead)
	if st.packets == 2 {
		limit = maxSetupLen
		if st.size > limit {
			return fmt.Errorf("%w: Vorbis setup header of more than %d bytes", core.ErrTooLarge, maxSetupLen)
		}
	}
	if room := limit - int64(len(st.current)); room > 0 {
		st.current = append(st.current, frag[:min(int64(len(frag)), room)]...)
	}
	return nil
}

func (w *walker) finishHeaderPacket(st *stream) error {
	packet := st.current
	st.current, st.size = nil, 0
	var err error
	if st.packets == 1 {
		err = w.commentPacket(st, packet)
	} else {
		if !bytes.HasPrefix(packet, vorbisSetupMagic) {
			err = core.Malformedf("ogg: third packet of Vorbis stream %#x is not a setup header", st.serial)
		}
		st.setup = packet
	}
	st.packets++
	return err
}

// commentPacket reports a comment packet. A packet that is exactly what cleaning
// writes reports nothing, however it was paginated.
func (w *walker) commentPacket(st *stream, packet []byte) error {
	magic := opusTagsMagic
	if st.codec == codecVorbis {
		magic = vorbisCommentMagic
	}
	if !bytes.HasPrefix(packet, magic) {
		return core.Malformedf("ogg: second packet of %s stream %#x is not a comment header", st.codec, st.serial)
	}
	where := st.codec.String() + " comment"
	rest := audiotag.VorbisComments(packet[len(magic):], where, w.sink)
	if st.codec == codecVorbis && len(rest) > 0 {
		rest = rest[1:] // the framing bit
	}
	if len(rest) > 0 {
		w.sink.Add(audiotag.PaddingField.Remove(core.ByteCount(int64(len(rest))), where))
	}
	return nil
}

// emitHeaders writes the new header pages and works out the renumbering for what
// follows. lastPage is the original page that finished the headers. Its end of
// stream flag carries over, as a stream can have no audio at all.
func (w *walker) emitHeaders(st *stream, lastPage page) error {
	packets := [][]byte{st.codec.minimalComment()}
	if st.codec == codecVorbis {
		packets = append(packets, st.setup)
	}
	pages := paginate(packets, st.serial, st.firstSeq+1, lastPage.flags&flagEOS)
	for _, raw := range pages {
		if err := w.write(raw); err != nil {
			return err
		}
	}
	st.delta = st.firstSeq + 1 + uint32(len(pages)) - (lastPage.seq + 1)
	st.setup = nil
	return nil
}

// audioPage copies an audio page, renumbering it if the headers changed length.
func (w *walker) audioPage(st *stream, p page) error {
	if err := checkAudioPackets(st, p); err != nil {
		return err
	}
	if st.delta == 0 || w.dst == nil {
		return w.write(p.raw)
	}
	w.scratch = append(w.scratch[:0], p.raw...)
	binary.LittleEndian.PutUint32(w.scratch[seqOffset:], p.seq+st.delta)
	binary.LittleEndian.PutUint32(w.scratch[crcOffset:], pageCRC(w.scratch))
	return w.write(w.scratch)
}

// checkAudioPackets refuses header packets among the audio. A second comment
// packet is ignored by players, which would make it a quiet way to keep tags in a
// file that looks clean. Nor can a page continue a packet that was never started,
// which no player reads and this check would not see. Audio packets cannot be
// inspected any further than this.
func checkAudioPackets(st *stream, p page) error {
	if p.flags&flagContinued != 0 && !st.audioOpen {
		return core.Malformedf("ogg: page %d of stream %#x continues a packet that never began", p.seq, st.serial)
	}
	if n := len(p.lacing); n > 0 {
		st.audioOpen = p.lacing[n-1] == segmentMax
	}
	start := 0
	atPacketStart := p.flags&flagContinued == 0
	for _, lace := range p.lacing {
		if atPacketStart && lace > 0 && isHeaderPacket(st.codec, p.payload[start:]) {
			return core.Malformedf("ogg: a header packet appears among the audio of stream %#x", st.serial)
		}
		start += int(lace)
		atPacketStart = lace < segmentMax
	}
	return nil
}

func isHeaderPacket(c codec, b []byte) bool {
	if c == codecVorbis {
		return b[0]&1 != 0 // audio packets have a clear low bit, headers a set one
	}
	return bytes.HasPrefix(b, opusTagsMagic) || bytes.HasPrefix(b, opusHeadMagic)
}

func (w *walker) write(raw []byte) error {
	if w.dst == nil {
		return nil
	}
	if _, err := w.dst.Write(raw); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
