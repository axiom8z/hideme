package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/engine"
)

func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const command = "inspect"
	asJSON := wantsJSON(args)
	fs := newFlagSet(command)
	var cfg engine.Config
	var useJSON, redact bool
	fs.BoolVar(&useJSON, "json", false, "")
	fs.BoolVar(&redact, "redact", false, "")
	fs.BoolVar(&cfg.Strict, "strict", false, "")
	fs.BoolVar(&cfg.Recursive, "r", false, "")
	fs.IntVar(&cfg.Jobs, "jobs", 0, "")

	paths, err := parseFlags(fs, args)
	if err == nil {
		err = checkPaths(paths, &cfg)
	}
	if done, code := finishParse(err, command, asJSON, usageInspect, stdout, stderr); done {
		return code
	}
	show := display{showValues: !redact, detailHint: "leave out --redact for why, which may quote the file"}
	return execute(ctx, engine.OpInspect, paths, cfg, newReporter(useJSON, command, false, show, stdout, stderr), stdout, stderr)
}

func runClean(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const command = "clean"
	asJSON := wantsJSON(args)
	fs := newFlagSet(command)
	var cfg engine.Config
	var useJSON, verbose bool
	var suffix, timestamp string
	fs.BoolVar(&useJSON, "json", false, "")
	fs.BoolVar(&cfg.Strict, "strict", false, "")
	fs.BoolVar(&cfg.Recursive, "r", false, "")
	fs.IntVar(&cfg.Jobs, "jobs", 0, "")
	fs.StringVar(&cfg.OutputDir, "o", "", "")
	fs.BoolVar(&cfg.InPlace, "in-place", false, "")
	fs.StringVar(&suffix, "suffix", "", "")
	fs.BoolVar(&cfg.Force, "force", false, "")
	fs.BoolVar(&cfg.RandomName, "random-name", false, "")
	fs.StringVar(&timestamp, "timestamp", "now", "")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "")
	fs.BoolVar(&verbose, "v", false, "")

	paths, err := parseFlags(fs, args)
	if err == nil {
		err = checkPaths(paths, &cfg)
	}
	if err == nil {
		cfg.Timestamp, err = engine.ParseTimestamp(timestamp)
	}
	if err == nil {
		err = checkOutputDirFlag(fs, cfg)
	}
	if done, code := finishParse(err, command, asJSON, usageClean, stdout, stderr); done {
		return code
	}
	cfg.Suffix = chooseSuffix(fs, suffix, cfg)
	show := display{showValues: verbose, namesOnly: !verbose, detailHint: "-v says why, and may quote the file"}
	return execute(ctx, engine.OpClean, paths, cfg, newReporter(useJSON, command, cfg.DryRun, show, stdout, stderr), stdout, stderr)
}

// chooseSuffix applies the default: an output next to its source needs a different
// name, and one in another directory does not. An explicit --suffix always wins.
func chooseSuffix(fs *flag.FlagSet, suffix string, cfg engine.Config) string {
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "suffix" })
	switch {
	case explicit:
		return suffix
	case cfg.InPlace || cfg.OutputDir != "":
		return ""
	}
	return engine.DefaultSuffix
}

// checkOutputDirFlag refuses an empty -o, which is nearly always an unset variable in a
// script. Taken as no -o at all, it wrote beside the source or, with --in-place,
// replaced the original, when the user meant the output to go somewhere else.
func checkOutputDirFlag(fs *flag.FlagSet, cfg engine.Config) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "o" && cfg.OutputDir == "" {
			err = errors.New("-o was given an empty directory name")
		}
	})
	return err
}

func checkPaths(paths []string, cfg *engine.Config) error {
	if cfg.Jobs < 0 {
		return fmt.Errorf("--jobs must be at least 1, got %d", cfg.Jobs)
	}
	if len(paths) == 0 {
		return errors.New("no paths given")
	}
	if slices.Contains(paths, "-") {
		return errors.New(`reading from standard input is not supported; "-" is not a path`)
	}
	return nil
}

// execute runs the engine and reports as it goes. A configuration the engine rejects
// reaches here because it depends on more than one flag or on the disk.
func execute(ctx context.Context, op engine.Op, paths []string, cfg engine.Config, rep reporter, stdout, stderr io.Writer) int {
	if err := cfg.Validate(op); err != nil {
		_, asJSON := rep.(*jsonReporter)
		return usageError(opName(op), asJSON, err, stdout, stderr)
	}
	rep.begin()
	summary, err := engine.Run(ctx, op, paths, cfg, rep.result)
	if err != nil {
		// Validate has already accepted this configuration, so this cannot be a usage
		// error, but it must still not end as a success.
		fmt.Fprintf(stderr, "hideme: %v\n", err)
		summary.Failed++
	}
	code := exitCode(summary)
	rep.finish(summary, code)
	return code
}

func opName(op engine.Op) string {
	if op == engine.OpClean {
		return "clean"
	}
	return "inspect"
}

func runFormats(args []string, stdout, stderr io.Writer) int {
	const command = "formats"
	asJSON := wantsJSON(args)
	fs := newFlagSet(command)
	var useJSON bool
	fs.BoolVar(&useJSON, "json", false, "")
	paths, err := parseFlags(fs, args)
	if err == nil && len(paths) > 0 {
		err = errors.New("formats takes no paths")
	}
	if done, code := finishParse(err, command, asJSON, usageFormats, stdout, stderr); done {
		return code
	}
	formats := sortedFormats()
	if useJSON {
		writeJSONFormats(stdout, formats)
		return exitOK
	}
	writeTextFormats(stdout, formats)
	return exitOK
}

// sortedFormats returns the registered formats in a stable order. Registration order
// comes from import order, which is not something the output should depend on.
func sortedFormats() []core.Format {
	var formats []core.Format
	for _, h := range core.Handlers() {
		formats = append(formats, h.Format())
	}
	slices.SortFunc(formats, func(a, b core.Format) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return formats
}
