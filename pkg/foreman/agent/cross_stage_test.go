package agent

import (
	"reflect"
	"testing"
)

// TestBranchIsEmpty encodes the "both required" rule: either condition alone is
// a weaker signal that has already misled a stage.
func TestBranchIsEmpty(t *testing.T) {
	cases := []struct {
		name  string
		facts BranchFacts
		want  bool
	}{
		{
			name:  "zero commits and identical refs is empty",
			facts: BranchFacts{CommitsAhead: 0, HeadSHA: "abc", BaseSHA: "abc"},
			want:  true,
		},
		{
			name:  "zero commits but moved refs is not empty",
			facts: BranchFacts{CommitsAhead: 0, HeadSHA: "abc", BaseSHA: "def"},
			want:  false,
		},
		{
			name:  "commits ahead and moved refs is not empty",
			facts: BranchFacts{CommitsAhead: 3, HeadSHA: "abc", BaseSHA: "def"},
			want:  false,
		},
		{
			name:  "commits ahead but identical refs is not empty",
			facts: BranchFacts{CommitsAhead: 2, HeadSHA: "abc", BaseSHA: "abc"},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.facts.BranchIsEmpty(); got != tc.want {
				t.Fatalf("BranchIsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContradictionsCoderClaimsEditsEmptyBranch is the first real incident: the
// coder returned GO with a specific summary against a branch with no commits.
func TestContradictionsCoderClaimsEditsEmptyBranch(t *testing.T) {
	claim := StageClaim{Stage: "coder", Verdict: "GO", ClaimsEdits: true}
	facts := BranchFacts{CommitsAhead: 0, HeadSHA: "abc", BaseSHA: "abc"}

	got := contradictions(claim, facts)
	if len(got) != 1 {
		t.Fatalf("expected exactly one contradiction, got %d: %q", len(got), got)
	}
	want := "coder: claims edits but branch is empty"
	if got[0] != want {
		t.Fatalf("got %q, want %q", got[0], want)
	}
}

// TestContradictionsReviewerClaimsEmptyBranchNonEmpty is the second real
// incident: the reviewer reported "no commits" when the branch had three.
func TestContradictionsReviewerClaimsEmptyBranchNonEmpty(t *testing.T) {
	claim := StageClaim{Stage: "reviewer", Verdict: "NO-GO", ClaimsEmptyBranch: true}
	facts := BranchFacts{CommitsAhead: 3, HeadSHA: "abc", BaseSHA: "def"}

	got := contradictions(claim, facts)
	if len(got) != 1 {
		t.Fatalf("expected exactly one contradiction, got %d: %q", len(got), got)
	}
	want := "reviewer: claims empty branch but CommitsAhead=3"
	if got[0] != want {
		t.Fatalf("got %q, want %q", got[0], want)
	}
}

// TestContradictionsNamedFileNotChanged checks that a file a stage names which
// is not among the changed files is flagged, naming it.
func TestContradictionsNamedFileNotChanged(t *testing.T) {
	claim := StageClaim{
		Stage:      "coder",
		Verdict:    "GO",
		NamedFiles: []string{"pkg/foreman/agent/repo/commit.go", "pkg/foreman/agent/nonexistent.go"},
	}
	facts := BranchFacts{
		CommitsAhead: 1,
		FilesChanged: []string{"pkg/foreman/agent/repo/commit.go", "README.md"},
		HeadSHA:      "abc",
		BaseSHA:      "def",
	}

	got := contradictions(claim, facts)
	if len(got) != 1 {
		t.Fatalf("expected exactly one contradiction, got %d: %q", len(got), got)
	}
	want := "coder: named file pkg/foreman/agent/nonexistent.go is not among the changed files"
	if got[0] != want {
		t.Fatalf("got %q, want %q", got[0], want)
	}
}

// TestContradictionsGatePassEmptyBranch flags a gate that passed trivially on a
// branch identical to its base.
func TestContradictionsGatePassEmptyBranch(t *testing.T) {
	claim := StageClaim{Stage: "gate", Verdict: "GATE-PASS"}
	facts := BranchFacts{CommitsAhead: 0, HeadSHA: "abc", BaseSHA: "abc"}

	got := contradictions(claim, facts)
	if len(got) != 1 {
		t.Fatalf("expected exactly one contradiction, got %d: %q", len(got), got)
	}
	want := "gate: GATE-PASS on an empty branch (checks passed trivially)"
	if got[0] != want {
		t.Fatalf("got %q, want %q", got[0], want)
	}
}

// TestContradictionsConsistentReturnsNil checks that a fully consistent claim
// yields nil, and specifically nil rather than an empty non-nil slice.
func TestContradictionsConsistentReturnsNil(t *testing.T) {
	claim := StageClaim{
		Stage:             "coder",
		Verdict:           "GO",
		ClaimsEdits:       true,
		ClaimsEmptyBranch: false,
		NamedFiles:        []string{"pkg/foreman/agent/repo/commit.go"},
	}
	facts := BranchFacts{
		CommitsAhead: 2,
		FilesChanged: []string{"pkg/foreman/agent/repo/commit.go", "README.md"},
		HeadSHA:      "abc",
		BaseSHA:      "def",
	}

	got := contradictions(claim, facts)
	if got != nil {
		t.Fatalf("expected nil, got non-nil %v", got)
	}
	// Prove it is the nil value, not a zero-length slice.
	if len(got) != 0 {
		t.Fatalf("expected zero length, got %d", len(got))
	}
}

// TestContradictionsZeroCommitsMovedRefsNotEmpty is the "both required" edge:
// CommitsAhead is 0 but the head and base differ, so the branch is NOT empty and
// no emptiness rule should fire.
func TestContradictionsZeroCommitsMovedRefsNotEmpty(t *testing.T) {
	claim := StageClaim{Stage: "gate", Verdict: "GATE-PASS", ClaimsEdits: true}
	facts := BranchFacts{CommitsAhead: 0, HeadSHA: "abc", BaseSHA: "def"}

	got := contradictions(claim, facts)
	if got != nil {
		t.Fatalf("expected nil for non-empty branch, got %v", got)
	}
}

// TestContradictionsMultipleDeterministicOrder fires several rules at once and
// asserts the exact slice twice in a row to prove the ordering is stable.
func TestContradictionsMultipleDeterministicOrder(t *testing.T) {
	claim := StageClaim{
		Stage:             "coder",
		Verdict:           "GATE-PASS",
		ClaimsEdits:       true,
		ClaimsEmptyBranch: false,
		NamedFiles:        []string{"zz.go", "aa.go"},
	}
	facts := BranchFacts{
		CommitsAhead: 0,
		FilesChanged: []string{"mm.go"},
		HeadSHA:      "abc",
		BaseSHA:      "abc",
	}

	want := []string{
		"coder: claims edits but branch is empty",
		"coder: named file aa.go is not among the changed files",
		"coder: named file zz.go is not among the changed files",
		"coder: GATE-PASS on an empty branch (checks passed trivially)",
	}

	first := contradictions(claim, facts)
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first pass:\ngot  %v\nwant %v", first, want)
	}

	second := contradictions(claim, facts)
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("second pass:\ngot  %v\nwant %v", second, want)
	}
}

// TestContradictionsZeroValuesNoPanic ensures the zero-value inputs do not panic.
func TestContradictionsZeroValuesNoPanic(t *testing.T) {
	claim := StageClaim{}
	facts := BranchFacts{}

	got := contradictions(claim, facts)
	// Zero-value StageClaim makes no claims and zero-value BranchFacts has equal
	// SHAs but no edits/empty assertions to trip on, so it must be consistent.
	if got != nil {
		t.Fatalf("expected nil for zero values, got %v", got)
	}
}

// TestShouldEscalate pins the escalation contract: no contradictions, no
// escalation; a single contradiction escalates (not a majority vote).
func TestShouldEscalate(t *testing.T) {
	if shouldEscalate(nil) {
		t.Fatalf("shouldEscalate(nil) = true, want false")
	}
	one := []string{"coder: claims edits but branch is empty"}
	if !shouldEscalate(one) {
		t.Fatalf("shouldEscalate(one entry) = false, want true")
	}
}
