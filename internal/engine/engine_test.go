package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

func TestCleanWritesNextToTheSource(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	want := filepath.Join(dir, "photo.clean.good")
	if r.Output != want {
		t.Errorf("output %q, want %q", r.Output, want)
	}
	if got := read(t, want); got != goodCleaned {
		t.Errorf("output holds %q", got)
	}
	if got := read(t, src); got != goodFile {
		t.Errorf("the original changed: %q", got)
	}
	if r.Format != "good" || r.BytesBefore != int64(len(goodFile)) || r.BytesAfter != int64(len(goodCleaned)) {
		t.Errorf("wrong details: %+v", r)
	}
	if len(r.Findings) != 1 || r.Findings[0].Field != "Secret" {
		t.Errorf("findings: %+v", r.Findings)
	}
	assertNoTemp(t, dir)
}

func TestFileWithNothingIsStillRewrittenAndReported(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "plain.good"), goodCleaned)

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusCleaned || !r.NothingFound() {
		t.Fatalf("status %q, nothing found %v: %s", r.Status, r.NothingFound(), r.Reason)
	}
	if !exists(filepath.Join(dir, "plain.clean.good")) {
		t.Error("no output was written")
	}
}

// TestLyingHandlerOutputIsDiscarded is the reason the engine exists as a separate
// layer: a handler that is wrong must fail loudly, and the original must survive.
func TestLyingHandlerOutputIsDiscarded(t *testing.T) {
	cases := []struct {
		name, contents, wantReason string
	}{
		{"residue scan catches what Inspect misses", "FAKELIAR\n<x:xmpmeta>Test Author</x:xmpmeta>\n", "<x:xmpmeta"},
		{"residue scan catches UTF-16 XMP", "FAKELIAR\n" + string(utf16Bytes("<?xpacket", 0)), "UTF-16LE"},
		{"residue scan catches Photoshop", "FAKELIAR\nPhotoshop 3.0\n", "Photoshop 3.0"},
		{"Inspect on the output", "FAKEBLIND\nSECRET=Test Author\n", "metadata remains in the output: Secret"},
		{"format changed", "FAKESHIFTY\n", "detected as good"},
		{"output unrecognisable", "FAKEGARBLER\n", "no longer recognised"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := write(t, filepath.Join(dir, "lie.bin"), c.contents)

			r := Clean(context.Background(), src, defaultConfig())

			if r.Status != StatusFailed || r.Kind != KindVerification {
				t.Fatalf("status %q kind %q: %s", r.Status, r.Kind, r.Reason)
			}
			if !strings.Contains(r.Reason, c.wantReason) {
				t.Errorf("reason %q does not mention %q", r.Reason, c.wantReason)
			}
			if strings.Contains(r.Reason, "Test Author") {
				t.Errorf("the reason repeats a value it should hide: %q", r.Reason)
			}
			if r.Output != "" || exists(filepath.Join(dir, "lie.clean.bin")) {
				t.Error("an output was left behind")
			}
			if got := read(t, src); got != c.contents {
				t.Errorf("the original changed: %q", got)
			}
			assertNoTemp(t, dir)
		})
	}
}

func TestLyingHandlerCannotReplaceAnOriginalInPlace(t *testing.T) {
	dir := t.TempDir()
	const contents = "FAKELIAR\n<x:xmpmeta>\n"
	src := write(t, filepath.Join(dir, "lie.bin"), contents)

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusFailed {
		t.Fatalf("status %q", r.Status)
	}
	if got := read(t, src); got != contents {
		t.Errorf("the original changed: %q", got)
	}
	assertNoTemp(t, dir)
}

func TestResidueWarnOnlyFormatsWarn(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "quote.bin"), "FAKEQUOTER\nthe text says <x:xmpmeta here\n")

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "<x:xmpmeta at byte 25") {
		t.Errorf("warnings: %q", r.Warnings)
	}
}

func TestHandlerWarningsAreKept(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "w.bin"), "FAKEWARNER\n")

	r := Clean(context.Background(), src, defaultConfig())

	if len(r.Warnings) != 1 || r.Warnings[0] != "a part of this file could not be cleaned" {
		t.Errorf("warnings: %q", r.Warnings)
	}
}

