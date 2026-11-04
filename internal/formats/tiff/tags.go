package tiff

import (
	"github.com/axiom8z/hideme/internal/exif"
)

// File signatures.
const (
	magicLittle        = "II*\x00"
	magicBig           = "MM\x00*"
	magicBigTIFFLittle = "II+\x00"
	magicBigTIFFBig    = "MM\x00+"
	magicORF           = "IIRO"
	magicORFBig        = "MMOR"
	magicORFAlternate  = "IIRS"
	magicRW2           = "IIU\x00"
)

// TIFF field types.
const (
	typeByte      uint16 = 1
	typeASCII     uint16 = 2
	typeShort     uint16 = 3
	typeLong      uint16 = 4
	typeRational  uint16 = 5
	typeSByte     uint16 = 6
	typeUndefined uint16 = 7
	typeSShort    uint16 = 8
	typeSLong     uint16 = 9
	typeSRational uint16 = 10
	typeFloat     uint16 = 11
	typeDouble    uint16 = 12
	typeIFD       uint16 = 13
)

func typeSize(typ uint16) int64 { return int64(exif.TypeSize(typ)) }

// Rendering tags.
const (
	tagNewSubfileType  uint16 = 254
	tagSubfileType     uint16 = 255
	tagImageWidth      uint16 = 256
	tagImageLength     uint16 = 257
	tagBitsPerSample   uint16 = 258
	tagCompression     uint16 = 259
	tagPhotometric     uint16 = 262
	tagThresholding    uint16 = 263
	tagFillOrder       uint16 = 266
	tagStripOffsets    uint16 = 273
	tagOrientation     uint16 = 274
	tagSamplesPerPixel uint16 = 277
	tagRowsPerStrip    uint16 = 278
	tagStripByteCounts uint16 = 279
	tagMinSampleValue  uint16 = 280
	tagMaxSampleValue  uint16 = 281
	tagXResolution     uint16 = 282
	tagYResolution     uint16 = 283
	tagPlanarConfig    uint16 = 284
	tagT4Options       uint16 = 292
	tagT6Options       uint16 = 293
	tagResolutionUnit  uint16 = 296
	tagPredictor       uint16 = 317
	tagWhitePoint      uint16 = 318
	tagPrimaries       uint16 = 319
	tagColorMap        uint16 = 320
	tagTileWidth       uint16 = 322
	tagTileLength      uint16 = 323
	tagTileOffsets     uint16 = 324
	tagTileByteCounts  uint16 = 325
	tagInkSet          uint16 = 332
	tagNumberOfInks    uint16 = 334
	tagDotRange        uint16 = 336
	tagExtraSamples    uint16 = 338
	tagSampleFormat    uint16 = 339
	tagSMinSampleValue uint16 = 340
	tagSMaxSampleValue uint16 = 341
	tagJPEGTables      uint16 = 347
	tagYCbCrCoeffs     uint16 = 529
	tagYCbCrSubSamp    uint16 = 530
	tagYCbCrPosition   uint16 = 531
	tagRefBlackWhite   uint16 = 532
	tagICCProfile      uint16 = 34675
)

// Tags with their own treatment when removed.
const (
	tagImageDescription uint16 = 270
	tagPageName         uint16 = 285
	tagTargetPrinter    uint16 = 337
	tagXMP              uint16 = 0x02BC
	tagImageID          uint16 = 32781
	tagIPTC             uint16 = 0x83BB
	tagPhotoshop        uint16 = 0x8649
	tagImageSourceData  uint16 = 0x935C
	tagMakerNote        uint16 = 0x927C
	tagExifIFD          uint16 = 0x8769
	tagGPSIFD           uint16 = 0x8825
	tagInteropIFD       uint16 = 0xA005
	tagJPEGThumbnail    uint16 = 0x0201
	tagJPEGThumbLength  uint16 = 0x0202
)

// Compression and photometric values the rules below refer to.
const (
	compressionNone    = 1
	compressionOldJPEG = 6
	compressionJPEG    = 7
	photometricPalette = 3
	photometricYCbCr   = 6
)

// supportedCompression lists the codes whose strips are self-contained streams
// that depend on nothing but the rendering tags. It is an allowlist because the
// codes it leaves out are what gives camera raw files away (34713 for NEF, 32767
// for ARW, 65535 for PEF and so on): their decoders rely on private tags that
// removing metadata would destroy. JPEG 2000 and WebP are out for a different
// reason: their strips are whole files that can carry metadata of their own.
var supportedCompression = map[uint32]bool{
	compressionNone: true,
	2:               true, // CCITT modified Huffman
	3:               true, // CCITT T.4
	4:               true, // CCITT T.6
	5:               true, // LZW
	compressionJPEG: true, // scanned for metadata segments; see jpeg.go
	8:               true, // Deflate
	32771:           true, // CCITT modified Huffman, word-aligned
	32773:           true, // PackBits
	32946:           true, // Deflate, older code
	34925:           true, // LZMA
	50000:           true, // Zstandard
}

// supportedPhotometric leaves out colour filter arrays and linear raw data, the
// mark of a camera raw image even when it is stored uncompressed.
var supportedPhotometric = map[uint32]bool{
	0: true, 1: true, 2: true, 3: true, 4: true, 5: true,
	photometricYCbCr: true,
	8:                true, 9: true, 10: true,
}

