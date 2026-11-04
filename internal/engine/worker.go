package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

// worker processes one task at a time. It holds no state of its own, so one value
// serves every goroutine in the pool.
type worker struct {
	op  Op
	cfg Config
}

// process never panics, because handlers are the code most exposed to hostile
// input and one bad file must not take the rest of a batch down with it. Deferred
// cleanups run on the way through, so a panicking handler leaves no temp file.
func (w worker) process(ctx context.Context, t task) (res Result) {
	if t.preset != nil {
		return *t.preset
	}
	start := time.Now()
	res = Result{Path: t.path, Output: t.output}
	defer func() {
		if p := recover(); p != nil {
			size := res.BytesBefore
			res = failure(t.path, fmt.Errorf("%w: %w", errPanic, fromHandler{fmt.Errorf("%v", p)}))
			res.BytesBefore = size
		}
		res.Elapsed = time.Since(start)
	}()

	err := ctx.Err()
	if err == nil {
		if w.op == OpInspect {
			err = w.inspect(ctx, t, &res)
		} else {
			err = w.clean(ctx, t, &res)
		}
	}
	if err == nil {
		return res
	}
	// During a walk, a file HideMe does not handle is an expected part of a folder.
	// Named on the command line, it means the user's instruction was not carried out.
	if t.walked && errors.Is(err, core.ErrUnsupported) {
		return Result{Path: t.path, Status: StatusUnsupported, Reason: describe(err), Detail: detail(err), Kind: KindUnsupported, BytesBefore: res.BytesBefore, Elapsed: time.Since(start)}
	}
	failed := failure(t.path, err)
	failed.MayLoseContent = res.MayLoseContent
	failed.BytesBefore = res.BytesBefore
	failed.Elapsed = time.Since(start)
	return failed
}

// fileSource is a Source over an open file with its size fixed when opened.
func fileSource(f *os.File, size int64) *io.SectionReader {
	return io.NewSectionReader(f, 0, size)
}

// openInput opens path for reading after checking it is a regular file, because
// merely opening a named pipe can block forever.
func openInput(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: it is not a regular file", errInvalidInput)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		f.Close()
		return nil, nil, fmt.Errorf("%w: it changed while being opened", errInvalidInput)
	}
	return f, info, nil
}

func (w worker) inspect(ctx context.Context, t task, res *Result) error {
	in, info, err := openInput(t.path)
	if err != nil {
		return err
	}
	defer in.Close()
	res.BytesBefore = info.Size()
	src := fileSource(in, info.Size())
	h, err := core.Detect(src)
	if err != nil {
		return err
	}
	rep, err := h.Inspect(ctx, src, w.cfg.options())
	if err != nil {
		return fromHandler{err}
	}
	if rep == nil {
		return fmt.Errorf("%w: the handler returned no report", errPanic)
	}
	res.Format = rep.Format
	res.Findings = rep.Findings
	res.Warnings = rep.Warnings
	res.MayLoseContent = rep.MayLoseContent
	addFilesystemMetadata(res, t.path)
	res.Status = StatusInspected
	return nil
}

