package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/axiom8z/hideme/internal/core"
)

// task is one input with the output decided for it. preset is set when planning
// already settled the outcome, as for a skipped symlink, so the result still takes
// its place in the output order.
type task struct {
	path   string
	rel    string // where the file goes beneath OutputDir, before renaming
	walked bool   // found by walking, not named by the user
	output string
	preset *Result
}

// plan turns the paths on the command line into tasks. Everything happens up
// front, before any work starts, for two reasons: a skipped output directory must
// be judged before this run writes into it, and outputs that collide must be
// caught before either is written, because with --force the second would silently
// overwrite the first.
func plan(ctx context.Context, op Op, paths []string, cfg Config) []task {
	p := &planner{ctx: ctx, op: op, cfg: cfg, seen: make(map[string][]string)}
	if op == OpClean && cfg.OutputDir != "" {
		if info, err := os.Stat(cfg.OutputDir); err == nil {
			p.outputDir = info
		}
	}
	for _, path := range paths {
		p.addRoot(path)
	}
	if op == OpClean {
		p.assignOutputs()
	}
	return p.tasks
}

// expandWildcards does what cmd.exe and PowerShell leave to each program. The
// characters cannot appear in a Windows file name, so there is no ambiguity, and
// on other systems the shell has already done this. A pattern that matches nothing
// stays as it is, to fail with a clear error.
func expandWildcards(paths []string) []string {
	var out []string
	for _, path := range paths {
		if !isPattern(path) {
			out = append(out, path)
			continue
		}
		matches, err := filepath.Glob(windowsPattern(path))
		if err != nil || len(matches) == 0 {
			out = append(out, path)
			continue
		}
		out = append(out, matches...)
	}
	return out
}

// isPattern reports a path HideMe must expand itself. The "?" of a \\?\ prefix is no
// wildcard.
func isPattern(path string) bool {
	return runtime.GOOS == "windows" && strings.ContainsAny(path[len(filepath.VolumeName(path)):], "*?")
}

// windowsPattern rewrites a pattern for filepath.Glob so that it means what it means
// to everything else on Windows: letters match in either case, or "*.jpg" would miss
// the camera's IMG_0001.JPG and say nothing, and "[" is an ordinary character. Only the
// path elements that need it change, because Glob has to read the directory holding
// each element it treats as a pattern, and the user may not be allowed to read those
// further up.
func windowsPattern(path string) string {
	volume := filepath.VolumeName(path)
	var b strings.Builder
	b.WriteString(volume)
	element := func(s string) {
		if !strings.ContainsAny(s, "*?[") {
			b.WriteString(s)
			return
		}
		for _, r := range s {
			lower, upper := unicode.ToLower(r), unicode.ToUpper(r)
			switch {
			case r == '[':
				b.WriteString("[[]")
			case lower != upper:
				b.WriteRune('[')
				b.WriteRune(lower)
				b.WriteRune(upper)
				b.WriteRune(']')
			default:
				b.WriteRune(r)
			}
		}
	}
	start := len(volume)
	for i := start; i < len(path); i++ {
		if os.IsPathSeparator(path[i]) {
			element(path[start:i])
			b.WriteByte(path[i])
			start = i + 1
		}
	}
	element(path[start:])
	return b.String()
}

type planner struct {
	ctx       context.Context
	op        Op
	cfg       Config
	outputDir os.FileInfo // nil when it does not exist yet
	seen      map[string][]string
	tasks     []task
}

func (p *planner) settle(r Result) {
	p.tasks = append(p.tasks, task{path: r.Path, preset: &r})
}

func (p *planner) fail(path string, err error) { p.settle(failure(path, err)) }

func (p *planner) invalid(path, format string, args ...any) {
	p.fail(path, fmt.Errorf("%w: %s", errInvalidInput, fmt.Sprintf(format, args...)))
}

// add queues a file once, so overlapping arguments such as "-r dir dir/sub" do not
// process it twice or, with --random-name, make two copies. Names that compare alike
// are only a hint: the file system decides whether they are one file, because a file
// wrongly taken for a duplicate is left uncleaned with nothing said about it.
func (p *planner) add(path, rel string, walked bool) {
	key := pathKey(path)
	for _, earlier := range p.seen[key] {
		if sameFile(earlier, path) {
			return
		}
	}
	p.seen[key] = append(p.seen[key], path)
	p.tasks = append(p.tasks, task{path: path, rel: rel, walked: walked})
}

