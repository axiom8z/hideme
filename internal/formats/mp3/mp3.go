// Package mp3 cleans MPEG audio files by keeping only their audio frames.
//
// An MP3 file has no container to rewrite: it is a run of self-synchronising
// frames with tags stuck on wherever a tool felt like it. That suits an
// allowlist. Walk the whole stream, keep what parses as a frame, and every tag
// format, past and future, falls away without having to be recognised. The tag
// decoding is there only to tell the user what was removed.
package mp3

import (
	"context"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
)

const (
	formatID = "mp3"

	// chainLen is how many consecutive frames establish that a stream has really
	// started. Random data passes one header check often enough to matter, but
	// passing several with each length pointing exactly at the next does not.
	chainLen = 3
	// longChainLen is how many are wanted when audio does not start where it should.
	// There a chain is the only evidence, and several places have to be tried.
	longChainLen = 8
	// maxLeadingTags and maxLeadingZeros bound the search for the first frame in
	// Match, which runs on every file the tool sees.
	maxLeadingTags  = 8
	maxLeadingZeros = 64 << 10
	// maxUnknownPrefix bounds what can come before the first frame with no tag to say
	// the file is audio. It is more than the longest frame, 2881 bytes, because the
	// usual cause is a stream cut in the middle of one.
	maxUnknownPrefix = 4 << 10
	// ctxCheckEvery spaces out context checks in the frame loop, which can
	// iterate millions of times.
	ctxCheckEvery = 1024
	// recentFrames covers audiotag.TailSize bytes in the shortest frames there
	// are, which are 24 bytes long.
	recentFrames = 8
)

const seekHeaderWhere = "seek header"

var nonAudioField = audiotag.Field{Name: "Non-audio data", Cat: core.CatHidden, Risk: core.RiskMed}

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "MP3 audio",
		Family:     core.FamilyAudio,
		Extensions: []string{".mp3", ".mp2", ".mpga"},
		Limitations: []string{
			"Data inside MPEG audio frames, such as ancillary bits and the header bits marked private or copyrighted, stays because removing it would mean changing the audio stream.",
			"The Xing, Info or VBRI seek header in the first frame stays, including the encoder version string in it, reported as retained, and its frame counts, seek table and encoder fields, about 150 bytes in all, which readers mostly do not check. The rest of the frame is cleared. If that frame has data ahead of its header, it may be audio, so it stays with a warning.",
			"Free-format bit rate streams are refused.",
			"A file with unrecognised data before its first frame and also among or after its frames is refused, because it could be something that stores an MP3 rather than an MP3.",
		},
	}
}

// Match requires a tag followed by a frame sync, or a chain of frames at the very
// start. A tag alone is not enough, because FLAC files can begin with one too, and a
// single sync is too easily found in arbitrary binary data.
//
// Failing those, it looks for a long chain of frames a little way in, which is what a
// stream cut mid-frame or a file behind some tool's header looks like. Audio found that
// way may be stored in something else, so another handler's claim wins, and process
// does not accept such a file unless the frames account for the rest of it.
func (handler) Match(head []byte, src core.Source) bool {
	if len(head) == 0 {
		return false
	}
	win := audiotag.NewWindow(src)
	pos, tagged := skipLeading(win)
	if tagged {
		return firstFrameFollows(win, pos) || longChainWithin(win, pos, pos+maxLeadingZeros)
	}
	if pos == 0 && chainAt(win, 0, src.Size(), chainLen) {
		return true
	}
	return longChainWithin(win, pos, pos+maxUnknownPrefix) && !ownedByAnother(head, src)
}

// skipLeading returns the offset after the zeros and the tags that can lead up to the
// audio, and whether there was a tag among them. Some taggers leave zero padding
// outside the size they declare, and some blank a tag instead of removing it.
func skipLeading(win *audiotag.Window) (int64, bool) {
	var pos int64
	tagged := false
	for range maxLeadingTags {
		pos += zeroRun(win, pos)
		r, ok := audiotag.TagAt(win, pos, win.Size())
		if !ok {
			return pos, tagged
		}
		pos += r.Len
		tagged = true
	}
	return pos + zeroRun(win, pos), tagged
}

func zeroRun(win *audiotag.Window, pos int64) int64 {
	var n int64
	for _, c := range win.AtMost(pos, maxLeadingZeros) {
		if c != 0 {
			break
		}
		n++
	}
	return n
}

// firstFrameFollows reports whether a frame header is at pos. A free-format one
// counts, so that the user hears why the file is refused.
func firstFrameFollows(win *audiotag.Window, pos int64) bool {
	_, status := parseHeader(win.AtMost(pos, headerLen))
	return status != headerInvalid
}

