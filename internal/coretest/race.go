package coretest

import "testing"

// SkipIfRace skips a test that holds code to a limit on time or memory. The race
// detector slows code down several times and changes what it allocates, so such a limit
// means nothing under it. The ordinary test run still enforces these limits.
func SkipIfRace(t testing.TB) {
	t.Helper()
	if raceEnabled {
		t.Skip("this test measures time or memory, which the race detector distorts")
	}
}
