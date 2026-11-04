package coretest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// recorder implements reporter so these tests can assert that a check fails.
type recorder struct{ failures []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}
func (r *recorder) Failed() bool { return len(r.failures) > 0 }

// lineFormat is a tiny format for exercising the checks: a magic line followed
// by lines of "secret:", "rot:" or "body:" data. It is correct unless one of the
// hooks below introduces the bug a test wants the harness to catch.
const lineMagic = "FAKE\n"

type lineHandler struct {
	// inspectFinding and cleanFinding add a finding to one side only.
	inspectFinding, cleanFinding *core.Finding
	// mutateOutput rewrites Clean's output.
	mutateOutput func(input, out []byte) []byte
	// cleanErr and inspectErr replace the result with an error.
	cleanErr, inspectErr func(input []byte) error
	emptyFormat          bool
	nilReport            bool
	unsafeValue          string
	retainWithoutReason  bool
	panicOn              string
	// strictErr fails Clean in strict mode; strictExtra is appended to its output.
	strictErr   error
	strictExtra string
	// cleanMayLoseContent sets the flag on Clean's report only.
	cleanMayLoseContent bool
}

func (h lineHandler) Format() core.Format {
	return core.Format{ID: "fake", Name: "Fake", Family: core.FamilyDocument, Extensions: []string{".fake"}}
}

func (h lineHandler) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte(lineMagic))
}

func (h lineHandler) analyse(src core.Source, opts core.Options) (*core.Report, []byte, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, nil, err
	}
	if h.panicOn != "" && bytes.Contains(data, []byte(h.panicOn)) {
		panic("hook: panic on input")
	}
	if !bytes.HasPrefix(data, []byte(lineMagic)) {
		return nil, nil, core.Unsupportedf("fake: missing magic")
	}
	rep := &core.Report{Format: "fake"}
	if h.emptyFormat {
		rep.Format = ""
	}
	out := []byte(lineMagic)
	for line := range strings.SplitSeq(string(data[len(lineMagic):]), "\n") {
		key, value, _ := strings.Cut(line, ":")
		switch key {
		case "secret":
			rep.Add(core.Finding{Category: core.CatIdentity, Risk: core.RiskHigh, Field: "Secret", Value: value, Action: core.ActionRemove})
		case "rot":
			if opts.Strict {
				rep.Add(core.Finding{Category: core.CatTechnical, Risk: core.RiskNone, Field: "Rotation", Action: core.ActionRemove})
				continue
			}
			f := core.Finding{Category: core.CatTechnical, Risk: core.RiskNone, Field: "Rotation", Action: core.ActionRetain, Reason: "visible"}
			if h.retainWithoutReason {
				f.Reason = ""
			}
			rep.Add(f)
			out = append(out, line+"\n"...)
		case "body":
			out = append(out, line+"\n"...)
		case "":
		default:
			return nil, nil, core.Malformedf("fake: unknown key %q", key)
		}
	}
	if h.unsafeValue != "" {
		rep.Findings = append(rep.Findings, core.Finding{Category: core.CatHidden, Risk: core.RiskLow, Field: "Unsafe", Value: h.unsafeValue, Action: core.ActionRetain, Reason: "x"})
	}
	return rep, out, nil
}

func (h lineHandler) Inspect(ctx context.Context, src core.Source, opts core.Options) (*core.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.nilReport {
		return nil, nil
	}
	rep, _, err := h.analyse(src, opts)
	if err != nil {
		return nil, err
	}
	if h.inspectErr != nil {
		data, _ := core.ReadAll(src)
		if err := h.inspectErr(data); err != nil {
			return nil, err
		}
	}
	if h.inspectFinding != nil {
		rep.Add(*h.inspectFinding)
	}
	return rep, nil
}

