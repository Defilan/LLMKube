package agent

import (
	"regexp"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// review_execution_gate.go is the deterministic rail behind #1618. The
// reviewer rubric's Section K mandates two execution runs (run the diff's own
// new test; probe one near-miss) whenever the diff touches `.go` files, but in
// the first live Section-K review the reviewer performed every read and skipped
// both runs, disclosing the omission in onTrust instead. Numbered step
// sequences are followed faithfully; checklist prose is treated as advisory.
// This rail makes the execution mandate self-enforcing: when the diff touches a
// `.go` file, a GO whose transcript never ran `go test` is demoted to NO-GO so
// it routes to escalation instead of approving a branch the reviewer did not
// execute.
//
// These functions are deterministic and model-free: they run over a stored
// transcript ([]oai.Message) and the ground-truth diff files, making no git
// calls. They mirror the shape of reviewer_diff_gate.go (walk a transcript,
// correlate tool results to the assistant tool_call that produced them, return
// a finding) and the demotion pattern of scope_overlap.go (stamp a GO->NO-GO
// rewrite with verdictDemoted / verdictDemotedBy / verdictClaimed /
// demotionReason).

// reGoTestCommand matches a shell command that invokes `go test`. It is
// applied to both the assistant tool_call arguments and the tool result
// content (the bash tool echoes the command back in its JSON output), so a
// `go test` run is caught whether the transcript records it as the request or
// as the result. The leading word boundary keeps it from matching a path or a
// comment; the space after `test` separates the subcommand from the package
// pattern (`go test ./pkg/...`).
var reGoTestCommand = regexp.MustCompile(`\bgo\s+test\b`)

// transcriptRanGoTest reports whether any bash invocation in the transcript is
// a `go test` run. It scans assistant tool_call arguments (via
// commandFromToolCallArgs, the same correlation the diff gate uses) and
// tool-role content (via toolContent, so a tool message whose Name is empty
// still counts). An empty transcript yields false without panic.
func transcriptRanGoTest(transcript []oai.Message) bool {
	for i := range transcript {
		m := transcript[i]
		switch m.Role {
		case oai.RoleAssistant:
			for _, tc := range m.ToolCalls {
				if reGoTestCommand.MatchString(commandFromToolCallArgs(tc.Function.Arguments)) {
					return true
				}
			}
		case oai.RoleTool:
			if reGoTestCommand.MatchString(toolContent(m)) {
				return true
			}
		}
	}
	return false
}

// enforceReviewerExecution applies the review-execution rail (#1618) to a
// reviewer's verdict. It fires only for a GO on a diff that touches a `.go`
// file: the Section-K execution runs are mandatory for Go diffs but are
// exempt for docs/YAML-only changes. A GO whose transcript never ran `go test`
// is demoted to NO-GO, and the skipped rail is recorded so the verdict is
// distinguishable afterwards from one that earned the execution check. A
// transcript that did run `go test` leaves the verdict untouched.
//
// The demotion is stamped exactly like the scope-overlap rail: verdictDemoted,
// verdictDemotedBy (naming this rail), verdictClaimed (the archived original
// verdict, first-writer-wins per #1678), and demotionReason. A non-GO verdict
// is not this rail's business and returns untouched.
func enforceReviewerExecution(
	log logr.Logger,
	extra map[string]any,
	diffFiles []string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	transcript []oai.Message,
) foremanv1alpha1.AgenticTaskVerdict {
	if extra == nil {
		return verdict
	}
	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		return verdict
	}
	// Non-.go diffs are exempt: the mandatory execution runs only apply when
	// the diff touches `.go` files. hasSourceFile defaults to [".go"] when exts
	// is nil, so this is the Go-only guard.
	if !hasSourceFile(diffFiles, nil) {
		return verdict
	}
	if transcriptRanGoTest(transcript) {
		return verdict
	}

	// The GO on a Go diff never ran `go test`. Record the skipped rail and
	// demote the verdict so the approval does not stand on an unexecuted
	// branch.
	recordRailSkipped(extra, railExecution, skipReasonNoTestRun)
	extra["verdictDemoted"] = true
	extra["verdictDemotedBy"] = railExecution
	// First-writer-wins for the claimed verdict: another demoting rail may have
	// already archived the reviewer's original GO (#1678).
	if _, ok := extra["verdictClaimed"]; !ok {
		extra["verdictClaimed"] = string(verdict)
	}
	extra["demotionReason"] = "reviewer returned GO on a .go diff without running `go test` in the transcript"
	log.Info("reviewer execution: GO on a .go diff with no `go test` run in the transcript; demoting to NO-GO",
		"verdictClaimed", verdict)
	return foremanv1alpha1.AgenticTaskVerdictNoGo
}
