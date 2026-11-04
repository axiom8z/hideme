package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// fakeHandler is a format defined entirely by the test: a file is its magic line,
// then lines of content, where a line starting with "SECRET=" is metadata. Each
// instance can be made to misbehave in one way, so that the engine's safeguards can
// be proved against handlers that are wrong in ways real ones may be one day.
type fakeHandler struct {
	id       string
	magic    string
	warnOnly bool
	clean    func(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error)
	inspect  func(ctx context.Context, src core.Source) (*core.Report, error)
}

func (h fakeHandler) Format() core.Format {
	return core.Format{ID: h.id, Name: "Fake " + h.id, Family: core.FamilyDocument, Extensions: []string{"." + h.id}, ResidueWarnOnly: h.warnOnly}
}

func (h fakeHandler) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte(h.magic))
}

func (h fakeHandler) Inspect(ctx context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	if h.inspect != nil {
		return h.inspect(ctx, src)
	}
	data := readSource(src)
	rep := &core.Report{Format: h.id}
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "SECRET="); ok {
			rep.Add(core.Finding{Category: core.CatIdentity, Risk: core.RiskHigh, Field: "Secret", Value: value, Action: core.ActionRemove})
		}
	}
	return rep, nil
}

func (h fakeHandler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	if h.clean != nil {
		return h.clean(ctx, src, dst)
	}
	rep, err := h.Inspect(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	var kept []string
	for line := range strings.SplitSeq(string(readSource(src)), "\n") {
		if !strings.HasPrefix(line, "SECRET=") {
			kept = append(kept, line)
		}
	}
	_, err = io.WriteString(dst, strings.Join(kept, "\n"))
	return rep, err
}

func readSource(src core.Source) []byte {
	data, err := core.ReadAll(src)
	if err != nil {
		panic(err)
	}
	return data
}

// copyThrough cleans by copying every byte, lying that nothing was there.
func copyThrough(id string) func(context.Context, core.Source, io.Writer) (*core.Report, error) {
	return func(_ context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
		_, err := dst.Write(readSource(src))
		return &core.Report{Format: id}, err
	}
}

func silent(id string) func(context.Context, core.Source) (*core.Report, error) {
	return func(context.Context, core.Source) (*core.Report, error) { return &core.Report{Format: id}, nil }
}

// Hooks that one test at a time installs. They are globals because handlers live in
// a registry for the whole test binary; those tests do not run in parallel.
var (
	cleanHook func(ctx context.Context) error
)

func init() {
	for _, h := range []fakeHandler{
		{id: "good", magic: "FAKEGOOD\n"},
		{
			// Blind to its own leftovers: only the residue scan can catch it.
			id: "liar", magic: "FAKELIAR\n",
			clean: copyThrough("liar"), inspect: silent("liar"),
		},
		{
			// Admits on reading the output that the metadata is still there.
			id: "blind", magic: "FAKEBLIND\n",
			clean: copyThrough("blind"),
		},
		{
			// Writes a file of a different format.
			id: "shifty", magic: "FAKESHIFTY\n",
			clean: func(_ context.Context, _ core.Source, dst io.Writer) (*core.Report, error) {
				_, err := io.WriteString(dst, "FAKEGOOD\nclean\n")
				return &core.Report{Format: "shifty"}, err
			},
			inspect: silent("shifty"),
		},
		{
			// Writes something no handler recognises.
			id: "garbler", magic: "FAKEGARBLER\n",
			clean: func(_ context.Context, _ core.Source, dst io.Writer) (*core.Report, error) {
				_, err := io.WriteString(dst, "no magic here")
				return &core.Report{Format: "garbler"}, err
			},
			inspect: silent("garbler"),
		},
		{
			// A format whose visible text may quote a signature.
			id: "quoter", magic: "FAKEQUOTER\n", warnOnly: true,
			clean: copyThrough("quoter"), inspect: silent("quoter"),
		},
		{
			id: "panicker", magic: "FAKEPANICKER\n",
			clean: func(_ context.Context, _ core.Source, dst io.Writer) (*core.Report, error) {
				_, _ = io.WriteString(dst, "half a file")
				panic("handler bug")
			},
			inspect: func(context.Context, core.Source) (*core.Report, error) { panic("handler bug") },
		},
		{
			id: "failer", magic: "FAKEFAILER\n",
			clean: func(_ context.Context, _ core.Source, dst io.Writer) (*core.Report, error) {
				_, _ = io.WriteString(dst, "half a file")
				return nil, core.Malformedf("this file is damaged")
			},
		},
		{
			id: "refuser", magic: "FAKEREFUSER\n",
			clean: func(context.Context, core.Source, io.Writer) (*core.Report, error) {
				return nil, core.Unsupportedf("this variant is not handled")
			},
			inspect: func(context.Context, core.Source) (*core.Report, error) {
				return nil, core.Unsupportedf("this variant is not handled")
			},
		},
		{
			id: "warner", magic: "FAKEWARNER\n",
			clean: func(_ context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
				rep := &core.Report{Format: "warner"}
				rep.Warn("a part of this file could not be cleaned")
				_, err := dst.Write(readSource(src))
				return rep, err
			},
			inspect: silent("warner"),
		},
		{
			id: "lossy", magic: "FAKELOSSY\n",
			clean: func(_ context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
				rep := &core.Report{Format: "lossy", MayLoseContent: true}
				rep.Warn("some bytes were dropped that may have been real content")
				_, err := dst.Write(readSource(src))
				return rep, err
			},
			inspect: silent("lossy"),
		},
		{
			// Hands control to the test partway through writing.
			id: "hooked", magic: "FAKEHOOKED\n",
			clean: func(ctx context.Context, src core.Source, dst io.Writer) (*core.Report, error) {
				if _, err := dst.Write(readSource(src)); err != nil {
					return nil, err
				}
				if cleanHook != nil {
					if err := cleanHook(ctx); err != nil {
						return nil, err
					}
				}
				return &core.Report{Format: "hooked"}, nil
			},
			inspect: silent("hooked"),
		},
	} {
		core.Register(h)
	}
}

// write creates a file with the given contents, making parents as needed.
func write(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// assertNoTemp fails if any temp file was left anywhere beneath dir.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if isTempName(d.Name()) {
			t.Errorf("temp file left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

const (
	goodFile    = "FAKEGOOD\nvisible\nSECRET=Test Author\nmore\n"
	goodCleaned = "FAKEGOOD\nvisible\nmore\n"
	// lossyFile belongs to a handler that cannot be sure its output holds everything.
	lossyFile = "FAKELOSSY\nvisible\n"
)

func defaultConfig() Config { return Config{Suffix: DefaultSuffix} }

func runAll(t *testing.T, op Op, paths []string, cfg Config) ([]Result, Summary) {
	t.Helper()
	var results []Result
	summary, err := Run(context.Background(), op, paths, cfg, func(r Result) { results = append(results, r) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return results, summary
}