// chainAt reports whether n compatible frames follow one another from pos and fit
// before end.
func chainAt(win *audiotag.Window, pos, end int64, n int) bool {
	frames, _ := followChain(win, pos, end, n)
	return frames == n
}

// followChain counts up to n compatible frames in a row from pos, fitting before end,
// and returns where they stop.
func followChain(win *audiotag.Window, pos, end int64, n int) (int, int64) {
	var first frameHeader
	for i := range n {
		h, status := parseHeader(win.AtMost(pos, headerLen))
		if status != headerOK || h.length > end-pos {
			return i, pos
		}
		if i == 0 {
			first = h
		} else if !first.compatible(h) {
			return i, pos
		}
		pos += h.length
	}
	return n, pos
}

// longChainWithin reports whether a long chain of frames starts in [pos, limit).
func longChainWithin(win *audiotag.Window, pos, limit int64) bool {
	for limit = min(limit, win.Size()); pos < limit; pos++ {
		if b := win.AtMost(pos, 1); len(b) == 1 && b[0] == 0xFF && chainAt(win, pos, win.Size(), longChainLen) {
			return true
		}
	}
	return false
}

// ownedByAnother asks the other handlers about a file this one would only be
// claiming on the strength of what is inside it. A WAV file holding MP3 frames has a
// run of them right after its header, and that handler registers after this one.
func ownedByAnother(head []byte, src core.Source) bool {
	for _, h := range core.Handlers() {
		if h.Format().ID != formatID && h.Match(head, src) {
			return true
		}
	}
	return false
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	return process(ctx, src, nil)
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	return process(ctx, src, dst)
}

// process walks the file and, when dst is not nil, copies its frames there.
func process(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
	win := audiotag.NewWindow(src)
	if n, ok := audiotag.ID3v2Length(win.AtMost(0, 10)); ok && n > src.Size() {
		return nil, core.Malformedf("mp3: the ID3v2 tag declares %d bytes in a %d-byte file", n, src.Size())
	}
	trailers := audiotag.TrailingTags(win, 0, src.Size())
	end := src.Size()
	if len(trailers) > 0 {
		end = trailers[len(trailers)-1].Off
	}

	w := &walker{ctx: ctx, win: win, dst: dst, end: end, trailers: trailers, sink: audiotag.NewSink(formatID)}
	w.expectedFirst, w.tagged = skipLeading(win)
	if err := w.run(); err != nil {
		return nil, err
	}
	if err := w.finish(); err != nil {
		return nil, err
	}
	for i := len(trailers) - 1; i >= 0; i-- {
		if err := audiotag.Describe(ctx, win, trailers[i], "", w.sink); err != nil {
			return nil, err
		}
	}
	if w.junkBytes > 0 {
		value := fmt.Sprintf("%s in %d places", core.ByteCount(w.junkBytes), w.junkRegions)
		w.sink.Add(nonAudioField.Remove(value, "outside the audio frames"))
	}
	return w.sink.Report(), nil
}

// span is a byte range of the file.
type span struct{ off, n int64 }

// walker holds the state of one pass over a file.
type walker struct {
	ctx      context.Context
	win      *audiotag.Window
	dst      io.Writer
	end      int64 // where the tags anchored to the end of the file begin
	trailers []audiotag.Region
	sink     *audiotag.Sink

	expectedFirst int64 // where the first frame should be, past any zeros and tags
	tagged        bool  // the file starts with a tag, which says it is audio
	unknownPrefix bool  // something unrecognised came before the first frame
	cutLastFrame  bool  // the file ends in a frame that runs past its end
	seek          seekHeader
	firstLen      int64
	kept          int64 // bytes of frames kept

	started  bool // a chain of frames has been seen
	sawFree  bool
	prevKept bool // the previous bytes were a frame, ending where the walk now is
	prev     frameHeader

	pending     span // kept bytes not yet copied, coalesced so copies stay large
	recent      [recentFrames]span
	frames      int64
	junkBytes   int64
	junkRegions int64
	inJunk      bool
}

func (w *walker) run() error {
	var pos int64
	for i := 0; pos < w.end; i++ {
		if i%ctxCheckEvery == 0 {
			if err := w.ctx.Err(); err != nil {
				return err
			}
		}
		if err := w.win.Err(); err != nil {
			return err
		}
		if r, ok := audiotag.TagAt(w.win, pos, w.end); ok {
			if err := audiotag.Describe(w.ctx, w.win, r, "", w.sink); err != nil {
				return err
			}
			pos += r.Len
			w.prevKept, w.inJunk = false, false
			continue
		}
		h, ok, err := w.frameAt(pos)
		if err != nil {
			return err
		}
		if ok {
			if err := w.keep(pos, h); err != nil {
				return err
			}
			pos += h.length
			continue
		}
		next, err := w.nextCandidate(pos + 1)
		if err != nil {
			return err
		}
		if !w.inJunk {
			w.junkRegions++
			w.inJunk = true
		}
		w.junkBytes += next - pos
		w.prevKept = false
		pos = next
	}
	return w.win.Err()
}

