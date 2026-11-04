package matroska

import "github.com/axiom8z/hideme/internal/core"

// Element IDs from RFC 8794 (EBML) and RFC 9559 (Matroska).
const (
	idEBML               = 0x1A45DFA3
	idEBMLVersion        = 0x4286
	idEBMLReadVersion    = 0x42F7
	idEBMLMaxIDLength    = 0x42F2
	idEBMLMaxSizeLength  = 0x42F3
	idDocType            = 0x4282
	idDocTypeVersion     = 0x4287
	idDocTypeReadVersion = 0x4285
	idVoid               = 0xEC
	idCRC32              = 0xBF

	idSegment     = 0x18538067
	idSeekHead    = 0x114D9B74
	idInfo        = 0x1549A966
	idCluster     = 0x1F43B675
	idTracks      = 0x1654AE6B
	idCues        = 0x1C53BB6B
	idAttachments = 0x1941A469
	idChapters    = 0x1043A770
	idTags        = 0x1254C367

	idSeek         = 0x4DBB
	idSeekID       = 0x53AB
	idSeekPosition = 0x53AC

	idTrackEntry           = 0xAE
	idTrackNumber          = 0xD7
	idTrackType            = 0x83
	idCodecID              = 0x86
	idContentEncodings     = 0x6D80
	idContentEncoding      = 0x6240
	idContentEncodingScope = 0x5032

	idSimpleBlock = 0xA3

	idTag             = 0x7373
	idSimpleTag       = 0x67C8
	idTagName         = 0x45A3
	idTagString       = 0x4487
	idTagBinary       = 0x4485
	idAttachedFile    = 0x61A7
	idFileDescription = 0x467E
	idFileName        = 0x466E
	idFileMediaType   = 0x4660
	idFileData        = 0x465C
	idEditionEntry    = 0x45B9
	idChapterAtom     = 0xB6
	idChapterDisplay  = 0x80
	idChapString      = 0x85
)

// Matroska track types the handler can vouch for. Other types, such as the
// metadata type 0x21, can carry a timed log inside the clusters, which this
// handler copies without reading.
const (
	trackVideo    = 1
	trackAudio    = 2
	trackSubtitle = 0x11
)

const (
	// contentEncodingEncryption is the ContentEncodingType value for encryption.
	contentEncodingEncryption = 1
	// contentScopePrivate is the ContentEncodingScope bit for a track's private data.
	contentScopePrivate = 2
)

// Highest versions of the EBML header's fields this parser understands. A file
// demanding more may use elements the allowlists would wrongly void.
const (
	maxEBMLReadVersion    = 1
	maxDocTypeReadVersion = 4
)

// Largest payloads the allowlists accept: EBML limits integers and floats to
// eight bytes, and the short strings are codec names and language codes. Without
// a limit, a kept element would be a place to hide anything.
const (
	maxScalarSize      = 8
	maxShortStringSize = 64
	// maxLanguageCode and maxLanguageSubtag are the longest primary language code in
	// use and the longest later subtag BCP 47 allows.
	maxLanguageCode   = 3
	maxLanguageSubtag = 8
)

type ruleKind uint8

const (
	ruleKeep   ruleKind = iota // copied unchanged
	ruleMaster                 // walked, with its children held to their own allowlist
	ruleBlank                  // payload overwritten with zeros: mandatory, so it cannot be voided
	ruleRemove                 // replaced with a Void element
	ruleVisit                  // handled by its visit function
)

type decoding uint8

const (
	decodeSize decoding = iota
	decodeText
	decodeHex
	decodeDate
)

// field describes how a metadata element appears in the report.
type field struct {
	name     string
	category core.Category
	risk     core.Risk
	decode   decoding
}

// rule says what happens to one child of a master. Anything a table does not
// list is voided and reported, so metadata elements added to the format later are
// removed too.
//
// The tables form a tree with no cycles, which bounds how deep a walk can go
// however deeply a hostile file nests its elements.
type rule struct {
	kind     ruleKind
	maxSize  int64 // for ruleKeep; zero means any size
	children map[uint32]rule
	visit    func(p *plan, e element, where string) error
	field    field
}

var (
	scalar      = rule{kind: ruleKeep, maxSize: maxScalarSize}
	shortString = rule{kind: ruleKeep, maxSize: maxShortStringSize}
	language    = rule{kind: ruleVisit, visit: (*plan).language}
	// opaque keeps data that decoders need and that has no layout to hold it to.
	opaque = rule{kind: ruleVisit, visit: (*plan).opaque}
)

