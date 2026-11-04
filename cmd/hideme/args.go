package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// parseFlags parses args with flags allowed anywhere, and returns the paths. The flag
// package alone stops at the first positional argument, so "hideme clean photo.jpg
// --in-place" would treat --in-place as a file name and clean a copy while the user
// believed the original had been replaced. So flags are pulled out first, each with
// its value if it takes one, and given to the flag package on their own.
//
// A word that starts with "-" and is not a known flag is an error rather than a
// path: nothing good comes of guessing. After "--" everything is a path.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, paths []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		if arg == "-" {
			return nil, errors.New(`reading from standard input is not supported; "-" is not a path`)
		}
		if !strings.HasPrefix(arg, "-") {
			paths = append(paths, arg)
			continue
		}
		flags = append(flags, arg)
		if !takesValue(fs, arg) {
			continue
		}
		if i+1 == len(args) {
			return nil, fmt.Errorf("flag needs an argument: %s", arg)
		}
		i++
		flags = append(flags, args[i])
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return paths, nil
}

// takesValue reports whether a flag written as arg consumes the next argument: it is
// defined, is not boolean, and has no "=value" of its own. An unknown flag says no,
// and fs.Parse then reports it by name.
func takesValue(fs *flag.FlagSet, arg string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if strings.Contains(name, "=") {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	b, isBool := f.Value.(interface{ IsBoolFlag() bool })
	return !isBool || !b.IsBoolFlag()
}
