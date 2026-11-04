// Package coretest is the test suite every format handler runs. It turns the
// rules in docs/DESIGN.md into assertions, so a handler that breaks one fails
// its own tests instead of relying on the engine to catch it in production.
//
// A handler's test file normally needs three calls:
//
//	func TestContract(t *testing.T) { coretest.Contract(t, handler{}, cases) }
//	func TestRobustness(t *testing.T) { coretest.Robustness(t, handler{}, sample) }
//	func FuzzClean(f *testing.F) {
//		f.Add(sample)
//		f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
//	}
//
// The package imports testing, so only test files may import it. FakeEXIF and
// FakeXMP supply metadata fixtures for handlers that embed those blocks.
package coretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
)

// Case is one input to Contract.
type Case struct {
	Name string
	Data []byte
	// WantFields lists the Finding.Field values Inspect must report.
	WantFields []string
	// WantRetained lists fields that must be reported with ActionRetain and a
	// Reason, both on the input and on the cleaned output.
	WantRetained []string
	// MustNotContain lists byte strings that must be absent from the output.
	MustNotContain [][]byte
	// MustContain lists content that must survive cleaning.
	MustContain [][]byte
	Opts        core.Options
}

// reporter is the part of *testing.T the checks use. Keeping it this narrow
// lets this package's own tests prove that a broken handler really fails.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
	Failed() bool
}

// Contract runs every handler rule against each case. Cases that do not set
// Opts.Strict are also held to the general rules under Strict, so a handler
// cannot break in a mode its author never exercised.
func Contract(t *testing.T, h core.Handler, cases []Case) {
	t.Helper()
	if len(cases) == 0 {
		t.Fatal("coretest: Contract needs at least one case")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			checkCase(t, h, c)
			if !c.Opts.Strict {
				checkStrict(t, h, c)
			}
		})
	}
}

// checkStrict holds a case to the rules that Strict cannot relax. Strict only
// ever removes more, so a valid file must still clean and whatever the case
// forbids must still be gone. MustContain and WantRetained are not checked here,
// because retained data is what Strict exists to remove.
func checkStrict(r reporter, h core.Handler, c Case) {
	r.Helper()
	opts := c.Opts
	opts.Strict = true
	checkInputWith(r, h, c.Data, opts)
	if r.Failed() {
		return // a panic above would only repeat here, without the recover
	}
	out, _, err := runClean(context.Background(), h, c.Data, opts)
	if err != nil {
		r.Errorf("Clean in strict mode: %v", err)
		return
	}
	checkContent(r, out, c.MustNotContain, nil)
}

// Robustness feeds a valid file through truncation at many lengths and through
// single-byte corruption across the file. The corruption comes from a seeded
// generator, so a failure reproduces on every run. It stops at the first
// failing input, whose description is part of the failure message.
func Robustness(t *testing.T, h core.Handler, data []byte) {
	t.Helper()
	checkRobustness(t, h, data)
}

// FuzzTarget is the body of a handler's FuzzClean target. It asserts that no
// input causes a panic or an error outside the core sentinels, and that any
// successful Clean satisfies the rules from Contract.
func FuzzTarget(t *testing.T, h core.Handler, data []byte) {
	t.Helper()
	checkInput(t, h, data)
}

func checkCase(r reporter, h core.Handler, c Case) {
	r.Helper()
	ctx := context.Background()
	if !matches(h, c.Data) {
		r.Errorf("Match is false for the input")
	}
	inspected, err := h.Inspect(ctx, core.BytesSource(c.Data), c.Opts)
	if err != nil {
		r.Errorf("Inspect: %v", err)
		return
	}
	checkFields(r, "Inspect", inspected, c.WantFields, c.WantRetained)

	out, cleaned, err := runClean(ctx, h, c.Data, c.Opts)
	if err != nil {
		r.Errorf("Clean: %v", err)
		return
	}
	checkCleaned(r, h, c.Data, c.Opts, inspected, cleaned, out)
	checkContent(r, out, c.MustNotContain, c.MustContain)
	if outReport, err := h.Inspect(ctx, core.BytesSource(out), c.Opts); err == nil {
		checkFields(r, "Inspect on the output", outReport, nil, c.WantRetained)
	}
	checkCancelled(r, h, c.Data, c.Opts)
	checkConcurrent(r, h, c.Data, c.Opts, out)
}