func master(name string, children map[uint32]rule) rule {
	return rule{kind: ruleMaster, children: children, field: field{name: name}}
}

func remove(name string, category core.Category, risk core.Risk, decode decoding) rule {
	return rule{kind: ruleRemove, field: field{name, category, risk, decode}}
}

func blank(name string, category core.Category, risk core.Risk) rule {
	return rule{kind: ruleBlank, field: field{name, category, risk, decodeText}}
}

var ebmlHeaderRules = map[uint32]rule{
	idEBMLVersion:        scalar,
	idEBMLReadVersion:    scalar,
	idEBMLMaxIDLength:    scalar,
	idEBMLMaxSizeLength:  scalar,
	idDocType:            shortString,
	idDocTypeVersion:     scalar,
	idDocTypeReadVersion: scalar,
}

// segmentRules covers the children of Segment that have no special walk: cluster,
// SeekHead, Void and CRC-32 are handled by plan.segment itself.
var segmentRules = map[uint32]rule{
	idInfo:        master("Info", infoRules),
	idTracks:      master("Tracks", tracksRules),
	idCues:        master("Cues", cuesRules),
	idTags:        {kind: ruleVisit, visit: (*plan).removeTags},
	idAttachments: {kind: ruleVisit, visit: (*plan).removeAttachments},
	idChapters:    {kind: ruleVisit, visit: (*plan).removeChapters},
}

var infoRules = map[uint32]rule{
	0x2AD7B1: scalar, // TimestampScale
	0x4489:   scalar, // Duration

	// MuxingApp and WritingApp are mandatory, so they stay as empty strings.
	0x4D80: blank("Muxing application", core.CatSoftware, core.RiskLow),
	0x5741: blank("Writing application", core.CatSoftware, core.RiskLow),

	0x7BA9:   remove("Title", core.CatContent, core.RiskLow, decodeText),
	0x4461:   remove("Creation date", core.CatTime, core.RiskMed, decodeDate),
	0x7384:   remove("Segment file name", core.CatIdentity, core.RiskMed, decodeText),
	0x3C83AB: remove("Previous file name", core.CatIdentity, core.RiskMed, decodeText),
	0x3E83BB: remove("Next file name", core.CatIdentity, core.RiskMed, decodeText),

	// These identifiers tie this file to its other versions and related files.
	0x73A4:   remove("Segment UID", core.CatHidden, core.RiskMed, decodeHex),
	0x3CB923: remove("Previous segment UID", core.CatHidden, core.RiskMed, decodeHex),
	0x3EB923: remove("Next segment UID", core.CatHidden, core.RiskMed, decodeHex),
	0x4444:   remove("Segment family", core.CatHidden, core.RiskMed, decodeHex),
	0x6924:   remove("Chapter translation", core.CatHidden, core.RiskLow, decodeSize),
}

var seekHeadRules = map[uint32]rule{
	idSeek: {kind: ruleVisit, visit: (*plan).seek},
}

var seekRules = map[uint32]rule{
	idSeekID:       {kind: ruleKeep, maxSize: maxIDWidth},
	idSeekPosition: scalar,
}

var tracksRules = map[uint32]rule{
	idTrackEntry: {kind: ruleVisit, visit: (*plan).track},
}

// trackEntryRules leaves out what is free text about a track or that refers to
// something removed: CodecName, CodecSettings, CodecInfoURL, CodecDownloadURL,
// AttachmentLink, TrackTranslate and the DivX trick-track links, which carry
// segment identifiers. All of them are voided and reported as unknown elements.
var trackEntryRules = map[uint32]rule{
	idTrackNumber: scalar,
	0x73C5:        scalar, // TrackUID
	idTrackType:   scalar,
	0xB9:          scalar,   // FlagEnabled
	0x88:          scalar,   // FlagDefault
	0x55AA:        scalar,   // FlagForced
	0x55AB:        scalar,   // FlagHearingImpaired
	0x55AC:        scalar,   // FlagVisualImpaired
	0x55AD:        scalar,   // FlagTextDescriptions
	0x55AE:        scalar,   // FlagOriginal
	0x55AF:        scalar,   // FlagCommentary
	0x9C:          scalar,   // FlagLacing
	0x6DE7:        scalar,   // MinCache
	0x6DF8:        scalar,   // MaxCache
	0x23E383:      scalar,   // DefaultDuration
	0x234E7A:      scalar,   // DefaultDecodedFieldDuration
	0x23314F:      scalar,   // TrackTimestampScale
	0x537F:        scalar,   // TrackOffset
	0x55EE:        scalar,   // MaxBlockAdditionID
	0x22B59C:      language, // Language
	0x22B59D:      language, // LanguageBCP47
	idCodecID:     {kind: ruleVisit, visit: (*plan).codecID},
	0x63A2:        {kind: ruleVisit, visit: (*plan).codecPrivate},
	0xAA:          scalar, // CodecDecodeAll
	0x6FAB:        scalar, // TrackOverlay
	0x56AA:        scalar, // CodecDelay
	0x56BB:        scalar, // SeekPreRoll
	0xE0:          master("Video", videoRules),
	0xE1:          master("Audio", audioRules),
	0xE2:          master("TrackOperation", trackOperationRules),
	0x41E4:        master("BlockAdditionMapping", blockAdditionMappingRules),

	idContentEncodings: master("ContentEncodings", contentEncodingsRules),

	0x536E: blank("Track name", core.CatContent, core.RiskLow),
}

