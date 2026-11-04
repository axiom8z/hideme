package svg

// svgElements lists the elements of SVG 1.1, SVG Tiny 1.2 and SVG 2, plus the drafts
// that Inkscape writes and renders: flowed text, mesh gradients and hatches. No
// renderer draws an element it does not know, so anything else can only be data for
// someone reading the file. Other tools leave plenty there.
var svgElements = setOf(
	"a", "altGlyph", "altGlyphDef", "altGlyphItem", "animate", "animateColor", "animateMotion", "animateTransform",
	"animation", "audio", "canvas", "circle", "clipPath", "color-profile", "cursor", "defs", "desc", "discard", "ellipse",
	"feBlend", "feColorMatrix", "feComponentTransfer", "feComposite", "feConvolveMatrix", "feDiffuseLighting",
	"feDisplacementMap", "feDistantLight", "feDropShadow", "feFlood", "feFuncA", "feFuncB", "feFuncG", "feFuncR",
	"feGaussianBlur", "feImage", "feMerge", "feMergeNode", "feMorphology", "feOffset", "fePointLight",
	"feSpecularLighting", "feSpotLight", "feTile", "feTurbulence", "filter",
	"flowDiv", "flowImage", "flowLine", "flowPara", "flowRef", "flowRegion", "flowRegionBreak", "flowRegionExclude",
	"flowRoot", "flowSpan", "flowTref",
	"font", "font-face", "font-face-format", "font-face-name", "font-face-src", "font-face-uri", "foreignObject",
	"g", "glyph", "glyphRef", "handler", "hatch", "hatchPath", "hatchpath", "hkern", "iframe", "image", "line",
	"linearGradient", "listener", "marker", "mask", "mesh", "meshGradient", "meshgradient", "meshPatch", "meshpatch",
	"meshRow", "meshrow", "metadata", "missing-glyph", "mpath", "page", "pageSet", "path", "pattern", "polygon",
	"polyline", "prefetch", "radialGradient", "rect", "script", "set", "solidColor", "solidcolor", "source", "stop",
	"style", "svg", "switch", "symbol", "tbreak", "text", "textArea", "textPath", "title", "track", "tref", "tspan",
	"unknown", "use", "video", "view", "vkern",
)

// textElements draw or otherwise use the characters directly inside them. Text in any
// other element is never drawn. An a or switch element takes after its parent, since
// each can stand inside a text element but draws nothing by itself elsewhere.
var textElements = setOf(
	"altGlyph", "desc", "flowDiv", "flowLine", "flowPara", "flowRoot", "flowSpan", "flowTref", "foreignObject",
	"glyphRef", "handler", "script", "style", "tbreak", "text", "textArea", "textPath", "title", "tref", "tspan",
)