func TestPanickingHandlerFailsOneFileOnly(t *testing.T) {
	dir := t.TempDir()
	bad := write(t, filepath.Join(dir, "bad.bin"), "FAKEPANICKER\n")
	good := write(t, filepath.Join(dir, "good.good"), goodFile)

	results, summary := runAll(t, OpClean, []string{bad, good}, defaultConfig())

	if results[0].Status != StatusFailed || results[0].Kind != KindInternal {
		t.Errorf("first result: %+v", results[0])
	}
	if results[1].Status != StatusCleaned {
		t.Errorf("second result: %+v", results[1])
	}
	if summary.Failed != 1 || summary.Cleaned != 1 {
		t.Errorf("summary: %+v", summary)
	}
	assertNoTemp(t, dir)

	r := Inspect(context.Background(), bad, Config{})
	if r.Status != StatusFailed || r.Kind != KindInternal {
		t.Errorf("Inspect: %+v", r)
	}
}

func TestFailedCleanLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "broken.bin"), "FAKEFAILER\n")

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusFailed || r.Kind != KindMalformed {
		t.Fatalf("status %q kind %q", r.Status, r.Kind)
	}
	if exists(filepath.Join(dir, "broken.clean.bin")) {
		t.Error("an output was left behind")
	}
	assertNoTemp(t, dir)
}

func TestCancelMidCleanRemovesTheTempFile(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "slow.bin"), "FAKEHOOKED\ncontent\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawTemp bool
	cleanHook = func(ctx context.Context) error {
		matches, err := filepath.Glob(filepath.Join(dir, tempPrefix+"*"))
		if err != nil {
			return err
		}
		sawTemp = len(matches) == 1
		cancel()
		return ctx.Err()
	}
	defer func() { cleanHook = nil }()

	r := Clean(ctx, src, defaultConfig())

	if !sawTemp {
		t.Error("the temp file was not in the destination directory during the clean")
	}
	if r.Status != StatusFailed || r.Kind != KindCanceled {
		t.Errorf("status %q kind %q", r.Status, r.Kind)
	}
	if exists(filepath.Join(dir, "slow.clean.bin")) {
		t.Error("an output was left behind")
	}
	assertNoTemp(t, dir)
}

func TestCanceledRunStillReportsEveryInput(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"a.good", "b.good", "c.good"} {
		paths = append(paths, write(t, filepath.Join(dir, name), goodFile))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var results []Result
	summary, err := Run(ctx, OpClean, paths, defaultConfig(), func(r Result) { results = append(results, r) })
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || summary.Failed != 3 {
		t.Fatalf("results %+v, summary %+v", results, summary)
	}
	for _, r := range results {
		if r.Kind != KindCanceled {
			t.Errorf("kind %q", r.Kind)
		}
	}
	assertNoTemp(t, dir)
}

func TestSourceChangingDuringCleanIsRefused(t *testing.T) {
	dir := t.TempDir()
	const contents = "FAKEHOOKED\ncontent\n"
	src := write(t, filepath.Join(dir, "moving.bin"), contents)
	const added = "another program saved this\n"
	cleanHook = func(context.Context) error {
		f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(added)
		return err
	}
	defer func() { cleanHook = nil }()

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusFailed || r.Kind != KindInvalidInput {
		t.Fatalf("status %q kind %q: %s", r.Status, r.Kind, r.Reason)
	}
	if got := read(t, src); got != contents+added {
		t.Errorf("--in-place lost what the other program saved: %q", got)
	}
}

func TestExistingOutputIsNotOverwrittenWithoutForce(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	out := write(t, filepath.Join(dir, "photo.clean.good"), "already here")

	r := Clean(context.Background(), src, defaultConfig())
	if r.Status != StatusFailed || r.Kind != KindOutputExists {
		t.Fatalf("status %q kind %q", r.Status, r.Kind)
	}
	if got := read(t, out); got != "already here" {
		t.Errorf("the existing output changed: %q", got)
	}

	cfg := defaultConfig()
	cfg.Force = true
	r = Clean(context.Background(), src, cfg)
	if r.Status != StatusCleaned {
		t.Fatalf("with force: status %q: %s", r.Status, r.Reason)
	}
	if got := read(t, out); got != goodCleaned {
		t.Errorf("force did not replace the output: %q", got)
	}
	assertNoTemp(t, dir)
}