func sameFile(a, b string) bool {
	first, err := os.Stat(a)
	if err != nil {
		return false
	}
	second, err := os.Stat(b)
	return err == nil && os.SameFile(first, second)
}

func (p *planner) addRoot(root string) {
	if pathCannotReach(root) {
		p.invalid(root, "%s", unreachableReason)
		return
	}
	info, err := os.Lstat(root)
	if err != nil {
		if isPattern(root) {
			err = fmt.Errorf("%w: no file matches this pattern", fs.ErrNotExist)
		}
		p.fail(root, err)
		return
	}
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		p.addLink(root)
	case mode.IsDir():
		if !p.cfg.Recursive {
			p.invalid(root, "it is a directory; use -r to process what is inside it")
			return
		}
		p.walk(root)
	case mode.IsRegular():
		p.add(root, filepath.Base(root), false)
	default:
		p.invalid(root, "it is not a regular file")
	}
}

// addLink handles a symlink named on the command line. Reading through it is what
// the user asked for, but replacing the link would leave the real file's metadata
// where it was while reporting success.
func (p *planner) addLink(link string) {
	target, err := os.Stat(link)
	switch {
	case err != nil:
		p.fail(link, err)
	case target.IsDir():
		p.invalid(link, "it is a symbolic link to a directory, and HideMe does not follow links when walking; name the directory itself")
	case !target.Mode().IsRegular():
		p.invalid(link, "it links to something that is not a regular file")
	case p.op == OpClean && p.cfg.InPlace:
		p.invalid(link, "--in-place would replace the link and leave the file it points at uncleaned; name that file itself")
	default:
		p.add(link, filepath.Base(link), false)
	}
}

func (p *planner) walk(root string) {
	prefix := mirrorPrefix(root)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := p.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			p.fail(path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && unreachableName(d.Name()) {
				p.invalid(path, "%s; what is inside it was not processed", unreachableReason)
				return fs.SkipDir
			}
			return p.visitDir(path, d)
		}
		p.visitEntry(root, prefix, path, d)
		return nil
	})
	if err != nil {
		p.fail(root, err)
	}
}

func (p *planner) visitDir(path string, d fs.DirEntry) error {
	if p.outputDir == nil {
		return nil
	}
	info, err := d.Info()
	if err != nil || !os.SameFile(info, p.outputDir) {
		return nil
	}
	if !isEmptyDir(path) {
		p.settle(skipped(path, KindOutputDir, "this is the output directory, so what is inside it was not cleaned"))
	}
	return fs.SkipDir
}

func isEmptyDir(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return errors.Is(err, io.EOF)
}

func (p *planner) visitEntry(root, prefix, path string, d fs.DirEntry) {
	name := d.Name()
	switch {
	case unreachableName(name):
		p.invalid(path, "%s", unreachableReason)
	case d.Type()&fs.ModeSymlink != 0:
		p.settle(skipped(path, KindSymlink, "symbolic links are not followed"))
	case !d.Type().IsRegular():
		p.settle(skipped(path, KindSpecial, "this is not a regular file"))
	case isTempName(name):
		p.settle(skipped(path, KindTempFile, "this is a temporary file left behind by an interrupted HideMe run"))
	case p.op == OpClean && p.cfg.carriesSuffix(name):
		p.settle(skipped(path, KindHasSuffix, fmt.Sprintf("this name already carries the output suffix %s, so it looks like an earlier output", p.cfg.Suffix)))
	default:
		rel, err := filepath.Rel(root, path)
		if err != nil {
			p.fail(path, err)
			return
		}
		p.add(path, filepath.Join(prefix, rel), true)
	}
}

// mirrorPrefix is the directory an input tree gets beneath OutputDir: like "cp -r
// photos out", the tree lands in out/photos, so two trees given together cannot
// collide. A root with no name of its own, such as "." or "/", has no prefix.
func mirrorPrefix(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if !filepath.IsLocal(base) || base == "." {
		return ""
	}
	return base
}

