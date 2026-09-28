//go:build race

package skillgap

// The race runtime reserves a very large virtual address range, so RLIMIT_AS
// cannot be used in race-test subprocesses. Production builds are never race
// instrumented and always take the hard Linux limit path.
const pdfRaceEnabled = true
