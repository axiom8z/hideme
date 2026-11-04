package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// Upper and lower case do not pair up one to one: Go lowers both "I" with a dot and a
// plain "I" to "i", and the Kelvin sign to "k", yet NTFS holds such names as different
// files. Treating them as one input once dropped the second from the run: no result,
// exit 0, and its metadata still in place.
func TestNamesThatFoldAlikeAreStillDifferentFiles(t *testing.T) {
	for _, pair := range [][2]string{
		{"istanbul.good", "İstanbul.good"},
		{"k.good", "K.good"},
		{"å.good", "Å.good"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			dir := t.TempDir()
			a := write(t, filepath.Join(dir, pair[0]), goodFile)
			b := write(t, filepath.Join(dir, pair[1]), goodFile)
			if entries, _ := os.ReadDir(dir); len(entries) != 2 {
				t.Skip("this file system holds both names as one file")
			}

			results, summary := runAll(t, OpClean, []string{dir}, Config{InPlace: true, Recursive: true})
			if summary.Failed != 0 || summary.Cleaned != 2 {
				t.Errorf("in place: %+v", results)
			}
			for _, path := range []string{a, b} {
				if strings.Contains(read(t, path), "SECRET=") {
					t.Errorf("%s still holds its metadata", path)
				}
			}

			write(t, a, goodFile)
			write(t, b, goodFile)
			results, summary = runAll(t, OpClean, []string{dir}, Config{OutputDir: filepath.Join(t.TempDir(), "out"), Recursive: true})
			if summary.Failed != 0 || summary.Cleaned != 2 {
				t.Errorf("output directory: %+v", results)
			}

			// Named on the command line, the same two must both be taken as well.
			results, _ = runAll(t, OpInspect, []string{a, b, a}, Config{})
			if len(results) != 2 {
				t.Errorf("named inputs: %+v", results)
			}
		})
	}
}

func TestSameFileUnderOtherCaseIsStillOneInput(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	if _, err := os.Stat(filepath.Join(dir, "PHOTO.GOOD")); err != nil {
		t.Skip("this file system is case-sensitive")
	}
	results, _ := runAll(t, OpInspect, []string{src, filepath.Join(dir, "PHOTO.GOOD"), src}, Config{})
	if len(results) != 1 {
		t.Errorf("results: %+v", results)
	}
}

func TestExistingOutputIsForecastByADryRun(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	output := write(t, filepath.Join(dir, "a.clean.good"), "earlier output")

	dry := Clean(context.Background(), src, Config{Suffix: DefaultSuffix, DryRun: true})
	actual := Clean(context.Background(), src, defaultConfig())

	if dry.Status != actual.Status || dry.Kind != actual.Kind || dry.Kind != KindOutputExists {
		t.Errorf("a dry run must say what a real run does: dry %q %q, real %q %q", dry.Status, dry.Kind, actual.Status, actual.Kind)
	}
	if !strings.Contains(dry.Reason, "a real run would fail") || !strings.Contains(dry.Reason, "--force") {
		t.Errorf("the reason does not read as a forecast: %q", dry.Reason)
	}
	if strings.Contains(actual.Reason, "would fail") {
		t.Errorf("a real run is no forecast: %q", actual.Reason)
	}
	forced := Clean(context.Background(), src, Config{Suffix: DefaultSuffix, DryRun: true, Force: true})
	if forced.Status != StatusWouldClean {
		t.Errorf("with --force: %q %s", forced.Status, forced.Reason)
	}
	if read(t, output) != "earlier output" {
		t.Error("a dry run changed an existing output")
	}
}

