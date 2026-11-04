package matroska

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const idCodecPrivate = 0x63A2

// vorbisTags builds the body of a Vorbis comment block: the vendor, then the fields.
func vorbisTags(vendor string, fields ...string) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(vendor)))
	out = append(out, vendor...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(fields)))
	for _, field := range fields {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(field)))
		out = append(out, field...)
	}
	return out
}

// lace writes one Xiph lacing length.
func lace(n int) []byte {
	return append(bytes.Repeat([]byte{255}, n/255), byte(n%255))
}

// vorbisPrivate lays out three Vorbis headers the way RFC 9559 stores them.
func vorbisPrivate(comment []byte) []byte {
	id := []byte("\x01vorbisID-HEADER")
	setup := []byte("\x05vorbisSETUP-HEADER-MUST-SURVIVE")
	return slices.Concat([]byte{2}, lace(len(id)), lace(len(comment)), id, comment, setup)
}

func audioTrack(codec string, private []byte) []byte {
	return el(idTrackEntry, num(idTrackNumber, 1), num(idTrackType, trackAudio), text(idCodecID, codec), el(idCodecPrivate, private))
}

func fileWithTrack(track []byte) []byte {
	return slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTracks, track)))
}

func taggedVorbisComment() []byte {
	body := vorbisTags("Test Vendor 1.0", "ARTIST=Test Author", "LOCATION=Eiffel Tower", "ENCODER=Test Encoder")
	return slices.Concat([]byte("\x03vorbis"), body, []byte{1})
}

func flacBlockBytes(kind byte, last bool, payload []byte) []byte {
	first := kind
	if last {
		first |= 0x80
	}
	n := len(payload)
	return slices.Concat([]byte{first, byte(n >> 16), byte(n >> 8), byte(n)}, payload)
}

func flacPrivate() []byte {
	streamInfo := bytes.Repeat([]byte("S"), 34)
	return slices.Concat(flacMagic,
		flacBlockBytes(flacStreamInfo, false, streamInfo),
		flacBlockBytes(flacVorbisComment, false, vorbisTags("Test Vendor 1.0", "ARTIST=Test Author", "TITLE=Test Title")),
		flacBlockBytes(flacPicture, false, []byte("Test Picture Bytes")),
		flacBlockBytes(2, false, []byte("Test Application")),
		flacBlockBytes(flacPadding, false, []byte("Test Stale Padding")),
		flacBlockBytes(flacSeekTable, true, []byte("SEEKTABLE-KEPT-18B")))
}

func TestCodecPrivateContract(t *testing.T) {
	theoraComment := slices.Concat([]byte("\x81theora"), vorbisTags("Test Vendor 1.0", "ARTIST=Test Author"))
	theora := el(idTrackEntry, num(idTrackNumber, 1), num(idTrackType, trackVideo), text(idCodecID, codecTheora), el(idCodecPrivate, vorbisPrivate(theoraComment)))
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name: "Vorbis", Data: fileWithTrack(audioTrack(codecVorbis, vorbisPrivate(taggedVorbisComment()))),
			WantFields:     []string{"Codec vendor", "Tag ARTIST", "Tag LOCATION", "Tag ENCODER"},
			MustNotContain: asBytes("Test Vendor", "Test Author", "Eiffel Tower", "Test Encoder"),
			MustContain:    asBytes("ID-HEADER", "SETUP-HEADER-MUST-SURVIVE"),
		},
		{
			Name: "Theora", Data: fileWithTrack(theora),
			WantFields:     []string{"Codec vendor", "Tag ARTIST"},
			MustNotContain: asBytes("Test Vendor", "Test Author"),
			MustContain:    asBytes("SETUP-HEADER-MUST-SURVIVE"),
		},
		{
			Name: "FLAC", Data: fileWithTrack(audioTrack(codecFLAC, flacPrivate())),
			WantFields:     []string{"Codec vendor", "Tag ARTIST", "Tag TITLE", "Embedded picture", "FLAC metadata block", "Hidden data in codec private data"},
			MustNotContain: asBytes("Test Vendor", "Test Author", "Test Title", "Test Picture", "Test Application", "Test Stale"),
			MustContain:    asBytes("fLaC", "SSSSSSSS", "SEEKTABLE-KEPT-18B"),
		},
		{
			Name: "Vorbis comment whose text runs past what is read",
			Data: fileWithTrack(audioTrack(codecVorbis, vorbisPrivate(slices.Concat(
				[]byte("\x03vorbis"), vorbisTags("Test Vendor 1.0", "METADATA_BLOCK_PICTURE="+string(bytes.Repeat([]byte("Z"), maxCodecHeaderRead))), []byte{1})))),
			WantFields:     []string{"Codec vendor"},
			MustNotContain: asBytes("Test Vendor", "ZZZZ"),
			MustContain:    asBytes("SETUP-HEADER-MUST-SURVIVE"),
		},
	})
}

// The private data keeps its size and its lengths, because the first bytes record
// the lengths of the headers. A decoder must still find an empty comment packet.
func TestVorbisCommentIsValidAfterCleaning(t *testing.T) {
	private := vorbisPrivate(taggedVorbisComment())
	out, _ := clean(t, fileWithTrack(audioTrack(codecVorbis, private)))
	i := bytes.Index(out, []byte("\x03vorbis"))
	if i < 0 {
		t.Fatal("the comment packet is gone")
	}
	packet := out[i : i+len(taggedVorbisComment())]
	want := append([]byte("\x03vorbis"), make([]byte, 8)...)
	want = append(want, 1)
	want = append(want, make([]byte, len(taggedVorbisComment())-len(want))...)
	if !bytes.Equal(packet, want) {
		t.Errorf("the comment packet is %q", packet)
	}
	if len(out) != len(fileWithTrack(audioTrack(codecVorbis, private))) {
		t.Error("the file changed size")
	}
}

