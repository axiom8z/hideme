//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestTempFileIsPrivateUntilVerified(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.bin"), "FAKEHOOKED\n")
	if err := os.Chmod(src, 0o644); err != nil {
		t.Fatal(err)
	}
	var mode os.FileMode
	cleanHook = func(context.Context) error {
		matches, err := filepath.Glob(filepath.Join(dir, tempPrefix+"*"))
		if err != nil || len(matches) != 1 {
			t.Errorf("temp files: %v %v", matches, err)
			return nil
		}
		info, err := os.Stat(matches[0])
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
		return nil
	}
	defer func() { cleanHook = nil }()

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if mode != 0o600 {
		t.Errorf("temp mode %v, want 0600", mode)
	}
	info, err := os.Stat(r.Output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("output mode %v, want the source's 0644", info.Mode().Perm())
	}
}

func TestSpecialFilesAreRefused(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.good")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("named pipes not available: %v", err)
	}

	// Named directly it fails, and during a walk it is skipped. Either way the test
	// finishing at all proves the engine did not try to open it and block.
	r := Clean(context.Background(), fifo, defaultConfig())
	if r.Status != StatusFailed || r.Kind != KindInvalidInput {
		t.Errorf("explicit: %+v", r)
	}

	results, summary := runAll(t, OpClean, []string{dir}, Config{Suffix: DefaultSuffix, Recursive: true})
	if summary.Skipped != 1 || results[0].Kind != KindSpecial {
		t.Errorf("walk: %+v", results)
	}
}