func TestPublishNeverReplacesWithoutPermission(t *testing.T) {
	dir := t.TempDir()
	temp := write(t, filepath.Join(dir, "temp"), "new")
	out := write(t, filepath.Join(dir, "out"), "old")

	if err := publish(temp, out, false); !errors.Is(err, errOutputExists) {
		t.Errorf("publish over an existing file: %v", err)
	}
	if got := read(t, out); got != "old" {
		t.Errorf("the existing file changed: %q", got)
	}
	if err := publish(temp, filepath.Join(dir, "fresh"), false); err != nil {
		t.Errorf("publish to a new name: %v", err)
	}
	if got := read(t, filepath.Join(dir, "fresh")); got != "new" {
		t.Errorf("published file holds %q", got)
	}
}

func TestOutputThatIsTheSourceIsRefusedEvenWithForce(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)

	// An empty suffix with -o pointing back at the source's own directory.
	r := Clean(context.Background(), src, Config{OutputDir: dir, Force: true})
	if r.Status != StatusFailed || r.Kind != KindSameFile {
		t.Errorf("same directory: status %q kind %q", r.Status, r.Kind)
	}
	if got := read(t, src); got != goodFile {
		t.Errorf("the source changed: %q", got)
	}
}

// TestSameFileUsesTheFileSystemNotTheName proves the comparison survives a name that
// differs only in case, which is the same file on Windows and on macOS by default,
// and a different name for the same file through a hard link everywhere.
func TestSameFileUsesTheFileSystemNotTheName(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS == "windows" {
		upper := filepath.Join(dir, "PHOTO.GOOD")
		if err := checkOutput(upper, info, true); !errors.Is(err, errSameFile) {
			t.Errorf("name differing only in case: %v", err)
		}
	}

	link := filepath.Join(dir, "other-name.good")
	if err := os.Link(src, link); err != nil {
		t.Skipf("hard links not available: %v", err)
	}
	if err := checkOutput(link, info, true); !errors.Is(err, errSameFile) {
		t.Errorf("hard link: %v", err)
	}
}

func TestOutputDirMirrorsStructure(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "photos")
	write(t, filepath.Join(tree, "top.good"), goodFile)
	write(t, filepath.Join(tree, "2024", "trip", "deep.good"), goodFile)
	single := write(t, filepath.Join(root, "elsewhere", "single.good"), goodFile)
	out := filepath.Join(root, "out")

	cfg := Config{OutputDir: out, Recursive: true}
	results, summary := runAll(t, OpClean, []string{tree, single}, cfg)

	if summary.Cleaned != 3 {
		t.Fatalf("results: %+v", results)
	}
	for _, want := range []string{
		filepath.Join(out, "photos", "top.good"),
		filepath.Join(out, "photos", "2024", "trip", "deep.good"),
		filepath.Join(out, "single.good"),
	} {
		if got := read(t, want); got != goodCleaned {
			t.Errorf("%s holds %q", want, got)
		}
	}
}

func TestOutputDirWithDotRootHasNoPrefix(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "in", "sub", "a.good"), goodFile)
	out := filepath.Join(root, "out")
	t.Chdir(filepath.Join(root, "in"))

	_, summary := runAll(t, OpClean, []string{"."}, Config{OutputDir: out, Recursive: true})

	if summary.Cleaned != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	if !exists(filepath.Join(out, "sub", "a.good")) {
		t.Error("the tree was not mirrored directly beneath the output directory")
	}
}

func TestPathTraversalCannotEscapeOutputDir(t *testing.T) {
	root := t.TempDir()
	src := write(t, filepath.Join(root, "a", "b", "victim.good"), goodFile)
	out := filepath.Join(root, "a", "b", "out")
	t.Chdir(filepath.Join(root, "a", "b"))

	// A path full of ".." still lands directly in the output directory.
	r := Clean(context.Background(), filepath.Join("..", "b", "victim.good"), Config{OutputDir: out})
	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if r.Output != filepath.Join(out, "victim.good") {
		t.Errorf("output %q", r.Output)
	}

	r = Clean(context.Background(), src, Config{OutputDir: out})
	if r.Kind != KindOutputExists {
		t.Errorf("the second run should find the output already there: %+v", r)
	}

	for _, suffix := range []string{"/../../escape", `\..\escape`, "a:b", "evil\x1b[31m"} {
		if err := (Config{Suffix: suffix}).Validate(OpClean); err == nil {
			t.Errorf("suffix %q was accepted", suffix)
		}
	}
}

