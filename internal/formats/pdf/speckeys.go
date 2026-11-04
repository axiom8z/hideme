package pdf

// specKeys is every dictionary key the PDF specification defines for the objects
// that have no allowlist of their own. A key outside it was put there by a producer.
// Such a key cannot be removed, because a renderer may depend on anything on objects
// like these, but it can be noticed, which is all this is for: a key missing here
// costs a warning and nothing else. Keys of what is always removed, such as a
// signature's or /Info's, are left out on purpose. If one is still here after the
// removal rules, nothing explains it.
var specKeys = keySet(
	// Streams, and every dictionary's own type.
	"Type", "Subtype", "Length", "Filter", "DecodeParms", "F", "FFilter", "FDecodeParms", "DL", "DP",
	"Length1", "Length2", "Length3", "Predictor", "Colors", "Columns", "EarlyChange", "EndOfLine", "EncodedByteAlign",
	"Rows", "EndOfBlock", "BlackIs1", "DamagedRowsBeforeError", "JBIG2Globals", "ColorTransform",

	// Annotations.
	"Rect", "Contents", "P", "NM", "M", "AP", "AS", "Border", "C", "StructParent", "OC", "ca", "CA", "BM",
	"Lang", "H", "BS", "BE", "A", "AA", "Dest", "PA", "QuadPoints", "T", "Popup", "RC",
	"IRT", "Subj", "RT", "IT", "ExData", "Open", "State", "StateModel", "Name", "DA", "Q", "DS", "LE", "CL",
	"RD", "L", "LL", "LLE", "LLO", "LN", "Cap", "CO", "CP", "Measure", "IC", "Vertices", "PS", "Path",
	"InkList", "Movie", "Sound", "FS", "Parent", "MK", "R", "BG", "BC", "AC", "I", "RI", "IX", "TP", "IF",
	"SW", "S", "FB", "RO", "OverlayText", "Repeat", "MN", "FixedPrint", "Sy", "U", "V", "N", "D", "Mode",
	"AnnotStates", "FontFauxing", "Version", "Rotate", "TI", "BleedBox", "TrimBox", "ArtBox", "CropBox",
	"MediaBox", "Trans", "Dur", "Annotation", "Operation", "Start", "Duration", "Rate", "Volume",
	"ShowControls", "Synchronous", "TF", "Mix", "Aspect", "Poster",

	// 3D and rich media.
	"3DD", "3DV", "3DA", "3DI", "3DB", "VA", "DV", "MS", "C2W", "U2L", "IN", "XN", "SA", "NP", "RM", "CV",
	"RichMediaSettings", "RichMediaContent", "Assets", "Configurations", "Views", "Activation", "Deactivation",
	"Instances", "Params", "Presentation", "Condition", "Binding", "FlashVars", "Style", "Transparent",
	"NavigationPane", "PassContext", "Toolbar", "Window", "Position", "Scripts", "Animation", "Scene",
	"Asset", "CMD", "TA", "LS", "PO", "OB", "CMPR", "O", "Nodes", "Limits", "HAlign", "VAlign", "HOffset",
	"VOffset", "Width", "Height", "Default", "Max", "Min",

	// Forms and signatures.
	"Fields", "NeedAppearances", "SigFlags", "DR", "XFA", "FT", "Kids", "TU", "TM", "Ff", "MaxLen", "Opt",
	"RV", "Lock", "SV", "SubFilter", "DigestMethod", "Reasons", "MDP", "TimeStamp", "LegalAttestation",
	"AddRevInfo", "URL", "URLType", "Ft", "Cert", "SubjectDN", "KeyUsage", "Issuer", "OID", "Action", "Flags",
	"Barcode",

	// Actions, where they have no /S to be recognised by.
	"Next", "SD", "NewWindow", "Win", "Mac", "Unix", "IsMap", "URI", "B", "JS", "PreserveRB", "AN", "OP", "Dp",

	// Graphics state, transparency, colour, patterns, shadings, functions.
	"LW", "LC", "LJ", "ML", "OPM", "op", "Font", "BG2", "UCR", "UCR2", "TR", "TR2", "HT", "FL", "SM", "SMask",
	"AIS", "TK", "UseBlackPtComp", "HTO", "G", "CS", "K", "WhitePoint", "BlackPoint", "Gamma", "Matrix", "Range",
	"Alternate", "Colorants", "Process", "MixingHints", "Components", "Solidities", "DotGain", "PrintingOrder",
	"ColorSpace", "ColorantName", "PatternType", "PaintType", "TilingType", "BBox", "XStep", "YStep", "Resources",
	"Shading", "ExtGState", "ShadingType", "Background", "AntiAlias", "Domain", "Function", "Coords", "Extend",
	"BitsPerCoordinate", "BitsPerComponent", "BitsPerFlag", "Decode", "VerticesPerRow", "FunctionType", "Size",
	"BitsPerSample", "Order", "Encode", "C0", "C1", "Functions", "Bounds", "HalftoneType", "HalftoneName",
	"Frequency", "Angle", "SpotFunction", "AccurateScreens", "TransferFunction", "Width2", "Height2", "Xsquare",
	"Ysquare", "Pattern", "XObject", "ProcSet", "Properties", "Group", "BoxColorInfo", "W",

	// Fonts, encodings and CMaps, where they have no /Type to be recognised by.
	"BaseFont", "FirstChar", "LastChar", "Widths", "FontDescriptor", "Encoding", "ToUnicode", "FontBBox",
	"FontMatrix", "CharProcs", "DescendantFonts", "CIDSystemInfo", "DW", "DW2", "W2", "CIDToGIDMap", "BaseEncoding",
	"Differences", "CMapName", "WMode", "UseCMap", "Registry", "Ordering", "Supplement", "FontName", "FontFamily",
	"FontStretch", "FontWeight", "ItalicAngle", "Ascent", "Descent", "Leading", "CapHeight", "XHeight", "StemV",
	"StemH", "AvgWidth", "MaxWidth", "MissingWidth", "FontFile", "FontFile2", "FontFile3", "CharSet", "FD", "CIDSet",
	"Panose",

	// Outlines, threads, structure and name trees.
	"Title", "Prev", "First", "Last", "Count", "SE", "Pg", "ID", "Alt", "E", "ActualText", "RoleMap", "ClassMap",
	"IDTree", "ParentTree", "ParentTreeNextKey", "MCID", "Stm", "StmOwn", "Obj", "Attr", "Namespaces",
	"PronunciationLexicon", "Ref", "NS", "Schema", "RoleMapNS", "Names", "Nums", "Dests", "Pages", "Templates",
	"AlternatePresentations", "Renditions", "JavaScript", "EmbeddedFiles",

	// Standard structure attributes.
	"Placement", "WritingMode", "BackgroundColor", "BorderColor", "BorderStyle", "BorderThickness", "Color", "Padding",
	"SpaceBefore", "SpaceAfter", "StartIndent", "EndIndent", "TextIndent", "TextAlign", "BlockAlign", "InlineAlign",
	"TBorderStyle", "TPadding", "BaselineShift", "LineHeight", "TextDecorationColor", "TextDecorationThickness",
	"TextDecorationType", "RubyAlign", "RubyPosition", "GlyphOrientationVertical", "ColumnCount", "ColumnGap",
	"ColumnWidths", "ListNumbering", "Role", "Checked", "Desc", "RowSpan", "ColSpan", "Headers", "Scope", "Summary",
	"Short",

	// Catalog-level dictionaries: viewer preferences, labels, marking, layers, extensions.
	"HideToolbar", "HideMenubar", "HideWindowUI", "FitWindow", "CenterWindow", "DisplayDocTitle",
	"NonFullScreenPageMode", "Direction", "ViewArea", "ViewClip", "PrintArea", "PrintClip", "PrintScaling", "Duplex",
	"PickTrayByPDFSize", "PrintPageRange", "NumCopies", "Enforce", "St", "Marked", "UserProperties", "Suspects",
	"Intent", "Usage", "Language", "Export", "Zoom", "Print", "View", "PageElement", "OCGs", "VE", "Configs", "BaseState",
	"ON", "OFF", "ListMode", "RBGroups", "Locked", "Event", "Category", "Preferred", "PrintState", "ViewState",
	"ExportState", "ADBE", "BaseVersion", "ExtensionLevel", "ExtensionRevision", "RH", "OutputCondition",
	"OutputConditionIdentifier", "RegistryName", "Info", "DestOutputProfile", "DestOutputProfileRef",

	// Page furniture: transitions, beads, measures, viewports, separations, parts.
	"Dm", "Di", "SS", "GPTS", "GCS", "DCS", "PDU", "Bounds", "X", "Y", "NA", "PD", "LA", "FD", "RM", "SY", "SS",
	"XPTS", "Image", "DefaultForPrinting", "DParts", "End", "RecordLevel", "Node", "DeviceColorant",

	// File specifications.
	"UF", "DOS", "CI", "Cl", "Size",

	// Media clips and renditions.
	"Clip", "MH", "SP", "Z", "PL", "CT", "TT", "MU", "PID", "LI", "PV", "RC", "SV", "BU", "OS", "MCD",
)