// frameAt decides whether the bytes at pos are a frame to keep.
//
// Frames can be separated by junk, so a frame cannot be required to have a valid
// neighbour on both sides without dropping real audio next to the junk. Instead a
// frame needs one: either the previous frame runs right up to it or its length
// lands on another compatible frame. A chance sync in junk has neither, unless the
// junk starts right after a frame, and framesInside deals with that.
//
// That rule is also what makes cleaning idempotent. Removing the junk only ever
// makes kept frames adjacent, so each one passes again for the same reason.
func (w *walker) frameAt(pos int64) (frameHeader, bool, error) {
	h, status := parseHeader(w.win.AtMost(pos, headerLen))
	if status == headerFree {
		w.sawFree = true
	}
	if status != headerOK {
		return frameHeader{}, false, nil
	}
	next := pos + h.length
	followsPrev := w.prevKept && w.prev.compatible(h)
	if next > w.end {
		// A frame that ends exactly where a trailing "tag" does means one of two
		// things: a real tag after a truncated frame, or audio that happens to
		// read "TAG". One reading leaks a tag and the other drops audio, so
		// neither is safe to guess.
		if followsPrev && w.endsAtTrailer(next) {
			return frameHeader{}, false, core.Malformedf("mp3: a frame overlaps what looks like a tag at the end of the file")
		}
		w.cutLastFrame = w.cutLastFrame || followsPrev
		return frameHeader{}, false, nil
	}
	if !w.started {
		if !chainAt(w.win, pos, w.end, chainLen) {
			return frameHeader{}, false, nil
		}
		w.started = true
		return h, true, nil
	}
	if next == w.end {
		return h, true, nil
	}
	n, status := parseHeader(w.win.AtMost(next, headerLen))
	if status == headerOK && h.compatible(n) && n.length <= w.end-next {
		return h, true, nil
	}
	return h, followsPrev && !w.framesInside(pos, next), nil
}

// framesInside reports whether frames start inside the span of what reads as a frame
// and has nothing valid after it. If so, it is junk that happens to start with a
// header, and its length has run on into real audio. Keeping it would keep the junk
// and lose those frames. A real frame just before junk holds no such thing.
func (w *walker) framesInside(pos, next int64) bool {
	for p := pos + 1; p < next; p++ {
		if b := w.win.AtMost(p, 1); len(b) == 0 || b[0] != 0xFF {
			continue
		}
		if frames, stop := followChain(w.win, p, w.end, chainLen); frames == chainLen || (frames > 0 && stop == w.end) {
			return true
		}
	}
	return false
}

func (w *walker) endsAtTrailer(off int64) bool {
	for _, r := range w.trailers {
		if r.Off+r.Len == off {
			return true
		}
	}
	return false
}

// keep records the frame at pos as part of the output.
func (w *walker) keep(pos int64, h frameHeader) error {
	w.recent[w.frames%recentFrames] = span{pos, h.length}
	w.frames++
	w.kept += h.length
	w.prevKept, w.inJunk = true, false
	w.prev = h
	if w.frames == 1 {
		w.firstLen = h.length
		w.unknownPrefix = !w.tagged && pos > w.expectedFirst
	}
	done, err := w.seekFrame(pos, h)
	if done || err != nil {
		return err
	}
	if w.pending.off+w.pending.n == pos {
		w.pending.n += h.length
		return nil
	}
	if err := w.flush(); err != nil {
		return err
	}
	w.pending = span{pos, h.length}
	return nil
}

func (w *walker) flush() error {
	if w.dst == nil || w.pending.n == 0 {
		return nil
	}
	return core.CopyRange(w.ctx, w.dst, w.win.Source(), w.pending.off, w.pending.n)
}

