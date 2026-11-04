package all

import (
	"bytes"
	"context"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/formats/all/samples"
)

func matchingHandlers(data []byte) []string {
	src := core.BytesSource(data)
	head := data[:min(len(data), core.HeadSize)]
	var ids []string
	for _, h := range core.Handlers() {
		if h.Match(head, src) {
			ids = append(ids, h.Format().ID)
		}
	}
	return ids
}

func TestExactlyOneHandlerMatchesEachSample(t *testing.T) {
	for _, s := range samples.All() {
		t.Run(s.Name, func(t *testing.T) {
			ids := matchingHandlers(s.Data)
			if len(ids) != 1 || ids[0] != s.Handler {
				t.Errorf("handlers matching: %q, want exactly %q", ids, s.Handler)
			}
		})
	}
}

// TestEveryHandlerHasASample keeps the test above honest as handlers are added.
func TestEveryHandlerHasASample(t *testing.T) {
	covered := make(map[string]bool)
	for _, s := range samples.All() {
		covered[s.Handler] = true
	}
	for _, h := range core.Handlers() {
		if id := h.Format().ID; !covered[id] {
			t.Errorf("handler %q has no sample in internal/formats/all/samples", id)
		}
	}
}

func TestFormatsAreWellFormedAndUnique(t *testing.T) {
	ids := make(map[string]bool)
	extensions := make(map[string]string)
	for _, h := range core.Handlers() {
		f := h.Format()
		if f.ID == "" || f.Name == "" || f.Family == "" {
			t.Errorf("incomplete format: %+v", f)
		}
		if ids[f.ID] {
			t.Errorf("format ID %q is registered twice", f.ID)
		}
		ids[f.ID] = true
		if len(f.Extensions) == 0 {
			t.Errorf("%s has no extensions", f.ID)
		}
		if len(f.Limitations) == 0 {
			t.Errorf("%s states no limitations, and `hideme formats` promises them", f.ID)
		}
		for _, ext := range f.Extensions {
			if other, taken := extensions[ext]; taken {
				t.Errorf("extension %s is claimed by both %s and %s", ext, other, f.ID)
			}
			extensions[ext] = f.ID
			if len(ext) < 2 || ext[0] != '.' {
				t.Errorf("%s: extension %q does not start with a dot", f.ID, ext)
			}
		}
	}
}

// TestSamplesCleanThroughTheRegistry runs each sample as the engine would, and so
// proves the samples are real inputs and not merely files that pass Match.
func TestSamplesCleanThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	for _, s := range samples.All() {
		t.Run(s.Name, func(t *testing.T) {
			src := core.BytesSource(s.Data)
			h, err := core.Detect(src)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			inspected, err := h.Inspect(ctx, src, core.Options{})
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if len(inspected.Residual()) == 0 {
				t.Error("the sample holds no metadata to remove, so it tests nothing")
			}
			var out bytes.Buffer
			if _, err := h.Clean(ctx, src, &out, core.Options{}); err != nil {
				t.Fatalf("Clean: %v", err)
			}
			for _, secret := range s.Secrets {
				if bytes.Contains(out.Bytes(), secret) {
					t.Errorf("the output still contains %q", secret)
				}
			}
			if s.Content != nil && !bytes.Contains(out.Bytes(), s.Content) {
				t.Errorf("the output lost its content %q", s.Content)
			}
			again, err := h.Inspect(ctx, core.BytesSource(out.Bytes()), core.Options{})
			if err != nil {
				t.Fatalf("Inspect on the output: %v", err)
			}
			if residual := again.Residual(); len(residual) > 0 {
				t.Errorf("residual findings: %+v", residual)
			}
		})
	}
}