func (h lineHandler) Clean(ctx context.Context, src core.Source, dst io.Writer, opts core.Options) (*core.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rep, out, err := h.analyse(src, opts)
	if err != nil {
		return nil, err
	}
	data, _ := core.ReadAll(src)
	if h.cleanErr != nil {
		if err := h.cleanErr(data); err != nil {
			return nil, err
		}
	}
	if h.cleanFinding != nil {
		rep.Add(*h.cleanFinding)
	}
	rep.MayLoseContent = h.cleanMayLoseContent
	if h.mutateOutput != nil {
		out = h.mutateOutput(data, out)
	}
	if opts.Strict {
		if h.strictErr != nil {
			return nil, h.strictErr
		}
		if !bytes.HasSuffix(out, []byte(h.strictExtra)) {
			out = append(out, h.strictExtra...) // once only, so this stays idempotent
		}
	}
	if _, err := dst.Write(out); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	return rep, nil
}

var lineSample = []byte(lineMagic + "secret:Test Author\nrot:6\nbody:real content\n")

func lineCase() Case {
	return Case{
		Name:           "sample",
		Data:           lineSample,
		WantFields:     []string{"Secret", "Rotation"},
		WantRetained:   []string{"Rotation"},
		MustNotContain: [][]byte{[]byte("Test Author")},
		MustContain:    [][]byte{[]byte("real content")},
	}
}

func TestCorrectHandlerPasses(t *testing.T) {
	h := lineHandler{}
	Contract(t, h, []Case{lineCase()})
	Robustness(t, h, lineSample)
	FuzzTarget(t, h, lineSample)
	FuzzTarget(t, h, nil)
	FuzzTarget(t, h, []byte("not this format"))
}

func TestRobustnessHandlesTinyInputs(t *testing.T) {
	Robustness(t, lineHandler{}, nil)
	Robustness(t, lineHandler{}, []byte("F"))
}

// counter gives each call a different value, to simulate a timestamp or nonce.
var counter atomic.Int64

func TestBrokenHandlerIsCaught(t *testing.T) {
	residual := []byte(lineMagic + "secret:left behind\nbody:real content\n")
	extra := core.Finding{Category: core.CatHidden, Risk: core.RiskLow, Field: "Extra", Action: core.ActionRemove}
	tests := []struct {
		name string
		h    lineHandler
		c    func(*Case)
		want string
	}{
		{
			name: "leaves metadata that Inspect sees on the output",
			h:    lineHandler{mutateOutput: func(_, _ []byte) []byte { return residual }},
			want: "residual finding",
		},
		{
			name: "leaves bytes a test forbids",
			h: lineHandler{mutateOutput: func(_, out []byte) []byte {
				return append(out, "body:Test Author\n"...)
			}},
			want: "still contains",
		},
		{
			name: "loses content",
			h: lineHandler{mutateOutput: func(_, _ []byte) []byte {
				return []byte(lineMagic + "rot:6\n")
			}},
			want: "lost content",
		},
		{
			name: "output no longer matches",
			h:    lineHandler{mutateOutput: func(_, _ []byte) []byte { return []byte("garbage") }},
			want: "no longer matches",
		},
		{
			name: "not idempotent",
			h: lineHandler{mutateOutput: func(input, out []byte) []byte {
				return append(out, fmt.Sprintf("body:%d\n", len(input))...)
			}},
			want: "not idempotent",
		},
		{
			name: "not deterministic",
			h: lineHandler{mutateOutput: func(_, out []byte) []byte {
				return append(out, fmt.Sprintf("body:%d\n", counter.Add(1))...)
			}},
			want: "not deterministic",
		},
		{
			name: "Inspect promises a finding Clean does not report",
			h:    lineHandler{inspectFinding: &extra},
			want: "disagree",
		},
		{
			name: "Clean reports a finding Inspect did not promise",
			h:    lineHandler{cleanFinding: &extra},
			want: "disagree",
		},
		{
			name: "Clean says content may be lost and Inspect does not",
			h:    lineHandler{cleanMayLoseContent: true},
			want: "MayLoseContent",
		},
		{
			name: "empty format",
			h:    lineHandler{emptyFormat: true},
			want: "empty Format",
		},
		{
			name: "nil report from Inspect",
			h:    lineHandler{nilReport: true},
			want: "nil report",
		},
		{
			name: "unsanitised value",
			h:    lineHandler{unsafeValue: "\x1b[31mred"},
			want: "unsanitised",
		},
		{
			name: "retained without a reason",
			h:    lineHandler{retainWithoutReason: true},
			want: "without giving a Reason",
		},
		{
			name: "input the handler does not match",
			h:    lineHandler{},
			c:    func(c *Case) { c.Data = []byte("nope") },
			want: "Match is false",
		},
		{
			name: "missing wanted field",
			h:    lineHandler{},
			c:    func(c *Case) { c.WantFields = []string{"Not There"} },
			want: `did not report field "Not There"`,
		},
		{
			name: "field not retained",
			h:    lineHandler{},
			c:    func(c *Case) { c.WantRetained = []string{"Secret"} },
			want: `did not report field "Secret" as retained`,
		},
		{
			name: "Clean fails on valid input",
			h:    lineHandler{cleanErr: func([]byte) error { return core.Malformedf("nope") }},
			want: "Clean:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := lineCase()
			if tt.c != nil {
				tt.c(&c)
			}
			var r recorder
			checkCase(&r, tt.h, c)
			assertFailure(t, r, tt.want)
		})
	}
}