func TestOutputDirSymlinkCannotRedirectWrites(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(out, "tree")); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "a.good"), goodFile)

	results, _ := runAll(t, OpClean, []string{tree}, Config{OutputDir: out, Recursive: true})

	if len(results) != 1 || results[0].Status != StatusFailed || results[0].Kind != KindInvalidInput {
		t.Fatalf("results: %+v", results)
	}
	if exists(filepath.Join(outside, "a.good")) {
		t.Error("the write followed a symlink out of the output directory")
	}
	assertNoTemp(t, outside)
}

func TestTwoInputsCannotClaimOneOutputEvenWithForce(t *testing.T) {
	root := t.TempDir()
	first := write(t, filepath.Join(root, "x", "same.good"), "FAKEGOOD\nfrom x\n")
	second := write(t, filepath.Join(root, "y", "same.good"), "FAKEGOOD\nfrom y\n")
	out := filepath.Join(root, "out")

	results, summary := runAll(t, OpClean, []string{first, second}, Config{OutputDir: out, Force: true})

	if summary.Cleaned != 1 || summary.Failed != 1 {
		t.Fatalf("results: %+v", results)
	}
	if results[1].Kind != KindCollision {
		t.Errorf("second result: %+v", results[1])
	}
	if got := read(t, filepath.Join(out, "same.good")); got != "FAKEGOOD\nfrom x\n" {
		t.Errorf("the output holds %q", got)
	}
}

func TestOutputCannotBeAnotherInput(t *testing.T) {
	dir := t.TempDir()
	first := write(t, filepath.Join(dir, "a.good"), goodFile)
	second := write(t, filepath.Join(dir, "a.clean.good"), "FAKEGOOD\nprecious\n")

	cfg := defaultConfig()
	cfg.Force = true
	results, _ := runAll(t, OpClean, []string{first, second}, cfg)

	if results[0].Kind != KindCollision {
		t.Errorf("first result: %+v", results[0])
	}
	if got := read(t, second); got != "FAKEGOOD\nprecious\n" {
		t.Errorf("an input was overwritten: %q", got)
	}
}

func TestDuplicateInputsAreProcessedOnce(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	cfg := defaultConfig()
	cfg.RandomName = true

	results, summary := runAll(t, OpClean, []string{src, filepath.Join(dir, ".", "a.good")}, cfg)

	if len(results) != 1 || summary.Cleaned != 1 {
		t.Fatalf("results: %+v", results)
	}
}

func TestInPlaceReplacesTheFile(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusCleaned || r.Output != src {
		t.Fatalf("status %q output %q: %s", r.Status, r.Output, r.Reason)
	}
	if got := read(t, src); got != goodCleaned {
		t.Errorf("the file holds %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries", len(entries))
	}
}

func TestInPlaceRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	real := write(t, filepath.Join(dir, "real.good"), goodFile)
	link := filepath.Join(dir, "link.good")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}

	r := Clean(context.Background(), link, Config{InPlace: true})

	if r.Status != StatusFailed || r.Kind != KindInvalidInput {
		t.Fatalf("status %q kind %q", r.Status, r.Kind)
	}
	if got := read(t, real); got != goodFile {
		t.Errorf("the target changed: %q", got)
	}
}

func TestExplicitSymlinkIsCleanedWithoutInPlace(t *testing.T) {
	dir := t.TempDir()
	real := write(t, filepath.Join(dir, "real.good"), goodFile)
	link := filepath.Join(dir, "link.good")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}

	r := Clean(context.Background(), link, defaultConfig())

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if got := read(t, filepath.Join(dir, "link.clean.good")); got != goodCleaned {
		t.Errorf("output holds %q", got)
	}
}

func TestInPlaceWarnsAboutHardLinks(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	other := filepath.Join(dir, "b.good")
	if err := os.Link(src, other); err != nil {
		t.Skipf("hard links not available: %v", err)
	}

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "2 names") {
		t.Errorf("warnings: %q", r.Warnings)
	}
	if got := read(t, other); got != goodFile {
		t.Errorf("the other name changed, so this test is not testing what it should: %q", got)
	}
}

