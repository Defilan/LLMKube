package agent

// mutation_check.go holds the pure decision logic for the gate's mutation
// check (#1555): a test added alongside a fix must actually constrain that
// fix. The mechanical test is that reverting the non-test hunks must break at
// least one new or modified test; zero failures means the tests are inert.
//
// This file is deliberately free of any I/O. It does not run git, does not
// execute tests, and does not touch the gate Job template. The revert step and
// test execution live in the gate Job; this package only turns the Job's
// outputs (a diff's file list and the names of tests that failed on revert)
// into a decision. Keeping it a pure function of its inputs is what makes it
// unit-testable without a workspace, and it is the seam a later, larger change
// can wire into the Job.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// jsTestSuffixes are the JS/TS test and spec file suffixes the gate profile
// cares about. They are matched as full suffixes against the base name, never
// with a substring contains check: "contest.go" must not be mistaken for a
// test file just because it contains the letters "test".
var jsTestSuffixes = []string{
	".test.ts",
	".test.tsx",
	".test.js",
	".test.jsx",
	".spec.ts",
	".spec.tsx",
	".spec.js",
	".spec.jsx",
}

// isTestPath reports whether p names a test file, following the Go, JS/TS, and
// Python conventions the gate already knows about:
//
//   - Go:    *_test.go
//   - JS/TS: *.test.{ts,tsx,js,jsx} and *.spec.{ts,tsx,js,jsx}
//   - Python: test_*.py and *_test.py
//
// Matching is done on the base name with strict suffix/prefix checks, so a file
// like "contest.go" (which contains "test" but is not a test file) is
// correctly rejected.
func isTestPath(p string) bool {
	base := filepath.Base(p)

	// Go test files. The full "_test.go" suffix (including the underscore) is
	// what distinguishes a test file from something like "contest.go".
	if strings.HasSuffix(base, "_test.go") {
		return true
	}

	// JS/TS test and spec files.
	for _, suf := range jsTestSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}

	// Python test files: leading "test_" or trailing "_test_", both on a .py.
	if strings.HasSuffix(base, ".py") {
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") {
			return true
		}
	}

	return false
}

// splitDiffPaths partitions a diff's file list into test files and everything
// else. Both returned slices are sorted and are non-nil even when the input is
// nil or empty, so callers can compare lengths without guarding against nil.
func splitDiffPaths(paths []string) (testPaths, nonTestPaths []string) {
	testPaths = []string{}
	nonTestPaths = []string{}
	for _, p := range paths {
		if isTestPath(p) {
			testPaths = append(testPaths, p)
		} else {
			nonTestPaths = append(nonTestPaths, p)
		}
	}
	sort.Strings(testPaths)
	sort.Strings(nonTestPaths)
	return testPaths, nonTestPaths
}

// mutationCheckApplicable reports whether the mutation check is meaningful for
// this diff. It is meaningful only when the branch changed BOTH production
// code and tests: there must be something to revert (non-test changes) and
// something to verify against that revert (test changes). When either side is
// empty the check would produce a false positive, so it is reported as
// not-applicable with a short reason rather than as a finding.
func mutationCheckApplicable(testPaths, nonTestPaths []string) (bool, string) {
	if len(nonTestPaths) == 0 {
		return false, "diff changes no production code; nothing to revert, so the mutation check is not applicable"
	}
	if len(testPaths) == 0 {
		return false, "diff changes no tests; nothing to verify against the revert, so the mutation check is not applicable"
	}
	return true, "diff changes both production code and tests; the mutation check applies"
}

// mutationFindingFromResults turns the names of the tests that failed after the
// non-test hunks were reverted into the check's verdict.
//
//   - At least one failure: the tests are load-bearing (ok=true). The note
//     names WHICH test(s) failed so the result shows what is actually
//     constraining the fix, not merely that something is.
//   - Zero failures: the new tests pass with the fix reverted, which means they
//     do not constrain it. That is a finding (ok=false), stated plainly.
func mutationFindingFromResults(failures []string) (ok bool, note string) {
	if len(failures) == 0 {
		return false, "new tests pass with the fix reverted; they do not constrain the change"
	}
	return true, fmt.Sprintf("test(s) failed with the fix reverted and are load-bearing: %s", strings.Join(failures, ", "))
}

// mutationCheckDisabled reports whether the mutation check has been explicitly
// turned off via the FOREMAN_MUTATION_CHECK environment variable. The default
// (unset or any value other than "0") is ENABLED; only the literal "0"
// disables it, so a stray "1" or empty value never silences the check.
func mutationCheckDisabled() bool {
	return os.Getenv("FOREMAN_MUTATION_CHECK") == "0"
}