func TestStrictChecksCatchBrokenHandlers(t *testing.T) {
	tests := []struct {
		name string
		h    lineHandler
		want string
	}{
		{"valid file fails only in strict mode", lineHandler{strictErr: core.Malformedf("nope")}, "Clean in strict mode"},
		{"strict mode leaves forbidden bytes", lineHandler{strictExtra: "body:Test Author\n"}, "still contains"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorder
			checkStrict(&r, tt.h, lineCase())
			assertFailure(t, r, tt.want)
		})
	}
	t.Run("a correct handler passes, and may drop retained data", func(t *testing.T) {
		var r recorder
		c := lineCase()
		c.MustContain = [][]byte{[]byte("rot:6")}
		checkStrict(&r, lineHandler{}, c)
		if r.Failed() {
			t.Errorf("unexpected failures: %q", r.failures)
		}
	})
}

func TestInputChecksCatchBrokenHandlers(t *testing.T) {
	bareError := errors.New("plain error")
	tests := []struct {
		name string
		h    lineHandler
		want string
	}{
		{"panic is reported, not fatal", lineHandler{panicOn: "body"}, "panic on a"},
		{"error wrapping no sentinel from Clean", lineHandler{cleanErr: func([]byte) error { return bareError }}, "Clean returned an error wrapping neither"},
		{"error wrapping no sentinel from Inspect", lineHandler{inspectErr: func([]byte) error { return bareError }}, "Inspect returned an error wrapping neither"},
		{"Clean succeeds where Inspect fails", lineHandler{inspectErr: func([]byte) error { return core.Malformedf("nope") }}, "Clean succeeded but Inspect failed"},
		{
			"residual data on the output",
			lineHandler{mutateOutput: func(_, _ []byte) []byte { return []byte(lineMagic + "secret:x\n") }},
			"residual finding",
		},
		{
			"not idempotent on hostile input",
			lineHandler{mutateOutput: func(input, out []byte) []byte { return append(out, fmt.Sprintf("body:%d\n", len(input))...) }},
			"not idempotent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorder
			checkInput(&r, tt.h, lineSample)
			assertFailure(t, r, tt.want)
		})
	}
}

func TestContextErrorsAreAccepted(t *testing.T) {
	for _, err := range []error{
		context.Canceled, context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", context.Canceled),
		core.ErrUnsupported, core.ErrEncrypted, core.ErrTooLarge,
		core.Malformedf("x"), core.Unsupportedf("x"),
	} {
		var r recorder
		checkError(&r, "test", err)
		if r.Failed() {
			t.Errorf("error %v was rejected: %q", err, r.failures)
		}
	}
}

func TestRobustnessReportsTheFailingInput(t *testing.T) {
	// Truncated to fewer than 10 bytes, the handler fails to classify its error.
	h := lineHandler{cleanErr: func(input []byte) error {
		if len(input) < 10 {
			return errors.New("plain error")
		}
		return nil
	}}
	var r recorder
	checkRobustness(&r, h, lineSample)
	assertFailure(t, r, "failing input: the file truncated to")
}

func TestRobustnessFindsCorruptionBugs(t *testing.T) {
	// Only a corrupted byte can produce 0xFF here, so this reaches the flip loop.
	h := lineHandler{panicOn: "\xFF"}
	var r recorder
	checkRobustness(&r, h, lineSample)
	assertFailure(t, r, "panic on a")
	assertFailure(t, r, "failing input: byte ")
}