func TestRandomNameKeepsOnlyTheExtension(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "IMG_20240101_123456.good"), goodFile)

	r := Clean(context.Background(), src, Config{OutputDir: filepath.Join(dir, "out"), RandomName: true})

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	name := filepath.Base(r.Output)
	stem, ok := strings.CutSuffix(name, ".good")
	if !ok || len(stem) != 32 {
		t.Fatalf("name %q", name)
	}
	if _, err := hex.DecodeString(stem); err != nil {
		t.Errorf("stem %q is not hex", stem)
	}
	if strings.Contains(r.Output, "IMG_") {
		t.Errorf("the original name leaked: %q", r.Output)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	src := write(t, filepath.Join(root, "photo.good"), goodFile)
	out := filepath.Join(root, "not", "yet", "there")

	r := Clean(context.Background(), src, Config{OutputDir: out, DryRun: true})

	if r.Status != StatusWouldClean || r.NothingFound() {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if r.BytesAfter != int64(len(goodCleaned)) {
		t.Errorf("bytes after: %d", r.BytesAfter)
	}
	if exists(filepath.Join(root, "not")) {
		t.Error("a dry run created the output directory")
	}
	assertNoTemp(t, root)

	r = Clean(context.Background(), write(t, filepath.Join(root, "lie.bin"), "FAKELIAR\n<?xpacket\n"), Config{OutputDir: out, DryRun: true})
	if r.Status != StatusFailed || r.Kind != KindVerification {
		t.Errorf("a dry run must predict a verification failure: %+v", r)
	}
	assertNoTemp(t, root)
}

func TestWalkSkipsAndCounts(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.good"), goodFile)
	write(t, filepath.Join(root, "b.clean.good"), goodCleaned)
	write(t, filepath.Join(root, "c.txt"), "not a format HideMe knows")
	write(t, filepath.Join(root, "d.bin"), "FAKEREFUSER\n")
	write(t, filepath.Join(root, tempPrefix+"123"+tempSuffix), "left by a crash")
	write(t, filepath.Join(root, "sub", "e.good"), goodFile)

	results, summary := runAll(t, OpClean, []string{root}, Config{Suffix: DefaultSuffix, Recursive: true})

	byName := make(map[string]Result)
	for _, r := range results {
		byName[filepath.Base(r.Path)] = r
	}
	for name, want := range map[string]Status{
		"a.good":                        StatusCleaned,
		"b.clean.good":                  StatusSkipped,
		"c.txt":                         StatusUnsupported,
		"d.bin":                         StatusUnsupported,
		tempPrefix + "123" + tempSuffix: StatusSkipped,
		"e.good":                        StatusCleaned,
	} {
		if byName[name].Status != want {
			t.Errorf("%s: status %q, want %q", name, byName[name].Status, want)
		}
	}
	if summary.Cleaned != 2 || summary.Skipped != 2 || summary.Unsupported != 2 || summary.Failed != 0 {
		t.Errorf("summary: %+v", summary)
	}
	if !exists(filepath.Join(root, "a.clean.good")) || exists(filepath.Join(root, "b.clean.clean.good")) {
		t.Error("wrong outputs")
	}

	// A re-run must not build photo.clean.clean.jpg, and must say why it did nothing.
	results, summary = runAll(t, OpClean, []string{root}, Config{Suffix: DefaultSuffix, Recursive: true})
	if exists(filepath.Join(root, "a.clean.clean.good")) {
		t.Error("a re-run cleaned an earlier output")
	}
	if summary.Cleaned != 0 || summary.Failed != 2 {
		t.Errorf("re-run: %+v %+v", summary, results)
	}
}

func TestWalkDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outsideDir := filepath.Join(root, "outside")
	secret := write(t, filepath.Join(outsideDir, "secret.good"), goodFile)
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "a.good"), goodFile)
	if err := os.Symlink(secret, filepath.Join(tree, "file-link.good")); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(tree, "dir-link")); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}

	results, summary := runAll(t, OpClean, []string{tree}, Config{Suffix: DefaultSuffix, Recursive: true})

	if summary.Cleaned != 1 || summary.Skipped != 2 {
		t.Fatalf("summary %+v, results %+v", summary, results)
	}
	for _, r := range results {
		if r.Status == StatusSkipped && r.Kind != KindSymlink {
			t.Errorf("kind %q for %s", r.Kind, r.Path)
		}
	}
	if exists(filepath.Join(outsideDir, "secret.clean.good")) {
		t.Error("a symlink was followed")
	}
}