// checkCleaned asserts what must hold after any successful Clean, whatever the
// input was.
func checkCleaned(r reporter, h core.Handler, data []byte, opts core.Options, inspected, cleaned *core.Report, out []byte) {
	r.Helper()
	ctx := context.Background()
	checkReport(r, "Inspect", inspected)
	checkReport(r, "Clean", cleaned)
	if inspected == nil || cleaned == nil {
		return
	}
	checkAgreement(r, inspected, cleaned)
	if !matches(h, out) {
		r.Errorf("the output no longer matches the handler")
	}

	verified, err := h.Inspect(ctx, core.BytesSource(out), opts)
	if err != nil {
		r.Errorf("Inspect on the output: %v", err)
		return
	}
	checkReport(r, "Inspect on the output", verified)
	if verified == nil {
		return
	}
	if verified.Format != cleaned.Format {
		r.Errorf("the output is format %q but the input is format %q", verified.Format, cleaned.Format)
	}
	for _, f := range verified.Residual() {
		r.Errorf("residual finding on the output: %q (%s) at %q", f.Field, f.Action, f.Where)
	}

	again, _, err := runClean(ctx, h, out, opts)
	if err != nil {
		r.Errorf("Clean on the output: %v", err)
	} else if !bytes.Equal(again, out) {
		r.Errorf("Clean is not idempotent: %d bytes became %d bytes, first difference at %d",
			len(out), len(again), firstDifference(out, again))
	}
	second, _, err := runClean(ctx, h, data, opts)
	if err != nil {
		r.Errorf("second Clean of the input: %v", err)
	} else if !bytes.Equal(second, out) {
		r.Errorf("Clean is not deterministic: runs gave %d and %d bytes, first difference at %d",
			len(out), len(second), firstDifference(out, second))
	}
}

// checkFields asserts that want appears in the report and that retained
// appears with ActionRetain and a Reason.
func checkFields(r reporter, label string, rep *core.Report, want, retained []string) {
	r.Helper()
	if rep == nil {
		r.Errorf("%s returned a nil report", label)
		return
	}
	for _, field := range want {
		if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == field }) {
			r.Errorf("%s did not report field %q; it reported %q", label, field, fieldNames(rep))
		}
	}
	for _, field := range retained {
		i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool {
			return f.Field == field && f.Action == core.ActionRetain
		})
		switch {
		case i < 0:
			r.Errorf("%s did not report field %q as retained; it reported %q", label, field, fieldNames(rep))
		case rep.Findings[i].Reason == "":
			r.Errorf("%s retains field %q without giving a Reason", label, field)
		}
	}
}

// checkReport asserts that a report is well formed: the engine prints these
// strings to a terminal and sorts by these categories, so a handler that
// bypasses Report.Add or leaves a field blank is a defect even if it cleans
// correctly.
func checkReport(r reporter, label string, rep *core.Report) {
	r.Helper()
	if rep == nil {
		r.Errorf("%s returned a nil report", label)
		return
	}
	if rep.Format == "" {
		r.Errorf("%s returned a report with an empty Format", label)
	}
	for _, f := range rep.Findings {
		if f.Field == "" || f.Category == "" || f.Risk == "" || f.Action == "" {
			r.Errorf("%s returned an incomplete finding: %+v", label, f)
		}
		for _, s := range []string{f.Field, f.Value, f.Where, f.Reason} {
			if !isSafeText(s) {
				r.Errorf("%s returned unsanitised text %q in finding %q", label, s, f.Field)
			}
		}
	}
	for _, w := range rep.Warnings {
		if !isSafeText(w) {
			r.Errorf("%s returned an unsanitised warning %q", label, w)
		}
	}
}

