package audiotag

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// pictureKinds are the picture types ID3v2 defines, which FLAC and Vorbis comments
// adopted unchanged.
var pictureKinds = []string{
	"other", "file icon", "other file icon", "front cover", "back cover",
	"leaflet page", "media", "lead artist", "artist", "conductor", "band",
	"composer", "lyricist", "recording location", "during recording",
	"during performance", "video screen capture", "bright coloured fish",
	"illustration", "band logo", "publisher logo",
}

func pictureKind(n uint32) string {
	if int64(n) < int64(len(pictureKinds)) {
		return pictureKinds[n]
	}
	return ""
}

// PictureValue formats what is known about an embedded picture.
func PictureValue(mime, kind, description string, size int64) string {
	parts := make([]string, 0, 4)
	for _, p := range []string{mime, kind, description} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(append(parts, core.ByteCount(size)), ", ")
}

// FLACPicture formats the picture structure used by FLAC's PICTURE block and the
// METADATA_BLOCK_PICTURE comment. b may hold only the start of the structure, and
// size is the length of the whole thing.
func FLACPicture(b []byte, size int64) string {
	if len(b) < 8 {
		return core.ByteCount(size)
	}
	kind := pictureKind(binary.BigEndian.Uint32(b))
	mimeLen := uint64(binary.BigEndian.Uint32(b[4:]))
	b = b[8:]
	if mimeLen > uint64(len(b)) {
		return PictureValue("", kind, "", size)
	}
	mime := string(b[:mimeLen])
	b = b[mimeLen:]
	var description string
	if len(b) >= 4 {
		descLen := uint64(binary.BigEndian.Uint32(b))
		if b = b[4:]; descLen <= uint64(len(b)) {
			description = string(b[:descLen])
		}
	}
	return PictureValue(mime, kind, description, size)
}

// vorbisPicture decodes just enough of a base64 picture comment to describe it.
// The prefix holds the header, and decoding the whole value would double the
// memory a cover image costs for no benefit.
func vorbisPicture(value string) string {
	const prefix = 4096 // a multiple of 4, as base64 requires
	head := value[:min(len(value), prefix)]
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		return fmt.Sprintf("%d bytes of base64", len(value))
	}
	return FLACPicture(raw, int64(len(value))/4*3)
}
