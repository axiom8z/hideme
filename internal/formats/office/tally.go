package office

import (
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// fieldSpec is everything about a finding except its value and location, so the
// fields can be declared once and read as a table.
type fieldSpec struct {
	field    string
	category core.Category
	risk     core.Risk
	action   core.Action
}

// maxValuesPerField bounds the report for one part. A hostile document can give
// every tracked change a different author; the user only needs to see that names
// were there, and the report must not grow with the file.
const maxValuesPerField = 20

// tally gathers one part's findings. A real document has thousands of rsid
// attributes and dozens of edits by one author, which belong on one line each.
type tally struct {
	where    string
	entries  []*tallyEntry
	seen     map[tallyKey]*tallyEntry
	distinct map[string]int
}

type tallyKey struct{ field, value string }

type tallyEntry struct {
	spec  fieldSpec
	value string
	count int // non-zero for fields reported as a count instead of a value
}

func newTally(where string) *tally {
	return &tally{where: where, seen: map[tallyKey]*tallyEntry{}, distinct: map[string]int{}}
}

// value records one decoded value of a field.
func (t *tally) value(spec fieldSpec, value string) {
	key := tallyKey{spec.field, value}
	if _, ok := t.seen[key]; ok || t.distinct[spec.field] >= maxValuesPerField {
		return
	}
	t.distinct[spec.field]++
	entry := &tallyEntry{spec: spec, value: value}
	t.seen[key] = entry
	t.entries = append(t.entries, entry)
}

// count records one more occurrence of a field whose values mean nothing to a
// reader, such as session identifiers.
func (t *tally) count(spec fieldSpec) {
	key := tallyKey{spec.field, ""}
	entry, ok := t.seen[key]
	if !ok {
		entry = &tallyEntry{spec: spec}
		t.seen[key] = entry
		t.entries = append(t.entries, entry)
	}
	entry.count++
}

// flush moves the findings to the report in the order they were first seen.
func (t *tally) flush(rep *core.Report) {
	for _, e := range t.entries {
		value := e.value
		if e.count > 0 {
			value = fmt.Sprintf("%d found", e.count)
		}
		rep.Add(core.Finding{
			Category: e.spec.category,
			Risk:     e.spec.risk,
			Field:    e.spec.field,
			Value:    value,
			Where:    t.where,
			Action:   e.spec.action,
		})
	}
	t.entries, t.seen, t.distinct = nil, map[tallyKey]*tallyEntry{}, map[string]int{}
}
