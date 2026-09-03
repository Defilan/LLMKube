package agent

import "github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"

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

// Rail names. Used in railsSkipped entries and, for the two rails that can
// rewrite a verdict, in the verdictDemotedBy marker those rails stamp. The
// demoting rails take their names from pkg/foreman/agent/reviewer so the
// controller, which reads verdictDemotedBy back out of the result envelope
// and cannot import this package, compares against the same strings (#1636).
const (
	railIssueAsk            = reviewer.RailIssueAsk
	railScopeOverlap        = reviewer.RailScopeOverlap
	railVerdictFromFindings = "verdict-from-findings"
	railEmptyClaim          = "empty-claim"
	railGroundedFinding     = "grounded-finding"
	// railExecution names the review-execution rail (#1618). It records a
	// GO on a Go diff whose review never ran `go test`, and a GO whose diff
	// could not be fetched at all; it never rewrites the verdict. Demotion is
	// a later flip once the fleet shows the runs fit the turn budget.
	railExecution = "review-execution"
)

// Reasons a rail could not run.
const (
	skipReasonNoDiff      = "diff-unavailable"
	skipReasonNoIssueBody = "no-issue-body"
	skipReasonNoDiffFiles = "no-diff-files"
	// skipReasonNoPathRefs is the likeliest of the four in practice: it fires
	// for any issue citing no extractable file paths, which hand-written
	// issues routinely do not.
	skipReasonNoPathRefs = "no-path-refs-in-issue"
	// skipReasonNoTestRun marks the review-execution rail (#1618) short-
	// circuiting: a GO on a .go diff whose transcript never ran `go test`.
	skipReasonNoTestRun = "no-test-run"
)

// Reasons the scope rail detected drift and then declined to act on it. These
// are not skips: the rail ran and answered. They are recorded because
// scopeDriftDetected alone cannot say which branch declined.
const (
	scopeNotDemotedNoSourceFile = "diff-has-no-source-file"
	scopeNotDemotedAlreadyNonGo = "verdict-already-non-go"
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