func TestRobustnessIsReproducible(t *testing.T) {
	run := func() []string {
		h := lineHandler{panicOn: "\xFF"}
		var r recorder
		checkRobustness(&r, h, lineSample)
		// Drop the stack trace, which differs between call sites.
		return r.failures[len(r.failures)-1:]
	}
	if a, b := run(), run(); a[0] != b[0] {
		t.Errorf("two runs failed differently:\n%s\n%s", a[0], b[0])
	}
}

func TestTruncationLengths(t *testing.T) {
	for _, n := range []int{0, 1, 2, 64, 128, 129, 1000, 1 << 20} {
		lengths := truncationLengths(n)
		for i, l := range lengths {
			if l < 0 || (n > 0 && l >= n) || (n == 0 && l != 0) {
				t.Errorf("n=%d: length %d is out of range", n, l)
			}
			if i > 0 && l <= lengths[i-1] {
				t.Errorf("n=%d: lengths are not strictly increasing at %d", n, i)
			}
		}
		if n > 0 && lengths[len(lengths)-1] != n-1 {
			t.Errorf("n=%d: never tries removing just the last byte", n)
		}
		if limit := headerBytes + tailBytes + spreadCuts + 1; len(lengths) > limit {
			t.Errorf("n=%d: %d lengths, over the limit of %d", n, len(lengths), limit)
		}
	}
}

func TestTIFFEncoderLayout(t *testing.T) {
	root := &IFD{
		Entries: []Entry{
			{Tag: 0x0112, Value: uint16(6)},
			{Tag: 0x010F, Value: "A much longer value than fits inline"},
		},
		GPS:       &IFD{Entries: []Entry{{Tag: 1, Value: "N"}}},
		Thumbnail: []byte("thumbnail"),
		Next:      &IFD{Entries: []Entry{{Tag: 0x0103, Value: uint16(6)}}},
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		block := root.Encode(order)
		if got := order.Uint16(block[2:]); got != 42 {
			t.Errorf("%v: magic = %d", order, got)
		}
		if got := order.Uint32(block[4:]); got != 8 {
			t.Errorf("%v: IFD0 offset = %d", order, got)
		}
		count := int(order.Uint16(block[8:]))
		if count != 5 { // two entries, the GPS pointer, two thumbnail tags
			t.Fatalf("%v: IFD0 has %d entries", order, count)
		}
		previous := uint16(0)
		for i := range count {
			tag := order.Uint16(block[10+12*i:])
			if tag <= previous {
				t.Errorf("%v: tag %#x is out of order", order, tag)
			}
			previous = tag
		}
		if !bytes.Contains(block, []byte("A much longer value")) || !bytes.Contains(block, []byte("thumbnail")) {
			t.Errorf("%v: out-of-line values are missing", order)
		}
		if next := order.Uint32(block[10+12*count:]); next == 0 || int(next) >= len(block) {
			t.Errorf("%v: next IFD offset = %d", order, next)
		}
	}
}

func TestFixturesAreDeterministicAndFake(t *testing.T) {
	if !bytes.Equal(FakeEXIF(6), FakeEXIF(6)) {
		t.Error("FakeEXIF is not deterministic")
	}
	if bytes.Equal(FakeEXIF(6), FakeEXIF(0)) {
		t.Error("FakeEXIF ignores its orientation")
	}
	for _, secret := range FakeEXIFSecrets() {
		if !bytes.Contains(FakeEXIF(0), secret) {
			t.Errorf("FakeEXIF does not contain its own secret %q", secret)
		}
	}
	for _, secret := range FakeXMPSecrets() {
		if !bytes.Contains(FakeXMP(), secret) {
			t.Errorf("FakeXMP does not contain its own secret %q", secret)
		}
	}
}

func assertFailure(t *testing.T, r recorder, want string) {
	t.Helper()
	if !r.Failed() {
		t.Fatalf("expected a failure containing %q, but the check passed", want)
	}
	for _, f := range r.failures {
		if strings.Contains(f, want) {
			return
		}
	}
	t.Errorf("no failure contains %q; got %q", want, r.failures)
}
