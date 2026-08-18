package agent

import (
	"path"
	"strings"
)

// TestLayout describes a repository's test-layout convention: where test
// files live (TestRoot) and where the code they cover lives (SourceRoot).
//
// The zero value (both roots empty) means "beside the code", which is the
// behaviour the built-in testTargetsForPath already provides: it only
// rewrites the basename and keeps the file's own directory.
//
// A non-zero value lets the scope-overlap rail fold a test file back to its
// subject in repositories with a parallel test tree, where the directory
// differs too. For example a Maven/Gradle Java project uses
// TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"} so that
// "src/test/java/a/FooTest.java" maps to "src/main/java/a/Foo.java", and a
// project keeping tests in a parallel top-level tree uses
// TestLayout{TestRoot: "tests", SourceRoot: "src"} so that "tests/test_foo.py"
// maps to "src/foo.py".
type TestLayout struct {
	TestRoot   string
	SourceRoot string
}

// IsZero reports whether the layout is unset: both roots are empty after
// trimming surrounding whitespace.
func (l TestLayout) IsZero() bool {
	return strings.TrimSpace(l.TestRoot) == "" && strings.TrimSpace(l.SourceRoot) == ""
}

// testTargetsWithLayout folds a test path p back to the candidate source
// paths it covers, honouring an optional parallel test-tree convention l.
//
// A zero layout delegates entirely to testTargetsForPath, preserving the
// beside-the-code behaviour exactly. A non-zero layout maps any path under
// TestRoot by rewriting the TestRoot prefix to SourceRoot and stripping the
// test decoration from the basename (FooTest.java -> Foo.java, test_foo.py
// -> foo.py, foo_test.go -> foo.go), returning the candidate subject paths.
// A path that is not under TestRoot falls back to the beside-the-code
// behaviour (testTargetsForPath) rather than returning nothing.
func testTargetsWithLayout(p string, l TestLayout) []string {
	if l.IsZero() {
		// Beside the code: the built-in helper already does exactly this,
		// so delegate rather than reimplement.
		return testTargetsForPath(p)
	}

	root := path.Clean(strings.TrimSpace(l.TestRoot))
	if root != "" {
		// Match on whole path segments so a TestRoot of "tests" does not
		// swallow the directory "testsuite" (the prefix trap).
		if rest, under := underTestRoot(p, root); under {
			sourceRoot := strings.TrimSpace(l.SourceRoot)
			subjectDir := path.Dir(rest)
			base := path.Base(rest)
			// Strip the test decoration from the basename; fall back to the
			// bare name when it is not a recognised test file, so a subject
			// path is still produced rather than nothing.
			if s := stripTestDecoration(base); s != "" {
				base = s
			}
			// Reassemble the subject path in the source tree. path.Join
			// collapses the "." subjectDir for a file directly under the root
			// and cleans any redundant separators.
			return []string{path.Join(sourceRoot, subjectDir, base)}
		}
	}

	// Not under the declared test root: keep the built-in behaviour.
	return testTargetsForPath(p)
}

// underTestRoot reports whether p is under the (already path.Clean'd) test
// root, and returns p with that root prefix removed. Matching is on whole
// path segments: "tests" matches "tests/..." but not "testsuite/...".
func underTestRoot(p, root string) (string, bool) {
	// p == root would be a directory, not a test file; treat it as not under.
	if p == root {
		return p, false
	}
	// Align on a "/" boundary so the root must be a whole leading segment run.
	if strings.HasPrefix(p, root+"/") {
		return p[len(root)+1:], true
	}
	return p, false
}

// stripTestDecoration strips the test decoration from a basename, returning
// the candidate subject basename, or "" when the name is not a recognised
// test file. It covers the beside-the-code conventions (X.test.ts -> X.ts,
// foo_test.go -> foo.go, test_foo.py -> foo.py) plus the Java suffix form
// (FooTest.java -> Foo.java) that testTargetsForPath does not handle because
// it was written for the beside-the-code case only.
//
// The JS/TS and Python prefixes are checked first and take precedence over
// the Java suffix, so "FooTest.java" (no "." in the stem) is the only name
// the suffix rule can claim, and "foo_test.go" keeps its ".go".
func stripTestDecoration(base string) string {
	ext := path.Ext(base)
	if ext == "" {
		return ""
	}
	stem := strings.TrimSuffix(base, ext)

	// JS/TS: X.test.ts / X.spec.ts -> X.ts.
	for _, marker := range []string{".test", ".spec"} {
		if strings.HasSuffix(stem, marker) {
			return strings.TrimSuffix(stem, marker) + ext
		}
	}
	// Python: test_X.py -> X.py.
	if strings.HasPrefix(stem, "test_") {
		return strings.TrimPrefix(stem, "test_") + ext
	}
	// Go: X_test.go -> X.go (and Python X_test.py -> X.py).
	if strings.HasSuffix(stem, "_test") {
		return strings.TrimSuffix(stem, "_test") + ext
	}
	// Java: FooTest.java -> Foo.java. Only reached when the stem carries none
	// of the decorations above, so a dotted stem like "foo.test" can never be
	// mis-stripped of its ".test".
	if len(stem) > len("Test") && strings.HasSuffix(stem, "Test") {
		return strings.TrimSuffix(stem, "Test") + ext
	}
	return ""
}
