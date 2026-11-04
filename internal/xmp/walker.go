package xmp

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	// maxValuesPerProperty and maxValueBytes bound what a hostile packet can make
	// the report hold: one finding per table row, each of limited size.
	maxValuesPerProperty = 16
	maxValueBytes        = 4096

	liElement = "rdf:li"
)

type attribute struct {
	name  string
	value string
}

// capture is a property element being read: its text may be spread over nested
// rdf:Seq, rdf:Alt and rdf:Bag lists.
type capture struct {
	index int
	name  string
	depth int
	text  strings.Builder
}

// walker turns a stream of elements into per-property values. Both the XML
// parser and the tolerant scan drive it, so the two paths cannot disagree about
// what a property means.
type walker struct {
	depth  int
	open   *capture
	values [][]string
	items  []int  // rdf:li count, for kindCount properties
	seen   []bool // whether a property appeared at all, even with no text
	other  int
}

func newWalker() *walker {
	return &walker{
		values: make([][]string, len(properties)),
		items:  make([]int, len(properties)),
		seen:   make([]bool, len(properties)),
	}
}

func (w *walker) start(name string, attrs []attribute) {
	w.depth++
	if w.open != nil {
		// Inside a list property, list items sit two levels down: the property
		// holds an rdf:Seq, which holds the items. A deeper rdf:li belongs to a
		// nested structure and is not another item.
		if name == liElement && w.depth == w.open.depth+2 {
			w.items[w.open.index]++
		}
		return
	}
	index, known := propertyIndex[name]
	if !known && isMetadataName(name) {
		w.other++
	}
	for _, a := range attrs {
		switch i, ok := propertyIndex[a.name]; {
		case ok:
			w.add(i, a.value)
		case known && a.name == "rdf:resource":
			w.add(index, a.value)
		case isMetadataName(a.name):
			w.other++
		}
	}
	if known {
		w.seen[index] = true
		w.open = &capture{index: index, name: name, depth: w.depth}
	}
}

func (w *walker) chars(s string) {
	if w.open == nil || properties[w.open.index].kind == kindCount {
		return
	}
	if room := maxValueBytes - w.open.text.Len(); room > 0 {
		w.open.text.WriteString(s[:min(len(s), room)])
	}
}

func (w *walker) end(name string) {
	w.depth--
	if w.open == nil {
		return
	}
	if name == liElement {
		w.flush()
	}
	// Names alone decide the match because the scan path may have lost count of
	// depth in a damaged packet.
	if name == w.open.name {
		w.flush()
		w.open = nil
	}
}

func (w *walker) flush() {
	value := w.open.text.String()
	w.open.text.Reset()
	w.add(w.open.index, value)
}

func (w *walker) add(i int, value string) {
	value = strings.TrimSpace(value)
	value = value[:min(len(value), maxValueBytes)]
	w.seen[i] = true
	if value == "" || len(w.values[i]) == maxValuesPerProperty || slices.Contains(w.values[i], value) {
		return
	}
	w.values[i] = append(w.values[i], value)
}

// isMetadataName reports whether a name that is not in the table still counts as
// a property. RDF syntax and namespace declarations do not, and nor does an
// unprefixed name, since every XMP property has a namespace.
func isMetadataName(name string) bool {
	prefix, _, found := strings.Cut(name, ":")
	if !found {
		return false
	}
	switch prefix {
	case "rdf", "x", "xml", "xmlns":
		return false
	}
	return true
}

// findings converts what was collected into report entries, in table order.
func (w *walker) findings(where string) []core.Finding {
	var out []core.Finding
	emit := func(p property, value, loc string) {
		out = append(out, core.Finding{
			Category: p.category,
			Risk:     p.risk,
			Field:    p.field,
			Value:    core.SafeText(value),
			Where:    core.SafeText(joinWhere(where, loc)),
			Action:   core.ActionRemove,
		})
	}
	gpsDone := false
	for i, p := range properties {
		if !w.seen[i] {
			continue
		}
		switch p.kind {
		case kindLatitude, kindLongitude:
			if !gpsDone {
				emit(p, w.coordinates(), "exif:GPS")
				gpsDone = true
			}
		case kindCount:
			emit(p, w.historyValue(i), p.name)
		default:
			emit(p, strings.Join(w.values[i], ", "), p.name)
		}
	}
	if w.other > 0 {
		emit(otherProperties, strconv.Itoa(w.other), "")
	}
	return out
}

func (w *walker) historyValue(i int) string {
	switch n := w.items[i]; n {
	case 0:
		return strings.Join(w.values[i], ", ")
	case 1:
		return "1 event"
	default:
		return strconv.Itoa(n) + " events"
	}
}

// coordinates formats the latitude and longitude as decimal degrees. A value
// that does not convert is shown as written: reporting something odd is better
// than hiding that a location is present.
func (w *walker) coordinates() string {
	lat := w.coordinate(propertyIndex["exif:GPSLatitude"])
	lon := w.coordinate(propertyIndex["exif:GPSLongitude"])
	switch {
	case lat != "" && lon != "":
		return lat + ", " + lon
	case lat != "":
		return "latitude " + lat
	case lon != "":
		return "longitude " + lon
	}
	return ""
}

func (w *walker) coordinate(i int) string {
	if len(w.values[i]) == 0 {
		return ""
	}
	raw := w.values[i][0]
	if degrees, ok := parseCoordinate(raw); ok {
		return strconv.FormatFloat(degrees, 'f', 6, 64)
	}
	return raw
}

// parseCoordinate reads the XMP GPSCoordinate forms "DDD,MM.mmk" and
// "DDD,MM,SSk", where k is a compass letter, plus bare decimal degrees, which
// some writers use against the specification.
//
// A sign on the degrees covers the whole value, so "-33,30" is -33.5 and not
// -33 + 0.5, and it does not cancel out a southern or western letter.
func parseCoordinate(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	negative := false
	switch s[len(s)-1] {
	case 'N', 'n', 'E', 'e':
		s = s[:len(s)-1]
	case 'S', 's', 'W', 'w':
		s, negative = s[:len(s)-1], true
	}
	parts := strings.Split(s, ",")
	if len(parts) > 3 {
		return 0, false
	}
	total, unit := 0.0, 1.0
	for i, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		if math.Signbit(v) {
			if i > 0 {
				return 0, false // minutes and seconds have no sign to give
			}
			negative = true
		}
		total += math.Abs(v) / unit
		unit *= 60
	}
	if total > 180 {
		return 0, false
	}
	// The zero check keeps a value of exactly zero from printing as "-0.000000".
	if negative && total != 0 {
		total = -total
	}
	return total, true
}

func joinWhere(where, name string) string {
	if where == "" || name == "" {
		return where + name
	}
	return where + "/" + name
}

func byteCount(n int) string {
	return strconv.Itoa(n) + " bytes"
}