// A dry run that promises a success the real run cannot deliver is worse than none.
func TestDryRunForecastsOutputPathsThatCannotWork(t *testing.T) {
	root := t.TempDir()
	src := write(t, filepath.Join(root, "tree", "a.good"), goodFile)
	blocker := write(t, filepath.Join(root, "blocker"), "a plain file")
	mirrored := filepath.Join(root, "mirrored")
	write(t, filepath.Join(mirrored, "tree"), "a plain file where the tree must go")

	for _, c := range []struct {
		name string
		cfg  Config
		arg  string
	}{
		{"output dir beneath a file", Config{OutputDir: filepath.Join(blocker, "sub")}, src},
		{"mirrored directory is a file", Config{OutputDir: mirrored, Recursive: true}, filepath.Join(root, "tree")},
	} {
		t.Run(c.name, func(t *testing.T) {
			actual, _ := runAll(t, OpClean, []string{c.arg}, c.cfg)
			if actual[0].Status != StatusFailed {
				t.Fatalf("the premise is wrong, a real run says %+v", actual[0])
			}
			c.cfg.DryRun = true
			results, _ := runAll(t, OpClean, []string{c.arg}, c.cfg)
			if results[0].Status != StatusFailed {
				t.Errorf("a dry run promised %q where a real run fails: %s", results[0].Status, actual[0].Reason)
			}
		})
	}
}

func TestDryRunDoesNotWriteThroughALinkInTheOutputDir(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	out := filepath.Join(root, "out")
	for _, dir := range []string{outside, out} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(out, "tree")); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "a.good"), goodFile)

	results, _ := runAll(t, OpClean, []string{tree}, Config{OutputDir: out, Recursive: true, DryRun: true})

	if results[0].Status != StatusFailed || results[0].Kind != KindInvalidInput {
		t.Errorf("a dry run must refuse what a real run refuses: %+v", results[0])
	}
}

// The extension is whatever follows the last dot, which in "John.Smith" or
// "Scan 2024.03.15" is the very thing --random-name is there to hide.
func TestRandomNameDropsWhatIsNotARealExtension(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, wantExt string }{
		{"John.Smith", ""},
		{"Scan 2024.03.15", ""},
		{"IMG.20240101_123456", ""},
		{"photo.good", ".good"},
		{"PHOTO2.GOOD", ".GOOD"},
	} {
		src := write(t, filepath.Join(dir, "in", c.name), goodFile)
		r := Clean(context.Background(), src, Config{OutputDir: filepath.Join(dir, "out"), RandomName: true})
		if r.Status != StatusCleaned {
			t.Fatalf("%s: %q: %s", c.name, r.Status, r.Reason)
		}
		if got := filepath.Ext(r.Output); got != c.wantExt {
			t.Errorf("%s: output %q has extension %q, want %q", c.name, filepath.Base(r.Output), got, c.wantExt)
		}
	}
}

func TestResidueFailureExplainsItself(t *testing.T) {
	dir := t.TempDir()
	content := "FAKELIAR\na manual about <x:xmpmeta and nothing else\n"
	src := write(t, filepath.Join(dir, "doc.liar"), content)

	for name, cfg := range map[string]Config{
		"beside": defaultConfig(),
		"inline": {InPlace: true},
	} {
		r := Clean(context.Background(), src, cfg)

		if r.Status != StatusFailed || r.Kind != KindVerification {
			t.Fatalf("%s: %q %q", name, r.Status, r.Kind)
		}
		for _, want := range []string{
			"<x:xmpmeta",
			"original is untouched",
			"no output was written",
			"false alarm",
			"PDF about XMP",
		} {
			if !strings.Contains(r.Reason, want) {
				t.Errorf("%s: the reason lacks %q: %q", name, want, r.Reason)
			}
		}
		if read(t, src) != content {
			t.Errorf("%s: the original changed", name)
		}
	}
	if exists(filepath.Join(dir, "doc.clean.liar")) {
		t.Error("the output survived")
	}
	assertNoTemp(t, dir)
}

