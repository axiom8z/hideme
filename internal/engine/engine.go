// Package engine owns everything that is not format-specific: finding files,
// naming outputs, writing them atomically, proving each one clean before it
// becomes visible, and reporting file-system metadata. The rules it enforces are
// in docs/DESIGN.md.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

// Op says what Run does to each file.
type Op int

const (
	// OpInspect reports metadata and changes nothing.
	OpInspect Op = iota
	// OpClean writes a verified metadata-free copy.
	OpClean
)

// Timestamp says which times an output file gets.
type Timestamp int

const (
	// TimestampNow leaves the output with the time it was written, the default.
	TimestampNow Timestamp = iota
	// TimestampZero sets every file time to ZeroTime, so that the output reveals
	// neither when the original was made nor when it was cleaned.
	TimestampZero
	// TimestampKeep copies the source's times. This retains the capture time, so
	// it is the one mode that keeps a piece of metadata on purpose.
	TimestampKeep
)

// ZeroTime is what TimestampZero writes. It is the earliest time a ZIP entry can
// hold, which makes it a familiar neutral value, and every file system HideMe
// runs on can store it.
var ZeroTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// ParseTimestamp converts the --timestamp flag's value.
func ParseTimestamp(s string) (Timestamp, error) {
	switch s {
	case "now":
		return TimestampNow, nil
	case "zero":
		return TimestampZero, nil
	case "keep":
		return TimestampKeep, nil
	}
	return 0, fmt.Errorf("unknown timestamp mode %q: use now, zero or keep", s)
}

// DefaultSuffix is inserted before the extension of an output placed next to its
// source, so that photo.jpg becomes photo.clean.jpg.
const DefaultSuffix = ".clean"

// Config controls a run.
type Config struct {
	// Strict also removes retained data such as orientation and ICC profiles.
	Strict bool
	// Recursive allows directories as inputs.
	Recursive bool
	// Jobs is how many files are processed at once. Zero means DefaultJobs.
	Jobs int

	// OutputDir, when set, receives outputs instead of the source's directory.
	OutputDir string
	// InPlace replaces each source through an atomic rename.
	InPlace bool
	// Suffix is inserted before the extension of each output name. It is literal:
	// the CLI chooses between DefaultSuffix and none.
	Suffix string
	// Force allows an existing output to be replaced, and with InPlace an original
	// whose cleaned copy may lack content it holds.
	Force bool
	// RandomName replaces each output's stem with random hex.
	RandomName bool
	// Timestamp chooses the output's file times.
	Timestamp Timestamp
	// DryRun cleans and verifies into a temp file that is then thrown away. It says
	// what a real run with the same settings would do, refusals included.
	DryRun bool
}

// DefaultJobs is the worker count when none is asked for. Past eight workers,
// HideMe is mostly waiting on the disk.
func DefaultJobs() int { return min(runtime.NumCPU(), 8) }

// Validate reports a configuration that can never work. It is a usage error
// rather than something that fails file by file.
func (c Config) Validate(op Op) error {
	if c.Jobs < 0 {
		return fmt.Errorf("jobs must not be negative, got %d", c.Jobs)
	}
	if op == OpInspect {
		return nil
	}
	if c.InPlace {
		switch {
		case c.OutputDir != "":
			return errors.New("--in-place and -o cannot be combined: one replaces the source, the other writes elsewhere")
		case c.RandomName:
			return errors.New("--in-place and --random-name cannot be combined: replacing a file keeps its name")
		case c.Suffix != "":
			return errors.New("--in-place and --suffix cannot be combined: replacing a file keeps its name")
		}
	}
	if strings.ContainsAny(c.Suffix, `/\:*?"<>|`) || core.SafeText(c.Suffix) != c.Suffix {
		return fmt.Errorf(`--suffix %q must not contain path separators, control characters or any of : * ? " < > |`, core.SafeText(c.Suffix))
	}
	if c.Suffix == "" && !c.InPlace && !c.RandomName && c.OutputDir == "" {
		return errors.New("an empty --suffix would write over the source: use -o DIR, --random-name or --in-place")
	}
	if c.OutputDir != "" {
		if info, err := os.Stat(c.OutputDir); err == nil && !info.IsDir() {
			return fmt.Errorf("-o %s is not a directory", core.SafeText(c.OutputDir))
		}
	}
	return nil
}

func (c Config) options() core.Options { return core.Options{Strict: c.Strict} }

func (c Config) jobs() int {
	if c.Jobs > 0 {
		return c.Jobs
	}
	return DefaultJobs()
}

// Status is the outcome for one input.
type Status string

