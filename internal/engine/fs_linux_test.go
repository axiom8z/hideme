package engine

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
)

func setXattr(t *testing.T, path, name, value string) {
	t.Helper()
	if err := syscall.Setxattr(path, name, []byte(value), 0); err != nil {
		t.Skipf("extended attributes not available here: %v", err)
	}
}

func TestExtendedAttributesAreReportedAndNotInherited(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	setXattr(t, src, "user.xdg.origin.url", "https://example.invalid/photo.good")
	setXattr(t, src, "user.test", "Test Value")

	inspected := Inspect(context.Background(), src, Config{})

	found := make(map[string]string)
	for _, f := range inspected.Findings {
		found[f.Field] = f.Value
	}
	if found["Download URL"] != "https://example.invalid/photo.good" {
		t.Errorf("findings: %+v", inspected.Findings)
	}
	if found["Extended attribute"] != "user.test, 10 bytes" {
		t.Errorf("findings: %+v", inspected.Findings)
	}

	cleaned := Clean(context.Background(), src, defaultConfig())
	if cleaned.Status != StatusCleaned {
		t.Fatalf("status %q: %s", cleaned.Status, cleaned.Reason)
	}
	buf := make([]byte, 4096)
	n, err := syscall.Listxattr(cleaned.Output, buf)
	if err != nil {
		t.Fatal(err)
	}
	if names := string(buf[:n]); names != "" {
		t.Errorf("the output inherited attributes: %q", names)
	}
}