func (p *planner) assignOutputs() {
	inputs := make(map[string]bool, len(p.tasks))
	for _, t := range p.tasks {
		if t.preset == nil {
			inputs[pathKey(t.path)] = true
		}
	}
	claimed := make(map[string]bool, len(p.tasks))
	for i := range p.tasks {
		t := &p.tasks[i]
		if t.preset != nil {
			continue
		}
		output, err := p.cfg.outputFor(t.path, t.rel)
		if err != nil {
			r := failure(t.path, err)
			t.preset = &r
			continue
		}
		t.output = output
		if p.cfg.InPlace {
			continue
		}
		key := pathKey(output)
		switch {
		case key == pathKey(t.path):
			err = errSameFile
		case inputs[key]:
			err = fmt.Errorf("%w: the output is another input", errCollision)
		case claimed[key]:
			err = errCollision
		}
		if err != nil {
			r := failure(t.path, err)
			t.preset = &r
			continue
		}
		claimed[key] = true
	}
}

// outputFor names the output for src. rel is the path beneath OutputDir and comes
// from the walk or from the file's own base name, so it cannot hold "..". The
// IsLocal check holds even so, and also refuses what only Windows treats as
// special: a reserved device name, or the colon that addresses an alternate
// stream, which would write the output into another file's hidden stream.
func (c Config) outputFor(src, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: its name is not usable as an output name", errInvalidInput)
	}
	if c.InPlace {
		return onDiskName(src), nil
	}
	dir, name := filepath.Split(rel)
	stem, ext := splitName(name)
	if c.RandomName {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("random name: %w", err)
		}
		stem = hex.EncodeToString(random)
		// Whatever follows the last dot counts as the extension, and in "John.Smith" or
		// "Scan 2024.03.15" that is the very thing this flag is there to hide.
		if !isKnownExtension(ext) {
			ext = ""
		}
	}
	name = stem + c.Suffix + ext
	if c.OutputDir == "" {
		return filepath.Join(filepath.Dir(src), name), nil
	}
	return filepath.Join(c.OutputDir, dir, name), nil
}

func isKnownExtension(ext string) bool {
	for _, h := range core.Handlers() {
		if slices.ContainsFunc(h.Format().Extensions, func(known string) bool { return strings.EqualFold(known, ext) }) {
			return true
		}
	}
	return false
}

// splitName separates a name from its extension. A dotfile's dot does not start an
// extension, so ".bashrc" becomes ".bashrc.clean" rather than ".clean.bashrc".
func splitName(name string) (stem, ext string) {
	ext = filepath.Ext(name)
	stem = strings.TrimSuffix(name, ext)
	if stem == "" {
		return name, ""
	}
	return stem, ext
}

func (c Config) carriesSuffix(name string) bool {
	if c.Suffix == "" {
		return false
	}
	suffix := strings.ToLower(c.Suffix)
	stem, _ := splitName(name)
	// The whole name counts too: a --random-name output with no extension ends in it.
	return strings.HasSuffix(strings.ToLower(stem), suffix) || strings.HasSuffix(strings.ToLower(name), suffix)
}

// pathKey makes paths comparable before any of them exists. It is only for spotting
// clashes within a run; os.SameFile settles the question for files on disk. On
// platforms whose file systems usually ignore case, two names differing only in
// case are treated as one, because being wrong there loses a file. Upper case is what
// NTFS compares: lowering would merge "\u0130" with "i" and the Kelvin sign with "k",
// which NTFS holds as different names.
func pathKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToUpper(abs)
	}
	return abs
}

// checkOutput refuses an output that must not be written. It runs early so that a
// doomed file does not cost a full clean; publish enforces the same rule atomically
// at the last moment.
func checkOutput(output string, source os.FileInfo, force bool) error {
	if target, err := os.Stat(output); err == nil {
		if os.SameFile(source, target) {
			return errSameFile
		}
		if target.IsDir() {
			return fmt.Errorf("%w: the output path is a directory", errInvalidInput)
		}
	}
	info, err := os.Lstat(output)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !force {
		return errOutputExists
	}
	if info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: the output path exists and is not a regular file", errInvalidInput)
	}
	return nil
}
