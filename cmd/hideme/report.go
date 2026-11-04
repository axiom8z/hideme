package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/engine"
)

// reporter receives results as the engine produces them. Each implementation prints
// the same information, and every string has been through sanitize first.
type reporter interface {
	begin()
	result(engine.Result)
	finish(summary engine.Summary, code int)
}

func newReporter(useJSON bool, command string, dryRun bool, show display, stdout, stderr io.Writer) reporter {
	notes := &stderrNotes{w: stderr}
	if useJSON {
		return &jsonReporter{w: stdout, notes: notes, command: command, dryRun: dryRun, show: show}
	}
	return &textReporter{w: stdout, notes: notes, command: command, dryRun: dryRun, show: show, color: colorEnabled(stdout)}
}

// display says how much of a finding may be printed. Values stay out of clean's
// output so that logs do not recreate what it just removed.
type display struct {
	showValues bool
	namesOnly  bool   // list field names in place of findings; text output only
	detailHint string // how to see a handler's own words about a failure
}

// sanitize makes a result safe to print. Handlers already sanitise through
// core.Report, but this is the last line of defence against a hostile file name or
// value rewriting the user's terminal, so it applies to every string regardless.
func sanitize(r engine.Result, show display) engine.Result {
	if safe := safePath(r.Path); safe != r.Path {
		r.Warnings = append(r.Warnings[:len(r.Warnings):len(r.Warnings)], "this file's name holds control or invisible characters, or is very long; it is shown changed")
		r.Path = safe
	}
	r.Output = safePath(r.Output)
	r.Format = core.SafeText(r.Format)
	// A handler's explanation may quote the file, and is no more fit for a log than the
	// values it stands beside.
	switch {
	case r.Detail == "":
	case show.showValues:
		r.Reason = r.Detail
	case show.detailHint != "":
		r.Reason += " (" + show.detailHint + ")"
	}
	r.Detail = ""
	r.Reason = core.SafeMessage(r.Reason)
	findings := make([]core.Finding, len(r.Findings))
	for i, f := range r.Findings {
		f.Field = core.SafeText(f.Field)
		f.Where = core.SafeText(f.Where)
		f.Reason = core.SafeText(f.Reason)
		f.Value = core.SafeText(f.Value)
		if !show.showValues {
			f.Value = ""
		}
		findings[i] = f
	}
	r.Findings = findings
	warnings := make([]string, len(r.Warnings))
	for i, w := range r.Warnings {
		warnings[i] = core.SafeMessage(w)
	}
	r.Warnings = warnings
	return r
}

// safePath sanitises a path one component at a time. core.SafeText caps its result
// at a length that a deep but harmless path can exceed, and a path cut in the middle
// would be useless, whereas a single file name that long is already suspect.
func safePath(path string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(path); i++ {
		if isSeparator(path[i]) {
			b.WriteString(core.SafeText(path[start:i]))
			b.WriteByte(path[i])
			start = i + 1
		}
	}
	b.WriteString(core.SafeText(path[start:]))
	return b.String()
}

// stderrNotes tells a person what they must not miss, on the stream that survives
// redirecting stdout to a file or a pipe. It also repeats warnings at the end, because
// in a long batch the one line saying a TIFF may have lost frames has scrolled away by
// the time the run finishes, and --in-place has by then replaced the original.
type stderrNotes struct {
	w        io.Writer
	warnings []string
	more     int
	added    int // warnings sanitize added, which the engine's summary cannot know of
}

const maxRecalledWarnings = 20

// remember takes the result both before and after sanitize.
func (n *stderrNotes) remember(raw, r engine.Result) {
	n.added += len(r.Warnings) - len(raw.Warnings)
	for _, w := range r.Warnings {
		if len(n.warnings) == maxRecalledWarnings {
			n.more++
			continue
		}
		n.warnings = append(n.warnings, r.Path+": "+w)
	}
}

func (n *stderrNotes) finish(s engine.Summary) {
	if len(n.warnings) > 0 {
		fmt.Fprintf(n.w, "hideme: warnings about what could not be cleaned:\n")
		for _, w := range n.warnings {
			fmt.Fprintf(n.w, "  %s\n", w)
		}
		if n.more > 0 {
			fmt.Fprintf(n.w, "  and %d more\n", n.more)
		}
	}
	if s.Failed > 0 {
		fmt.Fprintf(n.w, "hideme: %d of %d failed and were not processed\n", s.Failed, s.Files)
	}
	if notDone := s.Unsupported + s.Skipped; notDone > 0 {
		fmt.Fprintf(n.w, "hideme: %d of %d were unsupported or skipped, so they were not processed\n", notDone, s.Files)
	}
	if s.Files == 0 {
		fmt.Fprintln(n.w, "hideme: no files found")
	}
}