// svgAttributes lists the attributes of the same standards, and the CSS properties that
// are allowed as attributes. Renderers ignore all others, and they are where tools put
// things: draw.io, for one, writes the whole diagram, the browser and the time in
// attributes of the root. Attributes starting with "on" and the "data-" ones are handled
// separately.
var svgAttributes = setOf(
	// Accessibility.
	"aria-activedescendant", "aria-atomic", "aria-autocomplete", "aria-busy", "aria-checked", "aria-colcount", "aria-colindex",
	"aria-colspan", "aria-controls", "aria-current", "aria-describedby", "aria-description", "aria-details", "aria-disabled",
	"aria-dropeffect", "aria-errormessage", "aria-expanded", "aria-flowto", "aria-grabbed", "aria-haspopup", "aria-hidden",
	"aria-invalid", "aria-keyshortcuts", "aria-label", "aria-labelledby", "aria-level", "aria-live", "aria-modal",
	"aria-multiline", "aria-multiselectable", "aria-orientation", "aria-owns", "aria-placeholder", "aria-posinset",
	"aria-pressed", "aria-readonly", "aria-relevant", "aria-required", "aria-roledescription", "aria-rowcount",
	"aria-rowindex", "aria-rowspan", "aria-selected", "aria-setsize", "aria-sort", "aria-valuemax", "aria-valuemin",
	"aria-valuenow", "aria-valuetext",

	// Core, and what renderers read from style sheets.
	"class", "id", "lang", "style", "tabindex", "role", "focusable", "focusHighlight",
	"requiredExtensions", "requiredFeatures", "requiredFonts", "requiredFormats", "systemLanguage",
	"externalResourcesRequired", "version", "baseProfile", "contentScriptType", "contentStyleType", "zoomAndPan",
	"playbackorder", "timelinebegin", "timelineBegin", "snapshotTime", "syncBehavior", "syncBehaviorDefault",
	"syncMaster", "syncTolerance", "syncToleranceDefault", "transformBehavior", "overlay", "initialVisibility", "editable",
	"nav-down", "nav-down-left", "nav-down-right", "nav-left", "nav-next", "nav-prev", "nav-right", "nav-up", "nav-up-left",
	"nav-up-right",

	// Geometry and layout.
	"x", "y", "z", "x1", "x2", "y1", "y2", "cx", "cy", "r", "rx", "ry", "fx", "fy", "fr", "dx", "dy", "d", "points", "path",
	"pathLength", "width", "height", "viewBox", "viewTarget", "preserveAspectRatio", "transform", "transform-origin",
	"transform-box", "gradientTransform", "patternTransform", "rotate", "side", "orient", "refX", "refY", "markerHeight",
	"markerUnits", "markerWidth", "clipPathUnits", "gradientUnits", "maskContentUnits", "maskUnits", "patternContentUnits",
	"patternUnits", "spreadMethod", "offset", "edge", "pitch", "crossorigin", "decoding", "referrerpolicy", "fetchpriority",

	// Links.
	"href", "hreflang", "target", "rel", "download", "ping", "type", "media", "title", "src", "srcdoc", "autoplay", "controls",
	"loop", "muted", "poster", "preload", "sandbox", "allow", "allowfullscreen", "kind", "label", "srclang", "default", "defer",

	// Text and fonts.
	"lengthAdjust", "textLength", "startOffset", "method", "spacing", "format", "glyphRef", "accent-height", "alphabetic",
	"arabic-form", "ascent", "bbox", "cap-height", "descent", "g1", "g2", "glyph-name", "hanging", "horiz-adv-x",
	"horiz-origin-x", "horiz-origin-y", "ideographic", "k", "local", "mathematical", "name", "orientation", "overline-position",
	"overline-thickness", "panose-1", "slope", "stemh", "stemv", "strikethrough-position", "strikethrough-thickness", "string",
	"u1", "u2", "underline-position", "underline-thickness", "unicode", "unicode-range", "units-per-em", "v-alphabetic",
	"v-hanging", "v-ideographic", "v-mathematical", "vert-adv-y", "vert-origin-x", "vert-origin-y", "widths", "x-height",
	"rendering-intent",

	// Filters.
	"amplitude", "azimuth", "baseFrequency", "bias", "diffuseConstant", "divisor", "edgeMode", "elevation", "exponent",
	"filterRes", "filterUnits", "in", "in2", "intercept", "k1", "k2", "k3", "k4", "kernelMatrix", "kernelUnitLength",
	"limitingConeAngle", "mode", "numOctaves", "operator", "order", "pointsAtX", "pointsAtY", "pointsAtZ", "preserveAlpha",
	"primitiveUnits", "radius", "result", "scale", "seed", "specularConstant", "specularExponent", "stdDeviation",
	"stitchTiles", "surfaceScale", "tableValues", "targetX", "targetY", "type", "values", "xChannelSelector", "yChannelSelector",

	// Animation.
	"accumulate", "additive", "attributeName", "attributeType", "begin", "by", "calcMode", "dur", "end", "fill", "from", "keyPoints",
	"keySplines", "keyTimes", "max", "min", "origin", "repeatCount", "repeatDur", "restart", "to",

	// Events, for elements that listen.
	"defaultAction", "event", "handler", "observer", "phase", "propagate",

	// CSS properties allowed as attributes.
	"alignment-baseline", "audio-level", "baseline-shift", "buffered-rendering", "clip", "clip-path", "clip-rule", "color",
	"color-interpolation", "color-interpolation-filters", "color-profile", "color-rendering", "cursor", "direction", "display",
	"display-align", "dominant-baseline", "enable-background", "fill-opacity", "fill-rule", "filter", "flood-color",
	"flood-opacity", "font-family", "font-feature-settings", "font-kerning", "font-size", "font-size-adjust", "font-stretch",
	"font-style", "font-variant", "font-variant-caps", "font-variant-east-asian", "font-variant-ligatures",
	"font-variant-numeric", "font-variant-position", "font-variation-settings", "font-weight", "glyph-orientation-horizontal",
	"glyph-orientation-vertical", "hatch-pitch", "hatch-rotate", "image-rendering", "inline-size", "isolation", "kerning",
	"letter-spacing", "lighting-color", "line-height", "line-increment", "marker-end", "marker-mid", "marker-start", "mask",
	"mask-type", "mix-blend-mode", "opacity", "overflow", "paint-order", "pointer-events", "shape-inside", "shape-margin",
	"shape-padding", "shape-rendering", "shape-subtract", "solid-color", "solid-opacity", "stop-color", "stop-opacity", "stroke",
	"stroke-dasharray", "stroke-dashoffset", "stroke-linecap", "stroke-linejoin", "stroke-miterlimit", "stroke-opacity",
	"stroke-width", "text-align", "text-align-last", "text-anchor", "text-decoration", "text-decoration-color",
	"text-decoration-line", "text-decoration-style", "text-indent", "text-orientation", "text-overflow", "text-rendering",
	"text-transform", "unicode-bidi", "vector-effect", "viewport-fill", "viewport-fill-opacity", "visibility", "white-space",
	"word-spacing", "writing-mode",
)

// Attributes that may stay in the namespaces that keep their prefix.
var (
	xlinkAttributes  = setOf("href", "type", "role", "arcrole", "title", "show", "actuate")
	xmlAttributes    = setOf("lang", "space", "base", "id")
	eventsAttributes = setOf("event", "observer", "target", "handler", "phase", "propagate", "defaultAction")
)

func setOf(values ...string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// attributeOwners limits attributes that hold free text and that the standards define
// on one or two elements. Anywhere else, nothing reads them, and they are a place for a
// name to sit.
var attributeOwners = map[string]map[string]bool{
	"name":       setOf("font-face-name", "color-profile"),
	"title":      setOf("style"),
	"label":      setOf("track"),
	"kind":       setOf("track"),
	"srclang":    setOf("track"),
	"default":    setOf("track"),
	"string":     setOf("font-face-format"),
	"glyph-name": setOf("glyph"),
	"local":      setOf("color-profile"),
	"format":     setOf("altGlyph", "glyphRef"),
}

// textAttributes are shown to the user as a tooltip or read aloud, so they are the same
// kind of thing as a title element, and are reported like one.
var textAttributes = setOf("aria-label", "aria-description", "aria-roledescription", "aria-placeholder", "aria-valuetext")
