package agent

import (
	"reflect"
	"testing"
)

func TestIsTestPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo_test.go", true},
		{"a/b/x.test.ts", true},
		{"x.spec.tsx", true},
		{"test_x.py", true},
		{"x_test.py", true},
		{"foo.go", false},
		{"testdata/x.go", false},
		{"contest.go", false}, // substring trap: contains "test" but is not a test file
		{"README.md", false},
		// additional coverage
		{"x.test.tsx", true},
		{"x.test.js", true},
		{"x.test.jsx", true},
		{"x.spec.ts", true},
		{"x.spec.js", true},
		{"x.spec.jsx", true},
		{"test.py", false},    // no "test_" prefix, no "_test.py" suffix
		{"contest.go", false}, // repeated to be explicit
		{"mytest.go", false},  // ends in "test.go" but not "_test.go"
	}
	for _, tc := range cases {
		if got := isTestPath(tc.path); got != tc.want {
			t.Errorf("isTestPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSplitDiffPathsPartitionsAndSorts(t *testing.T) {
	input := []string{
		"zeta.go",
		"a_test.go",
		"beta/b.test.ts",
		"gamma.py",
		"delta_test.go",
		"epsilon/x_test.py",
	}
	tests, nonTests := splitDiffPaths(input)

	wantTests := []string{"a_test.go", "beta/b.test.ts", "delta_test.go", "epsilon/x_test.py"}
	wantNonTests := []string{"gamma.py", "zeta.go"}

	if !reflect.DeepEqual(tests, wantTests) {
		t.Errorf("testPaths = %v, want %v", tests, wantTests)
	}
	if !reflect.DeepEqual(nonTests, wantNonTests) {
		t.Errorf("nonTestPaths = %v, want %v", nonTests, wantNonTests)
	}

	// partition: every input accounted for exactly once
	if len(tests)+len(nonTests) != len(input) {
		t.Errorf("partition lost files: got %d total, want %d", len(tests)+len(nonTests), len(input))
	}
}

func TestSplitDiffPathsNilInput(t *testing.T) {
	tests, nonTests := splitDiffPaths(nil)
	if tests == nil {
		t.Error("testPaths is nil; want an empty non-nil slice")
	}
	if nonTests == nil {
		t.Error("nonTestPaths is nil; want an empty non-nil slice")
	}
	if len(tests) != 0 || len(nonTests) != 0 {
		t.Errorf("splitDiffPaths(nil) = %v, %v; want both empty", tests, nonTests)
	}
}

func TestMutationCheckApplicable(t *testing.T) {
	if ok, _ := mutationCheckApplicable([]string{"a_test.go"}, []string{}); ok {
		t.Error("applicable = true for test-only diff; want false (nothing to revert)")
	}
	if ok, _ := mutationCheckApplicable([]string{}, []string{"a.go"}); ok {
		t.Error("applicable = true for no-new-tests diff; want false (nothing to verify)")
	}
	if ok, reason := mutationCheckApplicable([]string{"a_test.go"}, []string{"a.go"}); !ok {
		t.Errorf("applicable = false, want true when both sides present (reason: %q)", reason)
	}
}

func TestMutationFindingFromResults(t *testing.T) {
	// One failure: ok, and the note names that test.
	ok, note := mutationFindingFromResults([]string{"TestCalc_Add"})
	if !ok {
		t.Fatalf("ok = false, want true when a test failed on revert")
	}
	if note != "test(s) failed with the fix reverted and are load-bearing: TestCalc_Add" {
		t.Errorf("note = %q, want it to name TestCalc_Add", note)
	}

	// Zero failures: finding, and the note states the tests pass with the fix reverted.
	ok, note = mutationFindingFromResults(nil)
	if ok {
		t.Error("ok = true, want false when no test failed on revert")
	}
	if note != "new tests pass with the fix reverted; they do not constrain the change" {
		t.Errorf("note = %q, want it to state the tests pass with the fix reverted", note)
	}
}

func TestMutationCheckDisabled(t *testing.T) {
	t.Setenv("FOREMAN_MUTATION_CHECK", "0")
	if !mutationCheckDisabled() {
		t.Error("mutationCheckDisabled() = false, want true when FOREMAN_MUTATION_CHECK=0")
	}

	t.Setenv("FOREMAN_MUTATION_CHECK", "1")
	if mutationCheckDisabled() {
		t.Error("mutationCheckDisabled() = true, want false when FOREMAN_MUTATION_CHECK=1 (default enabled)")
	}
}
