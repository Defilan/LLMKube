// cross_stage.go holds a deterministic, model-free contradiction detector for
// the pipeline stages. Each stage (coder, gate, reviewer) asserts overlapping,
// checkable facts about the same branch. When those assertions disagree,
// exactly one of them is wrong, and the disagreement is the strongest signal
// that something is amiss. This file only compares a stage's claims against the
// ground facts the caller supplies; it never shells out to git and never touches
// the executors. Computing BranchFacts from a real branch is deliberately left
// to the caller (see the executor/controller wiring, not this package's scope).
package agent

import (
	"fmt"
	"sort"
)

// BranchFacts is the ground truth about a single branch, computed once by the
// caller. It is a plain data struct so the detector below stays a pure function
// of (claim, facts).
type BranchFacts struct {
	CommitsAhead int      `json:"commitsAhead"`
	FilesChanged []string `json:"filesChanged"`
	NetLineDelta int      `json:"netLineDelta"`
	HeadSHA      string   `json:"headSHA"`
	BaseSHA      string   `json:"baseSHA"`
}

// BranchIsEmpty reports whether the branch carries no change relative to its
// base. Both conditions are required because either alone is a weaker signal
// that has already misled a stage: zero commits while the ref has moved is not
// the same as an identical ref, and an identical ref with a nonzero commit count
// is an inconsistency the caller must surface.
func (f BranchFacts) BranchIsEmpty() bool {
	return f.CommitsAhead == 0 && f.HeadSHA == f.BaseSHA
}

// StageClaim captures what a single pipeline stage asserted about the branch.
type StageClaim struct {
	Stage             string   `json:"stage"`
	Verdict           string   `json:"verdict"`
	ClaimsEdits       bool     `json:"claimsEdits"`
	ClaimsEmptyBranch bool     `json:"claimsEmptyBranch"`
	NamedFiles        []string `json:"namedFiles"`
}

// crossStageEvidence preserves what a stage claimed and the ground facts it was
// checked against, so a human or a later stage can see WHAT disagreed with WHAT
// rather than only that something did. The contradictions slice is the same
// human-readable list recorded under Extra["crossStageContradictions"]; the
// claim and facts are the structured inputs that produced it.
type crossStageEvidence struct {
	Claim          StageClaim  `json:"claim"`
	Facts          BranchFacts `json:"facts"`
	Contradictions []string    `json:"contradictions"`
}

// contradictions reports every way the claim disagrees with the ground facts.
// It returns nil (not an empty slice) when the claim is consistent with the
// facts. The order is deterministic: the rules fire in a fixed sequence, and
// NamedFiles are emitted in sorted order.
func contradictions(claim StageClaim, facts BranchFacts) []string {
	var cs []string

	// Rule 1: a stage that claims it made edits against a branch that is empty.
	if claim.ClaimsEdits && facts.BranchIsEmpty() {
		cs = append(cs, fmt.Sprintf("%s: claims edits but branch is empty", claim.Stage))
	}

	// Rule 2: a stage that claims the branch is empty when it is not, citing the
	// commit count that refutes it.
	if claim.ClaimsEmptyBranch && !facts.BranchIsEmpty() {
		cs = append(cs, fmt.Sprintf("%s: claims empty branch but CommitsAhead=%d", claim.Stage, facts.CommitsAhead))
	}

	// Rule 3: any named file the stage references that is not among the changed
	// files, in sorted order.
	if len(claim.NamedFiles) > 0 {
		changed := make(map[string]struct{}, len(facts.FilesChanged))
		for _, f := range facts.FilesChanged {
			changed[f] = struct{}{}
		}
		missing := make([]string, 0, len(claim.NamedFiles))
		for _, nf := range claim.NamedFiles {
			if _, ok := changed[nf]; !ok {
				missing = append(missing, nf)
			}
		}
		sort.Strings(missing)
		for _, nf := range missing {
			cs = append(cs, fmt.Sprintf("%s: named file %s is not among the changed files", claim.Stage, nf))
		}
	}

	// Rule 4: a gate that passes on an empty branch. The checks passed trivially,
	// so the verdict carries no information about the change.
	if claim.Verdict == "GATE-PASS" && facts.BranchIsEmpty() {
		cs = append(cs, fmt.Sprintf("%s: GATE-PASS on an empty branch (checks passed trivially)", claim.Stage))
	}

	if len(cs) == 0 {
		return nil
	}
	return cs
}

// shouldEscalate reports whether any contradiction warrants stopping the line.
// This is deliberately not a majority vote: two independent witnesses disagreeing
// is the strongest evidence available that something is wrong, so even a single
// contradiction escalates.
func shouldEscalate(cs []string) bool {
	return len(cs) > 0
}