func TestFLACChainAfterCleaning(t *testing.T) {
	out, _ := clean(t, fileWithTrack(audioTrack(codecFLAC, flacPrivate())))
	i := bytes.Index(out, flacMagic)
	if i < 0 {
		t.Fatal("the private data lost its signature")
	}
	pos := i + len(flacMagic)
	var kinds []byte
	for {
		first := out[pos]
		kinds = append(kinds, first&0x7F)
		n := int(out[pos+1])<<16 | int(out[pos+2])<<8 | int(out[pos+3])
		pos += flacHeaderSize + n
		if first&0x80 != 0 {
			break
		}
	}
	want := []byte{flacStreamInfo, flacPadding, flacPadding, flacPadding, flacPadding, flacSeekTable}
	if !bytes.Equal(kinds, want) {
		t.Errorf("block types are %v, want %v", kinds, want)
	}
}

func TestCodecPrivateCRC(t *testing.T) {
	crc := el(idCRC32, []byte{1, 2, 3, 4})
	track := el(idTrackEntry, crc, num(idTrackType, trackAudio), text(idCodecID, codecVorbis), el(idCodecPrivate, vorbisPrivate(taggedVorbisComment())))
	out, _ := clean(t, fileWithTrack(track))
	if bytes.Contains(out, crc) {
		t.Error("a stale CRC-32 survived in the TrackEntry")
	}
}

func TestOtherCodecPrivateIsKept(t *testing.T) {
	data := fileWithTrack(audioTrack("A_OPUS", []byte("OpusHeadKEPT")))
	if out, rep := clean(t, data); !bytes.Equal(out, data) || len(rep.Findings) != 0 {
		t.Errorf("Opus private data changed, or gave findings: %+v", rep.Findings)
	}
}

func TestBadCodecPrivate(t *testing.T) {
	comment := taggedVorbisComment()
	for name, data := range map[string][]byte{
		"empty Vorbis":        fileWithTrack(audioTrack(codecVorbis, nil)),
		"wrong packet count":  fileWithTrack(audioTrack(codecVorbis, []byte{1, 1, 'a', 'b'})),
		"cut lacing":          fileWithTrack(audioTrack(codecVorbis, []byte{2, 255, 255})),
		"packets overrun":     fileWithTrack(audioTrack(codecVorbis, []byte{2, 200, 200, 'a'})),
		"wrong comment magic": fileWithTrack(audioTrack(codecVorbis, vorbisPrivate(bytes.Repeat([]byte("x"), 30)))),
		"short comment":       fileWithTrack(audioTrack(codecVorbis, vorbisPrivate(comment[:9]))),
		"no FLAC signature":   fileWithTrack(audioTrack(codecFLAC, []byte("nope"))),
		"short FLAC":          fileWithTrack(audioTrack(codecFLAC, []byte("fL"))),
		"cut FLAC header":     fileWithTrack(audioTrack(codecFLAC, slices.Concat(flacMagic, []byte{0, 0}))),
		"FLAC block overrun":  fileWithTrack(audioTrack(codecFLAC, slices.Concat(flacMagic, []byte{4, 0, 1, 0}))),
		"two codec IDs":       fileWithTrack(el(idTrackEntry, num(idTrackType, trackAudio), text(idCodecID, "A_OPUS"), text(idCodecID, codecVorbis))),
	} {
		t.Run(name, func(t *testing.T) { wantError(t, data, core.ErrMalformed) })
	}
}

func TestDamagedCommentBlocksStillGo(t *testing.T) {
	for name, body := range map[string][]byte{
		"vendor overruns": slices.Concat([]byte{0xFF, 0xFF, 0xFF, 0x7F}, []byte("Test Secret")),
		"field overruns":  slices.Concat(vorbisTags(""), []byte{}, []byte{0xFF, 0xFF, 0xFF, 0x7F}, []byte("Test Secret")),
		"no count":        slices.Concat(binary.LittleEndian.AppendUint32(nil, 0), []byte("Tes")),
		"too short":       []byte("Te"),
	} {
		t.Run(name, func(t *testing.T) {
			private := slices.Concat(flacMagic, flacBlockBytes(flacStreamInfo, false, make([]byte, 34)), flacBlockBytes(flacVorbisComment, true, body))
			out, rep := clean(t, fileWithTrack(audioTrack(codecFLAC, private)))
			if bytes.Contains(out, []byte("Test")) || bytes.Contains(out, []byte("Tes")) {
				t.Errorf("a damaged comment block survived: %q", out)
			}
			if len(rep.Findings) == 0 {
				t.Error("nothing was reported")
			}
		})
	}
}

func TestRobustnessCodecPrivate(t *testing.T) {
	coretest.Robustness(t, handler{}, fileWithTrack(audioTrack(codecVorbis, vorbisPrivate(taggedVorbisComment()))))
	coretest.Robustness(t, handler{}, fileWithTrack(audioTrack(codecFLAC, flacPrivate())))
}

func TestXiphLacingLengths(t *testing.T) {
	for _, n := range []int{0, 1, 254, 255, 256, 509, 510, 511} {
		comment := make([]byte, n)
		raw := slices.Concat([]byte{2}, lace(3), lace(n), []byte("abc"), comment, []byte("z"))
		start, length, err := xiphCommentPacket(raw, int64(len(raw)))
		if err != nil {
			t.Errorf("length %d: %v", n, err)
			continue
		}
		if int(length) != n || int(start)+n != len(raw)-1 {
			t.Errorf("length %d: start %d, length %d, in %d bytes", n, start, length, len(raw))
		}
	}
	if _, _, err := xiphCommentPacket(nil, 0); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("no data gave %v", err)
	}
}