// isSafeText checks for the characters core.SafeText removes rather than
// comparing against its output, because truncation is not idempotent for a
// value that begins with whitespace and that difference is cosmetic.
func isSafeText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}

// checkAgreement asserts that Inspect predicted exactly what Clean did. A
// mismatch means a user could be told one thing while another happens.
func checkAgreement(r reporter, inspected, cleaned *core.Report) {
	r.Helper()
	if inspected.Format != cleaned.Format {
		r.Errorf("Inspect says format %q but Clean says %q", inspected.Format, cleaned.Format)
	}
	want, got := findingSet(inspected), findingSet(cleaned)
	if !slices.Equal(want, got) {
		r.Errorf("Inspect and Clean disagree:\n  Inspect: %q\n  Clean:   %q", want, got)
	}
	if inspected.MayLoseContent != cleaned.MayLoseContent {
		r.Errorf("Inspect says MayLoseContent is %v but Clean says %v", inspected.MayLoseContent, cleaned.MayLoseContent)
	}
}

// findingSet returns the sorted, deduplicated (Field, Action) pairs of a report.
func findingSet(rep *core.Report) []string {
	set := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		set = append(set, f.Field+" => "+string(f.Action))
	}
	slices.Sort(set)
	return slices.Compact(set)
}

func fieldNames(rep *core.Report) []string {
	names := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		names = append(names, f.Field)
	}
	return names
}

func checkContent(r reporter, out []byte, mustNot, must [][]byte) {
	r.Helper()
	for _, s := range mustNot {
		if bytes.Contains(out, s) {
			r.Errorf("the output still contains %q", s)
		}
	}
	for _, s := range must {
		if !bytes.Contains(out, s) {
			r.Errorf("the output lost content %q", s)
		}
	}
}

// checkCancelled asserts that an already-cancelled context cannot provoke a
// panic or an error the engine cannot classify. Finishing anyway is allowed,
// because a handler may complete before its first context check.
func checkCancelled(r reporter, h core.Handler, data []byte, opts core.Options) {
	r.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, inspectErr := h.Inspect(ctx, core.BytesSource(data), opts)
	checkError(r, "Inspect with a cancelled context", inspectErr)
	_, _, cleanErr := runClean(ctx, h, data, opts)
	checkError(r, "Clean with a cancelled context", cleanErr)
}

// checkConcurrent runs Clean from several goroutines, because the interface
// promises concurrent use and the engine relies on it. Comparing the outputs
// catches a shared buffer even in builds without the race detector.
func checkConcurrent(r reporter, h core.Handler, data []byte, opts core.Options, want []byte) {
	r.Helper()
	const workers = 4
	var wg sync.WaitGroup
	results := make([][]byte, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, errs[i] = runClean(context.Background(), h, data, opts)
		}()
	}
	wg.Wait()
	for i := range workers {
		if errs[i] != nil {
			r.Errorf("concurrent Clean: %v", errs[i])
		} else if !bytes.Equal(results[i], want) {
			r.Errorf("concurrent Clean produced different bytes, first difference at %d", firstDifference(want, results[i]))
		}
	}
}

// checkInput applies the rules that hold for arbitrary, possibly hostile, input.
func checkInput(r reporter, h core.Handler, data []byte) {
	r.Helper()
	for _, opts := range []core.Options{{}, {Strict: true}} {
		checkInputWith(r, h, data, opts)
	}
}

func checkInputWith(r reporter, h core.Handler, data []byte, opts core.Options) {
	r.Helper()
	defer func() {
		if p := recover(); p != nil {
			r.Errorf("panic on a %d-byte input (strict=%v): %v\n%s", len(data), opts.Strict, p, debug.Stack())
		}
	}()
	ctx := context.Background()
	matches(h, data)
	inspected, inspectErr := h.Inspect(ctx, core.BytesSource(data), opts)
	checkError(r, "Inspect", inspectErr)
	out, cleaned, cleanErr := runClean(ctx, h, data, opts)
	checkError(r, "Clean", cleanErr)
	if cleanErr != nil {
		return
	}
	if inspectErr != nil {
		r.Errorf("Clean succeeded but Inspect failed: %v", inspectErr)
		return
	}
	checkCleaned(r, h, data, opts, inspected, cleaned, out)
}

