package agent

import (
	"strings"
	"testing"
)

func TestExtractClauses_ExpectedBehaviorBullets(t *testing.T) {
	body := "Intro paragraph.\n\n## Expected Behavior\n\n- first behaviour\n- second behaviour\n- third behaviour\n"
	got := extractClauses(body)
	want := []string{"first behaviour", "second behaviour", "third behaviour"}
	assertClauses(t, got, want)
}

func TestExtractClauses_AcceptanceCriteriaNumbered(t *testing.T) {
	body := "## Acceptance Criteria\n\n1. the first thing happens\n" +
		"2. the second thing happens\n3) the third thing happens\n"
	got := extractClauses(body)
	want := []string{"the first thing happens", "the second thing happens", "the third thing happens"}
	assertClauses(t, got, want)
}

func TestExtractClauses_FollowingHeadingTerminatesSection(t *testing.T) {
	body := "## Expected Behavior\n- clause from expected\n\n" +
		"## Actual Behavior\n- clause from actual\n- another actual\n\n" +
		"## Something else\n- nope\n"
	got := extractClauses(body)
	want := []string{"clause from expected"}
	assertClauses(t, got, want)
}

func TestExtractClauses_HeadingCaseVariations(t *testing.T) {
	for _, body := range []string{
		"## expected behavior\n- a\n- b\n",
		"## Expected Behaviour\n- a\n- b\n",
		"## ACCEPTANCE CRITERIA\n- a\n- b\n",
	} {
		t.Run(strings.TrimSpace(body), func(t *testing.T) {
			got := extractClauses(body)
			if len(got) < 1 || got[0] != "a" {
				t.Fatalf("expected clauses starting with %q, got %v", "a", got)
			}
		})
	}
}

func TestExtractClauses_NoMatchingSection(t *testing.T) {
	body := "## Actual Behavior\n- not included\n\n" +
		"## Steps to Reproduce\n1. also not included\n\n" +
		"Just some prose with no matching heading.\n"
	got := extractClauses(body)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
	// nil body must not panic and returns empty.
	if got := extractClauses(""); len(got) != 0 {
		t.Fatalf("expected empty slice for empty body, got %v", got)
	}
}

func TestExtractClauses_ProseOnlySection(t *testing.T) {
	body := "## Expected Behavior\n\nThe agent should do X.\n\nWhen disabled, it should do Y.\n"
	got := extractClauses(body)
	want := []string{"The agent should do X.", "When disabled, it should do Y."}
	assertClauses(t, got, want)
}

func TestExtractClauses_IndentedBullets(t *testing.T) {
	body := "## Acceptance Criteria\n\n    - deeply indented item\n\t  * tab indented item\n  1. indented numbered\n"
	got := extractClauses(body)
	want := []string{"deeply indented item", "tab indented item", "indented numbered"}
	assertClauses(t, got, want)
}

func TestExtractClauses_HeadingMidSentenceDoesNotOpenSection(t *testing.T) {
	// The heading phrase appears mid-line, not at line start, so it must not
	// open a section. The preceding "## Note" is not a clause heading either.
	body := "## Note\nAs mentioned, ## Expected Behavior should hold for all cases.\n" +
		"- not a clause\n\n## Actual Behavior\n- still not a clause\n"
	got := extractClauses(body)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestExtractClauses_MultipleSectionsDocumentOrder(t *testing.T) {
	body := "## Expected Behavior\n- from expected\n\n" +
		"## Actual Behavior\n- ignored\n\n" +
		"## Acceptance Criteria\n- from criteria\n"
	got := extractClauses(body)
	want := []string{"from expected", "from criteria"}
	assertClauses(t, got, want)
}

func TestClauseChecklist(t *testing.T) {
	if got := clauseChecklist(nil); got != "" {
		t.Fatalf("expected empty string for empty input, got %q", got)
	}
	if got := clauseChecklist([]string{}); got != "" {
		t.Fatalf("expected empty string for empty slice, got %q", got)
	}
	got := clauseChecklist([]string{"a", "b", "c"})
	want := "- [ ] a\n- [ ] b\n- [ ] c"
	if got != want {
		t.Fatalf("checklist mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestUnsatisfiedClauses(t *testing.T) {
	clauses := []string{"one", "two", "three"}

	// Missing index is returned.
	got := unsatisfiedClauses(clauses, map[int]string{1: "path"})
	assertInts(t, got, []int{0, 2})

	// Whitespace-only citation is returned.
	got = unsatisfiedClauses(clauses, map[int]string{0: "  \n\t", 1: "path", 2: "path2"})
	assertInts(t, got, []int{0})

	// All cited -> empty.
	got = unsatisfiedClauses(clauses, map[int]string{0: "p0", 1: "p1", 2: "p2"})
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}

	// Out-of-range keys are ignored, not panicking.
	got = unsatisfiedClauses(clauses, map[int]string{99: "x", -1: "y", 0: "p0"})
	assertInts(t, got, []int{1, 2})

	// Empty cited map -> every clause unsatisfied.
	got = unsatisfiedClauses(clauses, map[int]string{})
	assertInts(t, got, []int{0, 1, 2})

	// Empty clauses -> empty result.
	if got := unsatisfiedClauses(nil, map[int]string{0: "p"}); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func assertClauses(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("clause count mismatch: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("clause[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func assertInts(t *testing.T, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("count mismatch: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}