var blockAdditionMappingRules = map[uint32]rule{
	0x41F0: scalar, // BlockAddIDValue
	0x41E7: scalar, // BlockAddIDType
	0x41ED: opaque, // BlockAddIDExtraData
}

var videoRules = map[uint32]rule{
	0x9A:     scalar, // FlagInterlaced
	0x9D:     scalar, // FieldOrder
	0x53B8:   scalar, // StereoMode
	0x53C0:   scalar, // AlphaMode
	0x53B9:   scalar, // OldStereoMode
	0xB0:     scalar, // PixelWidth
	0xBA:     scalar, // PixelHeight
	0x54AA:   scalar, // PixelCropBottom
	0x54BB:   scalar, // PixelCropTop
	0x54CC:   scalar, // PixelCropLeft
	0x54DD:   scalar, // PixelCropRight
	0x54B0:   scalar, // DisplayWidth
	0x54BA:   scalar, // DisplayHeight
	0x54B2:   scalar, // DisplayUnit
	0x54B3:   scalar, // AspectRatioType
	0x2EB524: scalar, // UncompressedFourCC
	0x2FB523: scalar, // GammaValue
	0x2383E3: scalar, // FrameRate
	0x55B0:   master("Colour", colourRules),
	0x7670:   master("Projection", projectionRules),
}

var colourRules = map[uint32]rule{
	0x55B1: scalar, // MatrixCoefficients
	0x55B2: scalar, // BitsPerChannel
	0x55B3: scalar, // ChromaSubsamplingHorz
	0x55B4: scalar, // ChromaSubsamplingVert
	0x55B5: scalar, // CbSubsamplingHorz
	0x55B6: scalar, // CbSubsamplingVert
	0x55B7: scalar, // ChromaSitingHorz
	0x55B8: scalar, // ChromaSitingVert
	0x55B9: scalar, // Range
	0x55BA: scalar, // TransferCharacteristics
	0x55BB: scalar, // Primaries
	0x55BC: scalar, // MaxCLL
	0x55BD: scalar, // MaxFALL
	0x55D0: master("MasteringMetadata", masteringRules),
}

var masteringRules = map[uint32]rule{
	0x55D1: scalar, // PrimaryRChromaticityX
	0x55D2: scalar, // PrimaryRChromaticityY
	0x55D3: scalar, // PrimaryGChromaticityX
	0x55D4: scalar, // PrimaryGChromaticityY
	0x55D5: scalar, // PrimaryBChromaticityX
	0x55D6: scalar, // PrimaryBChromaticityY
	0x55D7: scalar, // WhitePointChromaticityX
	0x55D8: scalar, // WhitePointChromaticityY
	0x55D9: scalar, // LuminanceMax
	0x55DA: scalar, // LuminanceMin
}

var projectionRules = map[uint32]rule{
	0x7671: scalar, // ProjectionType
	0x7672: opaque, // ProjectionPrivate
	0x7673: scalar, // ProjectionPoseYaw
	0x7674: scalar, // ProjectionPosePitch
	0x7675: scalar, // ProjectionPoseRoll
}

var audioRules = map[uint32]rule{
	0xB5:   scalar, // SamplingFrequency
	0x78B5: scalar, // OutputSamplingFrequency
	0x9F:   scalar, // Channels
	0x6264: scalar, // BitDepth
	0x52F1: scalar, // Emphasis
}

