// Package isobmff cleans files built on the ISO base media file format: HEIC,
// HEIF and AVIF images, and MP4, MOV, M4V, M4A, M4B and 3GP movies. Phone photos
// and videos are the most common way people leak their location, so this is the
// family where a miss hurts most.
//
// These files can be many gigabytes long and hold absolute offsets into
// themselves, so the file keeps its size and layout. Only the moov, moof and meta
// boxes are loaded. A removed box becomes a free box of the same size with a
// zeroed payload, and core.CopyPatched streams everything else through untouched.
//
// Inspect and Clean both call analyze and differ only in whether they apply the
// patches it plans, so what is reported and what is done cannot drift apart.
package isobmff

import (
	"context"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

func init() {
	core.Register(&handler{kind: kindHEIF, format: core.Format{
		ID:         "heif",
		Name:       "HEIF image (HEIC, AVIF)",
		Family:     core.FamilyImage,
		Extensions: []string{".heic", ".heif", ".hif", ".avif"},
		Limitations: []string{
			"Data inside the coded picture stream, such as HEVC SEI messages, is not removed.",
			"Depth maps, HDR gain maps and other auxiliary images are kept, because removing them changes how the photo looks; a depth map can reveal the shape of the scene.",
			"Other images in a collection or burst are kept, because they are content.",
			"Items stored in another file, or addressed through another item, are refused rather than cleaned.",
			"A JPEG or JPEG 2000 image item is a whole image file, and metadata inside it, such as EXIF, is not removed. HideMe warns when it finds one.",
			"Decoder configuration such as hvcC and av1C, and the bytes of an image item, are kept as written and are not inspected, apart from the lengths they state: bytes past those are cleared. Most of an hvcC header's 22 bytes go unread by decoders, and so could hold hidden data.",
			"A retained ICC colour profile is kept as it is and can name the device that made it; strict mode removes it.",
			"Values in the properties a decoder reads, such as image size, pixel depth and colour, are copied as they are, so a few bytes per property could survive.",
			"File-type brands that no specification defines are replaced. An unrecognised major brand stays, with a warning, because it says what the file is. It can name a camera maker's format.",
		},
	}})
	core.Register(&handler{kind: kindMP4, format: core.Format{
		ID:         "mp4",
		Name:       "MP4 or QuickTime media (MP4, MOV, M4V, M4A, M4B, 3GP)",
		Family:     core.FamilyVideo,
		Extensions: []string{".mp4", ".m4v", ".mov", ".qt", ".m4a", ".m4b", ".3gp", ".3g2"},
		Limitations: []string{
			"Data inside the encoded video or audio stream, such as H.264 SEI messages, is not removed.",
			"Subtitle tracks are kept and their text is not inspected. Some drones record GPS in subtitles.",
			"Motion JPEG and other video whose frames are whole image files keep any metadata inside those frames, such as EXIF. HideMe warns when it finds such a track.",
			"Auxiliary video tracks, which carry depth or transparency, are kept, because removing them changes how the video looks. A depth track can reveal the shape of the scene.",
			"Decoder configuration in a track's sample description, such as avcC and esds, is kept as written and is not inspected, apart from the lengths it states: bytes past those are cleared. Boxes there that HideMe does not recognise stay too, with a warning.",
			"Fixed-size fields that readers may use, such as durations, rates, dimensions and a video's rotation, are kept as written, even in strict mode. Many readers ignore many of them, so hidden data could survive there: about 54 bytes in the movie header, 54 per track in the track header and 10 per track in the media header. Reserved fields, and bytes past the end of a box, are cleared.",
			"Values in the tables that locate and describe samples, such as timings, groups of samples, dependencies and sub-samples, and track group numbers and the times in a fragment index, are kept as written. Readers do not cross-check most of them, so hidden data could survive there, up to a few bytes for each sample. Sample groups of a kind no specification defines are removed, and sync sample numbers that are out of order or out of range are cut.",
			"File-type brands that no specification defines are replaced. An unrecognised major brand stays, with a warning, because it says what the file is. It can name a camera maker's format, such as XAVC for Sony.",
			"Fragmented files that have a telemetry, timecode or other data track are refused, because the samples of a removed track cannot be located safely.",
			"Unreferenced bytes in media data are not checked in fragmented files, nor next to legacy QuickTime audio chunks, whose tables do not state their byte size.",
			"Track language codes are kept, because players use them to choose an audio track.",
			"An old QuickTime clipping region or matte stays, with a warning, because it changes how the video looks.",
			"Gapless playback information in iTunes tags (iTunSMPB) goes with the other tags, so a player may add a short silence at the start. The audio samples themselves do not change.",
		},
	}})
}

// handler is the same code registered twice. Only the kind of file it accepts
// differs, because the two share almost everything: a HEIF file can hold a moov
// and then follows the MP4 rules too.
type handler struct {
	kind   kind
	format core.Format
}

func (h *handler) Format() core.Format { return h.format }

func (h *handler) Match(_ []byte, src core.Source) bool {
	return detect(src).kind == h.kind
}

func (h *handler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	a, err := analyze(ctx, src, opts, h.kind)
	if err != nil {
		return nil, err
	}
	return a.report, nil
}

func (h *handler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	a, err := analyze(ctx, src, opts, h.kind)
	if err != nil {
		return nil, err
	}
	if err := core.CopyPatched(ctx, dst, src, a.patches); err != nil {
		return nil, err
	}
	return a.report, nil
}
