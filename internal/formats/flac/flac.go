// Package flac cleans FLAC files by keeping only STREAMINFO, the seek table and
// the audio frames.
//
// Everything else in the format is optional, so this is a plain allowlist: the
// metadata blocks are rewritten from those two, and the frames are copied
// verbatim. Seek table offsets count from the first frame, so removing metadata in
// front of the frames leaves them correct.
package flac

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/audiotag"
	"github.com/axiom8z/hideme/internal/core"
)

const (
	formatID = "flac"

	blockStreamInfo  = 0
	blockPadding     = 1
	blockApplication = 2
	blockSeekTable   = 3
	blockVorbis      = 4
	blockCueSheet    = 5
	blockPicture     = 6
	blockInvalid     = 127

	blockLastFlag   = 0x80
	blockHeaderLen  = 4
	streamInfoLen   = 34
	seekPointLen    = 18
	seekPlaceholder = ^uint64(0)

	// maxLeadingTags bounds how many ID3v2 tags can come before the magic.
	maxLeadingTags = 8
	// pictureHeadLen is enough of a PICTURE block to describe it. Its image data
	// comes last and is of no use for the report.
	pictureHeadLen = 64 << 10
	// cueCatalogLen is the media catalog number field at the start of a cue sheet.
	cueCatalogLen = 128
)

var (
	applicationField = audiotag.Field{Name: "Application data", Cat: core.CatHidden, Risk: core.RiskMed}
	cueSheetField    = audiotag.Field{Name: "Cue sheet", Cat: core.CatContent, Risk: core.RiskLow}
	seekTableField   = audiotag.Field{Name: "Invalid seek table", Cat: core.CatHidden, Risk: core.RiskLow}
)

type handler struct{}

func init() { core.Register(handler{}) }

func (handler) Format() core.Format {
	return core.Format{
		ID:         formatID,
		Name:       "FLAC audio",
		Family:     core.FamilyAudio,
		Extensions: []string{".flac"},
		Limitations: []string{
			"The MD5 of the decoded audio in STREAMINFO stays, because players use it to verify the file. It identifies the recording, not the person. Readers do not check it unless asked to, and so it, and the size hints and total sample count beside it, could hold about 30 bytes of hidden data.",
			"Files with undecipherable bytes between audio frames are refused rather than guessed at.",
			"Data inside audio frames stays. Frames are checked against their checksums, not decoded, so bytes added inside one under a corrected checksum look like audio.",
		},
	}
}

func (handler) Match(head []byte, src core.Source) bool {
	if len(head) < 4 {
		return false
	}
	if string(head[:4]) == "fLaC" {
		return true
	}
	if head[0] != 'I' {
		return false
	}
	win := audiotag.NewWindow(src)
	pos, err := skipLeadingTags(context.Background(), win, nil)
	return err == nil && string(win.At(pos, 4)) == "fLaC"
}

func (handler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	return process(ctx, src, nil)
}

func (handler) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	return process(ctx, src, dst)
}

// skipLeadingTags returns the offset after any ID3v2 tags at the start of the
// file, reporting them if sink is not nil.
func skipLeadingTags(ctx context.Context, win *audiotag.Window, sink *audiotag.Sink) (int64, error) {
	var pos int64
	for range maxLeadingTags {
		r, ok := audiotag.TagAt(win, pos, win.Size())
		if !ok || r.Kind != audiotag.KindID3v2 {
			break
		}
		if sink != nil {
			if err := audiotag.Describe(ctx, win, r, "", sink); err != nil {
				return 0, err
			}
		}
		pos += r.Len
	}
	return pos, win.Err()
}

