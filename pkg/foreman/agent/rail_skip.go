package agent

// Rail-skip observability (#1605).
//
// A single `repo.DiffNameOnly` call feeds four reviewer rails. When it fails
// or returns nothing, every one of them loses its input at once, which is the
// mechanism #1570 named: the safety net is disabled by the same condition that
// creates the hazard. Two of the four already returned the model's verdict
// silently, so a verdict nothing checked was indistinguishable afterwards from
// one that earned it.
//
// These helpers make that visible. They never change a verdict: demoting
// because a check could not run destroys good work, which is the #1552 lesson.

// railsSkippedKey is the single extra key recording every rail that could not
// run on a task, as "<rail>: <reason>" entries. One key rather than a marker
// per rail, because the operational question is "did anything fail to run on
// this verdict?" and answering it should not require knowing every rail's name.
const railsSkippedKey = "railsSkipped"

// Rail names used in railsSkipped entries.
const (
	railScopeOverlap        = "scope-overlap"
	railVerdictFromFindings = "verdict-from-findings"
	railEmptyClaim          = "empty-claim"
	railGroundedFinding     = "grounded-finding"
)

// Reasons a rail could not run.
const (
	skipReasonNoDiff      = "diff-unavailable"
	skipReasonNoIssueBody = "no-issue-body"
	skipReasonNoDiffFiles = "no-diff-files"
)

// recordRailSkipped appends a rail's inability to run, and why, to extra.
// Repeated entries collapse, so a rail short-circuiting twice on the same
// reason does not inflate the list. Tolerates a nil map so callers need no
// guard of their own.
func recordRailSkipped(extra map[string]any, rail, reason string) {
	if extra == nil {
		return
	}
	entry := rail + ": " + reason
	existing, _ := extra[railsSkippedKey].([]string)
	for _, e := range existing {
		if e == entry {
			return
		}
	}
	extra[railsSkippedKey] = append(existing, entry)
}
