package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/engine"
)

// textReporter prints one header line per file, then indented detail lines that each
// start with a fixed word, so that "grep GPS" and "grep WARNING" both work. All text
// HideMe itself adds is ASCII.
type textReporter struct {
	w       io.Writer
	notes   *stderrNotes
	command string
	dryRun  bool
	show    display
	color   bool
}

func (t *textReporter) begin() {}

func (t *textReporter) result(raw engine.Result) {
	r := sanitize(raw, t.show)
	t.notes.remember(raw, r)
	switch r.Status {
	case engine.StatusFailed:
		fmt.Fprintf(t.w, "%s: %s (%s): %s\n", r.Path, t.paint(red, "FAILED"), r.Kind, r.Reason)
	case engine.StatusUnsupported:
		fmt.Fprintf(t.w, "%s: %s: %s\n", r.Path, t.paint(yellow, "UNSUPPORTED"), r.Reason)
	case engine.StatusSkipped:
		fmt.Fprintf(t.w, "%s: %s: %s\n", r.Path, t.paint(yellow, "SKIPPED"), r.Reason)
	case engine.StatusInspected:
		fmt.Fprintf(t.w, "%s: %s, %d bytes, %s\n", r.Path, r.Format, r.BytesBefore, t.counts(r, "to remove"))
		t.findings(r)
	default:
		verb := "cleaned"
		if r.Status == engine.StatusWouldClean {
			verb = "would clean"
		}
		fmt.Fprintf(t.w, "%s -> %s: %s, %s, %s, %d -> %d bytes, %s\n",
			r.Path, r.Output, t.paint(green, verb), r.Format, t.counts(r, "removed"),
			r.BytesBefore, r.BytesAfter, r.Elapsed.Round(time.Millisecond))
		if t.show.namesOnly {
			t.fieldNames(r)
		} else {
			t.findings(r)
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(t.w, "  %s: %s\n", t.paint(yellow, "WARNING"), w)
	}
}

func (t *textReporter) counts(r engine.Result, verb string) string {
	if r.NothingFound() {
		return "nothing found"
	}
	var active, kept int
	for _, f := range r.Findings {
		if f.Action == core.ActionRetain {
			kept++
		} else {
			active++
		}
	}
	return fmt.Sprintf("%d %s, %d retained", active, verb, kept)
}

// riskOrder is the order findings are grouped in. Retained items come after every
// group, whatever their risk, because nothing is being done about them.
var riskOrder = []core.Risk{core.RiskHigh, core.RiskMed, core.RiskLow, core.RiskNone}

func (t *textReporter) findings(r engine.Result) {
	for _, risk := range riskOrder {
		for _, f := range r.Findings {
			if f.Risk == risk && f.Action != core.ActionRetain {
				fmt.Fprintf(t.w, "  %s %s\n", t.riskLabel(risk), describeFinding(f))
			}
		}
	}
	for _, f := range r.Findings {
		if f.Action == core.ActionRetain {
			fmt.Fprintf(t.w, "  %-6s %s\n", "kept", describeFinding(f))
		}
	}
}

func (t *textReporter) riskLabel(risk core.Risk) string {
	label := fmt.Sprintf("%-6s", risk)
	switch risk {
	case core.RiskHigh:
		return t.paint(red, label)
	case core.RiskMed:
		return t.paint(yellow, label)
	}
	return label
}

func describeFinding(f core.Finding) string {
	var b strings.Builder
	b.WriteString(f.Field)
	if f.Value != "" {
		b.WriteString(": ")
		b.WriteString(f.Value)
	}
	if f.Where != "" {
		fmt.Fprintf(&b, " (at %s)", f.Where)
	}
	if f.Action == core.ActionAnonymize {
		b.WriteString(" [anonymized]")
	}
	if f.Reason != "" {
		fmt.Fprintf(&b, " [%s]", f.Reason)
	}
	return b.String()
}

// fieldNames prints what clean did with no values: only what kind of metadata went
// and what stayed.
func (t *textReporter) fieldNames(r engine.Result) {
	var removed, retained []core.Finding
	for _, risk := range riskOrder {
		for _, f := range r.Findings {
			if f.Risk == risk && f.Action != core.ActionRetain {
				removed = append(removed, f)
			}
		}
	}
	for _, f := range r.Findings {
		if f.Action == core.ActionRetain {
			retained = append(retained, f)
		}
	}
	if len(removed) > 0 {
		fmt.Fprintf(t.w, "  removed: %s\n", joinFieldNames(removed))
	}
	if len(retained) > 0 {
		fmt.Fprintf(t.w, "  retained: %s\n", joinFieldNames(retained))
	}
}

// joinFieldNames lists each field once, with a count when it appears more than once.
func joinFieldNames(findings []core.Finding) string {
	counts := make(map[string]int, len(findings))
	var order []string
	for _, f := range findings {
		if counts[f.Field] == 0 {
			order = append(order, f.Field)
		}
		counts[f.Field]++
	}
	parts := make([]string, len(order))
	for i, name := range order {
		parts[i] = name
		if n := counts[name]; n > 1 {
			parts[i] = fmt.Sprintf("%s (%d)", name, n)
		}
	}
	return strings.Join(parts, ", ")
}

func (t *textReporter) finish(s engine.Summary, _ int) {
	s.Warnings += t.notes.added
	var done string
	switch {
	case t.command == "inspect":
		done = fmt.Sprintf("inspected: %d", s.Inspected)
	case t.dryRun:
		done = fmt.Sprintf("would clean: %d", s.WouldClean)
	default:
		done = fmt.Sprintf("cleaned: %d", s.Cleaned)
	}
	fmt.Fprintf(t.w, "Summary: files: %d, %s, failed: %d, unsupported: %d, skipped: %d, warnings: %d, ",
		s.Files, done, s.Failed, s.Unsupported, s.Skipped, s.Warnings)
	if t.command == "inspect" {
		fmt.Fprintf(t.w, "bytes: %d, ", s.BytesBefore)
	} else {
		fmt.Fprintf(t.w, "bytes: %d -> %d, ", s.BytesBefore, s.BytesAfter)
	}
	fmt.Fprintf(t.w, "time: %s\n", s.Elapsed.Round(time.Millisecond))
	t.notes.finish(s)
}

func writeTextFormats(w io.Writer, formats []core.Format) {
	for _, f := range formats {
		fmt.Fprintf(w, "%s: %s (%s)\n", f.ID, f.Name, f.Family)
		fmt.Fprintf(w, "  extensions: %s\n", strings.Join(f.Extensions, " "))
		for _, limit := range f.Limitations {
			fmt.Fprintf(w, "  limitation: %s\n", limit)
		}
	}
	fmt.Fprintf(w, "file-system metadata: %s\n", engine.FilesystemLimit)
}

// ANSI colour codes, the only escapes HideMe ever prints, and only to a terminal.
const (
	red    = "31"
	yellow = "33"
	green  = "32"
)

func (t *textReporter) paint(code, s string) string {
	if !t.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