// process parses the file and, when dst is not nil, writes the cleaned copy.
func process(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
	win := audiotag.NewWindow(src)
	sink := audiotag.NewSink(formatID)
	start, err := skipLeadingTags(ctx, win, sink)
	if err != nil {
		return nil, err
	}
	if string(win.At(start, 4)) != "fLaC" {
		if err := win.Err(); err != nil {
			return nil, err
		}
		return nil, core.Unsupportedf("flac: no fLaC signature")
	}

	meta, err := readMetadata(ctx, win, start+4, sink)
	if err != nil {
		return nil, err
	}

	tagStart := src.Size()
	if tags := audiotag.TrailingTags(win, meta.audioOff, src.Size()); len(tags) > 0 {
		tagStart = tags[len(tags)-1].Off
	}
	walker := &frameWalker{ctx: ctx, win: win, tagStart: tagStart, audioOff: meta.audioOff, points: meta.seekTable}
	audioEnd, err := walker.walk(meta.audioOff)
	if err != nil {
		return nil, err
	}
	if err := checkTrailing(ctx, walker, audioEnd, sink); err != nil {
		return nil, err
	}

	table := normalizeSeekTable(meta.seekTable, audioEnd-meta.audioOff)
	if !walker.allPointsFound() {
		table = nil
	}
	if table == nil && meta.seekTable != nil {
		sink.Add(seekTableField.Remove(core.ByteCount(int64(len(meta.seekTable))), "metadata"))
	}
	if dst != nil {
		if err := writeOutput(ctx, dst, src, meta, table, audioEnd); err != nil {
			return nil, err
		}
	}
	return sink.Report(), nil
}

// checkTrailing reports what follows the audio. If that holds anything with the
// form of an audio frame, then a frame failed its CRC and further audio follows.
// Dropping it would damage the recording and keeping it would keep bytes nothing
// has vouched for, so the file is refused.
func checkTrailing(ctx context.Context, f *frameWalker, audioEnd int64, sink *audiotag.Sink) error {
	off, n, err := audiotag.ScanTrailing(ctx, f.win, audioEnd, "after the audio", sink)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	truncated := frameHeaderLen(f.win.AtMost(off, maxFrameHeaderLen)) > 0
	if truncated {
		off++
		n--
	}
	found, err := f.headerIn(off, n)
	if err != nil {
		return err
	}
	if found {
		return core.Malformedf("flac: an audio frame failed its CRC or is surrounded by junk, near offset %d", off)
	}
	if truncated {
		sink.Warn("the last audio frame is truncated or fails its CRC, so it was dropped")
	}
	return nil
}

// metadata is what the kept blocks hold and where the audio starts.
type metadata struct {
	streamInfo []byte
	seekTable  []byte
	audioOff   int64
}

func readMetadata(ctx context.Context, win *audiotag.Window, pos int64, sink *audiotag.Sink) (*metadata, error) {
	meta := &metadata{}
	for first := true; ; first = false {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr := win.At(pos, blockHeaderLen)
		if hdr == nil {
			if err := win.Err(); err != nil {
				return nil, err
			}
			return nil, core.Malformedf("flac: metadata ends at offset %d without a last block", pos)
		}
		last := hdr[0]&blockLastFlag != 0
		kind := hdr[0] &^ blockLastFlag
		size := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		body := pos + blockHeaderLen
		if size > win.Size()-body {
			return nil, core.Malformedf("flac: metadata block at offset %d declares %d bytes, past the end of the file", pos, size)
		}
		if first && kind != blockStreamInfo {
			return nil, core.Malformedf("flac: the first metadata block is type %d, not STREAMINFO", kind)
		}
		if err := meta.readBlock(ctx, win, kind, body, size, sink); err != nil {
			return nil, err
		}
		pos = body + size
		if last {
			meta.audioOff = pos
			return meta, win.Err()
		}
	}
}