// clean follows the order in docs/DESIGN.md: the output only gets its real name
// after it has been written, synced and verified, and the source is closed first
// because Windows cannot rename over a file that is open.
func (w worker) clean(ctx context.Context, t task, res *Result) error {
	in, info, err := openInput(t.path)
	if err != nil {
		return err
	}
	inOpen := true
	closeInput := func() {
		if inOpen {
			in.Close()
			inOpen = false
		}
	}
	defer closeInput()
	res.BytesBefore = info.Size()
	src := fileSource(in, info.Size())

	h, err := core.Detect(src)
	if err != nil {
		return err
	}
	if !w.cfg.InPlace {
		if err := checkOutput(t.output, info, w.cfg.Force); err != nil {
			return w.cfg.forecast(err)
		}
	}
	dir, err := w.cfg.tempDir(t.output)
	if err != nil {
		return err
	}
	out, err := newTempOutput(dir)
	if err != nil {
		return err
	}
	defer out.discard()

	rep, err := h.Clean(ctx, src, out, w.cfg.options())
	if err != nil {
		return fromHandler{err}
	}
	if rep == nil {
		return fmt.Errorf("%w: the handler returned no report", errPanic)
	}
	res.MayLoseContent = rep.MayLoseContent
	if err := out.flushAndSync(); err != nil {
		return err
	}
	warnings, err := verify(ctx, out.file, out.size, h.Format(), rep, w.cfg.options())
	if err != nil {
		return err
	}
	if err := inputUnchanged(t.path, in, info); err != nil {
		return err
	}
	if w.cfg.InPlace && rep.MayLoseContent && !w.cfg.Force {
		return w.cfg.forecast(errMayLoseContent)
	}

	res.Format = rep.Format
	res.Findings = rep.Findings
	res.Warnings = appendUnique(rep.Warnings, warnings...)
	res.BytesAfter = out.size
	addFilesystemMetadata(res, t.path)
	if w.cfg.InPlace {
		addHardLinkWarning(res, in)
	}
	if w.cfg.DryRun {
		res.Status = StatusWouldClean
		return nil
	}

	if err := out.close(); err != nil {
		return err
	}
	closeInput()
	// Some mounted file systems refuse chmod. The temp file's mode is the private one,
	// so going on is safe, unlike with file times where the user asked for a promise.
	if err := os.Chmod(out.name, info.Mode().Perm()); err != nil {
		res.Warnings = append(res.Warnings, core.SafeText("the output could not be given the source's permissions and stays private to you: "+describe(err)))
	}
	times := w.cfg.Timestamp.times(info)
	if err := times.applyBeforePublish(out.name); err != nil {
		return err
	}
	// Last, because permissions copied from the source can stop this program changing
	// the file any further. A failure leaves the output more open than the source, which
	// the user must hear about but which is not worth losing the clean for.
	if err := copyPermissions(t.path, out.name, w.cfg.InPlace); err != nil {
		res.Warnings = append(res.Warnings, core.SafeText("the output could not be given the source's permissions, so check who can read it: "+describe(err)))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publish(out.name, t.output, w.cfg.InPlace || w.cfg.Force); err != nil {
		return err
	}
	out.published = true
	syncDir(filepath.Dir(t.output))
	if err := times.applyAfterPublish(t.output); err != nil {
		res.Warnings = append(res.Warnings, core.SafeText("the output is clean, but its creation time could not be set: "+describe(err)))
	}
	if w.cfg.InPlace && rep.MayLoseContent {
		res.Warnings = append(res.Warnings, "--force replaced the original even though the output may lack content it held")
	}
	res.Status = StatusCleaned
	return nil
}

// forecast words a refusal for a dry run, whose whole job is to say what a real run
// with the same flags would do. An output that already exists stops a real run, so it
// must stop a dry run as well, or a script that checks first is told yes and then no.
func (c Config) forecast(err error) error {
	if c.DryRun {
		return fmt.Errorf("a real run would fail: %w", err)
	}
	return err
}

// inputUnchanged catches another program writing to the source while it was being
// read. The output would then be a mix of old and new, and with --in-place the
// rename would throw away whatever the other program just saved.
func inputUnchanged(path string, in *os.File, before os.FileInfo) error {
	now, err := in.Stat()
	if err != nil {
		return err
	}
	if now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("%w: the file changed while it was being cleaned", errInvalidInput)
	}
	byName, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, byName) {
		return fmt.Errorf("%w: the file was replaced while it was being cleaned", errInvalidInput)
	}
	return nil
}

func addHardLinkWarning(res *Result, in *os.File) {
	links, err := hardLinkCount(in)
	if err == nil && links > 1 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("this file has %d names (hard links); --in-place replaces only this one, and the others still hold the original with its metadata", links))
	}
}

// addFilesystemMetadata adds what the file system holds about the file. It must
// run before an in-place replacement, which destroys it.
func addFilesystemMetadata(res *Result, path string) {
	rep := core.Report{Findings: res.Findings, Warnings: res.Warnings}
	inspectFilesystem(path, &rep)
	res.Findings = rep.Findings
	res.Warnings = rep.Warnings
}