const (
	// StatusInspected means Inspect ran and its findings are in the Result.
	StatusInspected Status = "inspected"
	// StatusCleaned means the output exists and passed verification.
	StatusCleaned Status = "cleaned"
	// StatusWouldClean is what a dry run reports where a real run says cleaned.
	StatusWouldClean Status = "would-clean"
	// StatusFailed means the file was not cleaned and no output was left.
	StatusFailed Status = "failed"
	// StatusUnsupported means a walk met a file HideMe cannot clean.
	StatusUnsupported Status = "unsupported"
	// StatusSkipped means a walk passed over this entry on purpose.
	StatusSkipped Status = "skipped"
)

// Result is what happened to one input.
type Result struct {
	Path     string
	Output   string // the output written, or that a dry run would write; else empty
	Status   Status
	Format   string         // Report.Format of the detected handler
	Findings []core.Finding // includes file-system metadata
	Warnings []string       // what HideMe could not clean
	// MayLoseContent says the output may lack content the original holds.
	MayLoseContent bool
	// Reason says why the file failed, was skipped or is unsupported. It is safe
	// to print: it holds no path and no text from the file.
	Reason string
	// Detail is a format handler's own explanation of a failure, when there is one. It
	// is sanitised, but it can quote text from the file, so it is for printing only when
	// the user has asked to see such text.
	Detail string
	// Kind is a stable classification of Reason, for scripts.
	Kind        string
	BytesBefore int64
	BytesAfter  int64
	Elapsed     time.Duration
}

// NothingFound reports a file processed successfully that had no metadata.
func (r Result) NothingFound() bool {
	switch r.Status {
	case StatusInspected, StatusCleaned, StatusWouldClean:
		return len(r.Findings) == 0
	}
	return false
}

// Summary totals a run.
type Summary struct {
	Files       int
	Inspected   int
	Cleaned     int
	WouldClean  int
	Failed      int
	Unsupported int
	Skipped     int
	Warnings    int
	BytesBefore int64
	BytesAfter  int64
	Elapsed     time.Duration
}

func (s *Summary) add(r Result) {
	s.Files++
	switch r.Status {
	case StatusInspected:
		s.Inspected++
	case StatusCleaned:
		s.Cleaned++
	case StatusWouldClean:
		s.WouldClean++
	case StatusFailed:
		s.Failed++
	case StatusUnsupported:
		s.Unsupported++
	case StatusSkipped:
		s.Skipped++
	}
	s.Warnings += len(r.Warnings)
	// The totals compare before with after, and a failed file has no after.
	if r.Status != StatusFailed {
		s.BytesBefore += r.BytesBefore
		s.BytesAfter += r.BytesAfter
	}
}

// Inspect reports the metadata in one file, changing nothing.
func Inspect(ctx context.Context, path string, cfg Config) Result {
	return one(ctx, OpInspect, path, cfg)
}

// Clean writes a verified metadata-free copy of one file, as named by cfg.
func Clean(ctx context.Context, path string, cfg Config) Result {
	return one(ctx, OpClean, path, cfg)
}

func one(ctx context.Context, op Op, path string, cfg Config) Result {
	if err := cfg.Validate(op); err != nil {
		return failure(path, err)
	}
	cfg.Recursive = false
	tasks := plan(ctx, op, []string{path}, cfg)
	return worker{op: op, cfg: cfg}.process(ctx, tasks[0])
}

// Run processes every path with a pool of workers and calls emit once per
// result, from the calling goroutine, in the order the inputs were given. A
// result is emitted as soon as all those before it have been, so a long batch
// reports progress instead of going quiet until the end. Canceling ctx stops the
// work: files in progress lose their temp files and report the cancellation.
//
// The returned error is only for a configuration that can never work.
func Run(ctx context.Context, op Op, paths []string, cfg Config, emit func(Result)) (Summary, error) {
	if err := cfg.Validate(op); err != nil {
		return Summary{}, err
	}
	start := time.Now()
	tasks := plan(ctx, op, expandWildcards(paths), cfg)

	type finished struct {
		index  int
		result Result
	}
	next := make(chan int)
	done := make(chan finished)
	w := worker{op: op, cfg: cfg}
	for range min(cfg.jobs(), max(len(tasks), 1)) {
		go func() {
			for i := range next {
				done <- finished{i, w.process(ctx, tasks[i])}
			}
		}()
	}
	go func() {
		// Every task is fed even after cancellation, because each must produce a
		// result; process returns at once for a canceled context.
		for i := range tasks {
			next <- i
		}
		close(next)
	}()

	var summary Summary
	pending := make(map[int]Result)
	emitted := 0
	for range tasks {
		f := <-done
		pending[f.index] = f.result
		for {
			r, ok := pending[emitted]
			if !ok {
				break
			}
			delete(pending, emitted)
			emit(r)
			summary.add(r)
			emitted++
		}
	}
	summary.Elapsed = time.Since(start)
	return summary, nil
}