// refusedTags name files that look like TIFF but are not safe to rewrite: their
// pictures or the data needed to decode them live where this handler would drop
// them. The value completes the sentence in the refusal message.
var refusedTags = map[uint16]string{
	0xC612: "a DNGVersion tag, so it is a DNG camera raw file",
	0x014A: "a SubIFDs tag, as camera raw files do, and those images would be lost",
	0x828D: "a colour filter pattern, so it is a camera raw file",
	0x828E: "a colour filter pattern, so it is a camera raw file",
	0x9216: "a TIFF/EP tag, so it is a camera raw file",
	0x835C: "a MetaMorph tag, and such a stack stores frames that no directory points to",
	0x835D: "a MetaMorph tag, and such a stack stores frames that no directory points to",
	0x835E: "a MetaMorph tag, and such a stack stores frames that no directory points to",
	0x835F: "a MetaMorph tag, and such a stack stores frames that no directory points to",
}

// typeSet is a set of TIFF field types.
type typeSet uint32

func types(list ...uint16) typeSet {
	var set typeSet
	for _, t := range list {
		set |= 1 << t
	}
	return set
}

func (s typeSet) has(t uint16) bool {
	return t < 32 && s&(1<<t) != 0
}

var (
	integers  = types(typeByte, typeShort, typeLong)
	rationals = types(typeRational)
	rawBytes  = types(typeByte, typeUndefined)
	anyNumber = integers | rationals | types(typeSByte, typeSShort, typeSLong, typeSRational, typeFloat, typeDouble)
)

// countRule says how many elements a tag may hold.
type countRule int

const (
	countOne         countRule = iota // exactly one
	countFixed                        // exactly shape.n
	countPerSample                    // one for all samples, or one per sample
	countUpToSamples                  // at most one per sample
	countColorMap                     // three tables of 2^BitsPerSample
	countDotRange                     // one pair for all samples, or one per sample
	countUnbounded                    // checked elsewhere, or bounded only by the file
)

// shape is the only form in which a tag is kept.
type shape struct {
	types typeSet
	count countRule
	n     uint32
}

// allowed is the tag allowlist: everything else is removed. A tag kept on its name
// alone could smuggle anything, so each entry also fixes the types and element
// counts it may have, and a value outside them makes the file malformed.
//
// Orientation and the ICC profile are absent because Options.Strict decides their
// fate; see buildPage.
//
// T4Options, T6Options and YCbCrCoefficients change how samples decode, and InkSet,
// NumberOfInks and DotRange change what they mean, so dropping one that is not at
// its default would damage the picture even though none of them names anyone.
var allowed = map[uint16]shape{
	tagNewSubfileType:  {integers, countOne, 0},
	tagSubfileType:     {integers, countOne, 0},
	tagImageWidth:      {integers, countOne, 0},
	tagImageLength:     {integers, countOne, 0},
	tagBitsPerSample:   {integers, countPerSample, 0},
	tagCompression:     {integers, countOne, 0},
	tagPhotometric:     {integers, countOne, 0},
	tagThresholding:    {integers, countOne, 0},
	tagFillOrder:       {integers, countOne, 0},
	tagStripOffsets:    {integers, countUnbounded, 0},
	tagSamplesPerPixel: {integers, countOne, 0},
	tagRowsPerStrip:    {integers, countOne, 0},
	tagStripByteCounts: {integers, countUnbounded, 0},
	tagMinSampleValue:  {integers, countPerSample, 0},
	tagMaxSampleValue:  {integers, countPerSample, 0},
	tagXResolution:     {rationals, countOne, 0},
	tagYResolution:     {rationals, countOne, 0},
	tagPlanarConfig:    {integers, countOne, 0},
	tagT4Options:       {integers, countOne, 0},
	tagT6Options:       {integers, countOne, 0},
	tagResolutionUnit:  {integers, countOne, 0},
	tagPredictor:       {integers, countOne, 0},
	tagWhitePoint:      {rationals, countFixed, 2},
	tagPrimaries:       {rationals, countFixed, 6},
	tagColorMap:        {integers, countColorMap, 0},
	tagTileWidth:       {integers, countOne, 0},
	tagTileLength:      {integers, countOne, 0},
	tagTileOffsets:     {integers, countUnbounded, 0},
	tagTileByteCounts:  {integers, countUnbounded, 0},
	tagInkSet:          {integers, countOne, 0},
	tagNumberOfInks:    {integers, countOne, 0},
	tagDotRange:        {integers, countDotRange, 0},
	tagExtraSamples:    {integers, countUpToSamples, 0},
	tagSampleFormat:    {integers, countPerSample, 0},
	tagSMinSampleValue: {anyNumber, countPerSample, 0},
	tagSMaxSampleValue: {anyNumber, countPerSample, 0},
	tagJPEGTables:      {rawBytes, countUnbounded, 0},
	tagYCbCrCoeffs:     {rationals, countFixed, 3},
	tagYCbCrSubSamp:    {integers, countFixed, 2},
	tagYCbCrPosition:   {integers, countOne, 0},
	tagRefBlackWhite:   {rationals, countFixed, 6},
}

// geoTags are the GeoTIFF tags. They pin an image to a place on the map, so they
// are reported together as location rather than in the catch-all count.
var geoTags = map[uint16]bool{
	0x830E: true, // ModelPixelScale
	0x8482: true, // ModelTiepoint
	0x85D8: true, // ModelTransformation
	0x87AF: true, // GeoKeyDirectory
	0x87B0: true, // GeoDoubleParams
	0x87B1: true, // GeoAsciiParams
}
