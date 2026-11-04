//go:build !windows

package engine

import (
	"os"
	"time"
)

// setCreationTime has nothing to do here: these platforms offer no standard way to
// set a birth time, and none tunnels it from a previous file of the same name.
func setCreationTime(string, time.Time) error { return nil }

func rename(from, to string) error { return os.Rename(from, to) }

// unreachableReason and the two checks below are for names only Windows mangles.
const unreachableReason = ""

func unreachableName(string) bool { return false }

func pathCannotReach(string) bool { return false }

// onDiskName has nothing to resolve: a name here is spelled one way.
func onDiskName(path string) string { return path }

// copyPermissions has nothing to do beyond the mode bits, which the caller has copied.
func copyPermissions(string, string, bool) error { return nil }