var trackOperationRules = map[uint32]rule{
	0xE3: master("TrackCombinePlanes", map[uint32]rule{
		0xE4: master("TrackPlane", map[uint32]rule{
			0xE5: scalar, // TrackPlaneUID
			0xE6: scalar, // TrackPlaneType
		}),
	}),
	0xE9: master("TrackJoinBlocks", map[uint32]rule{
		0xED: scalar, // TrackJoinUID
	}),
}

var contentEncodingsRules = map[uint32]rule{
	idContentEncoding: master("ContentEncoding", map[uint32]rule{
		0x5031:                 scalar, // ContentEncodingOrder
		idContentEncodingScope: scalar,
		0x5033:                 {kind: ruleVisit, visit: (*plan).contentEncodingType},
		0x5034: master("ContentCompression", map[uint32]rule{
			0x4254: scalar, // ContentCompAlgo
			0x4255: opaque, // ContentCompSettings
		}),
		0x5035: {kind: ruleVisit, visit: (*plan).contentEncryption},
	}),
}

var cuesRules = map[uint32]rule{
	0xBB: master("CuePoint", map[uint32]rule{
		0xB3: scalar, // CueTime
		0xB7: master("CueTrackPositions", map[uint32]rule{
			0xF7:   scalar, // CueTrack
			0xF1:   scalar, // CueClusterPosition
			0xF0:   scalar, // CueRelativePosition
			0xB2:   scalar, // CueDuration
			0x5378: scalar, // CueBlockNumber
			0xEA:   scalar, // CueCodecState
			0xDB: master("CueReference", map[uint32]rule{
				0x96:   scalar, // CueRefTime
				0x97:   scalar, // CueRefCluster
				0x535F: scalar, // CueRefNumber
				0xEB:   scalar, // CueRefCodecState
			}),
		}),
	}),
}

// clusterChildren are the IDs that can occur inside a cluster. A walk through an
// unknown-size cluster ends at the first other ID, which then gets judged at
// segment level, where an unrecognised element is voided rather than trusted.
var clusterChildren = map[uint32]bool{
	0xE7:          true, // Timestamp
	0x5854:        true, // SilentTracks
	0xA7:          true, // Position
	0xAB:          true, // PrevSize
	idSimpleBlock: true,
	0xA0:          true, // BlockGroup
	0xAF:          true, // EncryptedBlock
	idVoid:        true,
	idCRC32:       true,
}

var misplaced = rule{kind: ruleVisit, visit: (*plan).misplaced}

// clusterRules holds everything but a block's payload to an allowlist. Reading it
// costs one small read per block, so even a file of many gigabytes, with millions of
// blocks, takes seconds, and nothing is held in memory.
var clusterRules = map[uint32]rule{
	0xE7: scalar, // Timestamp
	0xA7: scalar, // Position
	0xAB: scalar, // PrevSize
	0x5854: master("SilentTracks", map[uint32]rule{
		0x58D7: scalar, // SilentTrackNumber
	}),
	idSimpleBlock: {kind: ruleVisit, visit: (*plan).block},
	0xA0:          master("BlockGroup", blockGroupRules),
	0xAF:          {kind: ruleVisit, visit: (*plan).encryptedBlock},

	idEBML:        misplaced,
	idSegment:     misplaced,
	idSeekHead:    misplaced,
	idInfo:        misplaced,
	idCluster:     misplaced,
	idTracks:      misplaced,
	idCues:        misplaced,
	idAttachments: misplaced,
	idChapters:    misplaced,
	idTags:        misplaced,
}

var blockGroupRules = map[uint32]rule{
	0xA1:   {kind: ruleVisit, visit: (*plan).block},
	0x9B:   scalar, // BlockDuration
	0xFA:   scalar, // ReferencePriority
	0xFB:   scalar, // ReferenceBlock
	0xFD:   scalar, // ReferenceVirtual
	0x75A2: scalar, // DiscardPadding
	0xA4:   opaque, // CodecState
	0x75A1: master("BlockAdditions", map[uint32]rule{
		0xA6: master("BlockMore", map[uint32]rule{
			0xEE: scalar, // BlockAddID
			0xA5: {kind: ruleVisit, visit: (*plan).blockAdditional},
		}),
	}),
	0x8E: master("Slices", map[uint32]rule{
		0xE8: master("TimeSlice", map[uint32]rule{
			0xCC: scalar, // LaceNumber
			0xCD: scalar, // FrameNumber
			0xCB: scalar, // BlockAdditionID
			0xCE: scalar, // Delay
			0xCF: scalar, // SliceDuration
		}),
	}),
}
