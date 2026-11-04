package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/engine"
)

const jsonSchema = "hideme/1"

// jsonResult is one input in the schema described by "hideme help json".
type jsonResult struct {
	Path         string         `json:"path"`
	Output       string         `json:"output,omitempty"`
	Status       engine.Status  `json:"status"`
	Format       string         `json:"format,omitempty"`
	NothingFound bool           `json:"nothing_found"`
	BytesBefore  int64          `json:"bytes_before"`
	BytesAfter   int64          `json:"bytes_after"`
	ElapsedMS    int64          `json:"elapsed_ms"`
	Findings     []core.Finding `json:"findings"`
	Warnings     []string       `json:"warnings"`
	// MayLoseContent is true when the output may lack content the original holds.
	MayLoseContent bool   `json:"may_lose_content"`
	Reason         string `json:"reason,omitempty"`
	ErrorKind      string `json:"error_kind,omitempty"`
}

type jsonSummary struct {
	Files       int   `json:"files"`
	Inspected   int   `json:"inspected"`
	Cleaned     int   `json:"cleaned"`
	WouldClean  int   `json:"would_clean"`
	Failed      int   `json:"failed"`
	Unsupported int   `json:"unsupported"`
	Skipped     int   `json:"skipped"`
	Warnings    int   `json:"warnings"`
	BytesBefore int64 `json:"bytes_before"`
	BytesAfter  int64 `json:"bytes_after"`
	ElapsedMS   int64 `json:"elapsed_ms"`
}

func toJSONSummary(s engine.Summary) jsonSummary {
	return jsonSummary{
		Files: s.Files, Inspected: s.Inspected, Cleaned: s.Cleaned, WouldClean: s.WouldClean,
		Failed: s.Failed, Unsupported: s.Unsupported, Skipped: s.Skipped, Warnings: s.Warnings,
		BytesBefore: s.BytesBefore, BytesAfter: s.BytesAfter, ElapsedMS: s.Elapsed.Milliseconds(),
	}
}

// jsonReporter writes one JSON document, but writes it as results arrive: each result
// is on its own line in the "results" array, so a long run can be followed, and the
// whole is still a single document once finished.
type jsonReporter struct {
	w       io.Writer
	notes   *stderrNotes
	command string
	dryRun  bool
	show    display
	wrote   bool
}

func (j *jsonReporter) begin() {
	fmt.Fprintf(j.w, `{"schema":%s,"command":%s,"dry_run":%t,"results":[`, mustJSON(jsonSchema), mustJSON(j.command), j.dryRun)
}

func (j *jsonReporter) result(raw engine.Result) {
	r := sanitize(raw, j.show)
	j.notes.remember(raw, r)
	if r.Status == engine.StatusFailed || r.Status == engine.StatusUnsupported || r.Status == engine.StatusSkipped {
		fmt.Fprintf(j.notes.w, "hideme: %s: %s: %s\n", r.Path, r.Status, r.Reason)
	}
	if j.wrote {
		fmt.Fprint(j.w, ",")
	}
	j.wrote = true
	fmt.Fprintf(j.w, "\n%s", mustJSON(jsonResult{
		Path:         r.Path,
		Output:       r.Output,
		Status:       r.Status,
		Format:       r.Format,
		NothingFound: r.NothingFound(),
		BytesBefore:  r.BytesBefore,
		BytesAfter:   r.BytesAfter,
		ElapsedMS:    r.Elapsed.Milliseconds(),
		Findings:     r.Findings,
		Warnings:     r.Warnings,

		MayLoseContent: r.MayLoseContent,
		Reason:         r.Reason,
		ErrorKind:      r.Kind,
	}))
}

func (j *jsonReporter) finish(s engine.Summary, code int) {
	s.Warnings += j.notes.added
	fmt.Fprintf(j.w, "\n],\"summary\":%s,\"exit_code\":%d}\n", mustJSON(toJSONSummary(s)), code)
	j.notes.finish(s)
}

// writeJSONError emits the document for a command line that was rejected, where no
// run took place.
func writeJSONError(w io.Writer, command string, err error) {
	fmt.Fprintln(w, mustJSON(struct {
		Schema   string       `json:"schema"`
		Command  string       `json:"command"`
		DryRun   bool         `json:"dry_run"`
		Results  []jsonResult `json:"results"`
		Summary  jsonSummary  `json:"summary"`
		ExitCode int          `json:"exit_code"`
		Error    string       `json:"error"`
	}{jsonSchema, core.SafeText(command), false, []jsonResult{}, jsonSummary{}, exitUsage, core.SafeText(err.Error())}))
}

func writeJSONFormats(w io.Writer, formats []core.Format) {
	type jsonFormat struct {
		ID          string      `json:"id"`
		Name        string      `json:"name"`
		Family      core.Family `json:"family"`
		Extensions  []string    `json:"extensions"`
		Limitations []string    `json:"limitations"`
	}
	list := make([]jsonFormat, len(formats))
	for i, f := range formats {
		list[i] = jsonFormat{f.ID, f.Name, f.Family, f.Extensions, f.Limitations}
		if list[i].Limitations == nil {
			list[i].Limitations = []string{}
		}
	}
	fmt.Fprintln(w, mustJSON(struct {
		Schema     string       `json:"schema"`
		Command    string       `json:"command"`
		Formats    []jsonFormat `json:"formats"`
		Filesystem string       `json:"filesystem"`
	}{jsonSchema, "formats", list, engine.FilesystemLimit}))
}

// mustJSON encodes a value made entirely of strings, numbers and slices, which cannot
// fail. HTML escaping is off so that values such as "<x:xmpmeta" stay readable.
func mustJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}
