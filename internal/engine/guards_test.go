package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceChangedDuringCleanLeavesNoOutputAndNoTemp(t *testing.T) {
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
	if !strings.Contains(r.Reason, "the file changed while it was being cleaned") {
		t.Errorf("reason %q does not name the guard", r.Reason)
	}
	if got := read(t, src); got != contents+added {
		t.Errorf("the other program's save was lost: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %v (err %v), want only the source", entries, err)
	}
	assertNoTemp(t, dir)
}

func TestHandlerReturningNoReportIsAPerFileFailure(t *testing.T) {
	const want = "the handler returned no report"
	for _, tc := range []struct {
		name string
		op   Op
	}{{"inspect", OpInspect}, {"clean", OpClean}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const contents = "FAKENILREPORT\ncontent\n"
			src := write(t, filepath.Join(dir, "a.bin"), contents)
			good := write(t, filepath.Join(dir, "b.good"), goodFile)

			results, summary := runAll(t, tc.op, []string{src, good}, defaultConfig())

			if len(results) != 2 || summary.Failed != 1 {
				t.Fatalf("results %+v, summary %+v", results, summary)
			}
			r := results[0]
			if r.Status != StatusFailed || r.Kind != KindInternal || !strings.Contains(r.Reason, want) {
				t.Errorf("status %q kind %q reason %q", r.Status, r.Kind, r.Reason)
			}
			if results[1].Status == StatusFailed {
				t.Errorf("the next file was dragged down: %s", results[1].Reason)
			}
			if got := read(t, src); got != contents {
				t.Errorf("the source changed: %q", got)
			}
			if exists(filepath.Join(dir, "a.clean.bin")) {
				t.Error("an output was written")
			}
			assertNoTemp(t, dir)
		})
	}
}

func TestPublishRenameFailureRemovesTheClaimedOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")

	err := publish(filepath.Join(dir, "no-such-temp"), out, false)

	if err == nil || !strings.HasPrefix(err.Error(), "rename: ") {
		t.Fatalf("error %v, want one wrapped with \"rename:\"", err)
	}
	if errors.Is(err, errOutputExists) {
		t.Error("the failure was reported as an existing output")
	}
	if exists(out) {
		t.Error("the claimed output was left behind")
	}
}