// fakeEntry lets the walk's decision be tested where links cannot be created, as on
// Windows without Developer Mode, so that it is always exercised somewhere.
type fakeEntry struct {
	name string
	mode os.FileMode
}

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return e.mode.IsDir() }
func (e fakeEntry) Type() os.FileMode          { return e.mode.Type() }
func (e fakeEntry) Info() (os.FileInfo, error) { return nil, errors.New("not used") }

func TestWalkDecidesFromTheEntryType(t *testing.T) {
	for _, c := range []struct {
		name string
		mode os.FileMode
		kind string
	}{
		{"link.good", os.ModeSymlink, KindSymlink},
		{"pipe.good", os.ModeNamedPipe, KindSpecial},
		{"socket.good", os.ModeSocket, KindSpecial},
		{"device.good", os.ModeDevice, KindSpecial},
		{"junction", os.ModeIrregular, KindSpecial},
	} {
		p := &planner{ctx: context.Background(), op: OpClean, cfg: defaultConfig(), seen: make(map[string][]string)}
		p.visitEntry("root", "root", filepath.Join("root", c.name), fakeEntry{c.name, c.mode})
		if len(p.tasks) != 1 || p.tasks[0].preset == nil || p.tasks[0].preset.Kind != c.kind || p.tasks[0].preset.Status != StatusSkipped {
			t.Errorf("%s: tasks %+v", c.name, p.tasks)
		}
	}
}

func TestSymlinkToDirectoryOnCommandLineIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	write(t, filepath.Join(dir, "a.good"), goodFile)
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}

	results, _ := runAll(t, OpClean, []string{link}, Config{Suffix: DefaultSuffix, Recursive: true})

	if len(results) != 1 || results[0].Kind != KindInvalidInput {
		t.Fatalf("results: %+v", results)
	}
}

func TestWalkSkipsOutputDirectoryUnderTheInput(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.good"), goodFile)
	out := filepath.Join(root, "cleaned")

	cfg := Config{OutputDir: out, Recursive: true}
	results, summary := runAll(t, OpClean, []string{root}, cfg)
	if summary.Cleaned != 1 || summary.Skipped != 0 {
		t.Fatalf("first run, with no output directory yet: %+v", results)
	}

	// Now it holds files, which must not become inputs. They were not cleaned by this
	// run, and exit 0 means every file was, so it is listed.
	cfg.Force = true
	results, summary = runAll(t, OpClean, []string{root}, cfg)
	if summary.Cleaned != 1 || summary.Skipped != 1 {
		t.Fatalf("second run: %+v %+v", summary, results)
	}
	base := filepath.Base(root)
	if exists(filepath.Join(out, base, "cleaned")) {
		t.Error("the output directory was walked into")
	}
	skippedDir := slices.IndexFunc(results, func(r Result) bool { return r.Kind == KindOutputDir })
	if skippedDir < 0 {
		t.Errorf("the output directory was not listed: %+v", results)
	}

	// An empty output directory holds nothing that was left out, so it says nothing.
	emptyRoot := t.TempDir()
	write(t, filepath.Join(emptyRoot, "a.good"), goodFile)
	emptyOut := filepath.Join(emptyRoot, "out")
	if err := os.Mkdir(emptyOut, 0o755); err != nil {
		t.Fatal(err)
	}
	_, summary = runAll(t, OpClean, []string{emptyRoot}, Config{OutputDir: emptyOut, Recursive: true})
	if summary.Skipped != 0 || summary.Cleaned != 1 {
		t.Errorf("empty output directory: %+v", summary)
	}
}

