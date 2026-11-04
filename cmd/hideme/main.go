// Command hideme removes privacy-sensitive metadata from files. This package only
// parses arguments and prints results; internal/engine does the work.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/engine"
	_ "github.com/axiom8z/hideme/internal/formats/all"
)

// Exit codes, part of the documented interface. Scripts rely on exitOK meaning every
// file was cleaned and verified, so nothing short of that may return it.
const (
	exitOK         = 0 // every file was processed and, for clean, verified
	exitFailed     = 1 // at least one file failed; the original is untouched
	exitUsage      = 2 // the command line was wrong; nothing was done
	exitIncomplete = 3 // finished, but some files were unsupported or skipped
)

// version can be set at build time: go build -ldflags "-X main.version=1.2.3".
var version string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first Ctrl-C cancels and lets temp files be removed. After that the handler
	// steps aside, so a second one kills a run the user no longer wants to wait for.
	context.AfterFunc(ctx, stop)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process-wide effects, so that tests can drive the real
// logic.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageMain)
		return exitUsage
	}
	command, rest := args[0], args[1:]
	switch command {
	case "-h", "-help", "--help", "help":
		return runHelp(rest, stdout, stderr)
	case "inspect":
		return runInspect(ctx, rest, stdout, stderr)
	case "clean":
		return runClean(ctx, rest, stdout, stderr)
	case "formats":
		return runFormats(rest, stdout, stderr)
	case "version":
		return runVersion(rest, stdout, stderr)
	}
	return usageError(command, wantsJSON(args), fmt.Errorf("unknown command %q", command), stdout, stderr)
}

func runHelp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usageMain)
		return exitOK
	}
	if len(args) == 1 {
		if text, ok := helpTopics[args[0]]; ok {
			fmt.Fprint(stdout, text)
			return exitOK
		}
	}
	fmt.Fprintf(stderr, "hideme: no help for %q\n\n%s", args, usageMain)
	return exitUsage
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version")
	paths, err := parseFlags(fs, args)
	if err == nil && len(paths) > 0 {
		err = errors.New("version takes no arguments")
	}
	if done, code := finishParse(err, "version", false, usageVersion, stdout, stderr); done {
		return code
	}
	fmt.Fprintln(stdout, versionString())
	return exitOK
}

func versionString() string {
	v := version
	if v == "" {
		v = "unknown"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			v = info.Main.Version
		}
	}
	return fmt.Sprintf("hideme %s %s %s/%s", v, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	// Errors and help are printed by finishParse, on the right stream for each.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// finishParse handles what comes out of parsing. It reports done when the command
// should stop now: a request for help is success on stdout, and anything else is a
// usage error.
func finishParse(err error, command string, asJSON bool, usage string, stdout, stderr io.Writer) (done bool, code int) {
	switch {
	case err == nil:
		return false, exitOK
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return true, exitOK
	}
	return true, usageError(command, asJSON, err, stdout, stderr)
}

// usageError prints to stderr and, when JSON was asked for, also emits a JSON
// document, so a script always has one document to read on stdout. The message for
// a person is one line and a pointer, because a full usage text would scroll the
// actual mistake off the screen.
func usageError(command string, asJSON bool, err error, stdout, stderr io.Writer) int {
	topic := "hideme help"
	if _, ok := helpTopics[command]; ok {
		topic += " " + command
	}
	// The flag package quotes a bad argument back verbatim, and a script may be passing
	// a hostile file name that merely starts with a dash.
	fmt.Fprintf(stderr, "hideme: %s\nRun %q for usage.\n", core.SafeText(err.Error()), topic)
	if asJSON {
		writeJSONError(stdout, command, err)
	}
	return exitUsage
}

// wantsJSON looks for --json before flags are parsed, because parsing is what may
// fail, and that failure must honour the request as well. It takes every spelling the
// flag package does, and as there the last one wins. A value that is no boolean is
// still someone asking for JSON.
func wantsJSON(args []string) bool {
	want := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "--json" && name != "-json" {
			continue
		}
		want = true
		if hasValue {
			if b, err := strconv.ParseBool(value); err == nil {
				want = b
			}
		}
	}
	return want
}

func exitCode(s engine.Summary) int {
	switch {
	case s.Failed > 0:
		return exitFailed
	case s.Unsupported > 0 || s.Skipped > 0:
		return exitIncomplete
	}
	return exitOK
}