// checkError asserts the error classification the engine relies on to tell the
// user why a file was not cleaned.
func checkError(r reporter, label string, err error) {
	r.Helper()
	if err == nil {
		return
	}
	for _, sentinel := range []error{
		core.ErrUnsupported, core.ErrMalformed, core.ErrEncrypted, core.ErrTooLarge,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, sentinel) {
			return
		}
	}
	r.Errorf("%s returned an error wrapping neither a core sentinel nor a context error: %v", label, err)
}

// How many mutated inputs Robustness tries. The counts are independent of the
// file size, so the run time stays predictable for a large fixture.
const (
	headerBytes  = 128              // every prefix this short is tried, because parsers branch most here
	tailBytes    = 32               // truncations this close to the end, where trailers live
	spreadCuts   = 96               // further truncation points spread across the file
	headerFlips  = 128              // leading bytes corrupted in turn
	randomFlips  = 384              // corruptions at seeded random positions
	mutationSeed = 0x4869_6465_4D65 // "HideMe"; fixed so failures reproduce
)

func checkRobustness(r reporter, h core.Handler, data []byte) {
	r.Helper()
	check := func(mutated []byte, format string, args ...any) bool {
		r.Helper()
		checkInput(r, h, mutated)
		if r.Failed() {
			r.Errorf("failing input: %s", fmt.Sprintf(format, args...))
			return false
		}
		return true
	}
	if !check(data, "the unmodified file") {
		return
	}
	for _, n := range truncationLengths(len(data)) {
		if !check(data[:n], "the file truncated to %d of %d bytes", n, len(data)) {
			return
		}
	}
	rng := rand.New(rand.NewPCG(mutationSeed, uint64(len(data))))
	mutated := bytes.Clone(data)
	flip := func(pos int) bool {
		r.Helper()
		original := mutated[pos]
		mutated[pos] = corruptByte(rng, original)
		ok := check(mutated, "byte %d changed from %#02x to %#02x", pos, original, mutated[pos])
		mutated[pos] = original
		return ok
	}
	for pos := range min(len(data), headerFlips) {
		if !flip(pos) {
			return
		}
	}
	if len(data) == 0 {
		return
	}
	for range randomFlips {
		if !flip(rng.IntN(len(data))) {
			return
		}
	}
}

// truncationLengths returns the sorted distinct prefix lengths to try for a
// file of size n, not including n itself.
func truncationLengths(n int) []int {
	var lengths []int
	for i := range min(n, headerBytes) {
		lengths = append(lengths, i)
	}
	for i := max(0, n-tailBytes); i < n; i++ {
		lengths = append(lengths, i)
	}
	for i := range spreadCuts {
		lengths = append(lengths, n*i/spreadCuts)
	}
	slices.Sort(lengths)
	return slices.Compact(lengths)
}

// corruptByte favours the values that most often slip past length checks.
func corruptByte(rng *rand.Rand, original byte) byte {
	switch rng.IntN(4) {
	case 0:
		if original != 0x00 {
			return 0x00
		}
	case 1:
		if original != 0xFF {
			return 0xFF
		}
	}
	return original ^ byte(1+rng.IntN(255))
}

func matches(h core.Handler, data []byte) bool {
	return h.Match(data[:min(len(data), core.HeadSize)], core.BytesSource(data))
}

func runClean(ctx context.Context, h core.Handler, data []byte, opts core.Options) ([]byte, *core.Report, error) {
	var out bytes.Buffer
	rep, err := h.Clean(ctx, core.BytesSource(data), &out, opts)
	return out.Bytes(), rep, err
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