func TestInputsThatCannotBeProcessed(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, "a.good"), goodFile)
	unknown := write(t, filepath.Join(dir, "plain.txt"), "just text")
	refused := write(t, filepath.Join(dir, "refused.bin"), "FAKEREFUSER\n")

	cases := []struct {
		name, path string
		wantKind   string
		wantReason string
	}{
		{"directory without -r", sub, KindInvalidInput, "use -r"},
		{"missing file", filepath.Join(dir, "missing.good"), KindNotFound, ""},
		{"unsupported named explicitly", unknown, KindUnsupported, "no handler"},
		{"handler refuses, named explicitly", refused, KindUnsupported, "cannot clean this kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			results, summary := runAll(t, OpClean, []string{c.path}, defaultConfig())
			if len(results) != 1 {
				t.Fatalf("results: %+v", results)
			}
			r := results[0]
			if r.Status != StatusFailed || r.Kind != c.wantKind {
				t.Errorf("status %q kind %q, want failed %q", r.Status, r.Kind, c.wantKind)
			}
			if !strings.Contains(r.Reason, c.wantReason) {
				t.Errorf("reason %q does not mention %q", r.Reason, c.wantReason)
			}
			if summary.Failed != 1 {
				t.Errorf("summary: %+v", summary)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	file := write(t, filepath.Join(dir, "a-file"), "x")
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"in-place with output dir", Config{InPlace: true, OutputDir: dir}},
		{"in-place with random name", Config{InPlace: true, RandomName: true}},
		{"in-place with suffix", Config{InPlace: true, Suffix: ".x"}},
		{"empty suffix beside the source", Config{}},
		{"output dir is a file", Config{OutputDir: file}},
		{"negative jobs", Config{Suffix: ".x", Jobs: -1}},
	} {
		if err := c.cfg.Validate(OpClean); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if _, err := Run(context.Background(), OpClean, nil, Config{}, func(Result) {}); err == nil {
		t.Error("Run accepted a bad configuration")
	}
	if err := (Config{}).Validate(OpInspect); err != nil {
		t.Errorf("Inspect needs no output settings: %v", err)
	}
}

func TestInspectChangesNothingAndReports(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	r := Inspect(context.Background(), src, Config{})

	if r.Status != StatusInspected || len(r.Findings) != 1 || r.Findings[0].Value != "Test Author" {
		t.Fatalf("result: %+v", r)
	}
	if r.Output != "" {
		t.Errorf("Inspect named an output: %q", r.Output)
	}
	after, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || read(t, src) != goodFile {
		t.Error("Inspect changed the file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Inspect created files: %v", entries)
	}
}

func TestInspectWalkDoesNotSkipEarlierOutputs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.clean.good"), goodFile)
	_, summary := runAll(t, OpInspect, []string{root}, Config{Recursive: true})
	if summary.Inspected != 1 || summary.Skipped != 0 {
		t.Errorf("summary: %+v", summary)
	}
}