// verify proves the temp file clean, by the three independent checks in
// docs/DESIGN.md, without trusting the report the handler just gave. It returns
// warnings to pass on to the user.
func verify(ctx context.Context, f *os.File, size int64, format core.Format, rep *core.Report, opts core.Options) ([]string, error) {
	src := fileSource(f, size)
	h, err := core.Detect(src)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: the output is no longer recognised as %s: %v", errVerification, format.ID, err)
	}
	if got := h.Format().ID; got != format.ID {
		return nil, fmt.Errorf("%w: the input was %s but the output is detected as %s", errVerification, format.ID, got)
	}
	again, err := h.Inspect(ctx, src, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the output cannot be read back: %w", errVerification, fromHandler{err})
	}
	if again == nil {
		return nil, fmt.Errorf("%w: the handler returned no report for its own output", errVerification)
	}
	if again.Format != rep.Format {
		return nil, fmt.Errorf("%w: the input was %s but the output reads as %s", errVerification, rep.Format, again.Format)
	}
	if residual := again.Residual(); len(residual) > 0 {
		return nil, fmt.Errorf("%w: metadata remains in the output: %s", errVerification, fieldNames(residual))
	}

	warnings := again.Warnings
	hits, err := scanResidue(ctx, fileSource(f, size))
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return warnings, nil
	}
	if !format.ResidueWarnOnly {
		return nil, fmt.Errorf("%w: the cleaned bytes still hold a metadata signature (%s). The original is untouched and no output was written. This can be a false alarm if the visible content itself mentions that text, as with a PDF about XMP, but HideMe cannot tell the two apart, so it does not claim the file is clean", errVerification, describeHits(hits))
	}
	return append(warnings, "the raw bytes contain a metadata signature ("+describeHits(hits)+"); this format's visible text can legitimately contain one, so check the output"), nil
}

// fieldNames names findings without their values. An error can end up in a log,
// and the values are what must not.
func fieldNames(findings []core.Finding) string {
	names := make([]string, 0, len(findings))
	for _, f := range findings {
		names = append(names, f.Field)
	}
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ", ")
}

func appendUnique(base []string, more ...string) []string {
	out := slices.Clone(base)
	for _, s := range more {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// tempDir returns where the temp file goes: the output's directory, so the final
// rename is atomic and the check for free space is the real one. A dry run must not
// create that directory, so it uses the nearest ancestor that exists, which is on
// the same file system in the normal case, after refusing what a real run would.
func (c Config) tempDir(output string) (string, error) {
	dir := filepath.Dir(output)
	if !c.DryRun {
		return dir, c.makeDirs(dir)
	}
	if err := c.checkDirs(dir); err != nil {
		return "", c.forecast(err)
	}
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return "", c.forecast(fmt.Errorf("%w: a parent of the output path is not a directory", errInvalidInput))
			}
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%w: no existing directory for the output", errInvalidInput)
		}
		dir = parent
	}
}

// makeDirs creates the directory for an output. Beneath OutputDir it goes one level at
// a time and refuses anything that is not a plain directory, because the relative path
// was safe when it was planned but what it leads to depends on what is already in the
// output directory: a symlink or junction there would redirect the write somewhere
// else. Creating the whole path first and looking afterwards would already have made
// directories out there.
func (c Config) makeDirs(dir string) error {
	if c.OutputDir == "" {
		return os.MkdirAll(dir, 0o755)
	}
	if err := os.MkdirAll(c.OutputDir, 0o755); err != nil {
		return err
	}
	return c.walkOutputDirs(dir, func(current string) error {
		// Other workers are making the same directories, so finding one is normal.
		if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		return nil
	})
}

// checkDirs is makeDirs with nothing created, for a dry run.
func (c Config) checkDirs(dir string) error {
	if c.OutputDir == "" {
		return nil
	}
	return c.walkOutputDirs(dir, func(string) error { return nil })
}

// walkOutputDirs visits each directory from OutputDir down to dir, refusing anything
// that is not a plain directory. What is not there yet is fine and ends the check,
// since nothing can be wrong beneath it.
func (c Config) walkOutputDirs(dir string, visit func(string) error) error {
	rel, err := filepath.Rel(c.OutputDir, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := c.OutputDir
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := visit(current); err != nil {
			return err
		}
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: a path inside the output directory is not a plain directory; it may be a link leading elsewhere", errInvalidInput)
		}
	}
	return nil
}