// seekFrame deals with a frame that holds a seek header. The one that matters comes
// first, but joined files keep one for each part. It reports true if it wrote the
// frame itself.
func (w *walker) seekFrame(pos int64, h frameHeader) (bool, error) {
	first := w.frames == 1
	seek, ok := parseSeekHeader(w.win.AtMost(pos, int(h.length)), h)
	if !ok || (!first && !seek.silent) {
		return false, nil
	}
	if first {
		w.seek = seek
	}
	if seek.encoder != "" {
		f := audiotag.EncoderField.Remove(seek.encoder, seekHeaderWhere)
		f.Action = core.ActionRetain
		f.Reason = "it is part of the frame holding the seek header, which players need for seeking and gapless playback"
		w.sink.Add(f)
	}
	if !seek.silent {
		if first {
			w.sink.Warn("the first frame reads like a seek header but has data ahead of it. Players skip such a frame, and it could be audio, so it was kept and whatever it holds stays")
		}
		return false, nil
	}
	// This frame holds no audio, so whatever follows its header has no reason to be
	// there, and zeroing it cannot change what a player does.
	spare := h.length - int64(seek.end)
	empty, err := audiotag.AllZero(w.ctx, w.win, pos+int64(seek.end), spare)
	if empty || err != nil {
		return false, err
	}
	w.sink.Add(audiotag.HiddenPaddingField.Remove(core.ByteCount(spare), seekHeaderWhere))
	if w.dst != nil {
		if err := w.flush(); err != nil {
			return false, err
		}
		if err := core.CopyRange(w.ctx, w.dst, w.win.Source(), pos, int64(seek.end)); err != nil {
			return false, err
		}
		if err := core.WriteZeros(w.ctx, w.dst, spare); err != nil {
			return false, err
		}
	}
	w.pending = span{pos + h.length, 0}
	return true, nil
}

// nextCandidate returns the next offset that could start a frame or a tag. A stretch
// of junk can be most of a huge file, so this honours ctx for itself.
func (w *walker) nextCandidate(pos int64) (int64, error) {
	for pos < w.end {
		if err := w.ctx.Err(); err != nil {
			return 0, err
		}
		chunk := w.win.From(pos)
		if chunk == nil {
			return 0, w.win.Err()
		}
		chunk = chunk[:min(int64(len(chunk)), w.end-pos)]
		for i, c := range chunk {
			if c == 0xFF || c == 'I' || c == 'A' {
				return pos + int64(i), nil
			}
		}
		pos += int64(len(chunk))
	}
	return w.end, nil
}

// finish copies the last run of frames and checks the result is something this
// handler would accept and leave unchanged.
func (w *walker) finish() error {
	if !w.started {
		if w.sawFree {
			return core.Unsupportedf("mp3: free-format bit rate streams are not supported")
		}
		return core.Malformedf("mp3: no run of %d consecutive MPEG audio frames found", chainLen)
	}
	if err := w.checkUnknownPrefix(); err != nil {
		return err
	}
	if err := w.flush(); err != nil {
		return err
	}
	w.warnIfSeekHeaderIsStale()
	tail := w.tail()
	if err := w.win.Err(); err != nil {
		return err
	}
	if audiotag.LooksLikeTrailer(tail) {
		return core.Malformedf("mp3: the last frame ends in bytes that read as a tag")
	}
	return nil
}

// checkUnknownPrefix refuses a file that only Match's search for a chain of frames
// could recognise, unless the frames account for the rest of it. One stored in an
// archive has a directory after it and the next file's header, and turning that into
// a bare MP3 would destroy everything else in the archive.
func (w *walker) checkUnknownPrefix() error {
	regions := w.junkRegions
	if w.cutLastFrame {
		regions--
	}
	if w.unknownPrefix && regions > 1 {
		return core.Unsupportedf("mp3: %s of unrecognised data surround these frames, so this may be something that stores an MP3 rather than an MP3", core.ByteCount(w.junkBytes))
	}
	return nil
}

// warnIfSeekHeaderIsStale compares the size in the seek header with the audio. Junk
// removed from between frames is usually junk added after encoding, which the header
// never counted, so cleaning puts that right. A header that did count it, or counted
// tags, now points players at the wrong bytes: ffmpeg lands seconds off with -usetoc
// on a file that lost 15 percent, though its default seeking never reads the table.
//
// Correcting it would mean rebuilding the table and the checksum of the LAME tag, on a
// path almost nothing takes, for a field that is only an approximation anyway.
//
// The table has 256 steps, so a smaller difference cannot move a seek by a whole
// step, and encoders differ on whether to count the header's own frame.
func (w *walker) warnIfSeekHeaderIsStale() {
	if w.seek.size == 0 {
		return
	}
	difference := w.seek.size - w.kept
	if difference < 0 {
		difference = -difference
	}
	if difference > max(w.kept/256, w.firstLen) {
		w.sink.Warn("the %s seek header says the audio is %d bytes but it is %d, so players that seek with its table can land in the wrong place",
			w.seek.kind, w.seek.size, w.kept)
	}
}

// tail assembles the last bytes of the output. With tags gone the output could
// end in frame bytes that happen to read as a tag signature, which the next pass
// would take for a tag. Refusing here keeps cleaning idempotent, and the odds on
// a real file are vanishingly small.
func (w *walker) tail() []byte {
	var tail []byte
	first := max(w.frames-recentFrames, 0)
	for i := first; i < w.frames; i++ {
		s := w.recent[i%recentFrames]
		tail = append(tail, w.win.At(s.off, int(s.n))...)
	}
	return tail[max(len(tail)-audiotag.TailSize, 0):]
}