// TestResultsKeepInputOrderAndArriveEarly checks both halves of the promise: order
// never depends on completion, and a finished first file is reported while a later
// one is still running, so a long batch does not go quiet.
func TestResultsKeepInputOrderAndArriveEarly(t *testing.T) {
	dir := t.TempDir()
	first := write(t, filepath.Join(dir, "1.good"), goodFile)
	second := write(t, filepath.Join(dir, "2.bin"), "FAKEHOOKED\n")
	third := write(t, filepath.Join(dir, "3.good"), goodFile)

	emittedFirst := make(chan struct{})
	cleanHook = func(context.Context) error {
		select {
		case <-emittedFirst:
			return nil
		case <-time.After(30 * time.Second):
			return errors.New("the first result was held back until the batch finished")
		}
	}
	defer func() { cleanHook = nil }()

	var got []string
	cfg := defaultConfig()
	cfg.Jobs = 2
	summary, err := Run(context.Background(), OpClean, []string{first, second, third}, cfg, func(r Result) {
		got = append(got, filepath.Base(r.Path))
		if len(got) == 1 {
			close(emittedFirst)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"1.good", "2.bin", "3.good"}) {
		t.Errorf("order: %q", got)
	}
	if summary.Failed != 0 || summary.Cleaned != 3 {
		t.Errorf("summary: %+v", summary)
	}
}

func TestSummaryAddsUpBytes(t *testing.T) {
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.good"), goodFile)
	b := write(t, filepath.Join(dir, "b.good"), goodFile)

	_, s := runAll(t, OpClean, []string{a, b}, defaultConfig())

	if s.Files != 2 || s.BytesBefore != 2*int64(len(goodFile)) || s.BytesAfter != 2*int64(len(goodCleaned)) {
		t.Errorf("summary: %+v", s)
	}
}

func TestTimestampModes(t *testing.T) {
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	setup := func(t *testing.T) (string, string) {
		dir := t.TempDir()
		src := write(t, filepath.Join(dir, "a.good"), goodFile)
		if err := os.Chtimes(src, old, old); err != nil {
			t.Fatal(err)
		}
		return src, filepath.Join(dir, "a.clean.good")
	}

	t.Run("now is the default", func(t *testing.T) {
		src, out := setup(t)
		before := time.Now().Add(-time.Minute)
		if r := Clean(context.Background(), src, defaultConfig()); r.Status != StatusCleaned {
			t.Fatalf("%+v", r)
		}
		info, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Before(before) {
			t.Errorf("modified %v", info.ModTime())
		}
	})

	t.Run("zero", func(t *testing.T) {
		src, out := setup(t)
		cfg := defaultConfig()
		cfg.Timestamp = TimestampZero
		if r := Clean(context.Background(), src, cfg); r.Status != StatusCleaned {
			t.Fatalf("%+v", r)
		}
		info, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(ZeroTime) {
			t.Errorf("modified %v", info.ModTime())
		}
		access, _ := sourceTimes(info)
		if !access.Equal(ZeroTime) {
			t.Errorf("accessed %v", access)
		}
	})

	t.Run("keep", func(t *testing.T) {
		src, out := setup(t)
		cfg := defaultConfig()
		cfg.Timestamp = TimestampKeep
		if r := Clean(context.Background(), src, cfg); r.Status != StatusCleaned {
			t.Fatalf("%+v", r)
		}
		info, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(old) {
			t.Errorf("modified %v", info.ModTime())
		}
	})

	t.Run("in place with zero", func(t *testing.T) {
		src, _ := setup(t)
		if r := Clean(context.Background(), src, Config{InPlace: true, Timestamp: TimestampZero}); r.Status != StatusCleaned {
			t.Fatalf("%+v", r)
		}
		info, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(ZeroTime) {
			t.Errorf("modified %v", info.ModTime())
		}
	})
}

func TestParseTimestamp(t *testing.T) {
	for s, want := range map[string]Timestamp{"now": TimestampNow, "zero": TimestampZero, "keep": TimestampKeep} {
		if got, err := ParseTimestamp(s); err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	if _, err := ParseTimestamp("yesterday"); err == nil {
		t.Error("a bad mode was accepted")
	}
}

func TestPermissionBitsAreCopied(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	if err := os.Chmod(src, 0o444); err != nil {
		t.Fatal(err)
	}

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	want, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(r.Output)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode().Perm() != want.Mode().Perm() {
		t.Errorf("mode %v, want %v", got.Mode().Perm(), want.Mode().Perm())
	}
}

func TestInPlaceOnReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	if err := os.Chmod(src, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(src, 0o644) })

	r := Clean(context.Background(), src, Config{InPlace: true})

	// Replacing a file is an operation on its directory, and read-only protects
	// contents, so a user who said --in-place about this file gets what they asked for.
	if r.Status != StatusCleaned {
		t.Fatalf("status %q kind %q: %s", r.Status, r.Kind, r.Reason)
	}
	if got := read(t, src); got != goodCleaned {
		t.Errorf("the file holds %q", got)
	}
	assertNoTemp(t, dir)
}

func TestDescribeHidesPaths(t *testing.T) {
	err := &os.PathError{Op: "open", Path: `C:\very\long\private\path`, Err: errors.New("denied")}
	if got := describe(err); got != "open: denied" {
		t.Errorf("describe: %q", got)
	}
	if got := describe(errors.New("bad \x1b[31mcolour")); strings.ContainsRune(got, 0x1b) {
		t.Errorf("describe kept an escape: %q", got)
	}
}

func TestReasonsNeverCarryControlCharacters(t *testing.T) {
	r := failure("x", core.Malformedf("value \x1b]0;evil\x07 here"))
	if strings.ContainsAny(r.Reason, "\x1b\x07") {
		t.Errorf("reason: %q", r.Reason)
	}
}