func TestInPlaceIsRefusedWhenTheOutputMayLoseContent(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "stack.lossy"), lossyFile)
	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, r Result) {
		t.Helper()
		if r.Status != StatusFailed || r.Kind != KindMayLoseContent || !r.MayLoseContent {
			t.Fatalf("%+v", r)
		}
		for _, want := range []string{"may lack content", "original is untouched", "--force", "-o DIR"} {
			if !strings.Contains(r.Reason, want) {
				t.Errorf("the reason lacks %q: %q", want, r.Reason)
			}
		}
		now, err := os.Stat(src)
		if err != nil || !os.SameFile(before, now) || read(t, src) != lossyFile {
			t.Error("the original was replaced")
		}
		assertNoTemp(t, dir)
	}

	t.Run("refused", func(t *testing.T) {
		check(t, Clean(context.Background(), src, Config{InPlace: true}))
	})
	t.Run("a dry run forecasts it", func(t *testing.T) {
		r := Clean(context.Background(), src, Config{InPlace: true, DryRun: true})
		check(t, r)
		if !strings.Contains(r.Reason, "a real run would fail") {
			t.Errorf("not a forecast: %q", r.Reason)
		}
	})
	t.Run("a separate output is allowed, with the warning", func(t *testing.T) {
		r := Clean(context.Background(), src, Config{OutputDir: filepath.Join(dir, "out")})
		if r.Status != StatusCleaned || !r.MayLoseContent || len(r.Warnings) == 0 {
			t.Fatalf("%+v", r)
		}
		if read(t, src) != lossyFile {
			t.Error("the original changed")
		}
	})
	t.Run("--force replaces it and says so", func(t *testing.T) {
		r := Clean(context.Background(), src, Config{InPlace: true, Force: true})
		if r.Status != StatusCleaned || !r.MayLoseContent {
			t.Fatalf("%+v", r)
		}
		now, err := os.Stat(src)
		if err != nil || os.SameFile(before, now) {
			t.Error("--force did not replace the original")
		}
		if !slices.ContainsFunc(r.Warnings, func(w string) bool { return strings.Contains(w, "--force replaced the original") }) {
			t.Errorf("no warning that the original is gone: %q", r.Warnings)
		}
	})
}

func TestHardLinkWarningIsTrueOfADryRun(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	if err := os.Link(src, filepath.Join(dir, "b.good")); err != nil {
		t.Skipf("hard links not available: %v", err)
	}
	r := Clean(context.Background(), src, Config{InPlace: true, DryRun: true})
	if len(r.Warnings) != 1 || strings.Contains(r.Warnings[0], "replaced") {
		t.Errorf("nothing was replaced, but: %q", r.Warnings)
	}
}

// A file that fails has still been opened and measured, so its size is known, and a
// report saying it held nothing is wrong. The run's totals are another matter: they
// compare before and after, and a failed file has no after.
func TestFailedResultKeepsItsSize(t *testing.T) {
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"no handler":       "just text",
		"handler refuses":  "FAKEREFUSER\n",
		"handler fails":    "FAKEFAILER\n",
		"handler panics":   "FAKEPANICKER\n",
		"output discarded": "FAKELIAR\n<x:xmpmeta>\n",
	} {
		src := write(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".bin"), contents)
		results, summary := runAll(t, OpClean, []string{src}, defaultConfig())
		r := results[0]
		if r.Status != StatusFailed {
			t.Fatalf("%s: status %q: %s", name, r.Status, r.Reason)
		}
		if r.BytesBefore != int64(len(contents)) {
			t.Errorf("%s: bytes before %d, want %d", name, r.BytesBefore, len(contents))
		}
		if summary.BytesBefore != 0 || summary.BytesAfter != 0 {
			t.Errorf("%s: a failed file counted in the totals: %+v", name, summary)
		}
	}
	if r := Inspect(context.Background(), write(t, filepath.Join(dir, "i.bin"), "FAKEREFUSER\n"), Config{}); r.BytesBefore != int64(len("FAKEREFUSER\n")) {
		t.Errorf("inspect: bytes before %d", r.BytesBefore)
	}
}