func (m *metadata) readBlock(ctx context.Context, win *audiotag.Window, kind byte, body, size int64, sink *audiotag.Sink) error {
	const where = "metadata"
	switch kind {
	case blockStreamInfo:
		if m.streamInfo != nil {
			return core.Malformedf("flac: more than one STREAMINFO block")
		}
		if size != streamInfoLen {
			return core.Malformedf("flac: STREAMINFO is %d bytes, not %d", size, streamInfoLen)
		}
		m.streamInfo = append([]byte(nil), win.At(body, streamInfoLen)...)
	case blockSeekTable:
		if m.seekTable != nil {
			return core.Malformedf("flac: more than one SEEKTABLE block")
		}
		if size%seekPointLen != 0 {
			return core.Malformedf("flac: SEEKTABLE of %d bytes is not a whole number of seek points", size)
		}
		m.seekTable = append(make([]byte, 0, size), win.At(body, int(size))...)
	case blockVorbis:
		if rest := audiotag.VorbisComments(win.At(body, int(size)), "VORBIS_COMMENT", sink); len(rest) > 0 {
			sink.Add(audiotag.HiddenPaddingField.Remove(core.ByteCount(int64(len(rest))), "VORBIS_COMMENT"))
		}
	case blockPicture:
		value := audiotag.FLACPicture(win.AtMost(body, int(min(size, pictureHeadLen))), size)
		sink.Add(audiotag.PictureField.Remove(value, "PICTURE"))
	case blockApplication:
		id := win.At(body, int(min(size, 4)))
		sink.Add(applicationField.Remove(fmt.Sprintf("%s, %s", audiotag.FixedText(id), core.ByteCount(size)), where))
	case blockCueSheet:
		catalog := audiotag.FixedText(win.At(body, int(min(size, cueCatalogLen))))
		sink.Add(cueSheetField.Remove(catalog, where))
	case blockPadding:
		return reportPadding(ctx, win, body, size, sink)
	case blockInvalid:
		return core.Malformedf("flac: invalid metadata block type 127")
	default:
		sink.Add(audiotag.OtherChunkField.Remove(fmt.Sprintf("block type %d, %s", kind, core.ByteCount(size)), where))
	}
	return nil
}

func reportPadding(ctx context.Context, win *audiotag.Window, body, size int64, sink *audiotag.Sink) error {
	empty, err := audiotag.AllZero(ctx, win, body, size)
	if err != nil {
		return err
	}
	field := audiotag.PaddingField
	if !empty {
		field = audiotag.HiddenPaddingField
	}
	sink.Add(field.Remove(core.ByteCount(size), "metadata"))
	return nil
}

// normalizeSeekTable returns the table to write. Seek points only speed up
// seeking, so a table that is not valid for this audio is dropped rather than
// trusted, which also gets rid of whatever was stashed in it. Unused placeholder
// points have meaningless fields, which are zeroed for the same reason. The walk over
// the frames makes the stricter check, that each point describes a real frame.
func normalizeSeekTable(table []byte, audioLen int64) []byte {
	if table == nil {
		return nil
	}
	out := append(make([]byte, 0, len(table)), table...)
	var prev uint64
	seen, placeholders := false, false
	for i := 0; i < len(out); i += seekPointLen {
		point := out[i : i+seekPointLen]
		sample := binary.BigEndian.Uint64(point)
		if sample == seekPlaceholder {
			placeholders = true
			clear(point[8:])
			continue
		}
		offset := binary.BigEndian.Uint64(point[8:])
		if placeholders || (seen && sample <= prev) || offset >= uint64(audioLen) {
			return nil
		}
		prev, seen = sample, true
	}
	return out
}

func writeOutput(ctx context.Context, dst io.Writer, src core.Source, meta *metadata, table []byte, audioEnd int64) error {
	header := make([]byte, 0, 4+blockHeaderLen+streamInfoLen+blockHeaderLen+len(table))
	header = append(header, "fLaC"...)
	if table == nil {
		header = appendBlock(header, blockStreamInfo, true, meta.streamInfo)
	} else {
		header = appendBlock(header, blockStreamInfo, false, meta.streamInfo)
		header = appendBlock(header, blockSeekTable, true, table)
	}
	if _, err := dst.Write(header); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return core.CopyRange(ctx, dst, src, meta.audioOff, audioEnd-meta.audioOff)
}

func appendBlock(out []byte, kind byte, last bool, body []byte) []byte {
	if last {
		kind |= blockLastFlag
	}
	n := len(body)
	out = append(out, kind, byte(n>>16), byte(n>>8), byte(n))
	return append(out, body...)
}