// A handler writes its error messages as it likes, and however it writes them, whatever
// it took from the file stays out of Reason. These are the mistakes a rule such as
// "always quote file text with %q" would not survive.
func TestReasonNeverHoldsAHandlersWords(t *testing.T) {
	const text = "Test Author"
	for name, err := range map[string]error{
		"quoted":           core.Malformedf("entry %q is bad", text),
		"not quoted":       core.Malformedf("entry %s is bad", text),
		"as bytes":         core.Unsupportedf("box %s", []byte(text)),
		"as a value":       core.Unsupportedf("box %v", text),
		"in hex":           fmt.Errorf("%w: %x", core.ErrMalformed, text),
		"unbalanced quote": core.Malformedf(`entry %s"`, `"`+text),
		"wrapped":          fmt.Errorf("part %s: %w", text, core.ErrMalformed),
		"no sentinel":      errors.New(text),
		"encrypted":        fmt.Errorf("%w: %s", core.ErrEncrypted, text),
		"too large":        fmt.Errorf("%w: %s", core.ErrTooLarge, text),
		"canceled":         fmt.Errorf("%s: %w", text, context.Canceled),
		"verification":     fmt.Errorf("%w: %w", errVerification, fromHandler{errors.New(text)}),
		"panic":            fmt.Errorf("%w: %w", errPanic, fromHandler{errors.New(text)}),
	} {
		r := failure("x", fromHandler{err})
		if strings.Contains(strings.ToLower(r.Reason), strings.ToLower(text)) {
			t.Errorf("%s: reason %q", name, r.Reason)
		}
		if strings.Contains(r.Reason, hex.EncodeToString([]byte(text))) {
			t.Errorf("%s: reason %q", name, r.Reason)
		}
		if r.Reason == "" || r.Detail == "" {
			t.Errorf("%s: %+v", name, r)
		}
		if name != "in hex" && !strings.Contains(r.Detail, text) {
			t.Errorf("%s: the handler's words are lost: %q", name, r.Detail)
		}
	}
}

// Each kind of failure keeps its stable name and gets a message of its own, through
// every way a handler's error reaches the engine.
func TestHandlerErrorsKeepTheirKindAndGetAFixedReason(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		contents, kind, reason, detail string
		status                         Status
	}{
		{"FAKEFAILER\n", KindMalformed, "malformed file: it could not be parsed", "this file is damaged", StatusFailed},
		{"FAKEREFUSER\n", KindUnsupported, "cannot clean this kind", "not handled", StatusFailed},
		{"FAKEPANICKER\n", KindInternal, "internal error", "handler bug", StatusFailed},
	} {
		src := write(t, filepath.Join(dir, c.kind+".bin"), c.contents)
		r := Clean(context.Background(), src, defaultConfig())
		if r.Status != c.status || r.Kind != c.kind {
			t.Errorf("%s: %+v", c.kind, r)
		}
		if !strings.Contains(r.Reason, c.reason) || strings.Contains(r.Reason, c.detail) || !strings.Contains(r.Detail, c.detail) {
			t.Errorf("%s: reason %q, detail %q", c.kind, r.Reason, r.Detail)
		}
	}

	// A walk's milder status for a file it cannot handle goes the same way.
	walk := filepath.Join(dir, "walk")
	write(t, filepath.Join(walk, "refused.bin"), "FAKEREFUSER\n")
	results, _ := runAll(t, OpClean, []string{walk}, Config{Recursive: true, Suffix: DefaultSuffix})
	if r := results[0]; r.Status != StatusUnsupported || strings.Contains(r.Reason, "not handled") || !strings.Contains(r.Detail, "not handled") {
		t.Errorf("walk: %+v", r)
	}

	// The engine's own refusals have no handler's words in them, and keep their full text.
	src := write(t, filepath.Join(dir, "own.good"), goodFile)
	r := Clean(context.Background(), src, Config{Suffix: DefaultSuffix})
	if r.Status != StatusCleaned {
		t.Fatalf("%+v", r)
	}
	r = Clean(context.Background(), src, Config{Suffix: DefaultSuffix})
	if r.Kind != KindOutputExists || !strings.Contains(r.Reason, "--force") || r.Detail != "" {
		t.Errorf("own refusal: %+v", r)
	}
}
